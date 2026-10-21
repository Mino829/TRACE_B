package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func photoInput(memoryID, id string, now time.Time) store.Photo {
	return store.Photo{ID: id, MemorySessionID: memoryID, ClientPhotoID: "client-" + id, RequestHash: "request-" + id,
		CapturedAt: now, UploadExpiresAt: now.Add(5 * time.Minute), MIMEType: "image/jpeg", SHA256: "metadata-digest",
		SizeBytes: 1024, Width: 640, Height: 480, StageKey: "stage-" + id, ObjectKey: "object-" + id}
}

func createPhoto(t *testing.T, f *testdb.Fixture, p store.Photo, now time.Time) store.Photo {
	t.Helper()
	created, err := f.Store.CreatePhoto(t.Context(), p, now)
	if err != nil {
		t.Fatal("create photo:", err)
	}
	return created
}

func TestPhotosConcurrentQuotaIdempotencyAndSequence(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	memory := fixtureExperience(t, f, now)
	var workers sync.WaitGroup
	results := make(chan error, 24)
	for i := range 24 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := f.Store.CreatePhoto(t.Context(), photoInput(memory.MemorySessionID, fmt.Sprintf("photo-%02d", i), now), now)
			results <- err
		}()
	}
	workers.Wait()
	close(results)
	var accepted, limited int
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, store.ErrPhotoLimit):
			limited++
		default:
			t.Fatal("concurrent registration:", err)
		}
	}
	if accepted != 12 || limited != 12 {
		t.Fatalf("quota was not serialized: accepted=%d limited=%d", accepted, limited)
	}
	photos, err := f.Store.Photos(t.Context(), memory.MemorySessionID)
	if err != nil || len(photos) != 12 {
		t.Fatal("quota list:", err)
	}
	for i, p := range photos {
		if p.SequenceNo != i+1 || p.Status != "pending" || p.UploadAttempts != 1 || !p.DeleteAfter.Equal(memory.MemoryDeleteAfter) {
			t.Fatal("initial lifecycle or sequence not assigned")
		}
	}
	original := photos[0]
	retry := photoInput(memory.MemorySessionID, "replacement-id", now)
	retry.ClientPhotoID, retry.RequestHash = original.ClientPhotoID, original.RequestHash
	replayed, err := f.Store.CreatePhoto(t.Context(), retry, now)
	if err != nil || replayed.ID != original.ID || replayed.SequenceNo != original.SequenceNo || replayed.StageKey != original.StageKey {
		t.Fatal("same client retry failed at quota:", err)
	}
	retry.RequestHash = "different-request"
	if _, err := f.Store.CreatePhoto(t.Context(), retry, now); !errors.Is(err, store.ErrConflict) {
		t.Fatal("different request reused client ID:", err)
	}
	if err := f.Store.QueuePhotoDeletion(t.Context(), memory.MemorySessionID, photos[0].ID, "failed", "invalid_image"); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.FinishPhotoDeletion(t.Context(), memory.MemorySessionID, photos[0].ID); err != nil {
		t.Fatal(err)
	}
	last := photos[len(photos)-1]
	if err := f.Store.QueuePhotoDeletion(t.Context(), memory.MemorySessionID, last.ID, "deleted", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.CreatePhoto(t.Context(), photoInput(memory.MemorySessionID, "over-quota", now), now); !errors.Is(err, store.ErrPhotoLimit) {
		t.Fatal("failed or deleting photos escaped quota:", err)
	}
	summary, err := f.Store.PhotoSummary(t.Context(), memory.MemorySessionID)
	if err != nil || summary.Pending != 10 || summary.Failed != 1 || summary.Ready != 0 {
		t.Fatal("status summary included deleting photo:", err)
	}
	if err := f.Store.FinishPhotoDeletion(t.Context(), memory.MemorySessionID, last.ID); err != nil {
		t.Fatal(err)
	}
	replacement := createPhoto(t, f, photoInput(memory.MemorySessionID, "new-photo", now), now)
	if replacement.SequenceNo != 13 {
		t.Fatal("sequence was reused after erasing the highest photo")
	}
	digest := sha256.Sum256([]byte(replacement.ID))
	located, err := f.Store.PhotoByHash(t.Context(), memory.MemorySessionID, hex.EncodeToString(digest[:]))
	if err != nil || located.ID != replacement.ID {
		t.Fatal("hashed receipt locator failed:", err)
	}
	if _, err := f.Store.PhotoByHash(t.Context(), "another-memory", hex.EncodeToString(digest[:])); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("hashed receipt lookup crossed memory scope:", err)
	}
}

