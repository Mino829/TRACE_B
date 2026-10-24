package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/route"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func markerFixture(t *testing.T) (config.Config, *testdb.Fixture, http.Handler) {
	t.Helper()
	f := testdb.Open(t)
	cfg := displayConfig()
	cfg.PhotoEnabled = true
	cfg.PhotoMarkersApproved = true
	cfg.PhotoMarkerWidthM = 5
	cfg.PhotoMarkerTTL = 15 * time.Minute
	cfg.PhotoMarkerMinSessions = 3
	graph, err := route.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	cfg.CoordinateVersion, cfg.CampusGraphVersion, cfg.ThreeDMapVersion = graph.CoordinateVersion, graph.CampusGraphVersion, graph.ThreeDMapVersion
	if err := f.Store.ConfigureRoutes(graph, cfg.CoordinateVersion, cfg.CampusGraphVersion, cfg.ThreeDMapVersion); err != nil {
		t.Fatal(err)
	}
	f.Store.EnableDisplay()
	if err := f.Store.ConfigureMarkers(store.MarkerConfig{Approved: true, WidthM: cfg.PhotoMarkerWidthM,
		TTL: cfg.PhotoMarkerTTL, SparseMinSessions: cfg.PhotoMarkerMinSessions,
		CoordinateVersion: cfg.CoordinateVersion, GraphVersion: cfg.CampusGraphVersion, ThreeDVersion: cfg.ThreeDMapVersion}); err != nil {
		t.Fatal(err)
	}
	return cfg, f, api.New(cfg, f.Store, nil)
}

func markerRequest(t *testing.T, h http.Handler, cfg config.Config, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	ts := fmt.Sprint(time.Now().Unix())
	return request(t, h, method, path, body, map[string]string{
		"X-Trace-Display-Issuer-Timestamp": ts,
		"X-Trace-Display-Issuer-Signature": signMessage(cfg.DisplayIssuerKey, method+"\n"+strings.Split(path, "?")[0]+"\n"+ts),
	})
}

func markerControls(t *testing.T, h http.Handler, cfg config.Config) store.MarkerControls {
	t.Helper()
	response := markerRequest(t, h, cfg, http.MethodGet, "/v1/operator/photo-markers/status", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status: %d %s", response.Code, response.Body.String())
	}
	var controls store.MarkerControls
	if err := json.Unmarshal(response.Body.Bytes(), &controls); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(response.Body.String(), "latitude") || strings.Contains(response.Body.String(), "longitude") {
		t.Fatalf("status leaked raw location: %s", response.Body.String())
	}
	return controls
}

func TestPhotoMarkerOperatorAuthenticationAndInput(t *testing.T) {
	cfg, _, h := markerFixture(t)
	pins := "/v1/operator/photo-markers/start"
	if got := request(t, h, http.MethodPost, pins, "", nil); got.Code != http.StatusForbidden {
		t.Fatalf("unsigned start: %d", got.Code)
	}
	ts := fmt.Sprint(time.Now().Unix())
	for _, headers := range []map[string]string{
		{"Authorization": "Bearer renderer"},
		{"X-Trace-Display-Issuer-Timestamp": ts, "X-Trace-Display-Issuer-Signature": signMessage(cfg.DisplayIssuerKey, "POST\n/v1/operator/photo-hotspots/start\n"+ts)},
		{"X-Trace-Display-Issuer-Timestamp": fmt.Sprint(time.Now().Add(-time.Minute).Unix()), "X-Trace-Display-Issuer-Signature": "invalid"},
		{"X-Trace-Display-Issuer-Timestamp": ts, "X-Trace-Display-Issuer-Signature": signMessage(cfg.DisplayIssuerKey, "POST\n"+pins+"\n"+ts), "Origin": "https://operator.example"},
	} {
		if got := request(t, h, http.MethodPost, pins, "", headers); got.Code != http.StatusForbidden {
			t.Fatalf("invalid issuer headers accepted: %d", got.Code)
		}
	}
	duplicate := httptest.NewRequest(http.MethodPost, pins, nil)
	duplicate.Header.Set("X-Trace-Display-Issuer-Timestamp", ts)
	duplicate.Header.Add("X-Trace-Display-Issuer-Signature", signMessage(cfg.DisplayIssuerKey, "POST\n"+pins+"\n"+ts))
	duplicate.Header.Add("X-Trace-Display-Issuer-Signature", "invalid")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, duplicate)
	if response.Code != http.StatusForbidden {
		t.Fatalf("duplicate issuer signature accepted: %d", response.Code)
	}
	if got := markerRequest(t, h, cfg, http.MethodPost, pins+"?unused=true", ""); got.Code != http.StatusBadRequest {
		t.Fatalf("unsigned query accepted: %d", got.Code)
	}
	if got := markerRequest(t, h, cfg, http.MethodPost, "/v1/operator/photo-markers/restart", ""); got.Code != http.StatusBadRequest {
		t.Fatalf("unknown action: %d", got.Code)
	}
	if got := markerRequest(t, h, cfg, http.MethodPost, pins, `{}`); got.Code != http.StatusBadRequest {
		t.Fatalf("unexpected start payload: %d", got.Code)
	}
	for _, body := range []string{`{}`, `{"sparse":null}`, `{"sparse":"false"}`, `{"sparse":false,"unknown":1}`} {
		if got := markerRequest(t, h, cfg, http.MethodPost, "/v1/operator/photo-markers/sparse", body); got.Code != http.StatusBadRequest {
			t.Fatalf("invalid sparse payload %s: %d", body, got.Code)
		}
	}
	controls := markerControls(t, h, cfg)
	if controls.PinsEnabled || controls.HotspotsEnabled || !controls.SparseOverride || controls.PinsVisible || controls.HotspotsVisible {
		t.Fatalf("unsafe default controls: %+v", controls)
	}
}

