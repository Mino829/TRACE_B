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
		ConsentLanguage: "ja", DisplayColor: "cyan", DisplaySymbol: "circle", CampusGraphVersion: "g1", CreatedAt: now,
		MemoryExpiresAt: now.Add(time.Hour), MemoryDeleteAfter: now.Add(2 * time.Hour),
		LiveExpiresAt: now.Add(30 * time.Minute), LiveDeleteAfter: now.Add(90 * time.Minute), LivePresence: true}
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
	if err != nil || !state.MemoryExpiresAt.Equal(v.MemoryExpiresAt) || !state.LiveExpiresAt.Equal(v.LiveExpiresAt) ||
		!state.MemoryDeleteAfter.Equal(v.MemoryDeleteAfter) || !state.LiveDeleteAfter.Equal(v.LiveDeleteAfter) {
		t.Fatal("independent session deadlines did not round trip:", err)
	}
	var tokenExpiry, linkExpiry time.Time
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT token_expires_at,expires_at FROM experience_links WHERE id=$1", v.LinkID).Scan(&tokenExpiry, &linkExpiry); err != nil ||
		!tokenExpiry.Equal(v.LiveExpiresAt) || !linkExpiry.Equal(v.LiveDeleteAfter) {
		t.Fatal("link token and relationship deadlines were not separated:", err)
	}
	p := store.Position{X: 10, Z: 20, AccuracyM: 8, RecordedAt: now, ReceivedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := f.Store.SavePosition(t.Context(), v.LiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.SaveIdempotency(t.Context(), "scope-hash", "key-hash", "request-hash", 204, []byte{}, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, active, err := f.Store.LiveSession(t.Context(), v.LiveSessionID)
	if err != nil || active == nil {
		t.Fatal("active position missing")
	}
	f.Store.PurgePositions(now.Add(61 * time.Second))
	_, removed, err := f.Store.LiveSession(t.Context(), v.LiveSessionID)
	if err != nil || removed != nil {
		t.Fatal("expired position remained in process memory")
	}
	var tableName *string
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT to_regclass('live_positions')::text").Scan(&tableName); err != nil || tableName != nil {
		t.Fatal("display positions still have a database table:", err)
	}
	if err := f.Store.Cleanup(t.Context(), v.LiveExpiresAt); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM experience_links").Scan(&remaining); err != nil || remaining != 1 {
		t.Fatal("link lost at tracking deadline:", err)
	}
	session, _, err := f.Store.LiveSession(t.Context(), v.LiveSessionID)
	if err != nil || session.TrackingStatus != "expired" {
		t.Fatal("live session not expired")
	}
	if _, err := f.Store.ExperienceByMemoryToken(t.Context(), v.ExperienceID, v.MemoryTokenHash, v.MemoryExpiresAt.Add(time.Second)); err != nil {
		t.Fatal("memory state should remain accessible after its expiry:", err)
	}
	if err := f.Store.Cleanup(t.Context(), v.LiveDeleteAfter); err != nil {
		t.Fatal(err)
	}
	state, err = f.Store.ExperienceByMemoryToken(t.Context(), v.ExperienceID, v.MemoryTokenHash, v.LiveDeleteAfter)
	if err != nil || state.LiveSessionID != "" {
		t.Fatal("live retention should not remove memory state:", err)
	}
	if err := f.Store.Cleanup(t.Context(), v.MemoryDeleteAfter); err != nil {
		t.Fatal(err)
	}
	var count int
	for _, table := range []string{"experiences", "memory_sessions", "live_presence_sessions", "experience_links"} {
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
func TestSessionExpiryAndDeleteAfterBoundaries(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC()
	v := fixtureExperience(t, f, now)
	point := store.Position{RecordedAt: v.LiveExpiresAt, ReceivedAt: v.LiveExpiresAt, ExpiresAt: v.LiveExpiresAt.Add(time.Minute)}
	if err := f.Store.SavePosition(t.Context(), v.LiveSessionID, point); !errors.Is(err, store.ErrConflict) {
		t.Fatal("position accepted at the live expiry boundary:", err)
	}
	for _, at := range []time.Time{v.MemoryExpiresAt, v.MemoryDeleteAfter.Add(-time.Microsecond)} {
		if _, err := f.Store.ExperienceByMemoryToken(t.Context(), v.ExperienceID, v.MemoryTokenHash, at); err != nil {
			t.Fatal("memory state not readable inside deletion window:", err)
		}
		if err := f.Store.AuthenticateMemory(t.Context(), v.MemorySessionID, v.MemoryTokenHash, at); err != nil {
			t.Fatal("memory cannot be deleted inside its deletion window:", err)
		}
	}
	if err := f.Store.AuthenticateLive(t.Context(), v.LiveSessionID, v.LiveTokenHash, v.LiveExpiresAt); err != nil {
		t.Fatal("live deletion token expired at tracking deadline:", err)
	}
	if err := f.Store.AuthenticateLive(t.Context(), v.LiveSessionID, v.LiveTokenHash, v.LiveDeleteAfter); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("live deletion token accepted at deletion deadline:", err)
	}
	if _, err := f.Store.ExperienceByMemoryToken(t.Context(), v.ExperienceID, v.MemoryTokenHash, v.MemoryDeleteAfter); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("memory state readable at deletion deadline:", err)
	}
}
func TestOnSiteRequiresArrivalAndExplicitConsent(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC()
	v := fixtureExperience(t, f, now)
	if err := f.Store.SetOnSite(t.Context(), v.LiveSessionID, "on-site-v1", true, now); !errors.Is(err, store.ErrConflict) {
		t.Fatal("approaching session became on-site:", err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), "UPDATE live_presence_sessions SET presence_phase='arrived' WHERE id=$1", v.LiveSessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.SetOnSite(t.Context(), v.LiveSessionID, "on-site-v1", true, now); err != nil {
		t.Fatal("arrival with consent rejected:", err)
	}
	live, _, err := f.Store.LiveSession(t.Context(), v.LiveSessionID)
	if err != nil || live.PresencePhase != "on_site" {
		t.Fatal("consent did not advance to on-site:", err)
	}
	if err := f.Store.SetOnSite(t.Context(), v.LiveSessionID, "", false, now); err != nil {
		t.Fatal("participant cannot stop after arrival:", err)
	}
	if err := f.Store.SetOnSite(t.Context(), v.LiveSessionID, "on-site-v1", true, now); !errors.Is(err, store.ErrConflict) {
		t.Fatal("ended session restarted:", err)
	}
}
