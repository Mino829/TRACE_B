package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func fallbackNow() time.Time {
	return time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
}

func fallbackLease(t *testing.T, f *testdb.Fixture, now time.Time) {
	t.Helper()
	acquired, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease", now)
	if err != nil || !acquired {
		t.Fatalf("lease: acquired=%t err=%v", acquired, err)
	}
}

func fallbackReplay(t *testing.T, s *store.Store, event, arrival, action, want string, now time.Time) {
	t.Helper()
	status, err := s.DisplayReplay(t.Context(), "holder", "lease", event, arrival, "arrival.replay."+action, now)
	if err != nil || status != want {
		t.Fatalf("%s: status=%q err=%v", action, status, err)
	}
}

func requireFallback(t *testing.T, state store.FallbackState, mode string, changedAt, deadline time.Time) {
	t.Helper()
	if state.Mode != mode || !state.ChangedAt.Equal(changedAt) {
		t.Fatalf("fallback: got=%+v want mode=%s changedAt=%s", state, mode, changedAt)
	}
	if deadline.IsZero() {
		if state.SwitchAt != nil {
			t.Fatalf("non-pending mode retained deadline: %+v", state)
		}
	} else if state.SwitchAt == nil || !state.SwitchAt.Equal(deadline) {
		t.Fatalf("pending deadline: got=%+v want=%s", state, deadline)
	}
}

func readFallback(t *testing.T, s *store.Store) store.FallbackState {
	t.Helper()
	state, err := s.ReadFallback(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func fallbackArrivalState(t *testing.T, f *testdb.Fixture, id, wantStatus, wantReason string) {
	t.Helper()
	var status, reason string
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT replay_status,COALESCE(skip_reason,'') FROM arrival_events WHERE id=$1`, id).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || reason != wantReason {
		t.Fatalf("arrival %s: status=%s reason=%s want=%s/%s", id, status, reason, wantStatus, wantReason)
	}
}

func fallbackTransitions(t *testing.T, f *testdb.Fixture) []store.DisplayEvent {
	t.Helper()
	events, err := f.Store.DisplayEvents(t.Context(), 0, 256)
	if err != nil {
		t.Fatal(err)
	}
	var transitions []store.DisplayEvent
	for _, event := range events {
		if event.Type == "system.fallback.changed" {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(event.Data, &fields); err != nil || len(fields) != 3 || fields["mode"] == nil || fields["switchAt"] == nil || fields["changedAt"] == nil {
				t.Fatalf("fallback event is not exactly FallbackState: %s %v", event.Data, err)
			}
			transitions = append(transitions, event)
		}
	}
	return transitions
}

func requireFallbackAudit(t *testing.T, f *testdb.Fixture, enables, disables int) {
	t.Helper()
	var gotEnables, gotDisables, unsafe int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT
 COUNT(*) FILTER (WHERE action='fallback.enable'),
 COUNT(*) FILTER (WHERE action='fallback.disable'),
 COUNT(*) FILTER (WHERE operator_id<>'verified-test-operator' OR target_id_short<>'fallback')
 FROM arrival_operator_audit WHERE action IN ('fallback.enable','fallback.disable')`).Scan(&gotEnables, &gotDisables, &unsafe); err != nil {
		t.Fatal(err)
	}
	if gotEnables != enables || gotDisables != disables || unsafe != 0 {
		t.Fatalf("fallback audit: enable=%d disable=%d unsafe=%d", gotEnables, gotDisables, unsafe)
	}
}

func reopenFallbackStore(t *testing.T, f *testdb.Fixture) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), f.URL, 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.EnableDisplay()
	return s
}

