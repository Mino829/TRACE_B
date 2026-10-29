package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
)

const operatorCredentialLifetime = 2 * time.Minute

type operatorCredentialKey struct{}

func (s *Server) operatorOriginAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, allowed := range s.cfg.OperatorAllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

func (s *Server) operatorAvailable(w http.ResponseWriter, r *http.Request) bool {
	if !s.cfg.OperatorEnabled || s.cfg.ValidateOperator() != nil {
		s.writeError(w, requestID(r), &apiError{Code: "NOT_AVAILABLE", Message: "operator access is disabled", Status: http.StatusServiceUnavailable})
		return false
	}
	if len(r.Header.Values("Origin")) != 1 || !s.operatorOriginAllowed(r.Header.Get("Origin")) {
		s.writeError(w, requestID(r), &apiError{Code: "ORIGIN_NOT_ALLOWED", Message: "operator origin is not allowed", Status: http.StatusForbidden})
		return false
	}
	return true
}

func legacyOperatorHeaders(r *http.Request) bool {
	return len(r.Header.Values("X-Trace-Issuer-Timestamp")) != 0 || len(r.Header.Values("X-Trace-Issuer-Signature")) != 0 ||
		len(r.Header.Values("X-Trace-Display-Issuer-Timestamp")) != 0 || len(r.Header.Values("X-Trace-Display-Issuer-Signature")) != 0
}

func (s *Server) denyOperator(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, requestID(r), &apiError{Code: "UNAUTHORIZED", Message: "valid operator credentials required", Status: http.StatusUnauthorized})
}

func (s *Server) operatorOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.operatorAvailable(w, r) {
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			s.writeError(w, requestID(r), &apiError{Code: "INVALID_REQUEST", Message: "request query must be empty", Status: http.StatusBadRequest})
			return
		}
		if len(r.Header.Values("Authorization")) != 1 || legacyOperatorHeaders(r) {
			s.denyOperator(w, r)
			return
		}
		token, err := bearerToken(r)
		if err != nil || len(token) != 43 || strings.ContainsAny(token, " .\t\r\n,") {
			s.denyOperator(w, r)
			return
		}
		now := s.now().UTC()
		credential, lookupErr := s.store.LookupOperatorCredential(r.Context(), hash(token), now)
		if errors.Is(lookupErr, store.ErrNotFound) {
			s.denyOperator(w, r)
			return
		}
		if lookupErr != nil {
			s.writeError(w, requestID(r), internalError())
			return
		}
		allowed := false
		for _, email := range s.cfg.OperatorEmails {
			if hash("operator.email\x00"+strings.ToLower(strings.TrimSpace(email))) == credential.EmailHash {
				allowed = true
				break
			}
		}
		if !allowed || credential.Role != "operator" || credential.Origin != r.Header.Get("Origin") || !now.Before(credential.ExpiresAt) {
			s.denyOperator(w, r)
			return
		}
		if wait := s.limiter.allow(now,
			rateRule{key: "operator/actor/" + credential.ActorID, rate: 1, burst: 30},
			rateRule{key: "operator/global", rate: 20, burst: 100}); wait > 0 {
			s.rateLimited(w, r, wait)
			return
		}
		ctx := store.WithOperatorIdentity(r.Context(), credential.ActorID, credential.EmailHash)
		ctx = context.WithValue(ctx, operatorCredentialKey{}, credential.ID)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) emptyOperatorRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_REQUEST", Message: "request query must be empty", Status: http.StatusBadRequest})
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(body) != 0 {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_REQUEST", Message: "request body must be empty", Status: http.StatusBadRequest})
		return false
	}
	return true
}

func (s *Server) issueOperatorCredential(w http.ResponseWriter, r *http.Request) {
	if !s.operatorAvailable(w, r) {
		return
	}
	now := s.now().UTC()
	// This limit precedes Google verification, including invalid token traffic.
	if wait := s.limiter.allow(now,
		rateRule{key: "operator-issuer/ip/" + s.clientIP(r), rate: 0.2, burst: 5},
		rateRule{key: "operator-issuer/global", rate: 1, burst: 10}); wait > 0 {
		s.rateLimited(w, r, wait)
		return
	}
	if !s.emptyOperatorRequest(w, r) {
		return
	}
	if len(r.Header.Values("Authorization")) != 1 || legacyOperatorHeaders(r) {
		s.denyOperator(w, r)
		return
	}
	token, authErr := bearerToken(r)
	if authErr != nil || !googleWebTokenHeaderValid(token) || s.validateGoogleIDToken == nil {
		s.denyOperator(w, r)
		return
	}
	payload, err := s.validateGoogleIDToken(r.Context(), token, s.cfg.OperatorGoogleClientID)
	now = s.now().UTC()
	if err != nil || !googleIdentityAllowed(payload, s.cfg.OperatorGoogleClientID, s.cfg.OperatorEmails, now) {
		s.denyOperator(w, r)
		return
	}
	// Both accepted Google issuer spellings identify the same account.
	actor := hash("operator.actor\x00https://accounts.google.com\x00" + payload.Subject)
	if wait := s.limiter.allow(now, rateRule{key: "operator-issuer/actor/" + actor, rate: 0.1, burst: 5}); wait > 0 {
		s.rateLimited(w, r, wait)
		return
	}
	opaque := randomSecret()
	credential := store.OperatorCredential{
		ID: randomID("opc"), TokenHash: hash(opaque), ActorID: actor,
		EmailHash: hash("operator.email\x00" + strings.ToLower(strings.TrimSpace(payload.Claims["email"].(string)))),
		Origin:    r.Header.Get("Origin"), Role: "operator", CreatedAt: now, ExpiresAt: now.Add(operatorCredentialLifetime),
	}
	if err := s.store.CreateOperatorCredential(store.WithOperatorIdentity(r.Context(), actor, credential.EmailHash), credential); err != nil {
		s.writeError(w, requestID(r), internalError())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"operatorToken": opaque, "credentialId": credential.ID, "expiresAt": credential.ExpiresAt, "role": credential.Role})
}

func (s *Server) revokeOperatorCredential(w http.ResponseWriter, r *http.Request) {
	s.revokeOwnOperatorCredentials(w, r, false)
}

func (s *Server) revokeOperatorCredentials(w http.ResponseWriter, r *http.Request) {
	s.revokeOwnOperatorCredentials(w, r, true)
}

func (s *Server) revokeOwnOperatorCredentials(w http.ResponseWriter, r *http.Request, all bool) {
	if !s.emptyOperatorRequest(w, r) {
		return
	}
	id, _ := r.Context().Value(operatorCredentialKey{}).(string)
	err := s.store.RevokeOperatorCredentials(r.Context(), id, all, s.now().UTC())
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
		s.denyOperator(w, r)
		return
	}
	if err != nil {
		s.writeError(w, requestID(r), internalError())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
