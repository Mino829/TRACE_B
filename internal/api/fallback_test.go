package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

const fallbackPath = "/v1/operator/fallback"

func fallbackRequest(t *testing.T, h http.Handler, cfg config.Config, method string) store.FallbackState {
	t.Helper()
	headers := issuerHeaders(cfg, method, fallbackPath)
	if cfg.EdgeSharedSecret != "" {
		headers["X-Trace-Edge-Secret"] = cfg.EdgeSharedSecret
	}
	got := request(t, h, method, fallbackPath, "", headers)
	return fallbackResponse(t, got)
}

func fallbackResponse(t *testing.T, got *httptest.ResponseRecorder) store.FallbackState {
	t.Helper()
	var response struct {
		Fallback store.FallbackState `json:"fallback"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &response); err != nil || got.Code != http.StatusOK {
		t.Fatalf("fallback response: %d %s (%v)", got.Code, got.Body.String(), err)
	}
	if response.Fallback.ChangedAt.IsZero() || (response.Fallback.Mode == "pending") != (response.Fallback.SwitchAt != nil) {
		t.Fatalf("invalid fallback timestamps: %+v", response.Fallback)
	}
	return response.Fallback
}

func seedFallbackPlaying(t *testing.T, f *testdb.Fixture, h http.Handler, cfg config.Config) {
	t.Helper()
	owner := seedLegacyExperience(t, f.Store, cfg)
	now := time.Now().UTC()
	proof := signProof(t, cfg, cfg.ArrivalVenue, now.Add(-time.Second), now.Add(20*time.Second))
	if got := arriveRequest(t, h, owner, proof, "fallback-playing"); got.code != 200 {
		t.Fatalf("arrival: %d %s", got.code, got.body)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE display_primary_lease SET holder_id='fallback-renderer',lease_id='fallback-lease',expires_at=$1`, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE arrival_events SET replay_status='playing',renderer_lease_id='fallback-lease',replay_started_at=$2 WHERE memory_session_id=$1`, owner.Memory.SessionID, now); err != nil {
		t.Fatal(err)
	}
}

func TestFallbackControlAndStatus(t *testing.T) {
	f := testdb.Open(t)
	cfg := displayConfig()
	cfg.EdgeSharedSecret = "independent-edge-secret-1234567890"
	h := api.New(cfg, f.Store, nil)
	state := fallbackRequest(t, h, cfg, http.MethodPost)
	if state.Mode != "fallback" {
		t.Fatalf("idle enable: %+v", state)
	}
	repeated := fallbackRequest(t, h, cfg, http.MethodPost)
	if repeated.Mode != state.Mode || !repeated.ChangedAt.Equal(state.ChangedAt) {
		t.Fatalf("repeated enable changed state: %+v -> %+v", state, repeated)
	}
	statusHeaders := issuerHeaders(cfg, http.MethodGet, operatorStatusPath)
	statusHeaders["X-Trace-Edge-Secret"] = cfg.EdgeSharedSecret
	status := fallbackResponse(t, request(t, h, http.MethodGet, operatorStatusPath, "", statusHeaders))
	if status.Mode != state.Mode || !status.ChangedAt.Equal(state.ChangedAt) {
		t.Fatalf("status fallback: %+v, want %+v", status, state)
	}
	state = fallbackRequest(t, h, cfg, http.MethodDelete)
	if state.Mode != "normal" {
		t.Fatalf("manual recovery: %+v", state)
	}
	var enabled, disabled int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FILTER (WHERE action='fallback.enable'), COUNT(*) FILTER (WHERE action='fallback.disable') FROM arrival_operator_audit WHERE operator_id='arrival_issuer' AND target_id_short='fallback'`).Scan(&enabled, &disabled); err != nil || enabled < 1 || disabled != 1 {
		t.Fatalf("fallback audits: enabled=%d disabled=%d err=%v", enabled, disabled, err)
	}
}

