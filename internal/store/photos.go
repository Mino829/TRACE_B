package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

var (
	ErrPhotoLimit   = errors.New("photo limit reached")
	ErrUploadLimit  = errors.New("upload attempt limit reached")
	ErrPhotoPending = errors.New("photo deletion pending")
)

type MemorySession struct {
	ID                                string
	CreatedAt, ExpiresAt, DeleteAfter time.Time
	Deleting                          bool
}

type PhotoLocation struct {
	Latitude, Longitude, AccuracyM float64
	RecordedAt                     time.Time
}

type Photo struct {
	ID, MemorySessionID, ClientPhotoID, RequestHash     string
	CapturedAt, CreatedAt, DeleteAfter, UploadExpiresAt time.Time
	Location                                            *PhotoLocation
	MIMEType, SHA256, Status, StageKey, ObjectKey       string
	SizeBytes                                           int64
	Width, Height, SequenceNo, UploadAttempts           int
	FailureCode, DeleteTarget                           string
}

type PhotoSummary struct {
	Ready, Pending, Failed int
}

type PhotoFence struct {
	Key       string
	NotBefore time.Time
}

const photoColumns = `id,memory_session_id,client_photo_id,request_hash,
 captured_at,created_at,delete_after,upload_expires_at,
 latitude,longitude,accuracy_m,location_recorded_at,
 mime_type,sha256,size_bytes,width,height,sequence_no,upload_attempts,
 status,COALESCE(stage_key,''),COALESCE(object_key,''),failure_code,delete_target`

type photoScanner interface {
	Scan(...any) error
}

func scanPhoto(row photoScanner) (Photo, error) {
	var p Photo
	var latitude, longitude, accuracy sql.NullFloat64
	var recorded sql.NullTime
	err := row.Scan(&p.ID, &p.MemorySessionID, &p.ClientPhotoID, &p.RequestHash,
		&p.CapturedAt, &p.CreatedAt, &p.DeleteAfter, &p.UploadExpiresAt,
		&latitude, &longitude, &accuracy, &recorded,
		&p.MIMEType, &p.SHA256, &p.SizeBytes, &p.Width, &p.Height, &p.SequenceNo, &p.UploadAttempts,
		&p.Status, &p.StageKey, &p.ObjectKey, &p.FailureCode, &p.DeleteTarget)
	if errors.Is(err, sql.ErrNoRows) {
		return Photo{}, ErrNotFound
	}
	if err != nil {
		return Photo{}, err
	}
	if latitude.Valid {
		p.Location = &PhotoLocation{Latitude: latitude.Float64, Longitude: longitude.Float64,
			AccuracyM: accuracy.Float64, RecordedAt: recorded.Time}
	}
	return p, nil
}

func (s *Store) MemorySession(ctx context.Context, id string) (MemorySession, error) {
	var m MemorySession
	err := s.q.QueryRowContext(ctx, `SELECT id,created_at,expires_at,delete_after,deleting FROM memory_sessions WHERE id=$1`, id).
		Scan(&m.ID, &m.CreatedAt, &m.ExpiresAt, &m.DeleteAfter, &m.Deleting)
	if errors.Is(err, sql.ErrNoRows) {
		return MemorySession{}, ErrNotFound
	}
	return m, err
}

func (s *Store) activePhotoMemory(ctx context.Context, id string, now time.Time) (MemorySession, error) {
	m, err := s.MemorySession(ctx, id)
	if err != nil {
		return m, err
	}
	if m.Deleting || !now.Before(m.ExpiresAt) {
		return m, ErrConflict
	}
	return m, nil
}

