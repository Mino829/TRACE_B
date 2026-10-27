package store_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func TestRendererRetryAfterThirtySecondsStillRequiresActiveMemory(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	err := f.Store.CreateExperience(t.Context(), store.NewExperience{
		ExperienceID: "retry-experience", MemorySessionID: "retry-memory",
		MemoryTokenHash: "retry-token", MemoryConsent: "memory", ConsentLanguage: "ja",
		CreatedAt: now, MemoryExpiresAt: now.Add(time.Hour), MemoryDeleteAfter: now.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.AcceptArrival(t.Context(), "retry-memory", "", "retry-arrival", "booth",
		"retry-receipt", "same-body", now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE arrival_events SET replay_status='skipped',
 skip_reason='renderer_failure',replay_finished_at=$1 WHERE id='retry-arrival'`, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.RetryDisplayArrival(t.Context(), "retry-arrival", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	candidates, err := f.Store.ReplayCandidates(t.Context(), now.Add(2*time.Minute))
	if err != nil || len(candidates) != 1 || candidates[0].ArrivalID != "retry-arrival" {
		t.Fatalf("retry lost its waiting arrival: %+v %v", candidates, err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE arrival_events SET replay_status='skipped',
 skip_reason='renderer_failure',replay_finished_at=$1 WHERE id='retry-arrival'`, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.RetryDisplayArrival(t.Context(), "retry-arrival", now.Add(time.Hour)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("retry after memory expiry: %v", err)
	}
}

func TestArrivalAfterLongWaitWithNewReceiptRemainsQueued(t *testing.T) {
	f := testdb.Open(t)
	f.Store.EnableDisplay()
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := fixtureExperience(t, f, now)
	first, err := f.Store.AcceptArrival(t.Context(), v.MemorySessionID, v.LinkTokenHash,
		"arrival-once", "booth", "receipt-first", "request", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.Store.AcceptArrival(t.Context(), v.MemorySessionID, v.LinkTokenHash,
		"arrival-duplicate", "booth", "receipt-second", "request", now.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		ArrivalID string `json:"arrivalId"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(second, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.ArrivalID != "arrival-once" || reply.Status != "queued" {
		t.Fatalf("long-wait arrival reply: %#v", reply)
	}
	var status, reason string
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT replay_status,COALESCE(skip_reason,'')
 FROM arrival_events WHERE id='arrival-once'`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || reason != "" {
		t.Fatalf("waiting arrival was skipped: %s/%s", status, reason)
	}
	repeated, err := f.Store.AcceptArrival(t.Context(), v.MemorySessionID, v.LinkTokenHash,
		"arrival-duplicate", "booth", "receipt-first", "request", now.Add(31*time.Second))
	if err != nil || string(repeated) != string(first) {
		t.Fatalf("original idempotency receipt changed: %s (%v)", repeated, err)
	}
	var events int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events
 WHERE kind='arrival.replay.skipped' AND data->>'arrivalId'='arrival-once' AND data->>'reason'='timeout'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("ordinary waiting emitted timeout events: %d", events)
	}
}
