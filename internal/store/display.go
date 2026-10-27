package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

// Display events have one PostgreSQL-serialized cursor. Payloads are strictly
// projected data: never store participant credentials, raw GPS or photo URLs.
type DisplayEvent struct {
	ID         int64           `json:"-"`
	Type       string          `json:"type"`
	OccurredAt time.Time       `json:"occurredAt"`
	Data       json.RawMessage `json:"data"`
}
type DisplayParticipant struct {
	LivePresenceID string       `json:"livePresenceId"`
	X              float64      `json:"x"`
	Z              float64      `json:"z"`
	AccuracyM      float64      `json:"accuracyM"`
	DisplayAlias   DisplayAlias `json:"displayAlias"`
	PresencePhase  string       `json:"presencePhase"`
	LastUpdatedAt  time.Time    `json:"lastUpdatedAt"`
	ExpiresAt      time.Time    `json:"expiresAt"`
}
type DisplayAlias struct {
	Color  string `json:"color"`
	Symbol string `json:"symbol"`
}
type DisplayState struct {
	Cursor       int64                `json:"cursor"`
	Participants []DisplayParticipant `json:"participants"`
	Arrivals     []DisplayArrival     `json:"arrivals"`
}
type DisplayArrival struct {
	ArrivalID             string                   `json:"arrivalId"`
	ReplayStatus          string                   `json:"replayStatus"`
	DisplayAlias          DisplayAlias             `json:"displayAlias"`
	PathNodeIDs           []string                 `json:"pathNodeIds"`
	PhotoCheckpoints      []DisplayPhotoCheckpoint `json:"photoCheckpoints"`
	ReplayDurationSeconds int                      `json:"replayDurationSeconds"`
}
type DisplayPhotoCheckpoint struct {
	CheckpointID string    `json:"checkpointId"`
	CapturedAt   time.Time `json:"capturedAt"`
}

