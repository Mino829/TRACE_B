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
	p, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 15), now)
	if err != nil || !p.NextSendAt.Equal(now.Add(15*time.Minute)) || countPush(t, f) != 1 {
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
	p, err = f.Store.PatchPush(t.Context(), v.MemorySessionID, nil, &paused, now.Add(16*time.Minute))
	if err != nil || p.Status != "paused" {
		t.Fatal("pause", err)
	}
	ids, err = f.Store.DuePushIDs(t.Context(), now.Add(30*time.Minute), 10)
	if err != nil || len(ids) != 0 {
		t.Fatal("paused job retained", err)
	}
	interval := 30
	active := "active"
	p, err = f.Store.PatchPush(t.Context(), v.MemorySessionID, &interval, &active, now.Add(17*time.Minute))
	if err != nil || !p.NextSendAt.Equal(now.Add(47*time.Minute)) {
		t.Fatal("resume", err)
	}
	// A photo-ready transition postpones only active subscriptions.
	if err := f.Store.PostponePush(t.Context(), v.MemorySessionID, now.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ids, err = f.Store.DuePushIDs(t.Context(), now.Add(49*time.Minute), 10)
	if err != nil || len(ids) != 0 {
		t.Fatal("photo did not postpone", err)
	}
	sendExpired := func(store.PushSubscription) (int, error) { sent++; return 410, nil }
	if err := f.Store.DeliverPush(t.Context(), v.MemorySessionID, now.Add(50*time.Minute), sendExpired); err != nil || countPush(t, f) != 0 {
		t.Fatal("provider expired subscription not erased", err)
	}
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 15), now.Add(51*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.StopLive(t.Context(), v.LiveSessionID, "participant_request", now.Add(52*time.Minute)); err != nil || countPush(t, f) != 0 {
		t.Fatal("stop retention", err)
	}
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 15), now.Add(53*time.Minute)); !errors.Is(err, store.ErrConflict) {
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
	if _, err := f.Store.PutPush(t.Context(), pushFixture(other.MemorySessionID, 15), now); err != nil {
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
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 15), now); err != nil {
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
	ids, err := f.Store.DuePushIDs(t.Context(), now.Add(19*time.Minute), 10)
	if err != nil || len(ids) != 0 {
		t.Fatal("ready did not postpone", err)
	}
	if err := f.Store.ReadyPhoto(t.Context(), v.MemorySessionID, photo.ID, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ids, err = f.Store.DuePushIDs(t.Context(), now.Add(20*time.Minute), 10)
	if err != nil || len(ids) != 1 {
		t.Fatal("ready replay reset schedule", err)
	}
	if err := f.Store.QueueMemoryPhotoDeletion(t.Context(), v.MemorySessionID); err != nil || countPush(t, f) != 0 {
		t.Fatal("deleting memory retained keys", err)
	}
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 15), now.Add(7*time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatal("deleting memory resubscribed", err)
	}
}

func TestArrivalHookAndNoResubscribe(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := fixtureExperience(t, f, now)
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 15), now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), "UPDATE live_presence_sessions SET presence_phase='arrived' WHERE id=$1", v.LiveSessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.Transaction(t.Context(), v.MemorySessionID, func(tx *store.Store) error {
		return tx.EndPushForMemory(t.Context(), v.MemorySessionID)
	}); err != nil {
		t.Fatal(err)
	}
	if countPush(t, f) != 0 {
		t.Fatal("arrival retained subscription")
	}
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 15), now.Add(time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatal("arrival resubscribed", err)
	}
}

func TestMemoryOnlyArrivalBlocksResubscribe(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := store.NewExperience{ExperienceID: "exp_only_push", MemorySessionID: "mem_only_push", MemoryTokenHash: "mh", MemoryConsent: "v1", ConsentLanguage: "ja", DisplayColor: "cyan", DisplaySymbol: "circle", CreatedAt: now, MemoryExpiresAt: now.Add(time.Hour), MemoryDeleteAfter: now.Add(2 * time.Hour)}
	if err := f.Store.CreateExperience(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 15), now); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.Transaction(t.Context(), v.MemorySessionID, func(tx *store.Store) error {
		return tx.EndPushForMemory(t.Context(), v.MemorySessionID)
	}); err != nil || countPush(t, f) != 0 {
		t.Fatal("memory-only arrival retained subscription", err)
	}
	if _, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 15), now.Add(time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatal("memory-only arrival resubscribed", err)
	}
}

func TestStopWaitsForInflightPushAndFencesFutureSends(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := fixtureExperience(t, f, now)
	p, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 15), now)
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
	p, err := f.Store.PutPush(t.Context(), pushFixture(v.MemorySessionID, 15), now)
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
