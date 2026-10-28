package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PushSubscription contains only encrypted browser credentials. Never return it from a public API.
type PushSubscription struct {
	MemorySessionID                  string
	EndpointEncrypted, KeysEncrypted []byte
	IntervalMinutes                  int
	Status                           string
	NextSendAt, ExpiresAt            time.Time
}

func (s *Store) PutPush(ctx context.Context, p PushSubscription, now time.Time) (PushSubscription, error) {
	if p.IntervalMinutes != 30 {
		return PushSubscription{}, ErrConflict
	}
	err := s.Transaction(ctx, p.MemorySessionID, func(tx *Store) error {
		if err := tx.lockPushControl(ctx); err != nil {
			return err
		}
		expires, err := tx.pushEligible(ctx, p.MemorySessionID, now)
		if err != nil {
			return err
		}
		p.ExpiresAt = expires
		p.Status = "active"
		p.NextSendAt = now.Add(time.Duration(p.IntervalMinutes) * time.Minute)
		_, err = tx.q.ExecContext(ctx, `INSERT INTO push_subscriptions
   (memory_session_id,endpoint_encrypted,keys_encrypted,interval_minutes,status,next_send_at,expires_at)
   VALUES ($1,$2,$3,$4,'active',$5,$6)
   ON CONFLICT (memory_session_id) DO UPDATE SET endpoint_encrypted=EXCLUDED.endpoint_encrypted,
   keys_encrypted=EXCLUDED.keys_encrypted,interval_minutes=EXCLUDED.interval_minutes,
   status='active',next_send_at=EXCLUDED.next_send_at,expires_at=EXCLUDED.expires_at`,
			p.MemorySessionID, p.EndpointEncrypted, p.KeysEncrypted, p.IntervalMinutes, p.NextSendAt, p.ExpiresAt)
		return err
	})
	return p, err
}

func (s *Store) pushEligible(ctx context.Context, id string, now time.Time) (time.Time, error) {
	var expires time.Time
	var deleting, stopped bool
	err := s.q.QueryRowContext(ctx, `SELECT LEAST(m.expires_at,COALESCE(
   (SELECT l.expires_at FROM experiences e JOIN live_presence_sessions l ON l.id=e.live_session_id
    WHERE e.memory_session_id=m.id),m.expires_at)),m.deleting,m.push_stopped OR
  EXISTS (SELECT 1 FROM experiences e JOIN live_presence_sessions l ON l.id=e.live_session_id
    WHERE e.memory_session_id=m.id AND (l.tracking_status<>'tracking' OR l.expires_at<=$2 OR l.presence_phase IN ('arrived','on_site')))
  FROM memory_sessions m WHERE m.id=$1 FOR UPDATE`, id, now).Scan(&expires, &deleting, &stopped)
	if errors.Is(err, sql.ErrNoRows) {
		return expires, ErrNotFound
	}
	if err != nil {
		return expires, err
	}
	if deleting || stopped || !now.Before(expires) {
		return expires, ErrConflict
	}
	return expires, nil
}

