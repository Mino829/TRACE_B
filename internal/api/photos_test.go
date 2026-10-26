package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/photo"
	"github.com/Mino829/trace_Backend/internal/photoasset"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

// Test-only object service: capabilities exercise actual HTTP PUT/GET while
// generation and deletion failures are controlled deterministically.
type photoObjectServer struct {
	mu                    sync.Mutex
	server                *httptest.Server
	objects               map[string]photoasset.Object
	grants                map[string]photoGrant
	generation            int64
	failDelete, failFence bool
	clock                 time.Time
}
type photoGrant struct {
	key      string
	expires  time.Time
	metadata photoasset.Metadata
	upload   bool
}

func newPhotoObjects(t *testing.T) *photoObjectServer {
	t.Helper()
	a := &photoObjectServer{objects: make(map[string]photoasset.Object), grants: make(map[string]photoGrant)}
	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		grant, ok := a.grants[r.URL.Path]
		now := time.Now()
		if !a.clock.IsZero() {
			now = a.clock
		}
		if !ok || !now.Before(grant.expires) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if grant.upload {
			if r.Method != http.MethodPut || r.Header.Get("x-goog-if-generation-match") != "0" || r.Header.Get("Content-Type") != grant.metadata.MIMEType {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if _, exists := a.objects[grant.key]; exists {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			data, err := io.ReadAll(io.LimitReader(r.Body, photoasset.MaxBytes+1))
			if err != nil || int64(len(data)) != grant.metadata.SizeBytes {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			a.generation++
			a.objects[grant.key] = photoasset.Object{Data: data, Generation: a.generation, ContentType: grant.metadata.MIMEType}
			w.WriteHeader(http.StatusOK)
			return
		}
		object, ok := a.objects[grant.key]
		if r.Method != http.MethodGet || !ok || len(object.Data) == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Type", object.ContentType)
		_, _ = w.Write(object.Data)
	}))
	t.Cleanup(a.server.Close)
	return a
}
func (a *photoObjectServer) UploadURL(_ context.Context, key string, m photoasset.Metadata, expires time.Time) (string, map[string]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	path := fmt.Sprintf("/upload/%d", len(a.grants)+1)
	a.grants[path] = photoGrant{key: key, expires: expires, metadata: m, upload: true}
	return a.server.URL + path, map[string]string{"Content-Type": m.MIMEType, "x-goog-if-generation-match": "0", "Cache-Control": "private, no-store"}, nil
}
func (a *photoObjectServer) Read(_ context.Context, key string, limit int64) (photoasset.Object, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	object, ok := a.objects[key]
	if !ok {
		return photoasset.Object{}, photoasset.ErrNotFound
	}
	if int64(len(object.Data)) > limit {
		return photoasset.Object{}, photoasset.ErrInvalidImage
	}
	return object, nil
}
func (a *photoObjectServer) Promote(_ context.Context, from, to string, generation int64, _ photoasset.Metadata) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.objects[to]; exists {
		return photoasset.ErrAlreadyExists
	}
	object, ok := a.objects[from]
	if !ok || object.Generation != generation {
		return photoasset.ErrNotFound
	}
	a.generation++
	object.Generation = a.generation
	a.objects[to] = object
	return nil
}
func (a *photoObjectServer) Fence(_ context.Context, key string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failFence {
		return photo.ErrStorage
	}
	a.generation++
	a.objects[key] = photoasset.Object{Generation: a.generation, ContentType: "application/octet-stream"}
	return nil
}
func (a *photoObjectServer) Delete(_ context.Context, key string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failDelete {
		return photo.ErrStorage
	}
	delete(a.objects, key)
	return nil
}
func (a *photoObjectServer) ViewURL(_ context.Context, key string, expires time.Time) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	path := fmt.Sprintf("/view/%d", len(a.grants)+1)
	a.grants[path] = photoGrant{key: key, expires: expires}
	return a.server.URL + path, nil
}
func photoJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 12))
	img.Set(2, 3, color.RGBA{R: 90, G: 120, B: 180, A: 255})
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, nil); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

