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

const registrationClosePath = "/v1/operator/registration/close"

func TestRegistrationClosePersistsAndPreservesExistingParticipant(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig(t, f)
	cfg.PhotoMetadataEnabled = true
	photoGraphFixture(t, f, &cfg)
	h := api.New(cfg, f.Store, nil)
	owner := createExperience(t, h, cfg)
	checkOpen := func(handler http.Handler, path string, want bool) {
		t.Helper()
		headers := map[string]string(nil)
		if path == operatorStatusPath {
			headers = operatorTestHeaders()
		}
		got := request(t, handler, "GET", path, "", headers)
		var status struct {
			RegistrationOpen bool `json:"registrationOpen"`
		}
		if err := json.Unmarshal(got.Body.Bytes(), &status); err != nil || got.Code != 200 || status.RegistrationOpen != want {
			t.Fatalf("registration status %s: %d %s (%v)", path, got.Code, got.Body.String(), err)
		}
	}
	checkOpen(h, "/v1/public/config", true)
	checkOpen(h, operatorStatusPath, true)
	for range 2 {
		got := request(t, h, "POST", registrationClosePath, "", operatorTestHeaders())
		if got.Code != 200 {
			t.Fatalf("close: %d %s", got.Code, got.Body.String())
		}
	}
	// A new Store and HTTP handler must not reopen registration from env config.
	other, err := store.Open(t.Context(), f.URL, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	restarted := api.New(cfg, other, nil)
	photoGraphFixture(t, &testdb.Fixture{Store: other}, &cfg)
	checkOpen(restarted, "/v1/public/config", false)
	checkOpen(restarted, operatorStatusPath, false)
	body := fmt.Sprintf(`{"memoryConsentVersion":%q,"consentLanguage":%q,"clientTime":%q}`, cfg.MemoryConsentVersion, cfg.ConsentLanguage, time.Now().UTC().Format(time.RFC3339Nano))
	got := request(t, restarted, "POST", "/v1/experiences", body, nil)
	if got.Code != 503 || !strings.Contains(got.Body.String(), `"REGISTRATION_CLOSED"`) {
		t.Fatalf("new enrollment after close: %d %s", got.Code, got.Body.String())
	}
	var enrollments int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM experiences`).Scan(&enrollments); err != nil || enrollments != 1 {
		t.Fatalf("closed registration created participant: %d %v", enrollments, err)
	}
	base := "/v2/memory-sessions/" + owner.Memory.SessionID
	headers := map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": "after-close"}
	got = request(t, restarted, "POST", base+"/photos", devicePhotoBody("after-close", "cp-a", time.Now().UTC()), headers)
	if got.Code != 201 {
		t.Fatalf("existing photo: %d %s", got.Code, got.Body.String())
	}
	proof := signProof(t, cfg, cfg.ArrivalVenue, time.Now().Add(-time.Second), time.Now().Add(20*time.Second))
	if arrived := arriveRequest(t, restarted, owner, proof, "after-close-arrival"); arrived.code != 200 {
		t.Fatalf("existing arrival: %d %s", arrived.code, arrived.body)
	}
	got = request(t, restarted, "DELETE", "/v1/memory-sessions/"+owner.Memory.SessionID, "", headers)
	if got.Code != 204 {
		t.Fatalf("existing deletion: %d %s", got.Code, got.Body.String())
	}
	var closed, repeated int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FILTER (WHERE result='closed'), COUNT(*) FILTER (WHERE result='already_closed') FROM arrival_operator_audit WHERE action='registration.close' AND operator_id=$1 AND target_id_short='registration'`, operatorTestActor).Scan(&closed, &repeated); err != nil || closed != 1 || repeated != 1 {
		t.Fatalf("closure audit: %d %d %v", closed, repeated, err)
	}
}

func TestRegistrationCloseRejectsUnauthorizedAndInvalidRequests(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig(t, f)
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
		{"browser", "", func(h map[string]string) { h["Origin"] = "https://operator.example" }, 403},
		{"legacy signature", "", func(h map[string]string) { h["X-Trace-Issuer-Signature"] = "obsolete" }, 401},
		{"legacy timestamp", "", func(h map[string]string) { h["X-Trace-Issuer-Timestamp"] = "1" }, 401},
		{"body", `{}`, func(map[string]string) {}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := operatorTestHeaders()
			headers["X-Trace-Edge-Secret"] = cfg.EdgeSharedSecret
			tc.change(headers)
			got := request(t, h, "POST", registrationClosePath, tc.body, headers)
			if got.Code != tc.want {
				t.Fatalf("rejected close: %d %s", got.Code, got.Body.String())
			}
		})
	}
	if closed, err := f.Store.RegistrationClosed(t.Context()); err != nil || closed {
		t.Fatalf("rejected requests closed registration: %t %v", closed, err)
	}
	var audits int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM arrival_operator_audit WHERE action='registration.close'`).Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("rejected close produced success audit: %d %v", audits, err)
	}
}

func TestRegistrationCloseAuditFailureRollsBackStop(t *testing.T) {
	f := testdb.Open(t)
	cfg := arrivalConfig(t, f)
	h := api.New(cfg, f.Store, nil)
	if _, err := f.SQL.ExecContext(t.Context(), `ALTER TABLE arrival_operator_audit ADD CONSTRAINT reject_close CHECK (action<>'registration.close')`); err != nil {
		t.Fatal(err)
	}
	got := request(t, h, "POST", registrationClosePath, "", operatorTestHeaders())
	if got.Code != 500 {
		t.Fatalf("audit failure: %d %s", got.Code, got.Body.String())
	}
	if closed, err := f.Store.RegistrationClosed(t.Context()); err != nil || closed {
		t.Fatalf("stop persisted without audit: %t %v", closed, err)
	}
	createExperience(t, h, cfg)
}
