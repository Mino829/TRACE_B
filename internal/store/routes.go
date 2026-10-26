package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/Mino829/trace_Backend/internal/route"
)

// ConfigureRoutes is called once before requests or cleanup begin. An active
// photo service must not start unless the configured spatial versions match.
func (s *Store) ConfigureRoutes(g *route.Graph, coordinate, graph, threeD string) error {
	if err := g.Compatible(coordinate, graph, threeD); err != nil {
		return err
	}
	s.routes = g
	return nil
}

type RouteSegment struct {
	ID                 string        `json:"routeSegmentId"`
	FromPhotoMomentID  string        `json:"fromPhotoMomentId"`
	ToPhotoMomentID    *string       `json:"toPhotoMomentId"`
	CoordinateVersion  string        `json:"coordinateVersion"`
	CampusGraphVersion string        `json:"campusGraphVersion"`
	ThreeDMapVersion   string        `json:"threeDMapVersion"`
	AlgorithmVersion   string        `json:"algorithmVersion"`
	Confidence         string        `json:"confidence"`
	Path               []route.Point `json:"path"`
	PathNodeIDs        []string      `json:"pathNodeIds"`
	LengthM            float64       `json:"lengthM"`
}

func (s *Store) RouteSegments(ctx context.Context, memoryID string) ([]RouteSegment, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id,from_photo_moment_id,to_photo_moment_id,coordinate_version,campus_graph_version,
 three_d_map_version,algorithm_version,confidence,path,path_node_ids,length_m FROM route_segments WHERE memory_session_id=$1
 ORDER BY (SELECT captured_at FROM photos WHERE id=from_photo_moment_id),
 (SELECT sequence_no FROM photos WHERE id=from_photo_moment_id)`, memoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	segments := make([]RouteSegment, 0)
	for rows.Next() {
		var v RouteSegment
		var to sql.NullString
		var raw, rawNodes []byte
		if err := rows.Scan(&v.ID, &v.FromPhotoMomentID, &to, &v.CoordinateVersion, &v.CampusGraphVersion, &v.ThreeDMapVersion, &v.AlgorithmVersion, &v.Confidence, &raw, &rawNodes, &v.LengthM); err != nil {
			return nil, err
		}
		if to.Valid {
			id := to.String
			v.ToPhotoMomentID = &id
		}
		if err := json.Unmarshal(raw, &v.Path); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(rawNodes, &v.PathNodeIDs); err != nil {
			return nil, err
		}
		segments = append(segments, v)
	}
	return segments, rows.Err()
}

// endpoint resolves photo metadata. A photo with a checkpoint ID uses the
// checkpoint graph node; a legacy photo falls back to its GPS anchor. A photo
// that resolves to nothing produces no endpoint and blocks its segments.
func (s *Store) photoEndpoint(p *Photo) *route.Endpoint {
	if p.CheckpointID != "" {
		// A graph revision can reuse the same ID at a different place. Never
		// reinterpret a saved photo against a different version of the graph.
		if p.CampusGraphVersion != s.routes.CampusGraphVersion {
			return nil
		}
		if _, ok := s.routes.CheckpointNode(p.CheckpointID); !ok {
			return nil
		}
		return &route.Endpoint{Checkpoint: p.CheckpointID, CapturedAt: p.CapturedAt}
	}
	if p.Location == nil {
		return nil
	}
	return &route.Endpoint{Anchor: &route.Anchor{Latitude: p.Location.Latitude, Longitude: p.Location.Longitude,
		AccuracyM: p.Location.AccuracyM, CapturedAt: p.CapturedAt, RecordedAt: p.Location.RecordedAt}, CapturedAt: p.CapturedAt}
}

// ReconcileRoutes holds the memory advisory lock through the photo state change
// and the segment replacement. This also handles out-of-order photo completions,
// retries, and delayed object deletion without showing stale adjacent segments.
func (s *Store) reconcileRoutes(ctx context.Context, memoryID string) error {
	if s.routes == nil {
		return nil
	}
	photos, err := s.Photos(ctx, memoryID)
	if err != nil {
		return err
	}
	if _, err := s.q.ExecContext(ctx, "DELETE FROM route_segments WHERE memory_session_id=$1", memoryID); err != nil {
		return err
	}
	ready := make([]int, 0, len(photos))
	for i := range photos {
		if photos[i].Status == "ready" {
			ready = append(ready, i)
		}
	}
	write := func(previous *Photo, toPhoto *string, from, to *route.Endpoint, algorithm string) error {
		result := route.Result{Confidence: "low", Path: []route.Point{}}
		if from != nil && to != nil {
			if algorithm == route.CheckpointAlgorithmVersion {
				// Checkpoint pairs resolve node to node; an unroutable graph
				// yields the unresolvable result on purpose.
				result = s.routes.Connect(from, to)
			} else {
				result = s.routes.Reconstruct(from.Anchor, to.Anchor)
			}
		}
		path, err := json.Marshal(result.Path)
		if err != nil {
			return err
		}
		nodes := result.NodeIDs
		if nodes == nil {
			nodes = []string{}
		}
		pathNodeIDs, err := json.Marshal(nodes)
		if err != nil {
			return err
		}
		pair := previous.ID
		if toPhoto != nil {
			pair += "\x00" + *toPhoto
		}
		key := sha256.Sum256([]byte(memoryID + "\x00" + pair))
		_, err = s.q.ExecContext(ctx, `INSERT INTO route_segments
 (id,memory_session_id,from_photo_moment_id,to_photo_moment_id,coordinate_version,campus_graph_version,
 three_d_map_version,algorithm_version,confidence,path,path_node_ids,length_m)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, "route-"+hex.EncodeToString(key[:16]), memoryID, previous.ID,
			toPhoto, s.routes.CoordinateVersion, s.routes.CampusGraphVersion, s.routes.ThreeDMapVersion,
			algorithm, result.Confidence, path, pathNodeIDs, result.LengthM)
		return err
	}
	for i := range ready {
		if i == 0 {
			continue
		}
		previous, current := &photos[ready[i-1]], &photos[ready[i]]
		toID := current.ID
		algorithm := route.PhotoEdgeAlgorithmVersion
		if previous.CheckpointID != "" || current.CheckpointID != "" {
			algorithm = route.CheckpointAlgorithmVersion
		}
		from, to := s.photoEndpoint(previous), s.photoEndpoint(current)
		// A pair with any missing or unresolvable endpoint stays segment-shaped
		// as an unresolvable row; the count tracks the photo sequence.
		if err := write(previous, &toID, from, to, algorithm); err != nil {
			return err
		}
	}
	if len(ready) > 0 {
		// A confirmed arrival appends a final segment to its checkpoint once the
		// session has a usable photo; zero photos mean no arrival segment.
		if err := s.appendArrivalSegment(ctx, memoryID, &photos[ready[len(ready)-1]], write); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) appendArrivalSegment(ctx context.Context, memoryID string, last *Photo, write func(*Photo, *string, *route.Endpoint, *route.Endpoint, string) error) error {
	var checkpointID string
	var arrivedAt time.Time
	err := s.q.QueryRowContext(ctx, `SELECT checkpoint_id,arrived_at FROM arrival_events WHERE memory_session_id=$1`,
		memoryID).Scan(&checkpointID, &arrivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, ok := s.routes.CheckpointNode(checkpointID); !ok {
		// An arrival from before the current graph, or with a checkpoint that
		// was retired, keeps no arrival segment.
		return nil
	}
	destination := &route.Endpoint{Checkpoint: checkpointID, CapturedAt: arrivedAt}
	if last.CapturedAt.After(arrivedAt) {
		return nil
	}
	return write(last, nil, s.photoEndpoint(last), destination, route.CheckpointAlgorithmVersion)
}

// Ready states are finalized before segments are visible. No GPS history is read.
func (s *Store) RefreshRoutes(ctx context.Context, memoryID string, now time.Time) error {
	return s.Transaction(ctx, memoryID, func(tx *Store) error {
		m, err := tx.MemorySession(ctx, memoryID)
		if err != nil {
			return err
		}
		if m.Deleting || !now.Before(m.DeleteAfter) {
			return ErrConflict
		}
		return tx.reconcileRoutes(ctx, memoryID)
	})
}
