package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	ToPhotoMomentID    string        `json:"toPhotoMomentId"`
	CoordinateVersion  string        `json:"coordinateVersion"`
	CampusGraphVersion string        `json:"campusGraphVersion"`
	ThreeDMapVersion   string        `json:"threeDMapVersion"`
	AlgorithmVersion   string        `json:"algorithmVersion"`
	Confidence         string        `json:"confidence"`
	Path               []route.Point `json:"path"`
	LengthM            float64       `json:"lengthM"`
}

func (s *Store) RouteSegments(ctx context.Context, memoryID string) ([]RouteSegment, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id,from_photo_moment_id,to_photo_moment_id,coordinate_version,campus_graph_version,
 three_d_map_version,algorithm_version,confidence,path,length_m FROM route_segments WHERE memory_session_id=$1
 ORDER BY (SELECT captured_at FROM photos WHERE id=from_photo_moment_id),
 (SELECT sequence_no FROM photos WHERE id=from_photo_moment_id)`, memoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	segments := make([]RouteSegment, 0)
	for rows.Next() {
		var v RouteSegment
		var raw []byte
		if err := rows.Scan(&v.ID, &v.FromPhotoMomentID, &v.ToPhotoMomentID, &v.CoordinateVersion, &v.CampusGraphVersion, &v.ThreeDMapVersion, &v.AlgorithmVersion, &v.Confidence, &raw, &v.LengthM); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &v.Path); err != nil {
			return nil, err
		}
		segments = append(segments, v)
	}
	return segments, rows.Err()
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
	var previous *Photo
	for i := range photos {
		p := &photos[i]
		if p.Status != "ready" {
			continue
		}
		if previous != nil {
			var from, to *route.Anchor
			if previous.Location != nil {
				from = &route.Anchor{Latitude: previous.Location.Latitude, Longitude: previous.Location.Longitude, AccuracyM: previous.Location.AccuracyM, CapturedAt: previous.CapturedAt, RecordedAt: previous.Location.RecordedAt}
			}
			if p.Location != nil {
				to = &route.Anchor{Latitude: p.Location.Latitude, Longitude: p.Location.Longitude, AccuracyM: p.Location.AccuracyM, CapturedAt: p.CapturedAt, RecordedAt: p.Location.RecordedAt}
			}
			result := s.routes.Reconstruct(from, to)
			path, err := json.Marshal(result.Path)
			if err != nil {
				return err
			}
			key := sha256.Sum256([]byte(memoryID + "\x00" + previous.ID + "\x00" + p.ID))
			_, err = s.q.ExecContext(ctx, `INSERT INTO route_segments
 (id,memory_session_id,from_photo_moment_id,to_photo_moment_id,coordinate_version,campus_graph_version,
 three_d_map_version,algorithm_version,confidence,path,length_m)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, "route-"+hex.EncodeToString(key[:16]), memoryID, previous.ID, p.ID,
				s.routes.CoordinateVersion, s.routes.CampusGraphVersion, s.routes.ThreeDMapVersion, route.AlgorithmVersion, result.Confidence, path, result.LengthM)
			if err != nil {
				return err
			}
		}
		previous = p
	}
	return nil
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