func TestFallbackPendingDeadlineAndManualRecovery(t *testing.T) {
	f := testdb.Open(t)
	cfg := displayConfig()
	h := api.New(cfg, f.Store, nil)
	seedFallbackPlaying(t, f, h, cfg)
	before := time.Now().UTC()
	pending := fallbackRequest(t, h, cfg, http.MethodPost)
	if pending.Mode != "pending" || pending.SwitchAt == nil {
		t.Fatalf("playing enable: %+v", pending)
	}
	if pending.SwitchAt.Before(before.Add(29*time.Second)) || pending.SwitchAt.After(time.Now().UTC().Add(30*time.Second)) {
		t.Fatalf("pending deadline not bounded by 30s: %+v", pending)
	}
	repeated := fallbackRequest(t, h, cfg, http.MethodPost)
	if repeated.Mode != "pending" || repeated.SwitchAt == nil || !repeated.SwitchAt.Equal(*pending.SwitchAt) || !repeated.ChangedAt.Equal(pending.ChangedAt) {
		t.Fatalf("enable extended deadline: %+v -> %+v", pending, repeated)
	}
	if normal := fallbackRequest(t, h, cfg, http.MethodDelete); normal.Mode != "normal" {
		t.Fatalf("cancel pending: %+v", normal)
	}
	var playing int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM arrival_events WHERE replay_status='playing'`).Scan(&playing); err != nil || playing != 1 {
		t.Fatalf("manual recovery skipped playing replay: %d %v", playing, err)
	}
	pending = fallbackRequest(t, h, cfg, http.MethodPost)
	if err := f.Store.RefreshDisplay(t.Context(), pending.SwitchAt.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	state := fallbackResponse(t, request(t, h, http.MethodGet, operatorStatusPath, "", issuerHeaders(cfg, http.MethodGet, operatorStatusPath)))
	if state.Mode != "fallback" {
		t.Fatalf("elapsed pending: %+v", state)
	}
	if normal := fallbackRequest(t, h, cfg, http.MethodDelete); normal.Mode != "normal" {
		t.Fatalf("recover after timeout: %+v", normal)
	}
}

func TestFallbackRejectsUnauthorizedAndNonemptyRequests(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		for _, name := range []string{"missing edge", "wrong edge", "edge only", "participant", "display", "browser", "expired", "cross path", "cross method", "body", "oversized body"} {
			t.Run(method+"/"+name, func(t *testing.T) {
				f := testdb.Open(t)
				cfg := displayConfig()
				cfg.EdgeSharedSecret = "independent-edge-secret-1234567890"
				h := api.New(cfg, f.Store, nil)
				initial, err := f.Store.SetFallback(t.Context(), method == http.MethodDelete, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				var auditsBefore int
				if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM arrival_operator_audit`).Scan(&auditsBefore); err != nil {
					t.Fatal(err)
				}
				headers := issuerHeaders(cfg, method, fallbackPath)
				headers["X-Trace-Edge-Secret"] = cfg.EdgeSharedSecret
				body, want := "", http.StatusForbidden
				switch name {
				case "missing edge":
					delete(headers, "X-Trace-Edge-Secret")
				case "wrong edge":
					headers["X-Trace-Edge-Secret"] = "wrong"
				case "edge only", "participant", "display":
					delete(headers, "X-Trace-Issuer-Signature")
					if name == "participant" {
						owner := seedLegacyExperience(t, f.Store, cfg)
						headers["Authorization"] = "Bearer " + owner.Memory.ParticipantToken
					} else if name == "display" {
						headers["Authorization"] = "Bearer " + displayToken(t, cfg)
					}
				case "browser":
					headers["Origin"] = cfg.DisplayAllowedOrigins[0]
				case "expired":
					headers["X-Trace-Issuer-Timestamp"] = "1"
					headers["X-Trace-Issuer-Signature"] = signMessage(cfg.ArrivalIssuerKey, method+"\n"+fallbackPath+"\n1")
				case "cross path":
					headers["X-Trace-Issuer-Signature"] = issuerHeaders(cfg, method, operatorStatusPath)["X-Trace-Issuer-Signature"]
				case "cross method":
					other := http.MethodPost
					if method == other {
						other = http.MethodDelete
					}
					headers["X-Trace-Issuer-Signature"] = issuerHeaders(cfg, other, fallbackPath)["X-Trace-Issuer-Signature"]
				case "body":
					body, want = `{}`, http.StatusBadRequest
				case "oversized body":
					body, want = strings.Repeat("x", 2<<20), http.StatusRequestEntityTooLarge
				}
				got := request(t, h, method, fallbackPath, body, headers)
				if got.Code != want {
					t.Fatalf("rejected control: %d %s, want %d", got.Code, got.Body.String(), want)
				}
				state, err := f.Store.ReadFallback(t.Context())
				if err != nil || state.Mode != initial.Mode || !state.ChangedAt.Equal(initial.ChangedAt) {
					t.Fatalf("rejected request mutated fallback: %+v -> %+v (%v)", initial, state, err)
				}
				var audits int
				if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM arrival_operator_audit`).Scan(&audits); err != nil || audits != auditsBefore {
					t.Fatalf("rejected request wrote audit: %d -> %d (%v)", auditsBefore, audits, err)
				}
			})
		}
	}
}

func TestFallbackUnavailable(t *testing.T) {
	for _, name := range []string{"display disabled", "display incomplete", "operator disabled", "operator key missing"} {
		t.Run(name, func(t *testing.T) {
			f := testdb.Open(t)
			cfg := displayConfig()
			switch name {
			case "display disabled":
				cfg.DisplayEnabled = false
			case "display incomplete":
				cfg.DisplayGoogleClientID = ""
			case "operator disabled":
				cfg.ArrivalEnabled = false
			case "operator key missing":
				cfg.ArrivalIssuerKey = ""
			}
			h := api.New(cfg, f.Store, nil)
			for _, method := range []string{http.MethodPost, http.MethodDelete} {
				got := request(t, h, method, fallbackPath, "", issuerHeaders(cfg, method, fallbackPath))
				if got.Code != http.StatusServiceUnavailable {
					t.Fatalf("disabled %s: %d %s", method, got.Code, got.Body.String())
				}
			}
			state, err := f.Store.ReadFallback(t.Context())
			if err != nil || state.Mode != "normal" {
				t.Fatalf("disabled control mutated state: %+v (%v)", state, err)
			}
		})
	}
}

func TestFallbackAuditFailureRollsBack(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			f := testdb.Open(t)
			cfg := displayConfig()
			h := api.New(cfg, f.Store, nil)
			initial, err := f.Store.SetFallback(t.Context(), method == http.MethodDelete, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			var cursor int64
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT last_id FROM display_cursor WHERE singleton=TRUE`).Scan(&cursor); err != nil {
				t.Fatal(err)
			}
			if _, err := f.SQL.ExecContext(t.Context(), `ALTER TABLE arrival_operator_audit ADD CONSTRAINT reject_fallback CHECK (action NOT IN ('fallback.enable','fallback.disable')) NOT VALID`); err != nil {
				t.Fatal(err)
			}
			got := request(t, h, method, fallbackPath, "", issuerHeaders(cfg, method, fallbackPath))
			if got.Code != http.StatusInternalServerError {
				t.Fatalf("audit failure: %d %s", got.Code, got.Body.String())
			}
			state, err := f.Store.ReadFallback(t.Context())
			if err != nil || state.Mode != initial.Mode || !state.ChangedAt.Equal(initial.ChangedAt) {
				t.Fatalf("audit failure persisted state: %+v -> %+v (%v)", initial, state, err)
			}
			var after int64
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT last_id FROM display_cursor WHERE singleton=TRUE`).Scan(&after); err != nil || after != cursor {
				t.Fatalf("audit failure persisted event: %d -> %d (%v)", cursor, after, err)
			}
		})
	}
}
