package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type Store struct {
	db        *sql.DB
	q         executor
	inTx      bool
	positions *positionCache
	pending   positionChange
}
type positionCache struct {
	mu     sync.RWMutex
	latest map[string]Position
}
type positionChange struct {
	id        string
	point     Position
	operation positionOperation
}
type positionOperation byte

const (
	noPositionChange positionOperation = iota
	setPosition
	removePosition
)

type NewExperience struct {
	ExperienceID, MemorySessionID, LiveSessionID, LinkID string
	MemoryTokenHash, LiveTokenHash, LinkTokenHash        string
	MemoryConsent, LiveConsent, ConsentLanguage          string
	DisplayColor, DisplaySymbol                          string
	CreatedAt, MemoryExpiresAt, MemoryDeleteAfter        time.Time
	LiveExpiresAt, LiveDeleteAfter                       time.Time
	LivePresence                                         bool
}
type ExperienceState struct {
	ExperienceID, MemorySessionID, LiveSessionID               string
	TrackingStatus, PresencePhase, DisplayColor, DisplaySymbol string
	MemoryExpiresAt, MemoryDeleteAfter                         time.Time
	LiveExpiresAt, LiveDeleteAfter                             time.Time
	ReplayStatus                                               string
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
	return &Store{db: db, q: db, positions: &positionCache{latest: make(map[string]Position)}}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error {
	if migrationsErr != nil {
		return migrationsErr
	}
	return s.pingVersion(ctx, len(migrations))
}

func (s *Store) pingVersion(ctx context.Context, version int) error {
	count, oldest, newest, err := schemaVersion(ctx, s.q)
	if err != nil {
		return err
	}
	if count != version || oldest != 1 || newest != count {
		return errors.New("database schema does not match this application; run migrate or deploy a compatible version")
	}
	return nil
}

// Transaction serializes mutations on a session across concurrent requests.
// Cache updates become visible only after the matching DB transaction commits.
func (s *Store) Transaction(ctx context.Context, key string, fn func(*Store) error) error {
	if s.inTx {
		if err := s.lockTransaction(ctx, key); err != nil {
			return err
		}
		return fn(s)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	work := &Store{db: s.db, q: tx, inTx: true, positions: s.positions}
	if err := work.lockTransaction(ctx, key); err != nil {
		return err
	}
	if err := fn(work); err != nil {
		return err
	}
	if work.pending.operation != noPositionChange {
		s.positions.mu.Lock()
		defer s.positions.mu.Unlock()
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	switch work.pending.operation {
	case setPosition:
		s.positions.latest[work.pending.id] = work.pending.point
	case removePosition:
		delete(s.positions.latest, work.pending.id)
	}
	return nil
}

func (s *Store) lockTransaction(ctx context.Context, key string) error {
	if key == "" {
		return nil
	}
	digest := sha256.Sum256([]byte(key))
	_, err := s.q.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(digest[:8])))
	return err
}
func (s *Store) CreateExperience(ctx context.Context, v NewExperience) error {
	return s.Transaction(ctx, "", func(tx *Store) error {
		_, err := tx.q.ExecContext(ctx, `INSERT INTO memory_sessions
  (id,consent_version,consent_language,token_hash,created_at,expires_at,delete_after)
  VALUES ($1,$2,$3,$4,$5,$6,$7)`, v.MemorySessionID, v.MemoryConsent, v.ConsentLanguage, v.MemoryTokenHash, v.CreatedAt, v.MemoryExpiresAt, v.MemoryDeleteAfter)
		if err != nil {
			return err
		}
		var liveID any
		if v.LivePresence {
			liveID = v.LiveSessionID
			_, err = tx.q.ExecContext(ctx, `INSERT INTO live_presence_sessions
   (id,consent_version,token_hash,tracking_status,presence_phase,display_color,display_symbol,created_at,expires_at,delete_after)
   VALUES ($1,$2,$3,'tracking','approaching',$4,$5,$6,$7,$8)`, v.LiveSessionID, v.LiveConsent, v.LiveTokenHash, v.DisplayColor, v.DisplaySymbol, v.CreatedAt, v.LiveExpiresAt, v.LiveDeleteAfter)
			if err != nil {
				return err
			}
			linkTokenExpires := v.MemoryExpiresAt
			if v.LiveExpiresAt.Before(linkTokenExpires) {
				linkTokenExpires = v.LiveExpiresAt
			}
			linkExpires := v.MemoryDeleteAfter
			if v.LiveDeleteAfter.Before(linkExpires) {
				linkExpires = v.LiveDeleteAfter
			}
			_, err = tx.q.ExecContext(ctx, `INSERT INTO experience_links (id,memory_session_id,live_session_id,token_hash,token_expires_at,expires_at)
   VALUES ($1,$2,$3,$4,$5,$6)`, v.LinkID, v.MemorySessionID, v.LiveSessionID, v.LinkTokenHash, linkTokenExpires, linkExpires)
			if err != nil {
				return err
			}
		}
		_, err = tx.q.ExecContext(ctx, `INSERT INTO experiences
  (id,memory_session_id,live_session_id,display_color,display_symbol,created_at,expires_at,delete_after)
  VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, v.ExperienceID, v.MemorySessionID, liveID, v.DisplayColor, v.DisplaySymbol, v.CreatedAt, v.MemoryExpiresAt, v.MemoryDeleteAfter)
		return err
	})
}
func (s *Store) ExperienceByMemoryToken(ctx context.Context, id, token string, now time.Time) (ExperienceState, error) {
	var v ExperienceState
	var stored string
	var liveExpires, liveDelete sql.NullTime
	err := s.q.QueryRowContext(ctx, `SELECT e.id,e.memory_session_id,COALESCE(e.live_session_id,''),m.token_hash,
 COALESCE(lp.tracking_status,''),COALESCE(lp.presence_phase,''),e.display_color,e.display_symbol,
 m.expires_at,m.delete_after,lp.expires_at,lp.delete_after
 FROM experiences e JOIN memory_sessions m ON m.id=e.memory_session_id LEFT JOIN live_presence_sessions lp ON lp.id=e.live_session_id
 WHERE e.id=$1 AND m.delete_after>$2`, id, now).Scan(&v.ExperienceID, &v.MemorySessionID, &v.LiveSessionID, &stored,
		&v.TrackingStatus, &v.PresencePhase, &v.DisplayColor, &v.DisplaySymbol,
		&v.MemoryExpiresAt, &v.MemoryDeleteAfter, &liveExpires, &liveDelete)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if !secureEqual(stored, token) {
		return ExperienceState{}, ErrNotFound
	}
	if liveExpires.Valid {
		v.LiveExpiresAt = liveExpires.Time
		v.LiveDeleteAfter = liveDelete.Time
		if !now.Before(v.LiveExpiresAt) && v.TrackingStatus == "tracking" {
			v.TrackingStatus = "expired"
			v.PresencePhase = "ended"
		}
	}
	arrival, err := s.arrivalForMemory(ctx, v.MemorySessionID)
	if err == nil {
		v.ReplayStatus = arrival.Status
		if arrival.Status == "queued" && !now.Before(arrival.ReplayDeadline) {
			v.ReplayStatus = "skipped"
		}
	} else if !errors.Is(err, ErrNotFound) {
		return ExperienceState{}, err
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
	now := time.Now()
	if v.TrackingStatus != "tracking" || !now.Before(v.ExpiresAt) {
		return v, nil, nil
	}
	s.positions.mu.RLock()
	p, found := s.positions.latest[id]
	s.positions.mu.RUnlock()
	if !found || !now.Before(p.ExpiresAt) {
		return v, nil, nil
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
		tx.positions.mu.RLock()
		previous, found := tx.positions.latest[id]
		tx.positions.mu.RUnlock()
		if found && !p.RecordedAt.After(previous.RecordedAt) {
			return ErrConflict
		}
		if _, err := tx.q.ExecContext(ctx, "UPDATE live_presence_sessions SET last_seen_at=$1 WHERE id=$2", p.ReceivedAt, id); err != nil {
			return err
		}
		tx.pending = positionChange{id: id, point: p, operation: setPosition}
		return nil
	})
}
func (s *Store) StopLive(ctx context.Context, id, reason string, now time.Time) error {
	return s.Transaction(ctx, id, func(tx *Store) error {
		var status string
		var expires time.Time
		err := tx.q.QueryRowContext(ctx, "SELECT tracking_status,expires_at FROM live_presence_sessions WHERE id=$1 FOR UPDATE", id).Scan(&status, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != "stopped" {
			if status != "tracking" || !now.Before(expires) {
				return ErrConflict
			}
			if _, err := tx.q.ExecContext(ctx, `UPDATE live_presence_sessions SET tracking_status='stopped',presence_phase='ended',
  stop_reason=$1,stopped_at=$2 WHERE id=$3`, reason, now, id); err != nil {
				return err
			}
		}
		tx.pending = positionChange{id: id, operation: removePosition}
		return nil
	})
}
func (s *Store) SetOnSite(ctx context.Context, id, consent string, continuing bool, now time.Time) error {
	return s.Transaction(ctx, id, func(tx *Store) error {
		var status, phase string
		var expires time.Time
		err := tx.q.QueryRowContext(ctx, "SELECT tracking_status,presence_phase,expires_at FROM live_presence_sessions WHERE id=$1 FOR UPDATE", id).
			Scan(&status, &phase, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != "tracking" || !now.Before(expires) || (phase != "arrived" && phase != "on_site") {
			return ErrConflict
		}
		if !continuing {
			return tx.StopLive(ctx, id, "on_site_declined", now)
		}
		_, err = tx.q.ExecContext(ctx, `UPDATE live_presence_sessions SET presence_phase='on_site',on_site_consent_version=$1 WHERE id=$2`, consent, id)
		return err
	})
}
func (s *Store) DeleteMemory(ctx context.Context, id string) error {
	return s.Transaction(ctx, id, func(tx *Store) error {
		var pending bool
		err := tx.q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photos WHERE memory_session_id=$1)
 FROM memory_sessions WHERE id=$1 FOR UPDATE`, id).Scan(&pending)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if pending {
			return ErrPhotoPending
		}
		return tx.deleteSession(ctx, "DELETE FROM memory_sessions WHERE id=$1", id)
	})
}
func (s *Store) DeleteLive(ctx context.Context, id string) error {
	return s.Transaction(ctx, id, func(tx *Store) error {
		if err := tx.deleteSession(ctx, "DELETE FROM live_presence_sessions WHERE id=$1", id); err != nil {
			return err
		}
		tx.pending = positionChange{id: id, operation: removePosition}
		return nil
	})
}

// DeleteExperiencePart records each successful half with its deletion in one
// transaction. The live half runs first so the memory row can still identify
// the experience on retry; receipts bind both credentials to the request key.
func (s *Store) DeleteExperiencePart(ctx context.Context, id, memoryHash, liveHash, receiptKey string, memory bool, now time.Time) error {
	return s.Transaction(ctx, "delete:experience:"+id, func(tx *Store) error {
		requestHash := hashCredentials(memoryHash, liveHash)
		liveScope := hashScope("delete:experience:live:" + id)
		scope := liveScope
		if memory {
			scope = hashScope("delete:experience:memory:" + id)
		}
		record, err := tx.GetIdempotency(ctx, scope, receiptKey, now)
		if err == nil {
			if record.RequestHash != requestHash {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if memory {
			liveRecord, err := tx.GetIdempotency(ctx, liveScope, receiptKey, now)
			if err != nil {
				return err
			}
			if liveRecord.RequestHash != requestHash {
				return ErrConflict
			}
		}
		experience, err := tx.ExperienceByMemoryToken(ctx, id, memoryHash, now)
		if err != nil {
			return err
		}
		if memory {
			if err = tx.DeleteMemory(ctx, experience.MemorySessionID); err != nil {
				return err
			}
		} else {
			if experience.LiveSessionID == "" {
				return ErrNotFound
			}
			if err = tx.AuthenticateLive(ctx, experience.LiveSessionID, liveHash, now); err != nil {
				return err
			}
			if err = tx.DeleteLive(ctx, experience.LiveSessionID); err != nil {
				return err
			}
		}
		return tx.SaveIdempotency(ctx, scope, receiptKey, requestHash, 204, []byte{}, now.Add(24*time.Hour))
	})
}

func hashCredentials(memoryHash, liveHash string) string {
	digest := sha256.Sum256([]byte(memoryHash + "\x00" + liveHash))
	return hex.EncodeToString(digest[:])
}
func hashScope(scope string) string {
	digest := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(digest[:])
}

func (s *Store) deleteSession(ctx context.Context, query string, args ...any) error {
	result, err := s.q.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PurgePositions physically erases expired display points from process memory.
func (s *Store) PurgePositions(now time.Time) {
	s.positions.mu.Lock()
	defer s.positions.mu.Unlock()
	for id, p := range s.positions.latest {
		if !now.Before(p.ExpiresAt) {
			delete(s.positions.latest, id)
		}
	}
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
	s.PurgePositions(now)
	return s.Transaction(ctx, "trace:cleanup", func(tx *Store) error {
		if err := tx.ExpireArrivals(ctx, now); err != nil {
			return err
		}
		if err := tx.queueExpiredPhotoMemories(ctx, now); err != nil {
			return err
		}
		for _, query := range []string{
			"UPDATE live_presence_sessions SET tracking_status='expired',presence_phase='ended' WHERE expires_at<=$1 AND tracking_status='tracking'",
			"DELETE FROM experience_links WHERE expires_at<=$1",
			"DELETE FROM experiences WHERE delete_after<=$1",
			"DELETE FROM memory_sessions WHERE delete_after<=$1 AND NOT EXISTS (SELECT 1 FROM photos WHERE memory_session_id=memory_sessions.id)",
			"DELETE FROM live_presence_sessions WHERE delete_after<=$1",
			"DELETE FROM idempotency_records WHERE expires_at<=$1",
			"DELETE FROM arrival_receipts WHERE expires_at<=$1",
			"DELETE FROM arrival_operator_audit WHERE created_at + INTERVAL '24 hours' <= $1",
		} {
			if _, err := tx.q.ExecContext(ctx, query, now); err != nil {
				return err
			}
		}
		return nil
	})
}
func secureEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
