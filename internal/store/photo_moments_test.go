package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/route"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func deviceExperience(t *testing.T, f *testdb.Fixture, now time.Time) store.NewExperience {
	return deviceExperienceWithRetention(t, f, now, time.Hour, 2*time.Hour)
}

func deviceExperienceWithRetention(t *testing.T, f *testdb.Fixture, now time.Time, lifetime, retention time.Duration) store.NewExperience {
	t.Helper()
	v := store.NewExperience{ExperienceID: "device-exp", MemorySessionID: "device-memory", MemoryTokenHash: "device-token",
		MemoryConsent: "memory-v1", ConsentLanguage: "ja", PhotoMode: store.PhotoModeDevice,
		DisplayColor: "cyan", DisplaySymbol: "circle", CampusGraphVersion: "g1", CreatedAt: now,
		MemoryExpiresAt: now.Add(lifetime), MemoryDeleteAfter: now.Add(retention)}
	if err := f.Store.CreateExperience(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	return v
}

func momentInput(id, memoryID, checkpoint string, captured time.Time) store.PhotoMoment {
	return store.PhotoMoment{ID: id, MemorySessionID: memoryID, ClientPhotoID: "client-" + id,
		RequestHash: "request-" + id, CheckpointID: checkpoint, CapturedAt: captured}
}

func TestPhotoMomentsRoutesReplayFenceAndSeparation(t *testing.T) {
	f := testdb.Open(t)
	checkpointFixture(t, f)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := deviceExperience(t, f, now)
	id := v.MemorySessionID
	memory, err := f.Store.MemorySession(t.Context(), id)
	if err != nil || memory.PhotoMode != store.PhotoModeDevice {
		t.Fatalf("device mode did not round-trip: %+v %v", memory, err)
	}
	if _, err := f.Store.CreatePhoto(t.Context(), photoInput(id, "legacy-rejected", now), now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("legacy photo accepted in device mode: %v", err)
	}
	if _, _, err := f.Store.CreatePhotoMoment(t.Context(), momentInput("missing", id, "mid", now), now); !errors.Is(err, store.ErrInvalidCheckpoint) {
		t.Fatalf("waypoint accepted as photo checkpoint: %v", err)
	}
	first, created, err := f.Store.CreatePhotoMoment(t.Context(), momentInput("first", id, "cp-a", now.Add(time.Minute)), now)
	if err != nil || !created || first.SequenceNo != 1 || first.CampusGraphVersion != "g1" {
		t.Fatalf("initial metadata: %+v %t %v", first, created, err)
	}
	momentDigest := sha256.Sum256([]byte(first.ID))
	byHash, err := f.Store.PhotoMomentByHash(t.Context(), id, hex.EncodeToString(momentDigest[:]))
	if err != nil || byHash.ID != first.ID {
		t.Fatalf("hashed receipt lookup: %+v %v", byHash, err)
	}
	replay := momentInput("different-server-id", id, "cp-a", now.Add(time.Minute))
	replay.ClientPhotoID, replay.RequestHash = first.ClientPhotoID, first.RequestHash
	replayed, inserted, err := f.Store.CreatePhotoMoment(t.Context(), replay, now)
	if err != nil || inserted || replayed.ID != first.ID || replayed.SequenceNo != 1 {
		t.Fatalf("same client request not stable: %+v %t %v", replayed, inserted, err)
	}
	replay.RequestHash = "changed"
	if _, _, err := f.Store.CreatePhotoMoment(t.Context(), replay, now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed client request accepted: %v", err)
	}
	second, _, err := f.Store.CreatePhotoMoment(t.Context(), momentInput("second", id, "cp-b", now), now)
	if err != nil || second.SequenceNo != 2 {
		t.Fatalf("out-of-order capture: %+v %v", second, err)
	}
	moments, err := f.Store.PhotoMoments(t.Context(), id)
	if err != nil || len(moments) != 2 || moments[0].ID != second.ID || moments[1].ID != first.ID {
		t.Fatalf("capture ordering: %+v %v", moments, err)
	}
	segments, err := f.Store.MomentRouteSegments(t.Context(), id)
	if err != nil || len(segments) != 1 || segments[0].FromPhotoMomentID != second.ID ||
		segments[0].ToPhotoMomentID == nil || *segments[0].ToPhotoMomentID != first.ID ||
		segments[0].AlgorithmVersion != route.CheckpointAlgorithmVersion || len(segments[0].PathNodeIDs) != 3 {
		t.Fatalf("checkpoint route: %+v %v", segments, err)
	}
	if err := f.Store.DeletePhotoMoment(t.Context(), id, second.ID, now); err != nil {
		t.Fatal(err)
	}
	if segments, err = f.Store.MomentRouteSegments(t.Context(), id); err != nil || len(segments) != 0 {
		t.Fatalf("delete left route references: %+v %v", segments, err)
	}
	deletedDigest := sha256.Sum256([]byte(second.ID))
	if _, err := f.Store.PhotoMomentByHash(t.Context(), id, hex.EncodeToString(deletedDigest[:])); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted photo still resolves: %v", err)
	}
	deletedReplay := momentInput("new-server-id", id, "cp-a", now)
	deletedReplay.ClientPhotoID = second.ClientPhotoID
	if _, _, err := f.Store.CreatePhotoMoment(t.Context(), deletedReplay, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted client ID was resurrected: %v", err)
	}
	if err := f.Store.DeletePhotoMoment(t.Context(), id, second.ID, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("repeat deletion unexpectedly succeeded: %v", err)
	}
	third, _, err := f.Store.CreatePhotoMoment(t.Context(), momentInput("third", id, "cp-b", now.Add(2*time.Minute)), now)
	if err != nil || third.SequenceNo != 3 {
		t.Fatalf("deleted sequence reused: %+v %v", third, err)
	}
	if _, err := f.Store.RouteSegments(t.Context(), id); err != nil {
		t.Fatal("legacy route table broken:", err)
	}
	var oldPhotos int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM photos WHERE memory_session_id=$1`, id).Scan(&oldPhotos); err != nil || oldPhotos != 0 {
		t.Fatalf("metadata generated legacy assets: %d %v", oldPhotos, err)
	}
}

func TestConcurrentPhotoMomentQuota(t *testing.T) {
	f := testdb.Open(t)
	checkpointFixture(t, f)
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := deviceExperience(t, f, now).MemorySessionID
	var workers sync.WaitGroup
	results := make(chan error, 24)
	for i := range 24 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, _, err := f.Store.CreatePhotoMoment(t.Context(),
				momentInput(fmt.Sprintf("concurrent-%02d", i), id, "cp-a", now), now)
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
			t.Fatal("concurrent metadata registration:", err)
		}
	}
	if accepted != 12 || limited != 12 {
		t.Fatalf("metadata quota was not serialized: accepted=%d limited=%d", accepted, limited)
	}
}

func TestPhotoMomentQuotaArrivalAndExpiry(t *testing.T) {
	f := testdb.Open(t)
	checkpointFixture(t, f)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := deviceExperience(t, f, now)
	id := v.MemorySessionID
	for i := range 12 {
		p := momentInput(fmt.Sprintf("m-%02d", i), id, "cp-a", now)
		if _, _, err := f.Store.CreatePhotoMoment(t.Context(), p, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.Store.CreatePhotoMoment(t.Context(), momentInput("over-quota", id, "cp-a", now), now); !errors.Is(err, store.ErrPhotoLimit) {
		t.Fatalf("quota not enforced: %v", err)
	}
	if err := f.Store.DeletePhotoMoment(t.Context(), id, "m-00", now); err != nil {
		t.Fatal(err)
	}
	p, _, err := f.Store.CreatePhotoMoment(t.Context(), momentInput("replacement", id, "cp-a", now), now)
	if err != nil || p.SequenceNo != 13 {
		t.Fatalf("quota did not recover while sequence stayed stable: %+v %v", p, err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `INSERT INTO arrival_events
 (id,memory_session_id,source,checkpoint_id,replay_status,arrived_at,photo_wait_until,replay_deadline)
 VALUES ('device-arrived',$1,'qr','cp-b','queued',$2,$2,$3)`, id, now.Add(time.Minute), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.RefreshMomentRoutes(t.Context(), id, now); err != nil {
		t.Fatal(err)
	}
	segments, err := f.Store.MomentRouteSegments(t.Context(), id)
	if err != nil || len(segments) != 12 || segments[len(segments)-1].ToPhotoMomentID != nil {
		t.Fatalf("arrival extension missing: %+v %v", segments, err)
	}
	candidates, err := f.Store.ReplayCandidates(t.Context(), now)
	if err != nil || len(candidates) != 1 || candidates[0].ReadyPhotoCount != 12 {
		t.Fatalf("renderer metadata count: %+v %v", candidates, err)
	}
	if _, _, err := f.Store.CreatePhotoMoment(t.Context(), momentInput("expired", id, "cp-b", now), v.MemoryExpiresAt); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("created beyond session expiry: %v", err)
	}
	if err := f.Store.Cleanup(t.Context(), v.MemoryDeleteAfter); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"photo_moments", "photo_moment_fences", "moment_route_segments", "memory_sessions"} {
		var count int
		if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s survived expiry: %d %v", table, count, err)
		}
	}
}

func TestDeletedMomentFenceSurvivesReceiptLifetime(t *testing.T) {
	f := testdb.Open(t)
	checkpointFixture(t, f)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := deviceExperienceWithRetention(t, f, now, 48*time.Hour, 49*time.Hour)
	p := momentInput("offline-replay", v.MemorySessionID, "cp-a", now)
	if _, _, err := f.Store.CreatePhotoMoment(t.Context(), p, now); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.DeletePhotoMoment(t.Context(), v.MemorySessionID, p.ID, now); err != nil {
		t.Fatal(err)
	}
	retryAt := now.Add(25 * time.Hour)
	if _, _, err := f.Store.CreatePhotoMoment(t.Context(), p, retryAt); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted ID revived after receipt lifetime: %v", err)
	}
	p.ID = "new-server-id"
	if _, _, err := f.Store.CreatePhotoMoment(t.Context(), p, retryAt); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted client ID revived after receipt lifetime: %v", err)
	}
	if err := f.Store.Cleanup(t.Context(), v.MemoryDeleteAfter); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM photo_moment_fences`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted client fence survived its memory retention: %d %v", count, err)
	}
}