func TestPhotoLocationAndExpiryBoundaries(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	memory := fixtureExperience(t, f, now)
	without := createPhoto(t, f, photoInput(memory.MemorySessionID, "no-location", now), now)
	if without.Location != nil {
		t.Fatal("missing location became zero coordinates")
	}
	input := photoInput(memory.MemorySessionID, "with-location", now)
	input.CapturedAt = now.Add(5 * time.Minute)
	input.Location = &store.PhotoLocation{Latitude: 35.5, Longitude: 139.5, AccuracyM: 4.25, RecordedAt: now.Add(time.Second)}
	createPhoto(t, f, input, now)
	loaded, err := f.Store.Photo(t.Context(), memory.MemorySessionID, input.ID)
	if err != nil || loaded.Location == nil || loaded.Location.Latitude != input.Location.Latitude ||
		loaded.Location.Longitude != input.Location.Longitude || loaded.Location.AccuracyM != input.Location.AccuracyM ||
		!loaded.Location.RecordedAt.Equal(input.Location.RecordedAt) {
		t.Fatal("location failed to round trip:", err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), "UPDATE photos SET latitude=1 WHERE id=$1", without.ID); err == nil {
		t.Fatal("partially nullable location accepted")
	}
	for _, tc := range []struct {
		name     string
		at       time.Time
		captured time.Time
		expires  time.Time
	}{
		{"before-memory", now, now.Add(-time.Microsecond), now.Add(time.Minute)},
		{"beyond-clock-window", now, now.Add(5*time.Minute + time.Microsecond), now.Add(time.Minute)},
		{"beyond-memory-expiry", memory.MemoryExpiresAt.Add(-time.Minute), memory.MemoryExpiresAt.Add(time.Microsecond), memory.MemoryExpiresAt},
		{"at-memory-expiry", memory.MemoryExpiresAt, memory.MemoryExpiresAt, memory.MemoryExpiresAt.Add(time.Minute)},
		{"upload-expired", now, now, now},
		{"upload-too-long", now, now, now.Add(5*time.Minute + time.Microsecond)},
	} {
		p := photoInput(memory.MemorySessionID, tc.name, tc.at)
		p.CapturedAt, p.UploadExpiresAt = tc.captured, tc.expires
		if _, err := f.Store.CreatePhoto(t.Context(), p, tc.at); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("%s boundary accepted: %v", tc.name, err)
		}
	}
	at := memory.MemoryExpiresAt.Add(-time.Minute)
	boundary := photoInput(memory.MemorySessionID, "captured-at-expiry", at)
	boundary.CapturedAt, boundary.UploadExpiresAt = memory.MemoryExpiresAt, memory.MemoryExpiresAt
	created := createPhoto(t, f, boundary, at)
	if !created.UploadExpiresAt.Equal(memory.MemoryExpiresAt) {
		t.Fatal("upload expiry did not retain the session boundary")
	}
	if err := f.Store.AuthenticateMemory(t.Context(), memory.MemorySessionID, memory.MemoryTokenHash, memory.MemoryExpiresAt); err != nil {
		t.Fatal("photo expiry changed credential deletion window:", err)
	}
	if _, err := f.Store.RenewPhotoUpload(t.Context(), memory.MemorySessionID, without.ID, memory.MemoryExpiresAt.Add(time.Minute), memory.MemoryExpiresAt); !errors.Is(err, store.ErrConflict) {
		t.Fatal("expired memory reissued upload:", err)
	}
	if err := f.Store.ReadyPhoto(t.Context(), memory.MemorySessionID, without.ID, memory.MemoryExpiresAt); !errors.Is(err, store.ErrConflict) {
		t.Fatal("expired memory completed upload:", err)
	}
}

