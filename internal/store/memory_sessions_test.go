package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func lifecycleMemory(t *testing.T, f *testdb.Fixture, now time.Time) store.NewExperience {
	t.Helper()
	v := store.NewExperience{ExperienceID: "lifecycle-experience", MemorySessionID: "lifecycle-memory", MemoryTokenHash: "owner-hash",
		MemoryConsent: "memory", ConsentLanguage: "ja", CreatedAt: now,
		MemoryExpiresAt: now.Add(time.Hour), MemoryDeleteAfter: now.Add(2 * time.Hour)}
	if err := f.Store.CreateExperience(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMemoryStatusSeparatesReplayFromParticipationAndRetention(t *testing.T) {
	for _, terminal := range []string{"completed", "skipped"} {
		t.Run(terminal, func(t *testing.T) {
			f := testdb.Open(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			v := lifecycleMemory(t, f, now)
			assertState := func(at time.Time, status string, replay *string) {
				t.Helper()
				state, err := f.Store.ExperienceByMemoryToken(t.Context(), v.ExperienceID, v.MemoryTokenHash, at)
				if err != nil || state.MemoryStatus != status || (state.ReplayStatus == nil) != (replay == nil) || (replay != nil && *state.ReplayStatus != *replay) {
					t.Fatalf("participant status=%s replay=%v: %+v %v", status, replay, state, err)
				}
			}
			assertState(now, "main", nil)
			if _, err := f.Store.AcceptArrival(t.Context(), v.MemorySessionID, "", "lifecycle-arrival", "booth", "receipt", "request", now); err != nil {
				t.Fatal(err)
			}
			queued := "queued"
			assertState(now, "arrived", &queued)
			if ok, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease", now); err != nil || !ok {
				t.Fatalf("lease: %t %v", ok, err)
			}
			if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "start", "lifecycle-arrival", "arrival.replay.started", now); err != nil {
				t.Fatal(err)
			}
			action := "arrival.replay.completed"
			if terminal == "skipped" {
				action = "arrival.replay.failed"
			}
			if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "finish", "lifecycle-arrival", action, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			assertState(now.Add(time.Second), "arrived", &terminal)
			assertState(v.MemoryExpiresAt, "ended", &terminal)
			if err := f.Store.AuthenticateMemory(t.Context(), v.MemorySessionID, v.MemoryTokenHash, v.MemoryDeleteAfter.Add(-time.Microsecond)); err != nil {
				t.Fatalf("owner rights ended early: %v", err)
			}
			if _, err := f.Store.ExperienceByMemoryToken(t.Context(), v.ExperienceID, v.MemoryTokenHash, v.MemoryDeleteAfter); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("state survived deletion deadline: %v", err)
			}
		})
	}
}

func TestMemoryStopSkipsQueuedAndPlayingAndAdvancesPendingFallback(t *testing.T) {
	for _, playing := range []bool{false, true} {
		name := "queued"
		if playing {
			name = "playing"
		}
		t.Run(name, func(t *testing.T) {
			f := testdb.Open(t)
			f.Store.EnableDisplay()
			now := time.Now().UTC().Truncate(time.Microsecond)
			v := lifecycleMemory(t, f, now)
			if _, err := f.Store.AcceptArrival(t.Context(), v.MemorySessionID, "", "lifecycle-arrival", "booth", "receipt", "request", now); err != nil {
				t.Fatal(err)
			}
			if ok, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease", now); err != nil || !ok {
				t.Fatalf("lease: %t %v", ok, err)
			}
			if playing {
				if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "start", "lifecycle-arrival", "arrival.replay.started", now); err != nil {
					t.Fatal(err)
				}
				state, err := f.Store.SetFallback(store.WithOperatorActor(t.Context(), "trusted-operator"), true, now.Add(time.Second))
				if err != nil || state.Mode != "pending" {
					t.Fatalf("pending fallback: %+v %v", state, err)
				}
			}
			stopped, err := f.Store.StopMemory(t.Context(), v.MemorySessionID, now.Add(2*time.Second))
			if err != nil || stopped.StoppedAt == nil {
				t.Fatalf("stop: %+v %v", stopped, err)
			}
			again, err := f.Store.StopMemory(t.Context(), v.MemorySessionID, now.Add(3*time.Second))
			if err != nil || again.StoppedAt == nil || !again.StoppedAt.Equal(*stopped.StoppedAt) {
				t.Fatalf("stop timestamp changed: %+v %v", again, err)
			}
			var status, reason string
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT replay_status,skip_reason FROM arrival_events WHERE id='lifecycle-arrival'`).Scan(&status, &reason); err != nil || status != "skipped" || reason != "participant_request" {
				t.Fatalf("stop did not retire replay: %s %s %v", status, reason, err)
			}
			var skips int
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events WHERE kind='arrival.replay.skipped'`).Scan(&skips); err != nil || skips != 1 {
				t.Fatalf("duplicate/missing stop event: %d %v", skips, err)
			}
			snapshot, err := f.Store.DisplaySnapshot(t.Context(), now.Add(3*time.Second))
			if err != nil || len(snapshot.Arrivals) != 0 || (playing && snapshot.Fallback.Mode != "fallback") {
				t.Fatalf("authoritative stopped projection: %+v %v", snapshot, err)
			}
			if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "late-start", "lifecycle-arrival", "arrival.replay.started", now.Add(3*time.Second)); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("stopped replay restarted: %v", err)
			}
			state, err := f.Store.ExperienceByMemoryToken(t.Context(), v.ExperienceID, v.MemoryTokenHash, now.Add(3*time.Second))
			if err != nil || state.MemoryStatus != "ended" || state.ReplayStatus == nil || *state.ReplayStatus != "skipped" {
				t.Fatalf("stopped owner state: %+v %v", state, err)
			}
		})
	}
}

