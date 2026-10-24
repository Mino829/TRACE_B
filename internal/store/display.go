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
	Pins         []MarkerPoint        `json:"pins"`
	Hotspots     []MarkerPoint        `json:"hotspots"`
}
type DisplayArrival struct {
	ArrivalID    string       `json:"arrivalId"`
	ReplayStatus string       `json:"replayStatus"`
	DisplayAlias DisplayAlias `json:"displayAlias"`
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

// Durable arrival and marker state share a cursor cut. Positions are volatile
// and copied separately; a following poll catches any concurrent change.
func (s *Store) DisplaySnapshot(ctx context.Context, now time.Time) (DisplayState, error) {
	state := DisplayState{Participants: []DisplayParticipant{}, Arrivals: []DisplayArrival{}, Pins: []MarkerPoint{}, Hotspots: []MarkerPoint{}}
	policyMismatch := false
	err := s.Transaction(ctx, "trace:markers", func(tx *Store) error {
		// Reconcile before locking display_cursor: marker -> cursor everywhere.
		if err := tx.reconcileMarkers(ctx, now); err != nil {
			return err
		}
		control, err := tx.markerControl(ctx)
		if err != nil {
			return err
		}
		_, policyErr := tx.validateMarkerControl(control)
		policyMismatch = errors.Is(policyErr, ErrConflict)
		if err := tx.q.QueryRowContext(ctx, `SELECT last_id FROM display_cursor WHERE singleton=TRUE FOR UPDATE`).Scan(&state.Cursor); err != nil {
			return err
		}
		rows, err := tx.q.QueryContext(ctx, `SELECT a.id,a.replay_status,e.display_color,e.display_symbol FROM arrival_events a
 JOIN memory_sessions m ON m.id=a.memory_session_id JOIN experiences e ON e.memory_session_id=m.id
 WHERE a.replay_status IN ('queued','playing') AND m.deleting=FALSE AND m.expires_at>$1 ORDER BY a.arrived_at,a.id`, now)
		if err != nil {
			return err
		}
		for rows.Next() {
			var a DisplayArrival
			err = rows.Scan(&a.ArrivalID, &a.ReplayStatus, &a.DisplayAlias.Color, &a.DisplayAlias.Symbol)
			if err != nil {
				break
			}
			state.Arrivals = append(state.Arrivals, a)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil || !tx.displayEnabled {
			return err
		}
		rows, err = tx.q.QueryContext(ctx, `SELECT kind,public_id,x,z FROM marker_visible ORDER BY kind,public_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p MarkerPoint
			if err = rows.Scan(&p.Kind, &p.ID, &p.X, &p.Z); err != nil {
				break
			}
			if p.Kind == "pin" {
				state.Pins = append(state.Pins, p)
			} else {
				state.Hotspots = append(state.Hotspots, p)
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		return err
	})
	if err == nil && policyMismatch {
		return DisplayState{}, ErrConflict
	}
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

var ErrDisplayResync = errors.New("display event cursor expired; resync required")

func (s *Store) DisplayEvents(ctx context.Context, cursor int64, limit int) ([]DisplayEvent, error) {
	if cursor < 0 || limit < 1 || limit > 256 {
		return nil, ErrConflict
	}
	var last int64
	if err := s.q.QueryRowContext(ctx, `SELECT last_id FROM display_cursor WHERE singleton=TRUE`).Scan(&last); err != nil {
		return nil, err
	}
	if cursor > last {
		return nil, ErrDisplayResync
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
	if err := s.ExpireArrivals(ctx, now); err != nil {
		return err
	}
	return s.Transaction(ctx, "trace:markers", func(tx *Store) error { return tx.reconcileMarkers(ctx, now) })
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
