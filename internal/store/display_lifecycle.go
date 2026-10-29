package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *Store) removeMemoryDisplay(ctx context.Context, memoryID string, now time.Time) error {
	// Callers take display:lease before per-memory locks.
	if _, err := s.advanceFallback(ctx, now); err != nil {
		return err
	}
	return s.removeMemoryDisplayWithReason(ctx, memoryID, "deleted", now)
}

// StopMemory holds display:lease before the memory lock. Repeated stops do not
// append another removal event, and a pending fallback progresses in this cut.
func (s *Store) stopMemoryDisplay(ctx context.Context, memoryID string, now time.Time) error {
	return s.removeMemoryDisplayWithReason(ctx, memoryID, "participant_request", now)
}

func (s *Store) removeMemoryDisplayWithReason(ctx context.Context, memoryID, reason string, now time.Time) error {
	var arrivalID, status string
	err := s.q.QueryRowContext(ctx, `SELECT id,replay_status FROM arrival_events WHERE memory_session_id=$1 FOR UPDATE`, memoryID).Scan(&arrivalID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == "queued" || status == "playing" {
		_, err = s.q.ExecContext(ctx, `UPDATE arrival_events SET replay_status='skipped',skip_reason=$3,replay_finished_at=$2 WHERE id=$1`, arrivalID, now, reason)
		if err != nil {
			return err
		}
		_, err = s.AppendDisplayEvent(ctx, "arrival.replay.skipped", map[string]string{"arrivalId": arrivalID, "reason": reason}, now)
		if err != nil {
			return err
		}
	}
	_, err = s.advanceFallback(ctx, now)
	return err
}

func (s *Store) removeExpiredMemoryDisplay(ctx context.Context, now time.Time) error {
	rows, err := s.q.QueryContext(ctx, `SELECT id,stopped_at IS NOT NULL FROM memory_sessions
 WHERE delete_after<=$1 OR ((expires_at<=$1 OR stopped_at IS NOT NULL) AND deleting=FALSE) ORDER BY id`, now)
	if err != nil {
		return err
	}
	type memoryRemoval struct {
		id      string
		stopped bool
	}
	var memories []memoryRemoval
	for rows.Next() {
		var memory memoryRemoval
		if err = rows.Scan(&memory.id, &memory.stopped); err != nil {
			break
		}
		memories = append(memories, memory)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, memory := range memories {
		var err error
		if memory.stopped {
			err = s.stopMemoryDisplay(ctx, memory.id, now)
		} else {
			err = s.removeMemoryDisplay(ctx, memory.id, now)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// A renderer failure is requeued only by an explicitly authenticated operator.
func (s *Store) RetryDisplayArrival(ctx context.Context, id string, now time.Time) error {
	if OperatorActor(ctx) == "" {
		return ErrConflict
	}
	return s.Transaction(ctx, "display:lease", func(tx *Store) error {
		if err := tx.lockTransaction(ctx, "arrival:queue"); err != nil {
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
		result, err := tx.q.ExecContext(ctx, `UPDATE arrival_events a SET replay_status='queued',skip_reason=NULL,replay_started_at=NULL,
 replay_finished_at=NULL,renderer_lease_id=NULL,photo_wait_until=$2
 FROM memory_sessions m WHERE a.id=$1 AND a.memory_session_id=m.id AND m.deleting=FALSE AND m.stopped_at IS NULL AND m.expires_at>$2
 AND a.replay_status='skipped' AND a.skip_reason='renderer_failure'`,
			id, now)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return ErrConflict
		}
		_, err = tx.AppendDisplayEvent(ctx, "arrival.replay.requested", map[string]string{"arrivalId": id}, now)
		if err != nil {
			return err
		}
		return tx.auditOperator(ctx, "arrival.retry", hashScope(id)[:12], "queued", now)
	})
}
