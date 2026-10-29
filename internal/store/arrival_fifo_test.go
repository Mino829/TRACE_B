package store_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func fifoExperience(t *testing.T, f *testdb.Fixture, id string, now time.Time) string {
	t.Helper()
	memoryID := "fifo-memory-" + id
	if err := f.Store.CreateExperience(t.Context(), store.NewExperience{
		ExperienceID: "fifo-experience-" + id, MemorySessionID: memoryID,
		MemoryTokenHash: "fifo-token-" + id, MemoryConsent: "memory", ConsentLanguage: "ja",
		CreatedAt: now, MemoryExpiresAt: now.Add(time.Hour), MemoryDeleteAfter: now.Add(2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	return memoryID
}

func fifoArrival(t *testing.T, f *testdb.Fixture, id string, now time.Time) string {
	t.Helper()
	memoryID := fifoExperience(t, f, id, now)
	arrivalID := "fifo-arrival-" + id
	if _, err := f.Store.AcceptArrival(t.Context(), memoryID, "", arrivalID, "booth", "receipt-"+id, "request", now); err != nil {
		t.Fatal(err)
	}
	return arrivalID
}

func TestArrivalWaitContinuesPastThirtySeconds(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := fifoArrival(t, f, "wait", now)
	later := now.Add(2 * time.Minute)
	if err := f.Store.Cleanup(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	candidates, err := f.Store.ReplayCandidates(t.Context(), later)
	if err != nil || len(candidates) != 1 || candidates[0].ArrivalID != id {
		t.Fatalf("ordinary waiting lost its arrival: %+v %v", candidates, err)
	}
	state, err := f.Store.ExperienceByMemoryToken(t.Context(), "fifo-experience-wait", "fifo-token-wait", later)
	if err != nil || state.ReplayStatus == nil || *state.ReplayStatus != "queued" {
		t.Fatalf("owner waiting state: %+v %v", state, err)
	}
	acquired, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease", later)
	if err != nil || !acquired {
		t.Fatalf("lease: %t %v", acquired, err)
	}
	if status, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "wait-start", id, "arrival.replay.started", later); err != nil || status != "playing" {
		t.Fatalf("long-waiting arrival did not start: %s %v", status, err)
	}
}

func TestDisplayRejectsOutOfOrderAndOverlappingReplay(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	first := fifoArrival(t, f, "first", now)
	second := fifoArrival(t, f, "second", now.Add(time.Second))
	acquired, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease", now.Add(2*time.Second))
	if err != nil || !acquired {
		t.Fatalf("lease: %t %v", acquired, err)
	}
	send := func(event, arrival, action string) error {
		_, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", event, arrival, action, now.Add(3*time.Second))
		return err
	}
	if err := send("second-first", second, "arrival.replay.started"); err != store.ErrConflict {
		t.Fatalf("out-of-order start: %v", err)
	}
	if err := send("first-start", first, "arrival.replay.started"); err != nil {
		t.Fatal(err)
	}
	if err := send("second-overlap", second, "arrival.replay.started"); err != store.ErrConflict {
		t.Fatalf("overlapping start: %v", err)
	}
	if err := send("first-end", first, "arrival.replay.completed"); err != nil {
		t.Fatal(err)
	}
	if err := send("second-start", second, "arrival.replay.started"); err != nil {
		t.Fatal(err)
	}
	if err := send("second-end", second, "arrival.replay.completed"); err != nil {
		t.Fatal(err)
	}
	var receipts int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_renderer_receipts WHERE event_id IN ('second-first','second-overlap')`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("rejected requests saved receipts: %d %v", receipts, err)
	}
}

func TestArrivalQueueAcceptsFiftyAndRetriesAfterCapacityReturns(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := range 50 {
		fifoArrival(t, f, fmt.Sprintf("capacity-%02d", i), now.Add(time.Duration(i)*time.Microsecond))
	}
	memoryID := fifoExperience(t, f, "overflow", now)
	if _, err := f.Store.AcceptArrival(t.Context(), memoryID, "", "overflow-arrival", "booth", "overflow-receipt", "request", now.Add(time.Second)); err != store.ErrArrivalQueueFull {
		t.Fatalf("queue-full request: %v", err)
	}
	var stopped bool
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT push_stopped FROM memory_sessions WHERE id=$1`, memoryID).Scan(&stopped); err != nil || stopped {
		t.Fatalf("full queue stopped push: %t %v", stopped, err)
	}
	for _, table := range []string{"arrival_events", "arrival_receipts"} {
		var count int
		if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table+" WHERE memory_session_id=$1", memoryID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("full queue persisted %s: %d %v", table, count, err)
		}
	}
	if err := f.Store.SkipArrival(store.WithOperatorActor(t.Context(), "trusted-test-operator"), "fifo-arrival-capacity-00", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.AcceptArrival(t.Context(), memoryID, "", "overflow-arrival", "booth", "overflow-receipt", "request", now.Add(2*time.Second)); err != nil {
		t.Fatalf("same-key retry after capacity recovery: %v", err)
	}
}

func TestConcurrentArrivalsCannotExceedFifty(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := range 49 {
		fifoArrival(t, f, fmt.Sprintf("seed-%02d", i), now)
	}
	ids := make([]string, 8)
	for i := range ids {
		ids[i] = fifoExperience(t, f, fmt.Sprintf("racer-%d", i), now)
	}
	var workers sync.WaitGroup
	results := make(chan error, len(ids))
	for i, id := range ids {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := f.Store.AcceptArrival(t.Context(), id, "", fmt.Sprintf("race-arrival-%d", i), "booth", "race-receipt", "request", now.Add(time.Second))
			results <- err
		}()
	}
	workers.Wait()
	close(results)
	accepted, full := 0, 0
	for err := range results {
		switch err {
		case nil:
			accepted++
		case store.ErrArrivalQueueFull:
			full++
		default:
			t.Fatal(err)
		}
	}
	if accepted != 1 || full != 7 {
		t.Fatalf("queue race: accepted=%d rejected=%d", accepted, full)
	}
	var queued int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM arrival_events WHERE replay_status='queued'`).Scan(&queued); err != nil || queued != 50 {
		t.Fatalf("durable queue: %d %v", queued, err)
	}
}

func TestPendingHeadCannotBeBypassedAndExpiryReleasesQueue(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	first := fifoArrival(t, f, "pending", now)
	createPhoto(t, f, photoInput("fifo-memory-pending", "head-photo", now), now)
	second := fifoArrival(t, f, "ready", now.Add(time.Second))
	acquired, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease", now.Add(time.Second))
	if err != nil || !acquired {
		t.Fatalf("lease: %t %v", acquired, err)
	}
	candidates, err := f.Store.ReplayCandidates(t.Context(), now.Add(2*time.Second))
	if err != nil || len(candidates) != 0 {
		t.Fatalf("pending FIFO head bypassed by candidates: %+v %v", candidates, err)
	}
	for _, id := range []string{first, second} {
		if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "early-"+id, id, "arrival.replay.started", now.Add(2*time.Second)); err != store.ErrConflict {
			t.Fatalf("early start %s: %v", id, err)
		}
	}
	if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "head-start", first, "arrival.replay.started", now.Add(15*time.Second)); err != nil {
		t.Fatalf("photo wait cap: %v", err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET expires_at=$1 WHERE id='fifo-memory-pending'`, now.Add(16*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.RefreshDisplay(t.Context(), now.Add(17*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "next-start", second, "arrival.replay.started", now.Add(17*time.Second)); err != nil {
		t.Fatalf("expired head blocked next arrival: %v", err)
	}
}

func TestStoppedQueueHeadDoesNotProjectOrBlockNextReplay(t *testing.T) {
	f := testdb.Open(t)
	f.Store.EnableDisplay()
	now := time.Now().UTC().Truncate(time.Microsecond)
	stopped := fifoArrival(t, f, "stopped-head", now)
	active := fifoArrival(t, f, "active-next", now.Add(time.Second))
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET stopped_at=$1 WHERE id='fifo-memory-stopped-head'`, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.Store.DisplaySnapshot(t.Context(), now.Add(2*time.Second))
	if err != nil || len(snapshot.Arrivals) != 1 || snapshot.Arrivals[0].ArrivalID != active {
		t.Fatalf("stopped arrival projected: %+v %v", snapshot, err)
	}
	if ok, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease", now.Add(2*time.Second)); err != nil || !ok {
		t.Fatalf("lease: %t %v", ok, err)
	}
	if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "stopped-start", stopped, "arrival.replay.started", now.Add(2*time.Second)); err != store.ErrConflict {
		t.Fatalf("stopped replay started: %v", err)
	}
	if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "active-start", active, "arrival.replay.started", now.Add(2*time.Second)); err != nil {
		t.Fatalf("stopped head blocked active replay: %v", err)
	}
	if err := f.Store.RefreshDisplay(t.Context(), now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	var status, reason string
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT replay_status,skip_reason FROM arrival_events WHERE id=$1`, stopped).Scan(&status, &reason); err != nil || status != "skipped" || reason != "participant_request" {
		t.Fatalf("stopped stale queue not removed: %s/%s %v", status, reason, err)
	}
}

func TestStoppedMemoryDoesNotOccupyQueueCapacity(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := range store.ArrivalQueueLimit {
		fifoArrival(t, f, fmt.Sprintf("stopped-capacity-%02d", i), now)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET stopped_at=$1 WHERE id='fifo-memory-stopped-capacity-00'`, now); err != nil {
		t.Fatal(err)
	}
	memoryID := fifoExperience(t, f, "active-capacity", now)
	if _, err := f.Store.AcceptArrival(t.Context(), memoryID, "", "active-capacity-arrival", "booth", "active-receipt", "request", now); err != nil {
		t.Fatalf("stopped arrival retained queue capacity: %v", err)
	}
}