// CreatePhoto keeps duplicate client requests stable before checking the quota.
// The session counter also prevents sequence reuse after the highest photo is erased.
func (s *Store) CreatePhoto(ctx context.Context, p Photo, now time.Time) (Photo, error) {
	var result Photo
	err := s.Transaction(ctx, p.MemorySessionID, func(tx *Store) error {
		m, err := tx.activePhotoMemory(ctx, p.MemorySessionID, now)
		if err != nil {
			return err
		}
		existing, err := scanPhoto(tx.q.QueryRowContext(ctx, "SELECT "+photoColumns+" FROM photos WHERE memory_session_id=$1 AND client_photo_id=$2", p.MemorySessionID, p.ClientPhotoID))
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
		if p.CapturedAt.Before(m.CreatedAt) || p.CapturedAt.After(now.Add(5*time.Minute)) || p.CapturedAt.After(m.ExpiresAt) ||
			!validUploadExpiry(p.UploadExpiresAt, now, m.ExpiresAt) {
			return ErrConflict
		}
		var count int
		if err := tx.q.QueryRowContext(ctx, "SELECT COUNT(*) FROM photos WHERE memory_session_id=$1", p.MemorySessionID).Scan(&count); err != nil {
			return err
		}
		if count >= 12 {
			return ErrPhotoLimit
		}
		p.CreatedAt, p.DeleteAfter = now, m.DeleteAfter
		p.Status, p.UploadAttempts, p.FailureCode, p.DeleteTarget = "pending", 1, "", ""
		if err := tx.q.QueryRowContext(ctx, `UPDATE memory_sessions SET photo_sequence=GREATEST(photo_sequence,
 (SELECT COALESCE(MAX(sequence_no),0) FROM photos WHERE memory_session_id=$1))+1 WHERE id=$1 RETURNING photo_sequence`, p.MemorySessionID).Scan(&p.SequenceNo); err != nil {
			return err
		}
		var latitude, longitude, accuracy, recorded any
		if p.Location != nil {
			latitude, longitude, accuracy, recorded = p.Location.Latitude, p.Location.Longitude, p.Location.AccuracyM, p.Location.RecordedAt
		}
		digest := sha256.Sum256([]byte(p.ID))
		insert, err := tx.q.ExecContext(ctx, `INSERT INTO photos
 (id,id_hash,memory_session_id,client_photo_id,request_hash,captured_at,created_at,delete_after,upload_expires_at,
 latitude,longitude,accuracy_m,location_recorded_at,mime_type,sha256,size_bytes,width,height,
 sequence_no,upload_attempts,status,stage_key,object_key)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,1,'pending',$20,$21)
 ON CONFLICT DO NOTHING`, p.ID, hex.EncodeToString(digest[:]), p.MemorySessionID, p.ClientPhotoID, p.RequestHash,
			p.CapturedAt, p.CreatedAt, p.DeleteAfter, p.UploadExpiresAt, latitude, longitude, accuracy, recorded,
			p.MIMEType, p.SHA256, p.SizeBytes, p.Width, p.Height, p.SequenceNo, p.StageKey, p.ObjectKey)
		if err != nil {
			return err
		}
		if n, err := insert.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrConflict
		}
		result = p
		return nil
	})
	return result, err
}

func validUploadExpiry(expires, now, memoryExpires time.Time) bool {
	return expires.After(now) && !expires.After(now.Add(5*time.Minute)) && !expires.After(memoryExpires)
}

// Photo and Photos return metadata for every lifecycle state; consumers must not
// issue view capabilities for anything except ready photos in non-deleting memory.
func (s *Store) Photo(ctx context.Context, memoryID, photoID string) (Photo, error) {
	return scanPhoto(s.q.QueryRowContext(ctx, "SELECT "+photoColumns+" FROM photos WHERE memory_session_id=$1 AND id=$2", memoryID, photoID))
}

func (s *Store) PhotoByHash(ctx context.Context, memoryID, photoIDHash string) (Photo, error) {
	return scanPhoto(s.q.QueryRowContext(ctx, "SELECT "+photoColumns+" FROM photos WHERE memory_session_id=$1 AND id_hash=$2", memoryID, photoIDHash))
}