func TestPhotoMomentGraphRetirementDoesNotInventRoute(t *testing.T) {
	f := testdb.Open(t)
	checkpointFixture(t, f)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := deviceExperience(t, f, now)
	id := v.MemorySessionID
	if _, _, err := f.Store.CreatePhotoMoment(t.Context(), momentInput("one", id, "cp-a", now), now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Store.CreatePhotoMoment(t.Context(), momentInput("original-second", id, "cp-b", now), now); err != nil {
		t.Fatal(err)
	}
	if segments, err := f.Store.MomentRouteSegments(t.Context(), id); err != nil || len(segments) != 1 {
		t.Fatalf("original graph missing route: %+v %v", segments, err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET campus_graph_version='retired' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Store.CreatePhotoMoment(t.Context(), momentInput("two", id, "cp-b", now), now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("retired graph accepted new photo: %v", err)
	}
	if err := f.Store.RefreshMomentRoutes(t.Context(), id, now); err != nil {
		t.Fatal(err)
	}
	if segments, err := f.Store.MomentRouteSegments(t.Context(), id); err != nil || len(segments) != 0 {
		t.Fatalf("retired graph generated routes: %+v %v", segments, err)
	}
}

func TestDeviceArrivalReconcilesZeroAndOnePhoto(t *testing.T) {
	f := testdb.Open(t)
	checkpointFixture(t, f)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := deviceExperience(t, f, now)
	id := v.MemorySessionID
	if _, err := f.Store.AcceptArrival(t.Context(), id, "", "arrival-1", "cp-b", "receipt-1", "hash-1", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	segments, err := f.Store.MomentRouteSegments(t.Context(), id)
	if err != nil || len(segments) != 0 {
		t.Fatalf("zero photos invented arrival route: %+v %v", segments, err)
	}
	m, _, err := f.Store.CreatePhotoMoment(t.Context(), momentInput("first", id, "cp-a", now), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	segments, err = f.Store.MomentRouteSegments(t.Context(), id)
	if err != nil || len(segments) != 1 || segments[0].FromPhotoMomentID != m.ID ||
		segments[0].ToPhotoMomentID != nil || len(segments[0].PathNodeIDs) != 3 {
		t.Fatalf("one photo missing arrival route: %+v %v", segments, err)
	}
	if err := f.Store.DeletePhotoMoment(t.Context(), id, m.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if segments, err := f.Store.MomentRouteSegments(t.Context(), id); err != nil || len(segments) != 0 {
		t.Fatalf("deleted last photo left arrival route: %+v %v", segments, err)
	}
}

func TestDeviceCutoverQueuesPendingButRetainsReadyLegacy(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := fixtureExperience(t, f, now)
	if _, _, err := f.Store.CreatePhotoMoment(t.Context(), momentInput("wrong-mode", v.MemorySessionID, "cp-a", now), now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("legacy session accepted metadata: %v", err)
	}
	ready := createPhoto(t, f, photoInput(v.MemorySessionID, "ready-legacy", now), now)
	pending := createPhoto(t, f, photoInput(v.MemorySessionID, "pending-legacy", now), now)
	if err := f.Store.ReadyPhoto(t.Context(), v.MemorySessionID, ready.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.QueueLegacyPendingPhotoUploads(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.QueueLegacyPendingPhotoUploads(t.Context()); err != nil {
		t.Fatal("repeat cutover:", err)
	}
	readyResult, err := f.Store.Photo(t.Context(), v.MemorySessionID, ready.ID)
	if err != nil || readyResult.Status != "ready" {
		t.Fatalf("old ready asset was removed: %+v %v", readyResult, err)
	}
	queued, err := f.Store.PendingPhotoDeletions(t.Context(), 10)
	if err != nil || len(queued) != 1 || queued[0].ID != pending.ID {
		t.Fatalf("cutover did not queue pending stage key: %+v %v", queued, err)
	}
	if err := f.Store.FinishPhotoDeletion(t.Context(), v.MemorySessionID, pending.ID); err != nil {
		t.Fatal(err)
	}
	fence, err := f.Store.PhotoFence(t.Context(), pending.StageKey)
	if err != nil || fence.NotBefore.Before(pending.UploadExpiresAt) {
		t.Fatalf("late signed PUT lost its fence: %+v %v", fence, err)
	}
	if _, err := f.Store.Photo(t.Context(), v.MemorySessionID, ready.ID); err != nil {
		t.Fatal("old ready asset no longer available:", err)
	}
}
