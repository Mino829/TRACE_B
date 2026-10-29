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
	if err := f.Store.RetryDisplayArrival(store.WithOperatorActor(t.Context(), "trusted-test-operator"), "retry-arrival", now.Add(2*time.Minute)); err != nil {
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
	if err := f.Store.RetryDisplayArrival(store.WithOperatorActor(t.Context(), "trusted-test-operator"), "retry-arrival", now.Add(time.Hour)); !errors.Is(err, store.ErrConflict) {
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

func TestArrivalRetriesResolveCurrentStateWithoutNewProof(t *testing.T) {
	for _, wanted := range []string{"queued", "playing", "completed", "skipped"} {
		t.Run(wanted, func(t *testing.T) {
			f := testdb.Open(t)
			f.Store.EnableDisplay()
			now := time.Now().UTC().Truncate(time.Microsecond)
			owner := fixtureExperience(t, f, now)
			if _, err := f.Store.AcceptArrival(t.Context(), owner.MemorySessionID, owner.LinkTokenHash,
				"current-arrival", "booth", "original-key", "original-request", now); err != nil {
				t.Fatal(err)
			}
			// Existing receipts may still contain the old serialized queued body.
			legacyBody := []byte(`{"arrivalId":"current-arrival","status":"queued","onSiteChoiceRequired":true}`)
			if _, err := f.SQL.ExecContext(t.Context(), `UPDATE arrival_receipts SET response_body=$1 WHERE memory_session_id=$2`, legacyBody, owner.MemorySessionID); err != nil {
				t.Fatal(err)
			}
			later := now.Add(time.Minute)
			if wanted == "playing" || wanted == "completed" {
				if ok, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease", later); err != nil || !ok {
					t.Fatalf("lease: %t %v", ok, err)
				}
				if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "start", "current-arrival", "arrival.replay.started", later); err != nil {
					t.Fatal(err)
				}
			}
			if wanted == "completed" {
				if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "complete", "current-arrival", "arrival.replay.completed", later.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if wanted == "skipped" {
				if err := f.Store.SkipOwnArrival(t.Context(), owner.MemorySessionID, later); err != nil {
					t.Fatal(err)
				}
			}
			var before int
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"original-key", "new-key"} {
				body, err := f.Store.AcceptArrival(t.Context(), owner.MemorySessionID, "", "not-a-new-arrival", "",
					key, "original-request", later.Add(2*time.Second))
				if err != nil {
					t.Fatalf("retry %s: %v", key, err)
				}
				var reply map[string]any
				if err := json.Unmarshal(body, &reply); err != nil {
					t.Fatal(err)
				}
				if reply["arrivalId"] != "current-arrival" || reply["status"] != wanted {
					t.Fatalf("stale retry %s: %s", key, body)
				}
				if _, present := reply["onSiteChoiceRequired"]; present {
					t.Fatalf("retired on-site choice exposed: %s", body)
				}
			}
			if _, err := f.Store.AcceptArrival(t.Context(), owner.MemorySessionID, "", "not-a-new-arrival", "",
				"original-key", "changed-request", later); !errors.Is(err, store.ErrIdempotencyConflict) {
				t.Fatalf("same key changed request: %v", err)
			}
			var events, arrivals int
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events`).Scan(&events); err != nil || events != before {
				t.Fatalf("retry emitted events: before=%d after=%d err=%v", before, events, err)
			}
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM arrival_events WHERE memory_session_id=$1`, owner.MemorySessionID).Scan(&arrivals); err != nil || arrivals != 1 {
				t.Fatalf("retry inserted another arrival: %d %v", arrivals, err)
			}
		})
	}
}