func (s *Store) Photos(ctx context.Context, memoryID string) ([]Photo, error) {
	rows, err := s.q.QueryContext(ctx, "SELECT "+photoColumns+" FROM photos WHERE memory_session_id=$1 ORDER BY captured_at,sequence_no", memoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	photos := make([]Photo, 0)
	for rows.Next() {
		p, err := scanPhoto(rows)
		if err != nil {
			return nil, err
		}
		photos = append(photos, p)
	}
	return photos, rows.Err()
}

func (s *Store) PhotoSummary(ctx context.Context, memoryID string) (PhotoSummary, error) {
	var summary PhotoSummary
	err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE status='ready'),
 COUNT(*) FILTER (WHERE status='pending'),COUNT(*) FILTER (WHERE status='failed')
 FROM photos WHERE memory_session_id=$1`, memoryID).Scan(&summary.Ready, &summary.Pending, &summary.Failed)
	return summary, err
}

func (s *Store) RenewPhotoUpload(ctx context.Context, memoryID, photoID string, expires, now time.Time) (Photo, error) {
	var p Photo
	err := s.Transaction(ctx, memoryID, func(tx *Store) error {
		m, err := tx.activePhotoMemory(ctx, memoryID, now)
		if err != nil {
			return err
		}
		p, err = tx.Photo(ctx, memoryID, photoID)
		if err != nil {
			return err
		}
		if p.Status != "pending" || !validUploadExpiry(expires, now, m.ExpiresAt) {
			return ErrConflict
		}
		if p.UploadAttempts >= 5 {
			return ErrUploadLimit
		}
		// Keep the longest issued capability lifetime for durable deletion fencing,
		// even if a caller's clock moves backwards between reissue attempts.
		if expires.Before(p.UploadExpiresAt) {
			expires = p.UploadExpiresAt
		}
		p.UploadAttempts++
		p.UploadExpiresAt = expires
		_, err = tx.q.ExecContext(ctx, `UPDATE photos SET upload_attempts=$3,upload_expires_at=$4 WHERE memory_session_id=$1 AND id=$2`,
			memoryID, photoID, p.UploadAttempts, p.UploadExpiresAt)
		return err
	})
	return p, err
}

func (s *Store) ReadyPhoto(ctx context.Context, memoryID, photoID string, now time.Time) error {
	return s.Transaction(ctx, memoryID, func(tx *Store) error {
		if _, err := tx.activePhotoMemory(ctx, memoryID, now); err != nil {
			return err
		}
		p, err := tx.Photo(ctx, memoryID, photoID)
		if err != nil {
			return err
		}
		if p.Status != "pending" && p.Status != "ready" {
			return ErrConflict
		}
		if err := tx.enqueuePhotoFence(ctx, p.StageKey, p.UploadExpiresAt); err != nil {
			return err
		}
		if _, err := tx.q.ExecContext(ctx, "UPDATE photos SET status='ready' WHERE memory_session_id=$1 AND id=$2", memoryID, photoID); err != nil {
			return err
		}
		if p.Status == "pending" {
			if err := tx.PostponePush(ctx, memoryID, now); err != nil {
				return err
			}
			return tx.reconcileRoutes(ctx, memoryID)
		}
		return nil
	})
}

func (s *Store) QueuePhotoDeletion(ctx context.Context, memoryID, photoID, target, failure string) error {
	if target != "deleted" && target != "failed" {
		return ErrConflict
	}
	return s.Transaction(ctx, memoryID, func(tx *Store) error {
		p, err := tx.Photo(ctx, memoryID, photoID)
		if err != nil {
			return err
		}
		// An explicit erasure request must never be downgraded by a late invalid
		// image failure, including a failure racing memory-wide deletion.
		if p.Status == "deleting" && p.DeleteTarget == "deleted" {
			return nil
		}
		if _, err = tx.q.ExecContext(ctx, `UPDATE photos SET status='deleting',delete_target=$3,failure_code=$4 WHERE memory_session_id=$1 AND id=$2`,
			memoryID, photoID, target, failure); err != nil {
			return err
		}
		return tx.reconcileRoutes(ctx, memoryID)
	})
}

func (s *Store) QueueMemoryPhotoDeletion(ctx context.Context, memoryID string) error {
	return s.Transaction(ctx, memoryID, func(tx *Store) error {
		if err := tx.deleteSession(ctx, "UPDATE memory_sessions SET deleting=TRUE WHERE id=$1", memoryID); err != nil {
			return err
		}
		if err := tx.StopPushForMemory(ctx, memoryID); err != nil {
			return err
		}
		if err := tx.removeMemoryDisplay(ctx, memoryID, time.Now().UTC()); err != nil {
			return err
		}
		_, err := tx.q.ExecContext(ctx, `UPDATE photos SET status='deleting',delete_target='deleted',failure_code='' WHERE memory_session_id=$1`, memoryID)
		return err
	})
}

func (s *Store) PendingPhotoDeletions(ctx context.Context, limit int) ([]Photo, error) {
	photos := make([]Photo, 0)
	if limit <= 0 {
		return photos, nil
	}
	rows, err := s.q.QueryContext(ctx, "SELECT "+photoColumns+" FROM photos WHERE status='deleting' ORDER BY delete_after,memory_session_id,sequence_no LIMIT $1", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanPhoto(rows)
		if err != nil {
			return nil, err
		}
		photos = append(photos, p)
	}
	return photos, rows.Err()
}

func (s *Store) FinishPhotoDeletion(ctx context.Context, memoryID, photoID string) error {
	return s.Transaction(ctx, memoryID, func(tx *Store) error {
		p, err := tx.Photo(ctx, memoryID, photoID)
		if err != nil {
			return err
		}
		if p.Status != "deleting" {
			return ErrConflict
		}
		if err := tx.enqueuePhotoFence(ctx, p.StageKey, p.UploadExpiresAt); err != nil {
			return err
		}
		if p.DeleteTarget == "deleted" {
			if err := tx.deleteSession(ctx, "DELETE FROM photos WHERE memory_session_id=$1 AND id=$2", memoryID, photoID); err != nil {
				return err
			}
		} else {
			if _, err = tx.q.ExecContext(ctx, `UPDATE photos SET status='failed',delete_target='',
 latitude=NULL,longitude=NULL,accuracy_m=NULL,location_recorded_at=NULL,stage_key=NULL,object_key=NULL
 WHERE memory_session_id=$1 AND id=$2`, memoryID, photoID); err != nil {
				return err
			}
		}
		return tx.reconcileRoutes(ctx, memoryID)
	})
}

func (s *Store) enqueuePhotoFence(ctx context.Context, key string, notBefore time.Time) error {
	if key == "" {
		return nil
	}
	_, err := s.q.ExecContext(ctx, `INSERT INTO photo_upload_fences (object_key,not_before) VALUES ($1,$2)
 ON CONFLICT (object_key) DO UPDATE SET not_before=GREATEST(photo_upload_fences.not_before,EXCLUDED.not_before)`, key, notBefore)
	return err
}

func (s *Store) ExpiredPhotoFences(ctx context.Context, now time.Time, limit int) ([]PhotoFence, error) {
	fences := make([]PhotoFence, 0)
	if limit <= 0 {
		return fences, nil
	}
	rows, err := s.q.QueryContext(ctx, "SELECT object_key,not_before FROM photo_upload_fences WHERE not_before<=$1 ORDER BY not_before,object_key LIMIT $2", now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var fence PhotoFence
		if err := rows.Scan(&fence.Key, &fence.NotBefore); err != nil {
			return nil, err
		}
		fences = append(fences, fence)
	}
	return fences, rows.Err()
}

func (s *Store) RemovePhotoFence(ctx context.Context, key string) error {
	return s.Transaction(ctx, "photo-fence:"+key, func(tx *Store) error {
		_, err := tx.q.ExecContext(ctx, "DELETE FROM photo_upload_fences WHERE object_key=$1", key)
		return err
	})
}

// PhotoStorageRequired includes the independent fence outbox after session erasure.
func (s *Store) PhotoStorageRequired(ctx context.Context) (bool, error) {
	var required bool
	err := s.q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photos)
 OR EXISTS(SELECT 1 FROM photo_upload_fences)`).Scan(&required)
	return required, err
}