func TestPhotoUploadReissueReadyAndFence(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	memory := fixtureExperience(t, f, now)
	p := createPhoto(t, f, photoInput(memory.MemorySessionID, "reissued", now), now)
	for attempt := 2; attempt <= 5; attempt++ {
		at := now.Add(time.Duration(attempt) * time.Second)
		expires := at.Add(5 * time.Minute)
		renewed, err := f.Store.RenewPhotoUpload(t.Context(), memory.MemorySessionID, p.ID, expires, at)
		if err != nil || renewed.UploadAttempts != attempt || !renewed.UploadExpiresAt.Equal(expires) || renewed.StageKey != p.StageKey || renewed.SequenceNo != p.SequenceNo {
			t.Fatal("upload reissue mutated stable metadata or attempts:", err)
		}
		p = renewed
	}
	if _, err := f.Store.RenewPhotoUpload(t.Context(), memory.MemorySessionID, p.ID, now.Add(6*time.Minute), now.Add(time.Minute)); !errors.Is(err, store.ErrUploadLimit) {
		t.Fatal("sixth upload attempt accepted:", err)
	}
	if err := f.Store.ReadyPhoto(t.Context(), memory.MemorySessionID, p.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.ReadyPhoto(t.Context(), memory.MemorySessionID, p.ID, now.Add(2*time.Minute)); err != nil {
		t.Fatal("ready repeat was not idempotent:", err)
	}
	if _, err := f.Store.RenewPhotoUpload(t.Context(), memory.MemorySessionID, p.ID, now.Add(6*time.Minute), now.Add(time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatal("ready photo reissued upload:", err)
	}
	if fences, err := f.Store.ExpiredPhotoFences(t.Context(), p.UploadExpiresAt.Add(-time.Microsecond), 10); err != nil || len(fences) != 0 {
		t.Fatal("upload fence expired before last capability:", err)
	}
	fences, err := f.Store.ExpiredPhotoFences(t.Context(), p.UploadExpiresAt, 10)
	if err != nil || len(fences) != 1 || fences[0].Key != p.StageKey || !fences[0].NotBefore.Equal(p.UploadExpiresAt) {
		t.Fatal("ready photo did not durably queue stage reclamation:", err)
	}
	summary, err := f.Store.PhotoSummary(t.Context(), memory.MemorySessionID)
	if err != nil || summary.Ready != 1 || summary.Pending != 0 {
		t.Fatal("ready summary:", err)
	}
	extended := p.UploadExpiresAt.Add(time.Minute)
	if _, err := f.SQL.ExecContext(t.Context(), "UPDATE photo_upload_fences SET not_before=$1 WHERE object_key=$2", extended, p.StageKey); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.ReadyPhoto(t.Context(), memory.MemorySessionID, p.ID, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.QueuePhotoDeletion(t.Context(), memory.MemorySessionID, p.ID, "deleted", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.FinishPhotoDeletion(t.Context(), memory.MemorySessionID, p.ID); err != nil {
		t.Fatal(err)
	}
	if fences, err := f.Store.ExpiredPhotoFences(t.Context(), p.UploadExpiresAt, 10); err != nil || len(fences) != 0 {
		t.Fatal("ready/deletion upsert shortened an existing fence:", err)
	}
	fences, err = f.Store.ExpiredPhotoFences(t.Context(), extended, 10)
	if err != nil || len(fences) != 1 || !fences[0].NotBefore.Equal(extended) {
		t.Fatal("fence upsert did not preserve GREATEST lifetime:", err)
	}
}

func TestPhotoDeletionRetainsKnowledgeUntilErased(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	memory := fixtureExperience(t, f, now)
	input := photoInput(memory.MemorySessionID, "invalid", now)
	input.Location = &store.PhotoLocation{Latitude: 35, Longitude: 139, AccuracyM: 10, RecordedAt: now}
	p := createPhoto(t, f, input, now)
	if err := f.Store.DeleteMemory(t.Context(), memory.MemorySessionID); !errors.Is(err, store.ErrPhotoPending) {
		t.Fatal("memory deletion discarded cloud cleanup knowledge:", err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), "DELETE FROM memory_sessions WHERE id=$1", memory.MemorySessionID); err == nil {
		t.Fatal("database foreign key cascaded photo cleanup knowledge")
	}
	if err := f.Store.FinishPhotoDeletion(t.Context(), memory.MemorySessionID, p.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatal("unrequested photo deletion finished:", err)
	}
	if err := f.Store.QueuePhotoDeletion(t.Context(), memory.MemorySessionID, p.ID, "failed", "invalid_image"); err != nil {
		t.Fatal(err)
	}
	// An external storage failure deliberately does not call FinishPhotoDeletion.
	pending, err := f.Store.PendingPhotoDeletions(t.Context(), 1)
	if err != nil || len(pending) != 1 || pending[0].StageKey != p.StageKey || pending[0].ObjectKey != p.ObjectKey || pending[0].Location == nil || pending[0].FailureCode != "invalid_image" || pending[0].DeleteTarget != "failed" {
		t.Fatal("failed external erasure lost retry metadata:", err)
	}
	if err := f.Store.ReadyPhoto(t.Context(), memory.MemorySessionID, p.ID, now); !errors.Is(err, store.ErrConflict) {
		t.Fatal("deleting photo became ready:", err)
	}
	if _, err := f.Store.RenewPhotoUpload(t.Context(), memory.MemorySessionID, p.ID, now.Add(time.Minute), now); !errors.Is(err, store.ErrConflict) {
		t.Fatal("deleting photo reissued upload:", err)
	}
	if err := f.Store.FinishPhotoDeletion(t.Context(), memory.MemorySessionID, p.ID); err != nil {
		t.Fatal(err)
	}
	failed, err := f.Store.Photo(t.Context(), memory.MemorySessionID, p.ID)
	if err != nil || failed.Status != "failed" || failed.Location != nil || failed.StageKey != "" || failed.ObjectKey != "" || failed.FailureCode != "invalid_image" || failed.DeleteTarget != "" {
		t.Fatal("failed photo retained GPS or cloud object keys:", err)
	}
	if err := f.Store.ReadyPhoto(t.Context(), memory.MemorySessionID, p.ID, now); !errors.Is(err, store.ErrConflict) {
		t.Fatal("failed photo became ready:", err)
	}
	if err := f.Store.DeleteMemory(t.Context(), memory.MemorySessionID); !errors.Is(err, store.ErrPhotoPending) {
		t.Fatal("failed photo escaped memory deletion guard:", err)
	}
	if err := f.Store.QueueMemoryPhotoDeletion(t.Context(), memory.MemorySessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.QueuePhotoDeletion(t.Context(), memory.MemorySessionID, p.ID, "failed", "late_failure"); err != nil {
		t.Fatal(err)
	}
	queued, err := f.Store.Photo(t.Context(), memory.MemorySessionID, p.ID)
	if err != nil || queued.DeleteTarget != "deleted" {
		t.Fatal("late failure downgraded explicit erasure:", err)
	}
	m, err := f.Store.MemorySession(t.Context(), memory.MemorySessionID)
	if err != nil || !m.Deleting {
		t.Fatal("memory deletion did not fence future uploads:", err)
	}
	if _, err := f.Store.CreatePhoto(t.Context(), photoInput(memory.MemorySessionID, "blocked", now), now); !errors.Is(err, store.ErrConflict) {
		t.Fatal("deleting memory accepted registration:", err)
	}
	if err := f.Store.FinishPhotoDeletion(t.Context(), memory.MemorySessionID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.DeleteMemory(t.Context(), memory.MemorySessionID); err != nil {
		t.Fatal("erased photo blocked memory deletion:", err)
	}
	if _, err := f.Store.Photo(t.Context(), memory.MemorySessionID, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("erased metadata survived:", err)
	}
	fences, err := f.Store.ExpiredPhotoFences(t.Context(), p.UploadExpiresAt, 10)
	if err != nil || len(fences) != 1 || fences[0].Key != p.StageKey {
		t.Fatal("fence outbox was coupled to deleted memory:", err)
	}
	if required, err := f.Store.PhotoStorageRequired(t.Context()); err != nil || !required {
		t.Fatal("orphan fence did not require storage:", err)
	}
	for range 2 {
		if err := f.Store.RemovePhotoFence(t.Context(), p.StageKey); err != nil {
			t.Fatal("fence removal is not idempotent:", err)
		}
	}
	if required, err := f.Store.PhotoStorageRequired(t.Context()); err != nil || required {
		t.Fatal("empty database still requires photo storage:", err)
	}
}

func TestPhotoExpiryRetainsMemoryAndKeysUntilDeletion(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	memory := fixtureExperience(t, f, now)
	p := createPhoto(t, f, photoInput(memory.MemorySessionID, "expires", now), now)
	if err := f.Store.Cleanup(t.Context(), memory.MemoryDeleteAfter.Add(-time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	before, err := f.Store.Photo(t.Context(), memory.MemorySessionID, p.ID)
	if err != nil || before.Status != "pending" {
		t.Fatal("photo erased before retention deadline:", err)
	}
	for range 2 {
		if err := f.Store.Cleanup(t.Context(), memory.MemoryDeleteAfter); err != nil {
			t.Fatal(err)
		}
	}
	m, err := f.Store.MemorySession(t.Context(), memory.MemorySessionID)
	if err != nil || !m.Deleting {
		t.Fatal("expired memory disappeared before cloud erasure:", err)
	}
	pending, err := f.Store.PendingPhotoDeletions(t.Context(), 10)
	if err != nil || len(pending) != 1 || pending[0].Status != "deleting" || pending[0].DeleteTarget != "deleted" || pending[0].StageKey != p.StageKey || pending[0].ObjectKey != p.ObjectKey {
		t.Fatal("expiry did not preserve object deletion retry metadata:", err)
	}
	if err := f.Store.AuthenticateMemory(t.Context(), memory.MemorySessionID, memory.MemoryTokenHash, memory.MemoryDeleteAfter); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("retained cleanup row kept expired authentication alive:", err)
	}
	if err := f.Store.FinishPhotoDeletion(t.Context(), memory.MemorySessionID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.Cleanup(t.Context(), memory.MemoryDeleteAfter); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.MemorySession(t.Context(), memory.MemorySessionID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("expired empty memory not erased:", err)
	}
	fences, err := f.Store.ExpiredPhotoFences(t.Context(), memory.MemoryDeleteAfter, 10)
	if err != nil || len(fences) != 1 || fences[0].Key != p.StageKey {
		t.Fatal("expiry cleanup removed independent fence outbox:", err)
	}
}

func TestPhotoNestedTransactionSerializesAndRollsBack(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	memory := fixtureExperience(t, f, now)
	p := photoInput(memory.MemorySessionID, "rollback", now)
	rollback := errors.New("intentional rollback")
	if err := f.Store.Transaction(t.Context(), memory.MemorySessionID, func(tx *store.Store) error {
		if _, err := tx.CreatePhoto(t.Context(), p, now); err != nil {
			return err
		}
		if listed, err := tx.Photos(t.Context(), memory.MemorySessionID); err != nil || len(listed) != 1 {
			return errors.New("transaction read did not use its executor")
		}
		if err := tx.ReadyPhoto(t.Context(), memory.MemorySessionID, p.ID, now); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal("nested transaction:", err)
	}
	if photos, err := f.Store.Photos(t.Context(), memory.MemorySessionID); err != nil || len(photos) != 0 {
		t.Fatal("rolled-back metadata escaped transaction:", err)
	}
	if fences, err := f.Store.ExpiredPhotoFences(t.Context(), now.Add(time.Hour), 10); err != nil || len(fences) != 0 {
		t.Fatal("rolled-back ready fence escaped transaction:", err)
	}
	created := createPhoto(t, f, p, now)
	if created.SequenceNo != 1 {
		t.Fatal("rolled-back sequence increment committed")
	}
	if err := f.Store.Transaction(t.Context(), "unrelated-outer-key", func(tx *store.Store) error {
		if err := tx.QueueMemoryPhotoDeletion(t.Context(), memory.MemorySessionID); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := f.Store.CreatePhoto(ctx, photoInput(memory.MemorySessionID, "racing", now), now)
			result <- err
		}()
		if err := <-result; err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("nested memory transaction failed to serialize a concurrent registration")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.CreatePhoto(t.Context(), photoInput(memory.MemorySessionID, "after-delete-intent", now), now); !errors.Is(err, store.ErrConflict) {
		t.Fatal("registration accepted after serialized deletion intent:", err)
	}
}

func TestPhotoConcurrentClientRetries(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	memory := fixtureExperience(t, f, now)
	type result struct {
		photo store.Photo
		err   error
	}
	results := make(chan result, 12)
	var workers sync.WaitGroup
	for i := range 12 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			p := photoInput(memory.MemorySessionID, fmt.Sprintf("retry-%02d", i), now)
			p.ClientPhotoID, p.RequestHash = "one-client-photo", "one-request"
			created, err := f.Store.CreatePhoto(t.Context(), p, now)
			results <- result{created, err}
		}()
	}
	workers.Wait()
	close(results)
	var firstID string
	for result := range results {
		if result.err != nil {
			t.Fatal("concurrent client retry:", result.err)
		}
		if firstID == "" {
			firstID = result.photo.ID
		}
		if result.photo.ID != firstID || result.photo.SequenceNo != 1 {
			t.Fatal("concurrent retries registered different logical photos")
		}
	}
	if photos, err := f.Store.Photos(t.Context(), memory.MemorySessionID); err != nil || len(photos) != 1 {
		t.Fatal("concurrent retries consumed multiple quota slots:", err)
	}
}

func TestExperienceDeletionCannotDiscardPhotoCleanup(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	memory := fixtureExperience(t, f, now)
	p := createPhoto(t, f, photoInput(memory.MemorySessionID, "experience-photo", now), now)
	if err := f.Store.DeleteExperiencePart(t.Context(), memory.ExperienceID, memory.MemoryTokenHash, memory.LiveTokenHash, "receipt", false, now); err != nil {
		t.Fatal("live half deletion:", err)
	}
	if err := f.Store.DeleteExperiencePart(t.Context(), memory.ExperienceID, memory.MemoryTokenHash, memory.LiveTokenHash, "receipt", true, now); !errors.Is(err, store.ErrPhotoPending) {
		t.Fatal("experience deletion bypassed outstanding-photo guard:", err)
	}
	if _, err := f.Store.Photo(t.Context(), memory.MemorySessionID, p.ID); err != nil {
		t.Fatal("blocked experience deletion lost photo knowledge:", err)
	}
	if err := f.Store.QueueMemoryPhotoDeletion(t.Context(), memory.MemorySessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.FinishPhotoDeletion(t.Context(), memory.MemorySessionID, p.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.Store.DeleteExperiencePart(t.Context(), memory.ExperienceID, memory.MemoryTokenHash, memory.LiveTokenHash, "receipt", true, now); err != nil {
			t.Fatal("erased experience could not finish/replay deletion:", err)
		}
	}
}
