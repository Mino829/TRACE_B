package store_test

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/route"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

// checkpointFixture keeps route assertions independent of live mapping data:
// two selectable photo nodes 40m apart through a non-selectable arrival node.
func checkpointFixture(t *testing.T, f *testdb.Fixture) {
	t.Helper()
	lonStep := 20 / (6378137 * math.Cos(35*math.Pi/180) * math.Pi / 180)
	raw := fmt.Sprintf(`{"schemaVersion":1,"campusGraphVersion":"g1","coordinateVersion":"c1","threeDMapVersion":"m1","nodes":[`+
		`{"id":"cp-a","lat":35,"lon":139,"local":{"east":0,"north":0},"gpsSnap":true,"role":"checkpoint","selectable":true},`+
		`{"id":"mid","lat":35,"lon":%.9f,"local":{"east":20,"north":0},"gpsSnap":true,"role":"checkpoint","selectable":false},`+
		`{"id":"cp-b","lat":35,"lon":%.9f,"local":{"east":40,"north":0},"gpsSnap":true,"role":"waypoint","selectable":true}],`+
		`"edges":[{"id":"am","from":"cp-a","to":"mid","kind":"ground","lengthMetres":20,"passable":true},`+
		`{"id":"mb","from":"mid","to":"cp-b","kind":"ground","lengthMetres":20,"passable":true}]}`, 139+lonStep, 139+2*lonStep)
	g, err := route.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Store.ConfigureRoutes(g, "c1", "g1", "m1"); err != nil {
		t.Fatal(err)
	}
}

func checkpointPhoto(name string, memoryID string, checkpoint string, at time.Time) store.Photo {
	p := photoInput(memoryID, name, at)
	p.CheckpointID = checkpoint
	return p
}

