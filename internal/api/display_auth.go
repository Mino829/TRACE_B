package api

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"google.golang.org/api/idtoken"
)

func (s *Server) displayOriginAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, allowed := range s.cfg.DisplayAllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

func (s *Server) issueDisplayToken(w http.ResponseWriter, r *http.Request) {
	if !s.displayAvailable() {
		s.writeError(w, requestID(r), &apiError{Code: "NOT_AVAILABLE", Message: "display is disabled", Status: http.StatusServiceUnavailable})
		return
	}
	now := s.now().UTC()
	// Bound even invalid JWT traffic before it can trigger Google JWKS requests.
	if wait := s.limiter.allow(now,
		rateRule{key: "display-issuer/ip/" + s.clientIP(r), rate: 0.2, burst: 5},
		rateRule{key: "display-issuer/global", rate: 1, burst: 10},
		rateRule{key: "api/global", rate: 100, burst: 100}); wait > 0 {
		s.rateLimited(w, r, wait)
		return
	}
	if len(r.Header.Values("Origin")) != 1 || !s.displayOriginAllowed(r.Header.Get("Origin")) {
		s.writeError(w, requestID(r), &apiError{Code: "ORIGIN_NOT_ALLOWED", Message: "display origin is not allowed", Status: http.StatusForbidden})
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_REQUEST", Message: "request query must be empty", Status: http.StatusBadRequest})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(body) != 0 {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_REQUEST", Message: "request body must be empty", Status: http.StatusBadRequest})
		return
	}
	deny := func() {
		// SDK errors can contain JWT contents or claims; do not return or log them.
		s.writeError(w, requestID(r), &apiError{Code: "UNAUTHORIZED", Message: "valid Google operator credentials required", Status: http.StatusUnauthorized})
	}
	if len(r.Header.Values("Authorization")) != 1 ||
		len(r.Header.Values("X-Trace-Display-Issuer-Timestamp")) != 0 ||
		len(r.Header.Values("X-Trace-Display-Issuer-Signature")) != 0 {
		deny()
		return
	}
	token, authErr := bearerToken(r)
	if authErr != nil || !googleWebTokenHeaderValid(token) || s.validateGoogleIDToken == nil {
		deny()
		return
	}
	payload, err := s.validateGoogleIDToken(r.Context(), token, s.cfg.DisplayGoogleClientID)
	now = s.now().UTC()
	if err != nil || !s.displayOperatorAllowed(payload, now) {
		deny()
		return
	}
	claim := displayCredential{
		Purpose: "display.ws", IssuedAt: now.Unix(), ExpiresAt: now.Add(displayCredentialLifetime).Unix(),
		Nonce: randomSecret(), Origin: r.Header.Get("Origin"),
	}
	encodedPayload, err := json.Marshal(claim)
	if err != nil {
		s.writeError(w, requestID(r), internalError())
		return
	}
	encoded := base64.RawURLEncoding.EncodeToString(encodedPayload)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"displayToken": encoded + "." + base64.RawURLEncoding.EncodeToString(mac(s.cfg.DisplayTokenKey, encoded)),
		"expiresAt":    time.Unix(claim.ExpiresAt, 0).UTC(),
	})
}

// Google OAuth Web ID tokens use RS256. Check the algorithm before the SDK;
// unrelated ES256 tokens must never reach its fixed-width signature decoder.
// This is only a format gate: the SDK still verifies the signature and audience.
func googleWebTokenHeaderValid(token string) bool {
	if len(token) == 0 || len(token) > 16<<10 || strings.ContainsAny(token, " \t\r\n,") {
		return false
	}
	encodedHeader, rest, headerOK := strings.Cut(token, ".")
	_, signature, signatureOK := strings.Cut(rest, ".")
	if !headerOK || !signatureOK || strings.Contains(signature, ".") {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encodedHeader)
	if err != nil {
		return false
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	return json.Unmarshal(raw, &header) == nil && header.Algorithm == "RS256" && header.KeyID != ""
}

func (s *Server) displayOperatorAllowed(payload *idtoken.Payload, now time.Time) bool {
	if payload == nil || (payload.Issuer != "https://accounts.google.com" && payload.Issuer != "accounts.google.com") ||
		payload.Audience != s.cfg.DisplayGoogleClientID || payload.Subject == "" || len(payload.Subject) > 255 ||
		strings.ContainsAny(payload.Subject, " \t\r\n") || payload.IssuedAt <= 0 || payload.IssuedAt > now.Unix() ||
		payload.Expires <= now.Unix() || payload.Expires <= payload.IssuedAt {
		return false
	}
	// The SDK accepts only a single string audience, rejecting multi-audience
	// tokens. If an authorized party is present it must be this Web client too.
	if azp, exists := payload.Claims["azp"]; exists {
		party, ok := azp.(string)
		if !ok || party != s.cfg.DisplayGoogleClientID {
			return false
		}
	}
	verified, ok := payload.Claims["email_verified"].(bool)
	if !ok || !verified {
		return false
	}
	email, ok := payload.Claims["email"].(string)
	if !ok {
		return false
	}
	email = strings.ToLower(strings.TrimSpace(email))
	// Google is authoritative for Gmail and verified Workspace addresses,
	// not for an arbitrary third-party email registered years ago.
	if !strings.HasSuffix(email, "@gmail.com") {
		domain, ok := payload.Claims["hd"].(string)
		if !ok || domain == "" {
			return false
		}
	}
	for _, allowed := range s.cfg.DisplayOperatorEmails {
		if email == allowed {
			return true
		}
	}
	return false
}
