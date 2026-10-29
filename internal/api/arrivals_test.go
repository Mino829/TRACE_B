package api_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func arrivalConfig(t *testing.T, f *testdb.Fixture) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.RegistrationOpen = true
	cfg.ArrivalEnabled = true
	cfg.ArrivalProofKey = "proof-secret-independent-of-operator-456789"
	cfg.ArrivalProofKeyID = "arrival-proof-test-v1"
	cfg.ArrivalVenue = "campus-booth"
	cfg.ArrivalCheckpointID = "booth-01"
	enableOperatorTest(t, f, &cfg)
	return cfg
}

func signMessage(key, data string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func projectedProof(t *testing.T, h http.Handler) string {
	t.Helper()
	path := "/v1/operator/arrival-proofs"
	r := request(t, h, "POST", path, "", operatorTestHeaders())
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
	payload, err := json.Marshal(map[string]any{"purpose": "arrival", "keyId": cfg.ArrivalProofKeyID,
		"checkpointId": cfg.ArrivalCheckpointID, "venue": venue, "issuedAt": issued.Unix(), "expiresAt": expires.Unix(),
		"nonce": base64.RawURLEncoding.EncodeToString([]byte("random-proof-nonce-123456"))})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + signMessage(cfg.ArrivalProofKey, encoded)
}

func alterProof(t *testing.T, cfg config.Config, proof, field string, value any) string {
	t.Helper()
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(proof, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if value == nil {
		delete(fields, field)
	} else {
		fields[field] = value
	}
	payload, err = json.Marshal(fields)
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

func TestArrivalOperatorIsolationAndProofBoundaries(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig(t, f)
	h := api.New(cfg, f.Store, nil)
	c := seedLegacyExperience(t, f.Store, cfg)
	path := "/v1/operator/arrival-proofs"
	for _, tc := range []struct {
		name    string
		headers map[string]string
		status  int
	}{
		{"participant bearer", map[string]string{"Origin": operatorTestHeaders()["Origin"], "Authorization": "Bearer " + c.Memory.ParticipantToken}, http.StatusUnauthorized},
		{"participant origin", func() map[string]string {
			v := operatorTestHeaders()
			v["Origin"] = cfg.AllowedOrigins[0]
			return v
		}(), http.StatusForbidden},
		{"obsolete issuer signature", map[string]string{"Origin": operatorTestHeaders()["Origin"], "X-Trace-Issuer-Timestamp": "1", "X-Trace-Issuer-Signature": "bad"}, http.StatusUnauthorized},
		{"missing bearer", map[string]string{"Origin": operatorTestHeaders()["Origin"]}, http.StatusUnauthorized},
	} {
		if r := request(t, h, "POST", path, "", tc.headers); r.Code != tc.status {
			t.Errorf("%s: got %d, want %d", tc.name, r.Code, tc.status)
		}
	}
	proof := projectedProof(t, h)
	if !strings.Contains(proof, ".") || strings.Contains(proof, c.Memory.ParticipantToken) {
		t.Fatal("proof leaked credentials")
	}
	var proofActor, proofEmailHash, proofTarget, proofResult string
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT operator_id,operator_email_hash,target_id_short,result
 FROM arrival_operator_audit WHERE action='arrival.proof.issue'`).Scan(&proofActor, &proofEmailHash, &proofTarget, &proofResult); err != nil ||
		proofActor != operatorTestActor || proofEmailHash != operatorTestHash("operator.email\x00operator@gmail.com") ||
		proofTarget == cfg.ArrivalCheckpointID || proofResult != "issued" {
		t.Fatalf("proof identity audit: %s %s %s %s %v", proofActor, proofEmailHash, proofTarget, proofResult, err)
	}
	now := time.Now().UTC()
	for name, candidate := range map[string]string{
		"tamper":              proof[:len(proof)-1] + "!",
		"missing purpose":     alterProof(t, cfg, proof, "purpose", nil),
		"wrong purpose":       alterProof(t, cfg, proof, "purpose", "display.ws"),
		"missing key ID":      alterProof(t, cfg, proof, "keyId", nil),
		"wrong key ID":        alterProof(t, cfg, proof, "keyId", "retired-proof-v0"),
		"missing nonce":       alterProof(t, cfg, proof, "nonce", nil),
		"short nonce":         alterProof(t, cfg, proof, "nonce", "c2hvcnQ"),
		"wrong checkpoint":    alterProof(t, cfg, proof, "checkpointId", "another-checkpoint"),
		"empty proof":         "",
		"foreign venue":       signProof(t, cfg, "another-booth", now, now.Add(20*time.Second)),
		"expired":             signProof(t, cfg, cfg.ArrivalVenue, now.Add(-31*time.Second), now.Add(-5*time.Second)),
		"expires exactly now": signProof(t, cfg, cfg.ArrivalVenue, now.Add(-20*time.Second), now),
		"overlong proof":      signProof(t, cfg, cfg.ArrivalVenue, now, now.Add(30*time.Second)),
		"future":              signProof(t, cfg, cfg.ArrivalVenue, now.Add(time.Minute), now.Add(time.Minute+20*time.Second)),
	} {
		if r := arriveRequest(t, h, c, candidate, name); r.code != http.StatusBadRequest || !strings.Contains(r.body, `"code":"INVALID_ARRIVAL_PROOF"`) {
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
	if r := arriveRequest(t, h, c, signProof(t, cfg, cfg.ArrivalVenue, now, now.Add(20*time.Second)), "valid"); r.code != 409 || !strings.Contains(r.body, `"code":"IDEMPOTENCY_CONFLICT"`) {
		t.Fatalf("same key different body: %d %s", r.code, r.body)
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
	if r := request(t, h, "POST", "/v1/live-presence-sessions/"+c.Live.SessionID+"/on-site", `{"continue":true,"consentVersion":"`+cfg.OnSiteConsentVersion+`"}`, map[string]string{"Authorization": "Bearer " + c.Live.ParticipantToken}); r.Code != http.StatusGone || !strings.Contains(r.Body.String(), `"code":"ON_SITE_DISABLED"`) {
		t.Fatalf("retired on-site continuation: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, "POST", "/v1/live-presence-sessions/"+c.Live.SessionID+"/on-site", `{"continue":false}`, map[string]string{"Authorization": "Bearer " + c.Live.ParticipantToken}); r.Code != http.StatusOK {
		t.Fatalf("legacy on-site stop: %d %s", r.Code, r.Body.String())
	}
}

func TestArrivalStopsPushAtomicallyForLiveAndMemoryOnly(t *testing.T) {
	for _, live := range []bool{true, false} {
		t.Run(fmt.Sprintf("live=%t", live), func(t *testing.T) {
			f := testdb.Open(t)
			cfg := arrivalConfig(t, f)
			h := api.New(cfg, f.Store, nil)
			var memoryID, memoryToken, linkToken, experienceID string
			if live {
				c := seedLegacyExperience(t, f.Store, cfg)
				memoryID, memoryToken, linkToken, experienceID = c.Memory.SessionID, c.Memory.ParticipantToken, c.LinkToken, c.ExperienceID
			} else {
				digest := sha256.Sum256([]byte("memory-only-token"))
				now := time.Now().UTC()
				memoryID, memoryToken, experienceID = "memory-only", "memory-only-token", "memory-only-experience"
				err := f.Store.CreateExperience(t.Context(), store.NewExperience{
					ExperienceID: experienceID, MemorySessionID: memoryID, MemoryTokenHash: fmt.Sprintf("%x", digest),
					MemoryConsent: cfg.MemoryConsentVersion, ConsentLanguage: "ja", DisplayColor: "cyan", DisplaySymbol: "circle",
					CreatedAt: now, MemoryExpiresAt: now.Add(time.Hour), MemoryDeleteAfter: now.Add(2 * time.Hour),
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			sub := store.PushSubscription{MemorySessionID: memoryID, EndpointEncrypted: []byte("encrypted-endpoint"),
				KeysEncrypted: []byte("encrypted-keys"), IntervalMinutes: 30}
			if _, err := f.Store.PutPush(t.Context(), sub, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			proof := projectedProof(t, h)
			body := fmt.Sprintf(`{"checkpointToken":%q,"experienceLinkToken":%q}`, proof, linkToken)
			path := "/v1/memory-sessions/" + memoryID + "/arrival"
			headers := map[string]string{"Authorization": "Bearer " + memoryToken, "Idempotency-Key": "arrival-stops-push"}
			for range 2 {
				response := request(t, h, "POST", path, body, headers)
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"queued"`) {
					t.Fatalf("arrival/retry: %d %s", response.Code, response.Body.String())
				}
			}
			var stopped bool
			var remaining int
			if err := f.SQL.QueryRowContext(t.Context(), "SELECT push_stopped FROM memory_sessions WHERE id=$1", memoryID).Scan(&stopped); err != nil {
				t.Fatal(err)
			}
			if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM push_subscriptions WHERE memory_session_id=$1", memoryID).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if !stopped || remaining != 0 {
				t.Fatalf("arrival retained push: stopped=%t subscriptions=%d", stopped, remaining)
			}
			if _, err := f.Store.PutPush(t.Context(), sub, time.Now().UTC()); err != store.ErrConflict {
				t.Fatalf("arrival permitted resubscribe: %v", err)
			}
			state := request(t, h, "GET", "/v1/experiences/"+experienceID, "", map[string]string{"Authorization": "Bearer " + memoryToken})
			if state.Code != http.StatusOK || !strings.Contains(state.Body.String(), `"replayStatus":"queued"`) {
				t.Fatalf("state after arrival: %d %s", state.Code, state.Body.String())
			}
		})
	}
}

