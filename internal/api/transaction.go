package api

import (
	"bytes"
	"errors"
	"io"
	"net/http"

	"github.com/Mino829/trace_Backend/internal/store"
)

// Buffer the response until COMMIT so a failed transaction never returns success.
func (s *Server) atomic(handle func(*Server, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Read the bounded body before taking database locks (slow clients must not hold them).
		body, err := readBody(w, r)
		if err != nil {
			s.writeError(w, requestID(r), err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if r.Method == http.MethodDelete && len(bytes.TrimSpace(body)) != 0 {
			s.writeError(w, requestID(r), &apiError{Code: "INVALID_BODY", Message: "delete requests must not contain a body", Status: http.StatusBadRequest})
			return
		}
		if _, err := bearerToken(r); err != nil {
			s.writeError(w, requestID(r), err)
			return
		}
		// Arrival, replay, Memory deletion/stop and legacy Live retirement all
		// take the display lock before their per-session mutation locks.
		key := "display:lease"
		response := &bufferedResponse{header: make(http.Header)}
		rejected := errors.New("request rejected")
		txErr := s.store.Transaction(r.Context(), key, func(db *store.Store) error {
			server := *s
			server.store = db
			handle(&server, response, r)
			if response.status >= 400 {
				return rejected
			}
			return nil
		})
		if txErr != nil && !errors.Is(txErr, rejected) {
			s.writeError(w, requestID(r), internalError())
			return
		}
		for name, values := range response.header {
			w.Header()[name] = values
		}
		if response.status == 0 {
			response.status = http.StatusNoContent
		}
		w.WriteHeader(response.status)
		_, _ = w.Write(response.body.Bytes())
	}
}

type bufferedResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *bufferedResponse) Header() http.Header { return w.header }
func (w *bufferedResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *bufferedResponse) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.body.Write(body)
}
