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

// PhotoMoment contains metadata only. Image bytes and attributes never reach this table.
type PhotoMoment struct {
	ID, MemorySessionID, ClientPhotoID, CheckpointID, RequestHash, CampusGraphVersion string
	CapturedAt                                                                        time.Time
	SequenceNo                                                                        int
}

const momentColumns = `id,memory_session_id,client_photo_id,checkpoint_id,request_hash,campus_graph_version,captured_at,sequence_no`

func scanMoment(row photoScanner) (PhotoMoment, error) {
	var p PhotoMoment
	err := row.Scan(&p.ID, &p.MemorySessionID, &p.ClientPhotoID, &p.CheckpointID,
		&p.RequestHash, &p.CampusGraphVersion, &p.CapturedAt, &p.SequenceNo)
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoMoment{}, ErrNotFound
	}
	if err == nil {
		p.CapturedAt = p.CapturedAt.UTC()
	}
	return p, err
}

// CreatePhotoMoment serializes quota, replay, sequence assignment and routes with deletion.
// A deleted client ID cannot be recreated before session retention ends.
func (s *Store) CreatePhotoMoment(ctx context.Context, p PhotoMoment, now time.Time) (PhotoMoment, bool, error) {
	var result PhotoMoment
	created := false
	err := s.Transaction(ctx, p.MemorySessionID, func(tx *Store) error {
		m, err := tx.activePhotoMemory(ctx, p.MemorySessionID, now)
		if err != nil {
			return err
		}
		if m.PhotoMode != PhotoModeDevice {
			return ErrConflict
		}
		var fenced bool
		if err := tx.q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_moment_fences
 WHERE memory_session_id=$1 AND client_photo_id=$2)`, p.MemorySessionID, p.ClientPhotoID).Scan(&fenced); err != nil {
			return err
		}
		if fenced {
			return ErrNotFound
		}
		existing, err := scanMoment(tx.q.QueryRowContext(ctx, `SELECT `+momentColumns+` FROM photo_moments
 WHERE memory_session_id=$1 AND client_photo_id=$2`, p.MemorySessionID, p.ClientPhotoID))
		if err == nil {
			if existing.RequestHash != p.RequestHash {
				return ErrConflict
			}
			result = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if p.ID == "" || p.ClientPhotoID == "" || p.RequestHash == "" || p.CheckpointID == "" ||
			p.CapturedAt.Before(m.CreatedAt) || p.CapturedAt.After(m.ExpiresAt) || p.CapturedAt.After(now.Add(5*time.Minute)) {
			return ErrConflict
		}
		if tx.routes == nil || m.CampusGraphVersion != tx.routes.CampusGraphVersion {
			return ErrConflict
		}
		if _, ok := tx.routes.PhotoCheckpointNode(p.CheckpointID); !ok {
			return ErrInvalidCheckpoint
		}
		var count int
		if err := tx.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_moments WHERE memory_session_id=$1`, p.MemorySessionID).Scan(&count); err != nil {
			return err
		}
		if count >= 12 {
			return ErrPhotoLimit
		}
		p.CampusGraphVersion = m.CampusGraphVersion
		if err := tx.q.QueryRowContext(ctx, `UPDATE memory_sessions SET photo_sequence=GREATEST(photo_sequence,
 (SELECT COALESCE(MAX(sequence_no),0) FROM photo_moments WHERE memory_session_id=$1))+1
 WHERE id=$1 RETURNING photo_sequence`, p.MemorySessionID).Scan(&p.SequenceNo); err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(p.ID))
		insert, err := tx.q.ExecContext(ctx, `INSERT INTO photo_moments
 (id,id_hash,memory_session_id,client_photo_id,checkpoint_id,request_hash,campus_graph_version,captured_at,sequence_no)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, p.ID, hex.EncodeToString(digest[:]),
			p.MemorySessionID, p.ClientPhotoID, p.CheckpointID, p.RequestHash, p.CampusGraphVersion, p.CapturedAt, p.SequenceNo)
		if err != nil {
			return err
		}
		if n, _ := insert.RowsAffected(); n != 1 {
			return ErrConflict
		}
		if err := tx.PostponePush(ctx, p.MemorySessionID, now); err != nil {
			return err
		}
		if err := tx.reconcileMomentRoutes(ctx, p.MemorySessionID); err != nil {
			return err
		}
		if err := tx.appendDisplayPhotoChange(ctx, p.MemorySessionID, "photo.moment.created", now); err != nil {
			return err
		}
		result, created = p, true
		return nil
	})
	return result, created, err
}

func (s *Store) PhotoMomentByHash(ctx context.Context, memoryID, photoIDHash string) (PhotoMoment, error) {
	return scanMoment(s.q.QueryRowContext(ctx, `SELECT `+momentColumns+` FROM photo_moments WHERE memory_session_id=$1 AND id_hash=$2`, memoryID, photoIDHash))
}

func (s *Store) PhotoMoments(ctx context.Context, memoryID string) ([]PhotoMoment, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+momentColumns+` FROM photo_moments WHERE memory_session_id=$1 ORDER BY captured_at,sequence_no`, memoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	moments := make([]PhotoMoment, 0)
	for rows.Next() {
		p, err := scanMoment(rows)
		if err != nil {
			return nil, err
		}
		moments = append(moments, p)
	}
	return moments, rows.Err()
}

// DeletePhotoMoment retains a minimal fence until memory deletion. Rebuild the
// surviving adjacency in the same transaction so no reader sees stale routes.
func (s *Store) DeletePhotoMoment(ctx context.Context, memoryID, photoID string, now time.Time) error {
	return s.Transaction(ctx, memoryID, func(tx *Store) error {
		m, err := tx.MemorySession(ctx, memoryID)
		if err != nil {
			return err
		}
		if m.PhotoMode != PhotoModeDevice || m.Deleting || !now.Before(m.DeleteAfter) {
			return ErrConflict
		}
		var clientID string
		err = tx.q.QueryRowContext(ctx, `SELECT client_photo_id FROM photo_moments WHERE memory_session_id=$1 AND id=$2`, memoryID, photoID).Scan(&clientID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `INSERT INTO photo_moment_fences (memory_session_id,client_photo_id)
 VALUES ($1,$2)`, memoryID, clientID); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM photo_moments WHERE memory_session_id=$1 AND id=$2`, memoryID, photoID); err != nil {
			return err
		}
		if err := tx.reconcileMomentRoutes(ctx, memoryID); err != nil {
			return err
		}
		return tx.appendDisplayPhotoChange(ctx, memoryID, "photo.moment.deleted", now)
	})
}

