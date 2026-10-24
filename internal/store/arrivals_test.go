package store_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func TestArrivalQueueLimitAcrossSessionsAndRecovery(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC()
	for i := range store.ArrivalQueueLimit + 1 {
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
		if i == store.ArrivalQueueLimit {
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
	if queued != store.ArrivalQueueLimit || overflow != 1 {
		t.Fatalf("queue bounds: queued=%d overflow=%d", queued, overflow)
	}
	if err := f.Store.SkipArrival(t.Context(), "arrival-0", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	candidates, err := f.Store.ReplayCandidates(t.Context(), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != store.ArrivalQueueLimit-1 {
		t.Fatalf("ready 0-photo candidates: %d", len(candidates))
	}
	// All remaining arrivals eventually timeout in durable storage; no entry is silently lost.
	if err := f.Store.Cleanup(t.Context(), now.Add(store.ArrivalQueueTimeout+time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM arrival_events WHERE replay_status='queued'").Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("timeout cleanup retained %d queue entries: %v", queued, err)
	}
}
