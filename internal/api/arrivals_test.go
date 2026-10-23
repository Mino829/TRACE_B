package api_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func arrivalConfig() config.Config {
	cfg := config.Default()
	cfg.ArrivalEnabled = true
	cfg.ArrivalIssuerKey = "issuer-secret-independent-of-participant-token-123"
	cfg.ArrivalProofKey = "proof-secret-independent-of-issuer-456789"
	cfg.ArrivalVenue = "campus-booth"
	cfg.ArrivalCheckpointID = "booth-01"
	return cfg
}

func signMessage(key, data string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func issuerHeaders(cfg config.Config, method, path string) map[string]string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	return map[string]string{"X-Trace-Issuer-Timestamp": ts,
		"X-Trace-Issuer-Signature": signMessage(cfg.ArrivalIssuerKey, method+"\n"+path+"\n"+ts)}
}

func projectedProof(t *testing.T, h http.Handler, cfg config.Config) string {
	t.Helper()
	path := "/v1/operator/arrival-proofs"
	r := request(t, h, "POST", path, "", issuerHeaders(cfg, "POST", path))
	if r.Code != http.StatusOK {
		t.Fatalf("issuer returned %d: %s", r.Code, r.Body.String())
	}
	var value struct {
		Token string `json:"checkpointToken"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value.Token == "" {
		t.Fatal("missing signed token")
	}
	return value.Token
}

func signProof(t *testing.T, cfg config.Config, venue string, issued, expires time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"checkpointId": cfg.ArrivalCheckpointID,
		"venue": venue, "issuedAt": issued.Unix(), "expiresAt": expires.Unix(),
		"nonce": base64.RawURLEncoding.EncodeToString([]byte("random-proof-nonce-123456"))})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + signMessage(cfg.ArrivalProofKey, encoded)
}

func arriveRequest(t *testing.T, h http.Handler, c createdExperience, proof, key string) *httpResult {
	t.Helper()
	body := fmt.Sprintf(`{"checkpointToken":%q,"experienceLinkToken":%q,"clientTime":"2000-01-01T00:00:00Z"}`, proof, c.LinkToken)
	r := request(t, h, "POST", "/v1/memory-sessions/"+c.Memory.SessionID+"/arrival", body,
		map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken, "Idempotency-Key": key})
	return &httpResult{r.Code, r.Body.String()}
}

type httpResult struct {
	code int
	body string
}

func TestArrivalIssuerIsolationAndProofBoundaries(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig()
	h := api.New(cfg, f.Store, nil)
	c := createExperience(t, h, cfg)
	path := "/v1/operator/arrival-proofs"
	for name, headers := range map[string]map[string]string{
		"participant bearer": {"Authorization": "Bearer " + c.Memory.ParticipantToken},
		"browser origin": func() map[string]string {
			v := issuerHeaders(cfg, "POST", path)
			v["Origin"] = cfg.AllowedOrigins[0]
			return v
		}(),
		"wrong signature": {"X-Trace-Issuer-Timestamp": strconv.FormatInt(time.Now().Unix(), 10), "X-Trace-Issuer-Signature": "bad"},
		"stale signature": func() map[string]string {
			ts := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
			return map[string]string{"X-Trace-Issuer-Timestamp": ts,
				"X-Trace-Issuer-Signature": signMessage(cfg.ArrivalIssuerKey, "POST\n"+path+"\n"+ts)}
		}(),
	} {
		if r := request(t, h, "POST", path, "", headers); r.Code != 403 {
			t.Errorf("%s: %d", name, r.Code)
		}
	}
	proof := projectedProof(t, h, cfg)
	if !strings.Contains(proof, ".") || strings.Contains(proof, c.Memory.ParticipantToken) {
		t.Fatal("proof leaked credentials")
	}
	now := time.Now().UTC()
	for name, candidate := range map[string]string{
		"tamper":              proof[:len(proof)-1] + "!",
		"foreign venue":       signProof(t, cfg, "another-booth", now, now.Add(20*time.Second)),
		"expired":             signProof(t, cfg, cfg.ArrivalVenue, now.Add(-31*time.Second), now.Add(-5*time.Second)),
		"expires exactly now": signProof(t, cfg, cfg.ArrivalVenue, now.Add(-20*time.Second), now),
		"overlong proof":      signProof(t, cfg, cfg.ArrivalVenue, now, now.Add(30*time.Second)),
		"future":              signProof(t, cfg, cfg.ArrivalVenue, now.Add(time.Minute), now.Add(time.Minute+20*time.Second)),
	} {
		if r := arriveRequest(t, h, c, candidate, name); r.code != 403 {
			t.Errorf("%s accepted: %d %s", name, r.code, r.body)
		}
	}
	wrongLink := fmt.Sprintf(`{"checkpointToken":%q,"experienceLinkToken":"foreign-link"}`, proof)
	forbidden := request(t, h, "POST", "/v1/memory-sessions/"+c.Memory.SessionID+"/arrival", wrongLink,
		map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken, "Idempotency-Key": "wrong-link"})
	if forbidden.Code != http.StatusConflict {
		t.Fatalf("foreign link updated live: %d %s", forbidden.Code, forbidden.Body.String())
	}
	for _, credential := range []string{c.Live.ParticipantToken, "not-a-participant-token"} {
		r := request(t, h, "POST", "/v1/memory-sessions/"+c.Memory.SessionID+"/arrival", wrongLink,
			map[string]string{"Authorization": "Bearer " + credential, "Idempotency-Key": "wrong-credential"})
		if r.Code != http.StatusUnauthorized {
			t.Fatalf("foreign credential: %d %s", r.Code, r.Body.String())
		}
	}
	if r := arriveRequest(t, h, c, proof, "valid"); r.code != 200 || !strings.Contains(r.body, `"status":"queued"`) {
		t.Fatalf("valid proof: %d %s", r.code, r.body)
	}
	if r := arriveRequest(t, h, c, proof, "valid"); r.code != 200 {
		t.Fatalf("retry: %d %s", r.code, r.body)
	}
	if r := arriveRequest(t, h, c, signProof(t, cfg, cfg.ArrivalVenue, now, now.Add(20*time.Second)), "valid"); r.code != 409 {
		t.Fatalf("same key different body: %d", r.code)
	}
	if r := arriveRequest(t, h, c, proof, "other-key"); r.code != 200 {
		t.Fatalf("duplicate arrival: %d %s", r.code, r.body)
	}
	other := createExperience(t, h, cfg)
	if r := arriveRequest(t, h, other, proof, "shared-projected-qr"); r.code != 200 {
		t.Fatalf("shared projector QR should admit another participant: %d %s", r.code, r.body)
	}
	var arrivals int
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM arrival_events WHERE memory_session_id=$1", c.Memory.SessionID).Scan(&arrivals); err != nil || arrivals != 1 {
		t.Fatalf("arrival count %d: %v", arrivals, err)
	}
	var phase, tracking string
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT presence_phase,tracking_status FROM live_presence_sessions WHERE id=$1", c.Live.SessionID).Scan(&phase, &tracking); err != nil || phase != "arrived" || tracking != "tracking" {
		t.Fatalf("live phase %s tracking %s: %v", phase, tracking, err)
	}
	state := request(t, h, "GET", "/v1/experiences/"+c.ExperienceID, "", map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken})
	if state.Code != 200 || !strings.Contains(state.Body.String(), `"replayStatus":"queued"`) || strings.Contains(state.Body.String(), "queuePosition") {
		t.Fatalf("private state: %d %s", state.Code, state.Body.String())
	}
	if r := request(t, h, "POST", "/v1/live-presence-sessions/"+c.Live.SessionID+"/on-site", `{"continue":true,"consentVersion":"`+cfg.OnSiteConsentVersion+`"}`, map[string]string{"Authorization": "Bearer " + c.Live.ParticipantToken}); r.Code != 200 {
		t.Fatalf("consented on-site transition: %d %s", r.Code, r.Body.String())
	}
}

func TestArrivalWithStoppedLiveStillRecordsMemoryEvent(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig()
	h := api.New(cfg, f.Store, nil)
	c := createExperience(t, h, cfg)
	stopPath := "/v1/live-presence-sessions/" + c.Live.SessionID + "/stop"
	stopped := request(t, h, "POST", stopPath, `{"reason":"participant_request"}`,
		map[string]string{"Authorization": "Bearer " + c.Live.ParticipantToken, "Idempotency-Key": "stop-before-arrival"})
	if stopped.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", stopped.Code, stopped.Body.String())
	}
	proof := projectedProof(t, h, cfg)
	if r := arriveRequest(t, h, c, proof, "after-stop"); r.code != http.StatusOK {
		t.Fatalf("memory arrival after stop: %d %s", r.code, r.body)
	}
	var phase, tracking string
	err := f.SQL.QueryRowContext(t.Context(), "SELECT presence_phase,tracking_status FROM live_presence_sessions WHERE id=$1", c.Live.SessionID).
		Scan(&phase, &tracking)
	if err != nil || phase != "ended" || tracking != "stopped" {
		t.Fatalf("stopped Live restarted: %s/%s %v", phase, tracking, err)
	}
}

func TestArrivalQueuePendingPhotoTimeoutSkipAndDeletion(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig()
	h := api.New(cfg, f.Store, nil)
	c := createExperience(t, h, cfg)
	now := time.Now().UTC()
	// A metadata-pending upload cannot block replay longer than 15 seconds.
	_, err := f.SQL.ExecContext(t.Context(), `INSERT INTO photos
 (id,id_hash,memory_session_id,client_photo_id,request_hash,captured_at,created_at,delete_after,upload_expires_at,
 mime_type,sha256,size_bytes,width,height,sequence_no,upload_attempts,status,stage_key,object_key)
 VALUES ('p1','h1',$1,'client-1','req-1',$2,$2,$3,$3,'image/jpeg','sha',10,1,1,1,1,'pending','stage-1','object-1')`, c.Memory.SessionID, now, c.Memory.DeleteAfter)
	if err != nil {
		t.Fatal(err)
	}
	proof := projectedProof(t, h, cfg)
	if r := arriveRequest(t, h, c, proof, "photo-pending"); r.code != 200 {
		t.Fatalf("pending photo should not block arrival: %d %s", r.code, r.body)
	}
	path := "/v1/operator/arrivals"
	list := request(t, h, "GET", path, "", issuerHeaders(cfg, "GET", path))
	if list.Code != 200 || strings.Contains(list.Body.String(), `"arrivalId"`) {
		t.Fatalf("pending photo wait: %d %s", list.Code, list.Body.String())
	}
	if _, err := f.SQL.ExecContext(t.Context(), "UPDATE arrival_events SET arrived_at=$2,photo_wait_until=$3 WHERE memory_session_id=$1",
		c.Memory.SessionID, now.Add(-20*time.Second), now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	list = request(t, h, "GET", path, "", issuerHeaders(cfg, "GET", path))
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"readyPhotoCount":0`) {
		t.Fatalf("wait cap with pending photo: %d %s", list.Code, list.Body.String())
	}
	if _, err := f.SQL.ExecContext(t.Context(), "UPDATE photos SET status='ready' WHERE memory_session_id=$1", c.Memory.SessionID); err != nil {
		t.Fatal(err)
	}
	list = request(t, h, "GET", path, "", issuerHeaders(cfg, "GET", path))
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"readyPhotoCount":1`) {
		t.Fatalf("one ready photo: %d %s", list.Code, list.Body.String())
	}
	if _, err := f.SQL.ExecContext(t.Context(), "UPDATE arrival_events SET replay_deadline=$2 WHERE memory_session_id=$1", c.Memory.SessionID, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	list = request(t, h, "GET", path, "", issuerHeaders(cfg, "GET", path))
	if list.Code != 200 || strings.Contains(list.Body.String(), `"arrivalId"`) {
		t.Fatalf("timeout candidate still visible: %d %s", list.Code, list.Body.String())
	}
	var status, reason string
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT replay_status,skip_reason FROM arrival_events WHERE memory_session_id=$1", c.Memory.SessionID).Scan(&status, &reason); err != nil || status != "skipped" || reason != "timeout" {
		t.Fatalf("timeout: %s/%s %v", status, reason, err)
	}
	// Removing a MemorySession cascades its arrival and excludes it from durable queue.
	noPhoto := createExperience(t, h, cfg)
	if r := arriveRequest(t, h, noPhoto, proof, "delete"); r.code != 200 {
		t.Fatalf("zero photo: %d %s", r.code, r.body)
	}
	deleted := request(t, h, "DELETE", "/v1/memory-sessions/"+noPhoto.Memory.SessionID, "", map[string]string{"Authorization": "Bearer " + noPhoto.Memory.ParticipantToken, "Idempotency-Key": "delete-arrived"})
	if deleted.Code != 204 {
		t.Fatalf("delete arrived: %d %s", deleted.Code, deleted.Body.String())
	}
	if r := arriveRequest(t, h, noPhoto, proof, "delete"); r.code != 401 {
		t.Fatalf("deleted retry: %d", r.code)
	}
	var count int
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM arrival_events WHERE memory_session_id=$1", noPhoto.Memory.SessionID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphan arrival: %d %v", count, err)
	}
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM arrival_receipts WHERE memory_session_id=$1", noPhoto.Memory.SessionID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted Memory retained arrival receipt: %d %v", count, err)
	}
}

func TestArrivalConcurrentDuplicateAndOperatorSkip(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig()
	h := api.New(cfg, f.Store, nil)
	c := createExperience(t, h, cfg)
	proof := projectedProof(t, h, cfg)
	results := make(chan *httpResult, 4)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results <- arriveRequest(t, h, c, proof, fmt.Sprintf("parallel-%d", i)) }(i)
	}
	wg.Wait()
	close(results)
	for result := range results {
		if result.code != 200 {
			t.Fatalf("parallel arrival: %d %s", result.code, result.body)
		}
	}
	var id string
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT id FROM arrival_events WHERE memory_session_id=$1", c.Memory.SessionID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	path := "/v1/operator/arrivals/" + id + "/skip"
	if r := request(t, h, "POST", path, "", map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken}); r.Code != 403 {
		t.Fatalf("participant skipped via operator: %d", r.Code)
	}
	if r := request(t, h, "POST", path, "", issuerHeaders(cfg, "POST", path)); r.Code != 200 {
		t.Fatalf("operator skip: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, "POST", path, "", issuerHeaders(cfg, "POST", path)); r.Code != 200 {
		t.Fatalf("operator retry skip: %d", r.Code)
	}
	var status string
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT replay_status FROM arrival_events WHERE id=$1", id).Scan(&status); err != nil || status != "skipped" {
		t.Fatalf("skip not durable: %s %v", status, err)
	}
	var auditCount int
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM arrival_operator_audit WHERE action='arrival.skip' AND target_id_short<>$1", id).
		Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("redacted operator audit: %d %v", auditCount, err)
	}
	// New arrival after skip still has its own bounded queue slot.
	other := createExperience(t, h, cfg)
	if r := arriveRequest(t, h, other, proof, "other"); r.code != 200 {
		t.Fatalf("arrival after skip: %d %s", r.code, r.body)
	}
	ownPath := "/v1/memory-sessions/" + other.Memory.SessionID + "/arrival/skip"
	if r := request(t, h, "POST", ownPath, "", map[string]string{"Authorization": "Bearer " + other.Memory.ParticipantToken}); r.Code != 200 {
		t.Fatalf("participant skip: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, "POST", ownPath, "", map[string]string{"Authorization": "Bearer " + other.Memory.ParticipantToken}); r.Code != 200 {
		t.Fatalf("participant skip retry: %d", r.Code)
	}
	var ownStatus, tracking string
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT a.replay_status,l.tracking_status FROM arrival_events a
 JOIN experience_links e ON e.memory_session_id=a.memory_session_id
 JOIN live_presence_sessions l ON l.id=e.live_session_id WHERE a.memory_session_id=$1`,
		other.Memory.SessionID).Scan(&ownStatus, &tracking); err != nil || ownStatus != "skipped" || tracking != "tracking" {
		t.Fatalf("skip stopped GPS or failed: %s/%s %v", ownStatus, tracking, err)
	}
}

func TestArrivalDeletionRaceNeverLeavesQueueEntry(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig()
	h := api.New(cfg, f.Store, nil)
	c := createExperience(t, h, cfg)
	proof := projectedProof(t, h, cfg)
	results := make(chan int, 2)
	start := make(chan struct{})
	go func() {
		<-start
		results <- arriveRequest(t, h, c, proof, "race-arrival").code
	}()
	go func() {
		<-start
		r := request(t, h, "DELETE", "/v1/memory-sessions/"+c.Memory.SessionID, "",
			map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken, "Idempotency-Key": "race-delete"})
		results <- r.Code
	}()
	close(start)
	a, b := <-results, <-results
	if (a != 204 && b != 204) || (a != 200 && a != 401 && a != 404 && a != 409 &&
		b != 200 && b != 401 && b != 404 && b != 409) {
		t.Fatalf("unexpected arrival/delete outcomes: %d %d", a, b)
	}
	var count int
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM arrival_events WHERE memory_session_id=$1", c.Memory.SessionID).
		Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted arrival persisted: %d %v", count, err)
	}
}
