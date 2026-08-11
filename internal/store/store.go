package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type Store struct {
	db *sql.DB
}

type NewExperience struct {
	ExperienceID    string
	MemorySessionID string
	LiveSessionID   string
	LinkID          string
	MemoryTokenHash string
	LiveTokenHash   string
	LinkTokenHash   string
	MemoryConsent   string
	LiveConsent     string
	ConsentLanguage string
	DisplayColor    string
	DisplaySymbol   string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	DeleteAfter     time.Time
	LivePresence    bool
}

type ExperienceState struct {
	ExperienceID    string
	MemorySessionID string
	LiveSessionID   string
	TrackingStatus  string
	PresencePhase   string
	DisplayColor    string
	DisplaySymbol   string
	ExpiresAt       time.Time
	DeleteAfter     time.Time
}

type LiveSession struct {
	ID             string
	TrackingStatus string
	PresencePhase  string
	DisplayColor   string
	DisplaySymbol  string
	ExpiresAt      time.Time
}

type Position struct {
	X          float64
	Z          float64
	AccuracyM  float64
	RecordedAt time.Time
	ReceivedAt time.Time
	ExpiresAt  time.Time
}

type IdempotencyRecord struct {
	RequestHash    string
	ResponseStatus int
	ResponseBody   []byte
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;`); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS experiences (
  id TEXT PRIMARY KEY,
  memory_session_id TEXT NOT NULL UNIQUE,
  live_session_id TEXT UNIQUE,
  display_color TEXT NOT NULL,
  display_symbol TEXT NOT NULL,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  delete_after TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS memory_sessions (
  id TEXT PRIMARY KEY,
  consent_version TEXT NOT NULL,
  consent_language TEXT NOT NULL,
  token_hash TEXT NOT NULL,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  delete_after TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS live_presence_sessions (
  id TEXT PRIMARY KEY,
  consent_version TEXT NOT NULL,
  on_site_consent_version TEXT,
  token_hash TEXT NOT NULL,
  tracking_status TEXT NOT NULL CHECK (tracking_status IN ('tracking','stopped','expired')),
  presence_phase TEXT NOT NULL CHECK (presence_phase IN ('approaching','arrived','on_site','ended')),
  stop_reason TEXT,
  display_color TEXT NOT NULL,
  display_symbol TEXT NOT NULL,
  created_at TEXT NOT NULL,
  stopped_at TEXT,
  expires_at TEXT NOT NULL,
  delete_after TEXT NOT NULL,
  last_seen_at TEXT
);
CREATE TABLE IF NOT EXISTS experience_links (
  id TEXT PRIMARY KEY,
  memory_session_id TEXT NOT NULL UNIQUE REFERENCES memory_sessions(id) ON DELETE CASCADE,
  live_session_id TEXT NOT NULL UNIQUE REFERENCES live_presence_sessions(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL,
  expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS live_positions (
  live_session_id TEXT PRIMARY KEY REFERENCES live_presence_sessions(id) ON DELETE CASCADE,
  accuracy_m REAL NOT NULL,
  local_x REAL NOT NULL,
  local_z REAL NOT NULL,
  recorded_at TEXT NOT NULL,
  received_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS idempotency_records (
  scope TEXT NOT NULL,
  key_hash TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  response_status INTEGER NOT NULL,
  response_body BLOB NOT NULL,
  expires_at TEXT NOT NULL,
  PRIMARY KEY (scope, key_hash)
);
CREATE INDEX IF NOT EXISTS idx_memory_delete_after ON memory_sessions(delete_after);
CREATE INDEX IF NOT EXISTS idx_live_expiry ON live_presence_sessions(tracking_status, expires_at);
CREATE INDEX IF NOT EXISTS idx_live_delete_after ON live_presence_sessions(delete_after);
CREATE INDEX IF NOT EXISTS idx_position_expiry ON live_positions(expires_at);
CREATE INDEX IF NOT EXISTS idx_idempotency_expiry ON idempotency_records(expires_at);
`
	_, err := db.Exec(schema)
	return err
}

