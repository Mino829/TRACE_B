package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// FallbackState is shared by renderer snapshots, transition events and operator status.
// A pending deadline is durable and never extended by repeated enable requests.
type FallbackState struct {
	Mode      string     `json:"mode"`
	SwitchAt  *time.Time `json:"switchAt"`
	ChangedAt time.Time  `json:"changedAt"`
}

func (s *Store) ReadFallback(ctx context.Context) (FallbackState, error) {
	var state FallbackState
	err := s.q.QueryRowContext(ctx, `SELECT mode,switch_at,changed_at FROM display_fallback_control WHERE singleton=TRUE`).
		Scan(&state.Mode, &state.SwitchAt, &state.ChangedAt)
	return state, err
}

// SetFallback serializes with replay and lease changes. State, events and the
// privacy-safe operator audit either all commit or all roll back.
func (s *Store) SetFallback(ctx context.Context, enabled bool, now time.Time) (FallbackState, error) {
	var state FallbackState
	err := s.Transaction(ctx, "display:lease", func(tx *Store) error {
		var err error
		state, err = tx.advanceFallback(ctx, now)
		if err != nil {
			return err
		}
		action, mode := "fallback.disable", "normal"
		var switchAt *time.Time
		if enabled {
			action = "fallback.enable"
			mode = state.Mode
			if mode == "normal" {
				playing, err := tx.hasPlayingReplay(ctx)
				if err != nil {
					return err
				}
				mode = "fallback"
				if playing {
					mode = "pending"
					deadline := now.Add(30 * time.Second)
					switchAt = &deadline
				}
			}
		}
		outcome := "already_" + state.Mode
		if mode != state.Mode {
			state, err = tx.changeFallback(ctx, mode, switchAt, now)
			if err != nil {
				return err
			}
			outcome = state.Mode
		}
		_, err = tx.q.ExecContext(ctx, `INSERT INTO arrival_operator_audit
 (operator_id,action,target_id_short,result,created_at) VALUES ('arrival_issuer',$1,'fallback',$2,$3)`, action, outcome, now)
		return err
	})
	return state, err
}

func (s *Store) hasPlayingReplay(ctx context.Context) (bool, error) {
	var playing bool
	err := s.q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM arrival_events WHERE replay_status='playing')`).Scan(&playing)
	return playing, err
}

// advanceFallback must run under display:lease, before accepting renderer
// messages or handling lease loss. Due operator deadlines take precedence over
// a late completion or simultaneous renderer expiry; queued arrivals are untouched.
func (s *Store) advanceFallback(ctx context.Context, now time.Time) (FallbackState, error) {
	state, err := s.ReadFallback(ctx)
	if err != nil || state.Mode != "pending" {
		return state, err
	}
	if !now.Before(*state.SwitchAt) {
		var id string
		err := s.q.QueryRowContext(ctx, `UPDATE arrival_events
 SET replay_status='skipped',skip_reason='operator',replay_finished_at=$1
 WHERE replay_status='playing' RETURNING id`, now).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return state, err
		}
		if err == nil {
			if _, err := s.AppendDisplayEvent(ctx, "arrival.replay.skipped", map[string]string{"arrivalId": id, "reason": "operator"}, now); err != nil {
				return state, err
			}
		}
	} else {
		playing, err := s.hasPlayingReplay(ctx)
		if err != nil || playing {
			return state, err
		}
	}
	return s.changeFallback(ctx, "fallback", nil, now)
}

func (s *Store) changeFallback(ctx context.Context, mode string, switchAt *time.Time, now time.Time) (FallbackState, error) {
	// Return the persisted precision in responses and events, including on systems
	// whose clock resolution is finer than PostgreSQL's microsecond timestamps.
	var state FallbackState
	if err := s.q.QueryRowContext(ctx, `UPDATE display_fallback_control SET mode=$1,switch_at=$2,changed_at=$3
 WHERE singleton=TRUE RETURNING mode,switch_at,changed_at`, mode, switchAt, now).
		Scan(&state.Mode, &state.SwitchAt, &state.ChangedAt); err != nil {
		return state, err
	}
	_, err := s.AppendDisplayEvent(ctx, "system.fallback.changed", state, now)
	return state, err
}
