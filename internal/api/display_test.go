package api_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	for _, forbidden := range []string{"latitude", "longitude", "uploadUrl", "imageUrl", "photoBytes", "displayAlias", experience.Memory.ParticipantToken, experience.Live.ParticipantToken, experience.Live.SessionID, experience.Memory.SessionID} {
		if forbidden != "" && strings.Contains(raw, forbidden) {
			t.Fatalf("unsafe display payload (%s): %s", forbidden, raw)
		}
	}
}

func TestDisplayDoesNotPublishLegacyPositionsButKeepsArrival(t *testing.T) {
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
	nextDisplay(t, conn, "display.snapshot")
	created := seedLegacyExperience(t, f.Store, cfg)
	path := "/v1/live-presence-sessions/" + created.Live.SessionID
	body := fmt.Sprintf(`{"point":{"latitude":35.0001,"longitude":135.0001,"accuracyM":8,"recordedAt":%q}}`, time.Now().UTC().Format(time.RFC3339Nano))
	if got := request(t, handler, "POST", path+"/position", body, map[string]string{"Authorization": "Bearer " + created.Live.ParticipantToken}); got.Code != http.StatusGone || !strings.Contains(got.Body.String(), `"code":"POSITION_DISABLED"`) {
		t.Fatalf("position retirement: %d %s", got.Code, got.Body.String())
	}
	now := time.Now().UTC()
	if err := f.Store.SavePosition(t.Context(), created.Live.SessionID, store.Position{
		X: 10, Z: 10, AccuracyM: 8, RecordedAt: now, ReceivedAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, position, err := f.Store.LiveSession(t.Context(), created.Live.SessionID); err != nil || position == nil || position.X != 10 {
		t.Fatalf("legacy owner position was lost: %+v %v", position, err)
	}
	standby, response, err := dialDisplay(t, server.URL, token, cfg)
	if err != nil {
		t.Fatalf("standby dial: %v %v", err, response)
	}
	defer standby.Close()
	snapshot := nextDisplay(t, standby, "display.snapshot")
	assertDisplaySafe(t, snapshot, created)
	var data map[string]json.RawMessage
	if err := json.Unmarshal(snapshot.Data, &data); err != nil {
		t.Fatal(err)
	}
	if _, present := data["participants"]; present {
		t.Fatalf("old positions published in snapshot: %s", snapshot.Data)
	}
	if err := f.Store.StopLive(t.Context(), created.Live.SessionID, "participant", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, position, err := f.Store.LiveSession(t.Context(), created.Live.SessionID); err != nil || position != nil {
		t.Fatalf("legacy stop did not erase position: %+v %v", position, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(750 * time.Millisecond))
	var leaked displayFrameTest
	if err := conn.ReadJSON(&leaked); err == nil {
		t.Fatalf("old position/phase event published: %+v", leaked)
	} else {
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("unexpected display read failure: %v", err)
		}
	}
	proof := projectedProof(t, handler, cfg)
	if arrived := arriveRequest(t, handler, created, proof, "legacy-display-arrival"); arrived.code != http.StatusOK {
		t.Fatalf("legacy memory arrival: %d %s", arrived.code, arrived.body)
	}
	assertDisplaySafe(t, nextDisplay(t, standby, "participant.arrived"), created)
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

// Equal checkpoint timestamps cannot identify a deleted photo on their own;
// display changes must carry the complete, current route without private IDs.
func TestDisplayDevicePhotoRouteArrivalSnapshotAndMutations(t *testing.T) {
	f := testdb.Open(t)
	cfg := displayConfig()
	cfg.PhotoMetadataEnabled = true
	photoGraphFixture(t, f, &cfg)
	cfg.ArrivalCheckpointID = "cp-b"
	handler := api.New(cfg, f.Store, nil)
	server := displayServer(t, handler)
	token := displayToken(t, handler, cfg)
	conn, response, err := dialDisplay(t, server.URL, token, cfg)
	if err != nil {
		t.Fatalf("dial: %v status=%v", err, response)
	}
	defer conn.Close()
	nextDisplay(t, conn, "display.snapshot")
	owner := createExperience(t, handler, cfg)
	base := "/v2/memory-sessions/" + owner.Memory.SessionID + "/photos"
	captured := time.Now().UTC()
	create := func(clientID, checkpoint string) string {
		t.Helper()
		r := request(t, handler, "POST", base, devicePhotoBody(clientID, checkpoint, captured),
			map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": clientID})
		if r.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", clientID, r.Code, r.Body.String())
		}
		var p struct {
			ID string `json:"photoMomentId"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &p); err != nil || p.ID == "" {
			t.Fatalf("photo ID: %+v %v", p, err)
		}
		return p.ID
	}
	first := create("display-first", "cp-a")
	second := create("display-second", "cp-b")
	proof := projectedProof(t, handler, cfg)
	arrival := arriveRequest(t, handler, owner, proof, "display-device-arrival")
	if arrival.code != http.StatusOK {
		t.Fatalf("arrival: %d %s", arrival.code, arrival.body)
	}
	verify := func(frame displayFrameTest, wantPath, wantPoints []string, privateIDs ...string) {
		t.Helper()
		assertDisplaySafe(t, frame, owner)
		for _, id := range privateIDs {
			if strings.Contains(string(frame.Data), id) {
				t.Fatalf("private photo ID in display: %s", frame.Data)
			}
		}
		var data struct {
			PathNodeIDs      []string `json:"pathNodeIds"`
			PhotoCheckpoints []struct {
				CheckpointID string    `json:"checkpointId"`
				CapturedAt   time.Time `json:"capturedAt"`
			} `json:"photoCheckpoints"`
			Arrivals []struct {
				PathNodeIDs      []string `json:"pathNodeIds"`
				PhotoCheckpoints []struct {
					CheckpointID string    `json:"checkpointId"`
					CapturedAt   time.Time `json:"capturedAt"`
				} `json:"photoCheckpoints"`
			} `json:"arrivals"`
		}
		if err := json.Unmarshal(frame.Data, &data); err != nil {
			t.Fatal(err)
		}
		points := data.PhotoCheckpoints
		if frame.Type == "display.snapshot" {
			if len(data.Arrivals) != 1 {
				t.Fatalf("snapshot arrivals: %s", frame.Data)
			}
			data.PathNodeIDs = data.Arrivals[0].PathNodeIDs
			points = data.Arrivals[0].PhotoCheckpoints
		}
		if fmt.Sprint(data.PathNodeIDs) != fmt.Sprint(wantPath) || len(points) != len(wantPoints) {
			t.Fatalf("wrong projected route: %s; want path %v checkpoints %v", frame.Data, wantPath, wantPoints)
		}
		for i, p := range points {
			if p.CheckpointID != wantPoints[i] || !p.CapturedAt.Equal(captured) {
				t.Fatalf("wrong checkpoint %d: %+v", i, p)
			}
		}
	}
	route := []string{"cp-a", "mid", "cp-b"}
	arrived := nextDisplay(t, conn, "participant.arrived")
	verify(arrived, route, []string{"cp-a", "cp-b"}, first, second)
	standby, response, err := dialDisplay(t, server.URL, token, cfg)
	if err != nil {
		t.Fatalf("reconnect: %v %v", err, response)
	}
	defer standby.Close()
	verify(nextDisplay(t, standby, "display.snapshot"), route, []string{"cp-a", "cp-b"}, first, second)

	if r := request(t, handler, "DELETE", base+"/"+first, "", map[string]string{
		"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": "display-delete-photo",
	}); r.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", r.Code, r.Body.String())
	}
	verify(nextDisplay(t, conn, "photo.moment.deleted"), []string{"cp-b"}, []string{"cp-b"}, first, second)
	verify(nextDisplay(t, standby, "photo.moment.deleted"), []string{"cp-b"}, []string{"cp-b"}, first, second)
	third := create("display-third", "cp-a")
	verify(nextDisplay(t, conn, "photo.moment.created"), []string{"cp-b", "mid", "cp-a", "mid", "cp-b"},
		[]string{"cp-b", "cp-a"}, first, second, third)
	verify(nextDisplay(t, standby, "photo.moment.created"), []string{"cp-b", "mid", "cp-a", "mid", "cp-b"},
		[]string{"cp-b", "cp-a"}, first, second, third)
	if r := request(t, handler, "POST", base, devicePhotoBody("display-third", "cp-a", captured),
		map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": "display-third"}); r.Code != http.StatusOK {
		t.Fatalf("idempotent replay: %d %s", r.Code, r.Body.String())
	}
	var createdEvents int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events WHERE kind='photo.moment.created'`).Scan(&createdEvents); err != nil || createdEvents != 1 {
		t.Fatalf("duplicate photo event: %d %v", createdEvents, err)
	}
	fourth := create("display-fourth", "cp-a")
	verify(nextDisplay(t, conn, "photo.moment.created"), []string{"cp-b", "mid", "cp-a", "mid", "cp-b"},
		[]string{"cp-b", "cp-a", "cp-a"}, first, second, third, fourth)
	if r := request(t, handler, "DELETE", base+"/"+third, "", map[string]string{
		"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": "display-delete-duplicate",
	}); r.Code != http.StatusNoContent {
		t.Fatalf("delete duplicate: %d %s", r.Code, r.Body.String())
	}
	verify(nextDisplay(t, conn, "photo.moment.deleted"), []string{"cp-b", "mid", "cp-a", "mid", "cp-b"},
		[]string{"cp-b", "cp-a"}, first, second, third, fourth)

	// A segment pinned to another map version must not leak a partial route.
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE moment_route_segments SET three_d_map_version='retired'
 WHERE memory_session_id=$1 AND to_photo_moment_id IS NULL`, owner.Memory.SessionID); err != nil {
		t.Fatal(err)
	}
	stale, response, err := dialDisplay(t, server.URL, token, cfg)
	if err != nil {
		t.Fatalf("reconnect with stale route: %v %v", err, response)
	}
	verify(nextDisplay(t, stale, "display.snapshot"), []string{}, []string{"cp-b", "cp-a"}, first, second, third, fourth)
	stale.Close()
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE moment_route_segments SET three_d_map_version=$2
 WHERE memory_session_id=$1 AND to_photo_moment_id IS NULL`, owner.Memory.SessionID, cfg.ThreeDMapVersion); err != nil {
		t.Fatal(err)
	}

	// A retired graph must not reinterpret saved checkpoint IDs as current nodes.
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET campus_graph_version='retired' WHERE id=$1`, owner.Memory.SessionID); err != nil {
		t.Fatal(err)
	}
	reconnected, response, err := dialDisplay(t, server.URL, token, cfg)
	if err != nil {
		t.Fatalf("reconnect after graph retirement: %v %v", err, response)
	}
	defer reconnected.Close()
	verify(nextDisplay(t, reconnected, "display.snapshot"), []string{}, []string{}, first, second, third, fourth)
}