func TestMemoryStopSerializesWithFirstArrivalAndReplayStart(t *testing.T) {
	for _, operation := range []string{"arrival", "replay"} {
		t.Run(operation, func(t *testing.T) {
			f := testdb.Open(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			v := lifecycleMemory(t, f, now)
			if operation == "replay" {
				if _, err := f.Store.AcceptArrival(t.Context(), v.MemorySessionID, "", "race-arrival", "booth", "receipt", "request", now); err != nil {
					t.Fatal(err)
				}
				if ok, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease", now); err != nil || !ok {
					t.Fatalf("lease: %t %v", ok, err)
				}
			}
			start := make(chan struct{})
			results := make(chan error, 2)
			var workers sync.WaitGroup
			workers.Add(2)
			go func() {
				defer workers.Done()
				<-start
				_, err := f.Store.StopMemory(t.Context(), v.MemorySessionID, now)
				results <- err
			}()
			go func() {
				defer workers.Done()
				<-start
				var err error
				if operation == "arrival" {
					_, err = f.Store.AcceptArrival(t.Context(), v.MemorySessionID, "", "race-arrival", "booth", "receipt", "request", now)
				} else {
					_, err = f.Store.DisplayReplay(t.Context(), "holder", "lease", "race-start", "race-arrival", "arrival.replay.started", now)
				}
				results <- err
			}()
			close(start)
			workers.Wait()
			close(results)
			for err := range results {
				if err != nil && !errors.Is(err, store.ErrConflict) {
					t.Fatalf("racing lifecycle operation: %v", err)
				}
			}
			state, err := f.Store.ExperienceByMemoryToken(t.Context(), v.ExperienceID, v.MemoryTokenHash, now)
			if err != nil || state.MemoryStatus != "ended" || (state.ReplayStatus != nil && *state.ReplayStatus != "skipped") {
				t.Fatalf("stop lost terminal race: %+v %v", state, err)
			}
			var active int
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM arrival_events WHERE memory_session_id=$1 AND replay_status IN ('queued','playing')`, v.MemorySessionID).Scan(&active); err != nil || active != 0 {
				t.Fatalf("racing replay remained active: %d %v", active, err)
			}
		})
	}
}

func TestMemoryStopWaitsForPushAndUnsubscribeRemainsNonterminal(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := lifecycleMemory(t, f, now)
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.DeletePush(t.Context(), v.MemorySessionID); err != nil {
		t.Fatal(err)
	}
	p, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now)
	if err != nil {
		t.Fatalf("ordinary unsubscribe became terminal: %v", err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	delivered, stopped := make(chan error, 1), make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() {
		delivered <- f.Store.DeliverPush(ctx, v.MemorySessionID, p.NextSendAt, func(store.PushSubscription) (int, error) { close(started); <-release; return 201, nil })
	}()
	select {
	case <-started:
	case err := <-delivered:
		t.Fatalf("push did not begin: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() { _, err := f.Store.StopMemory(ctx, v.MemorySessionID, p.NextSendAt); stopped <- err }()
	close(release)
	if err := <-delivered; err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	sent := false
	if err := f.Store.DeliverPush(t.Context(), v.MemorySessionID, p.NextSendAt.Add(time.Minute), func(store.PushSubscription) (int, error) { sent = true; return 201, nil }); err != nil || sent {
		t.Fatalf("post-stop push delivery: %t %v", sent, err)
	}
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("terminal stop allowed resubscription: %v", err)
	}
}

func TestMemoryMutationReceiptsCapRetentionAndCascadeWithoutWholeDeleteReceipt(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := lifecycleMemory(t, f, now)
	for _, scope := range []string{"photo-create", "photo-delete"} {
		if err := f.Store.SaveMemoryIdempotency(t.Context(), v.MemorySessionID, scope, "key", "request", 204, []byte{}, now.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		var deadline time.Time
		if err := f.SQL.QueryRowContext(t.Context(), `SELECT expires_at FROM idempotency_records WHERE scope=$1`, scope).Scan(&deadline); err != nil || !deadline.Equal(v.MemoryDeleteAfter) {
			t.Fatalf("receipt outlived owner: %s %v %v", scope, deadline, err)
		}
	}
	if err := f.Store.SaveIdempotency(t.Context(), "whole-delete", "key", "credential-hash", 204, []byte{}, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.StopMemory(t.Context(), v.MemorySessionID, now); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.AuthenticateMemory(t.Context(), v.MemorySessionID, v.MemoryTokenHash, v.MemoryDeleteAfter.Add(-time.Microsecond)); err != nil {
		t.Fatal("stop shortened deletion right:", err)
	}
	if _, err := f.Store.StopMemory(t.Context(), v.MemorySessionID, v.MemoryDeleteAfter); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stop accepted at deletion deadline: %v", err)
	}
	if err := f.Store.DeleteMemory(t.Context(), v.MemorySessionID); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"photo-create", "photo-delete"} {
		if _, err := f.Store.GetIdempotency(t.Context(), scope, "key", now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("whole delete retained %s: %v", scope, err)
		}
	}
	receipt, err := f.Store.GetIdempotency(t.Context(), "whole-delete", "key", now.Add(23*time.Hour))
	if err != nil || receipt.ResponseStatus != 204 || len(receipt.ResponseBody) != 0 {
		t.Fatalf("whole-delete content-free exception lost: %+v %v", receipt, err)
	}
}

func TestLegacyLiveStateExpiryAndDeleteAfterWithoutMemory(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := fixtureExperience(t, f, now)
	if err := f.Store.DeleteMemory(t.Context(), v.MemorySessionID); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{v.LiveExpiresAt, v.LiveDeleteAfter.Add(-time.Microsecond)} {
		live, err := f.Store.LiveSessionByToken(t.Context(), v.LiveSessionID, v.LiveTokenHash, at)
		if err != nil || live.TrackingStatus != "expired" || !live.DeleteAfter.Equal(v.LiveDeleteAfter) {
			t.Fatalf("independent expired Live state: %+v %v", live, err)
		}
	}
	if err := f.Store.StopLive(t.Context(), v.LiveSessionID, "participant_request", v.LiveExpiresAt); err != nil {
		t.Fatalf("expired owner lost stop right: %v", err)
	}
	var stoppedAt, expiresAt, deleteAfter time.Time
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT stopped_at,expires_at,delete_after FROM live_presence_sessions WHERE id=$1`, v.LiveSessionID).Scan(&stoppedAt, &expiresAt, &deleteAfter); err != nil || !stoppedAt.Equal(v.LiveExpiresAt) || !expiresAt.Equal(v.LiveExpiresAt) || !deleteAfter.Equal(v.LiveDeleteAfter) {
		t.Fatalf("expired stop changed lifetime: %v %v %v %v", stoppedAt, expiresAt, deleteAfter, err)
	}
	if err := f.Store.StopLive(t.Context(), v.LiveSessionID, "different-retry-reason", v.LiveDeleteAfter.Add(-time.Microsecond)); err != nil {
		t.Fatalf("stop retry lost retained right: %v", err)
	}
	live, err := f.Store.LiveSessionByToken(t.Context(), v.LiveSessionID, v.LiveTokenHash, v.LiveDeleteAfter.Add(-time.Microsecond))
	if err != nil || live.TrackingStatus != "stopped" {
		t.Fatalf("expired stop revived tracking: %+v %v", live, err)
	}
	var retriedAt time.Time
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT stopped_at FROM live_presence_sessions WHERE id=$1`, v.LiveSessionID).Scan(&retriedAt); err != nil || !retriedAt.Equal(stoppedAt) {
		t.Fatalf("expired stop timestamp changed: %v %v", retriedAt, err)
	}
	if err := f.Store.StopLive(t.Context(), v.LiveSessionID, "participant_request", v.LiveDeleteAfter); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Live stop survived deletion boundary: %v", err)
	}
	if _, err := f.Store.LiveSessionByToken(t.Context(), v.LiveSessionID, v.LiveTokenHash, v.LiveDeleteAfter); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Live read survived deletion boundary: %v", err)
	}
	if _, err := f.Store.LiveSessionByToken(t.Context(), v.LiveSessionID, v.MemoryTokenHash, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Memory credential read Live state: %v", err)
	}
}

func TestPersistedMemoryStopFencesPushWithoutLegacyTerminalFlag(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := lifecycleMemory(t, f, now)
	p, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET stopped_at=$2,push_stopped=FALSE WHERE id=$1`, v.MemorySessionID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("persisted stop allowed Push subscribe: %v", err)
	}
	status := "active"
	if _, err := f.Store.PatchPush(t.Context(), v.MemorySessionID, nil, &status, now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("persisted stop allowed Push patch: %v", err)
	}
	sent := false
	if err := f.Store.DeliverPush(t.Context(), v.MemorySessionID, p.NextSendAt, func(store.PushSubscription) (int, error) { sent = true; return 201, nil }); err != nil || sent {
		t.Fatalf("persisted stop allowed Push send: %t %v", sent, err)
	}
	var remaining int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM push_subscriptions WHERE memory_session_id=$1`, v.MemorySessionID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("persisted stop retained Push credentials: %d %v", remaining, err)
	}
}
