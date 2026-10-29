package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

type issuedOperator struct {
	Token     string    `json:"operatorToken"`
	ID        string    `json:"credentialId"`
	ExpiresAt time.Time `json:"expiresAt"`
	Role      string    `json:"role"`
}

func newOperatorGoogleTestIssuer(t *testing.T) (*displayGoogleTestIssuer, *testdb.Fixture) {
	t.Helper()
	f := newDisplayGoogleTestIssuer(t)
	db := testdb.Open(t)
	f.server.store = db.Store
	f.server.cfg.OperatorEnabled = true
	f.server.cfg.OperatorGoogleClientID = "123456789-operatorweb.apps.googleusercontent.com"
	f.server.cfg.OperatorEmails = []string{"operator@example.com"}
	f.server.cfg.OperatorAllowedOrigins = []string{"https://operator.example.com", "http://localhost:3000"}
	return f, db
}

func operatorGoogleRequest(f *displayGoogleTestIssuer, token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/operator/credentials", nil)
	r.Header.Set("Origin", f.server.cfg.OperatorAllowedOrigins[0])
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func operatorGoogleToken(t *testing.T, f *displayGoogleTestIssuer, change func(map[string]any)) string {
	t.Helper()
	return f.token(t, func(c map[string]any) {
		c["aud"] = f.server.cfg.OperatorGoogleClientID
		if change != nil {
			change(c)
		}
	})
}

func mintOperator(t *testing.T, f *displayGoogleTestIssuer, subject string) issuedOperator {
	t.Helper()
	token := operatorGoogleToken(t, f, func(c map[string]any) { c["sub"] = subject })
	w := httptest.NewRecorder()
	f.server.issueOperatorCredential(w, operatorGoogleRequest(f, token))
	if w.Code != http.StatusOK {
		t.Fatalf("issue operator: %d %s", w.Code, w.Body.String())
	}
	var issued issuedOperator
	if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.Role != "operator" || len(issued.Token) != 43 || issued.ID == "" || !issued.ExpiresAt.Equal(f.now.Add(2*time.Minute)) {
		t.Fatalf("invalid credential response: %+v", issued)
	}
	return issued
}

func useOperator(f *displayGoogleTestIssuer, token, origin string, next http.HandlerFunc) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/v1/operator/status", nil)
	r.Header.Set("Origin", origin)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.server.operatorOnly(next)(w, r)
	return w
}

func TestOperatorGoogleClaimsAndRendererEntitlementAreIndependent(t *testing.T) {
	f, _ := newOperatorGoogleTestIssuer(t)
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"renderer audience", func(c map[string]any) { c["aud"] = f.server.cfg.DisplayGoogleClientID }},
		{"multi audience", func(c map[string]any) { c["aud"] = []string{f.server.cfg.OperatorGoogleClientID, "other"} }},
		{"issuer", func(c map[string]any) { c["iss"] = "https://attacker.example.com" }},
		{"unlisted", func(c map[string]any) { c["email"] = "participant@example.com" }},
		{"unverified", func(c map[string]any) { c["email_verified"] = false }},
		{"subject", func(c map[string]any) { c["sub"] = "" }},
		{"expired", func(c map[string]any) { c["exp"] = f.now.Unix() }},
		{"future issued", func(c map[string]any) { c["iat"] = f.now.Add(time.Minute).Unix() }},
		{"missing issued", func(c map[string]any) { delete(c, "iat") }},
		{"expiry before issued", func(c map[string]any) {
			c["iat"] = f.now.Add(-time.Minute).Unix()
			c["exp"] = f.now.Add(-2 * time.Minute).Unix()
		}},
		{"authorized party", func(c map[string]any) { c["azp"] = f.server.cfg.DisplayGoogleClientID }},
		{"workspace authority", func(c map[string]any) { delete(c, "hd") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.server.limiter = newRateLimiter()
			token := operatorGoogleToken(t, f, tc.change)
			w := httptest.NewRecorder()
			f.server.issueOperatorCredential(w, operatorGoogleRequest(f, token))
			if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), "idtoken:") {
				t.Fatalf("identity rejection: %d %s", w.Code, w.Body.String())
			}
		})
	}
	f.server.limiter = newRateLimiter()
	f.server.cfg.OperatorEmails = []string{"different@gmail.com"}
	if w := f.send(f.request(f.token(t, nil))); w.Code != http.StatusOK {
		t.Fatal("operator allowlist affected renderer access")
	}
	w := httptest.NewRecorder()
	f.server.issueOperatorCredential(w, operatorGoogleRequest(f, operatorGoogleToken(t, f, nil)))
	if w.Code != http.StatusUnauthorized {
		t.Fatal("renderer entitlement authorized operator")
	}
}