type photoUploadResponse struct {
	ID      string            `json:"photoMomentId"`
	Status  string            `json:"status"`
	URL     string            `json:"uploadUrl"`
	Headers map[string]string `json:"uploadHeaders"`
	Expires time.Time         `json:"uploadExpiresAt"`
}

func registerPhoto(t *testing.T, h http.Handler, c createdExperience, data []byte, clientID string) (photoUploadResponse, string) {
	t.Helper()
	digest := sha256.Sum256(data)
	body := fmt.Sprintf(`{"clientPhotoId":%q,"capturedAt":%q,"mimeType":"image/jpeg","sizeBytes":%d,"width":16,"height":12,"sha256":%q}`, clientID, time.Now().UTC().Format(time.RFC3339Nano), len(data), hex.EncodeToString(digest[:]))
	r := request(t, h, "POST", "/v1/memory-sessions/"+c.Memory.SessionID+"/photos", body, map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken, "Idempotency-Key": "register-" + clientID})
	if r.Code != 201 {
		t.Fatalf("register: %d %s", r.Code, r.Body.String())
	}
	var result photoUploadResponse
	if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result, body
}
func putPhoto(t *testing.T, p photoUploadResponse, data []byte, want int) {
	t.Helper()
	req, err := http.NewRequest("PUT", p.URL, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range p.Headers {
		req.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("upload status %d want %d", response.StatusCode, want)
	}
}
func readyPhoto(t *testing.T, h http.Handler, c createdExperience, data []byte, id string) photoUploadResponse {
	t.Helper()
	p, _ := registerPhoto(t, h, c, data, id)
	putPhoto(t, p, data, 200)
	r := request(t, h, "POST", "/v1/memory-sessions/"+c.Memory.SessionID+"/photos/"+p.ID+"/complete", "", map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken})
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"ready"`) {
		t.Fatalf("complete: %d %s", r.Code, r.Body.String())
	}
	return p
}

func TestPhotoRegistrationRejectsLocationWithoutCreatingPhoto(t *testing.T) {
	f := testdb.Open(t)
	assets := newPhotoObjects(t)
	cfg := config.Default()
	cfg.PhotoEnabled = true
	cfg.RegistrationOpen = true
	h := api.New(cfg, f.Store, photo.New(f.Store, assets))
	c := createExperience(t, h, cfg)
	data := photoJPEG(t)
	digest := sha256.Sum256(data)
	body := fmt.Sprintf(`{"clientPhotoId":"gps-attempt","capturedAt":%q,"mimeType":"image/jpeg","sizeBytes":%d,"width":16,"height":12,"sha256":%q}`, time.Now().UTC().Format(time.RFC3339Nano), len(data), hex.EncodeToString(digest[:]))
	path := "/v1/memory-sessions/" + c.Memory.SessionID + "/photos"
	headers := map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken, "Idempotency-Key": "gps-rejected"}
	for _, location := range []string{
		`{"latitude":35.0,"longitude":135.0,"accuracyM":4,"recordedAt":"2025-01-01T00:00:00Z"}`,
		`{}`,
	} {
		withLocation := strings.TrimSuffix(body, "}") + `,"location":` + location + `}`
		r := request(t, h, "POST", path, withLocation, headers)
		if r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), `"code":"PHOTO_LOCATION_DISABLED"`) || strings.Contains(r.Body.String(), "135.0") {
			t.Fatalf("GPS metadata rejection: %d %s", r.Code, r.Body.String())
		}
		photos, err := f.Store.Photos(t.Context(), c.Memory.SessionID)
		if err != nil || len(photos) != 0 {
			t.Fatalf("GPS metadata created a photo: %+v %v", photos, err)
		}
	}
	withNull := strings.TrimSuffix(body, "}") + `,"location":null}`
	r := request(t, h, "POST", path, withNull, headers)
	if r.Code != http.StatusCreated {
		t.Fatalf("null location failed or rejection consumed idempotency key: %d %s", r.Code, r.Body.String())
	}
	photos, err := f.Store.Photos(t.Context(), c.Memory.SessionID)
	if err != nil || len(photos) != 1 || photos[0].Location != nil {
		t.Fatalf("null location stored unexpected coordinates: %+v %v", photos, err)
	}
	if p, _ := registerPhoto(t, h, c, data, "omitted-location"); p.ID == "" {
		t.Fatal("omitted location registration failed")
	}
}

func TestPhotoUploadStoryAndDeletionCapabilities(t *testing.T) {
	f := testdb.Open(t)
	assets := newPhotoObjects(t)
	cfg := config.Default()
	cfg.PhotoEnabled = true
	cfg.RegistrationOpen = true
	h := api.New(cfg, f.Store, photo.New(f.Store, assets))
	c := createExperience(t, h, cfg)
	other := createExperience(t, h, cfg)
	path := "/v1/memory-sessions/" + c.Memory.SessionID
	headers := map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken}
	story := request(t, h, "GET", path+"/memory", "", headers)
	if story.Code != 200 || !strings.Contains(story.Body.String(), `"moments":[]`) {
		t.Fatalf("zero photos: %s", story.Body.String())
	}
	data := photoJPEG(t)
	p, body := registerPhoto(t, h, c, data, "one")
	if p.Expires.After(time.Now().Add(5 * time.Minute)) {
		t.Fatal("upload capability too long")
	}
	for _, token := range []string{other.Memory.ParticipantToken, "wrong-token"} {
		if got := request(t, h, "GET", path+"/photos", "", map[string]string{"Authorization": "Bearer " + token}).Code; got != 401 {
			t.Fatalf("wrong credentials: %d", got)
		}
	}
	if got := request(t, h, "GET", path+"/memory", "", headers); strings.Contains(got.Body.String(), "imageUrl") {
		t.Fatal("pending photo exposed")
	}
	putPhoto(t, p, data, 200)
	putPhoto(t, p, data, 412)
	for range 2 {
		r := request(t, h, "POST", path+"/photos/"+p.ID+"/complete", "", headers)
		if r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"ready"`) {
			t.Fatalf("complete/replay: %d %s", r.Code, r.Body.String())
		}
	}
	story = request(t, h, "GET", path+"/memory", "", headers)
	var result struct {
		Moments []struct {
			ID         string `json:"photoMomentId"`
			URL        string `json:"imageUrl"`
			Confidence string `json:"locationConfidence"`
		} `json:"moments"`
	}
	if err := json.Unmarshal(story.Body.Bytes(), &result); err != nil || story.Code != 200 || len(result.Moments) != 1 || result.Moments[0].ID != p.ID || result.Moments[0].Confidence != "low" {
		t.Fatalf("story: %d %s", story.Code, story.Body.String())
	}
	response, err := http.Get(result.Moments[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !bytes.Equal(downloaded, data) {
		t.Fatal("verified image unavailable")
	}
	// Represent a photo saved before new GPS registration was disabled.
	recordedAt := time.Now().UTC()
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE photos SET latitude=$1, longitude=$2, accuracy_m=$3, location_recorded_at=$4 WHERE id=$5`,
		35.0, 135.0, 4.0, recordedAt, p.ID); err != nil {
		t.Fatal(err)
	}
	legacy, err := f.Store.Photo(t.Context(), c.Memory.SessionID, p.ID)
	if err != nil || legacy.Location == nil || legacy.Location.Latitude != 35.0 || legacy.Location.Longitude != 135.0 {
		t.Fatalf("preexisting GPS photo was not readable: %+v %v", legacy, err)
	}
	story = request(t, h, "GET", path+"/memory", "", headers)
	if err := json.Unmarshal(story.Body.Bytes(), &result); err != nil || story.Code != 200 || len(result.Moments) != 1 || result.Moments[0].Confidence != "high" {
		t.Fatalf("preexisting GPS photo missing from story: %d %s", story.Code, story.Body.String())
	}
	retry := request(t, h, "POST", path+"/photos", body, map[string]string{"Authorization": headers["Authorization"], "Idempotency-Key": "register-one"})
	if retry.Code != 200 || !strings.Contains(retry.Body.String(), p.ID) || strings.Contains(retry.Body.String(), "uploadUrl") {
		t.Fatalf("metadata replay: %d %s", retry.Code, retry.Body.String())
	}
	var altered map[string]any
	if err := json.Unmarshal([]byte(body), &altered); err != nil {
		t.Fatal(err)
	}
	altered["sha256"] = strings.Repeat("0", 64)
	different, _ := json.Marshal(altered)
	if r := request(t, h, "POST", path+"/photos", string(different), map[string]string{"Authorization": headers["Authorization"], "Idempotency-Key": "register-one"}); r.Code != 409 {
		t.Fatalf("idempotent photo content conflict accepted: %d %s", r.Code, r.Body.String())
	}
	headers["Idempotency-Key"] = "delete-one"
	for range 2 {
		if r := request(t, h, "DELETE", path+"/photos/"+p.ID, "", headers); r.Code != 204 {
			t.Fatalf("delete/replay: %d %s", r.Code, r.Body.String())
		}
	}
	if _, err := f.Store.Photo(t.Context(), c.Memory.SessionID, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("preexisting GPS photo retained after deletion: %v", err)
	}
	response, err = http.Get(result.Moments[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("deleted image still available")
	}
	putPhoto(t, p, data, 412)
	if got := request(t, h, "POST", path+"/photos", body, map[string]string{"Authorization": headers["Authorization"], "Idempotency-Key": "register-one"}).Code; got != 404 {
		t.Fatalf("deleted registration resurrected photo: %d", got)
	}
}

func TestFailedPhotoValidationRetainsDeletionIntentUntilErasure(t *testing.T) {
	f := testdb.Open(t)
	assets := newPhotoObjects(t)
	cfg := config.Default()
	cfg.PhotoEnabled = true
	cfg.RegistrationOpen = true
	service := photo.New(f.Store, assets)
	h := api.New(cfg, f.Store, service)
	c := createExperience(t, h, cfg)
	data := photoJPEG(t)
	exif := []byte{0xff, 0xe1, 0, 10, 'E', 'x', 'i', 'f', 0, 0, 0, 0}
	bad := append(append(append([]byte{}, data[:2]...), exif...), data[2:]...)
	p, _ := registerPhoto(t, h, c, bad, "bad-exif")
	putPhoto(t, p, bad, 200)
	path := "/v1/memory-sessions/" + c.Memory.SessionID
	headers := map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken}
	assets.failFence = true
	if r := request(t, h, "POST", path+"/photos/"+p.ID+"/complete", "", headers); r.Code != 503 {
		t.Fatalf("failed erasure: %d %s", r.Code, r.Body.String())
	}
	stored, err := f.Store.Photo(t.Context(), c.Memory.SessionID, p.ID)
	if err != nil || stored.Status != "deleting" {
		t.Fatalf("cleanup knowledge lost: %+v %v", stored, err)
	}
	assets.failFence = false
	if err := service.Purge(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	stored, err = f.Store.Photo(t.Context(), c.Memory.SessionID, p.ID)
	if err != nil || stored.Status != "failed" || stored.StageKey != "" || stored.ObjectKey != "" || stored.Location != nil {
		t.Fatalf("failed photo retained assets: %+v %v", stored, err)
	}
	if r := request(t, h, "GET", path+"/memory", "", headers); r.Code != 200 || strings.Contains(r.Body.String(), "imageUrl") {
		t.Fatal("invalid image exposed")
	}
	if r := request(t, h, "POST", path+"/photos/"+p.ID+"/upload-url", "", headers); r.Code != 409 {
		t.Fatal("failed photo allowed reupload")
	}
}

func TestMemoryAndExperienceDeletionRetryPhotoErasure(t *testing.T) {
	for _, combined := range []bool{false, true} {
		t.Run(fmt.Sprint(combined), func(t *testing.T) {
			f := testdb.Open(t)
			assets := newPhotoObjects(t)
			cfg := config.Default()
			cfg.PhotoEnabled = true
			cfg.RegistrationOpen = true
			h := api.New(cfg, f.Store, photo.New(f.Store, assets))
			var c createdExperience
			if combined {
				c = seedLegacyExperience(t, f.Store, cfg)
			} else {
				c = createExperience(t, h, cfg)
			}
			p := readyPhoto(t, h, c, photoJPEG(t), "delete-session")
			path := "/v1/memory-sessions/" + c.Memory.SessionID
			headers := map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken, "Idempotency-Key": "delete-session"}
			if combined {
				path = "/v1/experiences/" + c.ExperienceID
				headers["X-Trace-Live-Token"] = c.Live.ParticipantToken
			}
			assets.failDelete = true
			r := request(t, h, "DELETE", path, "", headers)
			if (!combined && r.Code != 503) || (combined && (r.Code != 200 || !strings.Contains(r.Body.String(), `"memory":"pending"`) || !strings.Contains(r.Body.String(), `"live":"deleted"`))) {
				t.Fatalf("deletion failure contract: %d %s", r.Code, r.Body.String())
			}
			if err := f.Store.AuthenticateMemory(t.Context(), c.Memory.SessionID, tokenHash(c.Memory.ParticipantToken), time.Now()); err != nil {
				t.Fatal("memory removed before object erasure")
			}
			if photo, err := f.Store.Photo(t.Context(), c.Memory.SessionID, p.ID); err != nil || photo.Status != "deleting" {
				t.Fatal("photo erasure intent lost")
			}
			assets.failDelete = false
			r = request(t, h, "DELETE", path, "", headers)
			if (!combined && r.Code != 204) || (combined && (r.Code != 200 || !strings.Contains(r.Body.String(), `"complete":true`))) {
				t.Fatalf("deletion retry: %d %s", r.Code, r.Body.String())
			}
			if err := f.Store.AuthenticateMemory(t.Context(), c.Memory.SessionID, tokenHash(c.Memory.ParticipantToken), time.Now()); err == nil {
				t.Fatal("memory survived completed deletion")
			}
			putPhoto(t, p, photoJPEG(t), 412)
		})
	}
}

func TestExpiredMemoryErasesPhotosBeforeLosingCleanupKnowledge(t *testing.T) {
	f := testdb.Open(t)
	assets := newPhotoObjects(t)
	cfg := config.Default()
	cfg.PhotoEnabled = true
	cfg.RegistrationOpen = true
	service := photo.New(f.Store, assets)
	h := api.New(cfg, f.Store, service)
	c := createExperience(t, h, cfg)
	p := readyPhoto(t, h, c, photoJPEG(t), "expire")
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE photos SET latitude=$1, longitude=$2, accuracy_m=$3, location_recorded_at=$4 WHERE id=$5`,
		35.0, 135.0, 4.0, time.Now().UTC(), p.ID); err != nil {
		t.Fatal(err)
	}
	if legacy, err := f.Store.Photo(t.Context(), c.Memory.SessionID, p.ID); err != nil || legacy.Location == nil {
		t.Fatalf("preexisting GPS photo missing before expiry: %+v %v", legacy, err)
	}
	future := c.Memory.DeleteAfter.Add(time.Second)
	if err := f.Store.Cleanup(t.Context(), future); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.MemorySession(t.Context(), c.Memory.SessionID); err != nil {
		t.Fatal("expired session lost photo cleanup knowledge")
	}
	assets.clock = future
	if err := service.Purge(t.Context(), future); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.Cleanup(t.Context(), future); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.MemorySession(t.Context(), c.Memory.SessionID); err == nil {
		t.Fatal("expired memory retained")
	}
	if _, err := f.Store.Photo(t.Context(), c.Memory.SessionID, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired GPS photo retained: %v", err)
	}
	assets.mu.Lock()
	remaining := len(assets.objects)
	assets.mu.Unlock()
	if remaining != 0 {
		t.Fatal("photo bytes or expired upload fences retained")
	}
	putPhoto(t, p, photoJPEG(t), 403)
}

