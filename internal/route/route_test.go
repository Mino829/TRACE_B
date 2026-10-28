package route

import (
	"encoding/json"
	"math"
	"sort"
	"testing"
	"time"
)

func anchorOn(g *Graph, e edge, fraction, accuracy float64, at time.Time) *Anchor {
	a, b := g.Nodes[e.a], g.Nodes[e.b]
	return &Anchor{Latitude: a.Latitude + (b.Latitude-a.Latitude)*fraction, Longitude: a.Longitude + (b.Longitude-a.Longitude)*fraction, AccuracyM: accuracy, CapturedAt: at, RecordedAt: at}
}
func smallGraph(t *testing.T, edits func(*Graph)) *Graph {
	t.Helper()
	g := &Graph{SchemaVersion: 1, CampusGraphVersion: "g1", CoordinateVersion: "c1", ThreeDMapVersion: "3d1"}
	// 20m eastward steps with a parallel alternative 8m to the north.
	g.Nodes = []node{{ID: "a", Latitude: 35, Longitude: 139, GPSSnap: true}, {ID: "b", Latitude: 35, Longitude: 139 + 20/(earthRadius*math.Cos(35*math.Pi/180)*math.Pi/180), GPSSnap: true}, {ID: "c", Latitude: 35, Longitude: 139 + 40/(earthRadius*math.Cos(35*math.Pi/180)*math.Pi/180), GPSSnap: true}, {ID: "d", Latitude: 35 + 8/(earthRadius*math.Pi/180), Longitude: 139, GPSSnap: true}, {ID: "e", Latitude: 35 + 8/(earthRadius*math.Pi/180), Longitude: 139 + 20/(earthRadius*math.Cos(35*math.Pi/180)*math.Pi/180), GPSSnap: true}}
	for i := range g.Nodes {
		g.Nodes[i].Local.East = float64(i%3) * 20
		if i > 2 {
			g.Nodes[i].Local.East = float64(i-3) * 20
			g.Nodes[i].Local.North = 8
		}
	}
	g.Edges = []edge{{ID: "ab", From: "a", To: "b", Length: 20, Passable: true}, {ID: "bc", From: "b", To: "c", Length: 20, Passable: true}, {ID: "ad", From: "a", To: "d", Length: 8, Passable: true}, {ID: "de", From: "d", To: "e", Length: 20, Passable: true}}
	if edits != nil {
		edits(g)
	}
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestRealCampusGraphSnapsOnlySafeOutdoorEdges(t *testing.T) {
	g, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) < 300 || len(g.Edges) < 350 {
		t.Fatal("embedded graph unexpectedly incomplete")
	}
	if err := g.Compatible(g.CoordinateVersion, g.CampusGraphVersion, g.ThreeDMapVersion); err != nil {
		t.Fatal(err)
	}
	if err := g.Compatible("wrong", g.CampusGraphVersion, g.ThreeDMapVersion); err == nil {
		t.Fatal("version mismatch accepted")
	}
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	var checked bool
	for _, e := range g.Edges {
		if !e.Passable || !g.Nodes[e.a].GPSSnap || !g.Nodes[e.b].GPSSnap || e.Kind != "ground" {
			continue
		}
		from, to := anchorOn(g, e, .25, 1, now), anchorOn(g, e, .75, 1, now.Add(time.Minute))
		r := g.Reconstruct(from, to)
		if r.Confidence == "high" && len(r.Path) >= 2 && r.LengthM > 0 {
			checked = true
			break
		}
	}
	if !checked {
		t.Fatal("no confident real-world edge projection found")
	}
	for _, e := range g.Edges {
		if g.Nodes[e.a].GPSSnap && g.Nodes[e.b].GPSSnap {
			continue
		}
		// Indoor-only graph segments are excluded even when GPS appears exact.
		if e.Kind == "indoor" {
			if e.a == e.b {
				t.Fatal("invalid indoor edge")
			}
			break
		}
	}
}
func TestRestrictionsClosureAndOpeningHours(t *testing.T) {
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		modify func(*edge)
	}{
		{"impassable", func(e *edge) { e.Passable = false }},
		{"restricted", func(e *edge) { e.Restricted = true }},
		{"closed", func(e *edge) { e.Closed = true }},
		{"time-closed", func(e *edge) { e.Hours = []hours{{Weekdays: []int{0}, Open: "10:00", Close: "11:00"}} }},
		{"temporary-closure", func(e *edge) { e.Closures = []closure{{Start: now.Add(-time.Minute), End: now.Add(time.Minute)}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := smallGraph(t, func(g *Graph) { tc.modify(&g.Edges[0]) })
			from, to := anchorOn(g, g.Edges[0], .25, 1, now), anchorOn(g, g.Edges[1], .75, 1, now.Add(time.Minute))
			r := g.Reconstruct(from, to)
			if r.Confidence != "low" || len(r.Path) != 0 {
				t.Fatalf("unsafe route disclosed: %+v", r)
			}
		})
	}
}
func TestMissingAndAmbiguousAnchors(t *testing.T) {
	g := smallGraph(t, nil)
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	from, to := anchorOn(g, g.Edges[0], .5, 1, now), anchorOn(g, g.Edges[1], .5, 1, now.Add(2*time.Minute))
	if result := g.Reconstruct(from, to); result.Confidence != "high" {
		t.Fatalf("known route: %+v", result)
	}
	for _, r := range []Result{g.Reconstruct(nil, to), g.Reconstruct(from, nil), g.Reconstruct(&Anchor{Latitude: 36, Longitude: 139, AccuracyM: 1, CapturedAt: now}, to), g.Reconstruct(anchorOn(g, g.Edges[0], .5, 101, now), to)} {
		if r.Confidence != "low" || len(r.Path) != 0 {
			t.Fatalf("uncertain route presented as measured: %+v", r)
		}
	}
	near := anchorOn(g, g.Edges[0], .98, 5, now)
	if r := g.Reconstruct(near, to); r.Confidence != "low" || len(r.Path) != 0 {
		t.Fatalf("multiple equally plausible snaps: %+v", r)
	}
}
func TestUnsupportedEdgeContractFailsClosed(t *testing.T) {
	g := smallGraph(t, nil)
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	edges := doc["edges"].([]any)
	edges[0].(map[string]any)["futureAccessPolicy"] = "members-only"
	raw, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); err == nil {
		t.Fatal("unknown access policy was silently ignored")
	}
}

