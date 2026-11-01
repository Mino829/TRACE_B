package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

const replayProof = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func completeReplay(t *testing.T, f *testdb.Fixture, id, event string, now time.Time) {
	t.Helper()
	for _, action := range []string{"started", "completed"} {
		if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", event+action, id, "arrival.replay."+action, now); err != nil {
			t.Fatalf("%s %s: %v", id, action, err)
		}
	}
}

func completedArrival(t *testing.T, f *testdb.Fixture, now time.Time) string {
	t.Helper()
	id := fifoArrival(t, f, "repeat", now)
	if ok, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease", now); err != nil || !ok {
		t.Fatalf("lease: %t %v", ok, err)
	}
	completeReplay(t, f, id, "initial-", now)
	return id
}

func TestParticipantReplayIsNewGenerationAtFIFOTail(t *testing.T) {
	f := testdb.Open(t)
	f.Store.EnableDisplay()
	now := time.Now().UTC().Truncate(time.Microsecond)
	root := completedArrival(t, f, now)
	other := fifoArrival(t, f, "other", now.Add(time.Second))
	body, err := f.Store.ReplayArrival(t.Context(), "fifo-memory-repeat", "repeat-generation", "booth", replayProof, "receipt-repeat", "replay-body", now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		ArrivalID string `json:"arrivalId"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(body, &reply); err != nil || reply.ArrivalID != "repeat-generation" || reply.Status != "queued" {
		t.Fatalf("replay response: %s %v", body, err)
	}
	candidates, err := f.Store.ReplayCandidates(t.Context(), now.Add(3*time.Second))
	if err != nil || len(candidates) != 2 || candidates[0].ArrivalID != other || candidates[1].ArrivalID != reply.ArrivalID {
		t.Fatalf("FIFO tail: %+v %v", candidates, err)
	}
	state, err := f.Store.DisplaySnapshot(t.Context(), now.Add(3*time.Second))
	if err != nil || len(state.Arrivals) != 2 || state.Arrivals[1].ArrivalID != reply.ArrivalID {
		t.Fatalf("snapshot generation/order: %+v %v", state.Arrivals, err)
	}
	for _, action := range []string{"started", "completed", "failed"} {
		if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "old-"+action, root, "arrival.replay."+action, now.Add(3*time.Second)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("stale %s accepted: %v", action, err)
		}
	}
	// Even an already acknowledged old completion cannot be recovered as current.
	if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "initial-completed", root, "arrival.replay.completed", now.Add(3*time.Second)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale renderer receipt accepted: %v", err)
	}
	initial, err := f.Store.AcceptArrival(t.Context(), "fifo-memory-repeat", "", "unused", "", "", "receipt-repeat", "request", now.Add(3*time.Second))
	if err != nil || !strings.Contains(string(initial), root) || !strings.Contains(string(initial), `"status":"queued"`) {
		t.Fatalf("initial arrival recovery changed identity: %s %v", initial, err)
	}
	if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "tail-bypass", reply.ArrivalID, "arrival.replay.started", now.Add(3*time.Second)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("replay bypassed FIFO: %v", err)
	}
	completeReplay(t, f, other, "other-", now.Add(3*time.Second))
	completeReplay(t, f, reply.ArrivalID, "repeat-", now.Add(4*time.Second))
	if _, err := f.Store.ReplayArrival(t.Context(), "fifo-memory-repeat", "third-generation", "booth", strings.Repeat("c", 64), "third-key", "third-body", now.Add(5*time.Second)); err != nil {
		t.Fatalf("third viewing: %v", err)
	}
	var id string
	var arrived time.Time
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT id,arrived_at FROM arrival_events WHERE memory_session_id='fifo-memory-repeat'`).Scan(&id, &arrived); err != nil || id != root || !arrived.Equal(now) {
		t.Fatalf("root arrival changed: %s %v %v", id, arrived, err)
	}
	var projected, requested int64
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT MIN(id) FILTER(WHERE kind='participant.arrived'), MIN(id) FILTER(WHERE kind='arrival.replay.requested') FROM display_events WHERE data->>'arrivalId'='repeat-generation'`).Scan(&projected, &requested); err != nil || requested != projected+1 {
		t.Fatalf("metadata/request projection order: %d %d %v", projected, requested, err)
	}
}

func TestReplayReceiptsAndProofUseAreIndependentAndDurable(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	completedArrival(t, f, now)
	// The initial proof cannot become a fresh replay by choosing a new key.
	if _, err := f.Store.ReplayArrival(t.Context(), "fifo-memory-repeat", "bad", "booth", strings.Repeat("a", 64), "new-key", "body", now); !errors.Is(err, store.ErrInvalidArrivalProof) {
		t.Fatalf("initial proof reused: %v", err)
	}
	body, err := f.Store.ReplayArrival(t.Context(), "fifo-memory-repeat", "replayed", "booth", replayProof, "receipt-repeat", "same-body", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	completeReplay(t, f, "replayed", "replayed-", now.Add(2*time.Second))
	for _, delay := range []time.Duration{time.Minute, 25 * time.Hour} {
		recovered, err := f.Store.ReplayArrival(t.Context(), "fifo-memory-repeat", "not-enqueued", "", "", "receipt-repeat", "same-body", now.Add(delay))
		if err != nil || string(recovered) != string(body) {
			t.Fatalf("receipt recovery: %s %v", recovered, err)
		}
	}
	if _, err := f.Store.ReplayArrival(t.Context(), "fifo-memory-repeat", "not-enqueued", "", "", "receipt-repeat", "different-body", now); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("changed body: %v", err)
	}
	if _, err := f.Store.ReplayArrival(t.Context(), "fifo-memory-repeat", "not-enqueued", "booth", replayProof, "different-key", "same-body", now.Add(3*time.Second)); !errors.Is(err, store.ErrInvalidArrivalProof) {
		t.Fatalf("proof reuse after completion: %v", err)
	}
	if _, err := f.Store.ReplayArrival(t.Context(), "fifo-memory-repeat", "not-enqueued", "", strings.Repeat("c", 64), "expired", "body", now); !errors.Is(err, store.ErrInvalidArrivalProof) {
		t.Fatalf("fresh operation accepted expired proof: %v", err)
	}
	// Shared projection proof is still usable by a different MemorySession.
	other := fifoArrival(t, f, "shared", now.Add(3*time.Second))
	completeReplay(t, f, other, "shared-", now.Add(3*time.Second))
	if _, err := f.Store.ReplayArrival(t.Context(), "fifo-memory-shared", "shared-replay", "booth", replayProof, "receipt", "body", now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.DeleteMemory(t.Context(), "fifo-memory-repeat"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"arrival_proof_uses", "arrival_replay_receipts", "arrival_receipts", "arrival_events"} {
		var count int
		if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table+" WHERE memory_session_id='fifo-memory-repeat'").Scan(&count); err != nil || count != 0 {
			t.Fatalf("cascade %s: %d %v", table, count, err)
		}
	}
}

func TestParallelReplayCreatesOneGeneration(t *testing.T) {
	for _, sameKey := range []bool{true, false} {
		t.Run(fmt.Sprint(sameKey), func(t *testing.T) {
			f := testdb.Open(t)
			f.Store.EnableDisplay()
			now := time.Now().UTC().Truncate(time.Microsecond)
			completedArrival(t, f, now)
			var wg sync.WaitGroup
			results := make(chan error, 8)
			for i := range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					key := "parallel"
					if !sameKey {
						key = fmt.Sprint(i)
					}
					_, err := f.Store.ReplayArrival(t.Context(), "fifo-memory-repeat", fmt.Sprintf("generation-%d", i), "booth", replayProof, key, "body", now.Add(time.Second))
					results <- err
				}()
			}
			wg.Wait()
			close(results)
			accepted := 0
			for err := range results {
				if err == nil {
					accepted++
				} else if !errors.Is(err, store.ErrConflict) {
					t.Fatal(err)
				}
			}
			want := 1
			if sameKey {
				want = 8
			}
			if accepted != want {
				t.Fatalf("accepted %d want %d", accepted, want)
			}
			var receipts, proofs, requests int
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM arrival_replay_receipts),(SELECT COUNT(*) FROM arrival_proof_uses WHERE proof_fingerprint=$1),(SELECT COUNT(*) FROM display_events WHERE kind='arrival.replay.requested')`, replayProof).Scan(&receipts, &proofs, &requests); err != nil || receipts != 1 || proofs != 1 || requests != 2 {
				t.Fatalf("duplicate generation: %d/%d/%d %v", receipts, proofs, requests, err)
			}
		})
	}
}

