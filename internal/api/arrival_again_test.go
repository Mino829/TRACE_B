package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func TestArrivalAgainAfterReplayWithFreshProof(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig(t, f)
	cfg.DisplayEnabled = true
	h := api.New(cfg, f.Store, nil)
	owner := createExperience(t, h, cfg)
	reply := func(result *httpResult) (string, string) {
		t.Helper()
		var value struct {
			ID     string `json:"arrivalId"`
			Status string `json:"status"`
		}
		if result.code != http.StatusOK || json.Unmarshal([]byte(result.body), &value) != nil || value.ID == "" {
			t.Fatalf("arrival: %d %s", result.code, result.body)
		}
		return value.ID, value.Status
	}
	firstID, _ := reply(arriveRequest(t, h, owner, projectedProof(t, h), "first-visit"))
	skip := request(t, h, "POST", "/v1/memory-sessions/"+owner.Memory.SessionID+"/arrival/skip", "",
		map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken})
	if skip.Code != http.StatusOK {
		t.Fatalf("participant skip: %d %s", skip.Code, skip.Body.String())
	}
	proof := projectedProof(t, h)
	againID, status := reply(arriveRequest(t, h, owner, proof, "second-visit"))
	if againID == firstID || status != "queued" {
		t.Fatalf("fresh proof after the replay did not queue again: %s/%s (first %s)", againID, status, firstID)
	}
	if retryID, retryStatus := reply(arriveRequest(t, h, owner, proof, "second-visit")); retryID != againID || retryStatus != "queued" {
		t.Fatalf("retry of the second visit changed it: %s/%s", retryID, retryStatus)
	}
	var rows int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM arrival_events WHERE memory_session_id=$1`,
		owner.Memory.SessionID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("arrival rows: %d %v", rows, err)
	}
}
