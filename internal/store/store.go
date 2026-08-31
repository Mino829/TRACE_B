package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type Store struct {
	db   *sql.DB
	q    executor
	inTx bool
}
type NewExperience struct {
	ExperienceID, MemorySessionID, LiveSessionID, LinkID string
	MemoryTokenHash, LiveTokenHash, LinkTokenHash        string
	MemoryConsent, LiveConsent, ConsentLanguage          string
	DisplayColor, DisplaySymbol                          string
	CreatedAt, ExpiresAt, DeleteAfter                    time.Time
	LivePresence                                         bool
}
type ExperienceState struct {
	ExperienceID, MemorySessionID, LiveSessionID               string
	TrackingStatus, PresencePhase, DisplayColor, DisplaySymbol string
	ExpiresAt, DeleteAfter                                     time.Time
}
type LiveSession struct {
	ID, TrackingStatus, PresencePhase, DisplayColor, DisplaySymbol string
	ExpiresAt                                                      time.Time
}
type Position struct {
	X, Z, AccuracyM                   float64
	RecordedAt, ReceivedAt, ExpiresAt time.Time
}
type IdempotencyRecord struct {
	RequestHash    string
	ResponseStatus int
	ResponseBody   []byte
}

// Open connects only. Schema changes are an explicit deployment step.
// Errors omit the DSN, which can contain credentials.
func Open(ctx context.Context, dsn string, maxConnections int) (*Store, error) {
	if dsn == "" || maxConnections < 1 || maxConnections > 100 {
		return nil, errors.New("database URL and connection limit (1..100) are required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection configuration")
	}
	cfg.ConnectTimeout = 5 * time.Second
	cfg.RuntimeParams["timezone"] = "UTC"
	cfg.RuntimeParams["statement_timeout"] = "10000"
	cfg.RuntimeParams["lock_timeout"] = "5000"
	cfg.RuntimeParams["idle_in_transaction_session_timeout"] = "15000"
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(maxConnections)
	db.SetMaxIdleConns(maxConnections)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, errors.New("PostgreSQL connection failed; check network and credentials")
	}
	return &Store{db: db, q: db}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error {
	var version int
	return s.q.QueryRowContext(ctx, "SELECT version FROM schema_migrations WHERE version=1").Scan(&version)
}

