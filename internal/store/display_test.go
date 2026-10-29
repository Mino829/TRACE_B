package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func TestDisplayCursorGapForcesSnapshot(t *testing.T) {
	f := testdb.Open(t)
	f.Store.EnableDisplay()
	now := time.Now().UTC()
	for range 3 {
		if _, err := f.Store.AppendDisplayEvent(t.Context(), "arrival.replay.requested", map[string]string{"arrivalId": "test"}, now); err != nil {
			t.Fatal(err)
		}
	}
	events, err := f.Store.DisplayEvents(t.Context(), 0, 10)
	if err != nil || len(events) != 3 || events[0].ID+1 != events[1].ID || events[1].ID+1 != events[2].ID {
		t.Fatalf("events not ordered: %v %v", events, err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `DELETE FROM display_events WHERE id=$1`, events[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.DisplayEvents(t.Context(), events[0].ID, 10); !errors.Is(err, store.ErrDisplayResync) {
		t.Fatalf("gap not detected: %v", err)
	}
}

func TestDisplayLeaseTimeoutSkipsAndFencesOldPrimary(t *testing.T) {
	f := testdb.Open(t)
	f.Store.EnableDisplay()
	now := time.Now().UTC()
	v := fixtureExperience(t, f, now)
	if _, err := f.Store.AcceptArrival(t.Context(), v.MemorySessionID, v.LinkTokenHash, "arrival-lease", "booth", "receipt", "request", now); err != nil {
		t.Fatal(err)
	}
	acquired, err := f.Store.AcquireDisplayLease(t.Context(), "renderer-one", "lease-one", now)
	if err != nil || !acquired {
		t.Fatalf("primary lease: %v %v", acquired, err)
	}
	acquired, err = f.Store.AcquireDisplayLease(t.Context(), "renderer-two", "lease-two", now)
	if err != nil || acquired {
		t.Fatalf("competing primary acquired lease: %v %v", acquired, err)
	}
	status, err := f.Store.DisplayReplay(t.Context(), "renderer-one", "lease-one", "start-once", "arrival-lease", "arrival.replay.started", now)
	if err != nil || status != "playing" {
		t.Fatalf("start: %q %v", status, err)
	}
	if err := f.Store.RefreshDisplay(t.Context(), now.Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT skip_reason FROM arrival_events WHERE id='arrival-lease'`).Scan(&reason); err != nil || reason != "renderer_failure" {
		t.Fatalf("lease expiry did not skip: %s %v", reason, err)
	}
	acquired, err = f.Store.AcquireDisplayLease(t.Context(), "renderer-two", "lease-two", now.Add(31*time.Second))
	if err != nil || !acquired {
		t.Fatalf("new primary not available: %v %v", acquired, err)
	}
	if _, err := f.Store.DisplayReplay(t.Context(), "renderer-one", "lease-one", "stale-finish", "arrival-lease", "arrival.replay.completed", now.Add(31*time.Second)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("old primary completed after timeout: %v", err)
	}
}

func TestMemoryStopRemovesQueuedAndPlayingDisplayOnce(t *testing.T) {
	for _, playing := range []bool{false, true} {
		name := "queued"
		if playing {
			name = "playing"
		}
		t.Run(name, func(t *testing.T) {
			f := testdb.Open(t)
			f.Store.EnableDisplay()
			now := time.Now().UTC().Truncate(time.Microsecond)
			id := fifoArrival(t, f, "stop-"+name, now)
			memoryID := "fifo-memory-stop-" + name
			if playing {
				if ok, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease", now); err != nil || !ok {
					t.Fatalf("lease: %t %v", ok, err)
				}
				if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "start", id, "arrival.replay.started", now); err != nil {
					t.Fatal(err)
				}
				state, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "trusted-test-operator"), true, now)
				if err != nil || state.Mode != "pending" {
					t.Fatalf("fallback pending: %+v %v", state, err)
				}
			}
			for range 2 {
				if _, err := f.Store.StopMemory(t.Context(), memoryID, now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			var status, reason string
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT replay_status,skip_reason FROM arrival_events WHERE id=$1`, id).Scan(&status, &reason); err != nil || status != "skipped" || reason != "participant_request" {
				t.Fatalf("stop left replay active: %s/%s %v", status, reason, err)
			}
			var events int
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events
 WHERE kind='arrival.replay.skipped' AND data->>'arrivalId'=$1 AND data->>'reason'='participant_request'`, id).Scan(&events); err != nil || events != 1 {
				t.Fatalf("stop removal events: %d %v", events, err)
			}
			snapshot, err := f.Store.DisplaySnapshot(t.Context(), now.Add(2*time.Second))
			if err != nil || len(snapshot.Arrivals) != 0 {
				t.Fatalf("stop remained in snapshot: %+v %v", snapshot, err)
			}
			if playing && snapshot.Fallback.Mode != "fallback" {
				t.Fatalf("stop failed pending fallback progression: %+v", snapshot.Fallback)
			}
			if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "late-start", id, "arrival.replay.started", now.Add(2*time.Second)); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("stopped arrival restarted: %v", err)
			}
			candidates, err := f.Store.ReplayCandidates(t.Context(), now.Add(2*time.Second))
			if err != nil || len(candidates) != 0 {
				t.Fatalf("stopped replay candidate: %+v %v", candidates, err)
			}
		})
	}
}

func TestStoppedMemoryRejectsNewArrivalAndRendererRetry(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	first := fifoExperience(t, f, "stopped-before-arrival", now)
	if _, err := f.Store.StopMemory(t.Context(), first, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.AcceptArrival(t.Context(), first, "", "must-not-arrive", "booth", "key", "request", now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stopped memory first arrival: %v", err)
	}
	second := fifoArrival(t, f, "stopped-retry", now)
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE arrival_events SET replay_status='skipped',skip_reason='renderer_failure' WHERE id=$1`, second); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.StopMemory(t.Context(), "fifo-memory-stopped-retry", now); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.RetryDisplayArrival(store.WithOperatorActor(t.Context(), "trusted-test-operator"), second, now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stopped renderer failure requeued: %v", err)
	}
}