func TestPhotoCompleteRecoversAfterReadyCommitFailure(t *testing.T) {
	f := testdb.Open(t)
	assets := newPhotoObjects(t)
	cfg := config.Default()
	cfg.PhotoEnabled = true
	cfg.RegistrationOpen = true
	h := api.New(cfg, f.Store, photo.New(f.Store, assets))
	c := createExperience(t, h, cfg)
	data := photoJPEG(t)
	p, _ := registerPhoto(t, h, c, data, "crash-retry")
	putPhoto(t, p, data, 200)
	if _, err := f.SQL.ExecContext(t.Context(), `CREATE FUNCTION reject_ready_photo() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.status='ready' THEN RAISE EXCEPTION 'simulated commit failure'; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `CREATE TRIGGER reject_ready_photo BEFORE UPDATE ON photos FOR EACH ROW EXECUTE FUNCTION reject_ready_photo()`); err != nil {
		t.Fatal(err)
	}
	path := "/v1/memory-sessions/" + c.Memory.SessionID + "/photos/" + p.ID + "/complete"
	headers := map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken}
	if r := request(t, h, "POST", path, "", headers); r.Code != 503 {
		t.Fatalf("ready commit failure: %d %s", r.Code, r.Body.String())
	}
	stored, err := f.Store.Photo(t.Context(), c.Memory.SessionID, p.ID)
	if err != nil || stored.Status != "pending" {
		t.Fatal("DB failure published ready state")
	}
	if _, err := f.SQL.ExecContext(t.Context(), "DROP TRIGGER reject_ready_photo ON photos"); err != nil {
		t.Fatal(err)
	}
	if r := request(t, h, "POST", path, "", headers); r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"ready"`) {
		t.Fatalf("crash retry lost promoted image: %d %s", r.Code, r.Body.String())
	}
	if photos, err := f.Store.Photos(t.Context(), c.Memory.SessionID); err != nil || len(photos) != 1 || photos[0].ID != p.ID {
		t.Fatal("complete retry duplicated photo")
	}
}

