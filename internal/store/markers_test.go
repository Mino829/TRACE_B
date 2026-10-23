package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func markerFixture(t *testing.T, f *testdb.Fixture, now time.Time) {
	t.Helper()
	f.Store.EnableDisplay()
	routeFixture(t, f)
	if err := f.Store.ConfigureMarkers(store.MarkerConfig{
		Approved: true, WidthM: 10, TTL: 15 * time.Minute, SparseMinSessions: 3,
		CoordinateVersion: "c1", GraphVersion: "g1", ThreeDVersion: "m1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.SetMarkerPinsEnabled(t.Context(), true, now); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.SetMarkerHotspotsEnabled(t.Context(), true, now); err != nil {
		t.Fatal(err)
	}
}

func markerPhoto(t *testing.T, f *testdb.Fixture, i int, now time.Time, lon float64) (string, string) {
	t.Helper()
	memory := fmt.Sprintf("marker-memory-%d", i)
	if err := f.Store.CreateExperience(t.Context(), store.NewExperience{
		ExperienceID: fmt.Sprintf("marker-experience-%d", i), MemorySessionID: memory,
		MemoryTokenHash: fmt.Sprintf("marker-token-%d", i), MemoryConsent: "memory-v1",
		ConsentLanguage: "ja", CreatedAt: now, MemoryExpiresAt: now.Add(time.Hour),
		MemoryDeleteAfter: now.Add(2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("marker-photo-%d", i)
	p := photoInput(memory, id, now)
	p.Location = &store.PhotoLocation{Latitude: 35, Longitude: lon, AccuracyM: 3, RecordedAt: now}
	createPhoto(t, f, p, now)
	if err := f.Store.ReadyPhoto(t.Context(), memory, id, now); err != nil {
		t.Fatal(err)
	}
	return memory, id
}

func markerState(t *testing.T, f *testdb.Fixture, now time.Time, pins, hotspots int) store.DisplayState {
	t.Helper()
	state, err := f.Store.DisplaySnapshot(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pins) != pins || len(state.Hotspots) != hotspots {
		t.Fatalf("pins=%d hotspots=%d, wanted %d/%d", len(state.Pins), len(state.Hotspots), pins, hotspots)
	}
	return state
}

func markerDeltas(t *testing.T, f *testdb.Fixture, cursor int64, count int) []store.DisplayEvent {
	t.Helper()
	events, err := f.Store.DisplayEvents(t.Context(), cursor, 128)
	if err != nil || len(events) != count {
		t.Fatalf("display events from %d: %+v %v; want %d", cursor, events, err, count)
	}
	return events
}

func TestMarkersBoundaryReplayDeletionAndPrivacy(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	markerState(t, f, now, 0, 0)
	if err := f.Store.SetMarkerPinsEnabled(t.Context(), true, now); !errors.Is(err, store.ErrMarkersDisabled) {
		t.Fatalf("unapproved pins enabled: %v", err)
	}
	markerFixture(t, f, now)
	status, err := f.Store.MarkerStatus(t.Context())
	if err != nil || !status.SparseOverride || !status.PinsEnabled || !status.HotspotsEnabled || status.PinsVisible {
		t.Fatalf("unsafe initial operator status: %+v %v", status, err)
	}
	if err := f.Store.SetMarkerSparseOverride(t.Context(), false, now); err != nil {
		t.Fatal(err)
	}
	markerPhoto(t, f, 1, now, 139)
	markerState(t, f, now, 0, 0)
	markerPhoto(t, f, 2, now, 139)
	markerState(t, f, now, 0, 0)
	markerPhoto(t, f, 3, now, 139)
	state := markerState(t, f, now, 3, 1)
	for _, pin := range state.Pins {
		if strings.HasPrefix(pin.ID, "marker-") || pin.Kind != "pin" || pin.X != 5 || pin.Z != 5 {
			t.Fatalf("unsafe or unquantized pin: %+v", pin)
		}
	}
	visible := markerDeltas(t, f, 0, 4)
	previousKey := ""
	for _, e := range visible {
		if e.Type != "photo.marker.created" && e.Type != "photo.hotspot.updated" {
			t.Fatalf("unexpected marker delta: %+v", e)
		}
		if !e.OccurredAt.Equal(now.Truncate(15 * time.Minute)) {
			t.Fatalf("exact event timestamp leaked: %+v", e)
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(e.Data, &data); err != nil || len(data) != 4 {
			t.Fatalf("non-projected payload: %s %v", e.Data, err)
		}
		if strings.Contains(string(e.Data), "marker-memory") || strings.Contains(string(e.Data), "marker-photo") {
			t.Fatalf("photo/session identity leaked: %s", e.Data)
		}
		key := e.Type + ":" + string(data["id"])
		if previousKey > key {
			t.Fatalf("marker events are not deterministic: %s after %s", key, previousKey)
		}
		previousKey = key
	}
	before := state.Cursor
	outside := photoInput("marker-memory-1", "outside-campus-marker", now)
	outside.Location = &store.PhotoLocation{Latitude: 35.5, Longitude: 139.5, AccuracyM: 2, RecordedAt: now}
	createPhoto(t, f, outside, now)
	if err := f.Store.ReadyPhoto(t.Context(), outside.MemorySessionID, outside.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.ReadyPhoto(t.Context(), "marker-memory-1", "marker-photo-1", now); err != nil {
		t.Fatal(err)
	}
	markerState(t, f, now, 3, 1)
	markerDeltas(t, f, before, 0)
	// One session departing drops the global floor: all previously visible pins
	// and the hotspot disappear together, even for still-ready photographs.
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			failures <- f.Store.QueuePhotoDeletion(t.Context(), "marker-memory-3", "marker-photo-3", "deleted", "")
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	hidden := markerState(t, f, now, 0, 0)
	for _, e := range markerDeltas(t, f, before, 4) {
		if e.Type != "photo.marker.deleted" && e.Type != "photo.hotspot.hidden" {
			t.Fatalf("unexpected deletion delta: %+v", e)
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(e.Data, &data); err != nil || len(data) != 1 || data["id"] == nil {
			t.Fatalf("unsafe tombstone payload: %s %v", e.Data, err)
		}
	}
	if _, err := f.Store.DisplayEvents(t.Context(), 0, 128); !errors.Is(err, store.ErrDisplayResync) {
		t.Fatalf("removed public coordinates replayed from old cursor: %v", err)
	}
	if hidden.Cursor <= before {
		t.Fatal("erasure did not advance the display cursor")
	}
	if _, err := f.Store.DisplayEvents(t.Context(), hidden.Cursor+1, 128); !errors.Is(err, store.ErrDisplayResync) {
		t.Fatalf("future display cursor accepted: %v", err)
	}
	if err := f.Store.FinishPhotoDeletion(t.Context(), "marker-memory-3", "marker-photo-3"); err != nil {
		t.Fatal(err)
	}
	markerState(t, f, now, 0, 0)
	markerDeltas(t, f, hidden.Cursor, 0)
}

func TestMarkersGlobalSessionsAndHotspotBuckets(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	markerFixture(t, f, now)
	if err := f.Store.SetMarkerSparseOverride(t.Context(), false, now); err != nil {
		t.Fatal(err)
	}
	first, _ := markerPhoto(t, f, 1, now, 139)
	again := photoInput(first, "same-session-photo", now)
	again.Location = &store.PhotoLocation{Latitude: 35, Longitude: 139, AccuracyM: 3, RecordedAt: now}
	createPhoto(t, f, again, now)
	if err := f.Store.ReadyPhoto(t.Context(), first, again.ID, now); err != nil {
		t.Fatal(err)
	}
	markerPhoto(t, f, 2, now, 139)
	markerState(t, f, now, 0, 0) // Three photos from just two distinct sessions.
	east := 20 / (6378137 * math.Cos(35*math.Pi/180) * math.Pi / 180)
	markerPhoto(t, f, 3, now, 139+east)
	markerState(t, f, now, 4, 0) // Global floor met, but neither bucket has three sessions.
	markerPhoto(t, f, 4, now, 139)
	markerState(t, f, now, 5, 1)
	if err := f.Store.QueuePhotoDeletion(t.Context(), "marker-memory-4", "marker-photo-4", "deleted", ""); err != nil {
		t.Fatal(err)
	}
	markerState(t, f, now, 4, 0) // Bucket floor fails while global floor remains met.
}

func TestMarkersTTLAndSparseAndOperatorSwitches(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	markerFixture(t, f, now)
	for i := 1; i <= 3; i++ {
		markerPhoto(t, f, i, now, 139)
	}
	markerState(t, f, now, 0, 0) // Start does not silently clear the safety latch.
	if err := f.Store.SetMarkerSparseOverride(t.Context(), false, now); err != nil {
		t.Fatal(err)
	}
	state := markerState(t, f, now, 3, 1)
	if err := f.Store.SetMarkerSparseOverride(t.Context(), true, now); err != nil {
		t.Fatal(err)
	}
	markerState(t, f, now, 0, 0)
	if _, err := f.Store.DisplayEvents(t.Context(), 0, 128); !errors.Is(err, store.ErrDisplayResync) {
		t.Fatalf("sparse override retained replayable coordinates: %v", err)
	}
	if err := f.Store.SetMarkerSparseOverride(t.Context(), false, now); err != nil {
		t.Fatal(err)
	}
	markerState(t, f, now, 3, 1)
	if err := f.Store.SetMarkerPinsEnabled(t.Context(), false, now); err != nil {
		t.Fatal(err)
	}
	markerState(t, f, now, 0, 1)
	status, err := f.Store.MarkerStatus(t.Context())
	if err != nil || status.PinsEnabled || !status.HotspotsEnabled || status.PinsVisible || !status.HotspotsVisible {
		t.Fatalf("independent pin stop/status: %+v %v", status, err)
	}
	if err := f.Store.SetMarkerHotspotsEnabled(t.Context(), false, now); err != nil {
		t.Fatal(err)
	}
	markerState(t, f, now, 0, 0)
	if err := f.Store.SetMarkerPinsEnabled(t.Context(), true, now); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.SetMarkerHotspotsEnabled(t.Context(), true, now); err != nil {
		t.Fatal(err)
	}
	markerState(t, f, now.Add(15*time.Minute-time.Microsecond), 3, 1)
	if err := f.Store.RefreshDisplay(t.Context(), now.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	markerState(t, f, now.Add(15*time.Minute), 0, 0)
	if _, err := f.Store.DisplayEvents(t.Context(), state.Cursor, 128); !errors.Is(err, store.ErrDisplayResync) {
		t.Fatalf("expiry did not invalidate stale projection: %v", err)
	}
	markerState(t, f, now.Add(16*time.Minute), 0, 0)
}

func TestMarkersVersionMismatchAndMemoryDeletion(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	f.Store.EnableDisplay()
	routeFixture(t, f)
	for _, config := range []store.MarkerConfig{
		{Approved: false, WidthM: 10, TTL: 15 * time.Minute, SparseMinSessions: 3, CoordinateVersion: "c1", GraphVersion: "g1", ThreeDVersion: "m1"},
		{Approved: true, WidthM: 10, TTL: 14 * time.Minute, SparseMinSessions: 3, CoordinateVersion: "c1", GraphVersion: "g1", ThreeDVersion: "m1"},
		{Approved: true, WidthM: 10, TTL: 15 * time.Minute, SparseMinSessions: 3, CoordinateVersion: "c1", GraphVersion: "wrong", ThreeDVersion: "m1"},
	} {
		if err := f.Store.ConfigureMarkers(config); !errors.Is(err, store.ErrMarkersDisabled) {
			t.Fatalf("unsafe marker policy accepted: %+v %v", config, err)
		}
	}
	if err := f.Store.ConfigureMarkers(store.MarkerConfig{
		Approved: true, WidthM: 10, TTL: 15 * time.Minute, SparseMinSessions: 3,
		CoordinateVersion: "c1", GraphVersion: "g1", ThreeDVersion: "m1",
	}); err != nil {
		t.Fatal(err)
	}
	for _, versions := range [][3]string{{"wrong", "g1", "m1"}, {"c1", "wrong", "m1"}, {"c1", "g1", "wrong"}} {
		if err := f.Store.ValidateMarkerVersions(versions[0], versions[1], versions[2]); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("mismatched spatial versions accepted: %v", err)
		}
	}
	if err := f.Store.SetMarkerPinsEnabled(t.Context(), true, now); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.SetMarkerHotspotsEnabled(t.Context(), true, now); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.SetMarkerSparseOverride(t.Context(), false, now); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		markerPhoto(t, f, i, now, 139)
	}
	active := markerState(t, f, now, 3, 1)
	if err := f.Store.QueueMemoryPhotoDeletion(t.Context(), "marker-memory-3"); err != nil {
		t.Fatal(err)
	}
	markerState(t, f, now, 0, 0)
	markerPhoto(t, f, 4, now, 139)
	markerState(t, f, now, 3, 1)
	if err := f.Store.ConfigureMarkers(store.MarkerConfig{
		Approved: true, WidthM: 5, TTL: 15 * time.Minute, SparseMinSessions: 3,
		CoordinateVersion: "c1", GraphVersion: "g1", ThreeDVersion: "m1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.DisplaySnapshot(t.Context(), now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed quantization was not rejected: %v", err)
	}
	var count int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM marker_visible`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("policy mismatch retained projection: %d %v", count, err)
	}
	if _, err := f.Store.DisplayEvents(t.Context(), active.Cursor, 128); !errors.Is(err, store.ErrDisplayResync) {
		t.Fatalf("policy mismatch retained replayable coordinates: %v", err)
	}
	if _, err := f.Store.MarkerStatus(t.Context()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("policy mismatch not reported: %v", err)
	}
	if err := f.Store.SetMarkerPinsEnabled(t.Context(), true, now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("incompatible policy restarted pins: %v", err)
	}
	if err := f.Store.SetMarkerPinsEnabled(t.Context(), false, now); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.SetMarkerSparseOverride(t.Context(), true, now); err != nil {
		t.Fatal(err)
	}
	memory, id := markerPhoto(t, f, 5, now, 139)
	photo, err := f.Store.Photo(t.Context(), memory, id)
	if err != nil || photo.Status != "ready" {
		t.Fatalf("nonessential policy mismatch blocked ready photo: %+v %v", photo, err)
	}
}

func TestMarkerStartupWithoutApprovalErasesPersistedProjection(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	markerFixture(t, f, now)
	if err := f.Store.SetMarkerSparseOverride(t.Context(), false, now); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		markerPhoto(t, f, i, now, 139)
	}
	markerState(t, f, now, 3, 1)
	restarted, err := store.Open(t.Context(), f.URL, 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	restarted.EnableDisplay()
	status, err := restarted.MarkerStatus(t.Context())
	if err != nil || status.PolicyApproved || status.PinsVisible || status.HotspotsVisible {
		t.Fatalf("unapproved restart retained visible data: %+v %v", status, err)
	}
	snapshot, err := restarted.DisplaySnapshot(t.Context(), now)
	if err != nil || len(snapshot.Pins) != 0 || len(snapshot.Hotspots) != 0 {
		t.Fatalf("unapproved restart exposed marker snapshot: %+v %v", snapshot, err)
	}
	if _, err := restarted.DisplayEvents(t.Context(), 0, 128); !errors.Is(err, store.ErrDisplayResync) {
		t.Fatalf("unapproved restart replayed prior coordinates: %v", err)
	}
	if err := restarted.SetMarkerPinsEnabled(t.Context(), false, now); err != nil {
		t.Fatal(err)
	}
	if err := restarted.SetMarkerHotspotsEnabled(t.Context(), false, now); err != nil {
		t.Fatal(err)
	}
	if err := restarted.SetMarkerSparseOverride(t.Context(), true, now); err != nil {
		t.Fatal(err)
	}
}
