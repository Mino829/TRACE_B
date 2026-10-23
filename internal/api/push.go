package api

import (
	"encoding/json"
	"net/http"

	"github.com/Mino829/trace_Backend/internal/push"
	"github.com/Mino829/trace_Backend/internal/store"
)

type putPushRequest struct {
	Consent      bool `json:"consent"`
	Subscription struct {
		Endpoint string    `json:"endpoint"`
		Keys     push.Keys `json:"keys"`
	} `json:"subscription"`
	IntervalMinutes int `json:"intervalMinutes"`
}
type patchPushRequest struct {
	IntervalMinutes *int    `json:"intervalMinutes"`
	Status          *string `json:"status"`
}

func validInterval(n int) bool { return n == 15 || n == 30 }

func (s *Server) pushAvailable(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.PushEnabled && s.push != nil {
		return true
	}
	s.writeError(w, requestID(r), &apiError{Code: "PUSH_UNAVAILABLE", Message: "notifications are unavailable", Status: http.StatusServiceUnavailable})
	return false
}
func (s *Server) putPush(w http.ResponseWriter, r *http.Request) {
	if !s.pushAvailable(w, r) {
		return
	}
	if err := s.authenticateMemory(r, r.PathValue("memorySessionId")); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	var input putPushRequest
	if err := decodeJSON(w, r, &input); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	if !input.Consent || !validInterval(input.IntervalMinutes) || !push.AllowedEndpoint(input.Subscription.Endpoint) ||
		push.DecodeKeys(input.Subscription.Keys.P256DH, input.Subscription.Keys.Auth) != nil {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_SUBSCRIPTION", Message: "valid consent, subscription and interval are required", Status: http.StatusBadRequest})
		return
	}
	id := r.PathValue("memorySessionId")
	endpoint, err := s.push.Seal(id, []byte(input.Subscription.Endpoint))
	if err != nil {
		s.writeError(w, requestID(r), internalError())
		return
	}
	keysJSON, err := json.Marshal(input.Subscription.Keys)
	if err != nil {
		s.writeError(w, requestID(r), internalError())
		return
	}
	keys, err := s.push.Seal(id, keysJSON)
	if err != nil {
		s.writeError(w, requestID(r), internalError())
		return
	}
	p, err := s.store.PutPush(r.Context(), store.PushSubscription{MemorySessionID: id, EndpointEncrypted: endpoint, KeysEncrypted: keys, IntervalMinutes: input.IntervalMinutes}, s.now().UTC())
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, pushResponse(p))
}
func (s *Server) patchPush(w http.ResponseWriter, r *http.Request) {
	if !s.pushAvailable(w, r) {
		return
	}
	id := r.PathValue("memorySessionId")
	if err := s.authenticateMemory(r, id); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	var input patchPushRequest
	if err := decodeJSON(w, r, &input); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	if (input.IntervalMinutes == nil && input.Status == nil) ||
		(input.IntervalMinutes != nil && !validInterval(*input.IntervalMinutes)) ||
		(input.Status != nil && *input.Status != "active" && *input.Status != "paused") {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_SUBSCRIPTION", Message: "allowed interval or status is required", Status: http.StatusBadRequest})
		return
	}
	p, err := s.store.PatchPush(r.Context(), id, input.IntervalMinutes, input.Status, s.now().UTC())
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, pushResponse(p))
}
func (s *Server) deletePush(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("memorySessionId")
	if err := s.authenticateMemory(r, id); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	if !s.emptyPhotoBody(w, r) {
		return
	}
	if err := s.store.DeletePush(r.Context(), id); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func pushResponse(p store.PushSubscription) map[string]any {
	result := map[string]any{"intervalMinutes": p.IntervalMinutes, "status": p.Status, "expiresAt": p.ExpiresAt}
	if p.Status == "active" {
		result["nextSendAt"] = p.NextSendAt
	} else {
		result["nextSendAt"] = nil
	}
	return result
}