func (s *Store) MomentRouteSegments(ctx context.Context, memoryID string) ([]RouteSegment, error) {
	return s.routeSegments(ctx, memoryID, `SELECT id,from_photo_moment_id,to_photo_moment_id,coordinate_version,campus_graph_version,
 three_d_map_version,algorithm_version,confidence,path,path_node_ids,length_m FROM moment_route_segments WHERE memory_session_id=$1
 ORDER BY (SELECT captured_at FROM photo_moments WHERE id=from_photo_moment_id),
 (SELECT sequence_no FROM photo_moments WHERE id=from_photo_moment_id)`)
}

// PhotoMomentMemory reads one committed snapshot, including when bulk expiry
// cleanup bypasses the session mutation lock. Writers need not wait for readers.
func (s *Store) PhotoMomentMemory(ctx context.Context, memoryID string) ([]PhotoMoment, []RouteSegment, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	view := Store{q: tx}
	moments, err := view.PhotoMoments(ctx, memoryID)
	if err != nil {
		return nil, nil, err
	}
	segments, err := view.MomentRouteSegments(ctx, memoryID)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return moments, segments, nil
}

func (s *Store) RefreshMomentRoutes(ctx context.Context, memoryID string, now time.Time) error {
	return s.Transaction(ctx, memoryID, func(tx *Store) error {
		m, err := tx.MemorySession(ctx, memoryID)
		if err != nil {
			return err
		}
		if m.PhotoMode != PhotoModeDevice || m.Deleting || !now.Before(m.DeleteAfter) {
			return ErrConflict
		}
		return tx.reconcileMomentRoutes(ctx, memoryID)
	})
}

func (s *Store) reconcileMomentRoutes(ctx context.Context, memoryID string) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM moment_route_segments WHERE memory_session_id=$1`, memoryID); err != nil {
		return err
	}
	moments, err := s.PhotoMoments(ctx, memoryID)
	if err != nil || len(moments) == 0 || s.routes == nil {
		return err
	}
	// A retired graph version must never be reconstructed using today's checkpoint IDs.
	m, err := s.MemorySession(ctx, memoryID)
	if err != nil {
		return err
	}
	if m.CampusGraphVersion != s.routes.CampusGraphVersion ||
		moments[0].CampusGraphVersion != m.CampusGraphVersion {
		return nil
	}
	write := func(from PhotoMoment, toID *string, to route.Endpoint) error {
		if from.CampusGraphVersion != s.routes.CampusGraphVersion {
			return nil
		}
		if _, ok := s.routes.NodeIndex(from.CheckpointID); !ok {
			return nil
		}
		if _, ok := s.routes.NodeIndex(to.Checkpoint); !ok {
			return nil
		}
		result := s.routes.Connect(&route.Endpoint{Checkpoint: from.CheckpointID, CapturedAt: from.CapturedAt}, &to)
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
		pair := from.ID
		if toID != nil {
			pair += "\x00" + *toID
		}
		key := sha256.Sum256([]byte(memoryID + "\x00" + pair))
		_, err = s.q.ExecContext(ctx, `INSERT INTO moment_route_segments
 (id,memory_session_id,from_photo_moment_id,to_photo_moment_id,coordinate_version,campus_graph_version,
 three_d_map_version,algorithm_version,confidence,path,path_node_ids,length_m)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, "route-"+hex.EncodeToString(key[:16]), memoryID, from.ID,
			toID, s.routes.CoordinateVersion, s.routes.CampusGraphVersion, s.routes.ThreeDMapVersion,
			route.CheckpointAlgorithmVersion, result.Confidence, path, pathNodeIDs, result.LengthM)
		return err
	}
	for i := 1; i < len(moments); i++ {
		previous, next := moments[i-1], moments[i]
		if next.CampusGraphVersion != s.routes.CampusGraphVersion {
			continue
		}
		if err := write(previous, &next.ID, route.Endpoint{Checkpoint: next.CheckpointID, CapturedAt: next.CapturedAt}); err != nil {
			return err
		}
	}
	var checkpointID string
	var arrivedAt time.Time
	err = s.q.QueryRowContext(ctx, `SELECT checkpoint_id,arrived_at FROM arrival_events WHERE memory_session_id=$1`, memoryID).Scan(&checkpointID, &arrivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	last := moments[len(moments)-1]
	if last.CapturedAt.After(arrivedAt) {
		return nil
	}
	return write(last, nil, route.Endpoint{Checkpoint: checkpointID, CapturedAt: arrivedAt})
}