// AppendDisplayEvent MUST run inside the mutation's transaction for atomic fan-out.
// The locked singleton row prevents later event IDs committing ahead of earlier IDs.
func (s *Store) AppendDisplayEvent(ctx context.Context, kind string, data any, now time.Time) (int64, error) {
	if !s.displayEnabled {
		return 0, nil
	}
	if !s.inTx {
		var id int64
		err := s.Transaction(ctx, "display:event", func(tx *Store) error { var e error; id, e = tx.AppendDisplayEvent(ctx, kind, data, now); return e })
		return id, err
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.q.QueryRowContext(ctx, `UPDATE display_cursor SET last_id=last_id+1 WHERE singleton=TRUE RETURNING last_id`).Scan(&id)
	if err != nil {
		return 0, err
	}
	_, err = s.q.ExecContext(ctx, `INSERT INTO display_events(id,kind,data,occurred_at) VALUES ($1,$2,$3,$4)`, id, kind, payload, now)
	return id, err
}

// Durable arrival state and its cursor share a transaction cut. Positions are
// volatile and are copied separately; a following poll catches any concurrent change.
func (s *Store) DisplaySnapshot(ctx context.Context, now time.Time) (DisplayState, error) {
	state := DisplayState{Participants: []DisplayParticipant{}, Arrivals: []DisplayArrival{}}
	err := s.Transaction(ctx, "display:snapshot", func(tx *Store) error {
		if err := tx.q.QueryRowContext(ctx, `SELECT last_id FROM display_cursor WHERE singleton=TRUE FOR UPDATE`).Scan(&state.Cursor); err != nil {
			return err
		}
		rows, err := tx.q.QueryContext(ctx, `SELECT a.id,a.replay_status,e.display_color,e.display_symbol,
 a.memory_session_id,m.photo_mode,COALESCE(m.campus_graph_version,'') FROM arrival_events a
 JOIN memory_sessions m ON m.id=a.memory_session_id JOIN experiences e ON e.memory_session_id=m.id
 WHERE a.replay_status IN ('queued','playing') AND m.deleting=FALSE AND m.expires_at>$1 ORDER BY a.arrived_at,a.id`, now)
		if err != nil {
			return err
		}
		type arrivalRow struct {
			arrival                      DisplayArrival
			memoryID, photoMode, version string
		}
		var arrivals []arrivalRow
		for rows.Next() {
			var a arrivalRow
			err = rows.Scan(&a.arrival.ArrivalID, &a.arrival.ReplayStatus,
				&a.arrival.DisplayAlias.Color, &a.arrival.DisplayAlias.Symbol, &a.memoryID, &a.photoMode, &a.version)
			if err != nil {
				break
			}
			arrivals = append(arrivals, a)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, a := range arrivals {
			projected, err := tx.projectDisplayArrival(ctx, a.arrival, a.memoryID, a.photoMode, a.version)
			if err != nil {
				return err
			}
			state.Arrivals = append(state.Arrivals, projected)
		}
		return nil
	})
	if err == nil {
		for _, p := range s.DisplayPositions(now) {
			state.Participants = append(state.Participants, p)
		}
		sort.Slice(state.Participants, func(i, j int) bool {
			return state.Participants[i].LivePresenceID < state.Participants[j].LivePresenceID
		})
	}
	return state, err
}

// The renderer receives only ordered checkpoint/time pairs and graph node IDs.
// A missing or retired segment never becomes a partial or reinterpreted route.
func (s *Store) projectDisplayArrival(ctx context.Context, a DisplayArrival, memoryID, photoMode, graphVersion string) (DisplayArrival, error) {
	a.PathNodeIDs = []string{}
	a.PhotoCheckpoints = []DisplayPhotoCheckpoint{}
	a.ReplayDurationSeconds = 15
	if photoMode != PhotoModeDevice || s.routes == nil || graphVersion != s.routes.CampusGraphVersion {
		return a, nil
	}
	moments, err := s.PhotoMoments(ctx, memoryID)
	if err != nil {
		return a, err
	}
	for _, p := range moments {
		if p.CampusGraphVersion != graphVersion {
			a.PhotoCheckpoints = []DisplayPhotoCheckpoint{}
			return a, nil
		}
		if _, ok := s.routes.CheckpointNode(p.CheckpointID); !ok {
			a.PhotoCheckpoints = []DisplayPhotoCheckpoint{}
			return a, nil
		}
		a.PhotoCheckpoints = append(a.PhotoCheckpoints, DisplayPhotoCheckpoint{CheckpointID: p.CheckpointID, CapturedAt: p.CapturedAt})
	}
	if len(moments) == 0 {
		return a, nil
	}
	segments, err := s.MomentRouteSegments(ctx, memoryID)
	if err != nil {
		return a, err
	}
	// One segment per photo: adjacent photos, then the final photo to arrival.
	if len(segments) != len(moments) {
		return a, nil
	}
	for i, segment := range segments {
		if segment.FromPhotoMomentID != moments[i].ID ||
			segment.CoordinateVersion != s.routes.CoordinateVersion ||
			segment.CampusGraphVersion != graphVersion ||
			segment.ThreeDMapVersion != s.routes.ThreeDMapVersion ||
			len(segment.PathNodeIDs) == 0 ||
			(i < len(moments)-1 && (segment.ToPhotoMomentID == nil || *segment.ToPhotoMomentID != moments[i+1].ID)) ||
			(i == len(moments)-1 && segment.ToPhotoMomentID != nil) {
			a.PathNodeIDs = []string{}
			return a, nil
		}
		if i > 0 {
			if a.PathNodeIDs[len(a.PathNodeIDs)-1] != segment.PathNodeIDs[0] {
				a.PathNodeIDs = []string{}
				return a, nil
			}
			a.PathNodeIDs = append(a.PathNodeIDs, segment.PathNodeIDs[1:]...)
		} else {
			a.PathNodeIDs = append(a.PathNodeIDs, segment.PathNodeIDs...)
		}
	}
	return a, nil
}

// Photo mutations after arrival replace the whole projection: point/time pairs
// alone cannot identify one deleted photo when capture timestamps coincide.
func (s *Store) appendDisplayPhotoChange(ctx context.Context, memoryID, kind string, now time.Time) error {
	if !s.displayEnabled {
		return nil
	}
	var a DisplayArrival
	var photoMode, graphVersion string
	err := s.q.QueryRowContext(ctx, `SELECT a.id,a.replay_status,e.display_color,e.display_symbol,m.photo_mode,COALESCE(m.campus_graph_version,'')
 FROM arrival_events a JOIN memory_sessions m ON m.id=a.memory_session_id
 JOIN experiences e ON e.memory_session_id=m.id
 WHERE a.memory_session_id=$1 AND a.replay_status IN ('queued','playing') AND m.deleting=FALSE AND m.expires_at>$2`,
		memoryID, now).Scan(&a.ArrivalID, &a.ReplayStatus, &a.DisplayAlias.Color, &a.DisplayAlias.Symbol, &photoMode, &graphVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	a, err = s.projectDisplayArrival(ctx, a, memoryID, photoMode, graphVersion)
	if err != nil {
		return err
	}
	_, err = s.AppendDisplayEvent(ctx, kind, a, now)
	return err
}

var ErrDisplayResync = errors.New("display event cursor expired; resync required")

func (s *Store) DisplayEvents(ctx context.Context, cursor int64, limit int) ([]DisplayEvent, error) {
	if limit < 1 || limit > 256 {
		return nil, ErrConflict
	}
	var last int64
	if err := s.q.QueryRowContext(ctx, `SELECT last_id FROM display_cursor WHERE singleton=TRUE`).Scan(&last); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `SELECT id,kind,data,occurred_at FROM display_events WHERE id>$1 ORDER BY id LIMIT $2`, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []DisplayEvent
	previous := cursor
	for rows.Next() {
		var e DisplayEvent
		if err := rows.Scan(&e.ID, &e.Type, &e.Data, &e.OccurredAt); err != nil {
			return nil, err
		}
		if e.ID != previous+1 {
			return nil, ErrDisplayResync
		}
		events = append(events, e)
		previous = e.ID
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(events) == 0 && cursor < last {
		return nil, ErrDisplayResync
	}
	return events, nil
}

// DisplayPositions returns only the current quantized, display-scoped projection.
// No coordinate or position event is written to PostgreSQL.
func (s *Store) DisplayPositions(now time.Time) map[string]DisplayParticipant {
	s.positions.mu.RLock()
	defer s.positions.mu.RUnlock()
	points := make(map[string]DisplayParticipant, len(s.positions.projected))
	for _, p := range s.positions.projected {
		if now.Before(p.ExpiresAt) {
			points[p.LivePresenceID] = p
		}
	}
	return points
}

// Arrival and lease state are durable. Position expiry is observed by WS polls
// and a fresh snapshot never exposes an expired cached value.
func (s *Store) RefreshDisplay(ctx context.Context, now time.Time) error {
	if err := s.Transaction(ctx, "display:lease", func(tx *Store) error { return tx.expireDisplayLease(ctx, now) }); err != nil {
		return err
	}
	return s.ExpireArrivals(ctx, now)
}

// Lease is fenced by an unpredictable ID and holder connection ID, in SQL.
func (s *Store) AcquireDisplayLease(ctx context.Context, holder, lease string, now time.Time) (bool, error) {
	acquired := false
	err := s.Transaction(ctx, "display:lease", func(tx *Store) error {
		if err := tx.expireDisplayLease(ctx, now); err != nil {
			return err
		}
		result, err := tx.q.ExecContext(ctx, `UPDATE display_primary_lease SET holder_id=$1,lease_id=$2,expires_at=$3 WHERE singleton=TRUE AND holder_id IS NULL`, holder, lease, now.Add(30*time.Second))
		if err == nil {
			n, _ := result.RowsAffected()
			acquired = n == 1
		}
		return err
	})
	return acquired, err
}
func (s *Store) RenewDisplayLease(ctx context.Context, holder, lease string, now time.Time) (bool, error) {
	result, err := s.q.ExecContext(ctx, `UPDATE display_primary_lease SET expires_at=$3 WHERE singleton=TRUE AND holder_id=$1 AND lease_id=$2 AND expires_at>$4`, holder, lease, now.Add(30*time.Second), now)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n == 1, nil
}
func (s *Store) ReleaseDisplayLease(ctx context.Context, holder, lease string, now time.Time) error {
	return s.Transaction(ctx, "display:lease", func(tx *Store) error {
		var old string
		err := tx.q.QueryRowContext(ctx, `SELECT lease_id FROM display_primary_lease WHERE singleton=TRUE AND holder_id=$1 AND lease_id=$2 FOR UPDATE`, holder, lease).Scan(&old)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return tx.clearDisplayLease(ctx, old, now)
	})
}
func (s *Store) expireDisplayLease(ctx context.Context, now time.Time) error {
	var lease sql.NullString
	err := s.q.QueryRowContext(ctx, `SELECT lease_id FROM display_primary_lease WHERE singleton=TRUE AND expires_at<=$1 FOR UPDATE`, now).Scan(&lease)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if lease.Valid {
		return s.clearDisplayLease(ctx, lease.String, now)
	}
	return nil
}
func (s *Store) clearDisplayLease(ctx context.Context, lease string, now time.Time) error {
	rows, err := s.q.QueryContext(ctx, `UPDATE arrival_events SET replay_status='skipped',skip_reason='renderer_failure',replay_finished_at=$2
 WHERE replay_status='playing' AND renderer_lease_id=$1 RETURNING id`, lease, now)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = s.AppendDisplayEvent(ctx, "arrival.replay.skipped", map[string]string{"arrivalId": id, "reason": "renderer_failure"}, now); err != nil {
			return err
		}
	}
	_, err = s.q.ExecContext(ctx, `UPDATE display_primary_lease SET holder_id=NULL,lease_id=NULL,expires_at=NULL WHERE singleton=TRUE`)
	return err
}

func (s *Store) DisplayReplay(ctx context.Context, holder, lease, eventID, arrivalID, action string, now time.Time) (string, error) {
	var status string
	err := s.Transaction(ctx, "display:lease", func(tx *Store) error {
		var savedLease, savedAction, savedArrival string
		err := tx.q.QueryRowContext(ctx, `SELECT lease_id,kind,arrival_id,result FROM display_renderer_receipts WHERE event_id=$1`, eventID).Scan(&savedLease, &savedAction, &savedArrival, &status)
		if err == nil {
			if savedLease != lease || savedAction != action || savedArrival != arrivalID {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var current string
		err = tx.q.QueryRowContext(ctx, `SELECT lease_id FROM display_primary_lease WHERE singleton=TRUE AND holder_id=$1 AND lease_id=$2 AND expires_at>$3 FOR UPDATE`, holder, lease, now).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		previous, next := "queued", "playing"
		switch action {
		case "arrival.replay.started":
		case "arrival.replay.completed":
			previous, next = "playing", "completed"
		case "arrival.replay.failed":
			previous, next = "playing", "skipped"
		default:
			return ErrConflict
		}
		query := `UPDATE arrival_events SET replay_status=$2,renderer_lease_id=$3,
 replay_started_at=CASE WHEN $2='playing' THEN $4 ELSE replay_started_at END,
 replay_finished_at=CASE WHEN $2<>'playing' THEN $4 ELSE replay_finished_at END,
 skip_reason=CASE WHEN $2='skipped' THEN 'renderer_failure' ELSE skip_reason END
 WHERE id=$1 AND replay_status=$5 AND (renderer_lease_id IS NULL OR renderer_lease_id=$3)
 AND ($2<>'playing' OR (replay_deadline>$4 AND (photo_wait_until<=$4 OR NOT EXISTS
 (SELECT 1 FROM photos p WHERE p.memory_session_id=arrival_events.memory_session_id AND p.status='pending'))))`
		result, err := tx.q.ExecContext(ctx, query, arrivalID, next, lease, now, previous)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return ErrConflict
		}
		status = next
		_, err = tx.AppendDisplayEvent(ctx, action, map[string]string{"arrivalId": arrivalID, "replayStatus": next}, now)
		if err != nil {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `INSERT INTO display_renderer_receipts(event_id,lease_id,kind,arrival_id,result,created_at) VALUES ($1,$2,$3,$4,$5,$6)`, eventID, lease, action, arrivalID, status, now)
		return err
	})
	return status, err
}