// Transaction serializes operations on a session across Cloud Run instances.
// The mutation and its idempotency receipt commit together, or not at all.
func (s *Store) Transaction(ctx context.Context, key string, fn func(*Store) error) error {
	if s.inTx {
		return fn(s)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if key != "" {
		digest := sha256.Sum256([]byte(key))
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(digest[:8]))); err != nil {
			return err
		}
	}
	if err := fn(&Store{db: s.db, q: tx, inTx: true}); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) CreateExperience(ctx context.Context, v NewExperience) error {
	return s.Transaction(ctx, "", func(tx *Store) error {
		_, err := tx.q.ExecContext(ctx, `INSERT INTO memory_sessions
  (id,consent_version,consent_language,token_hash,created_at,expires_at,delete_after)
  VALUES ($1,$2,$3,$4,$5,$6,$7)`, v.MemorySessionID, v.MemoryConsent, v.ConsentLanguage, v.MemoryTokenHash, v.CreatedAt, v.ExpiresAt, v.DeleteAfter)
		if err != nil {
			return err
		}
		var liveID any
		if v.LivePresence {
			liveID = v.LiveSessionID
			_, err = tx.q.ExecContext(ctx, `INSERT INTO live_presence_sessions
   (id,consent_version,token_hash,tracking_status,presence_phase,display_color,display_symbol,created_at,expires_at,delete_after)
   VALUES ($1,$2,$3,'tracking','approaching',$4,$5,$6,$7,$8)`, v.LiveSessionID, v.LiveConsent, v.LiveTokenHash, v.DisplayColor, v.DisplaySymbol, v.CreatedAt, v.ExpiresAt, v.DeleteAfter)
			if err != nil {
				return err
			}
			_, err = tx.q.ExecContext(ctx, `INSERT INTO experience_links (id,memory_session_id,live_session_id,token_hash,expires_at)
   VALUES ($1,$2,$3,$4,$5)`, v.LinkID, v.MemorySessionID, v.LiveSessionID, v.LinkTokenHash, v.ExpiresAt)
			if err != nil {
				return err
			}
		}
		_, err = tx.q.ExecContext(ctx, `INSERT INTO experiences
  (id,memory_session_id,live_session_id,display_color,display_symbol,created_at,expires_at,delete_after)
  VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, v.ExperienceID, v.MemorySessionID, liveID, v.DisplayColor, v.DisplaySymbol, v.CreatedAt, v.ExpiresAt, v.DeleteAfter)
		return err
	})
}
func (s *Store) ExperienceByMemoryToken(ctx context.Context, id, token string, now time.Time) (ExperienceState, error) {
	var v ExperienceState
	var stored string
	err := s.q.QueryRowContext(ctx, `SELECT e.id,e.memory_session_id,COALESCE(e.live_session_id,''),m.token_hash,
 COALESCE(lp.tracking_status,''),COALESCE(lp.presence_phase,''),e.display_color,e.display_symbol,e.expires_at,e.delete_after
 FROM experiences e JOIN memory_sessions m ON m.id=e.memory_session_id LEFT JOIN live_presence_sessions lp ON lp.id=e.live_session_id
 WHERE e.id=$1 AND e.delete_after>$2`, id, now).Scan(&v.ExperienceID, &v.MemorySessionID, &v.LiveSessionID, &stored, &v.TrackingStatus, &v.PresencePhase, &v.DisplayColor, &v.DisplaySymbol, &v.ExpiresAt, &v.DeleteAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if !secureEqual(stored, token) {
		return ExperienceState{}, ErrNotFound
	}
	if !now.Before(v.ExpiresAt) && v.TrackingStatus == "tracking" {
		v.TrackingStatus = "expired"
		v.PresencePhase = "ended"
	}
	return v, nil
}
func (s *Store) AuthenticateMemory(ctx context.Context, id, token string, now time.Time) error {
	return s.authenticate(ctx, "SELECT token_hash FROM memory_sessions WHERE id=$1 AND delete_after>$2", id, token, now)
}
func (s *Store) AuthenticateLive(ctx context.Context, id, token string, now time.Time) error {
	return s.authenticate(ctx, "SELECT token_hash FROM live_presence_sessions WHERE id=$1 AND delete_after>$2", id, token, now)
}
func (s *Store) authenticate(ctx context.Context, query, id, token string, now time.Time) error {
	var stored string
	err := s.q.QueryRowContext(ctx, query, id, now).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !secureEqual(stored, token) {
		return ErrNotFound
	}
	return nil
}
func (s *Store) LiveSession(ctx context.Context, id string) (LiveSession, *Position, error) {
	var v LiveSession
	err := s.q.QueryRowContext(ctx, "SELECT id,tracking_status,presence_phase,display_color,display_symbol,expires_at FROM live_presence_sessions WHERE id=$1", id).
		Scan(&v.ID, &v.TrackingStatus, &v.PresencePhase, &v.DisplayColor, &v.DisplaySymbol, &v.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil, ErrNotFound
	}
	if err != nil {
		return v, nil, err
	}
	var p Position
	err = s.q.QueryRowContext(ctx, `SELECT local_x,local_z,accuracy_m,recorded_at,received_at,expires_at FROM live_positions
 WHERE live_session_id=$1 AND expires_at>CURRENT_TIMESTAMP`, id).Scan(&p.X, &p.Z, &p.AccuracyM, &p.RecordedAt, &p.ReceivedAt, &p.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil, nil
	}
	if err != nil {
		return v, nil, err
	}
	return v, &p, nil
}
func (s *Store) SavePosition(ctx context.Context, id string, p Position) error {
	return s.Transaction(ctx, id, func(tx *Store) error {
		var expires time.Time
		var status string
		err := tx.q.QueryRowContext(ctx, "SELECT tracking_status,expires_at FROM live_presence_sessions WHERE id=$1 FOR UPDATE", id).Scan(&status, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != "tracking" || !p.ReceivedAt.Before(expires) {
			return ErrConflict
		}
		if p.ExpiresAt.After(expires) {
			p.ExpiresAt = expires
		}
		result, err := tx.q.ExecContext(ctx, `INSERT INTO live_positions (live_session_id,accuracy_m,local_x,local_z,recorded_at,received_at,expires_at)
  VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(live_session_id) DO UPDATE SET accuracy_m=EXCLUDED.accuracy_m,
  local_x=EXCLUDED.local_x,local_z=EXCLUDED.local_z,recorded_at=EXCLUDED.recorded_at,received_at=EXCLUDED.received_at,expires_at=EXCLUDED.expires_at
  WHERE live_positions.recorded_at<EXCLUDED.recorded_at`, id, p.AccuracyM, p.X, p.Z, p.RecordedAt, p.ReceivedAt, p.ExpiresAt)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return ErrConflict
		}
		_, err = tx.q.ExecContext(ctx, "UPDATE live_presence_sessions SET last_seen_at=$1 WHERE id=$2", p.ReceivedAt, id)
		return err
	})
}
func (s *Store) StopLive(ctx context.Context, id, reason string, now time.Time) error {
	return s.Transaction(ctx, id, func(tx *Store) error {
		result, err := tx.q.ExecContext(ctx, `UPDATE live_presence_sessions SET tracking_status='stopped',presence_phase='ended',
  stop_reason=COALESCE(stop_reason,$1),stopped_at=COALESCE(stopped_at,$2) WHERE id=$3`, reason, now, id)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.q.ExecContext(ctx, "DELETE FROM live_positions WHERE live_session_id=$1", id)
		return err
	})
}
func (s *Store) SetOnSite(ctx context.Context, id, consent string, continuing bool, now time.Time) error {
	if !continuing {
		return s.StopLive(ctx, id, "on_site_declined", now)
	}
	result, err := s.q.ExecContext(ctx, `UPDATE live_presence_sessions SET presence_phase='on_site',on_site_consent_version=$1
 WHERE id=$2 AND tracking_status='tracking' AND expires_at>$3 AND presence_phase IN ('approaching','arrived','on_site')`, consent, id, now)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}
func (s *Store) DeleteMemory(ctx context.Context, id string) error {
	return s.deleteSession(ctx, "DELETE FROM memory_sessions WHERE id=$1", id)
}
func (s *Store) DeleteLive(ctx context.Context, id string) error {
	return s.deleteSession(ctx, "DELETE FROM live_presence_sessions WHERE id=$1", id)
}
func (s *Store) deleteSession(ctx context.Context, query, id string) error {
	result, err := s.q.ExecContext(ctx, query, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
func (s *Store) GetIdempotency(ctx context.Context, scope, key string, now time.Time) (IdempotencyRecord, error) {
	var r IdempotencyRecord
	err := s.q.QueryRowContext(ctx, `SELECT request_hash,response_status,response_body FROM idempotency_records
 WHERE scope=$1 AND key_hash=$2 AND expires_at>$3`, scope, key, now).Scan(&r.RequestHash, &r.ResponseStatus, &r.ResponseBody)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}
func (s *Store) SaveIdempotency(ctx context.Context, scope, key, requestHash string, status int, body []byte, expires time.Time) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO idempotency_records (scope,key_hash,request_hash,response_status,response_body,expires_at)
 VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT(scope,key_hash) DO UPDATE SET request_hash=EXCLUDED.request_hash,
 response_status=EXCLUDED.response_status,response_body=EXCLUDED.response_body,expires_at=EXCLUDED.expires_at
 WHERE idempotency_records.expires_at<=CURRENT_TIMESTAMP`, scope, key, requestHash, status, body, expires)
	return err
}
func (s *Store) Cleanup(ctx context.Context, now time.Time) error {
	return s.Transaction(ctx, "trace:cleanup", func(tx *Store) error {
		for _, query := range []string{
			"UPDATE live_presence_sessions SET tracking_status='expired',presence_phase='ended' WHERE expires_at<=$1 AND tracking_status='tracking'",
			"DELETE FROM live_positions WHERE expires_at<=$1 OR live_session_id IN (SELECT id FROM live_presence_sessions WHERE tracking_status<>'tracking')",
			"DELETE FROM experience_links WHERE expires_at<=$1",
			"DELETE FROM experiences WHERE delete_after<=$1",
			"DELETE FROM memory_sessions WHERE delete_after<=$1",
			"DELETE FROM live_presence_sessions WHERE delete_after<=$1",
			"DELETE FROM idempotency_records WHERE expires_at<=$1",
		} {
			if _, err := tx.q.ExecContext(ctx, query, now); err != nil {
				return err
			}
		}
		return nil
	})
}
func secureEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
