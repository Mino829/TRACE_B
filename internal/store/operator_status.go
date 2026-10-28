package store

import (
	"context"
	"time"
)

// OperatorStatus contains aggregates only, never participant or renderer IDs.
type OperatorStatus struct {
	Queued               int
	Playing              int
	PrimaryLeaseActive   bool
	RegistrationClosed   bool
	NotificationsStopped bool
	Fallback             FallbackState `json:"fallback"`
}

// ReadOperatorStatus observes durable queue and lease state in one SQL snapshot.
// It does not renew leases, expire sessions, or otherwise change display state.
func (s *Store) ReadOperatorStatus(ctx context.Context, now time.Time) (OperatorStatus, error) {
	var status OperatorStatus
	err := s.q.QueryRowContext(ctx, `SELECT
 COUNT(*) FILTER (WHERE a.replay_status='queued'),
 COUNT(*) FILTER (WHERE a.replay_status='playing'),
 EXISTS (SELECT 1 FROM display_primary_lease
  WHERE singleton=TRUE AND holder_id IS NOT NULL AND lease_id IS NOT NULL AND expires_at>$1),
 (SELECT closed_at IS NOT NULL FROM registration_control WHERE singleton=TRUE),
 (SELECT stopped_at IS NOT NULL FROM notification_control WHERE singleton=TRUE),
 (SELECT mode FROM display_fallback_control WHERE singleton=TRUE),
 (SELECT switch_at FROM display_fallback_control WHERE singleton=TRUE),
 (SELECT changed_at FROM display_fallback_control WHERE singleton=TRUE)
 FROM arrival_events a JOIN memory_sessions m ON m.id=a.memory_session_id
 WHERE a.replay_status IN ('queued','playing') AND m.deleting=FALSE AND m.expires_at>$1`, now).
		Scan(&status.Queued, &status.Playing, &status.PrimaryLeaseActive, &status.RegistrationClosed, &status.NotificationsStopped,
			&status.Fallback.Mode, &status.Fallback.SwitchAt, &status.Fallback.ChangedAt)
	return status, err
}
