package api_test

import (
	"bytes"
	"crypto/ecdh"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/push"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func testPushConfig(t *testing.T) (config.Config, *push.Service, string, string) {
	t.Helper()
	enc := base64.RawURLEncoding
	cfg := config.Default()
	cfg.RegistrationOpen = true
	cfg.PushEnabled = true
	cfg.PushVAPIDPrivateKey = enc.EncodeToString(append(make([]byte, 31), 1))
	x, y := elliptic.P256().ScalarBaseMult(append(make([]byte, 31), 1))
	cfg.PushVAPIDPublicKey = enc.EncodeToString(elliptic.Marshal(elliptic.P256(), x, y))
	cfg.PushEncryptionKey = enc.EncodeToString(bytes.Repeat([]byte{42}, 32))
	cfg.PushVAPIDSubject = "mailto:approved@example.org"
	sender, err := push.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdh.P256().NewPrivateKey(append(make([]byte, 31), 2))
	if err != nil {
		t.Fatal(err)
	}
	return cfg, sender, enc.EncodeToString(key.PublicKey().Bytes()), enc.EncodeToString(bytes.Repeat([]byte{9}, 16))
}
func TestPushAPIRequiresConsentAndNeverExposesCredentials(t *testing.T) {
	f := testdb.Open(t)
	cfg, sender, public, auth := testPushConfig(t)
	h := api.New(cfg, f.Store, nil, sender)
	created := createExperience(t, h, cfg)
	path := "/v1/memory-sessions/" + created.Memory.SessionID + "/push-subscription"
	headers := map[string]string{"Authorization": "Bearer " + created.Memory.ParticipantToken}
	endpoint := "https://fcm.googleapis.com/fcm/send/secret-browser-token"
	body := fmt.Sprintf(`{"consent":true,"subscription":{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}},"intervalMinutes":30}`, endpoint, public, auth)
	for _, invalid := range []string{
		fmt.Sprintf(`{"consent":false,"subscription":{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}},"intervalMinutes":30}`, endpoint, public, auth),
		fmt.Sprintf(`{"consent":true,"subscription":{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}},"intervalMinutes":20}`, endpoint, public, auth),
		fmt.Sprintf(`{"consent":true,"subscription":{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}},"intervalMinutes":15}`, endpoint, public, auth),
		fmt.Sprintf(`{"consent":true,"subscription":{"endpoint":"https://127.0.0.1/internal","keys":{"p256dh":%q,"auth":%q}},"intervalMinutes":30}`, public, auth),
	} {
		if r := request(t, h, http.MethodPut, path, invalid, headers); r.Code != 400 {
			t.Fatalf("invalid subscription accepted: %d", r.Code)
		}
	}
	if r := request(t, h, http.MethodPut, path, body, nil); r.Code != 401 {
		t.Fatal("unauthenticated subscription")
	}
	r := request(t, h, http.MethodPut, path, body, headers)
	if r.Code != 200 || bytes.Contains(r.Body.Bytes(), []byte("secret-browser-token")) {
		t.Fatalf("subscription result %d %s", r.Code, r.Body.String())
	}
	var ciphertext []byte
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT endpoint_encrypted FROM push_subscriptions WHERE memory_session_id=$1", created.Memory.SessionID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte(endpoint)) {
		t.Fatal("endpoint persisted in plaintext")
	}
	opened, err := sender.Open(created.Memory.SessionID, ciphertext)
	if err != nil || string(opened) != endpoint {
		t.Fatal("encrypted endpoint round trip", err)
	}
	wrong := append([]byte{}, ciphertext...)
	if _, err := sender.Open("other-memory", wrong); err == nil {
		t.Fatal("ciphertext not session bound")
	}
	if r := request(t, h, http.MethodPatch, path, `{"intervalMinutes":15}`, headers); r.Code != http.StatusBadRequest {
		t.Fatalf("retired 15-minute interval accepted: %d %s", r.Code, r.Body.String())
	}
	r = request(t, h, http.MethodPatch, path, `{"status":"paused"}`, headers)
	if r.Code != 200 {
		t.Fatalf("pause %d %s", r.Code, r.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil || response["nextSendAt"] != nil {
		t.Fatal("paused schedule exposed", err)
	}
	if r := request(t, h, http.MethodDelete, path, "", headers); r.Code != 204 {
		t.Fatalf("unsubscribe %d", r.Code)
	}
	var count int
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM push_subscriptions").Scan(&count); err != nil || count != 0 {
		t.Fatal("subscription survived unsubscribe", err)
	}
}
func TestPushUnavailableByDefault(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	cfg.RegistrationOpen = true
	h := api.New(cfg, f.Store, nil)
	created := createExperience(t, h, cfg)
	r := request(t, h, http.MethodGet, "/v1/public/config", "", nil)
	if r.Code != 200 || !bytes.Contains(r.Body.Bytes(), []byte(`"enabled":false`)) {
		t.Fatal("config claimed enabled")
	}
	path := "/v1/memory-sessions/" + created.Memory.SessionID + "/push-subscription"
	r = request(t, h, http.MethodPut, path, `{}`, map[string]string{"Authorization": "Bearer " + created.Memory.ParticipantToken})
	if r.Code != 503 {
		t.Fatalf("disabled push status %d", r.Code)
	}
}