// queueExpiredPhotoMemories runs inside Cleanup's transaction. Acquire the same
// per-memory advisory lock as uploads before marking any expired session.
func (s *Store) queueExpiredPhotoMemories(ctx context.Context, now time.Time) error {
	rows, err := s.q.QueryContext(ctx, "SELECT id FROM memory_sessions WHERE delete_after<=$1 ORDER BY id", now)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.QueueMemoryPhotoDeletion(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return nil
}

// FinishPhotoDeleteReceipt advances only this operation's persisted intent;
// other unexpired idempotency records keep their original immutable response.
func (s *Store) FinishPhotoDeleteReceipt(ctx context.Context, scope, key string, now time.Time) error {
	result, err := s.q.ExecContext(ctx, `UPDATE idempotency_records SET response_status=204,response_body=$4
 WHERE scope=$1 AND key_hash=$2 AND expires_at>$3 AND response_status IN (202,204)`,
		scope, key, now, []byte{})
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) PhotoFence(ctx context.Context, key string) (PhotoFence, error) {
	var fence PhotoFence
	err := s.q.QueryRowContext(ctx, "SELECT object_key,not_before FROM photo_upload_fences WHERE object_key=$1", key).
		Scan(&fence.Key, &fence.NotBefore)
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoFence{}, ErrNotFound
	}
	return fence, err
}
