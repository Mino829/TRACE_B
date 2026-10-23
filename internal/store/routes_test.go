package store_test

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/route"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func routeFixture(t *testing.T, f *testdb.Fixture) {
	t.Helper()
	// Explicit test graph isolates the store state transitions from live mapping revisions.
	east := 20 / (6378137 * math.Cos(35*math.Pi/180) * math.Pi / 180)
	raw := fmt.Sprintf(`{"schemaVersion":1,"campusGraphVersion":"g1","coordinateVersion":"c1","threeDMapVersion":"m1", "nodes":[{"id":"a","lat":35,"lon":139,"local":{"east":0,"north":0},"gpsSnap":true},{"id":"b","lat":35,"lon":%.9f,"local":{"east":20,"north":0},"gpsSnap":true},{"id":"c","lat":35,"lon":%.9f,"local":{"east":40,"north":0},"gpsSnap":true},{"id":"d","lat":35,"lon":%.9f,"local":{"east":60,"north":0},"gpsSnap":true}],"edges":[{"id":"ab","from":"a","to":"b","kind":"ground","lengthMetres":20,"passable":true},{"id":"bc","from":"b","to":"c","kind":"ground","lengthMetres":20,"passable":true},{"id":"cd","from":"c","to":"d","kind":"ground","lengthMetres":20,"passable":true}]}`, 139+east, 139+2*east, 139+3*east)
	graph, err := route.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Store.ConfigureRoutes(graph, "c1", "g1", "m1"); err != nil {
		t.Fatal(err)
	}
	if err = f.Store.ConfigureRoutes(graph, "wrong", "g1", "m1"); err == nil {
		t.Fatal("accepted mismatched spatial versions")
	}
}
func TestRoutesUpdateOnReadyAndDeletion(t *testing.T) {
	f := testdb.Open(t)
	routeFixture(t, f)
	now := time.Now().UTC().Truncate(time.Microsecond)
	memory := fixtureExperience(t, f, now)
	id := memory.MemorySessionID
	lonStep := 20 / (6378137 * math.Cos(35*math.Pi/180) * math.Pi / 180)
	register := func(name string, x float64, at time.Time) *store.Photo {
		t.Helper()
		p := photoInput(id, name, now)
		p.CapturedAt = at
		p.Location = &store.PhotoLocation{Latitude: 35, Longitude: 139 + lonStep*x/20, AccuracyM: 1, RecordedAt: at}
		created := createPhoto(t, f, p, now.Add(3*time.Minute))
		return &created
	}
	segments := func() []store.RouteSegment {
		t.Helper()
		v, err := f.Store.RouteSegments(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if len(segments()) != 0 {
		t.Fatal("empty session has a route")
	}
	a := register("a", 5, now)
	if err := f.Store.ReadyPhoto(t.Context(), id, a.ID, now); err != nil {
		t.Fatal(err)
	}
	if len(segments()) != 0 {
		t.Fatal("one photo has a segment")
	}
	b := register("b", 30, now.Add(time.Minute))
	c := register("c", 55, now.Add(2*time.Minute))
	if err := f.Store.ReadyPhoto(t.Context(), id, c.ID, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if r := segments(); len(r) != 1 || r[0].ToPhotoMomentID != c.ID {
		t.Fatalf("out-of-order completion: %+v", r)
	}
	if err := f.Store.ReadyPhoto(t.Context(), id, b.ID, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if r := segments(); len(r) != 2 || r[0].ToPhotoMomentID != b.ID || r[1].ToPhotoMomentID != c.ID || r[0].Confidence == "low" || r[0].CoordinateVersion != "c1" || r[0].AlgorithmVersion != route.AlgorithmVersion {
		t.Fatalf("persisted versioned routes: %+v", r)
	}
	if err := f.Store.QueuePhotoDeletion(t.Context(), id, b.ID, "deleted", ""); err != nil {
		t.Fatal(err)
	}
	if r := segments(); len(r) != 1 || r[0].FromPhotoMomentID != a.ID || r[0].ToPhotoMomentID != c.ID {
		t.Fatalf("did not bridge deleted photo: %+v", r)
	}
	if err := f.Store.FinishPhotoDeletion(t.Context(), id, b.ID); err != nil {
		t.Fatal(err)
	}
	if r := segments(); len(r) != 1 || r[0].ToPhotoMomentID != c.ID {
		t.Fatalf("deleted photo still referenced: %+v", r)
	}
	if err := f.Store.QueuePhotoDeletion(t.Context(), id, a.ID, "deleted", ""); err != nil {
		t.Fatal(err)
	}
	if len(segments()) != 0 {
		t.Fatal("one remaining ready photo has routes")
	}
}
func TestRouteUnknownLocationAndBackfill(t *testing.T) {
	f := testdb.Open(t)
	routeFixture(t, f)
	now := time.Now().UTC().Truncate(time.Microsecond)
	memory := fixtureExperience(t, f, now)
	first := createPhoto(t, f, photoInput(memory.MemorySessionID, "missing", now), now)
	second := photoInput(memory.MemorySessionID, "present", now)
	second.CapturedAt = now.Add(time.Minute)
	second.Location = &store.PhotoLocation{Latitude: 35, Longitude: 139, AccuracyM: 1, RecordedAt: now}
	present := createPhoto(t, f, second, now.Add(time.Minute))
	for _, p := range []store.Photo{first, present} {
		if err := f.Store.ReadyPhoto(t.Context(), memory.MemorySessionID, p.ID, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	segments, err := f.Store.RouteSegments(t.Context(), memory.MemorySessionID)
	if err != nil || len(segments) != 1 || segments[0].Confidence != "low" || len(segments[0].Path) != 0 {
		t.Fatalf("missing anchor invented a path: %+v %v", segments, err)
	}
	if _, err = f.SQL.ExecContext(t.Context(), "DELETE FROM route_segments WHERE memory_session_id=$1", memory.MemorySessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.RefreshRoutes(t.Context(), memory.MemorySessionID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	segments, err = f.Store.RouteSegments(t.Context(), memory.MemorySessionID)
	if err != nil || len(segments) != 1 {
		t.Fatalf("route backfill failed: %+v %v", segments, err)
	}
}
