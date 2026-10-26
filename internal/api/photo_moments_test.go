package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/photo"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func devicePhotoBody(clientID, checkpoint string, captured time.Time) string {
	return fmt.Sprintf(`{"clientPhotoId":%q,"checkpointId":%q,"capturedAt":%q}`, clientID, checkpoint, captured.Format(time.RFC3339Nano))
}

func TestDevicePhotoMetadataAuthValidationIdempotencyAndRoutes(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	cfg.PhotoMetadataEnabled = true
	cfg.RegistrationOpen = true
	photoGraphFixture(t, f, &cfg)
	h := api.New(cfg, f.Store, nil)
	owner := createExperience(t, h, cfg)
	other := createExperience(t, h, cfg)
	base := "/v2/memory-sessions/" + owner.Memory.SessionID
	ownerHeaders := map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": "moment-create"}
	// Use a fresh limiter for each independent scenario; the production limiter
	// still applies to every /v2 request via the photo-metadata quota.
	send := func(method, path, body string, headers map[string]string) *momentHTTPResult {
		response := request(t, api.New(cfg, f.Store, nil), method, path, body, headers)
		return &momentHTTPResult{status: response.Code, body: response.Body.String()}
	}
	for _, path := range []string{base + "/photos", base + "/memory"} {
		if result := send("GET", path, "", nil); result.status != 401 {
			t.Fatalf("missing bearer %s: %+v", path, result)
		}
		if result := send("GET", path, "", map[string]string{"Authorization": "Bearer " + other.Memory.ParticipantToken}); result.status != 401 {
			t.Fatalf("cross-session bearer %s: %+v", path, result)
		}
	}
	if result := send("POST", base+"/photos", devicePhotoBody("wrong", "cp-a", time.Now().UTC()), map[string]string{"Authorization": "Bearer " + other.Memory.ParticipantToken, "Idempotency-Key": "wrong"}); result.status != 401 {
		t.Fatalf("cross-session write: %+v", result)
	}
	if result := send("GET", base+"/photos", "", ownerHeaders); result.status != 200 || !strings.Contains(result.body, `"photos":[]`) {
		t.Fatalf("empty list: %+v", result)
	}
	if result := send("GET", base+"/memory", "", ownerHeaders); result.status != 200 || !strings.Contains(result.body, `"segments":[]`) {
		t.Fatalf("empty memory: %+v", result)
	}
	captured := time.Now().UTC().Add(2 * time.Minute)
	body := devicePhotoBody("device-one", "cp-a", captured)
	for _, field := range []string{
		`"location":{"latitude":35,"longitude":139}`, `"latitude":35`, `"longitude":139`,
		`"coordinates":[35,139]`, `"sha256":"private-hash"`, `"sizeBytes":100`, `"width":20`,
		`"height":20`, `"mimeType":"image/jpeg"`, `"imageUrl":"https://example.invalid/photo"`,
		`"image":"base64-image"`, `"unknown":true`, `"clientPhotoId":"duplicate"`,
	} {
		rejected := strings.TrimSuffix(body, "}") + "," + field + "}"
		if result := send("POST", base+"/photos", rejected, ownerHeaders); result.status != 400 || strings.Contains(result.body, "private-hash") || strings.Contains(result.body, "base64-image") {
			t.Fatalf("forbidden metadata %s: %+v", field, result)
		}
	}
	for _, checkpoint := range []string{"", "not-a-checkpoint"} {
		if result := send("POST", base+"/photos", devicePhotoBody("bad", checkpoint, captured), ownerHeaders); result.status != 400 || !strings.Contains(result.body, `"INVALID_CHECKPOINT"`) {
			t.Fatalf("invalid checkpoint %s: %+v", checkpoint, result)
		}
	}
	if result := send("POST", base+"/photos", body, map[string]string{"Authorization": ownerHeaders["Authorization"]}); result.status != 400 {
		t.Fatalf("missing idempotency key: %+v", result)
	}
	created := send("POST", base+"/photos", body, ownerHeaders)
	if created.status != 201 {
		t.Fatalf("create: %+v", created)
	}
	var first struct {
		ID         string `json:"photoMomentId"`
		SequenceNo int    `json:"sequenceNo"`
	}
	if err := json.Unmarshal([]byte(created.body), &first); err != nil || first.ID == "" || first.SequenceNo != 1 {
		t.Fatalf("create metadata: %+v %v", created, err)
	}
	for _, forbidden := range []string{"imageUrl", "sha256", "sizeBytes", "latitude", "longitude", "uploadUrl", "mimeType"} {
		if strings.Contains(created.body, forbidden) {
			t.Fatalf("response exposed %s: %s", forbidden, created.body)
		}
	}
	if cloudPhotos, err := f.Store.Photos(t.Context(), owner.Memory.SessionID); err != nil || len(cloudPhotos) != 0 {
		t.Fatalf("device photo created a cloud asset: count=%d err=%v", len(cloudPhotos), err)
	}
	var receipt []byte
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT response_body FROM idempotency_records WHERE scope=$1`, tokenHash("create:moment:"+owner.Memory.SessionID)).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(receipt), `"photoHash"`) || strings.Contains(string(receipt), first.ID) || strings.Contains(string(receipt), "device-one") || strings.Contains(string(receipt), "cp-a") {
		t.Fatalf("receipt contains photo metadata: %s", receipt)
	}
	replay := send("POST", base+"/photos", " { \"checkpointId\":\"cp-a\", \"capturedAt\":\""+captured.Format(time.RFC3339Nano)+"\", \"clientPhotoId\":\"device-one\" } ", ownerHeaders)
	if replay.status != 200 || replay.body != created.body {
		t.Fatalf("semantic replay: %+v", replay)
	}
	secondKey := map[string]string{"Authorization": ownerHeaders["Authorization"], "Idempotency-Key": "other-key"}
	if again := send("POST", base+"/photos", body, secondKey); again.status != 200 || !strings.Contains(again.body, first.ID) {
		t.Fatalf("client ID retry: %+v", again)
	}
	if conflict := send("POST", base+"/photos", devicePhotoBody("device-one", "cp-b", captured), ownerHeaders); conflict.status != 409 {
		t.Fatalf("same key changed content: %+v", conflict)
	}
	if conflict := send("POST", base+"/photos", devicePhotoBody("device-one", "cp-b", captured), secondKey); conflict.status != 409 {
		t.Fatalf("same ID changed content: %+v", conflict)
	}
	older := send("POST", base+"/photos", devicePhotoBody("device-two", "cp-b", time.Now().UTC()), map[string]string{"Authorization": ownerHeaders["Authorization"], "Idempotency-Key": "second-moment"})
	if older.status != 201 {
		t.Fatalf("second moment: %+v", older)
	}
	var second struct {
		ID string `json:"photoMomentId"`
	}
	if err := json.Unmarshal([]byte(older.body), &second); err != nil {
		t.Fatal(err)
	}
	listed := send("GET", base+"/photos", "", ownerHeaders)
	if listed.status != 200 || !strings.Contains(listed.body, `"sequenceNo":2`) || strings.Index(listed.body, second.ID) > strings.Index(listed.body, first.ID) {
		t.Fatalf("captured-at order and sequence: %+v", listed)
	}
	story := send("GET", base+"/memory", "", ownerHeaders)
	if story.status != 200 || !strings.Contains(story.body, `"pathNodeIds"`) || !strings.Contains(story.body, `"graphVersion":"g1"`) || strings.Contains(story.body, `"imageUrl"`) || strings.Contains(story.body, `"latitude"`) || strings.Contains(story.body, `"path":`) {
		t.Fatalf("device story: %+v", story)
	}
	deletePath := base + "/photos/" + first.ID
	if result := send("DELETE", deletePath, "", map[string]string{"Authorization": "Bearer " + other.Memory.ParticipantToken, "Idempotency-Key": "delete"}); result.status != 401 {
		t.Fatalf("cross-session delete: %+v", result)
	}
	deleteHeaders := map[string]string{"Authorization": ownerHeaders["Authorization"], "Idempotency-Key": "delete"}
	if deleted := send("DELETE", deletePath, "", deleteHeaders); deleted.status != 204 {
		t.Fatalf("delete: %+v", deleted)
	}
	if deleted := send("DELETE", deletePath, "", deleteHeaders); deleted.status != 204 {
		t.Fatalf("delete replay: %+v", deleted)
	}
	if result := send("POST", base+"/photos", body, ownerHeaders); result.status != 404 {
		t.Fatalf("deleted ID receipt replay: %+v", result)
	}
	if result := send("POST", base+"/photos", body, map[string]string{"Authorization": ownerHeaders["Authorization"], "Idempotency-Key": "after-delete"}); result.status != 404 {
		t.Fatalf("deleted ID new key: %+v", result)
	}
	if result := send("GET", base+"/memory", "", ownerHeaders); result.status != 200 || strings.Contains(result.body, first.ID) || !strings.Contains(result.body, `"segments":[]`) {
		t.Fatalf("routes after delete: %+v", result)
	}
}

type momentHTTPResult struct {
	status int
	body   string
}

func TestDevicePhotoCapAndSessionExpiry(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	cfg.PhotoMetadataEnabled = true
	cfg.RegistrationOpen = true
	photoGraphFixture(t, f, &cfg)
	h := api.New(cfg, f.Store, nil)
	owner := createExperience(t, h, cfg)
	path := "/v2/memory-sessions/" + owner.Memory.SessionID + "/photos"
	var firstID string
	for n := range 12 {
		response := request(t, api.New(cfg, f.Store, nil), "POST", path, devicePhotoBody(fmt.Sprintf("device-%d", n), "cp-a", time.Now().UTC()), map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": fmt.Sprintf("create-%d", n)})
		if response.Code != 201 {
			t.Fatalf("moment %d: %d %s", n, response.Code, response.Body.String())
		}
		if n == 0 {
			var first struct {
				ID string `json:"photoMomentId"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
				t.Fatal(err)
			}
			firstID = first.ID
		}
	}
	extra := request(t, api.New(cfg, f.Store, nil), "POST", path, devicePhotoBody("device-13", "cp-a", time.Now().UTC()), map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": "create-13"})
	if extra.Code != 409 || !strings.Contains(extra.Body.String(), "PHOTO_LIMIT_REACHED") {
		t.Fatalf("photo cap: %d %s", extra.Code, extra.Body.String())
	}
	deleteResponse := request(t, api.New(cfg, f.Store, nil), "DELETE", path+"/"+firstID, "", map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": "free-slot"})
	if deleteResponse.Code != 204 {
		t.Fatalf("free photo slot: %d %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET expires_at=$1 WHERE id=$2`, time.Now().UTC().Add(-time.Minute), owner.Memory.SessionID); err != nil {
		t.Fatal(err)
	}
	expired := request(t, api.New(cfg, f.Store, nil), "POST", path, devicePhotoBody("after-expiry", "cp-a", time.Now().UTC()), map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": "expired"})
	if expired.Code != 409 && expired.Code != 404 {
		t.Fatalf("expired write must not succeed: %d %s", expired.Code, expired.Body.String())
	}
}

func TestDevicePhotoCutoverKeepsLegacyReadsAndDeletes(t *testing.T) {
	f := testdb.Open(t)
	assets := newPhotoObjects(t)
	legacyCfg := config.Default()
	legacyCfg.PhotoEnabled = true
	legacyCfg.RegistrationOpen = true
	photoGraphFixture(t, f, &legacyCfg)
	service := photo.New(f.Store, assets)
	legacy := api.New(legacyCfg, f.Store, service)
	owner := createExperience(t, legacy, legacyCfg)
	ready := readyPhoto(t, legacy, owner, photoJPEG(t), "legacy-photo")
	cfg := legacyCfg
	cfg.PhotoEnabled = false
	cfg.PhotoMetadataEnabled = true
	h := api.New(cfg, f.Store, service)
	base := "/v1/memory-sessions/" + owner.Memory.SessionID
	headers := map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": "after-cutover"}
	for _, path := range []string{base + "/photos/" + ready.ID + "/upload-url", base + "/photos/" + ready.ID + "/complete"} {
		if response := request(t, h, "POST", path, "", headers); response.Code != 503 {
			t.Fatalf("v1 upload operations active after cutover: %s: %d %s", path, response.Code, response.Body.String())
		}
	}
	if response := request(t, h, "POST", base+"/photos", photoRegistrationBody(t, photoJPEG(t), "new-cloud-photo", "cp-a"), headers); response.Code != 503 {
		t.Fatalf("v1 create active: %d %s", response.Code, response.Body.String())
	}
	// Guard the handler even if an in-process caller bypasses FromEnv's
	// mutually exclusive flag validation.
	bothModes := cfg
	bothModes.PhotoEnabled = true
	if response := request(t, api.New(bothModes, f.Store, service), "POST", base+"/photos", photoRegistrationBody(t, photoJPEG(t), "bad-both-modes", "cp-a"), headers); response.Code != 503 {
		t.Fatalf("v1 cloud write with both flags: %d %s", response.Code, response.Body.String())
	}
	if response := request(t, h, "GET", base+"/photos", "", headers); response.Code != 200 || !strings.Contains(response.Body.String(), ready.ID) {
		t.Fatalf("v1 list lost: %d %s", response.Code, response.Body.String())
	}
	story := request(t, h, "GET", base+"/memory", "", headers)
	if story.Code != 200 || !strings.Contains(story.Body.String(), `"imageUrl"`) {
		t.Fatalf("v1 story lost: %d %s", story.Code, story.Body.String())
	}
	var oldStory struct {
		Moments []struct {
			ImageURL string `json:"imageUrl"`
		} `json:"moments"`
	}
	if err := json.Unmarshal(story.Body.Bytes(), &oldStory); err != nil || len(oldStory.Moments) != 1 {
		t.Fatalf("v1 signed view: %v %s", err, story.Body.String())
	}
	res, err := http.Get(oldStory.Moments[0].ImageURL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("v1 signed GET failed: %d", res.StatusCode)
	}
	if response := request(t, h, "GET", "/v2/memory-sessions/"+owner.Memory.SessionID+"/photos", "", headers); response.Code != 404 {
		t.Fatalf("legacy session accepted on v2: %d %s", response.Code, response.Body.String())
	}
	if response := request(t, h, "DELETE", base+"/photos/"+ready.ID, "", headers); response.Code != 204 {
		t.Fatalf("v1 deletion lost: %d %s", response.Code, response.Body.String())
	}
}

func TestDevicePhotoEdgeSecretAndReadiness(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	cfg.PhotoMetadataEnabled = true
	cfg.RegistrationOpen = true
	cfg.EdgeSharedSecret = "test-edge-shared-secret"
	photoGraphFixture(t, f, &cfg)
	registration := api.New(cfg, f.Store, nil)
	body := fmt.Sprintf(`{"memoryConsentVersion":%q,"enableLivePresence":false,"consentLanguage":%q,"clientTime":%q}`,
		cfg.MemoryConsentVersion, cfg.ConsentLanguage, time.Now().UTC().Format(time.RFC3339Nano))
	response := request(t, registration, "POST", "/v1/experiences", body, map[string]string{"X-Trace-Edge-Secret": cfg.EdgeSharedSecret})
	if response.Code != 201 {
		t.Fatalf("register behind edge: %d %s", response.Code, response.Body.String())
	}
	var owner createdExperience
	if err := json.Unmarshal(response.Body.Bytes(), &owner); err != nil {
		t.Fatal(err)
	}
	path := "/v2/memory-sessions/" + owner.Memory.SessionID + "/photos"
	for _, secret := range []string{"", "incorrect"} {
		response = request(t, api.New(cfg, f.Store, nil), "GET", path, "", map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "X-Trace-Edge-Secret": secret})
		if response.Code != 403 || !strings.Contains(response.Body.String(), `"EDGE_ACCESS_DENIED"`) {
			t.Fatalf("v2 edge secret %q: %d %s", secret, response.Code, response.Body.String())
		}
	}
	response = request(t, api.New(cfg, f.Store, nil), "GET", path, "", map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "X-Trace-Edge-Secret": cfg.EdgeSharedSecret})
	if response.Code != 200 {
		t.Fatalf("v2 edge success: %d %s", response.Code, response.Body.String())
	}
	if _, err := f.SQL.ExecContext(t.Context(), "INSERT INTO schema_migrations (version) SELECT MAX(version)+1 FROM schema_migrations"); err != nil {
		t.Fatal(err)
	}
	response = request(t, api.New(cfg, f.Store, nil), "GET", path, "", map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "X-Trace-Edge-Secret": cfg.EdgeSharedSecret})
	if response.Code != 503 || !strings.Contains(response.Body.String(), `"NOT_READY"`) {
		t.Fatalf("v2 schema readiness: %d %s", response.Code, response.Body.String())
	}
}

