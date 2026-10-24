package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"sort"
	"time"
)

var ErrMarkersDisabled = errors.New("photo markers are disabled or lack approved spatial policy")

type MarkerConfig struct {
	// Approved must only be set after university approval of all three privacy parameters.
	Approved                                       bool
	WidthM                                         float64
	TTL                                            time.Duration
	SparseMinSessions                              int
	CoordinateVersion, GraphVersion, ThreeDVersion string
}

type MarkerPoint struct {
	Kind string  `json:"kind"`
	ID   string  `json:"id"`
	X    float64 `json:"x"`
	Z    float64 `json:"z"`
}

// ConfigureMarkers must run before serving requests. No defaults imply approval.
// This does not turn on either operator display switch.
func (s *Store) ConfigureMarkers(c MarkerConfig) error {
	if !c.Approved || !s.displayEnabled || s.routes == nil || c.WidthM < 5 || c.WidthM > 10 || math.IsNaN(c.WidthM) ||
		c.TTL < 15*time.Minute || c.TTL > 30*time.Minute || c.TTL%time.Second != 0 || c.SparseMinSessions < 3 ||
		s.routes.Compatible(c.CoordinateVersion, c.GraphVersion, c.ThreeDVersion) != nil {
		return ErrMarkersDisabled
	}
	s.markers = &c
	return nil
}

func (s *Store) markerPolicy() (*MarkerConfig, error) {
	if s.markers == nil || !s.displayEnabled || s.routes == nil || s.routes.Compatible(s.markers.CoordinateVersion, s.markers.GraphVersion, s.markers.ThreeDVersion) != nil {
		return nil, ErrMarkersDisabled
	}
	return s.markers, nil
}

type markerControl struct {
	pins, hotspots, sparse    bool
	coordinate, graph, threeD string
	width                     float64
	ttl, sparseMinimum        int
}

func (s *Store) markerControl(ctx context.Context) (markerControl, error) {
	var v markerControl
	err := s.q.QueryRowContext(ctx, `SELECT pins_enabled,hotspots_enabled,sparse_override,coordinate_version,graph_version,three_d_version,width_m,ttl_seconds,sparse_min_sessions FROM marker_control WHERE id=TRUE FOR UPDATE`).
		Scan(&v.pins, &v.hotspots, &v.sparse, &v.coordinate, &v.graph, &v.threeD, &v.width, &v.ttl, &v.sparseMinimum)
	return v, err
}
func (s *Store) validateMarkerControl(v markerControl) (*MarkerConfig, error) {
	c, err := s.markerPolicy()
	if err != nil {
		return nil, err
	}
	if v.coordinate != "" && (v.coordinate != c.CoordinateVersion || v.graph != c.GraphVersion || v.threeD != c.ThreeDVersion || v.width != c.WidthM || v.ttl != int(c.TTL/time.Second) || v.sparseMinimum != c.SparseMinSessions) {
		return nil, ErrConflict
	}
	return c, nil
}

// SetMarkerPinsEnabled and SetMarkerHotspotsEnabled change one operator switch
// under the global marker lock without overwriting the other switch.
func (s *Store) SetMarkerPinsEnabled(ctx context.Context, enabled bool, now time.Time) error {
	return s.setMarkerSwitch(ctx, "pins_enabled", enabled, now)
}
func (s *Store) SetMarkerHotspotsEnabled(ctx context.Context, enabled bool, now time.Time) error {
	return s.setMarkerSwitch(ctx, "hotspots_enabled", enabled, now)
}
func (s *Store) setMarkerSwitch(ctx context.Context, column string, enabled bool, now time.Time) error {
	return s.Transaction(ctx, "trace:markers", func(tx *Store) error {
		v, err := tx.markerControl(ctx)
		if err != nil {
			return err
		}
		if enabled {
			c, err := tx.validateMarkerControl(v)
			if err != nil {
				return err
			}
			if v.coordinate == "" {
				_, err = tx.q.ExecContext(ctx, `UPDATE marker_control SET coordinate_version=$1,graph_version=$2,three_d_version=$3,width_m=$4,ttl_seconds=$5,sparse_min_sessions=$6 WHERE id=TRUE`, c.CoordinateVersion, c.GraphVersion, c.ThreeDVersion, c.WidthM, int(c.TTL/time.Second), c.SparseMinSessions)
				if err != nil {
					return err
				}
			}
		}
		// column is selected exclusively by the two fixed public wrappers above.
		_, err = tx.q.ExecContext(ctx, "UPDATE marker_control SET "+column+"=$1 WHERE id=TRUE", enabled)
		if err != nil {
			return err
		}
		return tx.reconcileMarkers(ctx, now)
	})
}

