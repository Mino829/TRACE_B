package api

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
)

// Marker controls use the display issuer's path-bound HMAC, never a renderer
// token or participant credential. A withdrawn policy cannot block safe stops.
func (s *Server) authorizeMarkerOperator(w http.ResponseWriter, r *http.Request, requirePolicy bool) (time.Time, bool) {
	if !s.displayIssuerConfigured() ||
		(requirePolicy && (!s.cfg.PhotoEnabled || !s.cfg.DisplayEnabled || !s.cfg.PhotoMarkersApproved)) {
		s.writeError(w, requestID(r), &apiError{Code: "FORBIDDEN", Message: "photo markers are unavailable", Status: http.StatusForbidden})
		return time.Time{}, false
	}
	now, ok := s.authorizeDisplayIssuer(w, r)
	if !ok {
		return time.Time{}, false
	}
	if r.URL.RawQuery != "" {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_REQUEST", Message: "query parameters are not supported", Status: http.StatusBadRequest})
		return time.Time{}, false
	}
	return now, true
}

func (s *Server) markerEmptyBody(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(body) != 0 {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_REQUEST", Message: "request body must be empty", Status: http.StatusBadRequest})
		return false
	}
	return true
}

func (s *Server) markerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrMarkersDisabled):
		s.writeError(w, requestID(r), &apiError{Code: "FORBIDDEN", Message: "photo markers are unavailable", Status: http.StatusForbidden})
	case errors.Is(err, store.ErrConflict):
		s.writeError(w, requestID(r), &apiError{Code: "VERSION_MISMATCH", Message: "photo marker policy mismatch", Status: http.StatusConflict})
	default:
		s.writeStoreError(w, r, err)
	}
}

func (s *Server) operatorMarkerStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeMarkerOperator(w, r, true); !ok || !s.markerEmptyBody(w, r) {
		return
	}
	s.markerStatus(w, r)
}

func (s *Server) markerStatus(w http.ResponseWriter, r *http.Request) {
	controls, err := s.store.MarkerStatus(r.Context())
	if err != nil {
		s.markerError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, controls)
}

func (s *Server) operatorMarkerPins(w http.ResponseWriter, r *http.Request) {
	s.markerSwitch(w, r, true)
}

func (s *Server) operatorMarkerHotspots(w http.ResponseWriter, r *http.Request) {
	s.markerSwitch(w, r, false)
}

func (s *Server) markerSwitch(w http.ResponseWriter, r *http.Request, pins bool) {
	action := r.PathValue("action")
	now, ok := s.authorizeMarkerOperator(w, r, action != "stop")
	if !ok {
		return
	}
	if action != "start" && action != "stop" {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_REQUEST", Message: "action must be start or stop", Status: http.StatusBadRequest})
		return
	}
	if !s.markerEmptyBody(w, r) {
		return
	}
	var err error
	if pins {
		err = s.store.SetMarkerPinsEnabled(r.Context(), action == "start", now)
	} else {
		err = s.store.SetMarkerHotspotsEnabled(r.Context(), action == "start", now)
	}
	if err != nil {
		s.markerError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) operatorMarkerSparse(w http.ResponseWriter, r *http.Request) {
	now, ok := s.authorizeMarkerOperator(w, r, false)
	if !ok {
		return
	}
	var input struct {
		Sparse *bool `json:"sparse"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	if input.Sparse == nil {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_REQUEST", Message: "sparse must be a boolean", Status: http.StatusBadRequest})
		return
	}
	if !*input.Sparse && (!s.cfg.PhotoEnabled || !s.cfg.DisplayEnabled || !s.cfg.PhotoMarkersApproved) {
		s.writeError(w, requestID(r), &apiError{Code: "FORBIDDEN", Message: "photo markers are unavailable", Status: http.StatusForbidden})
		return
	}
	if err := s.store.SetMarkerSparseOverride(r.Context(), *input.Sparse, now); err != nil {
		s.markerError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
