package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrRegistrationClosed = errors.New("registration is closed")

// RegistrationClosed reports the durable operator stop, independent of config.
func (s *Store) RegistrationClosed(ctx context.Context) (bool, error) {
	var closed bool
	err := s.q.QueryRowContext(ctx, `SELECT closed_at IS NOT NULL FROM registration_control WHERE singleton=TRUE`).Scan(&closed)
	return closed, err
}

// CloseRegistration waits for in-flight enrollments and commits the stop with
// its audit record. Existing sessions and their expiry times are unchanged.
func (s *Store) CloseRegistration(ctx context.Context, now time.Time) error {
	if OperatorActor(ctx) == "" {
		return ErrConflict
	}
	return s.Transaction(ctx, "", func(tx *Store) error {
		var closedAt sql.NullTime
		if err := tx.q.QueryRowContext(ctx, `SELECT closed_at FROM registration_control WHERE singleton=TRUE FOR UPDATE`).Scan(&closedAt); err != nil {
			return err
		}
		outcome := "already_closed"
		if !closedAt.Valid {
			if _, err := tx.q.ExecContext(ctx, `UPDATE registration_control SET closed_at=$1 WHERE singleton=TRUE`, now); err != nil {
				return err
			}
			outcome = "closed"
		}
		return tx.auditOperator(ctx, "registration.close", "registration", outcome, now)
	})
}