func TestFallbackImmediateEnableRetainsQueueAndRequiresExplicitResume(t *testing.T) {
	f := testdb.Open(t)
	f.Store.EnableDisplay()
	now := fallbackNow()
	first := fifoArrival(t, f, "fallback-first", now)
	second := fifoArrival(t, f, "fallback-second", now.Add(time.Microsecond))
	fallbackLease(t, f, now)
	enabledAt := now.Add(time.Second)
	state, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, enabledAt)
	if err != nil {
		t.Fatal(err)
	}
	requireFallback(t, state, "fallback", enabledAt, time.Time{})
	state, err = f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	requireFallback(t, state, "fallback", enabledAt, time.Time{})
	if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "blocked", first, "arrival.replay.started", now.Add(2*time.Second)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("fallback start: %v", err)
	}
	third := fifoArrival(t, f, "fallback-new", now.Add(3*time.Second))
	if candidates, err := f.Store.ReplayCandidates(t.Context(), now.Add(3*time.Second)); err != nil || len(candidates) != 0 {
		t.Fatalf("fallback candidates: %+v %v", candidates, err)
	}
	snapshot, err := f.Store.DisplaySnapshot(t.Context(), now.Add(3*time.Second))
	if err != nil || len(snapshot.Arrivals) != 3 || snapshot.Arrivals[0].ArrivalID != first || snapshot.Arrivals[1].ArrivalID != second || snapshot.Arrivals[2].ArrivalID != third {
		t.Fatalf("fallback snapshot lost FIFO queue: %+v %v", snapshot, err)
	}
	requireFallback(t, snapshot.Fallback, "fallback", enabledAt, time.Time{})
	status, err := f.Store.ReadOperatorStatus(t.Context(), now.Add(3*time.Second))
	if err != nil || status.Queued != 3 || status.Playing != 0 || !status.PrimaryLeaseActive {
		t.Fatalf("operator status: %+v %v", status, err)
	}
	requireFallback(t, status.Fallback, "fallback", enabledAt, time.Time{})
	if transitions := fallbackTransitions(t, f); len(transitions) != 1 {
		t.Fatalf("repeated enable emitted transitions: %+v", transitions)
	}
	resumedAt := now.Add(4 * time.Second)
	state, err = f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), false, resumedAt)
	if err != nil {
		t.Fatal(err)
	}
	requireFallback(t, state, "normal", resumedAt, time.Time{})
	state, err = f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), false, now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	requireFallback(t, state, "normal", resumedAt, time.Time{})
	if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "bypass", second, "arrival.replay.started", now.Add(5*time.Second)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("resume bypassed FIFO: %v", err)
	}
	fallbackReplay(t, f.Store, "resume-first", first, "started", "playing", now.Add(5*time.Second))
	fallbackReplay(t, f.Store, "resume-first-end", first, "completed", "completed", now.Add(6*time.Second))
	fallbackReplay(t, f.Store, "resume-second", second, "started", "playing", now.Add(7*time.Second))
	if transitions := fallbackTransitions(t, f); len(transitions) != 2 {
		t.Fatalf("idempotent controls emitted transitions: %+v", transitions)
	}
	requireFallbackAudit(t, f, 2, 2)
}

func TestFallbackContinuesAcceptingArrivalsUnderExistingQueueLimit(t *testing.T) {
	f := testdb.Open(t)
	f.Store.EnableDisplay()
	now := fallbackNow()
	if _, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, now); err != nil {
		t.Fatal(err)
	}
	for i := range store.ArrivalQueueLimit {
		fifoArrival(t, f, fmt.Sprintf("fallback-capacity-%02d", i), now.Add(time.Duration(i)*time.Microsecond))
	}
	memoryID := fifoExperience(t, f, "fallback-overflow", now)
	if _, err := f.Store.AcceptArrival(t.Context(), memoryID, "", "fallback-overflow", "booth", "receipt-overflow", "request", now.Add(time.Second)); !errors.Is(err, store.ErrArrivalQueueFull) {
		t.Fatalf("fallback queue limit: %v", err)
	}
	snapshot, err := f.Store.DisplaySnapshot(t.Context(), now.Add(time.Second))
	if err != nil || len(snapshot.Arrivals) != store.ArrivalQueueLimit {
		t.Fatalf("fallback discarded queued arrivals: count=%d err=%v", len(snapshot.Arrivals), err)
	}
}