func TestPhotoMarkerOperatorIndependentSwitchesAndWithdrawalStops(t *testing.T) {
	cfg, f, h := markerFixture(t)
	for _, path := range []string{"/v1/operator/photo-markers/start", "/v1/operator/photo-hotspots/start"} {
		if got := markerRequest(t, h, cfg, http.MethodPost, path, ""); got.Code != http.StatusOK {
			t.Fatalf("start %s: %d %s", path, got.Code, got.Body.String())
		}
	}
	controls := markerControls(t, h, cfg)
	if !controls.PinsEnabled || !controls.HotspotsEnabled || !controls.SparseOverride || !controls.PolicyApproved {
		t.Fatalf("starts must not automatically release sparse latch: %+v", controls)
	}
	if got := markerRequest(t, h, cfg, http.MethodPost, "/v1/operator/photo-markers/sparse", `{"sparse":false}`); got.Code != http.StatusOK {
		t.Fatalf("manual sparse release: %d %s", got.Code, got.Body.String())
	}
	if got := markerRequest(t, h, cfg, http.MethodPost, "/v1/operator/photo-markers/stop", ""); got.Code != http.StatusOK {
		t.Fatalf("stop pins: %d", got.Code)
	}
	controls = markerControls(t, h, cfg)
	if controls.PinsEnabled || !controls.HotspotsEnabled || controls.SparseOverride {
		t.Fatalf("independent controls lost: %+v", controls)
	}
	cfg.PhotoMarkersApproved = false
	cfg.PhotoEnabled = false
	cfg.DisplayEnabled = false
	h = api.New(cfg, f.Store, nil)
	for _, path := range []string{"/v1/operator/photo-markers/status", "/v1/operator/photo-hotspots/start"} {
		method := http.MethodPost
		if strings.HasSuffix(path, "/status") {
			method = http.MethodGet
		}
		if got := markerRequest(t, h, cfg, method, path, ""); got.Code != http.StatusForbidden {
			t.Fatalf("withdrawn policy permitted %s: %d", path, got.Code)
		}
	}
	if got := markerRequest(t, h, cfg, http.MethodPost, "/v1/operator/photo-markers/sparse", `{"sparse":false}`); got.Code != http.StatusForbidden {
		t.Fatalf("withdrawn policy released latch: %d", got.Code)
	}
	for _, path := range []string{"/v1/operator/photo-hotspots/stop", "/v1/operator/photo-markers/stop"} {
		if got := markerRequest(t, h, cfg, http.MethodPost, path, ""); got.Code != http.StatusOK {
			t.Fatalf("withdrawn policy blocked safety stop %s: %d %s", path, got.Code, got.Body.String())
		}
	}
	if got := markerRequest(t, h, cfg, http.MethodPost, "/v1/operator/photo-markers/sparse", `{"sparse":true}`); got.Code != http.StatusOK {
		t.Fatalf("withdrawn policy blocked safety latch: %d %s", got.Code, got.Body.String())
	}
	var pins, hotspots, sparse bool
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT pins_enabled,hotspots_enabled,sparse_override FROM marker_control WHERE id=TRUE`).Scan(&pins, &hotspots, &sparse); err != nil || pins || hotspots || !sparse {
		t.Fatalf("safety controls did not persist: pins=%t hotspots=%t sparse=%t error=%v", pins, hotspots, sparse, err)
	}
}

func TestPhotoMarkerOperatorPolicyMismatchDoesNotPermitStart(t *testing.T) {
	cfg, f, h := markerFixture(t)
	if got := markerRequest(t, h, cfg, http.MethodPost, "/v1/operator/photo-markers/start", ""); got.Code != http.StatusOK {
		t.Fatalf("initial start: %d %s", got.Code, got.Body.String())
	}
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE marker_control SET width_m=6 WHERE id=TRUE`); err != nil {
		t.Fatal(err)
	}
	if got := markerRequest(t, h, cfg, http.MethodPost, "/v1/operator/photo-hotspots/start", ""); got.Code != http.StatusConflict {
		t.Fatalf("policy mismatch start: %d %s", got.Code, got.Body.String())
	}
	if got := markerRequest(t, h, cfg, http.MethodGet, "/v1/operator/photo-markers/status", ""); got.Code != http.StatusConflict {
		t.Fatalf("policy mismatch status: %d %s", got.Code, got.Body.String())
	}
	if got := markerRequest(t, h, cfg, http.MethodPost, "/v1/operator/photo-markers/stop", ""); got.Code != http.StatusOK {
		t.Fatalf("policy mismatch blocked safety stop: %d %s", got.Code, got.Body.String())
	}
}

