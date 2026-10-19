package photo

import (
	"context"
	"errors"
	"time"

	"github.com/Mino829/trace_Backend/internal/photoasset"
	"github.com/Mino829/trace_Backend/internal/store"
)

var (
	ErrStorage          = errors.New("photo storage is unavailable")
	ErrUploadIncomplete = errors.New("photo upload is incomplete")
)

type Service struct {
	db     *store.Store
	assets photoasset.Storage
}

func New(db *store.Store, assets photoasset.Storage) *Service {
	return &Service{db: db, assets: assets}
}

type Upload struct {
	URL       string
	Headers   map[string]string
	ExpiresAt time.Time
}

func metadata(p store.Photo) photoasset.Metadata {
	return photoasset.Metadata{MIMEType: p.MIMEType, SizeBytes: p.SizeBytes, Width: p.Width, Height: p.Height, SHA256: p.SHA256}
}

func (s *Service) Upload(ctx context.Context, p store.Photo, now time.Time) (Upload, error) {
	var result Upload
	err := s.db.Transaction(ctx, p.MemorySessionID, func(tx *store.Store) error {
		memory, err := tx.MemorySession(ctx, p.MemorySessionID)
		if err != nil {
			return err
		}
		current, err := tx.Photo(ctx, p.MemorySessionID, p.ID)
		if err != nil {
			return err
		}
		if memory.Deleting || !now.Before(memory.ExpiresAt) || current.Status != "pending" || !now.Before(current.UploadExpiresAt) {
			return store.ErrConflict
		}
		url, headers, err := s.assets.UploadURL(ctx, current.StageKey, metadata(current), current.UploadExpiresAt)
		if err != nil {
			return ErrStorage
		}
		result = Upload{URL: url, Headers: headers, ExpiresAt: current.UploadExpiresAt}
		return nil
	})
	return result, err
}

func (s *Service) View(ctx context.Context, p store.Photo, now time.Time) (string, error) {
	var result string
	err := s.db.Transaction(ctx, p.MemorySessionID, func(tx *store.Store) error {
		memory, err := tx.MemorySession(ctx, p.MemorySessionID)
		if err != nil {
			return err
		}
		current, err := tx.Photo(ctx, p.MemorySessionID, p.ID)
		if err != nil {
			return err
		}
		if current.Status != "ready" || memory.Deleting || !now.Before(memory.DeleteAfter) {
			return store.ErrNotFound
		}
		expires := now.Add(5 * time.Minute)
		if expires.After(memory.DeleteAfter) {
			expires = memory.DeleteAfter
		}
		result, err = s.assets.ViewURL(ctx, current.ObjectKey, expires)
		if err != nil {
			return ErrStorage
		}
		return nil
	})
	return result, err
}

// Complete serializes promotion against new upload grants and deletion intents.
// A crash after promotion can be retried: only the expected image is accepted
// from an existing immutable final object. No unverified URL is ever returned.
func (s *Service) Complete(ctx context.Context, memoryID, photoID string, now time.Time) (store.Photo, error) {
	var result store.Photo
	invalid := false
	err := s.db.Transaction(ctx, memoryID, func(tx *store.Store) error {
		memory, err := tx.MemorySession(ctx, memoryID)
		if err != nil {
			return err
		}
		if memory.Deleting || !now.Before(memory.DeleteAfter) {
			return store.ErrConflict
		}
		p, err := tx.Photo(ctx, memoryID, photoID)
		if err != nil {
			return err
		}
		result = p
		if p.Status == "ready" || p.Status == "failed" {
			return nil
		}
		if p.Status != "pending" || !now.Before(memory.ExpiresAt) {
			return store.ErrConflict
		}
		ready := func() error {
			if err := s.assets.Fence(ctx, p.StageKey); err != nil {
				return ErrStorage
			}
			if err := tx.ReadyPhoto(ctx, memoryID, photoID, now); err != nil {
				return err
			}
			result.Status = "ready"
			return nil
		}
		object, readErr := s.assets.Read(ctx, p.StageKey, photoasset.MaxBytes)
		if readErr != nil && !errors.Is(readErr, photoasset.ErrInvalidImage) && !errors.Is(readErr, photoasset.ErrNotFound) {
			return ErrStorage
		}
		if readErr != nil || object.ContentType != p.MIMEType || photoasset.Validate(object.Data, metadata(p)) != nil {
			// The process may have crashed after fencing the upload but before
			// committing ready. Recover only from the validated final copy.
			final, finalErr := s.assets.Read(ctx, p.ObjectKey, photoasset.MaxBytes)
			if finalErr == nil && final.ContentType == p.MIMEType && photoasset.Validate(final.Data, metadata(p)) == nil {
				return ready()
			}
			if !errors.Is(finalErr, photoasset.ErrNotFound) {
				return ErrStorage
			}
			if errors.Is(readErr, photoasset.ErrNotFound) {
				return ErrUploadIncomplete
			}
			invalid = true
			return tx.QueuePhotoDeletion(ctx, memoryID, photoID, "failed", "INVALID_IMAGE")
		}
		err = s.assets.Promote(ctx, p.StageKey, p.ObjectKey, object.Generation, metadata(p))
		if errors.Is(err, photoasset.ErrAlreadyExists) {
			final, readErr := s.assets.Read(ctx, p.ObjectKey, photoasset.MaxBytes)
			if readErr != nil || final.ContentType != p.MIMEType || photoasset.Validate(final.Data, metadata(p)) != nil {
				return ErrStorage
			}
		} else if err != nil {
			return ErrStorage
		}
		return ready()
	})
	if err != nil {
		return store.Photo{}, err
	}
	if invalid {
		p, err := s.db.Photo(ctx, memoryID, photoID)
		if err != nil {
			return store.Photo{}, err
		}
		if err := s.erase(ctx, p); err != nil {
			return store.Photo{}, err
		}
		return s.db.Photo(ctx, memoryID, photoID)
	}
	return result, nil
}

