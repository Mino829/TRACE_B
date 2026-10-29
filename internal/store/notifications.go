package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrPushStopped = errors.New("notifications are stopped")

// PushStopped reports the durable operator stop, independent of runtime config.
func (s *Store) PushStopped(ctx context.Context) (bool, error) {
	var stopped bool
	err := s.q.QueryRowContext(ctx, `SELECT stopped_at IS NOT NULL FROM notification_control WHERE singleton=TRUE`).Scan(&stopped)
	return stopped, err
}

// lockPushControl keeps the shared lock until the caller's transaction ends,
// including the provider response. Take it before locking a Memory or subscription.
func (s *Store) lockPushControl(ctx context.Context) error {
	var stopped bool
	if err := s.q.QueryRowContext(ctx, `SELECT stopped_at IS NOT NULL FROM notification_control WHERE singleton=TRUE FOR SHARE`).Scan(&stopped); err != nil {
		return err
	}
	if stopped {
		return ErrPushStopped
	}
	return nil
}

// StopNotifications serializes with subscription changes and in-flight sends,
// then commits the durable stop, credential removal and audit record together.
func (s *Store) StopNotifications(ctx context.Context, now time.Time) error {
	if OperatorActor(ctx) == "" {
		return ErrConflict
	}
	return s.Transaction(ctx, "", func(tx *Store) error {
		var stoppedAt sql.NullTime
		if err := tx.q.QueryRowContext(ctx, `SELECT stopped_at FROM notification_control WHERE singleton=TRUE FOR UPDATE`).Scan(&stoppedAt); err != nil {
			return err
		}
		outcome := "already_stopped"
		if !stoppedAt.Valid {
			if _, err := tx.q.ExecContext(ctx, `UPDATE notification_control SET stopped_at=$1 WHERE singleton=TRUE`, now); err != nil {
				return err
			}
			outcome = "stopped"
		}
		if err := tx.ClearPush(ctx); err != nil {
			return err
		}
		return tx.auditOperator(ctx, "notifications.stop", "notifications", outcome, now)
	})
}
