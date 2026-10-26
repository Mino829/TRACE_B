package route

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

//go:embed campus_graph.json
var campusGraph []byte

// PhotoEdgeAlgorithmVersion is used for segments between legacy GPS-anchored
// photos. Checkpoint-anchored pairs use CheckpointAlgorithmVersion.
const PhotoEdgeAlgorithmVersion = "photo-edges-dijkstra-1"

// CheckpointAlgorithmVersion marks segments built on checkpoint node pairs,
// where snap ambiguity and capture-time spacing do not apply.
const CheckpointAlgorithmVersion = "checkpoint-dijkstra-1"
const earthRadius = 6378137.0

var japanTime = time.FixedZone("JST", 9*3600)

// The graph's local coordinates are east/north metres. Story x/z uses east/north,
// not the Blender axes. The three spatial versions identify this projection.
type Point struct {
	T float64 `json:"t"`
	X float64 `json:"x"`
	Z float64 `json:"z"`
}

type Anchor struct {
	Latitude, Longitude, AccuracyM float64
	CapturedAt                     time.Time
	RecordedAt                     time.Time
}

type Result struct {
	Confidence string
	Path       []Point
	NodeIDs    []string
	LengthM    float64
}

type node struct {
	ID        string  `json:"id"`
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lon"`
	Local     struct {
		East  float64 `json:"east"`
		North float64 `json:"north"`
	} `json:"local"`
	GPSSnap bool   `json:"gpsSnap"`
	Role    string `json:"role"`
}

type hours struct {
	Weekdays   []int  `json:"weekdays"` // Sunday=0 through Saturday=6, Asia/Tokyo.
	Open       string `json:"open"`
	Close      string `json:"close"`
	start, end int
}

