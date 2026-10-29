package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

const operatorTestToken = "0123456789012345678901234567890123456789012"
const operatorTestActor = "7a4341b20fd3be82d5291bb299eaecfb92155fcd60d250374eef110d692ca108"
const operatorTestOrigin = "https://operator.example.com"

func operatorTestHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func enableOperatorTest(t *testing.T, f *testdb.Fixture, cfg *config.Config) {
	t.Helper()
	cfg.OperatorEnabled = true
	cfg.OperatorGoogleClientID = "123456789-operatorweb.apps.googleusercontent.com"
	cfg.OperatorEmails = []string{"operator@gmail.com"}
	cfg.OperatorAllowedOrigins = []string{operatorTestOrigin}
	var exists bool
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM operator_credentials WHERE id='opc_api_test')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		return
	}
	now := time.Now().UTC()
	err := f.Store.CreateOperatorCredential(store.WithOperatorActor(t.Context(), operatorTestActor), store.OperatorCredential{
		ID: "opc_api_test", TokenHash: operatorTestHash(operatorTestToken), ActorID: operatorTestActor,
		EmailHash: operatorTestHash("operator.email\x00operator@gmail.com"), Origin: operatorTestOrigin,
		Role: "operator", CreatedAt: now, ExpiresAt: now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func operatorTestHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + operatorTestToken, "Origin": operatorTestOrigin}
}

func TestOperatorRoutesRevokeCurrentAndAllWithoutArrival(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	enableOperatorTest(t, f, &cfg)
	h := api.New(cfg, f.Store, nil)
	if got := request(t, h, "GET", "/v1/operator/status", "", operatorTestHeaders()); got.Code != 200 {
		t.Fatalf("operator status must not require arrival: %d %s", got.Code, got.Body.String())
	}
	if got := request(t, h, "POST", "/v1/operator/registration/close", "", operatorTestHeaders()); got.Code != 200 {
		t.Fatalf("operator control must not require arrival: %d %s", got.Code, got.Body.String())
	}
	if got := request(t, h, "DELETE", "/v1/operator/credentials/current", "{}", operatorTestHeaders()); got.Code != 400 {
		t.Fatalf("revocation accepted nonempty body: %d %s", got.Code, got.Body.String())
	}
	if got := request(t, h, "DELETE", "/v1/operator/credentials/current", "", operatorTestHeaders()); got.Code != 204 {
		t.Fatalf("revoke current route: %d %s", got.Code, got.Body.String())
	}
	if got := request(t, h, "GET", "/v1/operator/status", "", operatorTestHeaders()); got.Code != 401 {
		t.Fatalf("revoked route accepted: %d %s", got.Code, got.Body.String())
	}
	enableOperatorTest(t, f, &cfg)
	if got := request(t, h, "DELETE", "/v1/operator/credentials", "", operatorTestHeaders()); got.Code != 204 {
		t.Fatalf("revoke all route: %d %s", got.Code, got.Body.String())
	}
	if got := request(t, h, "DELETE", "/v1/operator/credentials", "", operatorTestHeaders()); got.Code != 401 {
		t.Fatalf("repeat revocation returned fake success: %d %s", got.Code, got.Body.String())
	}
}

func TestOperatorRoutesKeepEdgeAndOriginSeparateFromRenderer(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	enableOperatorTest(t, f, &cfg)
	cfg.EdgeSharedSecret = "server-side-edge-secret-independent-12345"
	cfg.DisplayAllowedOrigins = []string{"https://renderer.example.com"}
	h := api.New(cfg, f.Store, nil)
	for _, path := range []string{"/v1/operator/credentials", "/v1/operator/status", "/v1/operator/credentials/current"} {
		if got := request(t, h, "POST", path, "", operatorTestHeaders()); got.Code != http.StatusForbidden {
			t.Fatalf("operator bypassed Edge: %s %d %s", path, got.Code, got.Body.String())
		}
	}
	headers := operatorTestHeaders()
	headers["X-Trace-Edge-Secret"] = cfg.EdgeSharedSecret
	if got := request(t, h, "OPTIONS", "/v1/operator/status", "", headers); got.Code != 204 || got.Header().Get("Access-Control-Allow-Origin") != operatorTestOrigin {
		t.Fatalf("operator origin preflight: %d %s", got.Code, got.Body.String())
	}
	headers["Origin"] = cfg.DisplayAllowedOrigins[0]
	if got := request(t, h, "OPTIONS", "/v1/operator/status", "", headers); got.Code != 403 || got.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("renderer origin authorized operator preflight: %d %s", got.Code, got.Body.String())
	}
	headers["Origin"] = operatorTestOrigin
	if got := request(t, h, "POST", "/v1/operator/credentials", "", headers); got.Code != 401 {
		t.Fatalf("opaque credential reused as Google login: %d %s", got.Code, got.Body.String())
	}
}