func TestCheckpointPhotoRegistrationRecordsGraphVersion(t *testing.T) {
	f := testdb.Open(t)
	checkpointFixture(t, f)
	now := time.Now().UTC().Truncate(time.Microsecond)
	memory := fixtureExperience(t, f, now)
	id := memory.MemorySessionID

	for _, checkpoint := range []string{"classroom-entrance", "mid"} {
		rejected := photoInput(id, "photo-rejected", now)
		rejected.CheckpointID = checkpoint
		if _, err := f.Store.CreatePhoto(t.Context(), rejected, now.Add(3*time.Minute)); !errors.Is(err, store.ErrInvalidCheckpoint) {
			t.Fatalf("unknown or non-selectable photo location %q: %v", checkpoint, err)
		}
	}
	missing := photoInput(id, "photo-missing", now)
	missing.CheckpointID, missing.Location = "cp-a", &store.PhotoLocation{Latitude: 35, Longitude: 139, AccuracyM: 1, RecordedAt: now}
	if _, err := f.Store.CreatePhoto(t.Context(), missing, now.Add(3*time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("checkpoint plus GPS rejected: %v", err)
	}
	created := createPhoto(t, f, checkpointPhoto("photo-a", id, "cp-a", now), now.Add(3*time.Minute))
	if created.CheckpointID != "cp-a" || created.CampusGraphVersion != "g1" {
		t.Fatalf("registration dropped checkpoint data: %+v", created)
	}
	p, err := f.Store.Photo(t.Context(), id, created.ID)
	if err != nil || p.CheckpointID != "cp-a" || p.Location != nil || p.CampusGraphVersion != "g1" {
		t.Fatalf("checkpoint not persisted: %+v %v", p, err)
	}
}

func TestCheckpointRouteSegments(t *testing.T) {
	f := testdb.Open(t)
	checkpointFixture(t, f)
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := fixtureExperience(t, f, now).MemorySessionID
	ready := func(p store.Photo) *store.Photo {
		created := createPhoto(t, f, p, now.Add(3*time.Minute))
		if err := f.Store.ReadyPhoto(t.Context(), id, created.ID, now.Add(4*time.Minute)); err != nil {
			t.Fatal(err)
		}
		return &created
	}
	segments := func() []store.RouteSegment {
		v, err := f.Store.RouteSegments(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// A single photo builds no segment, matching the GPS behaviour.
	one := ready(checkpointPhoto("photo-a", id, "cp-a", now))
	if got := segments(); len(got) != 0 {
		t.Fatalf("single checkpoint photo built segment: %+v", got)
	}
	// The checkpoint pair must connect as a node-to-node shortest path.
	two := ready(checkpointPhoto("photo-b", id, "cp-b", now.Add(time.Minute)))
	if got := segments(); len(got) != 1 {
		t.Fatalf("pair expected one segment: %+v", got)
	}
	segment := segments()[0]
	if segment.AlgorithmVersion != route.CheckpointAlgorithmVersion || segment.Confidence != "high" || segment.CampusGraphVersion != "g1" {
		t.Fatalf("checkpoint segment metadata: %+v", segment)
	}
	if len(segment.Path) != 3 || segment.Path[0].X != 0 || segment.Path[2].X != 40 || segment.LengthM != 40 {
		t.Fatalf("checkpoint path: %+v", segment.Path)
	}
	if len(segment.PathNodeIDs) != 3 || segment.PathNodeIDs[0] != "cp-a" ||
		segment.PathNodeIDs[1] != "mid" || segment.PathNodeIDs[2] != "cp-b" {
		t.Fatalf("checkpoint path node IDs were not persisted: %+v", segment.PathNodeIDs)
	}
	if *segment.ToPhotoMomentID != two.ID || segment.FromPhotoMomentID != one.ID {
		t.Fatalf("pair endpoints: %+v", segment)
	}
	// A checkpoint ID reused in a newer Graph must not relocate an old photo.
	if _, err := f.SQL.ExecContext(t.Context(), "UPDATE photos SET campus_graph_version='previous-graph' WHERE id=$1", one.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.RefreshRoutes(t.Context(), id, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := segments(); len(got) != 1 || got[0].Confidence != "low" || len(got[0].Path) != 0 {
		t.Fatalf("old checkpoint resolved against a different graph: %+v", got)
	}
	if _, err := f.SQL.ExecContext(t.Context(), "UPDATE photos SET campus_graph_version='g1' WHERE id=$1", one.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.RefreshRoutes(t.Context(), id, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A legacy GPS photo between the checkpoints mixes into a node route.
	mixed := photoInput(id, "photo-legacy", now.Add(2*time.Minute))
	mixed.Location = &store.PhotoLocation{Latitude: 35, Longitude: 139 + 1.5*20/(6378137*math.Cos(35*math.Pi/180)*math.Pi/180), AccuracyM: 1, RecordedAt: now.Add(2 * time.Minute)}
	ready(mixed)
	if got := segments(); len(got) != 2 {
		t.Fatalf("mixed pair should add a segment: %+v", got)
	}
	second := segments()[1]
	if second.AlgorithmVersion != route.CheckpointAlgorithmVersion || second.Confidence != "high" || *second.ToPhotoMomentID != mixed.ID {
		t.Fatalf("mixed segment: %+v", second)
	}
	// Deleting the mixed photo reconnects only the checkpoint pair.
	if err := f.Store.QueuePhotoDeletion(t.Context(), id, mixed.ID, "deleted", ""); err != nil {
		t.Fatal(err)
	}
	if got := segments(); len(got) != 1 || got[0].FromPhotoMomentID != one.ID || *got[0].ToPhotoMomentID != two.ID {
		t.Fatalf("deletion recalculation: %+v", got)
	}
}

func TestArrivalCheckpointExtendsRoute(t *testing.T) {
	f := testdb.Open(t)
	checkpointFixture(t, f)
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := fixtureExperience(t, f, now).MemorySessionID
	insert_arrival := func(arrivedAt time.Time) {
		if _, err := f.SQL.ExecContext(t.Context(), `INSERT INTO arrival_events
 (id,memory_session_id,source,checkpoint_id,replay_status,skip_reason,arrived_at,photo_wait_until)
 VALUES ('arr-1',$1,'qr','cp-b','skipped','operator',$2,$2)`, id, arrivedAt); err != nil {
			t.Fatal(err)
		}
	}
	// An arrival without photos stays segment-free.
	insert_arrival(now)
	segments := func() []store.RouteSegment {
		v, err := f.Store.RouteSegments(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := segments(); len(got) != 0 {
		t.Fatalf("photo-less arrival built segment: %+v", got)
	}
	created := createPhoto(t, f, checkpointPhoto("photo-a", id, "cp-a", now), now.Add(3*time.Minute))
	if err := f.Store.ReadyPhoto(t.Context(), id, created.ID, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := segments()
	if len(got) != 1 {
		t.Fatalf("single photo session keeps only the arrival segment: %+v", got)
	}
	arrival := got[0]
	if arrival.FromPhotoMomentID != created.ID || arrival.ToPhotoMomentID != nil ||
		arrival.AlgorithmVersion != route.CheckpointAlgorithmVersion || arrival.Confidence != "high" ||
		len(arrival.Path) != 3 || arrival.Path[2].X != 40 {
		t.Fatalf("arrival segment: %+v", arrival)
	}
}

func TestArrivalWithUnresolvableCheckpointKeepsNoSegment(t *testing.T) {
	f := testdb.Open(t)
	checkpointFixture(t, f)
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := fixtureExperience(t, f, now).MemorySessionID
	created := createPhoto(t, f, checkpointPhoto("photo-a", id, "cp-a", now), now.Add(3*time.Minute))
	if err := f.Store.ReadyPhoto(t.Context(), id, created.ID, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `INSERT INTO arrival_events
 (id,memory_session_id,source,checkpoint_id,replay_status,skip_reason,arrived_at,photo_wait_until)
 VALUES ('arr-2',$1,'qr','retired-checkpoint','skipped','operator',$2,$2)`, id, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.RefreshRoutes(t.Context(), id, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	segments, err := f.Store.RouteSegments(t.Context(), id)
	if err != nil || len(segments) != 0 {
		t.Fatalf("unknown arrival checkpoint must stay segment-less: %+v %v", segments, err)
	}
}
