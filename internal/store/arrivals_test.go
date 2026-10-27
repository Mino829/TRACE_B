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

func TestArrivalQueueLimitAcrossSessionsAndRecovery(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := range 11 {
		id := fmt.Sprintf("queue-memory-%d", i)
		err := f.Store.CreateExperience(t.Context(), store.NewExperience{
			ExperienceID: fmt.Sprintf("queue-experience-%d", i), MemorySessionID: id,
			MemoryTokenHash: fmt.Sprintf("token-%d", i), MemoryConsent: "memory", ConsentLanguage: "ja",
			CreatedAt: now, MemoryExpiresAt: now.Add(time.Hour), MemoryDeleteAfter: now.Add(2 * time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		body, err := f.Store.AcceptArrival(t.Context(), id, "", fmt.Sprintf("arrival-%d", i), "booth",
			fmt.Sprintf("receipt-%d", i), "same-body", now)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Status               string `json:"status"`
			OnSiteChoiceRequired bool   `json:"onSiteChoiceRequired"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		want := "queued"
		if i == 10 {
			want = "skipped"
		}
		if result.Status != want || result.OnSiteChoiceRequired {
			t.Fatalf("entry %d: %#v", i, result)
		}
	}
	var queued, overflow int
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM arrival_events WHERE replay_status='queued'").Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM arrival_events WHERE skip_reason='queue_full'").Scan(&overflow); err != nil {
		t.Fatal(err)
	}
	if queued != 10 || overflow != 1 {
		t.Fatalf("queue bounds: queued=%d overflow=%d", queued, overflow)
	}
	var deadline time.Time
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT replay_deadline FROM arrival_events WHERE id='arrival-0'").Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	if !deadline.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("arrival replay deadline: got %s, want 30 seconds after %s", deadline, now)
	}
	if err := f.Store.SkipArrival(t.Context(), "arrival-0", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	candidates, err := f.Store.ReplayCandidates(t.Context(), now.Add(29*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 9 {
		t.Fatalf("ready 0-photo candidates before deadline: %d", len(candidates))
	}
	// At the deadline, remaining arrivals are skipped without undoing arrival.
	if err := f.Store.Cleanup(t.Context(), now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM arrival_events WHERE replay_status='queued'").Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("deadline retained %d queue entries: %v", queued, err)
	}
	var reason string
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT skip_reason FROM arrival_events WHERE id='arrival-1'").Scan(&reason); err != nil || reason != "timeout" {
		t.Fatalf("deadline skip reason: %q (%v)", reason, err)
	}
}

func TestRendererRetryKeepsArrivalDeadline(t *testing.T) {
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
	if err := f.Store.RetryDisplayArrival(t.Context(), "retry-arrival", now.Add(29*time.Second)); err != nil {
		t.Fatal(err)
	}
	var deadline time.Time
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT replay_deadline FROM arrival_events WHERE id='retry-arrival'").Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	if !deadline.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("retry extended deadline to %s", deadline)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE arrival_events SET replay_status='skipped',
 skip_reason='renderer_failure',replay_finished_at=$1 WHERE id='retry-arrival'`, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.RetryDisplayArrival(t.Context(), "retry-arrival", now.Add(30*time.Second)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("retry at deadline: %v", err)
	}
}

func TestArrivalAfterDeadlineWithNewReceiptPersistsTimeout(t *testing.T) {
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
	if reply.ArrivalID != "arrival-once" || reply.Status != "skipped" {
		t.Fatalf("expired arrival reply: %#v", reply)
	}
	var status, reason string
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT replay_status,COALESCE(skip_reason,'')
 FROM arrival_events WHERE id='arrival-once'`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "skipped" || reason != "timeout" {
		t.Fatalf("expired arrival was not durably skipped: %s/%s", status, reason)
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
	if events != 1 {
		t.Fatalf("duplicate or missing timeout display event: %d", events)
	}
}
