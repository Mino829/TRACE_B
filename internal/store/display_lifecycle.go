package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *Store) removeMemoryDisplay(ctx context.Context, memoryID string, now time.Time) error {
	var arrivalID, status string
	err := s.q.QueryRowContext(ctx, `SELECT id,replay_status FROM arrival_events WHERE memory_session_id=$1 FOR UPDATE`, memoryID).Scan(&arrivalID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == "queued" || status == "playing" {
		_, err = s.q.ExecContext(ctx, `UPDATE arrival_events SET replay_status='skipped',skip_reason='deleted',replay_finished_at=$2 WHERE id=$1`, arrivalID, now)
		if err != nil {
			return err
		}
		_, err = s.AppendDisplayEvent(ctx, "arrival.replay.skipped", map[string]string{"arrivalId": arrivalID, "reason": "deleted"}, now)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) removeExpiredMemoryDisplay(ctx context.Context, now time.Time) error {
	rows, err := s.q.QueryContext(ctx, `SELECT id FROM memory_sessions WHERE delete_after<=$1 OR (expires_at<=$1 AND deleting=FALSE) ORDER BY id`, now)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.removeMemoryDisplay(ctx, id, now); err != nil {
			return err
		}
	}
	return nil
}

// Operator authorization is handled by the API. This never silently retries a
// disconnected renderer; only an explicit signed operator request requeues it.
func (s *Store) RetryDisplayArrival(ctx context.Context, id string, now time.Time) error {
	return s.Transaction(ctx, "arrival:queue", func(tx *Store) error {
		var count int
		if err := tx.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM arrival_events a
 JOIN memory_sessions m ON m.id=a.memory_session_id
 WHERE a.replay_status='queued' AND m.deleting=FALSE AND m.expires_at>$1`, now).Scan(&count); err != nil {
			return err
		}
		if count >= ArrivalQueueLimit {
			return ErrArrivalQueueFull
		}
		result, err := tx.q.ExecContext(ctx, `UPDATE arrival_events a SET replay_status='queued',skip_reason=NULL,replay_started_at=NULL,
 replay_finished_at=NULL,renderer_lease_id=NULL,photo_wait_until=$2
 FROM memory_sessions m WHERE a.id=$1 AND a.memory_session_id=m.id AND m.deleting=FALSE AND m.expires_at>$2
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
		_, err = tx.q.ExecContext(ctx, `INSERT INTO arrival_operator_audit
 (operator_id,action,target_id_short,result,created_at) VALUES ('arrival_issuer','arrival.retry',$1,'queued',$2)`, hashScope(id)[:12], now)
		return err
	})
}
