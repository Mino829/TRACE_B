package store_test

import (
	"errors"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
	"testing"
	"time"
)

func fixtureExperience(t *testing.T, f *testdb.Fixture, now time.Time) store.NewExperience {
	t.Helper()
	v := store.NewExperience{ExperienceID: "exp1", MemorySessionID: "mem1", LiveSessionID: "live1", LinkID: "link1",
		MemoryTokenHash: "memory-hash", LiveTokenHash: "live-hash", LinkTokenHash: "link-hash", MemoryConsent: "memory-v1", LiveConsent: "live-v1",
		ConsentLanguage: "ja", DisplayColor: "cyan", DisplaySymbol: "circle", CreatedAt: now, ExpiresAt: now.Add(time.Hour), DeleteAfter: now.Add(2 * time.Hour), LivePresence: true}
	if err := f.Store.CreateExperience(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	return v
}
func TestMigrationTimestampAndCleanup(t *testing.T) {
	f := testdb.Open(t)
	if err := f.Store.Migrate(t.Context()); err != nil {
		t.Fatal("repeat migration:", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := fixtureExperience(t, f, now)
	state, err := f.Store.ExperienceByMemoryToken(t.Context(), v.ExperienceID, v.MemoryTokenHash, now)
	if err != nil || !state.ExpiresAt.Equal(v.ExpiresAt) {
		t.Fatal("timestamp round trip failed:", err)
	}
	p := store.Position{X: 10, Z: 20, AccuracyM: 8, RecordedAt: now, ReceivedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := f.Store.SavePosition(t.Context(), v.LiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.SaveIdempotency(t.Context(), "scope-hash", "key-hash", "request-hash", 204, []byte{}, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.Cleanup(t.Context(), now.Add(61*time.Second)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM live_positions").Scan(&count); err != nil || count != 0 {
		t.Fatal("position TTL not cleaned")
	}
	if err := f.Store.Cleanup(t.Context(), v.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	session, _, err := f.Store.LiveSession(t.Context(), v.LiveSessionID)
	if err != nil || session.TrackingStatus != "expired" {
		t.Fatal("session not expired")
	}
	if err := f.Store.Cleanup(t.Context(), v.DeleteAfter); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"experiences", "memory_sessions", "live_presence_sessions", "experience_links", "live_positions"} {
		if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s not removed: %v", table, err)
		}
	}
	if _, err := f.Store.GetIdempotency(t.Context(), "scope-hash", "key-hash", now.Add(3*time.Hour)); err != nil {
		t.Fatal("receipt should survive session cleanup")
	}
	if err := f.Store.Cleanup(t.Context(), now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.GetIdempotency(t.Context(), "scope-hash", "key-hash", now.Add(24*time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("receipt TTL")
	}
}
func TestIndependentDeletionAndOutOfOrderPosition(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC()
	v := fixtureExperience(t, f, now)
	p := store.Position{X: 10, Z: 10, AccuracyM: 8, RecordedAt: now, ReceivedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := f.Store.SavePosition(t.Context(), v.LiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	p.RecordedAt = now.Add(-time.Second)
	if err := f.Store.SavePosition(t.Context(), v.LiveSessionID, p); !errors.Is(err, store.ErrConflict) {
		t.Fatal("older position accepted")
	}
	if err := f.Store.DeleteMemory(t.Context(), v.MemorySessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.AuthenticateLive(t.Context(), v.LiveSessionID, v.LiveTokenHash, now); err != nil {
		t.Fatal("live session was removed")
	}
	if _, err := f.Store.ExperienceByMemoryToken(t.Context(), v.ExperienceID, v.MemoryTokenHash, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("link/experience survived deletion")
	}
	if err := f.Store.StopLive(t.Context(), v.LiveSessionID, "participant_request", now); err != nil {
		t.Fatal(err)
	}
	p.RecordedAt = now.Add(time.Second)
	if err := f.Store.SavePosition(t.Context(), v.LiveSessionID, p); !errors.Is(err, store.ErrConflict) {
		t.Fatal("position accepted after stop")
	}
	if err := f.Store.SetOnSite(t.Context(), v.LiveSessionID, "on-site", true, now); !errors.Is(err, store.ErrConflict) {
		t.Fatal("stopped session restarted")
	}
}
func TestExpiredPositionHiddenBeforeCleanup(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC()
	v := fixtureExperience(t, f, now.Add(-time.Minute))
	p := store.Position{AccuracyM: 10, RecordedAt: now.Add(-time.Minute), ReceivedAt: now.Add(-time.Minute), ExpiresAt: now.Add(-time.Second)}
	if err := f.Store.SavePosition(t.Context(), v.LiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	_, position, err := f.Store.LiveSession(t.Context(), v.LiveSessionID)
	if err != nil || position != nil {
		t.Fatal("expired position visible before cleanup")
	}
}
