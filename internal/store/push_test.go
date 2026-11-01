package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func countPush(t *testing.T, f *testdb.Fixture) int {
	t.Helper()
	var count int
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM push_subscriptions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func pushFixture(memoryID string, interval int) store.PushSubscription {
	return store.PushSubscription{MemorySessionID: memoryID, EndpointEncrypted: []byte("encrypted-endpoint"), KeysEncrypted: []byte("encrypted-keys"), IntervalMinutes: interval}
}
func TestPushLifecycleAndProviderResponses(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := store.NewExperience{ExperienceID: "exp_push", MemorySessionID: "mem_push", LiveSessionID: "live_push", LinkID: "link_push", MemoryTokenHash: "mh", LiveTokenHash: "lh", LinkTokenHash: "kh", MemoryConsent: "v1", LiveConsent: "v1", ConsentLanguage: "ja", DisplayColor: "cyan", DisplaySymbol: "circle", CreatedAt: now, MemoryExpiresAt: now.Add(3 * time.Hour), MemoryDeleteAfter: now.Add(4 * time.Hour), LiveExpiresAt: now.Add(2 * time.Hour), LiveDeleteAfter: now.Add(3 * time.Hour), LivePresence: true}
	if err := f.Store.CreateExperience(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	p, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now)
	if err != nil || !p.NextSendAt.Equal(now.Add(30*time.Minute)) || countPush(t, f) != 1 {
		t.Fatal("subscribe", err)
	}
	ids, err := f.Store.DuePushIDs(t.Context(), p.NextSendAt, 10)
	if err != nil || len(ids) != 1 {
		t.Fatal("due not scheduled", err)
	}
	sent := 0
	send := func(got store.PushSubscription) (int, error) {
		sent++
		if string(got.EndpointEncrypted) != "encrypted-endpoint" {
			t.Fatal("lost ciphertext")
		}
		return 201, nil
	}
	if err := f.Store.DeliverPush(t.Context(), v.MemorySessionID, p.NextSendAt, send); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.DeliverPush(t.Context(), v.MemorySessionID, p.NextSendAt, send); err != nil || sent != 1 {
		t.Fatal("duplicate send", err)
	}
	paused := "paused"
	p, err = f.Store.PatchPush(t.Context(), v.MemorySessionID, nil, &paused, now.Add(31*time.Minute))
	if err != nil || p.Status != "paused" {
		t.Fatal("pause", err)
	}
	ids, err = f.Store.DuePushIDs(t.Context(), now.Add(45*time.Minute), 10)
	if err != nil || len(ids) != 0 {
		t.Fatal("paused job retained", err)
	}
	interval := 30
	active := "active"
	p, err = f.Store.PatchPush(t.Context(), v.MemorySessionID, &interval, &active, now.Add(32*time.Minute))
	if err != nil || !p.NextSendAt.Equal(now.Add(62*time.Minute)) {
		t.Fatal("resume", err)
	}
	// A photo-ready transition postpones only active subscriptions.
	if err := f.Store.PostponePush(t.Context(), v.MemorySessionID, now.Add(35*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ids, err = f.Store.DuePushIDs(t.Context(), now.Add(64*time.Minute), 10)
	if err != nil || len(ids) != 0 {
		t.Fatal("photo did not postpone", err)
	}
	sendExpired := func(store.PushSubscription) (int, error) { sent++; return 410, nil }
	if err := f.Store.DeliverPush(t.Context(), v.MemorySessionID, now.Add(65*time.Minute), sendExpired); err != nil || countPush(t, f) != 0 {
		t.Fatal("provider expired subscription not erased", err)
	}
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now.Add(66*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.StopLive(t.Context(), v.LiveSessionID, "participant_request", now.Add(67*time.Minute)); err != nil || countPush(t, f) != 0 {
		t.Fatal("stop retention", err)
	}
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now.Add(68*time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatal("stop resubscribed", err)
	}
}
func TestPushExpiryAndMemoryDeletion(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := fixtureExperience(t, f, now)
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now); err != nil {
		t.Fatal(err)
	}
	// Live expires sooner than Memory; never send after either lifetime.
	sent := false
	if err := f.Store.DeliverPush(t.Context(), v.MemorySessionID, v.LiveExpiresAt, func(store.PushSubscription) (int, error) { sent = true; return 201, nil }); err != nil || sent || countPush(t, f) != 0 {
		t.Fatal("expiry allowed send", err)
	}
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), v.LiveExpiresAt); !errors.Is(err, store.ErrConflict) {
		t.Fatal("expiry resubscribed", err)
	}
	// Memory-only participation does not depend on a Live session.
	other := store.NewExperience{ExperienceID: "memory-only", MemorySessionID: "memory-only", MemoryTokenHash: "hash", MemoryConsent: "v1", ConsentLanguage: "ja", DisplayColor: "cyan", DisplaySymbol: "circle", CreatedAt: now, MemoryExpiresAt: now.Add(time.Hour), MemoryDeleteAfter: now.Add(2 * time.Hour)}
	if err := f.Store.CreateExperience(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.PutPush(t.Context(), pushFixture(other.MemorySessionID, 30), now); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.DeleteMemory(t.Context(), other.MemorySessionID); err != nil || countPush(t, f) != 0 {
		t.Fatal("memory deletion retained keys", err)
	}
}

func TestReadyPhotoPostponesOnceAndDeletionQueuesEraseKeys(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := fixtureExperience(t, f, now)
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now); err != nil {
		t.Fatal(err)
	}
	photo := photoInput(v.MemorySessionID, "ready-push", now)
	if _, err := f.Store.CreatePhoto(t.Context(), photo, now); err != nil {
		t.Fatal(err)
	}
	at := now.Add(5 * time.Minute)
	if err := f.Store.ReadyPhoto(t.Context(), v.MemorySessionID, photo.ID, at); err != nil {
		t.Fatal(err)
	}
	ids, err := f.Store.DuePushIDs(t.Context(), now.Add(34*time.Minute), 10)
	if err != nil || len(ids) != 0 {
		t.Fatal("ready did not postpone", err)
	}
	if err := f.Store.ReadyPhoto(t.Context(), v.MemorySessionID, photo.ID, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ids, err = f.Store.DuePushIDs(t.Context(), now.Add(35*time.Minute), 10)
	if err != nil || len(ids) != 1 {
		t.Fatal("ready replay reset schedule", err)
	}
	if err := f.Store.QueueMemoryPhotoDeletion(t.Context(), v.MemorySessionID); err != nil || countPush(t, f) != 0 {
		t.Fatal("deleting memory retained keys", err)
	}
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now.Add(7*time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatal("deleting memory resubscribed", err)
	}
}

func pushPolicyExperience(t *testing.T, f *testdb.Fixture, now time.Time, live, continuation bool) store.NewExperience {
	t.Helper()
	v := store.NewExperience{
		ExperienceID: "policy-experience", MemorySessionID: "policy-memory",
		LiveSessionID: "policy-live", LinkID: "policy-link",
		MemoryTokenHash: "memory-hash", LiveTokenHash: "live-hash", LinkTokenHash: "link-hash",
		MemoryConsent: "reviewed-memory", LiveConsent: "live", ConsentLanguage: "ja",
		DisplayColor: "cyan", DisplaySymbol: "circle", CreatedAt: now,
		MemoryExpiresAt: now.Add(3 * time.Hour), MemoryDeleteAfter: now.Add(4 * time.Hour),
		LiveExpiresAt: now.Add(2 * time.Hour), LiveDeleteAfter: now.Add(3 * time.Hour),
		LivePresence: live, PushAfterArrival: continuation,
	}
	if err := f.Store.CreateExperience(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	var stored bool
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT push_after_arrival FROM memory_sessions WHERE id=$1`, v.MemorySessionID).Scan(&stored); err != nil || stored != continuation {
		t.Fatalf("policy snapshot: got=%t want=%t err=%v", stored, continuation, err)
	}
	return v
}

func acceptPushArrival(t *testing.T, f *testdb.Fixture, v store.NewExperience, now time.Time) {
	t.Helper()
	if _, err := f.Store.AcceptArrival(t.Context(), v.MemorySessionID, v.LinkTokenHash, "policy-arrival", "booth",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "policy-receipt", "policy-request", now); err != nil {
		t.Fatal(err)
	}
	if v.LivePresence {
		var phase string
		if err := f.SQL.QueryRowContext(t.Context(), `SELECT presence_phase FROM live_presence_sessions WHERE id=$1`, v.LiveSessionID).Scan(&phase); err != nil || phase != "arrived" {
			t.Fatalf("real arrival phase: %s %v", phase, err)
		}
	}
}

func TestArrivalWithoutContinuationTerminallyStopsPush(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := "memory-only"
		if live {
			name = "live-linked"
		}
		t.Run(name, func(t *testing.T) {
			f := testdb.Open(t)
			now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
			v := pushPolicyExperience(t, f, now, live, false)
			p, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now)
			if err != nil {
				t.Fatal(err)
			}
			acceptPushArrival(t, f, v, now.Add(time.Minute))
			var stopped bool
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT push_stopped FROM memory_sessions WHERE id=$1`, v.MemorySessionID).Scan(&stopped); err != nil || !stopped || countPush(t, f) != 0 {
				t.Fatalf("arrival did not terminally erase credentials: stopped=%t err=%v", stopped, err)
			}
			if err := f.Store.Cleanup(t.Context(), p.NextSendAt); err != nil {
				t.Fatal(err)
			}
			sent := false
			if err := f.Store.DeliverPush(t.Context(), v.MemorySessionID, p.NextSendAt, func(store.PushSubscription) (int, error) { sent = true; return 201, nil }); err != nil || sent {
				t.Fatalf("arrival allowed delivery: sent=%t err=%v", sent, err)
			}
			if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), p.NextSendAt); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("arrival allowed PUT: %v", err)
			}
			active := "active"
			if _, err := f.Store.PatchPush(t.Context(), v.MemorySessionID, nil, &active, p.NextSendAt); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("arrival allowed PATCH: %v", err)
			}
		})
	}
}