func TestPhotoMetadataLimitDoesNotBlockControlRequests(t *testing.T) {
	f := testdb.Open(t)
	assets := newPhotoObjects(t)
	cfg := config.Default()
	cfg.PhotoEnabled = true
	cfg.RegistrationOpen = true
	h := api.New(cfg, f.Store, photo.New(f.Store, assets))
	c := createExperience(t, h, cfg)
	var body string
	for i := range 6 {
		_, body = registerPhoto(t, h, c, photoJPEG(t), fmt.Sprintf("burst-%d", i))
	}
	headers := map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken, "Idempotency-Key": "burst-retry"}
	r := request(t, h, "POST", "/v1/memory-sessions/"+c.Memory.SessionID+"/photos", body, headers)
	if r.Code != 429 || r.Header().Get("Retry-After") == "" {
		t.Fatalf("metadata limit: %d %s", r.Code, r.Body.String())
	}
	state := request(t, h, "GET", "/v1/experiences/"+c.ExperienceID, "", headers)
	var result struct {
		Summary struct {
			Pending int `json:"pending"`
		} `json:"photoSummary"`
	}
	if err := json.Unmarshal(state.Body.Bytes(), &result); err != nil || state.Code != 200 || result.Summary.Pending != 6 {
		t.Fatalf("metadata burst blocked control or miscounted photos: %d %s", state.Code, state.Body.String())
	}
}