func TestFallbackPendingFinishesOnRendererCompletionOrFailure(t *testing.T) {
	for _, action := range []string{"completed", "failed"} {
		t.Run(action, func(t *testing.T) {
			f := testdb.Open(t)
			f.Store.EnableDisplay()
			now := fallbackNow()
			playing := fifoArrival(t, f, "pending-playing", now)
			queued := fifoArrival(t, f, "pending-queued", now.Add(time.Microsecond))
			fallbackLease(t, f, now)
			fallbackReplay(t, f.Store, "pending-start", playing, "started", "playing", now.Add(time.Second))
			enabledAt := now.Add(2 * time.Second)
			state, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, enabledAt)
			if err != nil {
				t.Fatal(err)
			}
			requireFallback(t, state, "pending", enabledAt, enabledAt.Add(30*time.Second))
			if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "pending-blocked", queued, "arrival.replay.started", now.Add(3*time.Second)); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("pending start: %v", err)
			}
			finishedAt := now.Add(10 * time.Second)
			wantStatus, wantReason := "completed", ""
			if action == "failed" {
				wantStatus, wantReason = "skipped", "renderer_failure"
			}
			fallbackReplay(t, f.Store, "pending-finish", playing, action, wantStatus, finishedAt)
			requireFallback(t, readFallback(t, f.Store), "fallback", finishedAt, time.Time{})
			fallbackArrivalState(t, f, playing, wantStatus, wantReason)
			fallbackArrivalState(t, f, queued, "queued", "")
			fallbackReplay(t, f.Store, "pending-finish", playing, action, wantStatus, finishedAt.Add(time.Second))
			transitions := fallbackTransitions(t, f)
			if len(transitions) != 2 {
				t.Fatalf("completion or receipt retry emitted duplicate transition: %+v", transitions)
			}
			var eventState store.FallbackState
			if err := json.Unmarshal(transitions[1].Data, &eventState); err != nil {
				t.Fatal(err)
			}
			requireFallback(t, eventState, "fallback", finishedAt, time.Time{})
		})
	}
}

func TestFallbackDeadlinePersistsAndLateCompletionCommitsTimeout(t *testing.T) {
	for _, lateness := range []time.Duration{0, time.Microsecond} {
		t.Run(lateness.String(), func(t *testing.T) {
			f := testdb.Open(t)
			f.Store.EnableDisplay()
			now := fallbackNow()
			playing := fifoArrival(t, f, "deadline-playing", now)
			queued := fifoArrival(t, f, "deadline-queued", now.Add(time.Microsecond))
			fallbackLease(t, f, now)
			fallbackReplay(t, f.Store, "deadline-start", playing, "started", "playing", now.Add(time.Second))
			enabledAt := now.Add(2 * time.Second)
			deadline := enabledAt.Add(30 * time.Second)
			if _, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, enabledAt); err != nil {
				t.Fatal(err)
			}
			if renewed, err := f.Store.RenewDisplayLease(t.Context(), "holder", "lease", now.Add(10*time.Second)); err != nil || !renewed {
				t.Fatalf("renew lease across operator deadline: %t %v", renewed, err)
			}
			newArrival := fifoArrival(t, f, "deadline-new", now.Add(15*time.Second))
			restarted := reopenFallbackStore(t, f)
			requireFallback(t, readFallback(t, restarted), "pending", enabledAt, deadline)
			before := deadline.Add(-time.Microsecond)
			if err := restarted.RefreshDisplay(t.Context(), before); err != nil {
				t.Fatal(err)
			}
			fallbackArrivalState(t, f, playing, "playing", "")
			state, err := restarted.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, before)
			if err != nil {
				t.Fatal(err)
			}
			requireFallback(t, state, "pending", enabledAt, deadline)
			at := deadline.Add(lateness)
			if _, err := restarted.DisplayReplay(t.Context(), "holder", "lease", "deadline-late-finish", playing, "arrival.replay.completed", at); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("completion at/after deadline accepted: %v", err)
			}
			requireFallback(t, readFallback(t, restarted), "fallback", at, time.Time{})
			fallbackArrivalState(t, f, playing, "skipped", "operator")
			fallbackArrivalState(t, f, queued, "queued", "")
			fallbackArrivalState(t, f, newArrival, "queued", "")
			var rejectedReceipts, skippedEvents int
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_renderer_receipts WHERE event_id='deadline-late-finish'`).Scan(&rejectedReceipts); err != nil || rejectedReceipts != 0 {
				t.Fatalf("rejected completion receipt: %d %v", rejectedReceipts, err)
			}
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events WHERE kind='arrival.replay.skipped' AND data->>'arrivalId'=$1 AND data->>'reason'='operator'`, playing).Scan(&skippedEvents); err != nil || skippedEvents != 1 {
				t.Fatalf("timeout skip event: %d %v", skippedEvents, err)
			}
			// A receipt records the original accepted result, not today's arrival state.
			fallbackReplay(t, restarted, "deadline-start", playing, "started", "playing", at.Add(time.Second))
			if _, err := restarted.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, at.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if len(fallbackTransitions(t, f)) != 2 {
				t.Fatal("deadline refresh or repeated enable duplicated transition")
			}
			resumedAt := at.Add(2 * time.Second)
			if _, err := restarted.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), false, resumedAt); err != nil {
				t.Fatal(err)
			}
			fallbackArrivalState(t, f, playing, "skipped", "operator")
			if err := restarted.RetryDisplayArrival(t.Context(), playing, resumedAt); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("operator timeout became renderer retry: %v", err)
			}
			fallbackReplay(t, restarted, "deadline-resume-next", queued, "started", "playing", resumedAt)
			requireFallbackAudit(t, f, 3, 1)
		})
	}
}

