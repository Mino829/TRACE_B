package api_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
	"github.com/gorilla/websocket"
)

func displayConfig() config.Config {
	cfg := arrivalConfig()
	cfg.DisplayEnabled = true
	cfg.DisplayIssuerKey = "display-issuer-key-not-arrival-issuer-123456"
	cfg.DisplayTokenKey = "display-token-signing-key-separate-987654"
	return cfg
}

// Wait for hijacked WS handlers before testdb drops their PostgreSQL schema.
func displayServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	var active sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/display/ws" {
			active.Add(1)
			defer active.Done()
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		server.Close()
		done := make(chan struct{})
		go func() { active.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("WebSocket handler did not release database before cleanup")
		}
	})
	return server
}

func displayToken(t *testing.T, h http.Handler, cfg config.Config) string {
	t.Helper()
	path := "/v1/operator/display-tokens"
	ts := fmt.Sprint(time.Now().Unix())
	r := request(t, h, "POST", path, "", map[string]string{"X-Trace-Display-Issuer-Timestamp": ts, "X-Trace-Display-Issuer-Signature": signMessage(cfg.DisplayIssuerKey, "POST\n"+path+"\n"+ts)})
	if r.Code != 200 {
		t.Fatalf("display token: %d %s", r.Code, r.Body.String())
	}
	var body struct {
		Token string `json:"displayToken"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil || body.Token == "" {
		t.Fatalf("display token response: %v", err)
	}
	return body.Token
}
func dialDisplay(t *testing.T, url, token string, cfg config.Config) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("X-Trace-Schema-Version", "1")
	headers.Set("X-Trace-Coordinate-Version", cfg.CoordinateVersion)
	headers.Set("X-Trace-Campus-Graph-Version", cfg.CampusGraphVersion)
	headers.Set("X-Trace-Three-D-Map-Version", cfg.ThreeDMapVersion)
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(url, "http")+"/v1/display/ws", headers)
	return conn, response, err
}

type displayFrameTest struct {
	Type              string          `json:"type"`
	EventID           string          `json:"eventId"`
	Data              json.RawMessage `json:"data"`
	SchemaVersion     int             `json:"schemaVersion"`
	CoordinateVersion string          `json:"coordinateVersion"`
}

func nextDisplay(t *testing.T, conn *websocket.Conn, kind string) displayFrameTest {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(6 * time.Second))
	for range 40 {
		var frame displayFrameTest
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("waiting for %s: %v", kind, err)
		}
		if frame.Type == kind {
			return frame
		}
	}
	t.Fatalf("missing frame %q", kind)
	return displayFrameTest{}
}
func displayInput(t *testing.T, conn *websocket.Conn, cfg config.Config, kind, eventID, arrivalID, leaseID string) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{"schemaVersion": 1, "type": kind, "eventId": eventID, "arrivalId": arrivalID, "leaseId": leaseID,
		"coordinateVersion": cfg.CoordinateVersion, "campusGraphVersion": cfg.CampusGraphVersion, "threeDMapVersion": cfg.ThreeDMapVersion}); err != nil {
		t.Fatal(err)
	}
}
func assertDisplaySafe(t *testing.T, frame displayFrameTest, experience createdExperience) {
	t.Helper()
	raw := string(frame.Data)
	for _, forbidden := range []string{"latitude", "longitude", "uploadUrl", "imageUrl", "photoBytes", experience.Memory.ParticipantToken, experience.Live.ParticipantToken, experience.Live.SessionID, experience.Memory.SessionID} {
		if forbidden != "" && strings.Contains(raw, forbidden) {
			t.Fatalf("unsafe display payload (%s): %s", forbidden, raw)
		}
	}
}

func TestDisplayNativeWebsocketSnapshotFanoutDeletionAndReconnect(t *testing.T) {
	f := testdb.Open(t)
	cfg := displayConfig()
	handler := api.New(cfg, f.Store, nil)
	server := displayServer(t, handler)
	token := displayToken(t, handler, cfg)
	conn, response, err := dialDisplay(t, server.URL, token, cfg)
	if err != nil {
		t.Fatalf("dial: %v status=%v", err, response)
	}
	defer conn.Close()
	snapshot := nextDisplay(t, conn, "display.snapshot")
	if snapshot.EventID == "" || snapshot.SchemaVersion != 1 || snapshot.CoordinateVersion != cfg.CoordinateVersion {
		t.Fatalf("versioned snapshot missing: %+v", snapshot)
	}
	created := seedLegacyExperience(t, f.Store, cfg)
	body := fmt.Sprintf(`{"point":{"latitude":35.0001,"longitude":135.0001,"accuracyM":8,"recordedAt":%q}}`, time.Now().UTC().Format(time.RFC3339Nano))
	path := "/v1/live-presence-sessions/" + created.Live.SessionID
	if got := request(t, handler, "POST", path+"/position", body, map[string]string{"Authorization": "Bearer " + created.Live.ParticipantToken}); got.Code != http.StatusGone || !strings.Contains(got.Body.String(), `"code":"POSITION_DISABLED"`) {
		t.Fatalf("position retirement: %d %s", got.Code, got.Body.String())
	}
	now := time.Now().UTC()
	if err := f.Store.SavePosition(t.Context(), created.Live.SessionID, store.Position{
		X: 10, Z: 10, AccuracyM: 8, RecordedAt: now, ReceivedAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	updated := nextDisplay(t, conn, "position.updated")
	assertDisplaySafe(t, updated, created)
	var position struct {
		LivePresenceID string  `json:"livePresenceId"`
		X              float64 `json:"x"`
		Z              float64 `json:"z"`
	}
	if err := json.Unmarshal(updated.Data, &position); err != nil || position.LivePresenceID == "" || position.LivePresenceID == created.Live.SessionID {
		t.Fatalf("display-scoped position: %v %+v", err, position)
	}
	var saved int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events WHERE kind LIKE 'position.%'`).Scan(&saved); err != nil || saved != 0 {
		t.Fatalf("position history written to PostgreSQL: count=%d error=%v", saved, err)
	}
	var table *string
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT to_regclass('display_positions')::text`).Scan(&table); err != nil || table != nil {
		t.Fatalf("position table exists: %v %v", table, err)
	}
	standby, response, err := dialDisplay(t, server.URL, token, cfg)
	if err != nil {
		t.Fatalf("standby dial: %v %v", err, response)
	}
	defer standby.Close()
	snapshot = nextDisplay(t, standby, "display.snapshot")
	assertDisplaySafe(t, snapshot, created)
	if !strings.Contains(string(snapshot.Data), position.LivePresenceID) {
		t.Fatalf("resync missing current position: %s", snapshot.Data)
	}
	if got := request(t, handler, "DELETE", path, "", map[string]string{"Authorization": "Bearer " + created.Live.ParticipantToken, "Idempotency-Key": "display-delete"}); got.Code != 204 {
		t.Fatalf("delete: %d %s", got.Code, got.Body.String())
	}
	removed := nextDisplay(t, conn, "tracking.stopped")
	if !strings.Contains(string(removed.Data), position.LivePresenceID) {
		t.Fatalf("delete tombstone wrong: %s", removed.Data)
	}
	if removed.EventID == updated.EventID {
		t.Fatal("event IDs reused")
	}
	conn.Close()
	standby.Close()
	restored, response, err := dialDisplay(t, server.URL, token, cfg)
	if err != nil {
		t.Fatalf("reconnect: %v %v", err, response)
	}
	defer restored.Close()
	snapshot = nextDisplay(t, restored, "display.snapshot")
	if strings.Contains(string(snapshot.Data), position.LivePresenceID) {
		t.Fatalf("deleted position returned on reconnect: %s", snapshot.Data)
	}
}

func TestDisplayNativeWebsocketTTLRemovalAndFreshSnapshot(t *testing.T) {
	f := testdb.Open(t)
	cfg := displayConfig()
	handler := api.New(cfg, f.Store, nil)
	server := displayServer(t, handler)
	conn, response, err := dialDisplay(t, server.URL, displayToken(t, handler, cfg), cfg)
	if err != nil {
		t.Fatalf("dial: %v %v", err, response)
	}
	defer conn.Close()
	nextDisplay(t, conn, "display.snapshot")
	created := seedLegacyExperience(t, f.Store, cfg)
	now := time.Now().UTC()
	if err := f.Store.SavePosition(t.Context(), created.Live.SessionID, store.Position{
		X: 12, Z: 15, AccuracyM: 8, RecordedAt: now, ReceivedAt: now, ExpiresAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	updated := nextDisplay(t, conn, "position.updated")
	removed := nextDisplay(t, conn, "position.expired")
	var point struct {
		LivePresenceID string `json:"livePresenceId"`
	}
	if err := json.Unmarshal(updated.Data, &point); err != nil || point.LivePresenceID == "" || !strings.Contains(string(removed.Data), point.LivePresenceID) {
		t.Fatalf("expiry tombstone: %v %s", err, removed.Data)
	}
	replacement, response, err := dialDisplay(t, server.URL, displayToken(t, handler, cfg), cfg)
	if err != nil {
		t.Fatalf("fresh snapshot: %v %v", err, response)
	}
	defer replacement.Close()
	snapshot := nextDisplay(t, replacement, "display.snapshot")
	if strings.Contains(string(snapshot.Data), point.LivePresenceID) {
		t.Fatalf("expired position restored: %s", snapshot.Data)
	}
}

func TestDisplayNativeGapResynchronizesWithSnapshot(t *testing.T) {
	f := testdb.Open(t)
	cfg := displayConfig()
	handler := api.New(cfg, f.Store, nil)
	server := displayServer(t, handler)
	conn, response, err := dialDisplay(t, server.URL, displayToken(t, handler, cfg), cfg)
	if err != nil {
		t.Fatalf("dial: %v %v", err, response)
	}
	defer conn.Close()
	initial := nextDisplay(t, conn, "display.snapshot")
	barrier, err := f.SQL.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback()
	digest := sha256.Sum256([]byte("display:refresh"))
	if _, err := barrier.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock($1)`, int64(binary.BigEndian.Uint64(digest[:8]))); err != nil {
		t.Fatal(err)
	}
	lost, err := f.Store.AppendDisplayEvent(t.Context(), "arrival.replay.requested", map[string]string{"arrivalId": "lost"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `DELETE FROM display_events WHERE id=$1`, lost); err != nil {
		t.Fatal(err)
	}
	last, err := f.Store.AppendDisplayEvent(t.Context(), "arrival.replay.requested", map[string]string{"arrivalId": "live"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := barrier.Commit(); err != nil {
		t.Fatal(err)
	}
	restored := nextDisplay(t, conn, "display.snapshot")
	if restored.EventID != fmt.Sprint(last) || restored.EventID == initial.EventID {
		t.Fatalf("gap did not force resync: initial=%s new=%s", initial.EventID, restored.EventID)
	}
}

func TestDisplayNativeWebsocketLeaseReplayAndVersionRejection(t *testing.T) {
	f := testdb.Open(t)
	cfg := displayConfig()
	handler := api.New(cfg, f.Store, nil)
	server := displayServer(t, handler)
	token := displayToken(t, handler, cfg)
	secondDB, err := store.Open(t.Context(), f.URL, 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { secondDB.Close() })
	standbyServer := displayServer(t, api.New(cfg, secondDB, nil))
	for _, invalid := range []string{"participant-token", cfg.ArrivalIssuerKey, cfg.DisplayIssuerKey} {
		conn, response, err := dialDisplay(t, server.URL, invalid, cfg)
		if conn != nil {
			conn.Close()
		}
		if err == nil || response == nil || response.StatusCode != 401 {
			t.Fatalf("credential boundary: %v %v", err, response)
		}
	}
	expiredPayload, _ := json.Marshal(map[string]any{"purpose": "display.ws", "issuedAt": time.Now().Add(-3 * time.Minute).Unix(),
		"expiresAt": time.Now().Add(-time.Minute).Unix(), "nonce": base64.RawURLEncoding.EncodeToString([]byte("sixteen-byte-nonce"))})
	encoded := base64.RawURLEncoding.EncodeToString(expiredPayload)
	expired := encoded + "." + signMessage(cfg.DisplayTokenKey, encoded)
	rejected, rejection, expiredErr := dialDisplay(t, server.URL, expired, cfg)
	if rejected != nil {
		rejected.Close()
	}
	if expiredErr == nil || rejection == nil || rejection.StatusCode != 401 {
		t.Fatalf("expired display token accepted: %v %v", expiredErr, rejection)
	}
	wrong := cfg
	wrong.ThreeDMapVersion = "incompatible"
	conn, response, err := dialDisplay(t, server.URL, token, wrong)
	if conn != nil {
		conn.Close()
	}
	if err == nil || response == nil || response.StatusCode != 409 {
		t.Fatalf("version not rejected: %v %v", err, response)
	}
	primary, response, err := dialDisplay(t, server.URL, token, cfg)
	if err != nil {
		t.Fatalf("dial primary: %v %v", err, response)
	}
	defer primary.Close()
	nextDisplay(t, primary, "display.snapshot")
	standby, response, err := dialDisplay(t, standbyServer.URL, token, cfg)
	if err != nil {
		t.Fatalf("dial standby: %v %v", err, response)
	}
	defer standby.Close()
	nextDisplay(t, standby, "display.snapshot")
	displayInput(t, primary, cfg, "renderer.primary.acquire", "", "", "")
	leaseFrame := nextDisplay(t, primary, "renderer.primary.granted")
	var lease struct {
		ID string `json:"leaseId"`
	}
	if err := json.Unmarshal(leaseFrame.Data, &lease); err != nil || lease.ID == "" {
		t.Fatalf("primary lease: %v %+v", err, lease)
	}
	displayInput(t, standby, cfg, "renderer.primary.acquire", "", "", "")
	nextDisplay(t, standby, "renderer.primary.conflict")
	created := createExperience(t, handler, cfg)
	proof := projectedProof(t, handler, cfg)
	result := arriveRequest(t, handler, created, proof, "replay-arrival")
	if result.code != 200 {
		t.Fatalf("arrival: %d %s", result.code, result.body)
	}
	var arrival struct {
		ID string `json:"arrivalId"`
	}
	if err := json.Unmarshal([]byte(result.body), &arrival); err != nil || arrival.ID == "" {
		t.Fatalf("arrival id: %v", err)
	}
	requested := nextDisplay(t, primary, "arrival.replay.requested")
	assertDisplaySafe(t, requested, created)
	displayInput(t, standby, cfg, "arrival.replay.started", "event-standby", arrival.ID, lease.ID)
	nextDisplay(t, standby, "renderer.rejected")
	displayInput(t, primary, cfg, "arrival.replay.started", "event-start-0001", arrival.ID, lease.ID)
	ack := nextDisplay(t, primary, "renderer.ack")
	if !strings.Contains(string(ack.Data), "playing") {
		t.Fatalf("start not confirmed: %s", ack.Data)
	}
	displayInput(t, primary, cfg, "arrival.replay.started", "event-start-0001", arrival.ID, lease.ID)
	nextDisplay(t, primary, "renderer.ack")
	displayInput(t, primary, cfg, "arrival.replay.completed", "event-finish-0001", arrival.ID, lease.ID)
	ack = nextDisplay(t, primary, "renderer.ack")
	if !strings.Contains(string(ack.Data), "completed") {
		t.Fatalf("complete not confirmed: %s", ack.Data)
	}
	displayInput(t, primary, cfg, "arrival.replay.completed", "event-finish-0001", arrival.ID, lease.ID)
	nextDisplay(t, primary, "renderer.ack")
	var status string
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT replay_status FROM arrival_events WHERE id=$1`, arrival.ID).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("durable completion: %s %v", status, err)
	}
}

func TestDisplayDisconnectSkipsPlayingAndOperatorExplicitlyRetries(t *testing.T) {
	f := testdb.Open(t)
	cfg := displayConfig()
	handler := api.New(cfg, f.Store, nil)
	server := displayServer(t, handler)
	conn, response, err := dialDisplay(t, server.URL, displayToken(t, handler, cfg), cfg)
	if err != nil {
		t.Fatalf("dial: %v %v", err, response)
	}
	nextDisplay(t, conn, "display.snapshot")
	displayInput(t, conn, cfg, "renderer.primary.acquire", "", "", "")
	granted := nextDisplay(t, conn, "renderer.primary.granted")
	var lease struct {
		ID string `json:"leaseId"`
	}
	_ = json.Unmarshal(granted.Data, &lease)
	created := createExperience(t, handler, cfg)
	arrivalResult := arriveRequest(t, handler, created, projectedProof(t, handler, cfg), "disconnect-test")
	if arrivalResult.code != 200 {
		t.Fatalf("arrival: %s", arrivalResult.body)
	}
	var arrival struct {
		ID string `json:"arrivalId"`
	}
	_ = json.Unmarshal([]byte(arrivalResult.body), &arrival)
	displayInput(t, conn, cfg, "arrival.replay.started", "disconnect-start-001", arrival.ID, lease.ID)
	nextDisplay(t, conn, "renderer.ack")
	conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var status, reason string
		if err := f.SQL.QueryRowContext(t.Context(), `SELECT replay_status,COALESCE(skip_reason,'') FROM arrival_events WHERE id=$1`, arrival.ID).Scan(&status, &reason); err != nil {
			t.Fatal(err)
		}
		if status == "skipped" && reason == "renderer_failure" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disconnect did not fence playing replay: %s %s", status, reason)
		}
		time.Sleep(20 * time.Millisecond)
	}
	retryPath := "/v1/operator/arrivals/" + arrival.ID + "/retry"
	retried := request(t, handler, "POST", retryPath, "", issuerHeaders(cfg, "POST", retryPath))
	if retried.Code != 200 {
		t.Fatalf("operator retry: %d %s", retried.Code, retried.Body.String())
	}
	successor, response, err := dialDisplay(t, server.URL, displayToken(t, handler, cfg), cfg)
	if err != nil {
		t.Fatalf("successor dial: %v %v", err, response)
	}
	defer successor.Close()
	snapshot := nextDisplay(t, successor, "display.snapshot")
	if !strings.Contains(string(snapshot.Data), arrival.ID) {
		t.Fatalf("explicitly retried arrival not restored: %s", snapshot.Data)
	}
	displayInput(t, successor, cfg, "renderer.primary.acquire", "", "", "")
	nextLease := nextDisplay(t, successor, "renderer.primary.granted")
	var resumed struct {
		ID string `json:"leaseId"`
	}
	_ = json.Unmarshal(nextLease.Data, &resumed)
	displayInput(t, successor, cfg, "arrival.replay.started", "resume-start-0001", arrival.ID, resumed.ID)
	nextDisplay(t, successor, "renderer.ack")
	displayInput(t, successor, cfg, "arrival.replay.failed", "renderer-failed-01", arrival.ID, resumed.ID)
	ack := nextDisplay(t, successor, "renderer.ack")
	if !strings.Contains(string(ack.Data), "skipped") {
		t.Fatalf("failure not persisted: %s", ack.Data)
	}
	var reason string
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT skip_reason FROM arrival_events WHERE id=$1`, arrival.ID).Scan(&reason); err != nil || reason != "renderer_failure" {
		t.Fatalf("renderer failure reason: %s %v", reason, err)
	}
}