func TestOperatorCredentialLifecycleOriginExpiryAndPrivateAudit(t *testing.T) {
	f, db := newOperatorGoogleTestIssuer(t)
	first := mintOperator(t, f, "google-operator-subject")
	second := mintOperator(t, f, "google-operator-subject")
	other := mintOperator(t, f, "another-operator-subject")
	origin := f.server.cfg.OperatorAllowedOrigins[0]
	var actor string
	next := func(w http.ResponseWriter, r *http.Request) {
		actor = store.OperatorActor(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}
	if w := useOperator(f, first.Token, origin, next); w.Code != http.StatusNoContent || actor == "" || actor == "google-operator-subject" {
		t.Fatal("verified actor missing")
	}
	for _, token := range []string{"participant-token", f.token(t, nil), operatorGoogleToken(t, f, nil)} {
		if w := useOperator(f, token, origin, next); w.Code != http.StatusUnauthorized {
			t.Fatalf("foreign credential accepted: %d", w.Code)
		}
	}
	display := f.send(f.request(f.token(t, nil)))
	var renderer struct {
		Token string `json:"displayToken"`
	}
	if err := json.Unmarshal(display.Body.Bytes(), &renderer); err != nil {
		t.Fatal(err)
	}
	if w := useOperator(f, renderer.Token, origin, next); w.Code != http.StatusUnauthorized {
		t.Fatal("renderer token accepted")
	}
	if w := useOperator(f, first.Token, "http://localhost:3000", next); w.Code != http.StatusUnauthorized {
		t.Fatal("credential accepted on another allowed origin")
	}
	firstActor := actor
	if w := useOperator(f, first.Token, "https://evil.example.com", next); w.Code != http.StatusForbidden {
		t.Fatal("unlisted origin accepted")
	}
	f.server.cfg.OperatorEmails = []string{"removed@gmail.com"}
	if w := useOperator(f, first.Token, origin, next); w.Code != http.StatusUnauthorized {
		t.Fatal("removed allowlist account retained access")
	}
	f.server.cfg.OperatorEmails = []string{"operator@example.com"}
	if w := useOperator(f, first.Token, origin, f.server.revokeOperatorCredential); w.Code != http.StatusNoContent {
		t.Fatalf("current revoke: %d %s", w.Code, w.Body.String())
	}
	if w := useOperator(f, first.Token, origin, next); w.Code != http.StatusUnauthorized {
		t.Fatal("revoked credential accepted")
	}
	if w := useOperator(f, second.Token, origin, next); w.Code != http.StatusNoContent {
		t.Fatal("current revoke removed sibling")
	}
	if w := useOperator(f, second.Token, origin, f.server.revokeOperatorCredentials); w.Code != http.StatusNoContent {
		t.Fatalf("all revoke: %d %s", w.Code, w.Body.String())
	}
	if w := useOperator(f, second.Token, origin, next); w.Code != http.StatusUnauthorized {
		t.Fatal("all revoke retained credential")
	}
	if w := useOperator(f, other.Token, origin, next); w.Code != http.StatusNoContent {
		t.Fatal("all revoke affected different actor")
	}
	var tokenHash, actorID, emailHash, storedOrigin, role string
	if err := db.SQL.QueryRowContext(t.Context(), `SELECT token_hash,actor_id,email_hash,origin,role FROM operator_credentials WHERE id=$1`, other.ID).Scan(&tokenHash, &actorID, &emailHash, &storedOrigin, &role); err != nil {
		t.Fatal(err)
	}
	if tokenHash != hash(other.Token) || actorID != hash("operator.actor\x00https://accounts.google.com\x00another-operator-subject") || emailHash != hash("operator.email\x00operator@example.com") || storedOrigin != origin || role != "operator" {
		t.Fatal("unexpected credential persistence")
	}
	var issued, current, all, unsafe int
	if err := db.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FILTER (WHERE action='credential.issue'),COUNT(*) FILTER (WHERE action='credential.revoke'),COUNT(*) FILTER (WHERE action='credential.revoke_all'),COUNT(*) FILTER (WHERE operator_id NOT IN ($1,$2) OR operator_email_hash IS DISTINCT FROM $3 OR target_id_short LIKE '%@%' OR result LIKE '%@%') FROM arrival_operator_audit`, firstActor, actorID, hash("operator.email\x00operator@example.com")).Scan(&issued, &current, &all, &unsafe); err != nil || issued != 3 || current != 1 || all != 1 || unsafe != 0 {
		t.Fatalf("private actor audit: %d %d %d %d %v", issued, current, all, unsafe, err)
	}
	f.now = f.now.Add(2 * time.Minute)
	if w := useOperator(f, other.Token, origin, next); w.Code != http.StatusUnauthorized {
		t.Fatal("expired credential accepted")
	}
	if err := db.Store.CleanupOperatorCredentials(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err := db.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM operator_credentials`).Scan(&retained); err != nil || retained != 0 {
		t.Fatalf("expiry cleanup: %d %v", retained, err)
	}
	var attributable int
	if err := db.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM arrival_operator_audit WHERE operator_email_hash=$1`, hash("operator.email\x00operator@example.com")).Scan(&attributable); err != nil || attributable != 5 {
		t.Fatalf("credential cleanup lost attributable audit: %d %v", attributable, err)
	}
}

func TestOperatorIssuerLimitsBeforeGoogleAndActorOperationLimits(t *testing.T) {
	f, _ := newOperatorGoogleTestIssuer(t)
	for range 5 {
		w := httptest.NewRecorder()
		f.server.issueOperatorCredential(w, operatorGoogleRequest(f, "invalid"))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("invalid token: %d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	f.server.issueOperatorCredential(w, operatorGoogleRequest(f, operatorGoogleToken(t, f, nil)))
	if w.Code != http.StatusTooManyRequests || f.validations != 0 {
		t.Fatal("issuer did not bound Google lookups")
	}
	f.server.limiter = newRateLimiter()
	first := mintOperator(t, f, "google-operator-subject")
	second := mintOperator(t, f, "google-operator-subject")
	other := mintOperator(t, f, "another-subject")
	next := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }
	origin := f.server.cfg.OperatorAllowedOrigins[0]
	for range 30 {
		if w := useOperator(f, first.Token, origin, next); w.Code != http.StatusNoContent {
			t.Fatalf("operation budget: %d", w.Code)
		}
	}
	if w := useOperator(f, second.Token, origin, next); w.Code != http.StatusTooManyRequests {
		t.Fatal("credential renewal bypassed actor budget")
	}
	if w := useOperator(f, other.Token, origin, next); w.Code != http.StatusNoContent {
		t.Fatal("one actor exhausted another actor budget")
	}
}

func TestOperatorIssuerRequestShapeAndOriginBeforeGoogle(t *testing.T) {
	f, _ := newOperatorGoogleTestIssuer(t)
	token := operatorGoogleToken(t, f, nil)
	for _, tc := range []struct {
		name, body, query string
		change            func(*http.Request)
		status            int
	}{
		{"body", "{}", "", func(*http.Request) {}, 400},
		{"query", "", "?token=not-accepted", func(*http.Request) {}, 400},
		{"missing origin", "", "", func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"duplicate origin", "", "", func(r *http.Request) { r.Header.Add("Origin", r.Header.Get("Origin")) }, 403},
		{"renderer origin", "", "", func(r *http.Request) { r.Header.Set("Origin", f.server.cfg.DisplayAllowedOrigins[0]) }, 403},
		{"duplicate authorization", "", "", func(r *http.Request) { r.Header.Add("Authorization", "Bearer "+token) }, 401},
		{"legacy HMAC", "", "", func(r *http.Request) { r.Header.Set("X-Trace-Issuer-Signature", "legacy") }, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.server.limiter = newRateLimiter()
			r := httptest.NewRequest(http.MethodPost, "/v1/operator/credentials"+tc.query, strings.NewReader(tc.body))
			r.Header.Set("Origin", f.server.cfg.OperatorAllowedOrigins[0])
			r.Header.Set("Authorization", "Bearer "+token)
			tc.change(r)
			w := httptest.NewRecorder()
			f.server.issueOperatorCredential(w, r)
			if w.Code != tc.status {
				t.Fatalf("request shape: %d %s", w.Code, w.Body.String())
			}
		})
	}
	if f.validations != 0 {
		t.Fatal("rejected request reached Google verifier")
	}
}

func TestOperatorGoogleGmailIssuerAliasesShareActor(t *testing.T) {
	f, db := newOperatorGoogleTestIssuer(t)
	f.server.cfg.OperatorEmails = []string{"operator@gmail.com"}
	actor := hash("operator.actor\x00https://accounts.google.com\x00gmail-subject")
	for _, issuer := range []string{"https://accounts.google.com", "accounts.google.com"} {
		token := operatorGoogleToken(t, f, func(c map[string]any) {
			c["iss"], c["sub"], c["email"] = issuer, "gmail-subject", "Operator@Gmail.com"
			c["azp"] = f.server.cfg.OperatorGoogleClientID
			delete(c, "hd")
		})
		w := httptest.NewRecorder()
		f.server.issueOperatorCredential(w, operatorGoogleRequest(f, token))
		if w.Code != http.StatusOK {
			t.Fatalf("Gmail issuer alias rejected: %d %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), "gmail-subject") || strings.Contains(w.Body.String(), "gmail.com") {
			t.Fatal("Google identity leaked in operator credential response")
		}
	}
	var credentials int
	if err := db.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM operator_credentials WHERE actor_id=$1`, actor).Scan(&credentials); err != nil || credentials != 2 {
		t.Fatalf("issuer aliases did not share pseudonymous actor: %d %v", credentials, err)
	}
}