func (s *Store) CreateExperience(ctx context.Context, value NewExperience) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	created := value.CreatedAt.UTC().Format(time.RFC3339Nano)
	expires := value.ExpiresAt.UTC().Format(time.RFC3339Nano)
	deletes := value.DeleteAfter.UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT INTO memory_sessions
    (id, consent_version, consent_language, token_hash, created_at, expires_at, delete_after)
    VALUES (?, ?, ?, ?, ?, ?, ?)`, value.MemorySessionID, value.MemoryConsent, value.ConsentLanguage,
		value.MemoryTokenHash, created, expires, deletes); err != nil {
		return err
	}
	var liveID any
	if value.LivePresence {
		liveID = value.LiveSessionID
		if _, err = tx.ExecContext(ctx, `INSERT INTO live_presence_sessions
      (id, consent_version, token_hash, tracking_status, presence_phase, display_color, display_symbol,
       created_at, expires_at, delete_after)
      VALUES (?, ?, ?, 'tracking', 'approaching', ?, ?, ?, ?, ?)`, value.LiveSessionID,
			value.LiveConsent, value.LiveTokenHash, value.DisplayColor, value.DisplaySymbol, created, expires, deletes); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO experience_links
      (id, memory_session_id, live_session_id, token_hash, expires_at) VALUES (?, ?, ?, ?, ?)`,
			value.LinkID, value.MemorySessionID, value.LiveSessionID, value.LinkTokenHash, expires); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO experiences
    (id, memory_session_id, live_session_id, display_color, display_symbol, created_at, expires_at, delete_after)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, value.ExperienceID, value.MemorySessionID, liveID,
		value.DisplayColor, value.DisplaySymbol, created, expires, deletes); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ExperienceByMemoryToken(ctx context.Context, id, tokenHash string, now time.Time) (ExperienceState, error) {
	var state ExperienceState
	var liveID sql.NullString
	var storedHash string
	var tracking, phase sql.NullString
	var expires, deletes string
	err := s.db.QueryRowContext(ctx, `SELECT e.id, e.memory_session_id, e.live_session_id,
    m.token_hash, COALESCE(lp.tracking_status, ''), COALESCE(lp.presence_phase, ''),
    e.display_color, e.display_symbol, e.expires_at, e.delete_after
    FROM experiences e
    JOIN memory_sessions m ON m.id = e.memory_session_id
    LEFT JOIN live_presence_sessions lp ON lp.id = e.live_session_id
    WHERE e.id = ?`, id).Scan(&state.ExperienceID, &state.MemorySessionID, &liveID, &storedHash,
		&tracking, &phase, &state.DisplayColor, &state.DisplaySymbol, &expires, &deletes)
	if errors.Is(err, sql.ErrNoRows) {
		return ExperienceState{}, ErrNotFound
	}
	if err != nil {
		return ExperienceState{}, err
	}
	if !secureEqual(storedHash, tokenHash) {
		return ExperienceState{}, ErrNotFound
	}
	state.LiveSessionID = liveID.String
	state.TrackingStatus = tracking.String
	state.PresencePhase = phase.String
	state.ExpiresAt, err = parseTime(expires)
	if err != nil {
		return ExperienceState{}, err
	}
	state.DeleteAfter, err = parseTime(deletes)
	if err != nil {
		return ExperienceState{}, err
	}
	if now.After(state.DeleteAfter) {
		return ExperienceState{}, ErrNotFound
	}
	return state, nil
}

func (s *Store) AuthenticateMemory(ctx context.Context, id, tokenHash string, now time.Time) error {
	return s.authenticate(ctx, `SELECT token_hash, delete_after FROM memory_sessions WHERE id = ?`, id, tokenHash, now)
}

func (s *Store) AuthenticateLive(ctx context.Context, id, tokenHash string, now time.Time) error {
	return s.authenticate(ctx, `SELECT token_hash, delete_after FROM live_presence_sessions WHERE id = ?`, id, tokenHash, now)
}

