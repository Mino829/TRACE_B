package api

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	Origin    string `json:"origin"`
}

func (s *Server) displayAvailable() bool {
	cfg := s.cfg
	return cfg.DisplayEnabled && cfg.DisplayGoogleClientID != "" && len(cfg.DisplayOperatorEmails) > 0 &&
		len(cfg.DisplayAllowedOrigins) > 0 && len(cfg.DisplayTokenKey) >= 32 &&
		cfg.DisplayTokenKey != cfg.EdgeSharedSecret && cfg.DisplayTokenKey != cfg.ArrivalIssuerKey &&
		cfg.DisplayTokenKey != cfg.ArrivalProofKey
}

func (s *Server) verifyDisplayToken(token, origin string, now time.Time) bool {
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
	return err == nil && len(nonce) >= 16 && claim.Purpose == "display.ws" && claim.Origin == origin &&
		claim.IssuedAt <= now.Unix() && claim.ExpiresAt > now.Unix() && claim.ExpiresAt > claim.IssuedAt &&
		claim.ExpiresAt-claim.IssuedAt <= int64(displayCredentialLifetime.Seconds())
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
	if len(r.Header.Values("Origin")) != 1 || !s.displayOriginAllowed(r.Header.Get("Origin")) ||
		r.URL.RawQuery != "" || r.URL.ForceQuery || len(r.Header.Values("Authorization")) != 0 {
		s.writeError(w, requestID(r), &apiError{Code: "FORBIDDEN", Message: "allowed display origin without URL credentials required", Status: 403})
		return
	}
	if wait := s.limiter.allow(s.now(), rateRule{key: "display-connect/ip/" + s.clientIP(r), rate: 1, burst: 10},
		rateRule{key: "display-connect/global", rate: 10, burst: 20},
		rateRule{key: "api/global", rate: 100, burst: 100}); wait > 0 {
		s.rateLimited(w, r, wait)
		return
	}
	conn, err := (&websocket.Upgrader{HandshakeTimeout: 5 * time.Second, CheckOrigin: func(r *http.Request) bool {
		return len(r.Header.Values("Origin")) == 1 && s.displayOriginAllowed(r.Header.Get("Origin"))
	}}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	if !s.authenticateDisplay(conn, r.Header.Get("Origin")) {
		return
	}
	s.displayConnections.Add(1)
	defer s.displayConnections.Add(-1)
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
				if event.Type == "system.fallback.changed" {
					state, err = s.store.DisplaySnapshot(ctx, s.now().UTC())
					if err != nil {
						return
					}
					cursor = state.Cursor
					if sendDisplay(conn, s.frame(strconv.FormatInt(cursor, 10), "display.snapshot", state)) != nil {
						return
					}
					break
				}
			}
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

// A browser cannot set Authorization or version headers in its WebSocket
// handshake. Authenticate its first text frame before any state or lease access.
func (s *Server) authenticateDisplay(conn *websocket.Conn, origin string) bool {
	conn.SetReadLimit(maxJSONBody)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, payload, err := conn.ReadMessage()
	if err != nil {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4401, "display authentication required"), time.Now().Add(time.Second))
		return false
	}
	var auth struct {
		SchemaVersion      int    `json:"schemaVersion"`
		Type               string `json:"type"`
		DisplayToken       string `json:"displayToken"`
		CoordinateVersion  string `json:"coordinateVersion"`
		CampusGraphVersion string `json:"campusGraphVersion"`
		ThreeDMapVersion   string `json:"threeDMapVersion"`
	}
	if kind != websocket.TextMessage || decodeBytes(payload, &auth) != nil || auth.Type != "renderer.authenticate" ||
		!s.verifyDisplayToken(auth.DisplayToken, origin, s.now().UTC()) {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4401, "valid display credential required"), time.Now().Add(time.Second))
		return false
	}
	if auth.SchemaVersion != 1 || auth.CoordinateVersion != s.cfg.CoordinateVersion ||
		auth.CampusGraphVersion != s.cfg.CampusGraphVersion || auth.ThreeDMapVersion != s.cfg.ThreeDMapVersion {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4409, "renderer bundle version mismatch"), time.Now().Add(time.Second))
		return false
	}
	return true
}
func sendDisplay(conn *websocket.Conn, frame displayFrame) error {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteJSON(frame)
}
