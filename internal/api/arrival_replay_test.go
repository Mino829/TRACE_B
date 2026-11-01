package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func TestParticipantReplayHTTPIdentityProofAndReceiptBoundaries(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig(t, f)
	cfg.DisplayEnabled = true
	h := api.New(cfg, f.Store, nil)
	owner := seedLegacyExperience(t, f.Store, cfg)
	initialProof := projectedProof(t, h)
	initial := arriveRequest(t, h, owner, initialProof, "shared-key")
	if initial.code != http.StatusOK {
		t.Fatalf("initial: %+v", initial)
	}
	var first struct {
		ArrivalID string `json:"arrivalId"`
	}
	if err := json.Unmarshal([]byte(initial.body), &first); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if ok, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease", now); err != nil || !ok {
		t.Fatalf("lease: %t %v", ok, err)
	}
	finish := func(id, prefix string) {
		t.Helper()
		for _, action := range []string{"started", "completed"} {
			if _, err := f.Store.DisplayReplay(t.Context(), "holder", "lease", prefix+action, id, "arrival.replay."+action, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	finish(first.ArrivalID, "initial-")
	path := "/v1/memory-sessions/" + owner.Memory.SessionID + "/arrival/replay"
	headers := map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": "shared-key"}
	bodyFor := func(proof string) string {
		return fmt.Sprintf(`{"checkpointToken":%q,"clientTime":"2000-01-01T00:00:00Z"}`, proof)
	}
	for _, feature := range []string{"arrival", "display"} {
		disabled := cfg
		if feature == "arrival" {
			disabled.ArrivalEnabled = false
		} else {
			disabled.DisplayEnabled = false
		}
		if r := request(t, api.New(disabled, f.Store, nil), "POST", path, bodyFor(initialProof), headers); r.Code != http.StatusServiceUnavailable || !strings.Contains(r.Body.String(), "NOT_AVAILABLE") {
			t.Fatalf("%s disabled: %d %s", feature, r.Code, r.Body.String())
		}
	}
	if r := request(t, h, "POST", path, bodyFor(initialProof), nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous replay: %d", r.Code)
	}
	if r := request(t, h, "POST", path, bodyFor(initialProof), map[string]string{"Authorization": headers["Authorization"]}); r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "IDEMPOTENCY_KEY_REQUIRED") {
		t.Fatalf("missing key: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, h, "POST", path, bodyFor(initialProof), headers); r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "INVALID_ARRIVAL_PROOF") {
		t.Fatalf("initial proof reuse: %d %s", r.Code, r.Body.String())
	}
	proof := projectedProof(t, h)
	body := bodyFor(proof)
	accepted := request(t, h, "POST", path, body, headers)
	if accepted.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", accepted.Code, accepted.Body.String())
	}
	var replay struct {
		ArrivalID string `json:"arrivalId"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(accepted.Body.Bytes(), &replay); err != nil || replay.ArrivalID == first.ArrivalID || replay.Status != "queued" {
		t.Fatalf("new generation: %+v %v", replay, err)
	}
	headers["Idempotency-Key"] = "another-key"
	if r := request(t, h, "POST", path, body, headers); r.Code != http.StatusConflict || !strings.Contains(r.Body.String(), "STATE_CONFLICT") {
		t.Fatalf("queued conflict: %d %s", r.Code, r.Body.String())
	}
	finish(replay.ArrivalID, "replayed-")
	if r := request(t, h, "POST", path, body, headers); r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "INVALID_ARRIVAL_PROOF") {
		t.Fatalf("proof reuse after completed: %d %s", r.Code, r.Body.String())
	}
	// Invalidating the issuing key makes the original proof unverifiable; the
	// previously accepted request must still recover its exact saved response.
	rotated := cfg
	rotated.ArrivalProofKey = "rotated-proof-secret-1234567890123456789"
	restarted := api.New(rotated, f.Store, nil)
	headers["Idempotency-Key"] = "shared-key"
	if r := request(t, restarted, "POST", path, body, headers); r.Code != http.StatusOK || r.Body.String() != accepted.Body.String() {
		t.Fatalf("receipt after proof invalidation: %d %s", r.Code, r.Body.String())
	}
	if r := request(t, restarted, "POST", path, body+" ", headers); r.Code != http.StatusConflict || !strings.Contains(r.Body.String(), "IDEMPOTENCY_CONFLICT") {
		t.Fatalf("changed request: %d %s", r.Code, r.Body.String())
	}
	if r := arriveRequest(t, restarted, owner, initialProof, "shared-key"); r.code != http.StatusOK || !strings.Contains(r.body, first.ArrivalID) || !strings.Contains(r.body, `"status":"completed"`) {
		t.Fatalf("initial receipt no reenqueuing: %+v", r)
	}
}