func TestFallbackDisableCancelsPendingWithoutInterruptingCurrentReplay(t *testing.T) {
	f := testdb.Open(t)
	f.Store.EnableDisplay()
	now := fallbackNow()
	playing := fifoArrival(t, f, "cancel-playing", now)
	queued := fifoArrival(t, f, "cancel-queued", now.Add(time.Microsecond))
	fallbackLease(t, f, now)
	fallbackReplay(t, f.Store, "cancel-start", playing, "started", "playing", now.Add(time.Second))
	if _, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	resumedAt := now.Add(10 * time.Second)
	state, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), false, resumedAt)
	if err != nil {
		t.Fatal(err)
	}
	requireFallback(t, state, "normal", resumedAt, time.Time{})
	fallbackArrivalState(t, f, playing, "playing", "")
	fallbackReplay(t, f.Store, "cancel-finish", playing, "completed", "completed", now.Add(11*time.Second))
	fallbackReplay(t, f.Store, "cancel-next", queued, "started", "playing", now.Add(12*time.Second))
	requireFallback(t, readFallback(t, f.Store), "normal", resumedAt, time.Time{})
	if len(fallbackTransitions(t, f)) != 2 {
		t.Fatal("cancelled deadline emitted another transition")
	}
}

func TestFallbackLeaseLossAndReconnectNeverResume(t *testing.T) {
	for _, loss := range []string{"release", "expiry", "simultaneous-deadline"} {
		t.Run(loss, func(t *testing.T) {
			f := testdb.Open(t)
			f.Store.EnableDisplay()
			now := fallbackNow()
			playing := fifoArrival(t, f, "loss-playing", now)
			queued := fifoArrival(t, f, "loss-queued", now.Add(time.Microsecond))
			fallbackLease(t, f, now)
			fallbackReplay(t, f.Store, "loss-start", playing, "started", "playing", now)
			enabledAt := now.Add(time.Second)
			if loss == "simultaneous-deadline" {
				enabledAt = now
			}
			if _, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, enabledAt); err != nil {
				t.Fatal(err)
			}
			lostAt := now.Add(30 * time.Second)
			if loss == "release" {
				lostAt = now.Add(3 * time.Second)
				if err := f.Store.ReleaseDisplayLease(t.Context(), "holder", "lease", lostAt); err != nil {
					t.Fatal(err)
				}
			} else if err := f.Store.RefreshDisplay(t.Context(), lostAt); err != nil {
				t.Fatal(err)
			}
			wantReason := "renderer_failure"
			if loss == "simultaneous-deadline" {
				wantReason = "operator"
			}
			fallbackArrivalState(t, f, playing, "skipped", wantReason)
			fallbackArrivalState(t, f, queued, "queued", "")
			restarted := reopenFallbackStore(t, f)
			acquired, err := restarted.AcquireDisplayLease(t.Context(), "new-holder", "new-lease", lostAt.Add(time.Second))
			if err != nil || !acquired {
				t.Fatalf("reconnect lease: %t %v", acquired, err)
			}
			requireFallback(t, readFallback(t, restarted), "fallback", lostAt, time.Time{})
			if _, err := restarted.DisplayReplay(t.Context(), "new-holder", "new-lease", "loss-blocked", queued, "arrival.replay.started", lostAt.Add(time.Second)); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("reconnect resumed queue: %v", err)
			}
			if _, err := restarted.DisplayReplay(t.Context(), "holder", "lease", "loss-stale", playing, "arrival.replay.completed", lostAt.Add(time.Second)); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("stale renderer escaped lease fencing: %v", err)
			}
			if err := restarted.RefreshDisplay(t.Context(), lostAt.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			requireFallback(t, readFallback(t, restarted), "fallback", lostAt, time.Time{})
		})
	}
}