type closure struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type edge struct {
	ID         string    `json:"id"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	Kind       string    `json:"kind"`
	Length     float64   `json:"lengthMetres"`
	Passable   bool      `json:"passable"`
	Restricted bool      `json:"restricted"`
	Closed     bool      `json:"closed"`
	Hours      []hours   `json:"openingHours,omitempty"`
	Closures   []closure `json:"closures,omitempty"`
	a, b       int
}

type Graph struct {
	SchemaVersion           int    `json:"schemaVersion"`
	CampusGraphVersion      string `json:"campusGraphVersion"`
	CoordinateVersion       string `json:"coordinateVersion"`
	ThreeDMapVersion        string `json:"threeDMapVersion"`
	Nodes                   []node `json:"nodes"`
	Edges                   []edge `json:"edges"`
	adj                     [][]int
	checkpointNodes         map[string]int
	originLat, originLon    float64
	originEast, originNorth float64
}

func Embedded() (*Graph, error) { return Parse(campusGraph) }

func Parse(data []byte) (*Graph, error) {
	var g Graph
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, err
	}
	var edgeFields struct {
		Edges []map[string]json.RawMessage `json:"edges"`
	}
	if err := json.Unmarshal(data, &edgeFields); err != nil {
		return nil, err
	}
	known := map[string]bool{"id": true, "from": true, "to": true, "kind": true, "lengthMetres": true,
		"planarLengthMetres": true, "passable": true, "restricted": true, "closed": true,
		"openingHours": true, "closures": true}
	for _, fields := range edgeFields.Edges {
		for name, value := range fields {
			if !known[name] || string(value) == "null" {
				return nil, fmt.Errorf("unsupported CampusGraph edge attribute %q", name)
			}
		}
	}
	if g.SchemaVersion != 1 || g.CampusGraphVersion == "" || g.CoordinateVersion == "" || g.ThreeDMapVersion == "" || len(g.Nodes) == 0 || len(g.Edges) == 0 {
		return nil, errors.New("unsupported or incomplete CampusGraph schema")
	}
	g.originLat, g.originLon = g.Nodes[0].Latitude, g.Nodes[0].Longitude
	g.originEast, g.originNorth = g.Nodes[0].Local.East, g.Nodes[0].Local.North
	ids := make(map[string]int, len(g.Nodes))
	checkpoints := make(map[string]int, len(g.Nodes))
	g.adj = make([][]int, len(g.Nodes))
	for i, n := range g.Nodes {
		if n.ID == "" || !finite(n.Latitude) || !finite(n.Longitude) || !finite(n.Local.East) || !finite(n.Local.North) || n.Latitude < -90 || n.Latitude > 90 || n.Longitude < -180 || n.Longitude > 180 {
			return nil, errors.New("invalid graph node")
		}
		if _, exists := ids[n.ID]; exists {
			return nil, errors.New("duplicate graph node")
		}
		ids[n.ID] = i
		if n.Role == "checkpoint" {
			checkpoints[n.ID] = i
		}
		x, z := g.project(n.Latitude, n.Longitude)
		if math.Hypot(x-n.Local.East, z-n.Local.North) > 0.3 {
			return nil, errors.New("graph local and WGS84 coordinates disagree")
		}
	}
	seen := make(map[string]bool, len(g.Edges))
	for i := range g.Edges {
		e := &g.Edges[i]
		var ok bool
		if e.a, ok = ids[e.From]; !ok {
			return nil, errors.New("edge references absent node")
		}
		if e.b, ok = ids[e.To]; !ok {
			return nil, errors.New("edge references absent node")
		}
		if e.ID == "" || seen[e.ID] || e.a == e.b || !finite(e.Length) || e.Length <= 0 {
			return nil, errors.New("invalid graph edge")
		}
		seen[e.ID] = true
		for j := range e.Hours {
			h := &e.Hours[j]
			var err error
			h.start, err = clock(h.Open)
			if err != nil {
				return nil, err
			}
			h.end, err = clock(h.Close)
			if err != nil || h.start >= h.end || len(h.Weekdays) == 0 {
				return nil, errors.New("invalid edge opening hours")
			}
			for _, day := range h.Weekdays {
				if day < 0 || day > 6 {
					return nil, errors.New("invalid edge weekday")
				}
			}
		}
		for _, c := range e.Closures {
			if c.Start.IsZero() || !c.Start.Before(c.End) {
				return nil, errors.New("invalid edge closure")
			}
		}
		g.adj[e.a] = append(g.adj[e.a], i)
		g.adj[e.b] = append(g.adj[e.b], i)
	}
	g.checkpointNodes = checkpoints
	return &g, nil
}

// CheckpointNode resolves a participant-chosen checkpoint ID to a graph node
// index. Only role=="checkpoint" nodes count; every other ID fails.
func (g *Graph) CheckpointNode(id string) (int, bool) {
	if id == "" {
		return -1, false
	}
	i, ok := g.checkpointNodes[id]
	return i, ok
}

func clock(s string) (int, error) {
	var h, m int
	if len(s) != 5 || s[2] != ':' || s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' || s[3] < '0' || s[3] > '9' || s[4] < '0' || s[4] > '9' {
		return 0, errors.New("invalid opening time")
	}
	h = int(s[0]-'0')*10 + int(s[1]-'0')
	m = int(s[3]-'0')*10 + int(s[4]-'0')
	if h > 23 || m > 59 {
		return 0, errors.New("invalid opening time")
	}
	return 60*h + m, nil
}
func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

func (g *Graph) Compatible(coordinate, graph, threeD string) error {
	if g.CoordinateVersion != coordinate || g.CampusGraphVersion != graph || g.ThreeDMapVersion != threeD {
		return fmt.Errorf("CampusGraph spatial versions are incompatible with configured coordinate/graph/3D versions")
	}
	return nil
}

func (g *Graph) project(lat, lon float64) (float64, float64) {
	return g.originEast + earthRadius*math.Cos(g.originLat*math.Pi/180)*(lon-g.originLon)*math.Pi/180,
		g.originNorth + earthRadius*(lat-g.originLat)*math.Pi/180
}

func (e *edge) available(at time.Time) bool {
	if !e.Passable || e.Restricted || e.Closed {
		return false
	}
	for _, c := range e.Closures {
		if !at.Before(c.Start) && at.Before(c.End) {
			return false
		}
	}
	if len(e.Hours) == 0 {
		return true
	}
	jst := at.In(japanTime)
	minute := jst.Hour()*60 + jst.Minute()
	for _, h := range e.Hours {
		if minute >= h.start && minute < h.end {
			for _, day := range h.Weekdays {
				if day == int(jst.Weekday()) {
					return true
				}
			}
		}
	}
	return false
}

type candidate struct {
	edge                     int
	fraction, x, z, distance float64
}

// A route between two photos may have traversed the edge at any time between
// captures. Only retain edges open for the entire interval.
func (e *edge) availableBetween(start, end time.Time) bool {
	if !e.available(start) || !e.available(end) {
		return false
	}
	for _, c := range e.Closures {
		if c.Start.Before(end) && c.End.After(start) {
			return false
		}
	}
	if len(e.Hours) > 0 {
		a, b := start.In(japanTime), end.In(japanTime)
		if a.Year() != b.Year() || a.YearDay() != b.YearDay() {
			return false
		}
		for _, h := range e.Hours {
			if a.Hour()*60+a.Minute() >= h.start && b.Hour()*60+b.Minute() < h.end {
				for _, day := range h.Weekdays {
					if day == int(a.Weekday()) {
						return true
					}
				}
			}
		}
		return false
	}
	return true
}

func (g *Graph) candidates(a Anchor, at time.Time) []candidate {
	if !finite(a.Latitude) || !finite(a.Longitude) || !finite(a.AccuracyM) || a.AccuracyM <= 0 || a.AccuracyM > 100 {
		return nil
	}
	x, z := g.project(a.Latitude, a.Longitude)
	var cs []candidate
	for i := range g.Edges {
		e := &g.Edges[i]
		if !e.available(at) || !g.Nodes[e.a].GPSSnap || !g.Nodes[e.b].GPSSnap {
			continue
		}
		p, q := g.Nodes[e.a].Local, g.Nodes[e.b].Local
		dx, dz := q.East-p.East, q.North-p.North
		d := dx*dx + dz*dz
		if d < 0.01 {
			continue
		} // No GPS snapping to vertical indoor links.
		fraction := math.Max(0, math.Min(1, ((x-p.East)*dx+(z-p.North)*dz)/d))
		px, pz := p.East+fraction*dx, p.North+fraction*dz
		distance := math.Hypot(px-x, pz-z)
		if distance <= a.AccuracyM {
			cs = append(cs, candidate{i, fraction, px, pz, distance})
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].distance < cs[j].distance })
	if len(cs) > 6 {
		cs = cs[:6]
	}
	return cs
}

func (g *Graph) shortest(from, to int, startAt, endAt time.Time) (float64, []int) {
	dist := make([]float64, len(g.Nodes))
	prev := make([]int, len(g.Nodes))
	used := make([]bool, len(g.Nodes))
	for i := range dist {
		dist[i] = math.Inf(1)
		prev[i] = -1
	}
	dist[from] = 0
	for range g.Nodes {
		v := -1
		for i := range dist {
			if !used[i] && (v < 0 || dist[i] < dist[v]) {
				v = i
			}
		}
		if v < 0 || math.IsInf(dist[v], 1) || v == to {
			break
		}
		used[v] = true
		for _, ei := range g.adj[v] {
			e := &g.Edges[ei]
			if !e.availableBetween(startAt, endAt) {
				continue
			}
			next := e.a
			if next == v {
				next = e.b
			}
			if dist[v]+e.Length < dist[next] {
				dist[next] = dist[v] + e.Length
				prev[next] = v
			}
		}
	}
	if math.IsInf(dist[to], 1) {
		return math.Inf(1), nil
	}
	path := []int{to}
	for v := to; v != from; {
		v = prev[v]
		if v < 0 {
			return math.Inf(1), nil
		}
		path = append(path, v)
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return dist[to], path
}

type option struct {
	score, length float64
	path          []Point
	signature     string
}

func (g *Graph) between(a, b candidate, startAt, endAt time.Time) option {
	ea, eb := g.Edges[a.edge], g.Edges[b.edge]
	if !ea.availableBetween(startAt, endAt) || !eb.availableBetween(startAt, endAt) {
		return option{score: math.Inf(1)}
	}
	best := option{score: math.Inf(1)}
	for _, start := range []struct {
		node   int
		length float64
	}{{ea.a, a.fraction * ea.Length}, {ea.b, (1 - a.fraction) * ea.Length}} {
		for _, end := range []struct {
			node   int
			length float64
		}{{eb.a, b.fraction * eb.Length}, {eb.b, (1 - b.fraction) * eb.Length}} {
			length, nodes := g.shortest(start.node, end.node, startAt, endAt)
			if nodes == nil {
				continue
			}
			length += start.length + end.length
			if length >= best.score {
				continue
			}
			path := []Point{{X: a.x, Z: a.z}}
			for _, n := range nodes {
				p := g.Nodes[n].Local
				path = append(path, Point{X: p.East, Z: p.North})
			}
			path = append(path, Point{X: b.x, Z: b.z})
			best = option{score: length, length: length, path: path, signature: ea.ID + ":" + eb.ID}
		}
	}
	if a.edge == b.edge {
		length := math.Abs(a.fraction-b.fraction) * ea.Length
		if length < best.score {
			best = option{score: length, length: length, path: []Point{{X: a.x, Z: a.z}, {X: b.x, Z: b.z}}, signature: ea.ID}
		}
	}
	return best
}

// Reconstruct does not join missing anchors with an invented straight line.
// Competing edge snaps reduce confidence even when one route has a lower score.
func (g *Graph) Reconstruct(from, to *Anchor) Result {
	unknown := Result{Confidence: "low", Path: []Point{}}
	if from == nil || to == nil || to.CapturedAt.Before(from.CapturedAt) {
		return unknown
	}
	if from.RecordedAt.IsZero() || to.RecordedAt.IsZero() ||
		from.CapturedAt.Sub(from.RecordedAt) > 60*time.Second || to.CapturedAt.Sub(to.RecordedAt) > 60*time.Second ||
		from.RecordedAt.Sub(from.CapturedAt) > 10*time.Second || to.RecordedAt.Sub(to.CapturedAt) > 10*time.Second {
		return unknown
	}
	a, b := g.candidates(*from, from.CapturedAt), g.candidates(*to, to.CapturedAt)
	if len(a) == 0 || len(b) == 0 {
		return unknown
	}
	var options []option
	for _, left := range a {
		for _, right := range b {
			o := g.between(left, right, from.CapturedAt, to.CapturedAt)
			if !math.IsInf(o.score, 1) {
				o.score += 2 * (left.distance + right.distance)
				options = append(options, o)
			}
		}
	}
	if len(options) == 0 {
		return unknown
	}
	sort.Slice(options, func(i, j int) bool { return options[i].score < options[j].score })
	best := options[0]
	confidence := "high"
	if from.AccuracyM > 30 || to.AccuracyM > 30 || a[0].distance > 10 || b[0].distance > 10 {
		confidence = "medium"
	}
	for _, alternative := range options[1:] {
		if alternative.signature != best.signature && alternative.score-best.score < math.Max(5, best.score*0.1) {
			confidence = "low"
			break
		}
	}
	// An implausible speed or very weak GPS estimate cannot establish an actual path.
	elapsed := to.CapturedAt.Sub(from.CapturedAt).Seconds()
	if (elapsed <= 0 && best.length > 0) || (elapsed > 0 && best.length/elapsed > 8) {
		confidence = "low"
	}
	if confidence == "low" {
		return unknown
	}
	if len(best.path) > 200 {
		return unknown
	}
	return Result{Confidence: confidence, Path: condense(best.path), LengthM: best.length}
}

func condense(path []Point) []Point {
	// Drop duplicate junction points; preserve sharp turns; never interpolate a
	// GPS history. A single point path is still kept renderable.
	out := make([]Point, 0, len(path))
	for _, p := range path {
		if len(out) == 0 || math.Hypot(out[len(out)-1].X-p.X, out[len(out)-1].Z-p.Z) > 0.01 {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return out
	}
	if len(out) == 1 {
		out = append(out, out[0])
	}
	for i := range out {
		out[i].T = float64(i) / float64(len(out)-1)
	}
	return out
}

type Endpoint struct {
	// Anchor is set for legacy photos that stored GPS. Checkpoint is the ID of
	// the participant-chosen checkpoint node, or empty without one.
	Anchor     *Anchor
	Checkpoint string
	CapturedAt time.Time
}

type mount struct {
	node  int
	extra float64
	point Point
}

func (g *Graph) placements(e *Endpoint, startAt, endAt time.Time) []mount {
	if e.Checkpoint != "" {
		node, ok := g.CheckpointNode(e.Checkpoint)
		if !ok {
			return nil
		}
		p := g.Nodes[node].Local
		return []mount{{node, 0, Point{X: p.East, Z: p.North}}}
	}
	if e.Anchor == nil {
		return nil
	}
	var out []mount
	for _, c := range g.candidates(*e.Anchor, e.CapturedAt) {
		ed := g.Edges[c.edge]
		if !ed.availableBetween(startAt, endAt) {
			continue
		}
		pt := Point{X: c.x, Z: c.z}
		out = append(out, mount{ed.a, c.fraction * ed.Length, pt}, mount{ed.b, (1 - c.fraction) * ed.Length, pt})
	}
	return out
}

// Connect builds the short route between two photo endpoints without coupling
// it to capture interval, snap competition or GPS plausibility checks: any pair
// that resolves to graph nodes uses checkpoint semantics. A segment between
// GPS-anchored photos must instead use Reconstruct.
func (g *Graph) Connect(from, to *Endpoint) Result {
	unknown := Result{Confidence: "low", Path: []Point{}}
	if from == nil || to == nil || to.CapturedAt.Before(from.CapturedAt) {
		return unknown
	}
	a, b := g.placements(from, from.CapturedAt, to.CapturedAt), g.placements(to, from.CapturedAt, to.CapturedAt)
	if len(a) == 0 || len(b) == 0 {
		return unknown
	}
	best := option{score: math.Inf(1)}
	var bestNodes []int
	for _, left := range a {
		for _, right := range b {
			length, nodes := g.shortest(left.node, right.node, from.CapturedAt, to.CapturedAt)
			if nodes == nil {
				continue
			}
			score := length + left.extra + right.extra
			if score >= best.score {
				continue
			}
			path := make([]Point, 0, len(nodes)+2)
			path = append(path, left.point)
			for _, n := range nodes {
				p := g.Nodes[n].Local
				if n == left.node && len(path) == 1 &&
					math.Hypot(left.point.X-p.East, left.point.Z-p.North) <= 0.01 {
					continue // The mount point already coincides with the start node.
				}
				path = append(path, Point{X: p.East, Z: p.North})
			}
			path = append(path, right.point)
			best = option{score: score, path: path}
			bestNodes = nodes
		}
	}
	if math.IsInf(best.score, 1) {
		return unknown
	}
	nodeIDs := make([]string, len(bestNodes))
	for i, n := range bestNodes {
		nodeIDs[i] = g.Nodes[n].ID
	}
	return Result{Confidence: "high", Path: condense(best.path), NodeIDs: nodeIDs, LengthM: best.score}
}
