package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

func (s *Store) consumeArrivalProof(ctx context.Context, memoryID, fingerprint string) error {
	result, err := s.q.ExecContext(ctx, `INSERT INTO arrival_proof_uses(memory_session_id,proof_fingerprint)
 VALUES ($1,$2) ON CONFLICT DO NOTHING`, memoryID, fingerprint)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrInvalidArrivalProof
	}
	return nil
}

// ReplayArrival separates a fresh, explicit viewing request from initial arrival
// recovery. Accepted receipts are checked before proof freshness and state.
func (s *Store) ReplayArrival(ctx context.Context, memoryID, replayID, checkpointID, proofFingerprint, receiptKey, requestHash string, now time.Time) ([]byte, error) {
	var body []byte
	err := s.Transaction(ctx, "display:lease", func(tx *Store) error {
		if err := tx.lockTransaction(ctx, "arrival:queue"); err != nil {
			return err
		}
		return tx.Transaction(ctx, memoryID, func(tx *Store) error {
			var savedHash string
			err := tx.q.QueryRowContext(ctx, `SELECT request_hash,response_body FROM arrival_replay_receipts
 WHERE memory_session_id=$1 AND key_hash=$2`, memoryID, receiptKey).Scan(&savedHash, &body)
			if err == nil {
				if savedHash != requestHash {
					return ErrIdempotencyConflict
				}
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			var expires time.Time
			var stopped sql.NullTime
			var deleting bool
			err = tx.q.QueryRowContext(ctx, `SELECT expires_at,stopped_at,deleting FROM memory_sessions WHERE id=$1 FOR UPDATE`, memoryID).Scan(&expires, &stopped, &deleting)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if deleting || stopped.Valid || !now.Before(expires) {
				return ErrConflict
			}
			a, err := tx.arrivalForMemory(ctx, memoryID)
			if err != nil {
				return err
			}
			if a.Status != "completed" {
				return ErrConflict
			}
			if checkpointID == "" || proofFingerprint == "" {
				return ErrInvalidArrivalProof
			}
			var count int
			if err := tx.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM arrival_events a JOIN memory_sessions m ON m.id=a.memory_session_id
 WHERE a.replay_status='queued' AND m.deleting=FALSE AND m.stopped_at IS NULL AND m.expires_at>$1`, now).Scan(&count); err != nil {
				return err
			}
			if count >= ArrivalQueueLimit {
				return ErrArrivalQueueFull
			}
			if err := tx.consumeArrivalProof(ctx, memoryID, proofFingerprint); err != nil {
				return err
			}
			_, err = tx.q.ExecContext(ctx, `UPDATE arrival_events SET replay_id=$2,queued_at=$3,replay_status='queued',
 skip_reason=NULL,replay_started_at=NULL,replay_finished_at=NULL,renderer_lease_id=NULL,photo_wait_until=$3
 WHERE memory_session_id=$1`, memoryID, replayID, now)
			if err != nil {
				return err
			}
			if err := tx.appendReplayRequest(ctx, memoryID, replayID, now); err != nil {
				return err
			}
			body, err = json.Marshal(struct {
				ArrivalID string `json:"arrivalId"`
				Status    string `json:"status"`
			}{replayID, "queued"})
			if err != nil {
				return err
			}
			body = append(body, '\n')
			_, err = tx.q.ExecContext(ctx, `INSERT INTO arrival_replay_receipts(memory_session_id,key_hash,request_hash,response_body)
 VALUES ($1,$2,$3,$4)`, memoryID, receiptKey, requestHash, body)
			return err
		})
	})
	return body, err
}

func (s *Store) appendReplayRequest(ctx context.Context, memoryID, replayID string, now time.Time) error {
	if s.displayEnabled {
		var photoMode, graphVersion string
		if err := s.q.QueryRowContext(ctx, `SELECT photo_mode,COALESCE(campus_graph_version,'') FROM memory_sessions WHERE id=$1`, memoryID).Scan(&photoMode, &graphVersion); err != nil {
			return err
		}
		projected, err := s.projectDisplayArrival(ctx, DisplayArrival{ArrivalID: replayID, ReplayStatus: "queued"}, memoryID, photoMode, graphVersion)
		if err != nil {
			return err
		}
		if _, err := s.AppendDisplayEvent(ctx, "participant.arrived", projected, now); err != nil {
			return err
		}
	}
	_, err := s.AppendDisplayEvent(ctx, "arrival.replay.requested", map[string]string{"arrivalId": replayID}, now)
	return err
}
