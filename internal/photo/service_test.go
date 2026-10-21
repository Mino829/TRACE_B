package photo

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/photoasset"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

type erasureObjects struct {
	photoasset.Storage
	objects map[string][]byte
}

func (o *erasureObjects) Fence(_ context.Context, key string) error {
	o.objects[key] = nil
	return nil
}
func (o *erasureObjects) Delete(_ context.Context, key string) error {
	delete(o.objects, key)
	return nil
}

func TestStaleDeletionSnapshotCannotRecreateAnUntrackedFence(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := f.Store.CreateExperience(t.Context(), store.NewExperience{
		ExperienceID: "exp_cleanup", MemorySessionID: "mem_cleanup", MemoryTokenHash: "memory-hash",
		MemoryConsent: "consent", ConsentLanguage: "ja", DisplayColor: "cyan", DisplaySymbol: "circle",
		CreatedAt: now.Add(-time.Minute), MemoryExpiresAt: now.Add(time.Hour), MemoryDeleteAfter: now.Add(2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	p, err := f.Store.CreatePhoto(t.Context(), store.Photo{
		ID: "photo_cleanup", MemorySessionID: "mem_cleanup", ClientPhotoID: "client-photo", RequestHash: "request-hash",
		CapturedAt: now, UploadExpiresAt: now.Add(5 * time.Minute), MIMEType: "image/jpeg", SHA256: strings.Repeat("a", 64),
		SizeBytes: 13, Width: 1, Height: 1, StageKey: "uploads/cleanup", ObjectKey: "photos/cleanup",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	objects := &erasureObjects{objects: map[string][]byte{p.StageKey: []byte("private bytes"), p.ObjectKey: []byte("private bytes")}}
	service := New(f.Store, objects)
	if err := f.Store.QueuePhotoDeletion(t.Context(), p.MemorySessionID, p.ID, "deleted", ""); err != nil {
		t.Fatal(err)
	}
	// Two workers can have read the same deletion intent before either finishes.
	snapshot, err := f.Store.Photo(t.Context(), p.MemorySessionID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.erase(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := service.Purge(t.Context(), now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.DeleteMemory(t.Context(), p.MemorySessionID); err != nil {
		t.Fatal(err)
	}
	if err := service.erase(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	if len(objects.objects) != 0 {
		t.Fatal("stale work recreated an orphaned cloud object")
	}
	if required, err := f.Store.PhotoStorageRequired(t.Context()); err != nil || required {
		t.Fatal("completed erasure retained cleanup state", err)
	}
}