func TestGenerationP95(t *testing.T) {
	g, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	var e edge
	for _, candidate := range g.Edges {
		if candidate.Kind == "ground" && g.Nodes[candidate.a].GPSSnap && g.Nodes[candidate.b].GPSSnap {
			e = candidate
			break
		}
	}
	from, to := anchorOn(g, e, .25, 5, now), anchorOn(g, e, .75, 5, now.Add(time.Minute))
	timings := make([]time.Duration, 100)
	for i := range timings {
		start := time.Now()
		g.Reconstruct(from, to)
		timings[i] = time.Since(start)
	}
	sort.Slice(timings, func(i, j int) bool { return timings[i] < timings[j] })
	t.Logf("route generation p95 (%d samples, real graph): %s", len(timings), timings[94])
}

func TestConnectResolvesCheckpointPairs(t *testing.T) {
	g := smallGraph(t, nil)
	a, c := "a", "c"
	now := time.Now()
	result := g.Connect(&Endpoint{Checkpoint: "a", CapturedAt: now}, &Endpoint{Checkpoint: c, CapturedAt: now.Add(time.Minute)})
	if result.Confidence != "high" || len(result.Path) != 3 || result.LengthM != 40 {
		t.Fatalf("straight pair: %+v", result)
	}
	if result.Path[0].T != 0 || result.Path[2].T != 1 || result.Path[1].T != 0.5 {
		t.Fatalf("parametrisation broken: %+v", result.Path)
	}
	if len(result.NodeIDs) != 3 || result.NodeIDs[0] != "a" || result.NodeIDs[1] != "b" || result.NodeIDs[2] != "c" {
		t.Fatalf("display needs the graph's ordered node IDs: %+v", result.NodeIDs)
	}
	same := g.Connect(&Endpoint{Checkpoint: "a", CapturedAt: now}, &Endpoint{Checkpoint: "a", CapturedAt: now})
	if same.Confidence != "high" || len(same.Path) != 2 || same.LengthM != 0 {
		t.Fatalf("identical checkpoints: %+v", same)
	}
	if len(same.NodeIDs) != 1 || same.NodeIDs[0] != "a" {
		t.Fatalf("identical checkpoint needs its graph node: %+v", same.NodeIDs)
	}
	// A mixed pair resolves the GPS anchor onto edges reachable from the node.
	mixed := g.Connect(&Endpoint{Checkpoint: "a", CapturedAt: now},
		&Endpoint{Anchor: anchorOn(g, g.Edges[1], 0.5, 1, now.Add(time.Minute)), CapturedAt: now.Add(time.Minute)})
	if mixed.Confidence != "high" || len(mixed.Path) < 3 {
		t.Fatalf("mixed pair: %+v", mixed)
	}
	missing := g.Connect(&Endpoint{Checkpoint: "definitely-missing", CapturedAt: now}, &Endpoint{Checkpoint: "a", CapturedAt: now})
	if missing.Confidence != "low" || len(missing.Path) != 0 {
		t.Fatalf("unroutable pair accepted: %+v", missing)
	}
	reversed := g.Connect(&Endpoint{Checkpoint: a, CapturedAt: now.Add(time.Minute)}, &Endpoint{Checkpoint: "c", CapturedAt: now})
	if reversed.Confidence != "low" || len(reversed.Path) != 0 {
		t.Fatalf("reversed capture order accepted: %+v", reversed)
	}
}

