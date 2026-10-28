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

func TestArrivalQueueFullHTTPIsRetryableWithoutPartialArrival(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig()
	h := api.New(cfg, f.Store, nil)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := range 50 {
		id := fmt.Sprintf("full-memory-%02d", i)
		if err := f.Store.CreateExperience(t.Context(), store.NewExperience{
			ExperienceID: fmt.Sprintf("full-exp-%02d", i), MemorySessionID: id, MemoryTokenHash: id,
			MemoryConsent: "memory", ConsentLanguage: "ja", CreatedAt: now,
			MemoryExpiresAt: now.Add(time.Hour), MemoryDeleteAfter: now.Add(2 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Store.AcceptArrival(t.Context(), id, "", fmt.Sprintf("full-arrival-%02d", i), "booth", "full-receipt", "request", now); err != nil {
			t.Fatal(err)
		}
	}
	owner := createExperience(t, h, cfg)
	proof := projectedProof(t, h, cfg)
	path := "/v1/memory-sessions/" + owner.Memory.SessionID + "/arrival"
	body := fmt.Sprintf(`{"checkpointToken":%q,"experienceLinkToken":%q,"clientTime":"2000-01-01T00:00:00Z"}`, proof, owner.LinkToken)
	headers := map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": "full-retry"}
	full := request(t, h, "POST", path, body, headers)
	if full.Code != http.StatusServiceUnavailable || !strings.Contains(full.Body.String(), `"code":"ARRIVAL_QUEUE_FULL"`) || full.Header().Get("Retry-After") != "5" {
		t.Fatalf("full response: %d %s %v", full.Code, full.Body.String(), full.Header())
	}
	var stopped bool
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT push_stopped FROM memory_sessions WHERE id=$1`, owner.Memory.SessionID).Scan(&stopped); err != nil || stopped {
		t.Fatalf("queue full stopped push: %t %v", stopped, err)
	}
	for _, table := range []string{"arrival_events", "arrival_receipts"} {
		var count int
		if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table+" WHERE memory_session_id=$1", owner.Memory.SessionID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s: %d %v", table, count, err)
		}
	}
	if err := f.Store.SkipArrival(t.Context(), "full-arrival-00", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	retry := request(t, h, "POST", path, body, headers)
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `"status":"queued"`) {
		t.Fatalf("recovered same-key arrival: %d %s", retry.Code, retry.Body.String())
	}
	replay := request(t, h, "POST", path, body, headers)
	if replay.Code != http.StatusOK || replay.Body.String() != retry.Body.String() {
		t.Fatalf("accepted receipt replay: %d %s", replay.Code, replay.Body.String())
	}
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT push_stopped FROM memory_sessions WHERE id=$1`, owner.Memory.SessionID).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("accepted arrival failed to stop push: %t %v", stopped, err)
	}
}

func TestDisplayWebSocketWaitsAndReplaysInOrder(t *testing.T) {
	f := testdb.Open(t)
	cfg := displayConfig()
	h := api.New(cfg, f.Store, nil)
	server := displayServer(t, h)
	token := displayToken(t, cfg)
	conn, response, err := dialDisplay(t, server.URL, token, cfg)
	if err != nil {
		t.Fatalf("dial: %v %v", err, response)
	}
	defer conn.Close()
	nextDisplay(t, conn, "display.snapshot")
	displayInput(t, conn, cfg, "renderer.primary.acquire", "", "", "")
	frame := nextDisplay(t, conn, "renderer.primary.granted")
	var lease struct {
		ID string `json:"leaseId"`
	}
	if err := json.Unmarshal(frame.Data, &lease); err != nil || lease.ID == "" {
		t.Fatalf("lease: %s %v", frame.Data, err)
	}
	proof := projectedProof(t, h, cfg)
	owners := []createdExperience{createExperience(t, h, cfg), createExperience(t, h, cfg)}
	ids := make([]string, 2)
	for i, owner := range owners {
		result := arriveRequest(t, h, owner, proof, fmt.Sprintf("fifo-websocket-%d", i))
		if result.code != http.StatusOK {
			t.Fatalf("arrival %d: %d %s", i, result.code, result.body)
		}
		var arrival struct {
			ID string `json:"arrivalId"`
		}
		if err := json.Unmarshal([]byte(result.body), &arrival); err != nil || arrival.ID == "" {
			t.Fatalf("arrival ID: %s %v", result.body, err)
		}
		ids[i] = arrival.ID
	}
	// Age queued requests without sleeping or extending their session lifetime.
	now := time.Now().UTC()
	for i, id := range ids {
		if _, err := f.SQL.ExecContext(t.Context(), `UPDATE arrival_events SET arrived_at=$2,photo_wait_until=$2 WHERE id=$1`, id, now.Add(-2*time.Minute).Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	restored, response, err := dialDisplay(t, server.URL, token, cfg)
	if err != nil {
		t.Fatalf("snapshot reconnect: %v %v", err, response)
	}
	defer restored.Close()
	snapshot := nextDisplay(t, restored, "display.snapshot")
	for _, id := range ids {
		if !strings.Contains(string(snapshot.Data), id) {
			t.Fatalf("long-wait arrival missing from snapshot: %s", snapshot.Data)
		}
	}
	for _, owner := range owners {
		state := request(t, h, "GET", "/v1/experiences/"+owner.ExperienceID, "", map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken})
		if state.Code != http.StatusOK || !strings.Contains(state.Body.String(), `"replayStatus":"queued"`) {
			t.Fatalf("owner state: %d %s", state.Code, state.Body.String())
		}
	}
	displayInput(t, conn, cfg, "arrival.replay.started", "fifo-second-first", ids[1], lease.ID)
	nextDisplay(t, conn, "renderer.rejected")
	displayInput(t, conn, cfg, "arrival.replay.started", "fifo-first-start", ids[0], lease.ID)
	if ack := nextDisplay(t, conn, "renderer.ack"); !strings.Contains(string(ack.Data), `"replayStatus":"playing"`) {
		t.Fatalf("first start: %s", ack.Data)
	}
	displayInput(t, conn, cfg, "arrival.replay.started", "fifo-second-overlap", ids[1], lease.ID)
	nextDisplay(t, conn, "renderer.rejected")
	displayInput(t, conn, cfg, "arrival.replay.completed", "fifo-first-complete", ids[0], lease.ID)
	nextDisplay(t, conn, "renderer.ack")
	displayInput(t, conn, cfg, "arrival.replay.started", "fifo-second-start", ids[1], lease.ID)
	if ack := nextDisplay(t, conn, "renderer.ack"); !strings.Contains(string(ack.Data), `"replayStatus":"playing"`) {
		t.Fatalf("second start: %s", ack.Data)
	}
	displayInput(t, conn, cfg, "arrival.replay.completed", "fifo-second-complete", ids[1], lease.ID)
	nextDisplay(t, conn, "renderer.ack")
	var completed int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM arrival_events WHERE replay_status='completed'`).Scan(&completed); err != nil || completed != 2 {
		t.Fatalf("completion: %d %v", completed, err)
	}
}
