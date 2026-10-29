package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

const notificationsStopPath = "/v1/operator/notifications/stop"

func TestNotificationStopPersistsClearsAllSubscriptionsAndBlocksResubscribe(t *testing.T) {
	f := testdb.Open(t)
	cfg, sender, public, auth := testPushConfig(t)
	operator := arrivalConfig(t, f)
	cfg.ArrivalEnabled = true
	cfg.ArrivalProofKey, cfg.ArrivalProofKeyID = operator.ArrivalProofKey, operator.ArrivalProofKeyID
	cfg.ArrivalVenue, cfg.ArrivalCheckpointID = operator.ArrivalVenue, operator.ArrivalCheckpointID
	enableOperatorTest(t, f, &cfg)
	h := api.New(cfg, f.Store, nil, sender)
	checkState := func(handler http.Handler, enabled, stopped bool) {
		t.Helper()
		got := request(t, handler, "GET", "/v1/public/config", "", nil)
		var publicState struct {
			RegistrationOpen bool `json:"registrationOpen"`
			Notification     struct {
				Enabled        bool   `json:"enabled"`
				VAPIDPublicKey string `json:"vapidPublicKey"`
			} `json:"notification"`
		}
		if err := json.Unmarshal(got.Body.Bytes(), &publicState); err != nil || got.Code != 200 || publicState.Notification.Enabled != enabled || !publicState.RegistrationOpen {
			t.Fatalf("public state: %d %s (%v)", got.Code, got.Body.String(), err)
		}
		if enabled && publicState.Notification.VAPIDPublicKey != cfg.PushVAPIDPublicKey || !enabled && publicState.Notification.VAPIDPublicKey != "" {
			t.Fatalf("incorrect public notification key: %+v", publicState.Notification)
		}
		got = request(t, handler, "GET", operatorStatusPath, "", operatorTestHeaders())
		var status struct {
			Notification struct {
				Enabled         bool `json:"enabled"`
				OperatorStopped bool `json:"operatorStopped"`
			} `json:"notification"`
		}
		if err := json.Unmarshal(got.Body.Bytes(), &status); err != nil || got.Code != 200 || status.Notification.Enabled != enabled || status.Notification.OperatorStopped != stopped {
			t.Fatalf("operator state: %d %s (%v)", got.Code, got.Body.String(), err)
		}
	}
	body := fmt.Sprintf(`{"consent":true,"subscription":{"endpoint":"https://fcm.googleapis.com/fcm/send/synthetic-notification","keys":{"p256dh":%q,"auth":%q}},"intervalMinutes":30}`, public, auth)
	owner := createExperience(t, h, cfg)
	path := "/v1/memory-sessions/" + owner.Memory.SessionID + "/push-subscription"
	headers := map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken}
	if got := request(t, h, "PUT", path, body, headers); got.Code != 200 {
		t.Fatalf("subscribe: %d %s", got.Code, got.Body.String())
	}
	pausedOwner := createExperience(t, h, cfg)
	pausedPath := "/v1/memory-sessions/" + pausedOwner.Memory.SessionID + "/push-subscription"
	pausedHeaders := map[string]string{"Authorization": "Bearer " + pausedOwner.Memory.ParticipantToken}
	if got := request(t, h, "PUT", pausedPath, body, pausedHeaders); got.Code != 200 {
		t.Fatalf("second subscribe: %d %s", got.Code, got.Body.String())
	}
	if got := request(t, h, "PATCH", pausedPath, `{"status":"paused"}`, pausedHeaders); got.Code != 200 {
		t.Fatalf("pause: %d %s", got.Code, got.Body.String())
	}
	checkState(h, true, false)
	var firstStop time.Time
	for attempt := range 2 {
		got := request(t, h, "POST", notificationsStopPath, "", operatorTestHeaders())
		if got.Code != 200 || !strings.Contains(got.Body.String(), `"notificationsStopped":true`) {
			t.Fatalf("stop: %d %s", got.Code, got.Body.String())
		}
		var at time.Time
		if err := f.SQL.QueryRowContext(t.Context(), `SELECT stopped_at FROM notification_control`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			firstStop = at
		} else if !at.Equal(firstStop) {
			t.Fatal("repeat changed initial stop timestamp")
		}
	}
	var subscriptions int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM push_subscriptions`).Scan(&subscriptions); err != nil || subscriptions != 0 {
		t.Fatalf("active or paused credentials retained: %d %v", subscriptions, err)
	}
	other, err := store.Open(t.Context(), f.URL, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	restarted := api.New(cfg, other, nil, sender)
	checkState(restarted, false, true)
	for _, action := range []struct{ method, body string }{{"PUT", body}, {"PATCH", `{"status":"active"}`}} {
		got := request(t, restarted, action.method, path, action.body, headers)
		if got.Code != 503 || !strings.Contains(got.Body.String(), `"PUSH_UNAVAILABLE"`) {
			t.Fatalf("resubscribe/resume: %d %s", got.Code, got.Body.String())
		}
	}
	// Stopping notifications must not close registration or allow a new Memory to bypass the stop.
	newOwner := createExperience(t, restarted, cfg)
	newPath := "/v1/memory-sessions/" + newOwner.Memory.SessionID + "/push-subscription"
	if got := request(t, restarted, "PUT", newPath, body, map[string]string{"Authorization": "Bearer " + newOwner.Memory.ParticipantToken}); got.Code != 503 {
		t.Fatalf("new Memory bypassed stop: %d %s", got.Code, got.Body.String())
	}
	if got := request(t, restarted, "DELETE", path, "", headers); got.Code != 204 {
		t.Fatalf("unsubscribe after stop: %d %s", got.Code, got.Body.String())
	}
	proof := signProof(t, cfg, cfg.ArrivalVenue, time.Now().Add(-time.Second), time.Now().Add(20*time.Second))
	if got := arriveRequest(t, restarted, owner, proof, "notifications-stop-arrival"); got.code != 200 {
		t.Fatalf("arrival after notification stop: %d %s", got.code, got.body)
	}
	var stopped, repeated int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FILTER (WHERE result='stopped'),COUNT(*) FILTER (WHERE result='already_stopped') FROM arrival_operator_audit WHERE action='notifications.stop' AND operator_id=$1 AND target_id_short='notifications'`, operatorTestActor).Scan(&stopped, &repeated); err != nil || stopped != 1 || repeated != 1 {
		t.Fatalf("stop audit: %d %d %v", stopped, repeated, err)
	}
}

