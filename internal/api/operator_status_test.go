package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/testdb"
	"github.com/gorilla/websocket"
)

const operatorStatusPath = "/v1/operator/status"

type operatorStatusResponse struct {
	ObservedAt   time.Time `json:"observedAt"`
	ArrivalQueue struct {
		Queued  int `json:"queued"`
		Playing int `json:"playing"`
	} `json:"arrivalQueue"`
	Display struct {
		Enabled            bool `json:"enabled"`
		Connections        int  `json:"connections"`
		PrimaryLeaseActive bool `json:"primaryLeaseActive"`
	} `json:"display"`
}

func TestOperatorStatusAuthorization(t *testing.T) {
	f := testdb.Open(t)
	cfg := displayConfig(t, f)
	cfg.EdgeSharedSecret = "independent-edge-secret-1234567890"
	h := api.New(cfg, f.Store, nil)
	for _, tc := range []struct {
		name   string
		change func(map[string]string)
		want   int
	}{
		{"missing edge", func(h map[string]string) { delete(h, "X-Trace-Edge-Secret") }, 403},
		{"edge alone", func(h map[string]string) { delete(h, "Authorization") }, 401},
		{"participant", func(h map[string]string) { h["Authorization"] = "Bearer participant-token" }, 401},
		{"display", func(h map[string]string) { h["Authorization"] = "Bearer " + displayToken(t, cfg) }, 401},
		{"renderer origin", func(h map[string]string) { h["Origin"] = cfg.DisplayAllowedOrigins[0] }, 403},
		{"legacy signature", func(h map[string]string) { h["X-Trace-Issuer-Signature"] = "obsolete" }, 401},
		{"legacy timestamp", func(h map[string]string) { h["X-Trace-Issuer-Timestamp"] = "1" }, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := operatorTestHeaders()
			headers["X-Trace-Edge-Secret"] = cfg.EdgeSharedSecret
			tc.change(headers)
			got := request(t, h, "GET", operatorStatusPath, "", headers)
			if got.Code != tc.want {
				t.Fatalf("status = %d: %s", got.Code, got.Body.String())
			}
		})
	}
	cfg.OperatorEnabled = false
	got := request(t, api.New(cfg, f.Store, nil), "GET", operatorStatusPath, "", map[string]string{"X-Trace-Edge-Secret": cfg.EdgeSharedSecret})
	if got.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled operator status = %d", got.Code)
	}
}

func TestOperatorStatusQueueAndLeaseExpiry(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig(t, f)
	h := api.New(cfg, f.Store, nil)
	for _, state := range []string{"queued", "playing", "completed", "skipped", "expired", "deleting"} {
		c := seedLegacyExperience(t, f.Store, cfg)
		proof := signProof(t, cfg, cfg.ArrivalVenue, time.Now().Add(-time.Second), time.Now().Add(20*time.Second))
		if got := arriveRequest(t, h, c, proof, "status-"+state); got.code != 200 {
			t.Fatalf("arrival: %d %s", got.code, got.body)
		}
		switch state {
		case "expired":
			if _, err := f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET expires_at=$2 WHERE id=$1`, c.Memory.SessionID, time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
		case "deleting":
			if _, err := f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET deleting=TRUE WHERE id=$1`, c.Memory.SessionID); err != nil {
				t.Fatal(err)
			}
		default:
			if _, err := f.SQL.ExecContext(t.Context(), `UPDATE arrival_events SET replay_status=$2 WHERE memory_session_id=$1`, c.Memory.SessionID, state); err != nil {
				t.Fatal(err)
			}
		}
	}
	read := func() operatorStatusResponse {
		t.Helper()
		got := request(t, h, "GET", operatorStatusPath, "", operatorTestHeaders())
		if got.Code != 200 {
			t.Fatalf("status: %d %s", got.Code, got.Body.String())
		}
		var status operatorStatusResponse
		if err := json.Unmarshal(got.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status.ArrivalQueue.Queued != 1 || status.ArrivalQueue.Playing != 1 || status.Display.Enabled || status.Display.Connections != 0 {
			t.Fatalf("unexpected status: %+v", status)
		}
		return status
	}
	if read().Display.PrimaryLeaseActive {
		t.Fatal("unowned lease is active")
	}
	now := time.Now().UTC()
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE display_primary_lease SET holder_id='private-renderer',lease_id='private-lease',expires_at=$1`, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !read().Display.PrimaryLeaseActive {
		t.Fatal("valid lease is inactive")
	}
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE display_primary_lease SET expires_at=$1`, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if read().Display.PrimaryLeaseActive {
		t.Fatal("expired lease is active before cleanup")
	}
	var playing int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM arrival_events WHERE replay_status='playing'`).Scan(&playing); err != nil || playing != 1 {
		t.Fatalf("status read mutated queue: %d %v", playing, err)
	}
}

func TestOperatorStatusAuthenticatedDisplayLifecycle(t *testing.T) {
	f := testdb.Open(t)
	cfg := displayConfig(t, f)
	h := api.New(cfg, f.Store, nil)
	server := displayServer(t, h)
	read := func() operatorStatusResponse {
		t.Helper()
		got := request(t, h, "GET", operatorStatusPath, "", operatorTestHeaders())
		if got.Code != 200 {
			t.Fatalf("status: %d %s", got.Code, got.Body.String())
		}
		var status operatorStatusResponse
		if err := json.Unmarshal(got.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status.ObservedAt.IsZero() || !status.Display.Enabled {
			t.Fatalf("missing observation: %+v", status)
		}
		for _, secret := range []string{operatorTestToken, cfg.DisplayTokenKey, "leaseId", "holderId", "memorySessionId", "photoCheckpoints"} {
			if strings.Contains(got.Body.String(), secret) {
				t.Fatalf("private data in status: %s", got.Body.String())
			}
		}
		return status
	}
	// An upgraded socket awaiting authentication is not an authorized renderer.
	pending, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/display/ws", http.Header{"Origin": {cfg.DisplayAllowedOrigins[0]}})
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Close()
	if read().Display.Connections != 0 {
		t.Fatal("unauthenticated connection counted")
	}
	primary, _, err := dialDisplay(t, server.URL, displayToken(t, cfg), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	nextDisplay(t, primary, "display.snapshot")
	standby, _, err := dialDisplay(t, server.URL, displayToken(t, cfg), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer standby.Close()
	nextDisplay(t, standby, "display.snapshot")
	displayInput(t, primary, cfg, "renderer.primary.acquire", "", "", "")
	nextDisplay(t, primary, "renderer.primary.granted")
	if status := read(); status.Display.Connections != 2 || !status.Display.PrimaryLeaseActive {
		t.Fatalf("active renderers: %+v", status)
	}
	primary.Close()
	standby.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		status := read()
		if status.Display.Connections == 0 && !status.Display.PrimaryLeaseActive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disconnected renderers: %+v", status)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