func TestDevicePublicConfigHasNoUploadCapability(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	cfg.PhotoMetadataEnabled = true
	response := request(t, api.New(cfg, f.Store, nil), "GET", "/v1/public/config", "", nil)
	if response.Code != 200 {
		t.Fatalf("public config: %d %s", response.Code, response.Body.String())
	}
	var doc struct {
		Photo map[string]any `json:"photo"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Photo["enabled"] != true || doc.Photo["storageMode"] != "device" || doc.Photo["maxLocalBytes"] != float64(30*1024*1024) || doc.Photo["maxCount"] != float64(12) {
		t.Fatalf("device config: %+v", doc.Photo)
	}
	for _, key := range []string{"maxUploadBytes", "uploadUrl", "uploadHeaders", "maxLongEdgePixels"} {
		if _, exists := doc.Photo[key]; exists {
			t.Fatalf("cloud upload field %s: %+v", key, doc.Photo)
		}
	}
	cfg.PhotoMetadataEnabled = false
	legacy := api.New(cfg, f.Store, nil)
	doc.Photo = nil
	response = request(t, legacy, "GET", "/v1/public/config", "", nil)
	if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Photo["enabled"] != false || doc.Photo["maxUploadBytes"] != float64(5*1024*1024) {
		t.Fatalf("legacy config changed: %+v", doc.Photo)
	}
	if _, exists := doc.Photo["storageMode"]; exists {
		t.Fatalf("device mode advertised while disabled: %+v", doc.Photo)
	}
	response = request(t, legacy, "GET", "/v2/memory-sessions/unused/photos", "", nil)
	if response.Code != 503 || !strings.Contains(response.Body.String(), `"PHOTO_DISABLED"`) {
		t.Fatalf("v2 route remained enabled: %d %s", response.Code, response.Body.String())
	}
}