func TestPushAfterArrivalConsentIsEnrollmentSnapshot(t *testing.T) {
	f := testdb.Open(t)
	cfg, sender, public, auth := testPushConfig(t)
	operator := arrivalConfig(t, f)
	cfg.ArrivalEnabled = true
	cfg.ArrivalProofKey, cfg.ArrivalProofKeyID = operator.ArrivalProofKey, operator.ArrivalProofKeyID
	cfg.ArrivalVenue, cfg.ArrivalCheckpointID = operator.ArrivalVenue, operator.ArrivalCheckpointID
	cfg.OperatorEnabled, cfg.OperatorGoogleClientID = operator.OperatorEnabled, operator.OperatorGoogleClientID
	cfg.OperatorEmails, cfg.OperatorAllowedOrigins = operator.OperatorEmails, operator.OperatorAllowedOrigins
	cfg.MemoryConsentVersion = "2026-10-31-v1"
	cfg.ConsentBody = "写真撮影の通知は到着時に終了します。"
	original := api.New(cfg, f.Store, nil, sender)
	oldArrived := createExperience(t, original, cfg)
	oldWaiting := createExperience(t, original, cfg)
	body := fmt.Sprintf(`{"consent":true,"subscription":{"endpoint":"https://fcm.googleapis.com/fcm/send/arrival-private-token","keys":{"p256dh":%q,"auth":%q}},"intervalMinutes":30}`, public, auth)
	toggle := func(h http.Handler, owner createdExperience, method, input string, want int) {
		t.Helper()
		path := "/v1/memory-sessions/" + owner.Memory.SessionID + "/push-subscription"
		r := request(t, h, method, path, input, map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken})
		if r.Code != want {
			t.Fatalf("%s push: got %d, want %d: %s", method, r.Code, want, r.Body.String())
		}
		if bytes.Contains(r.Body.Bytes(), []byte("arrival-private-token")) || bytes.Contains(r.Body.Bytes(), []byte(auth)) {
			t.Fatal("push response exposed subscription credentials")
		}
	}
	arrive := func(h http.Handler, owner createdExperience, key string) {
		t.Helper()
		if r := arriveRequest(t, h, owner, projectedProof(t, h), key); r.code != http.StatusOK {
			t.Fatalf("arrival: %d %s", r.code, r.body)
		}
	}
	assertSnapshot := func(owner createdExperience, wantPolicy, wantStopped bool) {
		t.Helper()
		var policy, stopped bool
		if err := f.SQL.QueryRowContext(t.Context(), `SELECT push_after_arrival,push_stopped FROM memory_sessions WHERE id=$1`, owner.Memory.SessionID).Scan(&policy, &stopped); err != nil || policy != wantPolicy || stopped != wantStopped {
			t.Fatalf("enrollment snapshot policy=%t stopped=%t, want %t/%t: %v", policy, stopped, wantPolicy, wantStopped, err)
		}
	}
	toggle(original, oldArrived, http.MethodPut, body, http.StatusOK)
	toggle(original, oldWaiting, http.MethodPut, body, http.StatusOK)
	arrive(original, oldArrived, "old-before-rollout")
	assertSnapshot(oldArrived, false, true)

	cfg.MemoryConsentVersion = "2026-11-01-photo-after-arrival-v2"
	cfg.ConsentBody = "写真撮影の通知は到着後も継続します。通知は任意で停止できます。"
	cfg.PushAfterArrivalConsentVersion = cfg.MemoryConsentVersion
	reviewed := api.New(cfg, f.Store, nil, sender)
	mismatch := fmt.Sprintf(`{"memoryConsentVersion":"2026-10-31-v1","consentLanguage":%q,"clientTime":%q}`, cfg.ConsentLanguage, time.Now().UTC().Format(time.RFC3339Nano))
	if r := request(t, reviewed, http.MethodPost, "/v1/experiences", mismatch, nil); r.Code != http.StatusConflict || !bytes.Contains(r.Body.Bytes(), []byte("CONSENT_VERSION_MISMATCH")) {
		t.Fatalf("old consent accepted after rollout: %d %s", r.Code, r.Body.String())
	}
	enrolled := createExperience(t, reviewed, cfg)
	waitingEnrolled := createExperience(t, reviewed, cfg)
	toggle(reviewed, waitingEnrolled, http.MethodPut, body, http.StatusOK)
	assertSnapshot(enrolled, true, false)
	var count int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM push_subscriptions WHERE memory_session_id=$1`, enrolled.Memory.SessionID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("policy enrolled push without explicit opt-in: count=%d err=%v", count, err)
	}
	toggle(reviewed, enrolled, http.MethodPut, body, http.StatusOK)
	arrive(reviewed, enrolled, "reviewed-after-rollout")
	toggle(reviewed, enrolled, http.MethodPut, body, http.StatusOK)
	toggle(reviewed, enrolled, http.MethodPatch, `{"status":"paused"}`, http.StatusOK)
	toggle(reviewed, enrolled, http.MethodPatch, `{"status":"active","intervalMinutes":30}`, http.StatusOK)
	assertSnapshot(enrolled, true, false)
	arrive(reviewed, oldWaiting, "old-after-rollout")
	for _, owner := range []createdExperience{oldArrived, oldWaiting} {
		toggle(reviewed, owner, http.MethodPut, body, http.StatusConflict)
		toggle(reviewed, owner, http.MethodPatch, `{"status":"paused"}`, http.StatusConflict)
		toggle(reviewed, owner, http.MethodPatch, `{"status":"active"}`, http.StatusConflict)
		assertSnapshot(owner, false, true)
	}

	cfg.PushAfterArrivalConsentVersion = ""
	restarted := api.New(cfg, f.Store, nil, sender)
	arrive(restarted, waitingEnrolled, "reviewed-before-policy-disabled")
	toggle(restarted, waitingEnrolled, http.MethodPut, body, http.StatusOK)
	toggle(restarted, waitingEnrolled, http.MethodPatch, `{"status":"active"}`, http.StatusOK)
	assertSnapshot(waitingEnrolled, true, false)
	toggle(restarted, enrolled, http.MethodPut, body, http.StatusOK)
	toggle(restarted, enrolled, http.MethodPatch, `{"status":"active"}`, http.StatusOK)
	assertSnapshot(enrolled, true, false)
	withoutPolicy := createExperience(t, restarted, cfg)
	toggle(restarted, withoutPolicy, http.MethodPut, body, http.StatusOK)
	arrive(restarted, withoutPolicy, "reviewed-version-policy-disabled")
	assertSnapshot(withoutPolicy, false, true)
}