func TestInvalidNewArrivalDoesNotPersistOrStopPush(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	owner := fixtureExperience(t, f, now)
	if _, err := f.Store.AcceptArrival(t.Context(), owner.MemorySessionID, owner.LinkTokenHash,
		"invalid-arrival", "", "invalid-key", "invalid-request", now); !errors.Is(err, store.ErrInvalidArrivalProof) {
		t.Fatalf("invalid new proof: %v", err)
	}
	var arrivals, receipts int
	var stopped bool
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT
 (SELECT COUNT(*) FROM arrival_events WHERE memory_session_id=$1),
 (SELECT COUNT(*) FROM arrival_receipts WHERE memory_session_id=$1),push_stopped
 FROM memory_sessions WHERE id=$1`, owner.MemorySessionID).Scan(&arrivals, &receipts, &stopped); err != nil || arrivals != 0 || receipts != 0 || stopped {
		t.Fatalf("invalid proof mutated participant: arrivals=%d receipts=%d push_stopped=%t err=%v", arrivals, receipts, stopped, err)
	}
}

func TestOperatorArrivalMutationsRequireTrustedActorAndAuditIt(t *testing.T) {
	f := testdb.Open(t)
	f.Store.EnableDisplay()
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := fifoArrival(t, f, "actor", now)
	if err := f.Store.SkipArrival(t.Context(), id, now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("untrusted skip: %v", err)
	}
	if err := f.Store.RetryDisplayArrival(t.Context(), id, now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("untrusted retry: %v", err)
	}
	var status string
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT replay_status FROM arrival_events WHERE id=$1`, id).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("untrusted mutation changed arrival: %s %v", status, err)
	}
	trusted := store.WithOperatorIdentity(t.Context(), "verified-operator-actor", "verified-email-hash")
	if err := f.Store.SkipArrival(trusted, id, now); err != nil {
		t.Fatal(err)
	}
	var actor, emailHash, target, result string
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT operator_id,operator_email_hash,target_id_short,result FROM arrival_operator_audit WHERE action='arrival.skip'`).Scan(&actor, &emailHash, &target, &result); err != nil || actor != "verified-operator-actor" || emailHash != "verified-email-hash" || target == id || result != "skipped" {
		t.Fatalf("operator identity audit: %s %s %s %s %v", actor, emailHash, target, result, err)
	}
}

func TestArrivalOperatorAuditFailureRollsBackMutationAndEvents(t *testing.T) {
	for _, action := range []string{"arrival.skip", "arrival.retry"} {
		t.Run(action, func(t *testing.T) {
			f := testdb.Open(t)
			f.Store.EnableDisplay()
			now := time.Now().UTC().Truncate(time.Microsecond)
			id := fifoArrival(t, f, "audit-failure", now)
			expected := "queued"
			if action == "arrival.retry" {
				if ok, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease", now); err != nil || !ok {
					t.Fatalf("lease: %t %v", ok, err)
				}
				if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", "start", id, "arrival.replay.started", now); err != nil {
					t.Fatal(err)
				}
				if err := f.Store.ReleaseDisplayLease(t.Context(), "holder", "lease", now); err != nil {
					t.Fatal(err)
				}
				expected = "skipped"
			}
			var before int
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := f.SQL.ExecContext(t.Context(), `ALTER TABLE arrival_operator_audit
 ADD CONSTRAINT reject_arrival_mutation_audit CHECK (action NOT IN ('arrival.skip','arrival.retry'))`); err != nil {
				t.Fatal(err)
			}
			trusted := store.WithOperatorActor(t.Context(), "trusted-test-operator")
			var mutationErr error
			if action == "arrival.skip" {
				mutationErr = f.Store.SkipArrival(trusted, id, now)
			} else {
				mutationErr = f.Store.RetryDisplayArrival(trusted, id, now)
			}
			if mutationErr == nil {
				t.Fatal("operator mutation succeeded without its audit")
			}
			var status string
			var after int
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT replay_status FROM arrival_events WHERE id=$1`, id).Scan(&status); err != nil || status != expected {
				t.Fatalf("unaudited mutation committed: %s want %s err=%v", status, expected, err)
			}
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events`).Scan(&after); err != nil || after != before {
				t.Fatalf("unaudited mutation emitted events: before=%d after=%d err=%v", before, after, err)
			}
		})
	}
}
