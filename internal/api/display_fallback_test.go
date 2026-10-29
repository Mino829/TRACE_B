package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func TestDisplayFallbackSnapshotsPreserveQueueAcrossReconnect(t *testing.T) {
	f := testdb.Open(t)
	cfg := displayConfig(t, f)
	h := api.New(cfg, f.Store, nil)
	server := displayServer(t, h)
	conn, _, err := dialDisplay(t, server.URL, displayToken(t, cfg), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	nextDisplay(t, conn, "display.snapshot")
	displayInput(t, conn, cfg, "renderer.primary.acquire", "", "", "")
	var lease struct {
		ID string `json:"leaseId"`
	}
	if frame := nextDisplay(t, conn, "renderer.primary.granted"); json.Unmarshal(frame.Data, &lease) != nil || lease.ID == "" {
		t.Fatalf("missing primary lease: %s", frame.Data)
	}
	proof := projectedProof(t, h)
	arrive := func(index int) string {
		t.Helper()
		result := arriveRequest(t, h, createExperience(t, h, cfg), proof, fmt.Sprintf("fallback-ws-arrival-%d", index))
		var value struct {
			ID string `json:"arrivalId"`
		}
		if result.code != http.StatusOK || json.Unmarshal([]byte(result.body), &value) != nil || value.ID == "" {
			t.Fatalf("arrival %d: %d %s", index, result.code, result.body)
		}
		return value.ID
	}
	first, second := arrive(1), arrive(2)
	displayInput(t, conn, cfg, "arrival.replay.started", "fallback-ws-start-first", first, lease.ID)
	if frame := nextDisplay(t, conn, "renderer.ack"); !jsonHasReplayStatus(frame.Data, "playing") {
		t.Fatalf("first replay not playing: %s", frame.Data)
	}
	path := "/v1/operator/fallback"
	control := func(method string) {
		t.Helper()
		if response := request(t, h, method, path, "", operatorTestHeaders()); response.Code != http.StatusOK {
			t.Fatalf("%s fallback: %d %s", method, response.Code, response.Body.String())
		}
	}
	verifySnapshot := func(frame displayFrameTest, mode string, want map[string]string) {
		t.Helper()
		var state store.DisplayState
		if err := json.Unmarshal(frame.Data, &state); err != nil {
			t.Fatal(err)
		}
		if state.Fallback.Mode != mode || (state.Fallback.SwitchAt != nil) != (mode == "pending") {
			t.Fatalf("fallback state: %+v, want %s", state.Fallback, mode)
		}
		if len(state.Arrivals) != len(want) {
			t.Fatalf("queue snapshot: %s", frame.Data)
		}
		for _, arrival := range state.Arrivals {
			if status, ok := want[arrival.ArrivalID]; !ok || arrival.ReplayStatus != status {
				t.Fatalf("unexpected arrival %+v in %s", arrival, frame.Data)
			}
		}
	}
	verifyTransition := func(mode string, want map[string]string) {
		t.Helper()
		frame := nextDisplay(t, conn, "system.fallback.changed")
		var state store.FallbackState
		if err := json.Unmarshal(frame.Data, &state); err != nil || state.Mode != mode {
			t.Fatalf("fallback transition: %s %v", frame.Data, err)
		}
		verifySnapshot(nextDisplay(t, conn, "display.snapshot"), mode, want)
	}
	control(http.MethodPost)
	verifyTransition("pending", map[string]string{first: "playing", second: "queued"})
	displayInput(t, conn, cfg, "arrival.replay.completed", "fallback-ws-complete-first", first, lease.ID)
	if frame := nextDisplay(t, conn, "renderer.ack"); !jsonHasReplayStatus(frame.Data, "completed") {
		t.Fatalf("current replay did not finish: %s", frame.Data)
	}
	verifyTransition("fallback", map[string]string{second: "queued"})
	third := arrive(3)
	displayInput(t, conn, cfg, "arrival.replay.started", "fallback-ws-blocked-start", second, lease.ID)
	nextDisplay(t, conn, "renderer.rejected")
	reconnected, _, err := dialDisplay(t, server.URL, displayToken(t, cfg), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	want := map[string]string{second: "queued", third: "queued"}
	verifySnapshot(nextDisplay(t, reconnected, "display.snapshot"), "fallback", want)
	control(http.MethodDelete)
	verifyTransition("normal", want)
	nextDisplay(t, reconnected, "system.fallback.changed")
	verifySnapshot(nextDisplay(t, reconnected, "display.snapshot"), "normal", want)
	displayInput(t, conn, cfg, "arrival.replay.started", "fallback-ws-resume-fifo", second, lease.ID)
	if frame := nextDisplay(t, conn, "renderer.ack"); !jsonHasReplayStatus(frame.Data, "playing") {
		t.Fatalf("FIFO did not resume at queued head: %s", frame.Data)
	}
	displayInput(t, conn, cfg, "arrival.replay.completed", "fallback-ws-complete-second", second, lease.ID)
	nextDisplay(t, conn, "renderer.ack")
}

func jsonHasReplayStatus(data json.RawMessage, expected string) bool {
	var value struct {
		Status string `json:"replayStatus"`
	}
	return json.Unmarshal(data, &value) == nil && value.Status == expected
}