func TestFallbackMemoryExpiryFinishesPendingWithoutDroppingQueue(t *testing.T) {
	f := testdb.Open(t)
	f.Store.EnableDisplay()
	now := fallbackNow()
	playing := fifoArrival(t, f, "expiry-playing", now)
	queued := fifoArrival(t, f, "expiry-queued", now.Add(time.Microsecond))
	fallbackLease(t, f, now)
	fallbackReplay(t, f.Store, "expiry-start", playing, "started", "playing", now.Add(time.Second))
	if _, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	expiredAt := now.Add(10 * time.Second)
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET expires_at=$1 WHERE id='fifo-memory-expiry-playing'`, expiredAt); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.RefreshDisplay(t.Context(), expiredAt); err != nil {
		t.Fatal(err)
	}
	requireFallback(t, readFallback(t, f.Store), "fallback", expiredAt, time.Time{})
	fallbackArrivalState(t, f, playing, "skipped", "deleted")
	fallbackArrivalState(t, f, queued, "queued", "")
}

func TestFallbackAuditFailureRollsBackControlAndTransitionEvents(t *testing.T) {
	for _, playing := range []bool{false, true} {
		t.Run(fmt.Sprintf("playing=%t", playing), func(t *testing.T) {
			f := testdb.Open(t)
			f.Store.EnableDisplay()
			now := fallbackNow()
			if playing {
				id := fifoArrival(t, f, "audit-playing", now)
				fallbackLease(t, f, now)
				fallbackReplay(t, f.Store, "audit-start", id, "started", "playing", now)
			}
			initial := readFallback(t, f.Store)
			if _, err := f.SQL.ExecContext(t.Context(), `ALTER TABLE arrival_operator_audit ADD CONSTRAINT reject_fallback_enable CHECK (action<>'fallback.enable')`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, now.Add(time.Second)); err == nil {
				t.Fatal("failed audit reported successful enable")
			}
			requireFallback(t, readFallback(t, f.Store), "normal", initial.ChangedAt, time.Time{})
			if len(fallbackTransitions(t, f)) != 0 {
				t.Fatal("failed audit committed transition event")
			}
			requireFallbackAudit(t, f, 0, 0)
		})
	}
}

func TestFallbackDeadlineRollbackKeepsReplayStateEventsAndCursorAtomic(t *testing.T) {
	for _, failure := range []string{"audit", "event"} {
		t.Run(failure, func(t *testing.T) {
			f := testdb.Open(t)
			f.Store.EnableDisplay()
			now := fallbackNow()
			playing := fifoArrival(t, f, "rollback-playing", now)
			queued := fifoArrival(t, f, "rollback-queued", now.Add(time.Microsecond))
			fallbackLease(t, f, now)
			fallbackReplay(t, f.Store, "rollback-start", playing, "started", "playing", now)
			enabledAt := now.Add(time.Second)
			if _, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, enabledAt); err != nil {
				t.Fatal(err)
			}
			before, err := f.Store.DisplaySnapshot(t.Context(), enabledAt)
			if err != nil {
				t.Fatal(err)
			}
			constraint := `ALTER TABLE arrival_operator_audit ADD CONSTRAINT reject_fallback CHECK (action<>'fallback.enable') NOT VALID`
			if failure == "event" {
				constraint = `ALTER TABLE display_events ADD CONSTRAINT reject_fallback CHECK (kind<>'system.fallback.changed' OR data->>'mode'<>'fallback')`
			}
			if _, err := f.SQL.ExecContext(t.Context(), constraint); err != nil {
				t.Fatal(err)
			}
			deadline := enabledAt.Add(30 * time.Second)
			if failure == "audit" {
				_, err = f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, deadline)
			} else {
				err = f.Store.RefreshDisplay(t.Context(), deadline)
			}
			if err == nil {
				t.Fatal("injected failure reported success")
			}
			requireFallback(t, readFallback(t, f.Store), "pending", enabledAt, deadline)
			fallbackArrivalState(t, f, playing, "playing", "")
			fallbackArrivalState(t, f, queued, "queued", "")
			after, err := f.Store.DisplaySnapshot(t.Context(), deadline)
			if err != nil || after.Cursor != before.Cursor {
				t.Fatalf("rollback changed event cursor: before=%d after=%d err=%v", before.Cursor, after.Cursor, err)
			}
			if len(fallbackTransitions(t, f)) != 1 {
				t.Fatal("failed timeout committed transition")
			}
			requireFallbackAudit(t, f, 1, 0)
		})
	}
}

func TestFallbackDisableAuditAndOuterTransactionRollback(t *testing.T) {
	f := testdb.Open(t)
	f.Store.EnableDisplay()
	now := fallbackNow()
	if _, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `ALTER TABLE arrival_operator_audit ADD CONSTRAINT reject_fallback_disable CHECK (action<>'fallback.disable')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), false, now.Add(time.Second)); err == nil {
		t.Fatal("failed disable audit reported success")
	}
	requireFallback(t, readFallback(t, f.Store), "fallback", now, time.Time{})
	if _, err := f.SQL.ExecContext(t.Context(), `ALTER TABLE arrival_operator_audit DROP CONSTRAINT reject_fallback_disable`); err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("intentional rollback")
	if err := f.Store.Transaction(t.Context(), "", func(tx *store.Store) error {
		if _, err := tx.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), false, now.Add(2*time.Second)); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("outer transaction: %v", err)
	}
	requireFallback(t, readFallback(t, f.Store), "fallback", now, time.Time{})
	if len(fallbackTransitions(t, f)) != 1 {
		t.Fatal("failed disable leaked transition")
	}
	requireFallbackAudit(t, f, 1, 0)
}

func TestFallbackControlReturnsDurableSubmicrosecondDeadline(t *testing.T) {
	f := testdb.Open(t)
	f.Store.EnableDisplay()
	now := fallbackNow().Add(123 * time.Nanosecond)
	arrival := fifoArrival(t, f, "precision-playing", now)
	fallbackLease(t, f, now)
	fallbackReplay(t, f.Store, "precision-start", arrival, "started", "playing", now)
	first, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	persisted := readFallback(t, f.Store)
	repeated, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "verified-test-operator"), true, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for name, state := range map[string]store.FallbackState{"first": first, "repeat": repeated} {
		if state.Mode != "pending" || state.SwitchAt == nil || persisted.SwitchAt == nil {
			t.Fatalf("%s: missing pending deadline: %+v", name, state)
		}
		if !state.SwitchAt.Equal(*persisted.SwitchAt) || !state.ChangedAt.Equal(persisted.ChangedAt) {
			t.Fatalf("%s changed durable timestamps: response=%+v persisted=%+v", name, state, persisted)
		}
	}
}