func (s *Store) PatchPush(ctx context.Context, id string, interval *int, status *string, now time.Time) (PushSubscription, error) {
	if interval != nil && *interval != 30 {
		return PushSubscription{}, ErrConflict
	}
	var p PushSubscription
	err := s.Transaction(ctx, id, func(tx *Store) error {
		if err := tx.lockPushControl(ctx); err != nil {
			return err
		}
		if _, err := tx.pushEligible(ctx, id, now); err != nil {
			return err
		}
		err := tx.q.QueryRowContext(ctx, `SELECT interval_minutes,status,expires_at FROM push_subscriptions WHERE memory_session_id=$1 FOR UPDATE`, id).Scan(&p.IntervalMinutes, &p.Status, &p.ExpiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if interval != nil {
			p.IntervalMinutes = *interval
		}
		if status != nil {
			p.Status = *status
		}
		p.MemorySessionID = id
		if p.Status == "active" {
			p.NextSendAt = now.Add(time.Duration(p.IntervalMinutes) * time.Minute)
		}
		var next any
		if p.Status == "active" {
			next = p.NextSendAt
		}
		_, err = tx.q.ExecContext(ctx, `UPDATE push_subscriptions SET interval_minutes=$2,status=$3,next_send_at=$4 WHERE memory_session_id=$1`, id, p.IntervalMinutes, p.Status, next)
		return err
	})
	return p, err
}

// PostponePush is called in the same session transaction as the first ready transition.
func (s *Store) PostponePush(ctx context.Context, id string, now time.Time) error {
	_, err := s.q.ExecContext(ctx, `UPDATE push_subscriptions SET next_send_at=$2 + interval_minutes * interval '1 minute'
  WHERE memory_session_id=$1 AND status='active' AND expires_at>$2`, id, now)
	return err
}

// StopPushForMemory removes credentials but allows the participant to subscribe
// again. Arrival and Live stop must instead use EndPushForMemory.
func (s *Store) StopPushForMemory(ctx context.Context, id string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE memory_session_id=$1`, id)
	return err
}

// EndPushForMemory is the terminal arrival hook. Call on the same Store tx
// committing the first arrival, using its Memory ID, even without Live.
func (s *Store) EndPushForMemory(ctx context.Context, id string) error {
	result, err := s.q.ExecContext(ctx, `UPDATE memory_sessions SET push_stopped=TRUE WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.StopPushForMemory(ctx, id)
}

// ClearPush removes credentials if push is disabled, including after rollback.
func (s *Store) ClearPush(ctx context.Context) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM push_subscriptions`)
	return err
}

func (s *Store) stopPushForLive(ctx context.Context, id string) error {
	var memoryID string
	err := s.q.QueryRowContext(ctx, `SELECT m.id FROM memory_sessions m JOIN experiences e ON e.memory_session_id=m.id
  WHERE e.live_session_id=$1 FOR UPDATE OF m`, id).Scan(&memoryID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.EndPushForMemory(ctx, memoryID)
}

func (s *Store) DeletePush(ctx context.Context, id string) error {
	return s.Transaction(ctx, id, func(tx *Store) error { return tx.StopPushForMemory(ctx, id) })
}

func (s *Store) DuePushIDs(ctx context.Context, now time.Time, limit int) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT memory_session_id FROM push_subscriptions
 WHERE status='active' AND next_send_at<=$1
 AND (SELECT stopped_at IS NULL FROM notification_control WHERE singleton=TRUE)
 ORDER BY next_send_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DeliverPush holds the notification-control and per-session transaction locks
// through the provider response. Stops, deletion and photo completion cannot race
// a send once acquired. A provider-accepted request followed by a database failure
// can be retried; exactly-once delivery cannot be guaranteed by Web Push.
func (s *Store) DeliverPush(ctx context.Context, id string, now time.Time, send func(PushSubscription) (int, error)) error {
	return s.Transaction(ctx, id, func(tx *Store) error {
		if err := tx.lockPushControl(ctx); err != nil {
			if errors.Is(err, ErrPushStopped) {
				return nil
			}
			return err
		}
		expires, err := tx.pushEligible(ctx, id, now)
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
			return tx.StopPushForMemory(ctx, id)
		}
		if err != nil {
			return err
		}
		var p PushSubscription
		p.MemorySessionID = id
		err = tx.q.QueryRowContext(ctx, `SELECT endpoint_encrypted,keys_encrypted,interval_minutes,status,next_send_at,expires_at
   FROM push_subscriptions WHERE memory_session_id=$1 FOR UPDATE`, id).Scan(&p.EndpointEncrypted, &p.KeysEncrypted, &p.IntervalMinutes, &p.Status, &p.NextSendAt, &p.ExpiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if p.Status != "active" || now.Before(p.NextSendAt) {
			return nil
		}
		if !now.Before(expires) || !now.Before(p.ExpiresAt) {
			return tx.StopPushForMemory(ctx, id)
		}
		status, sendErr := send(p)
		if status == 404 || status == 410 {
			return tx.StopPushForMemory(ctx, id)
		}
		next := now.Add(time.Duration(p.IntervalMinutes) * time.Minute)
		if sendErr != nil || status < 200 || status >= 300 {
			next = now.Add(time.Minute)
		}
		_, err = tx.q.ExecContext(ctx, `UPDATE push_subscriptions SET next_send_at=$2 WHERE memory_session_id=$1`, id, next)
		return err
	})
}
