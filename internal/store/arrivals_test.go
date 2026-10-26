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
	now := time.Now().UTC()
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
	now := time.Now().UTC()
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
