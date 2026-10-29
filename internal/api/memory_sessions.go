package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
)

func (s *Server) stopMemory(w http.ResponseWriter, r *http.Request) {
	memoryID := r.PathValue("memorySessionId")
	body, bodyErr := readBody(w, r)
	if bodyErr != nil {
		s.writeError(w, requestID(r), bodyErr)
		return
	}
	if len(body) != 0 {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_BODY", Message: "request body must be empty", Status: http.StatusBadRequest})
		return
	}
	if err := s.authenticateMemory(r, memoryID); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	scope := "stop:memory:" + memoryID
	if s.replayIdempotent(w, r, scope, nil) {
		return
	}
	now := s.now().UTC()
	memory, err := s.store.StopMemory(r.Context(), memoryID, now)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	response, err := json.Marshal(map[string]any{
		"status": "ended", "stoppedAt": memory.StoppedAt,
		"expiresAt": memory.ExpiresAt, "deleteAfter": memory.DeleteAfter,
	})
	if err != nil {
		s.writeError(w, requestID(r), internalError())
		return
	}
	response = append(response, '\n')
	token, _ := bearerToken(r)
	if err := s.store.SaveMemoryIdempotency(r.Context(), memoryID, hash(scope),
		hash(r.Header.Get("Idempotency-Key")+"\x00"+token), hashBytes(nil), http.StatusOK, response, now.Add(24*time.Hour)); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(response)
}

func (s *Server) liveState(w http.ResponseWriter, r *http.Request) {
	token, authErr := bearerToken(r)
	if authErr != nil {
		s.writeError(w, requestID(r), authErr)
		return
	}
	live, err := s.store.LiveSessionByToken(r.Context(), r.PathValue("liveSessionId"), hash(token), s.now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		s.writeError(w, requestID(r), &apiError{Code: "UNAUTHORIZED", Message: "invalid participant credentials", Status: http.StatusUnauthorized})
		return
	}
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"sessionId": live.ID, "trackingStatus": live.TrackingStatus,
		"expiresAt": live.ExpiresAt, "deleteAfter": live.DeleteAfter,
	})
}
