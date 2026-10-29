package api

import "net/http"

func (s *Server) setFallback(w http.ResponseWriter, r *http.Request) {
	if !s.displayAvailable() {
		s.writeError(w, requestID(r), &apiError{Code: "NOT_AVAILABLE", Message: "display is disabled", Status: http.StatusServiceUnavailable})
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	if len(body) != 0 {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_REQUEST", Message: "fallback request must have no body", Status: http.StatusBadRequest})
		return
	}
	state, storeErr := s.store.SetFallback(r.Context(), r.Method == http.MethodPost, s.now().UTC())
	if storeErr != nil {
		s.writeStoreError(w, r, storeErr)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"fallback": state})
}