func TestNotificationStopAuthorizationAndBodyValidation(t *testing.T) {
	f := testdb.Open(t)
	cfg := displayConfig(t, f)
	cfg.EdgeSharedSecret = "independent-edge-secret-1234567890"
	h := api.New(cfg, f.Store, nil)
	for _, tc := range []struct {
		name, body string
		change     func(map[string]string)
		want       int
	}{
		{"missing edge", "", func(h map[string]string) { delete(h, "X-Trace-Edge-Secret") }, 403},
		{"edge only", "", func(h map[string]string) { delete(h, "Authorization") }, 401},
		{"participant", "", func(h map[string]string) { h["Authorization"] = "Bearer participant" }, 401},
		{"display", "", func(h map[string]string) { h["Authorization"] = "Bearer " + displayToken(t, cfg) }, 401},
		{"renderer origin", "", func(h map[string]string) { h["Origin"] = cfg.DisplayAllowedOrigins[0] }, 403},
		{"legacy signature", "", func(h map[string]string) { h["X-Trace-Issuer-Signature"] = "obsolete" }, 401},
		{"legacy timestamp", "", func(h map[string]string) { h["X-Trace-Issuer-Timestamp"] = "1" }, 401},
		{"body", `{}`, func(map[string]string) {}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := operatorTestHeaders()
			headers["X-Trace-Edge-Secret"] = cfg.EdgeSharedSecret
			tc.change(headers)
			got := request(t, h, "POST", notificationsStopPath, tc.body, headers)
			if got.Code != tc.want {
				t.Fatalf("rejected stop: %d %s", got.Code, got.Body.String())
			}
		})
	}
	if stopped, err := f.Store.PushStopped(t.Context()); err != nil || stopped {
		t.Fatalf("rejected request changed stop: %t %v", stopped, err)
	}
	cfg.OperatorEnabled = false
	if got := request(t, api.New(cfg, f.Store, nil), "POST", notificationsStopPath, "", map[string]string{"X-Trace-Edge-Secret": cfg.EdgeSharedSecret}); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled operator: %d %s", got.Code, got.Body.String())
	}
}

func TestNotificationStopWorksWithPushDisabled(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig(t, f)
	h := api.New(cfg, f.Store, nil)
	owner := createExperience(t, h, cfg)
	if _, err := f.Store.PutPush(t.Context(), store.PushSubscription{MemorySessionID: owner.Memory.SessionID, EndpointEncrypted: []byte("encrypted-endpoint"), KeysEncrypted: []byte("encrypted-keys"), IntervalMinutes: 30}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got := request(t, h, "POST", notificationsStopPath, "", operatorTestHeaders())
	var retained int
	if got.Code != 200 {
		t.Fatalf("stop disabled Push: %d %s", got.Code, got.Body.String())
	}
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM push_subscriptions`).Scan(&retained); err != nil || retained != 0 {
		t.Fatalf("disabled Push retained credentials: %d %v", retained, err)
	}
}
