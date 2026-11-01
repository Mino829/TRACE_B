package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func TestContinuationStillHonorsAllPushStops(t *testing.T) {
	for _, guard := range []string{"memory-stop", "live-stop", "tracking-stopped", "stopped-at", "push-stopped", "deleting", "memory-expiry", "live-expiry", "unsubscribe", "provider-404", "provider-410", "global-stop"} {
		for _, cleanupFirst := range []bool{false, true} {
			name := guard + "/delivery"
			if cleanupFirst {
				name = guard + "/cleanup"
			}
			t.Run(name, func(t *testing.T) {
				f := testdb.Open(t)
				now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
				v := pushPolicyExperience(t, f, now, guard != "memory-expiry", true)
				p, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now)
				if err != nil {
					t.Fatal(err)
				}
				acceptPushArrival(t, f, v, now.Add(time.Minute))
				at := now.Add(2 * time.Minute)
				switch guard {
				case "memory-stop":
					_, err = f.Store.StopMemory(t.Context(), v.MemorySessionID, at)
				case "live-stop":
					err = f.Store.StopLive(t.Context(), v.LiveSessionID, "participant_request", at)
				case "stopped-at":
					_, err = f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET stopped_at=$2 WHERE id=$1`, v.MemorySessionID, at)
				case "tracking-stopped":
					_, err = f.SQL.ExecContext(t.Context(), `UPDATE live_presence_sessions SET tracking_status='stopped' WHERE id=$1`, v.LiveSessionID)
				case "push-stopped":
					_, err = f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET push_stopped=TRUE WHERE id=$1`, v.MemorySessionID)
				case "deleting":
					err = f.Store.QueueMemoryPhotoDeletion(t.Context(), v.MemorySessionID)
				case "memory-expiry":
					_, err = f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET expires_at=$2 WHERE id=$1`, v.MemorySessionID, p.NextSendAt)
				case "live-expiry":
					_, err = f.SQL.ExecContext(t.Context(), `UPDATE live_presence_sessions SET expires_at=$2 WHERE id=$1`, v.LiveSessionID, p.NextSendAt)
				case "unsubscribe":
					err = f.Store.DeletePush(t.Context(), v.MemorySessionID)
				case "provider-404", "provider-410":
					status := 404
					if guard == "provider-410" {
						status = 410
					}
					err = f.Store.DeliverPush(t.Context(), v.MemorySessionID, p.NextSendAt, func(store.PushSubscription) (int, error) { return status, nil })
				case "global-stop":
					err = f.Store.StopNotifications(store.WithOperatorActor(t.Context(), "verified-test-operator"), at)
				}
				if err != nil {
					t.Fatal(err)
				}
				if cleanupFirst {
					if err := f.Store.Cleanup(t.Context(), p.NextSendAt); err != nil || countPush(t, f) != 0 {
						t.Fatalf("cleanup retained ineligible credentials: %v", err)
					}
				}
				sent := false
				if err := f.Store.DeliverPush(t.Context(), v.MemorySessionID, p.NextSendAt, func(store.PushSubscription) (int, error) { sent = true; return 201, nil }); err != nil || sent || countPush(t, f) != 0 {
					t.Fatalf("continuation bypassed guard: sent=%t err=%v", sent, err)
				}
				if guard == "unsubscribe" || guard == "provider-404" || guard == "provider-410" {
					active := "active"
					if _, err := f.Store.PatchPush(t.Context(), v.MemorySessionID, nil, &active, p.NextSendAt); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("PATCH restored erased credentials: %v", err)
					}
					return
				}
				want := store.ErrConflict
				if guard == "global-stop" {
					want = store.ErrPushStopped
				}
				if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), p.NextSendAt); !errors.Is(err, want) {
					t.Fatalf("guard allowed PUT: %v", err)
				}
				active := "active"
				if _, err := f.Store.PatchPush(t.Context(), v.MemorySessionID, nil, &active, p.NextSendAt); !errors.Is(err, want) {
					t.Fatalf("guard allowed PATCH: %v", err)
				}
			})
		}
	}
}

func TestArrivalContinuationNeverCreatesOrRestoresSubscriptions(t *testing.T) {
	for _, previous := range []string{"never-subscribed", "unsubscribed", "provider-removed", "terminally-stopped"} {
		t.Run(previous, func(t *testing.T) {
			f := testdb.Open(t)
			now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
			v := pushPolicyExperience(t, f, now, true, true)
			if previous != "never-subscribed" {
				p, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now)
				if err != nil {
					t.Fatal(err)
				}
				switch previous {
				case "unsubscribed":
					err = f.Store.DeletePush(t.Context(), v.MemorySessionID)
				case "provider-removed":
					err = f.Store.DeliverPush(t.Context(), v.MemorySessionID, p.NextSendAt, func(store.PushSubscription) (int, error) { return 410, nil })
				case "terminally-stopped":
					err = f.Store.Transaction(t.Context(), v.MemorySessionID, func(tx *store.Store) error {
						return tx.EndPushForMemory(t.Context(), v.MemorySessionID)
					})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			at := now.Add(31 * time.Minute)
			acceptPushArrival(t, f, v, at)
			if err := f.Store.Cleanup(t.Context(), at); err != nil || countPush(t, f) != 0 {
				t.Fatalf("arrival restored credentials: %v", err)
			}
			var stopped bool
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT push_stopped FROM memory_sessions WHERE id=$1`, v.MemorySessionID).Scan(&stopped); err != nil || stopped != (previous == "terminally-stopped") {
				t.Fatalf("arrival changed terminal latch: stopped=%t err=%v", stopped, err)
			}
			sent := false
			if err := f.Store.DeliverPush(t.Context(), v.MemorySessionID, at, func(store.PushSubscription) (int, error) { sent = true; return 201, nil }); err != nil || sent {
				t.Fatalf("arrival created delivery: sent=%t err=%v", sent, err)
			}
			if previous == "terminally-stopped" {
				if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), at); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("true policy undid old terminal stop: %v", err)
				}
			}
		})
	}
}