func TestArrivalWithStoppedLiveStillRecordsMemoryEvent(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig(t, f)
	h := api.New(cfg, f.Store, nil)
	c := seedLegacyExperience(t, f.Store, cfg)
	stopPath := "/v1/live-presence-sessions/" + c.Live.SessionID + "/stop"
	stopped := request(t, h, "POST", stopPath, `{"reason":"participant_request"}`,
		map[string]string{"Authorization": "Bearer " + c.Live.ParticipantToken, "Idempotency-Key": "stop-before-arrival"})
	if stopped.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", stopped.Code, stopped.Body.String())
	}
	proof := projectedProof(t, h)
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

func TestArrivalQueuePendingPhotoWaitAndDeletion(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig(t, f)
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
	proof := projectedProof(t, h)
	if r := arriveRequest(t, h, c, proof, "photo-pending"); r.code != 200 {
		t.Fatalf("pending photo should not block arrival: %d %s", r.code, r.body)
	}
	path := "/v1/operator/arrivals"
	list := request(t, h, "GET", path, "", operatorTestHeaders())
	if list.Code != 200 || strings.Contains(list.Body.String(), `"arrivalId"`) {
		t.Fatalf("pending photo wait: %d %s", list.Code, list.Body.String())
	}
	if _, err := f.SQL.ExecContext(t.Context(), "UPDATE arrival_events SET arrived_at=$2,photo_wait_until=$3 WHERE memory_session_id=$1",
		c.Memory.SessionID, now.Add(-20*time.Second), now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	list = request(t, h, "GET", path, "", operatorTestHeaders())
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"readyPhotoCount":0`) {
		t.Fatalf("wait cap with pending photo: %d %s", list.Code, list.Body.String())
	}
	if _, err := f.SQL.ExecContext(t.Context(), "UPDATE photos SET status='ready' WHERE memory_session_id=$1", c.Memory.SessionID); err != nil {
		t.Fatal(err)
	}
	list = request(t, h, "GET", path, "", operatorTestHeaders())
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"readyPhotoCount":1`) {
		t.Fatalf("one ready photo: %d %s", list.Code, list.Body.String())
	}
	if _, err := f.SQL.ExecContext(t.Context(), "UPDATE arrival_events SET arrived_at=$2,photo_wait_until=$2 WHERE memory_session_id=$1", c.Memory.SessionID, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	list = request(t, h, "GET", path, "", operatorTestHeaders())
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"readyPhotoCount":1`) {
		t.Fatalf("long-waiting arrival disappeared: %d %s", list.Code, list.Body.String())
	}
	var status, reason string
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT replay_status,COALESCE(skip_reason,'') FROM arrival_events WHERE memory_session_id=$1", c.Memory.SessionID).Scan(&status, &reason); err != nil || status != "queued" || reason != "" {
		t.Fatalf("long wait was skipped: %s/%s %v", status, reason, err)
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
	cfg := arrivalConfig(t, f)
	h := api.New(cfg, f.Store, nil)
	c := createExperience(t, h, cfg)
	proof := projectedProof(t, h)
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
	if r := request(t, h, "POST", path, "", operatorTestHeaders()); r.Code != 200 {
		t.Fatalf("operator skip: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, "POST", path, "", operatorTestHeaders()); r.Code != 200 {
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
	other := seedLegacyExperience(t, f.Store, cfg)
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
	cfg := arrivalConfig(t, f)
	h := api.New(cfg, f.Store, nil)
	c := createExperience(t, h, cfg)
	proof := projectedProof(t, h)
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

func TestArrivalRetryReturnsCurrentStateWithExpiredOrRetiredProof(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig(t, f)
	cfg.DisplayEnabled = true
	h := api.New(cfg, f.Store, nil)
	owner := createExperience(t, h, cfg)
	proof := projectedProof(t, h)
	first := arriveRequest(t, h, owner, proof, "original-arrival")
	if first.code != http.StatusOK {
		t.Fatalf("first arrival: %d %s", first.code, first.body)
	}
	var original struct {
		ID string `json:"arrivalId"`
	}
	if err := json.Unmarshal([]byte(first.body), &original); err != nil || original.ID == "" {
		t.Fatalf("arrival ID: %s %v", first.body, err)
	}
	if err := f.Store.SkipOwnArrival(t.Context(), owner.Memory.SessionID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	rotated := cfg
	rotated.ArrivalProofKeyID = "arrival-proof-test-v2"
	rotated.ArrivalProofKey = "new-proof-key-without-old-key-verification-123"
	retiredProofHandler := api.New(rotated, f.Store, nil)
	expired := signProof(t, cfg, cfg.ArrivalVenue, time.Now().Add(-time.Minute), time.Now().Add(-40*time.Second))
	for _, retry := range []struct {
		handler http.Handler
		proof   string
		key     string
	}{
		{retiredProofHandler, proof, "original-arrival"},
		{h, expired, "new-key-after-proof-expiry"},
	} {
		result := arriveRequest(t, retry.handler, owner, retry.proof, retry.key)
		var current struct {
			ID     string `json:"arrivalId"`
			Status string `json:"status"`
		}
		err := json.Unmarshal([]byte(result.body), &current)
		if result.code != http.StatusOK || err != nil || current.ID != original.ID || current.Status != "skipped" ||
			strings.Contains(result.body, "onSiteChoiceRequired") {
			t.Fatalf("current arrival retry: %d %s %v", result.code, result.body, err)
		}
	}
	if result := arriveRequest(t, h, owner, expired, "original-arrival"); result.code != http.StatusConflict || !strings.Contains(result.body, `"code":"IDEMPOTENCY_CONFLICT"`) {
		t.Fatalf("changed expired request bypassed receipt conflict: %d %s", result.code, result.body)
	}
	newOwner := createExperience(t, h, cfg)
	if result := arriveRequest(t, h, newOwner, expired, "new-arrival-expired-proof"); result.code != http.StatusBadRequest || !strings.Contains(result.body, `"code":"INVALID_ARRIVAL_PROOF"`) {
		t.Fatalf("expired proof authorized new arrival: %d %s", result.code, result.body)
	}
	var after int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events`).Scan(&after); err != nil || before != after {
		t.Fatalf("arrival recovery emitted duplicate events: before=%d after=%d err=%v", before, after, err)
	}
}

func TestArrivalProofIssuanceRequiresSuccessfulAudit(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig(t, f)
	h := api.New(cfg, f.Store, nil)
	if _, err := f.SQL.ExecContext(t.Context(), `ALTER TABLE arrival_operator_audit
 ADD CONSTRAINT reject_arrival_proof_audit CHECK (action<>'arrival.proof.issue')`); err != nil {
		t.Fatal(err)
	}
	response := request(t, h, "POST", "/v1/operator/arrival-proofs", "", operatorTestHeaders())
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "checkpointToken") {
		t.Fatalf("unaudited proof issued: %d %s", response.Code, response.Body.String())
	}
}
