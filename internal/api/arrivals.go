package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
)

const checkpointLifetime = 25 * time.Second
const issuerSignatureLifetime = 15 * time.Second

type checkpointProof struct {
	CheckpointID string `json:"checkpointId"`
	Venue        string `json:"venue"`
	IssuedAt     int64  `json:"issuedAt"`
	ExpiresAt    int64  `json:"expiresAt"`
	Nonce        string `json:"nonce"`
}

func mac(key, message string) []byte {
	h := hmac.New(sha256.New, []byte(key))
	_, _ = h.Write([]byte(message))
	return h.Sum(nil)
}

// A projected proof is a signed JSON payload, not a navigable URL. It never
// contains a participant token; the Web App camera retains its existing session.
func (s *Server) issueArrivalProof(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeIssuer(w, r) {
		return
	}
	body, bodyErr := readBody(w, r)
	if bodyErr != nil {
		s.writeError(w, requestID(r), bodyErr)
		return
	}
	if len(body) != 0 {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_REQUEST", Message: "issuer request must have no body", Status: http.StatusBadRequest})
		return
	}
	now := s.now().UTC()
	proof := checkpointProof{CheckpointID: s.cfg.ArrivalCheckpointID, Venue: s.cfg.ArrivalVenue,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(checkpointLifetime).Unix(), Nonce: randomSecret()}
	payload, err := json.Marshal(proof)
	if err != nil {
		s.writeError(w, requestID(r), internalError())
		return
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	token := encoded + "." + base64.RawURLEncoding.EncodeToString(mac(s.cfg.ArrivalProofKey, encoded))
	s.writeJSON(w, http.StatusOK, map[string]any{"checkpointToken": token, "expiresAt": time.Unix(proof.ExpiresAt, 0).UTC()})
}

// Operator HMAC authorization is entirely separate from participant Bearer auth
// and from the QR signing key. Timestamp signatures expire in 15 seconds; key
// rotation revokes all previously issued operator signatures immediately.
func (s *Server) authorizeIssuer(w http.ResponseWriter, r *http.Request) bool {
	if !s.cfg.ArrivalEnabled || len(s.cfg.ArrivalIssuerKey) < 32 || len(s.cfg.ArrivalProofKey) < 32 ||
		s.cfg.ArrivalIssuerKey == s.cfg.ArrivalProofKey ||
		(s.cfg.EdgeSharedSecret != "" && s.cfg.ArrivalIssuerKey == s.cfg.EdgeSharedSecret) {
		s.writeError(w, requestID(r), &apiError{Code: "NOT_AVAILABLE", Message: "arrival is unavailable", Status: http.StatusServiceUnavailable})
		return false
	}
	if r.Header.Get("Origin") != "" || len(r.Header.Values("X-Trace-Issuer-Timestamp")) != 1 ||
		len(r.Header.Values("X-Trace-Issuer-Signature")) != 1 {
		s.writeError(w, requestID(r), &apiError{Code: "FORBIDDEN", Message: "operator credentials required", Status: http.StatusForbidden})
		return false
	}
	timestamp := r.Header.Get("X-Trace-Issuer-Timestamp")
	value, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || strconv.FormatInt(value, 10) != timestamp ||
		s.now().UTC().Sub(time.Unix(value, 0)) > issuerSignatureLifetime ||
		time.Unix(value, 0).Sub(s.now().UTC()) > issuerSignatureLifetime {
		s.writeError(w, requestID(r), &apiError{Code: "FORBIDDEN", Message: "operator credentials expired", Status: http.StatusForbidden})
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Trace-Issuer-Signature"))
	if err != nil || subtle.ConstantTimeCompare(sig, mac(s.cfg.ArrivalIssuerKey, r.Method+"\n"+r.URL.Path+"\n"+timestamp)) != 1 {
		s.writeError(w, requestID(r), &apiError{Code: "FORBIDDEN", Message: "operator credentials required", Status: http.StatusForbidden})
		return false
	}
	if wait := s.limiter.allow(s.now(), rateRule{key: "issuer/global", rate: 1, burst: 10},
		rateRule{key: "api/global", rate: 100, burst: 100}); wait > 0 {
		s.rateLimited(w, r, wait)
		return false
	}
	return true
}

func (s *Server) verifyCheckpoint(value string, now time.Time) (string, bool) {
	if len(value) > 2048 || len(value) < 30 {
		return "", false
	}
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return "", false
	}
	actual, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || subtle.ConstantTimeCompare(actual, mac(s.cfg.ArrivalProofKey, parts[0])) != 1 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	var proof checkpointProof
	if err := json.Unmarshal(payload, &proof); err != nil {
		return "", false
	}
	nonce, err := base64.RawURLEncoding.DecodeString(proof.Nonce)
	if err != nil || len(nonce) < 16 || proof.Venue != s.cfg.ArrivalVenue ||
		proof.CheckpointID != s.cfg.ArrivalCheckpointID || proof.IssuedAt > now.Unix() ||
		proof.ExpiresAt <= now.Unix() || proof.ExpiresAt <= proof.IssuedAt ||
		proof.ExpiresAt-proof.IssuedAt > int64(checkpointLifetime.Seconds()) {
		return "", false
	}
	return proof.CheckpointID, true
}

type arrivalRequest struct {
	CheckpointToken     string `json:"checkpointToken"`
	ExperienceLinkToken string `json:"experienceLinkToken"`
	ClientTime          string `json:"clientTime"`
}

func (s *Server) arrive(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.ArrivalEnabled {
		s.writeError(w, requestID(r), &apiError{Code: "NOT_AVAILABLE", Message: "arrival is unavailable", Status: http.StatusServiceUnavailable})
		return
	}
	memoryID := r.PathValue("memorySessionId")
	if err := s.authenticateMemory(r, memoryID); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) == 0 || len(key) > 200 {
		s.writeError(w, requestID(r), &apiError{Code: "IDEMPOTENCY_KEY_REQUIRED", Message: "a valid Idempotency-Key header is required", Status: http.StatusBadRequest})
		return
	}
	body, inputErr := readBody(w, r)
	if inputErr != nil {
		s.writeError(w, requestID(r), inputErr)
		return
	}
	var input arrivalRequest
	if err := decodeBytes(body, &input); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	now := s.now().UTC()
	// A receipt can be replayed even after its QR expires, but not after its
	// MemorySession is deleted (authentication above fails then).
	token, _ := bearerToken(r)
	receiptKey := hash(key + "\x00" + token)
	record, err := s.store.GetArrivalReceipt(r.Context(), memoryID, receiptKey, now)
	if err == nil {
		if record.RequestHash != hashBytes(body) {
			s.writeError(w, requestID(r), &apiError{Code: "IDEMPOTENCY_CONFLICT", Message: "the idempotency key was already used with different content", Status: http.StatusConflict})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(record.ResponseBody)
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		s.writeStoreError(w, r, err)
		return
	}
	checkpointID, valid := s.verifyCheckpoint(input.CheckpointToken, now)
	if !valid {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_CHECKPOINT", Message: "checkpoint proof is invalid or expired", Status: http.StatusForbidden})
		return
	}
	result, err := s.store.AcceptArrival(r.Context(), memoryID, hash(input.ExperienceLinkToken), randomID("arrival"), checkpointID,
		receiptKey, hashBytes(body), now)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
}

func (s *Server) operatorArrivals(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeIssuer(w, r) {
		return
	}
	candidates, err := s.store.ReplayCandidates(r.Context(), s.now().UTC())
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if candidates == nil {
		candidates = []store.ReplayCandidate{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"replayCandidates": candidates})
}

func (s *Server) operatorSkipArrival(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeIssuer(w, r) {
		return
	}
	if err := s.store.SkipArrival(r.Context(), r.PathValue("arrivalId"), s.now().UTC()); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"replayStatus": "skipped"})
}

func (s *Server) operatorRetryArrival(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeIssuer(w, r) {
		return
	}
	if !s.cfg.DisplayEnabled {
		s.writeError(w, requestID(r), &apiError{Code: "NOT_AVAILABLE", Message: "display is disabled", Status: http.StatusServiceUnavailable})
		return
	}
	if err := s.store.RetryDisplayArrival(r.Context(), r.PathValue("arrivalId"), s.now().UTC()); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"replayStatus": "queued"})
}

func (s *Server) skipOwnArrival(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.ArrivalEnabled {
		s.writeError(w, requestID(r), &apiError{Code: "NOT_AVAILABLE", Message: "arrival is unavailable", Status: http.StatusServiceUnavailable})
		return
	}
	memoryID := r.PathValue("memorySessionId")
	if err := s.authenticateMemory(r, memoryID); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	if err := s.store.SkipOwnArrival(r.Context(), memoryID, s.now().UTC()); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"replayStatus": "skipped"})
}