func TestOperatorVerifiedIdentitySurvivesInControlAuditAfterLogout(t *testing.T) {
	f, db := newOperatorGoogleTestIssuer(t)
	credential := mintOperator(t, f, "control-operator")
	origin := f.server.cfg.OperatorAllowedOrigins[0]
	if w := useOperator(f, credential.Token, origin, f.server.closeRegistration); w.Code != http.StatusOK {
		t.Fatalf("verified operator control: %d %s", w.Code, w.Body.String())
	}
	if w := useOperator(f, credential.Token, origin, f.server.revokeOperatorCredential); w.Code != http.StatusNoContent {
		t.Fatalf("verified operator logout: %d %s", w.Code, w.Body.String())
	}
	if err := db.Store.CleanupOperatorCredentials(t.Context(), f.now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var actor, emailHash, result string
	if err := db.SQL.QueryRowContext(t.Context(), `SELECT operator_id,operator_email_hash,result FROM arrival_operator_audit WHERE action='registration.close'`).Scan(&actor, &emailHash, &result); err != nil {
		t.Fatal(err)
	}
	if actor != hash("operator.actor\x00https://accounts.google.com\x00control-operator") || emailHash != hash("operator.email\x00operator@example.com") || result != "closed" {
		t.Fatal("control audit lost the verified account identity")
	}
}