func (s *Store) authenticate(ctx context.Context, query, id, tokenHash string, now time.Time) error {
	var storedHash, deleteAfter string
	err := s.db.QueryRowContext(ctx, query, id).Scan(&storedHash, &deleteAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	deadline, err := parseTime(deleteAfter)
	if err != nil {
		return err
	}
	if now.After(deadline) || !secureEqual(storedHash, tokenHash) {
		return ErrNotFound
	}
	return nil
}

func (s *Store) LiveSession(ctx context.Context, id string) (LiveSession, *Position, error) {
	var session LiveSession
	var expires string
	err := s.db.QueryRowContext(ctx, `SELECT id, tracking_status, presence_phase, display_color,
    display_symbol, expires_at FROM live_presence_sessions WHERE id = ?`, id).Scan(&session.ID,
		&session.TrackingStatus, &session.PresencePhase, &session.DisplayColor, &session.DisplaySymbol, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return LiveSession{}, nil, ErrNotFound
	}
	if err != nil {
		return LiveSession{}, nil, err
	}
	session.ExpiresAt, err = parseTime(expires)
	if err != nil {
		return LiveSession{}, nil, err
	}
	var position Position
	var recorded, received, positionExpires string
	err = s.db.QueryRowContext(ctx, `SELECT local_x, local_z, accuracy_m, recorded_at, received_at,
    expires_at FROM live_positions WHERE live_session_id = ?`, id).Scan(&position.X, &position.Z,
		&position.AccuracyM, &recorded, &received, &positionExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return session, nil, nil
	}
	if err != nil {
		return LiveSession{}, nil, err
	}
	position.RecordedAt, err = parseTime(recorded)
	if err != nil {
		return LiveSession{}, nil, err
	}
	position.ReceivedAt, err = parseTime(received)
	if err != nil {
		return LiveSession{}, nil, err
	}
	position.ExpiresAt, err = parseTime(positionExpires)
	if err != nil {
		return LiveSession{}, nil, err
	}
	return session, &position, nil
}

func (s *Store) SavePosition(ctx context.Context, liveID string, position Position) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO live_positions
    (live_session_id, accuracy_m, local_x, local_z, recorded_at, received_at, expires_at)
    VALUES (?, ?, ?, ?, ?, ?, ?)
    ON CONFLICT(live_session_id) DO UPDATE SET accuracy_m=excluded.accuracy_m,
      local_x=excluded.local_x, local_z=excluded.local_z, recorded_at=excluded.recorded_at,
      received_at=excluded.received_at, expires_at=excluded.expires_at`, liveID, position.AccuracyM,
		position.X, position.Z, formatTime(position.RecordedAt), formatTime(position.ReceivedAt), formatTime(position.ExpiresAt))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE live_presence_sessions SET last_seen_at=? WHERE id=?`,
		formatTime(position.ReceivedAt), liveID)
	return err
}

func (s *Store) StopLive(ctx context.Context, id, reason string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE live_presence_sessions SET tracking_status='stopped',
    presence_phase='ended', stop_reason=?, stopped_at=? WHERE id=? AND tracking_status='tracking'`,
		reason, formatTime(now), id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		var status string
		if err := s.db.QueryRowContext(ctx, `SELECT tracking_status FROM live_presence_sessions WHERE id=?`, id).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM live_positions WHERE live_session_id=?`, id)
	return err
}

func (s *Store) SetOnSite(ctx context.Context, id, consentVersion string, continuing bool, now time.Time) error {
	if !continuing {
		return s.StopLive(ctx, id, "on_site_declined", now)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE live_presence_sessions SET presence_phase='on_site',
    on_site_consent_version=? WHERE id=? AND tracking_status='tracking' AND presence_phase IN ('arrived','approaching')`,
		consentVersion, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrConflict
	}
	return nil
}

func (s *Store) DeleteMemory(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM experience_links WHERE memory_session_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE experiences SET memory_session_id='deleted:' || id WHERE memory_session_id=?`, id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM memory_sessions WHERE id=?`, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) DeleteLive(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM experience_links WHERE live_session_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE experiences SET live_session_id=NULL WHERE live_session_id=?`, id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM live_presence_sessions WHERE id=?`, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) GetIdempotency(ctx context.Context, scope, keyHash string, now time.Time) (IdempotencyRecord, error) {
	var record IdempotencyRecord
	var expires string
	err := s.db.QueryRowContext(ctx, `SELECT request_hash, response_status, response_body, expires_at
    FROM idempotency_records WHERE scope=? AND key_hash=?`, scope, keyHash).Scan(&record.RequestHash,
		&record.ResponseStatus, &record.ResponseBody, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return IdempotencyRecord{}, ErrNotFound
	}
	if err != nil {
		return IdempotencyRecord{}, err
	}
	deadline, err := parseTime(expires)
	if err != nil {
		return IdempotencyRecord{}, err
	}
	if now.After(deadline) {
		return IdempotencyRecord{}, ErrNotFound
	}
	return record, nil
}

func (s *Store) SaveIdempotency(ctx context.Context, scope, keyHash, requestHash string, status int, body []byte, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO idempotency_records
    (scope, key_hash, request_hash, response_status, response_body, expires_at) VALUES (?, ?, ?, ?, ?, ?)
    ON CONFLICT(scope, key_hash) DO NOTHING`, scope, keyHash, requestHash, status, body, formatTime(expires))
	return err
}

func (s *Store) Cleanup(ctx context.Context, now time.Time) error {
	nowText := formatTime(now)
	statements := []string{
		`DELETE FROM live_positions WHERE expires_at <= ?`,
		`UPDATE live_presence_sessions SET tracking_status='expired', presence_phase='ended' WHERE expires_at <= ? AND tracking_status='tracking'`,
		`DELETE FROM idempotency_records WHERE expires_at <= ?`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement, nowText); err != nil {
			return err
		}
	}
	return nil
}

func secureEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored time: %w", err)
	}
	return parsed, nil
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