func TestParticipantReplayRejectsInactiveAndNonCompletedStates(t *testing.T) {
	for _, state := range []string{"queued", "playing", "skipped", "stopped", "expired", "deleting", "deleted"} {
		t.Run(state, func(t *testing.T) {
			f := testdb.Open(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			completedArrival(t, f, now)
			switch state {
			case "queued", "playing", "skipped":
				if _, err := f.SQL.ExecContext(t.Context(), `UPDATE arrival_events SET replay_status=$1`, state); err != nil {
					t.Fatal(err)
				}
			case "stopped":
				if _, err := f.Store.StopMemory(t.Context(), "fifo-memory-repeat", now); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET expires_at=$1 WHERE id='fifo-memory-repeat'`, now); err != nil {
					t.Fatal(err)
				}
			case "deleting":
				if _, err := f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET deleting=TRUE WHERE id='fifo-memory-repeat'`); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := f.Store.DeleteMemory(t.Context(), "fifo-memory-repeat"); err != nil {
					t.Fatal(err)
				}
			}
			_, err := f.Store.ReplayArrival(t.Context(), "fifo-memory-repeat", "forbidden", "booth", replayProof, "key", "body", now)
			want := store.ErrConflict
			if state == "deleted" {
				want = store.ErrNotFound
			}
			if !errors.Is(err, want) {
				t.Fatalf("%s: %v", state, err)
			}
			var receipts, proofs int
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM arrival_replay_receipts),(SELECT COUNT(*) FROM arrival_proof_uses WHERE proof_fingerprint=$1)`, replayProof).Scan(&receipts, &proofs); err != nil || receipts != 0 || proofs != 0 {
				t.Fatalf("rejection consumed state: %d/%d %v", receipts, proofs, err)
			}
		})
	}
}

func TestReplayQueueCapacityRollsBackProofAndReceipt(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	completedArrival(t, f, now)
	var first string
	for i := range store.ArrivalQueueLimit {
		id := fifoArrival(t, f, fmt.Sprint(i), now.Add(time.Second))
		if i == 0 {
			first = id
		}
	}
	if _, err := f.Store.ReplayArrival(t.Context(), "fifo-memory-repeat", "replayed", "booth", replayProof, "capacity", "body", now.Add(2*time.Second)); !errors.Is(err, store.ErrArrivalQueueFull) {
		t.Fatalf("full queue: %v", err)
	}
	var receipts, proofs int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM arrival_replay_receipts),(SELECT COUNT(*) FROM arrival_proof_uses WHERE proof_fingerprint=$1)`, replayProof).Scan(&receipts, &proofs); err != nil || receipts != 0 || proofs != 0 {
		t.Fatalf("capacity failure consumed state: %d/%d %v", receipts, proofs, err)
	}
	if err := f.Store.SkipArrival(store.WithOperatorActor(t.Context(), "operator"), first, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.ReplayArrival(t.Context(), "fifo-memory-repeat", "replayed", "booth", replayProof, "capacity", "body", now.Add(4*time.Second)); err != nil {
		t.Fatalf("retry after capacity recovery: %v", err)
	}
}

func TestReplayRemovalEventsAlwaysUseCurrentGeneration(t *testing.T) {
	for _, action := range []string{"own-skip", "operator-skip", "stop", "delete", "expiry", "lease-loss", "fallback"} {
		t.Run(action, func(t *testing.T) {
			f := testdb.Open(t)
			f.Store.EnableDisplay()
			now := time.Now().UTC().Truncate(time.Microsecond)
			completedArrival(t, f, now)
			if _, err := f.Store.ReplayArrival(t.Context(), "fifo-memory-repeat", "current-generation", "booth", replayProof, "replay", "body", now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			at := now.Add(2 * time.Second)
			var err error
			switch action {
			case "own-skip":
				err = f.Store.SkipOwnArrival(t.Context(), "fifo-memory-repeat", at)
			case "operator-skip":
				err = f.Store.SkipArrival(store.WithOperatorActor(t.Context(), "operator"), "current-generation", at)
			case "stop":
				_, err = f.Store.StopMemory(t.Context(), "fifo-memory-repeat", at)
			case "delete":
				err = f.Store.DeleteMemory(t.Context(), "fifo-memory-repeat")
			case "expiry":
				_, err = f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET expires_at=$1 WHERE id='fifo-memory-repeat'`, at)
				if err == nil {
					err = f.Store.RefreshDisplay(t.Context(), at)
				}
			case "lease-loss", "fallback":
				_, err = f.Store.DisplayReplay(t.Context(), "holder", "lease", "current-start", "current-generation", "arrival.replay.started", at)
				if err == nil && action == "lease-loss" {
					err = f.Store.ReleaseDisplayLease(t.Context(), "holder", "lease", at)
				} else if err == nil {
					_, err = f.Store.SetFallback(store.WithOperatorActor(t.Context(), "operator"), true, at)
					if err == nil {
						err = f.Store.RefreshDisplay(t.Context(), at.Add(30*time.Second))
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events WHERE kind='arrival.replay.skipped' AND data->>'arrivalId'='current-generation'`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("current generation removal: %d %v", count, err)
			}
		})
	}
}

func TestOperatorFailureRetryCreatesFencedProjectedGeneration(t *testing.T) {
	f := testdb.Open(t)
	f.Store.EnableDisplay()
	now := time.Now().UTC().Truncate(time.Microsecond)
	root := fifoArrival(t, f, "failure", now)
	if ok, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease", now); err != nil || !ok {
		t.Fatalf("lease: %t %v", ok, err)
	}
	for _, action := range []string{"started", "failed"} {
		if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "failure-"+action, root, "arrival.replay."+action, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Store.RetryDisplayArrival(store.WithOperatorActor(t.Context(), "operator"), root, "retry-current", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"late-old-failure", "failure-failed"} {
		if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", event, root, "arrival.replay.failed", now.Add(time.Second)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("old retry generation accepted: %v", err)
		}
	}
	var metadata, requested int64
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT MIN(id) FILTER(WHERE kind='participant.arrived'),MIN(id) FILTER(WHERE kind='arrival.replay.requested') FROM display_events WHERE data->>'arrivalId'='retry-current'`).Scan(&metadata, &requested); err != nil || requested != metadata+1 {
		t.Fatalf("operator retry metadata: %d/%d %v", metadata, requested, err)
	}
	completeReplay(t, f, "retry-current", "operator-retry-", now.Add(2*time.Second))
}