// MarkerControls contains only operator-safe booleans; it exposes no
// participant counts, location, identifiers, or event timing.
type MarkerControls struct {
	PinsEnabled     bool `json:"pinsEnabled"`
	HotspotsEnabled bool `json:"hotspotsEnabled"`
	SparseOverride  bool `json:"sparseOverride"`
	PolicyApproved  bool `json:"policyApproved"`
	PinsVisible     bool `json:"pinsVisible"`
	HotspotsVisible bool `json:"hotspotsVisible"`
}

func (s *Store) MarkerStatus(ctx context.Context) (MarkerControls, error) {
	result := MarkerControls{}
	mismatch := false
	err := s.Transaction(ctx, "trace:markers", func(tx *Store) error {
		v, err := tx.markerControl(ctx)
		if err != nil {
			return err
		}
		result.PinsEnabled, result.HotspotsEnabled, result.SparseOverride = v.pins, v.hotspots, v.sparse
		_, policyErr := tx.validateMarkerControl(v)
		result.PolicyApproved = policyErr == nil && v.coordinate != ""
		mismatch = errors.Is(policyErr, ErrConflict)
		if err := tx.reconcileMarkers(ctx, time.Now()); err != nil {
			return err
		}
		return tx.q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM marker_visible WHERE kind='pin'),EXISTS(SELECT 1 FROM marker_visible WHERE kind='hotspot')`).Scan(&result.PinsVisible, &result.HotspotsVisible)
	})
	if err == nil && mismatch {
		err = ErrConflict
	}
	return result, err
}

// SetMarkerSparseOverride immediately hides all markers when an operator detects
// unexpectedly low attendance. The automatic distinct-session floor still applies.
func (s *Store) SetMarkerSparseOverride(ctx context.Context, sparse bool, now time.Time) error {
	return s.Transaction(ctx, "trace:markers", func(tx *Store) error {
		v, err := tx.markerControl(ctx)
		if err != nil {
			return err
		}
		if !sparse {
			if _, err = tx.validateMarkerControl(v); err != nil {
				return err
			}
		}
		_, err = tx.q.ExecContext(ctx, `UPDATE marker_control SET sparse_override=$1 WHERE id=TRUE`, sparse)
		if err != nil {
			return err
		}
		return tx.reconcileMarkers(ctx, now)
	})
}

func markerID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func (s *Store) markerBucketID(ctx context.Context, x, z int) (string, error) {
	var id string
	err := s.q.QueryRowContext(ctx, `SELECT public_id FROM marker_bucket_ids WHERE bucket_x=$1 AND bucket_z=$2`, x, z).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id, err = markerID()
	if err != nil {
		return "", err
	}
	_, err = s.q.ExecContext(ctx, `INSERT INTO marker_bucket_ids(bucket_x,bucket_z,public_id) VALUES ($1,$2,$3)`, x, z, id)
	return id, err
}

// readyMarker runs in the photo's transaction, after its ready transition. Never
// backfill photos finalized while this optional capability was disabled.
func (s *Store) readyMarker(ctx context.Context, p Photo, now time.Time) error {
	if p.Location == nil || s.markers == nil {
		return nil
	}
	if err := s.lockTransaction(ctx, "trace:markers"); err != nil {
		return err
	}
	v, err := s.markerControl(ctx)
	if err != nil {
		return err
	}
	c, err := s.validateMarkerControl(v)
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrMarkersDisabled) {
		// Optional display policy drift must never block a participant's upload.
		return s.reconcileMarkers(ctx, now)
	}
	if err != nil {
		return err
	}
	if !v.pins && !v.hotspots {
		return nil
	}
	x, z, eligible := s.routes.ProjectMarker(p.Location.Latitude, p.Location.Longitude, p.Location.AccuracyM, p.CapturedAt)
	if !eligible {
		return nil
	}
	bx, bz := math.Floor(x/c.WidthM), math.Floor(z/c.WidthM)
	if bx < -2147483648 || bx > 2147483647 || bz < -2147483648 || bz > 2147483647 {
		return ErrConflict
	}
	id, err := markerID()
	if err != nil {
		return err
	}
	expiry := now.Add(c.TTL)
	if p.DeleteAfter.Before(expiry) {
		expiry = p.DeleteAfter
	}
	if !now.Before(expiry) {
		return nil
	}
	_, err = s.q.ExecContext(ctx, `INSERT INTO marker_photos(photo_id,public_id,bucket_x,bucket_z,expires_at) VALUES ($1,$2,$3,$4,$5)`, p.ID, id, int(bx), int(bz), expiry)
	if err != nil {
		return err
	}
	return s.reconcileMarkers(ctx, now)
}

// retireMarker is called at deletion intent (not after asynchronous object deletion).
func (s *Store) retireMarker(ctx context.Context, photoID string, now time.Time) error {
	if err := s.lockTransaction(ctx, "trace:markers"); err != nil {
		return err
	}
	if _, err := s.q.ExecContext(ctx, `DELETE FROM marker_photos WHERE photo_id=$1`, photoID); err != nil {
		return err
	}
	return s.reconcileMarkers(ctx, now)
}

func (s *Store) retireMemoryMarkers(ctx context.Context, memoryID string, now time.Time) error {
	if err := s.lockTransaction(ctx, "trace:markers"); err != nil {
		return err
	}
	if _, err := s.q.ExecContext(ctx, `DELETE FROM marker_photos WHERE photo_id IN (SELECT id FROM photos WHERE memory_session_id=$1)`, memoryID); err != nil {
		return err
	}
	return s.reconcileMarkers(ctx, now)
}

// ValidateMarkerVersions must be applied to the display client's handshake
// before delivering marker snapshots or deltas.
func (s *Store) ValidateMarkerVersions(coordinate, graph, threeD string) error {
	c, err := s.markerPolicy()
	if err != nil {
		return err
	}
	if c.CoordinateVersion != coordinate || c.GraphVersion != graph || c.ThreeDVersion != threeD {
		return ErrConflict
	}
	return nil
}

// reconcileMarkers is called under the marker advisory lock. It materializes
// visible points and appends their display deltas in the same transaction.
// Always acquire the marker lock before the display cursor lock.
func (s *Store) reconcileMarkers(ctx context.Context, now time.Time) error {
	v, err := s.markerControl(ctx)
	if err != nil {
		return err
	}
	c, policyErr := s.validateMarkerControl(v)
	if _, err := s.q.ExecContext(ctx, `DELETE FROM marker_photos WHERE expires_at<=$1`, now); err != nil {
		return err
	}
	var desired map[string]MarkerPoint
	if policyErr == nil && v.coordinate != "" && !v.sparse && (v.pins || v.hotspots) {
		const eligible = `FROM marker_photos m JOIN photos p ON p.id=m.photo_id
 JOIN memory_sessions ms ON ms.id=p.memory_session_id
 WHERE p.status='ready' AND NOT ms.deleting AND m.expires_at>$1 AND p.delete_after>$1 AND ms.expires_at>$1`
		var distinct int
		if err := s.q.QueryRowContext(ctx, `SELECT COUNT(DISTINCT p.memory_session_id) `+eligible, now).Scan(&distinct); err != nil {
			return err
		}
		if distinct >= c.SparseMinSessions {
			desired = make(map[string]MarkerPoint)
			if v.pins {
				rows, err := s.q.QueryContext(ctx, `SELECT m.public_id,m.bucket_x,m.bucket_z `+eligible+` ORDER BY m.public_id`, now)
				if err != nil {
					return err
				}
				for rows.Next() {
					var id string
					var x, z int
					if err = rows.Scan(&id, &x, &z); err != nil {
						break
					}
					desired["pin:"+id] = MarkerPoint{Kind: "pin", ID: id, X: (float64(x) + 0.5) * c.WidthM, Z: (float64(z) + 0.5) * c.WidthM}
				}
				if err == nil {
					err = rows.Err()
				}
				rows.Close()
				if err != nil {
					return err
				}
			}
			if v.hotspots {
				rows, err := s.q.QueryContext(ctx, `SELECT m.bucket_x,m.bucket_z `+eligible+`
 GROUP BY m.bucket_x,m.bucket_z HAVING COUNT(DISTINCT p.memory_session_id)>=3 ORDER BY m.bucket_x,m.bucket_z`, now)
				if err != nil {
					return err
				}
				type bucket struct{ x, z int }
				var buckets []bucket
				for rows.Next() {
					var b bucket
					if err = rows.Scan(&b.x, &b.z); err != nil {
						break
					}
					buckets = append(buckets, b)
				}
				if err == nil {
					err = rows.Err()
				}
				rows.Close()
				if err != nil {
					return err
				}
				for _, b := range buckets {
					id, err := s.markerBucketID(ctx, b.x, b.z)
					if err != nil {
						return err
					}
					desired["hotspot:"+id] = MarkerPoint{Kind: "hotspot", ID: id, X: (float64(b.x) + 0.5) * c.WidthM, Z: (float64(b.z) + 0.5) * c.WidthM}
				}
			}
		}
	}
	rows, err := s.q.QueryContext(ctx, `SELECT kind,public_id,x,z FROM marker_visible ORDER BY kind,public_id`)
	if err != nil {
		return err
	}
	var existing map[string]MarkerPoint
	for rows.Next() {
		var p MarkerPoint
		if err = rows.Scan(&p.Kind, &p.ID, &p.X, &p.Z); err != nil {
			break
		}
		if existing == nil {
			existing = make(map[string]MarkerPoint)
		}
		existing[p.Kind+":"+p.ID] = p
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(existing))
	for key := range existing {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		p := existing[key]
		if next, ok := desired[key]; ok && next == p {
			continue
		}
		if _, err := s.q.ExecContext(ctx, `DELETE FROM marker_visible WHERE kind=$1 AND public_id=$2`, p.Kind, p.ID); err != nil {
			return err
		}
		// A removed point must not be replayable from an older display cursor.
		// The deliberate cursor gap forces clients to take an erased snapshot.
		created := "photo.marker.created"
		hidden := "photo.marker.deleted"
		if p.Kind == "hotspot" {
			created, hidden = "photo.hotspot.updated", "photo.hotspot.hidden"
		}
		if _, err := s.q.ExecContext(ctx, `DELETE FROM display_events WHERE kind=$1 AND data->>'id'=$2`, created, p.ID); err != nil {
			return err
		}
		if _, err := s.AppendDisplayEvent(ctx, hidden, map[string]string{"id": p.ID}, now.Truncate(15*time.Minute)); err != nil {
			return err
		}
	}
	if _, err := s.q.ExecContext(ctx, `DELETE FROM marker_bucket_ids b WHERE NOT EXISTS
 (SELECT 1 FROM marker_photos m WHERE m.bucket_x=b.bucket_x AND m.bucket_z=b.bucket_z)`); err != nil {
		return err
	}
	keys = keys[:0]
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		p := desired[key]
		if current, ok := existing[key]; ok && current == p {
			continue
		}
		if _, err := s.q.ExecContext(ctx, `INSERT INTO marker_visible(kind,public_id,x,z) VALUES ($1,$2,$3,$4)`, p.Kind, p.ID, p.X, p.Z); err != nil {
			return err
		}
		kind := "photo.marker.created"
		if p.Kind == "hotspot" {
			kind = "photo.hotspot.updated"
		}
		if _, err := s.AppendDisplayEvent(ctx, kind, p, now.Truncate(15*time.Minute)); err != nil {
			return err
		}
	}
	return nil
}
