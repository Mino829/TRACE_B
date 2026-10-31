package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const (
	ArrivalQueueLimit = 50
	ArrivalPhotoWait  = 15 * time.Second
)

var (
	ErrArrivalQueueFull    = errors.New("arrival queue is full")
	ErrInvalidArrivalProof = errors.New("arrival proof is invalid or expired")
)

type Arrival struct {
	ID, Status, CheckpointID, SkipReason string
	ArrivedAt, PhotoWaitUntil            time.Time
	LivePresencePhase                    string
}

type ReplayCandidate struct {
	ArrivalID       string `json:"arrivalId"`
	ReadyPhotoCount int    `json:"readyPhotoCount"`
}

// ArrivalRouteExpected mirrors the conditions for appendArrivalSegment. A
// confirmed arrival need not yield a segment when its checkpoint was retired
// or the last ready photo was captured after arrival.
func (s *Store) ArrivalRouteExpected(ctx context.Context, memoryID string, lastCaptured time.Time) (bool, error) {
	if s.routes == nil || lastCaptured.IsZero() {
		return false, nil
	}
	var checkpointID string
	var arrivedAt time.Time
	err := s.q.QueryRowContext(ctx, `SELECT checkpoint_id,arrived_at FROM arrival_events WHERE memory_session_id=$1`,
		memoryID).Scan(&checkpointID, &arrivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, ok := s.routes.NodeIndex(checkpointID)
	return ok && !lastCaptured.After(arrivedAt), nil
}

func (s *Store) arrivalForMemory(ctx context.Context, memoryID string) (Arrival, error) {
	var a Arrival
	var skip sql.NullString
	err := s.q.QueryRowContext(ctx, `SELECT id,replay_status,checkpoint_id,skip_reason,arrived_at,photo_wait_until
 FROM arrival_events WHERE memory_session_id=$1`, memoryID).Scan(&a.ID, &a.Status, &a.CheckpointID, &skip,
		&a.ArrivedAt, &a.PhotoWaitUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.SkipReason = skip.String
	return a, err
}

// The projected QR is shared; each MemorySession has one arrival row, which a
// fresh QR read after its replay has ended queues again. The queue lock
// bounds the number of durable queued entries under concurrency.
func (s *Store) AcceptArrival(ctx context.Context, memoryID, linkTokenHash, arrivalID, checkpointID, receiptKey, requestHash string, now time.Time) ([]byte, error) {
	var body []byte
	err := s.Transaction(ctx, "display:lease", func(tx *Store) error {
		if err := tx.lockTransaction(ctx, "arrival:queue"); err != nil {
			return err
		}
		return tx.Transaction(ctx, memoryID, func(tx *Store) error {
			savedRequestHash, receiptErr := tx.arrivalReceiptRequestHash(ctx, memoryID, receiptKey, now)
			if receiptErr == nil && savedRequestHash != requestHash {
				return ErrIdempotencyConflict
			}
			if receiptErr != nil && !errors.Is(receiptErr, ErrNotFound) {
				return receiptErr
			}
			var expires time.Time
			var stopped sql.NullTime
			var deleting bool
			var photoMode, graphVersion string
			err := tx.q.QueryRowContext(ctx, `SELECT expires_at,stopped_at,deleting,photo_mode,COALESCE(campus_graph_version,'')
 FROM memory_sessions WHERE id=$1 FOR UPDATE`, memoryID).Scan(&expires, &stopped, &deleting, &photoMode, &graphVersion)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if deleting {
				return ErrConflict
			}
			a, err := tx.arrivalForMemory(ctx, memoryID)
			// A replay that has played or been skipped plays again when the
			// participant reads a fresh booth QR under a new key. Retries of an
			// earlier key, and codes that expired, still only read the state.
			again := err == nil && (a.Status == "completed" || a.Status == "skipped") && checkpointID != "" &&
				errors.Is(receiptErr, ErrNotFound) && !stopped.Valid && now.Before(expires)
			if errors.Is(err, ErrNotFound) || again {
				if checkpointID == "" {
					return ErrInvalidArrivalProof
				}
				if stopped.Valid || !now.Before(expires) {
					return ErrConflict
				}
				var stored string
				err = tx.q.QueryRowContext(ctx, `SELECT token_hash FROM experience_links WHERE memory_session_id=$1
 AND expires_at>$2 AND token_expires_at>$2`, memoryID, now).Scan(&stored)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if err == nil {
					if !secureEqual(stored, linkTokenHash) {
						return ErrConflict
					}
					result, err := tx.q.ExecContext(ctx, `UPDATE live_presence_sessions SET presence_phase='arrived'
 WHERE id=(SELECT live_session_id FROM experience_links WHERE memory_session_id=$1 AND expires_at>$2 AND token_expires_at>$2)
 AND tracking_status='tracking' AND presence_phase='approaching' AND expires_at>$2`, memoryID, now)
					if err != nil {
						return err
					}
					if n, _ := result.RowsAffected(); n == 1 {
						a.LivePresencePhase = "arrived"
					}
				}
				if err := tx.EndPushForMemory(ctx, memoryID); err != nil {
					return err
				}
				var count int
				if err := tx.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM arrival_events a
 JOIN memory_sessions m ON m.id=a.memory_session_id
 WHERE a.replay_status='queued' AND m.deleting=FALSE AND m.stopped_at IS NULL AND m.expires_at>$1`, now).Scan(&count); err != nil {
					return err
				}
				if count >= ArrivalQueueLimit {
					return ErrArrivalQueueFull
				}
				a.ID, a.Status, a.CheckpointID = arrivalID, "queued", checkpointID
				a.ArrivedAt, a.PhotoWaitUntil = now, now.Add(ArrivalPhotoWait)
				if again {
					// The row starts over under a new ID, so renderers take it as a
					// new arrival, and its time moves to now, so the photos taken
					// since the last replay join the route.
					_, err = tx.q.ExecContext(ctx, `UPDATE arrival_events SET id=$1,checkpoint_id=$3,replay_status='queued',
 skip_reason=NULL,arrived_at=$4,photo_wait_until=$5,replay_started_at=NULL,replay_finished_at=NULL,renderer_lease_id=NULL
 WHERE memory_session_id=$2`, a.ID, memoryID, checkpointID, now, a.PhotoWaitUntil)
				} else {
					_, err = tx.q.ExecContext(ctx, `INSERT INTO arrival_events
 (id,memory_session_id,source,checkpoint_id,replay_status,arrived_at,photo_wait_until)
 VALUES ($1,$2,'qr',$3,'queued',$4,$5)`, a.ID, memoryID, checkpointID, now, a.PhotoWaitUntil)
				}
				if err != nil {
					return err
				}
				// The route extension uses the storage contract pinned at
				// registration; old uploaded assets remain independent.
				if photoMode == PhotoModeDevice {
					err = tx.reconcileMomentRoutes(ctx, memoryID)
				} else {
					err = tx.reconcileRoutes(ctx, memoryID)
				}
				if err != nil {
					return err
				}
				if tx.displayEnabled {
					projected, err := tx.projectDisplayArrival(ctx, DisplayArrival{
						ArrivalID: a.ID, ReplayStatus: a.Status,
					}, memoryID, photoMode, graphVersion)
					if err != nil {
						return err
					}
					if _, err := tx.AppendDisplayEvent(ctx, "participant.arrived", projected, now); err != nil {
						return err
					}
				}
				if a.Status == "queued" {
					_, err = tx.AppendDisplayEvent(ctx, "arrival.replay.requested", map[string]string{"arrivalId": a.ID}, now)
					if err != nil {
						return err
					}
				}
			} else if err != nil {
				return err
			} else {
				a.LivePresencePhase, err = tx.linkedPhase(ctx, memoryID)
				if err != nil {
					return err
				}
			}
			body, err = json.Marshal(struct {
				ArrivalID         string `json:"arrivalId"`
				Status            string `json:"status"`
				LivePresencePhase string `json:"livePresencePhase,omitempty"`
			}{a.ID, a.Status, a.LivePresencePhase})
			if err != nil {
				return err
			}
			body = append(body, '\n')
			if receiptErr == nil {
				return nil
			}
			locator, err := json.Marshal(struct {
				ArrivalID string `json:"arrivalId"`
			}{a.ID})
			if err != nil {
				return err
			}
			return tx.saveArrivalReceipt(ctx, memoryID, receiptKey, requestHash, locator, now.Add(24*time.Hour), now)
		})
	})
	return body, err
}

func (s *Store) linkedPhase(ctx context.Context, memoryID string) (string, error) {
	var phase string
	err := s.q.QueryRowContext(ctx, `SELECT COALESCE(l.presence_phase,'') FROM experience_links e
 LEFT JOIN live_presence_sessions l ON l.id=e.live_session_id WHERE e.memory_session_id=$1`, memoryID).Scan(&phase)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return phase, err
}

func (s *Store) arrivalReceiptRequestHash(ctx context.Context, memoryID, key string, now time.Time) (string, error) {
	var requestHash string
	err := s.q.QueryRowContext(ctx, `SELECT request_hash FROM arrival_receipts
 WHERE memory_session_id=$1 AND key_hash=$2 AND expires_at>$3`, memoryID, key, now).Scan(&requestHash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return requestHash, err
}

func (s *Store) saveArrivalReceipt(ctx context.Context, memoryID, key, requestHash string, body []byte, expires, now time.Time) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO arrival_receipts (memory_session_id,key_hash,request_hash,response_body,expires_at)
 VALUES ($1,$2,$3,$4,$5) ON CONFLICT(memory_session_id,key_hash) DO UPDATE
 SET request_hash=EXCLUDED.request_hash,response_body=EXCLUDED.response_body,expires_at=EXCLUDED.expires_at
 WHERE arrival_receipts.expires_at<=$6`, memoryID, key, requestHash, body, expires, now)
	return err
}

// ReplayCandidates returns a bounded FIFO view of active waiting arrivals.
// Pending photos delay readiness by at most 15 seconds, not the arrival itself.
// DisplayReplay independently fences order and prevents overlapping playback.
func (s *Store) ReplayCandidates(ctx context.Context, now time.Time) ([]ReplayCandidate, error) {
	if err := s.RefreshDisplay(ctx, now); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `SELECT a.id,
 (SELECT COUNT(*) FROM photos p WHERE p.memory_session_id=a.memory_session_id AND p.status='ready')
 + (SELECT COUNT(*) FROM photo_moments p WHERE p.memory_session_id=a.memory_session_id AND p.captured_at<=a.arrived_at)
 FROM arrival_events a JOIN memory_sessions m ON m.id=a.memory_session_id
 WHERE a.replay_status='queued' AND m.deleting=FALSE AND m.stopped_at IS NULL AND m.expires_at>$1
 AND (SELECT mode='normal' FROM display_fallback_control WHERE singleton=TRUE)
 AND (a.photo_wait_until<=$1 OR NOT EXISTS (
 SELECT 1 FROM photos p WHERE p.memory_session_id=a.memory_session_id AND p.status='pending'))
 AND NOT EXISTS (
 SELECT 1 FROM arrival_events earlier JOIN memory_sessions em ON em.id=earlier.memory_session_id
 WHERE earlier.replay_status='queued' AND em.deleting=FALSE AND em.stopped_at IS NULL AND em.expires_at>$1
 AND (earlier.arrived_at,earlier.id)<(a.arrived_at,a.id) AND earlier.photo_wait_until>$1
 AND EXISTS (SELECT 1 FROM photos p WHERE p.memory_session_id=earlier.memory_session_id AND p.status='pending'))
 ORDER BY a.arrived_at,a.id LIMIT $2`, now, ArrivalQueueLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []ReplayCandidate
	for rows.Next() {
		var c ReplayCandidate
		if err := rows.Scan(&c.ArrivalID, &c.ReadyPhotoCount); err != nil {
			return nil, err
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

func (s *Store) SkipArrival(ctx context.Context, id string, now time.Time) error {
	if OperatorActor(ctx) == "" {
		return ErrConflict
	}
	return s.Transaction(ctx, "display:lease", func(tx *Store) error {
		if _, err := tx.advanceFallback(ctx, now); err != nil {
			return err
		}
		result, err := tx.q.ExecContext(ctx, `UPDATE arrival_events SET replay_status='skipped',skip_reason='operator',replay_finished_at=$2
 WHERE id=$1 AND replay_status IN ('queued','playing')`, id, now)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 0 {
			_, err = tx.AppendDisplayEvent(ctx, "arrival.replay.skipped", map[string]string{"arrivalId": id, "reason": "operator"}, now)
			if err != nil {
				return err
			}
		}
		outcome := "skipped"
		if n, _ := result.RowsAffected(); n == 0 {
			var status string
			err = tx.q.QueryRowContext(ctx, `SELECT replay_status FROM arrival_events WHERE id=$1`, id).Scan(&status)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if status != "skipped" {
				return ErrConflict
			}
			outcome = "already_skipped"
		}
		if _, err := tx.advanceFallback(ctx, now); err != nil {
			return err
		}
		return tx.auditOperator(ctx, "arrival.skip", hashScope(id)[:12], outcome, now)
	})
}

// RecordArrivalProofIssue records a proof only for an authenticated operator.
func (s *Store) RecordArrivalProofIssue(ctx context.Context, checkpointID string, now time.Time) error {
	return s.auditOperator(ctx, "arrival.proof.issue", hashScope(checkpointID)[:12], "issued", now)
}

func (s *Store) auditOperator(ctx context.Context, action, target, result string, now time.Time) error {
	actor := OperatorActor(ctx)
	if actor == "" {
		return ErrConflict
	}
	_, err := s.q.ExecContext(ctx, `INSERT INTO arrival_operator_audit
 (operator_id,operator_email_hash,action,target_id_short,result,created_at) VALUES ($1,NULLIF($2,''),$3,$4,$5,$6)`,
		actor, OperatorEmailHash(ctx), action, target, result, now)
	return err
}

func (s *Store) SkipOwnArrival(ctx context.Context, memoryID string, now time.Time) error {
	return s.Transaction(ctx, "display:lease", func(tx *Store) error {
		if err := tx.lockTransaction(ctx, memoryID); err != nil {
			return err
		}
		var id string
		err := tx.q.QueryRowContext(ctx, `UPDATE arrival_events SET replay_status='skipped',
 skip_reason='participant_request',replay_finished_at=$2
 WHERE memory_session_id=$1 AND replay_status='queued' RETURNING id`, memoryID, now).Scan(&id)
		if err == nil {
			_, err = tx.AppendDisplayEvent(ctx, "arrival.replay.skipped", map[string]string{"arrivalId": id, "reason": "participant_request"}, now)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		a, err := tx.arrivalForMemory(ctx, memoryID)
		if err != nil {
			return err
		}
		if a.Status == "skipped" {
			return nil
		}
		return ErrConflict
	})
}
