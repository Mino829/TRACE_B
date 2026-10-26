package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func TestCombinedDeletionHonorsBothControlBudgets(t *testing.T) {
	for _, limited := range []string{"memory", "live"} {
		t.Run(limited, func(t *testing.T) {
			f := testdb.Open(t)
			cfg := config.Default()
			handler := api.New(cfg, f.Store, nil)
			created := seedLegacyExperience(t, f.Store, cfg)
			if _, err := f.SQL.ExecContext(t.Context(), "UPDATE live_presence_sessions SET presence_phase='arrived' WHERE id=$1", created.Live.SessionID); err != nil {
				t.Fatal(err)
			}
			statePath := "/v1/experiences/" + created.ExperienceID
			livePath := "/v1/live-presence-sessions/" + created.Live.SessionID
			onSite := fmt.Sprintf(`{"continue":true,"consentVersion":%q}`, cfg.OnSiteConsentVersion)
			spend := func(kind string) {
				t.Helper()
				method, path, body, token := http.MethodGet, statePath, "", created.Memory.ParticipantToken
				if kind == "live" {
					method, path, body, token = http.MethodPost, livePath+"/on-site", onSite, created.Live.ParticipantToken
				}
				for range 30 {
					r := request(t, handler, method, path, body, map[string]string{"Authorization": "Bearer " + token})
					want := http.StatusOK
					if kind == "live" {
						want = http.StatusGone
					}
					if r.Code != want {
						t.Fatalf("%s control request: %d %s", kind, r.Code, r.Body.String())
					}
				}
			}
			spend(limited)
			for range 3 {
				r := request(t, handler, http.MethodDelete, statePath, "", map[string]string{
					"Authorization":      "Bearer " + created.Memory.ParticipantToken,
					"X-Trace-Live-Token": created.Live.ParticipantToken,
					"Idempotency-Key":    "limited-combined-delete",
					"Origin":             cfg.AllowedOrigins[0],
				})
				var body struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				retry, err := strconv.Atoi(r.Header().Get("Retry-After"))
				if r.Code != http.StatusTooManyRequests || err != nil || retry <= 0 || json.Unmarshal(r.Body.Bytes(), &body) != nil || body.Error.Code != "RATE_LIMITED" {
					t.Fatalf("combined deletion bypassed %s budget: status=%d body=%s retry=%q", limited, r.Code, r.Body.String(), r.Header().Get("Retry-After"))
				}
				if r.Header().Get("Access-Control-Expose-Headers") != "Retry-After" {
					t.Fatal("browser cannot read retry delay")
				}
			}
			if err := f.Store.AuthenticateMemory(t.Context(), created.Memory.SessionID, tokenHash(created.Memory.ParticipantToken), time.Now()); err != nil {
				t.Fatal("limited deletion removed memory:", err)
			}
			if err := f.Store.AuthenticateLive(t.Context(), created.Live.SessionID, tokenHash(created.Live.ParticipantToken), time.Now()); err != nil {
				t.Fatal("limited deletion removed live:", err)
			}
			other := "memory"
			if limited == "memory" {
				other = "live"
			}
			// A rejection must not spend the other credential's remaining budget.
			spend(other)
		})
	}
}
