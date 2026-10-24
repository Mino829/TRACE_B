package api

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/gorilla/websocket"
)

const displayCredentialLifetime = 2 * time.Minute
const displayConnectionLifetime = 50 * time.Minute // Below Cloud Run's 60-minute request ceiling.

type displayCredential struct {
	Purpose   string `json:"purpose"`
	IssuedAt  int64  `json:"issuedAt"`
	ExpiresAt int64  `json:"expiresAt"`
	Nonce     string `json:"nonce"`
}

func (s *Server) displayAvailable() bool {
	cfg := s.cfg
	return cfg.DisplayEnabled && len(cfg.DisplayIssuerKey) >= 32 && len(cfg.DisplayTokenKey) >= 32 &&
		cfg.DisplayIssuerKey != cfg.DisplayTokenKey && cfg.DisplayIssuerKey != cfg.EdgeSharedSecret &&
		cfg.DisplayTokenKey != cfg.EdgeSharedSecret && cfg.DisplayIssuerKey != cfg.ArrivalIssuerKey &&
		cfg.DisplayIssuerKey != cfg.ArrivalProofKey && cfg.DisplayTokenKey != cfg.ArrivalIssuerKey &&
		cfg.DisplayTokenKey != cfg.ArrivalProofKey
}

// Only a private operator-controlled caller with the independent issuer key
// may mint this short-lived credential. Neither key is sent to renderers.
func (s *Server) issueDisplayToken(w http.ResponseWriter, r *http.Request) {
	if !s.displayAvailable() {
		s.writeError(w, requestID(r), &apiError{Code: "NOT_AVAILABLE", Message: "display is disabled", Status: 503})
		return
	}
	if r.Header.Get("Origin") != "" || r.Header.Get("Authorization") != "" || len(r.Header.Values("X-Trace-Display-Issuer-Timestamp")) != 1 || len(r.Header.Values("X-Trace-Display-Issuer-Signature")) != 1 {
		s.writeError(w, requestID(r), &apiError{Code: "FORBIDDEN", Message: "display issuer credentials required", Status: 403})
		return
	}
	timestamp := r.Header.Get("X-Trace-Display-Issuer-Timestamp")
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	now := s.now().UTC()
	if err != nil || seconds < now.Add(-15*time.Second).Unix() || seconds > now.Add(15*time.Second).Unix() {
		s.writeError(w, requestID(r), &apiError{Code: "FORBIDDEN", Message: "display issuer credentials required", Status: 403})
		return
	}
	sig, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Trace-Display-Issuer-Signature"))
	if err != nil || subtle.ConstantTimeCompare(sig, mac(s.cfg.DisplayIssuerKey, "POST\n/v1/operator/display-tokens\n"+timestamp)) != 1 {
		s.writeError(w, requestID(r), &apiError{Code: "FORBIDDEN", Message: "display issuer credentials required", Status: 403})
		return
	}
	if wait := s.limiter.allow(now, rateRule{key: "display-issuer/global", rate: 1, burst: 10},
		rateRule{key: "api/global", rate: 100, burst: 100}); wait > 0 {
		s.rateLimited(w, r, wait)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(body) > 0 {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_REQUEST", Message: "request body must be empty", Status: 400})
		return
	}
	claim := displayCredential{Purpose: "display.ws", IssuedAt: now.Unix(), ExpiresAt: now.Add(displayCredentialLifetime).Unix(), Nonce: randomSecret()}
	payload, err := json.Marshal(claim)
	if err != nil {
		s.writeError(w, requestID(r), internalError())
		return
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	s.writeJSON(w, 200, map[string]any{"displayToken": encoded + "." + base64.RawURLEncoding.EncodeToString(mac(s.cfg.DisplayTokenKey, encoded)), "expiresAt": time.Unix(claim.ExpiresAt, 0).UTC()})
}
func (s *Server) verifyDisplayToken(token string, now time.Time) bool {
	if len(token) > 1024 {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || subtle.ConstantTimeCompare(sig, mac(s.cfg.DisplayTokenKey, parts[0])) != 1 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var claim displayCredential
	if json.Unmarshal(payload, &claim) != nil {
		return false
	}
	nonce, err := base64.RawURLEncoding.DecodeString(claim.Nonce)
	return err == nil && len(nonce) >= 16 && claim.Purpose == "display.ws" && claim.IssuedAt <= now.Unix() && claim.ExpiresAt > now.Unix() && claim.ExpiresAt > claim.IssuedAt && claim.ExpiresAt-claim.IssuedAt <= int64(displayCredentialLifetime.Seconds())
}

type displayMessage struct {
	SchemaVersion      int     `json:"schemaVersion"`
	Type               string  `json:"type"`
	EventID            string  `json:"eventId"`
	ArrivalID          string  `json:"arrivalId"`
	LeaseID            string  `json:"leaseId"`
	CoordinateVersion  string  `json:"coordinateVersion"`
	CampusGraphVersion string  `json:"campusGraphVersion"`
	ThreeDMapVersion   string  `json:"threeDMapVersion"`
	FPS                float64 `json:"fps"`
	Mode               string  `json:"mode"`
}
type displayFrame struct {
	SchemaVersion      int       `json:"schemaVersion"`
	CoordinateVersion  string    `json:"coordinateVersion"`
	CampusGraphVersion string    `json:"campusGraphVersion"`
	ThreeDMapVersion   string    `json:"threeDMapVersion"`
	EventID            string    `json:"eventId"`
	Type               string    `json:"type"`
	OccurredAt         time.Time `json:"occurredAt"`
	Data               any       `json:"data"`
}

func (s *Server) frame(id, kind string, data any) displayFrame {
	return displayFrame{1, s.cfg.CoordinateVersion, s.cfg.CampusGraphVersion, s.cfg.ThreeDMapVersion, id, kind, s.now().UTC(), data}
}

func (s *Server) displayWS(w http.ResponseWriter, r *http.Request) {
	if !s.displayAvailable() {
		s.writeError(w, requestID(r), &apiError{Code: "NOT_AVAILABLE", Message: "display is disabled", Status: 503})
		return
	}
	if r.Header.Get("Origin") != "" || r.URL.RawQuery != "" {
		s.writeError(w, requestID(r), &apiError{Code: "FORBIDDEN", Message: "native renderer connection required", Status: 403})
		return
	}
	if len(r.Header.Values("Authorization")) != 1 {
		s.writeError(w, requestID(r), &apiError{Code: "UNAUTHORIZED", Message: "one display credential required", Status: 401})
		return
	}
	token, authErr := bearerToken(r)
	if authErr != nil || !s.verifyDisplayToken(token, s.now().UTC()) {
		s.writeError(w, requestID(r), &apiError{Code: "UNAUTHORIZED", Message: "valid display credential required", Status: 401})
		return
	}
	if len(r.Header.Values("X-Trace-Schema-Version")) != 1 ||
		len(r.Header.Values("X-Trace-Coordinate-Version")) != 1 ||
		len(r.Header.Values("X-Trace-Campus-Graph-Version")) != 1 ||
		len(r.Header.Values("X-Trace-Three-D-Map-Version")) != 1 ||
		r.Header.Get("X-Trace-Schema-Version") != "1" || r.Header.Get("X-Trace-Coordinate-Version") != s.cfg.CoordinateVersion || r.Header.Get("X-Trace-Campus-Graph-Version") != s.cfg.CampusGraphVersion || r.Header.Get("X-Trace-Three-D-Map-Version") != s.cfg.ThreeDMapVersion {
		s.writeError(w, requestID(r), &apiError{Code: "VERSION_MISMATCH", Message: "renderer bundle version mismatch", Status: 409})
		return
	}
	conn, err := (&websocket.Upgrader{HandshakeTimeout: 5 * time.Second, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	// No HTTP request deadline after upgrade. Read/write and total connection
	// lifetimes below bound the hijacked connection independently.
	ctx, cancel := context.WithTimeout(r.Context(), displayConnectionLifetime)
	defer cancel()
	holder := randomID("renderer")
	lease := ""
	defer func() {
		releaseCtx, end := context.WithTimeout(context.Background(), 5*time.Second)
		defer end()
		if lease != "" {
			_ = s.store.ReleaseDisplayLease(releaseCtx, holder, lease, s.now().UTC())
		}
	}()
	if err := s.store.RefreshDisplay(ctx, s.now().UTC()); err != nil {
		return
	}
	state, err := s.store.DisplaySnapshot(ctx, s.now().UTC())
	if err != nil {
		return
	}
	cursor := state.Cursor
	if err := sendDisplay(conn, s.frame(strconv.FormatInt(cursor, 10), "display.snapshot", state)); err != nil {
		return
	}
	// Position deltas are per-connection and never replayed from PostgreSQL.
	observed := make(map[string]store.DisplayParticipant, len(state.Participants))
	for _, p := range state.Participants {
		observed[p.LivePresenceID] = p
	}
	messages := make(chan displayMessage, 16)
	readErr := make(chan error, 1)
	conn.SetReadLimit(256 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(30 * time.Second)) })
	go func() {
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			var msg displayMessage
			if err := decodeBytes(payload, &msg); err != nil {
				readErr <- errors.New("invalid display message")
				return
			}
			select {
			case messages <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	count := 0
	window := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-readErr:
			return
		case <-heartbeat.C:
			if lease != "" {
				ok, err := s.store.RenewDisplayLease(ctx, holder, lease, s.now().UTC())
				if err != nil {
					return
				}
				if !ok {
					lease = ""
					if sendDisplay(conn, s.frame(randomID("control"), "renderer.primary.lost", map[string]string{})) != nil {
						return
					}
				}
			}
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if conn.WriteMessage(websocket.PingMessage, nil) != nil {
				return
			}
		case <-poll.C:
			if err := s.store.RefreshDisplay(ctx, s.now().UTC()); err != nil {
				return
			}
			events, err := s.store.DisplayEvents(ctx, cursor, 128)
			if errors.Is(err, store.ErrDisplayResync) {
				state, err = s.store.DisplaySnapshot(ctx, s.now().UTC())
				if err != nil {
					return
				}
				cursor = state.Cursor
				if sendDisplay(conn, s.frame(strconv.FormatInt(cursor, 10), "display.snapshot", state)) != nil {
					return
				}
				observed = make(map[string]store.DisplayParticipant, len(state.Participants))
				for _, p := range state.Participants {
					observed[p.LivePresenceID] = p
				}
				continue
			}
			if err != nil {
				return
			}
			for _, event := range events {
				frame := s.frame(strconv.FormatInt(event.ID, 10), event.Type, event.Data)
				frame.OccurredAt = event.OccurredAt
				if sendDisplay(conn, frame) != nil {
					return
				}
				cursor = event.ID
			}
			positions := s.store.DisplayPositions(s.now().UTC())
			for id, p := range positions {
				if prior, found := observed[id]; !found || prior != p {
					if sendDisplay(conn, s.frame(randomID("position"), "position.updated", p)) != nil {
						return
					}
				}
			}
			for id, p := range observed {
				if _, found := positions[id]; !found {
					kind := "tracking.stopped"
					if !s.now().UTC().Before(p.ExpiresAt) {
						kind = "position.expired"
					}
					if sendDisplay(conn, s.frame(randomID("position"), kind, map[string]string{"livePresenceId": id})) != nil {
						return
					}
				}
			}
			observed = positions
		case msg := <-messages:
			if time.Since(window) >= time.Second {
				window = time.Now()
				count = 0
			}
			count++
			if count > 10 {
				return
			}
			if msg.SchemaVersion != 1 || msg.CoordinateVersion != s.cfg.CoordinateVersion || msg.CampusGraphVersion != s.cfg.CampusGraphVersion || msg.ThreeDMapVersion != s.cfg.ThreeDMapVersion {
				_ = sendDisplay(conn, s.frame(randomID("control"), "renderer.version.rejected", map[string]string{}))
				return
			}
			switch msg.Type {
			case "renderer.primary.acquire":
				if lease != "" {
					if sendDisplay(conn, s.frame(randomID("control"), "renderer.primary.granted", map[string]string{"leaseId": lease})) != nil {
						return
					}
					continue
				}
				id := randomID("lease")
				ok, err := s.store.AcquireDisplayLease(ctx, holder, id, s.now().UTC())
				if err != nil {
					return
				}
				if ok {
					lease = id
					if sendDisplay(conn, s.frame(randomID("control"), "renderer.primary.granted", map[string]string{"leaseId": lease})) != nil {
						return
					}
				}
				if !ok {
					if sendDisplay(conn, s.frame(randomID("control"), "renderer.primary.conflict", map[string]string{})) != nil {
						return
					}
				}
			case "renderer.heartbeat":
				_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
				if msg.LeaseID != "" {
					if lease == "" || msg.LeaseID != lease {
						return
					}
					ok, err := s.store.RenewDisplayLease(ctx, holder, lease, s.now().UTC())
					if err != nil {
						return
					}
					if !ok {
						lease = ""
						if sendDisplay(conn, s.frame(randomID("control"), "renderer.primary.lost", map[string]string{})) != nil {
							return
						}
					}
				}
			case "arrival.replay.started", "arrival.replay.completed", "arrival.replay.failed":
				if lease == "" || msg.LeaseID != lease || len(msg.EventID) < 8 || len(msg.EventID) > 128 || len(msg.ArrivalID) < 8 || len(msg.ArrivalID) > 128 {
					if sendDisplay(conn, s.frame(randomID("control"), "renderer.rejected", map[string]string{"eventId": msg.EventID, "code": "LEASE_OR_EVENT_INVALID"})) != nil {
						return
					}
					continue
				}
				status, err := s.store.DisplayReplay(ctx, holder, lease, msg.EventID, msg.ArrivalID, msg.Type, s.now().UTC())
				if errors.Is(err, store.ErrConflict) {
					if sendDisplay(conn, s.frame(randomID("control"), "renderer.rejected", map[string]string{"eventId": msg.EventID, "code": "STATE_CONFLICT"})) != nil {
						return
					}
					continue
				}
				if err != nil {
					return
				}
				if sendDisplay(conn, s.frame(randomID("control"), "renderer.ack", map[string]string{"eventId": msg.EventID, "replayStatus": status})) != nil {
					return
				}
			default:
				return
			}
		}
	}
}
func sendDisplay(conn *websocket.Conn, frame displayFrame) error {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteJSON(frame)
}
