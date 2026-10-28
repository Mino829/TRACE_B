package api

import "net/http"

func (s *Server) operatorStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeIssuer(w, r) {
		return
	}
	now := s.now().UTC()
	status, err := s.store.ReadOperatorStatus(r.Context(), now)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"observedAt":       now,
		"registrationOpen": s.cfg.RegistrationOpen && !status.RegistrationClosed,
		"arrivalQueue":     map[string]int{"queued": status.Queued, "playing": status.Playing},
		"fallback":         status.Fallback,
		"notification": map[string]bool{
			"enabled":         s.cfg.PushEnabled && s.push != nil && !status.NotificationsStopped,
			"operatorStopped": status.NotificationsStopped,
		},
		"display": map[string]any{
			"enabled":            s.displayAvailable(),
			"connections":        s.displayConnections.Load(),
			"primaryLeaseActive": status.PrimaryLeaseActive,
		},
	})
}
