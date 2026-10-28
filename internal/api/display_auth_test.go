package api

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/testdb"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/option"
)

type displayGoogleJWKSTransport func(*http.Request) (*http.Response, error)

func (f displayGoogleJWKSTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type displayGoogleTestIssuer struct {
	server      *Server
	key         *rsa.PrivateKey
	now         time.Time
	validations int
}

func newDisplayGoogleTestIssuer(t *testing.T) *displayGoogleTestIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &displayGoogleTestIssuer{key: key, now: time.Now().UTC().Truncate(time.Second)}
	jwks, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "display-google-test",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	validator, err := idtoken.NewValidator(t.Context(), option.WithHTTPClient(&http.Client{
		Transport: displayGoogleJWKSTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "https://www.googleapis.com/oauth2/v3/certs" {
				return nil, fmt.Errorf("unexpected Google verification endpoint")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Cache-Control": {"public, max-age=3600"}}, Body: io.NopCloser(strings.NewReader(string(jwks))), Request: r}, nil
		}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DisplayEnabled = true
	cfg.DisplayGoogleClientID = "123456789-displayweb.apps.googleusercontent.com"
	cfg.DisplayOperatorEmails = []string{"operator@example.com"}
	cfg.DisplayAllowedOrigins = []string{"https://display.example.com", "http://localhost:3000"}
	cfg.DisplayTokenKey = "display-token-signing-key-independent-123456"
	cfg.EdgeSharedSecret = "participant-edge-secret-remains-private-123456"
	fixture.server = &Server{cfg: cfg, now: func() time.Time { return fixture.now }, limiter: newRateLimiter()}
	fixture.server.validateGoogleIDToken = func(ctx context.Context, token, audience string) (*idtoken.Payload, error) {
		fixture.validations++
		return validator.Validate(ctx, token, audience)
	}
	return fixture
}

func (f *displayGoogleTestIssuer) token(t *testing.T, change func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{
		"iss": "https://accounts.google.com", "aud": f.server.cfg.DisplayGoogleClientID,
		"sub": "google-operator-subject", "email": "Operator@Example.com", "email_verified": true, "hd": "example.com",
		"iat": f.now.Add(-time.Minute).Unix(), "exp": f.now.Add(time.Hour).Unix(),
	}
	if change != nil {
		change(claims)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"display-google-test","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	content := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(content))
	signature, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return content + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (f *displayGoogleTestIssuer) request(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/operator/display-tokens", nil)
	r.Header.Set("Origin", f.server.cfg.DisplayAllowedOrigins[0])
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func (f *displayGoogleTestIssuer) send(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.server.issueDisplayToken(w, r)
	return w
}

func TestDisplayGoogleIssuerMintsOriginBoundShortLivedCredential(t *testing.T) {
	f := newDisplayGoogleTestIssuer(t)
	for _, issuer := range []string{"https://accounts.google.com", "accounts.google.com"} {
		googleToken := f.token(t, func(claims map[string]any) {
			claims["iss"] = issuer
			claims["azp"] = f.server.cfg.DisplayGoogleClientID
		})
		w := f.send(f.request(googleToken))
		if w.Code != http.StatusOK {
			t.Fatalf("authorized Google operator: %d %s", w.Code, w.Body.String())
		}
		var response struct {
			Token     string    `json:"displayToken"`
			ExpiresAt time.Time `json:"expiresAt"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		origin := f.server.cfg.DisplayAllowedOrigins[0]
		if !f.server.verifyDisplayToken(response.Token, origin, f.now) ||
			f.server.verifyDisplayToken(response.Token, "https://other.example.com", f.now) ||
			f.server.verifyDisplayToken(response.Token, origin, f.now.Add(2*time.Minute)) ||
			!response.ExpiresAt.Equal(f.now.Add(2*time.Minute)) {
			t.Fatal("display credential is not origin-bound with a two-minute lifetime")
		}
		parts := strings.Split(response.Token, ".")
		raw, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{googleToken, f.server.cfg.DisplayTokenKey, f.server.cfg.EdgeSharedSecret, "operator@example.com", "google-operator-subject"} {
			if strings.Contains(w.Body.String(), secret) || strings.Contains(strings.ToLower(string(raw)), secret) {
				t.Fatal("operator identity or a long-lived credential leaked in the response")
			}
		}
	}
	// Gmail needs no hosted domain; a single audience needs no azp.
	f.server.cfg.DisplayOperatorEmails = []string{"operator@gmail.com"}
	if w := f.send(f.request(f.token(t, func(c map[string]any) {
		c["email"] = "operator@gmail.com"
		delete(c, "hd")
	}))); w.Code != http.StatusOK {
		t.Fatalf("Gmail single-audience token: %d", w.Code)
	}
}

func TestDisplayGoogleIssuerRejectsSignedInvalidIdentityClaims(t *testing.T) {
	f := newDisplayGoogleTestIssuer(t)
	for _, tt := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"wrong audience", func(c map[string]any) { c["aud"] = "other-client.apps.googleusercontent.com" }},
		{"multiple audiences", func(c map[string]any) { c["aud"] = []string{f.server.cfg.DisplayGoogleClientID, "another-client"} }},
		{"missing audience", func(c map[string]any) { delete(c, "aud") }},
		{"wrong issuer", func(c map[string]any) { c["iss"] = "https://accounts.google.com.evil.example" }},
		{"unverified email", func(c map[string]any) { c["email_verified"] = false }},
		{"string verification", func(c map[string]any) { c["email_verified"] = "true" }},
		{"missing verification", func(c map[string]any) { delete(c, "email_verified") }},
		{"unlisted operator", func(c map[string]any) { c["email"] = "participant@example.com" }},
		{"missing email", func(c map[string]any) { delete(c, "email") }},
		{"missing subject", func(c map[string]any) { delete(c, "sub") }},
		{"blank subject", func(c map[string]any) { c["sub"] = " " }},
		{"third-party email without Google authority", func(c map[string]any) { delete(c, "hd") }},
		{"nonstring hosted domain", func(c map[string]any) { c["hd"] = true }},
		{"empty hosted domain", func(c map[string]any) { c["hd"] = "" }},
		{"expired", func(c map[string]any) { c["exp"] = f.now.Add(-time.Second).Unix() }},
		{"expires now", func(c map[string]any) { c["exp"] = f.now.Unix() }},
		{"missing expiry", func(c map[string]any) { delete(c, "exp") }},
		{"future issued", func(c map[string]any) { c["iat"] = f.now.Add(time.Minute).Unix() }},
		{"missing issued time", func(c map[string]any) { delete(c, "iat") }},
		{"expiry before issued", func(c map[string]any) {
			c["iat"] = f.now.Add(time.Hour).Unix()
			c["exp"] = f.now.Add(time.Minute).Unix()
		}},
		{"wrong authorized party", func(c map[string]any) { c["azp"] = "different-web-client" }},
		{"nonstring authorized party", func(c map[string]any) { c["azp"] = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f.server.limiter = newRateLimiter()
			token := f.token(t, tt.change)
			w := f.send(f.request(token))
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"code":"UNAUTHORIZED"`) ||
				strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), "idtoken:") || strings.Contains(w.Body.String(), "displayToken") {
				t.Fatalf("invalid Google identity: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDisplayGoogleIssuerRejectsOtherCredentialsAndRequestShapes(t *testing.T) {
	f := newDisplayGoogleTestIssuer(t)
	valid := f.token(t, nil)
	minted := f.send(f.request(valid))
	var display struct {
		Token string `json:"displayToken"`
	}
	if minted.Code != http.StatusOK || json.Unmarshal(minted.Body.Bytes(), &display) != nil || display.Token == "" {
		t.Fatal("could not prepare a real display credential")
	}
	for _, tt := range []struct {
		name   string
		status int
		mutate func(*http.Request)
	}{
		{"invalid signature", 401, func(r *http.Request) {
			parts := strings.Split(valid, ".")
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			sig[0] ^= 1
			r.Header.Set("Authorization", "Bearer "+parts[0]+"."+parts[1]+"."+base64.RawURLEncoding.EncodeToString(sig))
		}},
		{"participant credential", 401, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+randomSecret()) }},
		{"display credential", 401, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+display.Token) }},
		{"missing auth", 401, func(r *http.Request) { r.Header.Del("Authorization") }},
		{"multiple auth", 401, func(r *http.Request) { r.Header.Add("Authorization", "Bearer "+valid) }},
		{"combined auth", 401, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+valid+", Bearer "+valid) }},
		{"missing origin", 403, func(r *http.Request) { r.Header.Del("Origin") }},
		{"null origin", 403, func(r *http.Request) { r.Header.Set("Origin", "null") }},
		{"unlisted origin", 403, func(r *http.Request) { r.Header.Set("Origin", "https://display.example.com.evil.example") }},
		{"multiple origins", 403, func(r *http.Request) { r.Header.Add("Origin", "https://display.example.com") }},
		{"origin list", 403, func(r *http.Request) { r.Header.Set("Origin", "https://display.example.com http://localhost:3000") }},
		{"body", 400, func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader("{}")) }},
		{"whitespace body", 400, func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(" ")) }},
		{"query", 400, func(r *http.Request) { r.URL.RawQuery = "id_token=" + valid }},
		{"empty query marker", 400, func(r *http.Request) { r.URL.ForceQuery = true }},
		{"old HMAC path", 401, func(r *http.Request) {
			r.Header.Del("Authorization")
			r.Header.Set("X-Trace-Display-Issuer-Timestamp", fmt.Sprint(f.now.Unix()))
			r.Header.Set("X-Trace-Display-Issuer-Signature", base64.RawURLEncoding.EncodeToString(mac("retired-issuer-key", "POST\n/v1/operator/display-tokens\n"+fmt.Sprint(f.now.Unix()))))
		}},
		{"old HMAC with Google", 401, func(r *http.Request) { r.Header.Set("X-Trace-Display-Issuer-Signature", "") }},
		{"short ES256 signature", 401, func(r *http.Request) {
			header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","kid":"unrelated"}`))
			r.Header.Set("Authorization", "Bearer "+header+"."+strings.Split(valid, ".")[1]+".AA")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f.server.limiter = newRateLimiter()
			r := f.request(valid)
			tt.mutate(r)
			w := f.send(r)
			if w.Code != tt.status || strings.Contains(w.Body.String(), valid) || strings.Contains(w.Body.String(), "displayToken") {
				t.Fatalf("invalid mint request: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDisplayGoogleIssuerLimitsBeforeGoogleValidation(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(fmt.Sprint("global=", global), func(t *testing.T) {
			f := newDisplayGoogleTestIssuer(t)
			invalid := f.token(t, func(c map[string]any) { c["iss"] = "untrusted-issuer" })
			burst := 5
			if global {
				burst = 10
			}
			for i := 0; i <= burst; i++ {
				r := f.request(invalid)
				if global {
					r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i+1)
				}
				w := f.send(r)
				want := http.StatusUnauthorized
				if i == burst {
					want = http.StatusTooManyRequests
				}
				if w.Code != want || (i == burst && w.Header().Get("Retry-After") == "") {
					t.Fatalf("invalid JWT flood request %d: %d %s", i, w.Code, w.Body.String())
				}
			}
			if f.validations != burst {
				t.Fatalf("quota did not precede SDK verification: validations=%d", f.validations)
			}
		})
	}
}

func TestDisplayGoogleIssuerUsesBrowserMiddlewareWithoutEdgeSecret(t *testing.T) {
	f := newDisplayGoogleTestIssuer(t)
	db := testdb.Open(t)
	f.server.store = db.Store
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/operator/display-tokens", f.server.issueDisplayToken)
	handler := f.server.middleware(mux)
	r := f.request(f.token(t, nil))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("Access-Control-Allow-Origin") != r.Header.Get("Origin") ||
		w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Request-ID") == "" {
		t.Fatalf("browser Google issuance through production middleware: %d %s", w.Code, w.Body.String())
	}
}