func TestPhotoMarkersStayInvisibleAfterRestartWithoutApproval(t *testing.T) {
	cfg, f, h := markerFixture(t)
	if got := markerRequest(t, h, cfg, http.MethodPost, "/v1/operator/photo-markers/start", ""); got.Code != http.StatusOK {
		t.Fatalf("initial start: %d %s", got.Code, got.Body.String())
	}
	if got := markerRequest(t, h, cfg, http.MethodPost, "/v1/operator/photo-markers/sparse", `{"sparse":false}`); got.Code != http.StatusOK {
		t.Fatalf("manual sparse release: %d %s", got.Code, got.Body.String())
	}
	// Simulate materialized visibility that survived a previous process.
	if _, err := f.SQL.ExecContext(t.Context(), `INSERT INTO marker_visible(kind,public_id,x,z) VALUES ('pin','opaque-restart',2.5,2.5)`); err != nil {
		t.Fatal(err)
	}
	restarted, err := store.Open(t.Context(), f.URL, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	controls, err := restarted.MarkerStatus(t.Context())
	if err != nil || controls.PolicyApproved || controls.PinsVisible || controls.HotspotsVisible {
		t.Fatalf("withdrawn policy on restart exposed markers: %+v, %v", controls, err)
	}
	var visible int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM marker_visible`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("retained visibility not erased on restart: count=%d err=%v", visible, err)
	}
}

// The renderer consumes the same cursor for photo deltas and snapshots; stopping
// one channel must retract it without hiding the other.
func TestPhotoMarkerWebsocketSnapshotAndHotspotStop(t *testing.T) {
	cfg, f, h := markerFixture(t)
	for _, path := range []string{"/v1/operator/photo-markers/start", "/v1/operator/photo-hotspots/start"} {
		if got := markerRequest(t, h, cfg, http.MethodPost, path, ""); got.Code != http.StatusOK {
			t.Fatalf("enable %s: %d %s", path, got.Code, got.Body.String())
		}
	}
	if got := markerRequest(t, h, cfg, http.MethodPost, "/v1/operator/photo-markers/sparse", `{"sparse":false}`); got.Code != http.StatusOK {
		t.Fatalf("release sparse latch: %d %s", got.Code, got.Body.String())
	}
	server := displayServer(t, h)
	token := displayToken(t, h, cfg)
	conn, response, err := dialDisplay(t, server.URL, token, cfg)
	if err != nil {
		t.Fatalf("dial: %v status=%v", err, response)
	}
	defer conn.Close()
	snapshot := nextDisplay(t, conn, "display.snapshot")
	var initial struct {
		Pins     []store.MarkerPoint `json:"pins"`
		Hotspots []store.MarkerPoint `json:"hotspots"`
	}
	if err := json.Unmarshal(snapshot.Data, &initial); err != nil || len(initial.Pins) != 0 || len(initial.Hotspots) != 0 {
		t.Fatalf("initial snapshot: %s %v", snapshot.Data, err)
	}
	now := time.Now().UTC()
	for i := 1; i <= 3; i++ {
		memory := fmt.Sprintf("ws-marker-memory-%d", i)
		photoID := fmt.Sprintf("ws-marker-photo-%d", i)
		if err := f.Store.CreateExperience(t.Context(), store.NewExperience{
			ExperienceID: fmt.Sprintf("ws-marker-experience-%d", i), MemorySessionID: memory,
			MemoryTokenHash: fmt.Sprintf("ws-marker-token-%d", i), MemoryConsent: "memory-v1",
			ConsentLanguage: "ja", CreatedAt: now, MemoryExpiresAt: now.Add(time.Hour),
			MemoryDeleteAfter: now.Add(2 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		photo := store.Photo{
			ID: photoID, MemorySessionID: memory, ClientPhotoID: photoID, RequestHash: photoID,
			CapturedAt: now, UploadExpiresAt: now.Add(4 * time.Minute),
			Location: &store.PhotoLocation{Latitude: 35.6607341, Longitude: 139.7945577, AccuracyM: 3, RecordedAt: now},
			MIMEType: "image/jpeg", SizeBytes: 500, Width: 16, Height: 12, SHA256: "digest",
			StageKey: "stage-" + photoID, ObjectKey: "object-" + photoID,
		}
		if _, err := f.Store.CreatePhoto(t.Context(), photo, now); err != nil {
			t.Fatal(err)
		}
		if err := f.Store.ReadyPhoto(t.Context(), memory, photoID, now); err != nil {
			t.Fatal(err)
		}
	}
	updated := nextDisplay(t, conn, "photo.hotspot.updated")
	if strings.Contains(string(updated.Data), "ws-marker-memory") || strings.Contains(string(updated.Data), "ws-marker-photo") {
		t.Fatalf("private ID leaked: %s", updated.Data)
	}
	var hotspot store.MarkerPoint
	if err := json.Unmarshal(updated.Data, &hotspot); err != nil || hotspot.ID == "" || hotspot.Kind != "hotspot" {
		t.Fatalf("hotspot frame: %s %v", updated.Data, err)
	}
	for range 3 {
		nextDisplay(t, conn, "photo.marker.created")
	}
	if got := markerRequest(t, h, cfg, http.MethodPost, "/v1/operator/photo-hotspots/stop", ""); got.Code != http.StatusOK {
		t.Fatalf("stop hotspot: %d %s", got.Code, got.Body.String())
	}
	hidden := nextDisplay(t, conn, "photo.hotspot.hidden")
	var tombstone map[string]string
	if err := json.Unmarshal(hidden.Data, &tombstone); err != nil || tombstone["id"] != hotspot.ID || len(tombstone) != 1 {
		t.Fatalf("hotspot hidden frame: %s %v", hidden.Data, err)
	}
	reconnected, response, err := dialDisplay(t, server.URL, token, cfg)
	if err != nil {
		t.Fatalf("reconnect: %v status=%v", err, response)
	}
	defer reconnected.Close()
	var current struct {
		Pins     []store.MarkerPoint `json:"pins"`
		Hotspots []store.MarkerPoint `json:"hotspots"`
	}
	if err := json.Unmarshal(nextDisplay(t, reconnected, "display.snapshot").Data, &current); err != nil || len(current.Pins) != 3 || len(current.Hotspots) != 0 {
		t.Fatalf("reconnected snapshot: pins=%d hotspots=%d err=%v", len(current.Pins), len(current.Hotspots), err)
	}
}
