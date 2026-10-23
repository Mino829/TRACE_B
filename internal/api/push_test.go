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

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/push"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func testPushConfig(t *testing.T) (config.Config, *push.Service, string, string) {
	t.Helper()
	enc := base64.RawURLEncoding
	cfg := config.Default()
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