func TestLegacyLiveArrivalWithoutContinuationStillBlocksPush(t *testing.T) {
	for _, phase := range []string{"arrived", "on_site"} {
		for _, cleanupFirst := range []bool{false, true} {
			name := phase + "/delivery"
			if cleanupFirst {
				name = phase + "/cleanup"
			}
			t.Run(name, func(t *testing.T) {
				f := testdb.Open(t)
				now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
				v := pushPolicyExperience(t, f, now, true, false)
				p, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now)
				if err != nil {
					t.Fatal(err)
				}
				// Legacy Live transitions may have no arrival event or terminal latch.
				if _, err := f.SQL.ExecContext(t.Context(), `UPDATE live_presence_sessions SET presence_phase=$2 WHERE id=$1`, v.LiveSessionID, phase); err != nil {
					t.Fatal(err)
				}
				if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), p.NextSendAt); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("legacy phase allowed PUT: %v", err)
				}
				active := "active"
				if _, err := f.Store.PatchPush(t.Context(), v.MemorySessionID, nil, &active, p.NextSendAt); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("legacy phase allowed PATCH: %v", err)
				}
				if cleanupFirst {
					if err := f.Store.Cleanup(t.Context(), p.NextSendAt); err != nil || countPush(t, f) != 0 {
						t.Fatalf("cleanup retained legacy credentials: %v", err)
					}
				}
				sent := false
				if err := f.Store.DeliverPush(t.Context(), v.MemorySessionID, p.NextSendAt, func(store.PushSubscription) (int, error) { sent = true; return 201, nil }); err != nil || sent || countPush(t, f) != 0 {
					t.Fatalf("legacy phase allowed delivery: sent=%t err=%v", sent, err)
				}
			})
		}
	}
}