func TestArrivalContinuationSurvivesCleanupAndAllowsDeliveryAndMutations(t *testing.T) {
	for _, phase := range []string{"memory-only", "arrived", "on_site"} {
		t.Run(phase, func(t *testing.T) {
			f := testdb.Open(t)
			now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
			v := pushPolicyExperience(t, f, now, phase != "memory-only", true)
			p, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now)
			if err != nil {
				t.Fatal(err)
			}
			acceptPushArrival(t, f, v, now.Add(time.Minute))
			if phase == "on_site" {
				if err := f.Store.SetOnSite(t.Context(), v.LiveSessionID, "on-site", true, now.Add(2*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.Store.Cleanup(t.Context(), p.NextSendAt); err != nil {
				t.Fatal(err)
			}
			var stopped bool
			var next time.Time
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT m.push_stopped,p.next_send_at FROM memory_sessions m
 JOIN push_subscriptions p ON p.memory_session_id=m.id WHERE m.id=$1`, v.MemorySessionID).Scan(&stopped, &next); err != nil || stopped || !next.Equal(p.NextSendAt) {
				t.Fatalf("arrival or cleanup changed subscription: stopped=%t next=%v err=%v", stopped, next, err)
			}
			// A fresh Store must honor the durable snapshot without a runtime allowlist.
			reader, err := store.Open(t.Context(), f.URL, 5)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			ids, err := reader.DuePushIDs(t.Context(), p.NextSendAt, 10)
			if err != nil || len(ids) != 1 || ids[0] != v.MemorySessionID {
				t.Fatalf("arrival lost due notification: %v %v", ids, err)
			}
			sent := 0
			if err := reader.DeliverPush(t.Context(), v.MemorySessionID, p.NextSendAt, func(got store.PushSubscription) (int, error) {
				sent++
				if string(got.EndpointEncrypted) != "encrypted-endpoint" || string(got.KeysEncrypted) != "encrypted-keys" {
					t.Error("arrival changed stored credentials")
				}
				return 201, nil
			}); err != nil || sent != 1 {
				t.Fatalf("arrival blocked due delivery: sent=%d err=%v", sent, err)
			}
			at := p.NextSendAt.Add(time.Minute)
			if _, err := reader.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), at); err != nil {
				t.Fatalf("post-arrival PUT: %v", err)
			}
			paused, active := "paused", "active"
			if p, err := reader.PatchPush(t.Context(), v.MemorySessionID, nil, &paused, at); err != nil || p.Status != paused {
				t.Fatalf("post-arrival pause: %+v %v", p, err)
			}
			interval := 30
			if p, err := reader.PatchPush(t.Context(), v.MemorySessionID, &interval, &active, at.Add(time.Minute)); err != nil || !p.NextSendAt.Equal(at.Add(31*time.Minute)) {
				t.Fatalf("post-arrival resume: %+v %v", p, err)
			}
		})
	}
}

func TestStopWaitsForInflightPushAndFencesFutureSends(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := fixtureExperience(t, f, now)
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE live_presence_sessions SET expires_at=$2 WHERE id=$1`, v.LiveSessionID, now.Add(50*time.Minute)); err != nil {
		t.Fatal(err)
	}
	p, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	delivery := make(chan error, 1)
	go func() {
		delivery <- f.Store.DeliverPush(t.Context(), v.MemorySessionID, p.NextSendAt, func(store.PushSubscription) (int, error) {
			close(started)
			<-release
			return 201, nil
		})
	}()
	select {
	case <-started:
	case err := <-delivery:
		t.Fatal("delivery did not start", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- f.Store.StopLive(t.Context(), v.LiveSessionID, "participant_request", p.NextSendAt) }()
	close(release)
	if err := <-delivery; err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	sent := false
	if err := f.Store.DeliverPush(t.Context(), v.MemorySessionID, p.NextSendAt.Add(15*time.Minute), func(store.PushSubscription) (int, error) { sent = true; return 201, nil }); err != nil || sent || countPush(t, f) != 0 {
		t.Fatal("post-stop send or retained credentials", err)
	}
}

func TestPushRetriesAndUnsubscribePreventsDelivery(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := fixtureExperience(t, f, now)
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE live_presence_sessions SET expires_at=$2 WHERE id=$1`, v.LiveSessionID, now.Add(50*time.Minute)); err != nil {
		t.Fatal(err)
	}
	p, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 30), now)
	if err != nil {
		t.Fatal(err)
	}
	delivered := 0
	if err := f.Store.DeliverPush(t.Context(), v.MemorySessionID, p.NextSendAt, func(store.PushSubscription) (int, error) { delivered++; return 503, nil }); err != nil {
		t.Fatal(err)
	}
	ids, err := f.Store.DuePushIDs(t.Context(), p.NextSendAt.Add(59*time.Second), 10)
	if err != nil || len(ids) != 0 {
		t.Fatal("retry scheduled too early", err)
	}
	ids, err = f.Store.DuePushIDs(t.Context(), p.NextSendAt.Add(time.Minute), 10)
	if err != nil || len(ids) != 1 {
		t.Fatal("retry not scheduled", err)
	}
	if err := f.Store.DeletePush(t.Context(), v.MemorySessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.DeliverPush(t.Context(), v.MemorySessionID, p.NextSendAt.Add(time.Minute), func(store.PushSubscription) (int, error) { delivered++; return 201, nil }); err != nil || delivered != 1 || countPush(t, f) != 0 {
		t.Fatal("sent after unsubscribe", err)
	}
}