func (s *Service) erase(ctx context.Context, snapshot store.Photo) error {
	return s.db.Transaction(ctx, snapshot.MemorySessionID, func(tx *store.Store) error {
		// Cleanup may hold an older deletion snapshot. Never recreate a
		// stage fence after another worker has already finished its erasure.
		p, err := tx.Photo(ctx, snapshot.MemorySessionID, snapshot.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if p.Status != "deleting" {
			return nil
		}
		finish := func(work *store.Store) error {
			// A signed generation-zero PUT remains usable while the object
			// is absent. Keep zero bytes until every grant has expired.
			if p.StageKey != "" {
				if err := s.assets.Fence(ctx, p.StageKey); err != nil {
					return ErrStorage
				}
			}
			if p.ObjectKey != "" {
				if err := s.assets.Delete(ctx, p.ObjectKey); err != nil {
					return ErrStorage
				}
			}
			return work.FinishPhotoDeletion(ctx, p.MemorySessionID, p.ID)
		}
		if p.StageKey == "" {
			return finish(tx)
		}
		// Same lock as fence reclamation, always after the memory lock.
		return tx.Transaction(ctx, "photo-fence:"+p.StageKey, finish)
	})
}

func (s *Service) DeletePhoto(ctx context.Context, memoryID, photoID string) error {
	if err := s.db.QueuePhotoDeletion(ctx, memoryID, photoID, "deleted", ""); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	p, err := s.db.Photo(ctx, memoryID, photoID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.erase(ctx, p)
}

func (s *Service) DeleteMemoryPhotos(ctx context.Context, memoryID string) error {
	if err := s.db.QueueMemoryPhotoDeletion(ctx, memoryID); err != nil {
		return err
	}
	photos, err := s.db.Photos(ctx, memoryID)
	if err != nil {
		return err
	}
	for _, p := range photos {
		if err := s.erase(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// Purge resumes durable deletion intents and then removes expired zero-byte
// upload fences. Object errors never discard the persisted cleanup knowledge.
func (s *Service) Purge(ctx context.Context, now time.Time) error {
	photos, err := s.db.PendingPhotoDeletions(ctx, 100)
	if err != nil {
		return err
	}
	var first error
	for _, p := range photos {
		if err := s.erase(ctx, p); err != nil && first == nil {
			first = err
		}
	}
	fences, err := s.db.ExpiredPhotoFences(ctx, now, 100)
	if err != nil {
		return err
	}
	for _, fence := range fences {
		err := s.db.Transaction(ctx, "photo-fence:"+fence.Key, func(tx *store.Store) error {
			current, err := tx.PhotoFence(ctx, fence.Key)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if now.Before(current.NotBefore) {
				return nil
			}
			if err := s.assets.Delete(ctx, fence.Key); err != nil {
				return ErrStorage
			}
			return tx.RemovePhotoFence(ctx, fence.Key)
		})
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}
