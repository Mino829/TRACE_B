package api_test

import (
	"encoding/json"
	"errors"
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

func TestMemoryStopIsTerminalButRetainsOwnerReadsAndDeletion(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	cfg.RegistrationOpen = true
	cfg.PhotoMetadataEnabled = true
	photoGraphFixture(t, f, &cfg)
	h := api.New(cfg, f.Store, nil)
	owner := createExperience(t, h, cfg)
	if owner.Memory.Status != "main" {
		t.Fatalf("enrollment participant status: %q", owner.Memory.Status)
	}
	other := createExperience(t, h, cfg)
	memoryPath := "/v1/memory-sessions/" + owner.Memory.SessionID
	photoPath := "/v2/memory-sessions/" + owner.Memory.SessionID + "/photos"
	headers := map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": "stop-owner"}
	state := request(t, h, "GET", "/v1/experiences/"+owner.ExperienceID, "", headers)
	if state.Code != 200 || !strings.Contains(state.Body.String(), `"status":"main"`) || !strings.Contains(state.Body.String(), `"replayStatus":null`) || strings.Contains(state.Body.String(), "displayAlias") {
		t.Fatalf("initial participant state: %d %s", state.Code, state.Body.String())
	}
	var color, symbol string
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT display_color,display_symbol FROM experiences WHERE id=$1`, owner.ExperienceID).Scan(&color, &symbol); err != nil || color != "" || symbol != "" {
		t.Fatalf("new participant received a retired alias: %q %q %v", color, symbol, err)
	}
	photo := request(t, h, "POST", photoPath, devicePhotoBody("before-stop", "cp-a", time.Now().UTC()), headers)
	if photo.Code != 201 {
		t.Fatalf("photo before stop: %d %s", photo.Code, photo.Body.String())
	}
	var moment struct {
		ID string `json:"photoMomentId"`
	}
	if err := json.Unmarshal(photo.Body.Bytes(), &moment); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.PutPush(t.Context(), store.PushSubscription{MemorySessionID: owner.Memory.SessionID, IntervalMinutes: 30, EndpointEncrypted: []byte("encrypted-endpoint"), KeysEncrypted: []byte("encrypted-keys")}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body    string
		headers map[string]string
		status  int
	}{
		{"", nil, 401},
		{"", map[string]string{"Authorization": "Bearer " + other.Memory.ParticipantToken, "Idempotency-Key": "cross-owner"}, 401},
		{"{}", headers, 400},
		{"", map[string]string{"Authorization": headers["Authorization"]}, 400},
	} {
		response := request(t, h, "POST", memoryPath+"/stop", tc.body, tc.headers)
		if response.Code != tc.status {
			t.Fatalf("stop rejected request: got=%d want=%d body=%s", response.Code, tc.status, response.Body.String())
		}
	}
	before, err := f.Store.MemorySession(t.Context(), owner.Memory.SessionID)
	if err != nil || before.StoppedAt != nil {
		t.Fatalf("rejected stop changed participant: %+v %v", before, err)
	}
	type result struct {
		code int
		body string
	}
	results := make(chan result, 4)
	var workers sync.WaitGroup
	for i := range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			response := request(t, api.New(cfg, f.Store, nil), "POST", memoryPath+"/stop", "", map[string]string{"Authorization": headers["Authorization"], "Idempotency-Key": fmt.Sprintf("stop-%d", i%2)})
			results <- result{response.Code, response.Body.String()}
		}()
	}
	workers.Wait()
	close(results)
	var stableBody string
	for response := range results {
		if response.code != 200 || (stableBody != "" && response.body != stableBody) {
			t.Fatalf("concurrent stop was not stable: %+v first=%s", response, stableBody)
		}
		stableBody = response.body
	}
	after, err := f.Store.MemorySession(t.Context(), owner.Memory.SessionID)
	if err != nil || after.StoppedAt == nil || !after.ExpiresAt.Equal(before.ExpiresAt) || !after.DeleteAfter.Equal(before.DeleteAfter) {
		t.Fatalf("stop shortened retention or lost timestamp: before=%+v after=%+v err=%v", before, after, err)
	}
	var subscriptions int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM push_subscriptions WHERE memory_session_id=$1`, owner.Memory.SessionID).Scan(&subscriptions); err != nil || subscriptions != 0 {
		t.Fatalf("stop left push credentials: %d %v", subscriptions, err)
	}
	if _, err := f.Store.PutPush(t.Context(), store.PushSubscription{MemorySessionID: owner.Memory.SessionID, IntervalMinutes: 30}, time.Now().UTC()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stopped participant resubscribed: %v", err)
	}
	status := "active"
	if _, err := f.Store.PatchPush(t.Context(), owner.Memory.SessionID, nil, &status, time.Now().UTC()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stopped participant patched push: %v", err)
	}
	if _, err := f.Store.AcceptArrival(t.Context(), owner.Memory.SessionID, "", "late-arrival", "booth", "late-key", "late-request", time.Now().UTC()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stopped participant first arrival succeeded: %v", err)
	}
	response := request(t, api.New(cfg, f.Store, nil), "POST", photoPath, devicePhotoBody("after-stop", "cp-a", time.Now().UTC()), map[string]string{"Authorization": headers["Authorization"], "Idempotency-Key": "photo-after-stop"})
	if response.Code != 409 || !strings.Contains(response.Body.String(), `"STATE_CONFLICT"`) {
		t.Fatalf("stopped participant created photo: %d %s", response.Code, response.Body.String())
	}
	state = request(t, h, "GET", "/v1/experiences/"+owner.ExperienceID, "", headers)
	if state.Code != 200 || !strings.Contains(state.Body.String(), `"status":"ended"`) || !strings.Contains(state.Body.String(), `"replayStatus":null`) {
		t.Fatalf("stopped state unavailable: %d %s", state.Code, state.Body.String())
	}
	for _, path := range []string{photoPath, "/v2/memory-sessions/" + owner.Memory.SessionID + "/memory"} {
		response := request(t, h, "GET", path, "", headers)
		if response.Code != 200 || !strings.Contains(response.Body.String(), moment.ID) {
			t.Fatalf("stopped owner lost photo read: %s %d %s", path, response.Code, response.Body.String())
		}
	}
	if response := request(t, h, "DELETE", photoPath+"/"+moment.ID, "", headers); response.Code != 204 {
		t.Fatalf("stopped owner lost photo delete: %d %s", response.Code, response.Body.String())
	}
	for range 2 {
		if response := request(t, h, "DELETE", memoryPath, "", headers); response.Code != 204 {
			t.Fatalf("stopped owner lost whole delete/replay: %d %s", response.Code, response.Body.String())
		}
	}
	if response := request(t, h, "POST", memoryPath+"/stop", "", map[string]string{"Authorization": headers["Authorization"], "Idempotency-Key": "stop-0"}); response.Code != 401 {
		t.Fatalf("stop receipt survived whole deletion: %d %s", response.Code, response.Body.String())
	}
	var receipts int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM idempotency_records WHERE memory_session_id=$1`, owner.Memory.SessionID).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("whole delete retained participant/photo receipts: %d %v", receipts, err)
	}
}

func TestLegacyLiveStateRemainsIndependentAfterMemoryDeletion(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	h := api.New(cfg, f.Store, nil)
	owner := seedLegacyExperience(t, f.Store, cfg)
	path := "/v1/live-presence-sessions/" + owner.Live.SessionID
	if err := f.Store.SavePosition(t.Context(), owner.Live.SessionID, store.Position{X: 123, Z: 456, ReceivedAt: time.Now().UTC(), RecordedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.DeleteMemory(t.Context(), owner.Memory.SessionID); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", owner.Memory.ParticipantToken, "unrelated"} {
		response := request(t, h, http.MethodGet, path, "", map[string]string{"Authorization": "Bearer " + token})
		if response.Code == 200 {
			t.Fatalf("foreign token read legacy live: %s", response.Body.String())
		}
	}
	headers := map[string]string{"Authorization": "Bearer " + owner.Live.ParticipantToken, "Idempotency-Key": "legacy-stop-independent"}
	assertState := func(expected string) {
		t.Helper()
		response := request(t, h, "GET", path, "", headers)
		var state map[string]json.RawMessage
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &state) != nil || len(state) != 4 || string(state["trackingStatus"]) != `"`+expected+`"` || state["sessionId"] == nil || state["expiresAt"] == nil || state["deleteAfter"] == nil {
			t.Fatalf("legacy state contract: %d %s", response.Code, response.Body.String())
		}
	}
	assertState("tracking")
	if response := request(t, h, "POST", path+"/stop", `{"reason":"participant_request"}`, headers); response.Code != 200 {
		t.Fatalf("independent legacy stop: %d %s", response.Code, response.Body.String())
	}
	assertState("stopped")
	for range 2 {
		if response := request(t, h, "DELETE", path, "", headers); response.Code != 204 {
			t.Fatalf("independent legacy delete/replay: %d %s", response.Code, response.Body.String())
		}
	}
}
