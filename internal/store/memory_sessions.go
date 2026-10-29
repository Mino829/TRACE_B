package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// StopMemory ends participation without shortening the owner's read/delete window.
// Display and session locks follow the same order as arrival and replay mutations.
func (s *Store) StopMemory(ctx context.Context, id string, now time.Time) (MemorySession, error) {
	var memory MemorySession
	err := s.Transaction(ctx, "display:lease", func(tx *Store) error {
		if err := tx.lockTransaction(ctx, id); err != nil {
			return err
		}
		var err error
		memory, err = tx.MemorySession(ctx, id)
		if err != nil {
			return err
		}
		if !now.Before(memory.DeleteAfter) {
			return ErrNotFound
		}
		if memory.Deleting {
			return ErrConflict
		}
		if err := tx.q.QueryRowContext(ctx, `UPDATE memory_sessions SET stopped_at=COALESCE(stopped_at,$2)
 WHERE id=$1 RETURNING stopped_at`, id, now).Scan(&memory.StoppedAt); err != nil {
			return err
		}
		if err := tx.EndPushForMemory(ctx, id); err != nil {
			return err
		}
		return tx.stopMemoryDisplay(ctx, id, now)
	})
	return memory, err
}

// LiveSessionByToken retains legacy owner rights independently of Memory rows.
// This state-only query never reads the retired position cache or alias fields.
func (s *Store) LiveSessionByToken(ctx context.Context, id, token string, now time.Time) (LiveSession, error) {
	var live LiveSession
	var stored string
	err := s.q.QueryRowContext(ctx, `SELECT id,token_hash,tracking_status,expires_at,delete_after
 FROM live_presence_sessions WHERE id=$1 AND delete_after>$2`, id, now).
		Scan(&live.ID, &stored, &live.TrackingStatus, &live.ExpiresAt, &live.DeleteAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return LiveSession{}, ErrNotFound
	}
	if err != nil {
		return LiveSession{}, err
	}
	if !secureEqual(stored, token) {
		return LiveSession{}, ErrNotFound
	}
	if live.TrackingStatus == "tracking" && !now.Before(live.ExpiresAt) {
		live.TrackingStatus = "expired"
	}
	return live, nil
}

// SaveMemoryIdempotency binds a mutation receipt to its owner's deletion lifecycle.
// The existing operation receipt window is capped, never extended, by delete_after.
func (s *Store) SaveMemoryIdempotency(ctx context.Context, memoryID, scope, key, requestHash string, status int, body []byte, expires time.Time) error {
	result, err := s.q.ExecContext(ctx, `INSERT INTO idempotency_records
 (scope,key_hash,request_hash,response_status,response_body,expires_at,memory_session_id)
 SELECT $1,$2,$3,$4,$5,LEAST($6,delete_after),id FROM memory_sessions WHERE id=$7
 ON CONFLICT(scope,key_hash) DO UPDATE SET request_hash=EXCLUDED.request_hash,
 response_status=EXCLUDED.response_status,response_body=EXCLUDED.response_body,
 expires_at=EXCLUDED.expires_at,memory_session_id=EXCLUDED.memory_session_id
 WHERE idempotency_records.expires_at<=CURRENT_TIMESTAMP`, scope, key, requestHash, status, body, expires, memoryID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}
