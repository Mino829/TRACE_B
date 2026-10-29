package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
)

const checkpointLifetime = 25 * time.Second

type checkpointProof struct {
	Purpose      string `json:"purpose"`
	KeyID        string `json:"keyId"`
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
	if !s.cfg.ArrivalEnabled {
		s.writeError(w, requestID(r), &apiError{Code: "NOT_AVAILABLE", Message: "arrival is unavailable", Status: http.StatusServiceUnavailable})
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
	proof := checkpointProof{Purpose: "arrival", KeyID: s.cfg.ArrivalProofKeyID,
		CheckpointID: s.cfg.ArrivalCheckpointID, Venue: s.cfg.ArrivalVenue,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(checkpointLifetime).Unix(), Nonce: randomSecret()}
	payload, err := json.Marshal(proof)
	if err != nil {
		s.writeError(w, requestID(r), internalError())
		return
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	token := encoded + "." + base64.RawURLEncoding.EncodeToString(mac(s.cfg.ArrivalProofKey, encoded))
	if err := s.store.RecordArrivalProofIssue(r.Context(), proof.CheckpointID, now); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"checkpointToken": token, "expiresAt": time.Unix(proof.ExpiresAt, 0).UTC()})
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
	if err != nil || len(nonce) < 16 || proof.Purpose != "arrival" || proof.KeyID == "" ||
		proof.KeyID != s.cfg.ArrivalProofKeyID || proof.Venue != s.cfg.ArrivalVenue ||
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
	// The store resolves existing arrivals and receipts under the queue/session
	// locks. Expired proofs cannot create an arrival but may recover current state.
	token, _ := bearerToken(r)
	receiptKey := hash(key + "\x00" + token)
	checkpointID, _ := s.verifyCheckpoint(input.CheckpointToken, now)
	result, err := s.store.AcceptArrival(r.Context(), memoryID, hash(input.ExperienceLinkToken), randomID("arrival"), checkpointID,
		receiptKey, hashBytes(body), now)
	if err != nil {
		if errors.Is(err, store.ErrInvalidArrivalProof) {
			s.writeError(w, requestID(r), &apiError{Code: "INVALID_ARRIVAL_PROOF", Message: "arrival proof is invalid or expired", Status: http.StatusBadRequest})
			return
		}
		s.writeStoreError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
}

func (s *Server) operatorArrivals(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.ArrivalEnabled {
		s.writeError(w, requestID(r), &apiError{Code: "NOT_AVAILABLE", Message: "arrival is unavailable", Status: http.StatusServiceUnavailable})
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
	if !s.cfg.ArrivalEnabled {
		s.writeError(w, requestID(r), &apiError{Code: "NOT_AVAILABLE", Message: "arrival is unavailable", Status: http.StatusServiceUnavailable})
		return
	}
	if err := s.store.SkipArrival(r.Context(), r.PathValue("arrivalId"), s.now().UTC()); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"replayStatus": "skipped"})
}

func (s *Server) operatorRetryArrival(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.ArrivalEnabled {
		s.writeError(w, requestID(r), &apiError{Code: "NOT_AVAILABLE", Message: "arrival is unavailable", Status: http.StatusServiceUnavailable})
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