func TestConnectPreservesGPSMountJunction(t *testing.T) {
	g := smallGraph(t, nil)
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	from := &Endpoint{Anchor: anchorOn(g, g.Edges[0], .5, 1, now), CapturedAt: now}
	to := &Endpoint{Checkpoint: "d", CapturedAt: now.Add(time.Minute)}
	got := g.Connect(from, to)
	if got.Confidence != "high" || math.Abs(got.LengthM-18) > .01 || len(got.Path) != 3 ||
		math.Abs(got.Path[0].X-10) > .1 || got.Path[1].X != 0 || got.Path[1].Z != 0 ||
		got.Path[2].X != 0 || got.Path[2].Z != 8 {
		t.Fatalf("route must follow the GPS mount through node a: %+v", got)
	}
}

func TestConnectRejectsGPSMountClosedBetweenPhotos(t *testing.T) {
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	g := smallGraph(t, func(g *Graph) {
		g.Edges[0].Closures = []closure{{Start: now.Add(20 * time.Second), End: now.Add(40 * time.Second)}}
	})
	from := &Endpoint{Anchor: anchorOn(g, g.Edges[0], .5, 1, now), CapturedAt: now}
	to := &Endpoint{Checkpoint: "b", CapturedAt: now.Add(time.Minute)}
	got := g.Connect(from, to)
	if got.Confidence != "low" || len(got.Path) != 0 {
		t.Fatalf("closed GPS mount disclosed as a passable route: %+v", got)
	}
}

func TestPhotoSelectionIsExplicitAndIndependentOfRouteEndpoints(t *testing.T) {
	g := smallGraph(t, func(g *Graph) { g.Nodes[0].Selectable = true })
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	nodes := document["nodes"].([]any)
	nodes[0].(map[string]any)["role"] = "waypoint"
	nodes[1].(map[string]any)["role"] = "checkpoint"
	nodes[2].(map[string]any)["role"] = "checkpoint"
	delete(nodes[2].(map[string]any), "selectable")
	nodes[3].(map[string]any)["selectable"] = nil
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	g, err = Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   string
		want bool
	}{{"a", true}, {"b", false}, {"c", false}, {"d", false}, {"", false}, {"missing", false}} {
		if _, ok := g.PhotoCheckpointNode(tc.id); ok != tc.want {
			t.Errorf("photo selection %q = %t, want %t", tc.id, ok, tc.want)
		}
	}
	now := time.Now().UTC()
	result := g.Connect(&Endpoint{Checkpoint: "a", CapturedAt: now}, &Endpoint{Checkpoint: "c", CapturedAt: now})
	if result.LengthM != 40 || len(result.NodeIDs) != 3 || result.NodeIDs[0] != "a" ||
		result.NodeIDs[1] != "b" || result.NodeIDs[2] != "c" {
		t.Fatalf("selection policy restricted internal route endpoints: %+v", result)
	}
}
