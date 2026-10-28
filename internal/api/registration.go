package api

import "net/http"

func (s *Server) closeRegistration(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeIssuer(w, r) {
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	if len(body) != 0 {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_REQUEST", Message: "registration close request must have no body", Status: http.StatusBadRequest})
		return
	}
	if err := s.store.CloseRegistration(r.Context(), s.now().UTC()); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"registrationOpen": false})
}
