package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/photo"
	"github.com/Mino829/trace_Backend/internal/push"
	"github.com/Mino829/trace_Backend/internal/store"
)

const maxJSONBody = 16 << 10

type Server struct {
	cfg     config.Config
	store   *store.Store
	limiter *rateLimiter
	now     func() time.Time
	photos  *photo.Service
	push    *push.Service
}

type apiError struct {
	Code    string
	Message string
	Status  int
}

func (e *apiError) Error() string { return e.Code }

func New(cfg config.Config, db *store.Store, photos *photo.Service, sender ...*push.Service) http.Handler {
	server := &Server{cfg: cfg, store: db, now: time.Now, limiter: newRateLimiter(), photos: photos}
	if len(sender) != 0 {
		server.push = sender[0]
	}
	if cfg.DisplayEnabled && db != nil {
		db.EnableDisplay()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", server.live)
	mux.HandleFunc("GET /health/ready", server.ready)
	mux.HandleFunc("GET /v1/public/config", server.limitedPublic("config", server.publicConfig))
	mux.HandleFunc("POST /v1/experiences", server.limitedPublic("create", server.createExperience))
	mux.HandleFunc("POST /v1/operator/arrival-proofs", server.issueArrivalProof)
	mux.HandleFunc("GET /v1/operator/arrivals", server.operatorArrivals)
	mux.HandleFunc("POST /v1/operator/arrivals/{arrivalId}/skip", server.operatorSkipArrival)
	mux.HandleFunc("POST /v1/operator/display-tokens", server.issueDisplayToken)
	mux.HandleFunc("GET /v1/operator/photo-markers/status", server.operatorMarkerStatus)
	mux.HandleFunc("POST /v1/operator/photo-markers/{action}", server.operatorMarkerPins)
	mux.HandleFunc("POST /v1/operator/photo-hotspots/{action}", server.operatorMarkerHotspots)
	mux.HandleFunc("POST /v1/operator/photo-markers/sparse", server.operatorMarkerSparse)
	mux.HandleFunc("POST /v1/operator/arrivals/{arrivalId}/retry", server.operatorRetryArrival)
	mux.HandleFunc("GET /v1/display/ws", server.displayWS)
	mux.HandleFunc("GET /v1/experiences/{experienceId}", server.limitedSession("session", server.experienceState))
	mux.HandleFunc("DELETE /v1/experiences/{experienceId}", server.limitedSession("experience-delete", server.deleteExperience))
	mux.HandleFunc("POST /v1/live-presence-sessions/{liveSessionId}/position", server.limitedSession("position", server.atomic((*Server).updatePosition)))
	mux.HandleFunc("POST /v1/live-presence-sessions/{liveSessionId}/on-site", server.limitedSession("session", server.atomic((*Server).onSite)))
	mux.HandleFunc("POST /v1/live-presence-sessions/{liveSessionId}/stop", server.limitedSession("session", server.atomic((*Server).stopLive)))
	mux.HandleFunc("POST /v1/memory-sessions/{memorySessionId}/arrival", server.limitedSession("session", server.arrive))
	mux.HandleFunc("POST /v1/memory-sessions/{memorySessionId}/arrival/skip", server.limitedSession("session", server.skipOwnArrival))
	mux.HandleFunc("DELETE /v1/memory-sessions/{memorySessionId}", server.limitedSession("session", server.prepareMemoryPhotoDeletion(server.atomic((*Server).deleteMemory))))
	mux.HandleFunc("DELETE /v1/live-presence-sessions/{liveSessionId}", server.limitedSession("session", server.atomic((*Server).deleteLive)))
	mux.HandleFunc("GET /v1/memory-sessions/{memorySessionId}/photos", server.limitedSession("session", server.listPhotos))
	mux.HandleFunc("POST /v1/memory-sessions/{memorySessionId}/photos", server.limitedSession("photo-metadata", server.createPhoto))
	mux.HandleFunc("POST /v1/memory-sessions/{memorySessionId}/photos/{photoMomentId}/upload-url", server.limitedSession("session", server.renewPhotoUpload))
	mux.HandleFunc("POST /v1/memory-sessions/{memorySessionId}/photos/{photoMomentId}/complete", server.limitedSession("session", server.completePhoto))
	mux.HandleFunc("DELETE /v1/memory-sessions/{memorySessionId}/photos/{photoMomentId}", server.limitedSession("session", server.deletePhoto))
	mux.HandleFunc("GET /v1/memory-sessions/{memorySessionId}/memory", server.limitedSession("session", server.photoMemory))
	mux.HandleFunc("PUT /v1/memory-sessions/{memorySessionId}/push-subscription", server.limitedSession("push", server.putPush))
	mux.HandleFunc("PATCH /v1/memory-sessions/{memorySessionId}/push-subscription", server.limitedSession("push", server.patchPush))
	mux.HandleFunc("DELETE /v1/memory-sessions/{memorySessionId}/push-subscription", server.limitedSession("push", server.deletePush))
	return server.middleware(mux)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := randomID("req")
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasPrefix(r.URL.Path, "/v1/") && s.cfg.EdgeSharedSecret != "" &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Trace-Edge-Secret")), []byte(s.cfg.EdgeSharedSecret)) != 1 {
			s.writeError(w, requestID, &apiError{Code: "EDGE_ACCESS_DENIED", Message: "access denied", Status: http.StatusForbidden})
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Trace-Live-Token")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Expose-Headers", "Retry-After")
		}
		if r.Method == http.MethodOptions {
			if origin := r.Header.Get("Origin"); origin != "" && !s.originAllowed(origin) {
				s.writeError(w, requestID, &apiError{Code: "ORIGIN_NOT_ALLOWED", Message: "origin is not allowed", Status: http.StatusForbidden})
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("request panic", "requestId", requestID, "error", recovered)
				s.writeError(w, requestID, &apiError{Code: "INTERNAL_ERROR", Message: "internal server error", Status: http.StatusInternalServerError})
			}
		}()
		if r.URL.Path != "/v1/display/ws" {
			controller := http.NewResponseController(w)
			_ = controller.SetReadDeadline(time.Now().Add(10 * time.Second))
			_ = controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
		}
		ctx := r.Context()
		cancel := func() {}
		if r.URL.Path != "/v1/display/ws" {
			ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		}
		defer cancel()
		if strings.HasPrefix(r.URL.Path, "/v1/") && s.store.Ping(ctx) != nil {
			s.writeError(w, requestID, &apiError{Code: "NOT_READY", Message: "required services are unavailable", Status: http.StatusServiceUnavailable})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, requestIDKey{}, requestID)))
	})
}

type requestIDKey struct{}

func (s *Server) originAllowed(origin string) bool {
	for _, allowed := range s.cfg.AllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

func (s *Server) live(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		s.writeError(w, requestID(r), &apiError{Code: "NOT_READY", Message: "required services are unavailable", Status: http.StatusServiceUnavailable})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) publicConfig(w http.ResponseWriter, _ *http.Request) {
	vapidPublicKey := ""
	if s.cfg.PushEnabled && s.push != nil {
		vapidPublicKey = s.cfg.PushVAPIDPublicKey
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"registrationOpen": s.cfg.RegistrationOpen,
		"consent": map[string]any{
			"version":  s.cfg.MemoryConsentVersion,
			"language": s.cfg.ConsentLanguage,
			"body":     s.cfg.ConsentBody,
		},
		"livePresenceConsentVersion":  s.cfg.LiveConsentVersion,
		"onSiteConsentVersion":        s.cfg.OnSiteConsentVersion,
		"positionIntervalSeconds":     s.cfg.PositionIntervalSeconds,
		"livePositionTtlSeconds":      int(s.cfg.LivePositionTTL.Seconds()),
		"photoLocationRetentionHours": int(s.cfg.DataRetention.Hours()),
		"spatial": map[string]string{
			"coordinateVersion":  s.cfg.CoordinateVersion,
			"campusGraphVersion": s.cfg.CampusGraphVersion,
			"threeDMapVersion":   s.cfg.ThreeDMapVersion,
		},
		"photo": map[string]any{
			"enabled":           s.cfg.PhotoEnabled && s.photos != nil,
			"maxCount":          12,
			"maxUploadBytes":    5 * 1024 * 1024,
			"maxLongEdgePixels": 1920,
			"publicDisplay":     false,
		},
		"notification": map[string]any{
			"enabled":                 s.cfg.PushEnabled && s.push != nil,
			"defaultIntervalMinutes":  30,
			"allowedIntervalsMinutes": []int{15, 30},
			"exactDeliveryGuaranteed": false,
			"vapidPublicKey":          vapidPublicKey,
		},
		"contact": s.cfg.Contact,
	})
}

type createExperienceRequest struct {
	MemoryConsentVersion       string    `json:"memoryConsentVersion"`
	LivePresenceConsentVersion string    `json:"livePresenceConsentVersion"`
	EnableLivePresence         bool      `json:"enableLivePresence"`
	ConsentLanguage            string    `json:"consentLanguage"`
	ClientTime                 time.Time `json:"clientTime"`
}

func (s *Server) createExperience(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.RegistrationOpen {
		s.writeError(w, requestID(r), &apiError{Code: "REGISTRATION_CLOSED", Message: "registration is closed", Status: http.StatusServiceUnavailable})
		return
	}
	var input createExperienceRequest
	if err := decodeJSON(w, r, &input); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	if input.MemoryConsentVersion != s.cfg.MemoryConsentVersion ||
		(input.EnableLivePresence && input.LivePresenceConsentVersion != s.cfg.LiveConsentVersion) {
		s.writeError(w, requestID(r), &apiError{Code: "CONSENT_VERSION_MISMATCH", Message: "consent version is no longer current", Status: http.StatusConflict})
		return
	}
	if input.ConsentLanguage != s.cfg.ConsentLanguage {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_CONSENT_LANGUAGE", Message: "consent language is not supported", Status: http.StatusBadRequest})
		return
	}
	now := s.now().UTC()
	if input.ClientTime.IsZero() || input.ClientTime.Before(now.Add(-24*time.Hour)) || input.ClientTime.After(now.Add(5*time.Minute)) {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_CLIENT_TIME", Message: "clientTime is outside the allowed range", Status: http.StatusBadRequest})
		return
	}
	memoryToken := randomSecret()
	liveToken := randomSecret()
	linkToken := randomSecret()
	experienceID := randomID("exp")
	memoryID := randomID("mem")
	liveID := randomID("live")
	memoryExpires := now.Add(s.cfg.MemorySessionTTL)
	memoryDeleteAfter := memoryExpires.Add(s.cfg.DataRetention)
	liveExpires := now.Add(s.cfg.LiveSessionTTL)
	liveDeleteAfter := liveExpires.Add(s.cfg.DataRetention)
	color, symbol := displayAlias(experienceID)
	value := store.NewExperience{
		ExperienceID: experienceID, MemorySessionID: memoryID, LiveSessionID: liveID, LinkID: randomID("link"),
		MemoryTokenHash: hash(memoryToken), LiveTokenHash: hash(liveToken), LinkTokenHash: hash(linkToken),
		MemoryConsent: input.MemoryConsentVersion, LiveConsent: input.LivePresenceConsentVersion,
		ConsentLanguage: input.ConsentLanguage, DisplayColor: color, DisplaySymbol: symbol,
		CreatedAt: now, MemoryExpiresAt: memoryExpires, MemoryDeleteAfter: memoryDeleteAfter,
		LiveExpiresAt: liveExpires, LiveDeleteAfter: liveDeleteAfter, LivePresence: input.EnableLivePresence,
	}
	if err := s.store.CreateExperience(r.Context(), value); err != nil {
		slog.Error("create experience", "requestId", requestID(r), "error", err)
		s.writeError(w, requestID(r), internalError())
		return
	}
	response := map[string]any{
		"experienceId": experienceID,
		"memorySession": map[string]any{
			"sessionId": memoryID, "participantToken": memoryToken, "expiresAt": memoryExpires, "deleteAfter": memoryDeleteAfter,
		},
		"displayAlias":            map[string]string{"color": color, "symbol": symbol},
		"positionIntervalSeconds": s.cfg.PositionIntervalSeconds,
	}
	if input.EnableLivePresence {
		response["experienceLinkToken"] = linkToken
		response["livePresenceSession"] = map[string]any{
			"sessionId": liveID, "participantToken": liveToken, "trackingStatus": "tracking", "presencePhase": "approaching",
			"expiresAt": liveExpires, "deleteAfter": liveDeleteAfter,
		}
	}
	s.writeJSON(w, http.StatusCreated, response)
}

func (s *Server) experienceState(w http.ResponseWriter, r *http.Request) {
	token, err := bearerToken(r)
	if err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	state, storeErr := s.store.ExperienceByMemoryToken(r.Context(), r.PathValue("experienceId"), hash(token), s.now().UTC())
	if storeErr != nil {
		s.writeStoreError(w, r, storeErr)
		return
	}
	summary, storeErr := s.store.PhotoSummary(r.Context(), state.MemorySessionID)
	if storeErr != nil {
		s.photoError(w, r, storeErr)
		return
	}
	response := map[string]any{
		"memorySession": map[string]any{
			"sessionId": state.MemorySessionID, "expiresAt": state.MemoryExpiresAt, "deleteAfter": state.MemoryDeleteAfter,
		},
		"displayAlias": map[string]string{"color": state.DisplayColor, "symbol": state.DisplaySymbol},
		"photoSummary": map[string]int{"ready": summary.Ready, "pending": summary.Pending, "failed": summary.Failed, "maxCount": 12},
		"replayStatus": state.ReplayStatus,
	}
	if state.LiveSessionID != "" {
		response["livePresenceSession"] = map[string]any{
			"sessionId": state.LiveSessionID, "trackingStatus": state.TrackingStatus, "presencePhase": state.PresencePhase,
			"expiresAt": state.LiveExpiresAt, "deleteAfter": state.LiveDeleteAfter,
		}
	}
	s.writeJSON(w, http.StatusOK, response)
}

type positionRequest struct {
	Point struct {
		Latitude   float64   `json:"latitude"`
		Longitude  float64   `json:"longitude"`
		AccuracyM  float64   `json:"accuracyM"`
		RecordedAt time.Time `json:"recordedAt"`
	} `json:"point"`
}

func (s *Server) updatePosition(w http.ResponseWriter, r *http.Request) {
	liveID := r.PathValue("liveSessionId")
	if err := s.authenticateLive(r, liveID); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	var input positionRequest
	if err := decodeJSON(w, r, &input); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	now := s.now().UTC()
	point := input.Point
	if point.Latitude < s.cfg.MinLatitude || point.Latitude > s.cfg.MaxLatitude ||
		point.Longitude < s.cfg.MinLongitude || point.Longitude > s.cfg.MaxLongitude {
		s.writeError(w, requestID(r), &apiError{Code: "OUTSIDE_GEOFENCE", Message: "position is outside the allowed area", Status: http.StatusUnprocessableEntity})
		return
	}
	if point.AccuracyM <= 0 || point.AccuracyM > s.cfg.MaxPositionAccuracyMeters {
		s.writeError(w, requestID(r), &apiError{Code: "LOW_ACCURACY", Message: "position accuracy is insufficient", Status: http.StatusUnprocessableEntity})
		return
	}
	if point.RecordedAt.IsZero() || point.RecordedAt.Before(now.Add(-2*time.Minute)) {
		s.writeError(w, requestID(r), &apiError{Code: "TOO_OLD", Message: "position is too old", Status: http.StatusUnprocessableEntity})
		return
	}
	if point.RecordedAt.After(now.Add(15 * time.Second)) {
		s.writeError(w, requestID(r), &apiError{Code: "FUTURE_TIME", Message: "position time is in the future", Status: http.StatusUnprocessableEntity})
		return
	}
	session, previous, storeErr := s.store.LiveSession(r.Context(), liveID)
	if storeErr != nil {
		s.writeStoreError(w, r, storeErr)
		return
	}
	if session.TrackingStatus != "tracking" || !now.Before(session.ExpiresAt) {
		s.writeError(w, requestID(r), &apiError{Code: "TRACKING_STOPPED", Message: "tracking is not active", Status: http.StatusConflict})
		return
	}
	if previous != nil && !point.RecordedAt.After(previous.RecordedAt) {
		s.writeError(w, requestID(r), &apiError{Code: "TOO_OLD", Message: "a newer position was already accepted", Status: http.StatusUnprocessableEntity})
		return
	}
	x, z := s.toLocal(point.Latitude, point.Longitude)
	x = quantize(x, s.cfg.PositionQuantizationMeters)
	z = quantize(z, s.cfg.PositionQuantizationMeters)
	if previous != nil {
		distance := math.Hypot(x-previous.X, z-previous.Z)
		seconds := point.RecordedAt.Sub(previous.RecordedAt).Seconds()
		allowed := 15*seconds + point.AccuracyM + previous.AccuracyM
		if distance > allowed {
			s.writeError(w, requestID(r), &apiError{Code: "IMPOSSIBLE_JUMP", Message: "position changed too quickly", Status: http.StatusUnprocessableEntity})
			return
		}
	}
	expires := now.Add(s.cfg.LivePositionTTL)
	if expires.After(session.ExpiresAt) {
		expires = session.ExpiresAt
	}
	if err := s.store.SavePosition(r.Context(), liveID, store.Position{
		X: x, Z: z, AccuracyM: point.AccuracyM, RecordedAt: point.RecordedAt.UTC(), ReceivedAt: now, ExpiresAt: expires,
	}); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "latestRecordedAt": point.RecordedAt.UTC(), "expiresAt": expires})
}

type onSiteRequest struct {
	Continue       bool   `json:"continue"`
	ConsentVersion string `json:"consentVersion"`
}

func (s *Server) onSite(w http.ResponseWriter, r *http.Request) {
	liveID := r.PathValue("liveSessionId")
	if err := s.authenticateLive(r, liveID); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	var input onSiteRequest
	if err := decodeJSON(w, r, &input); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	if input.Continue && input.ConsentVersion != s.cfg.OnSiteConsentVersion {
		s.writeError(w, requestID(r), &apiError{Code: "CONSENT_VERSION_MISMATCH", Message: "consent version is no longer current", Status: http.StatusConflict})
		return
	}
	if err := s.store.SetOnSite(r.Context(), liveID, input.ConsentVersion, input.Continue, s.now().UTC()); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	phase, tracking := "ended", "stopped"
	if input.Continue {
		phase, tracking = "on_site", "tracking"
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"trackingStatus": tracking, "presencePhase": phase, "displayGranularity": "area"})
}

type stopRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) stopLive(w http.ResponseWriter, r *http.Request) {
	liveID := r.PathValue("liveSessionId")
	body, apiErr := readBody(w, r)
	if apiErr != nil {
		s.writeError(w, requestID(r), apiErr)
		return
	}
	scope := "stop:" + liveID
	if replayed := s.replayIdempotent(w, r, scope, body); replayed {
		return
	}
	if err := s.authenticateLive(r, liveID); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	var input stopRequest
	if err := decodeBytes(body, &input); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	if input.Reason == "" || len(input.Reason) > 64 {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_REASON", Message: "reason is required", Status: http.StatusBadRequest})
		return
	}
	if err := s.store.StopLive(r.Context(), liveID, input.Reason, s.now().UTC()); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	response := map[string]string{"trackingStatus": "stopped", "presencePhase": "ended"}
	s.writeIdempotentJSON(w, r, scope, body, http.StatusOK, response)
}

// deleteExperience commits the live deletion before the memory deletion. A
// failed second half leaves a durable receipt for a retry with the same key.
func (s *Server) deleteExperience(w http.ResponseWriter, r *http.Request) {
	memoryToken, authErr := bearerToken(r)
	if authErr != nil {
		s.writeError(w, requestID(r), authErr)
		return
	}
	liveToken := r.Header.Get("X-Trace-Live-Token")
	key := r.Header.Get("Idempotency-Key")
	if liveToken == "" {
		s.writeError(w, requestID(r), &apiError{Code: "UNAUTHORIZED", Message: "live participant credentials are required", Status: http.StatusUnauthorized})
		return
	}
	if key == "" || len(key) > 200 {
		s.writeError(w, requestID(r), &apiError{Code: "IDEMPOTENCY_KEY_REQUIRED", Message: "a valid Idempotency-Key header is required", Status: http.StatusBadRequest})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(body) != 0 {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_REQUEST", Message: "request body must be empty", Status: http.StatusBadRequest})
		return
	}
	id := r.PathValue("experienceId")
	now := s.now().UTC()
	receiptKey := hash(key + "\x00" + memoryToken + "\x00" + liveToken)
	memoryHash, liveHash := hash(memoryToken), hash(liveToken)
	if err := s.store.DeleteExperiencePart(r.Context(), id, memoryHash, liveHash, receiptKey, false, now); err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	response := map[string]any{"memory": "pending", "live": "deleted", "complete": false}
	if s.photos != nil {
		state, stateErr := s.store.ExperienceByMemoryToken(r.Context(), id, memoryHash, now)
		if stateErr == nil {
			if err := s.photos.DeleteMemoryPhotos(r.Context(), state.MemorySessionID); err != nil {
				s.writeJSON(w, http.StatusOK, response)
				return
			}
		} else if !errors.Is(stateErr, store.ErrNotFound) {
			s.writeJSON(w, http.StatusOK, response)
			return
		}
	}
	if err := s.store.DeleteExperiencePart(r.Context(), id, memoryHash, liveHash, receiptKey, true, now); err == nil {
		response["memory"] = "deleted"
		response["complete"] = true
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) deleteMemory(w http.ResponseWriter, r *http.Request) {
	s.deleteSession(w, r, "memory", r.PathValue("memorySessionId"))
}

func (s *Server) deleteLive(w http.ResponseWriter, r *http.Request) {
	s.deleteSession(w, r, "live", r.PathValue("liveSessionId"))
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request, kind, id string) {
	scope := "delete:" + kind + ":" + id
	if replayed := s.replayIdempotent(w, r, scope, nil); replayed {
		return
	}
	var authErr *apiError
	if kind == "memory" {
		authErr = s.authenticateMemory(r, id)
	} else {
		authErr = s.authenticateLive(r, id)
	}
	if authErr != nil {
		s.writeError(w, requestID(r), authErr)
		return
	}
	var err error
	if kind == "memory" {
		err = s.store.DeleteMemory(r.Context(), id)
	} else {
		err = s.store.DeleteLive(r.Context(), id)
	}
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	if err := s.saveIdempotency(r, scope, nil, http.StatusNoContent, nil); err != nil {
		s.writeError(w, requestID(r), internalError())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authenticateMemory(r *http.Request, id string) *apiError {
	token, err := bearerToken(r)
	if err != nil {
		return err
	}
	if storeErr := s.store.AuthenticateMemory(r.Context(), id, hash(token), s.now().UTC()); storeErr != nil {
		if errors.Is(storeErr, store.ErrNotFound) {
			return &apiError{Code: "UNAUTHORIZED", Message: "invalid participant credentials", Status: http.StatusUnauthorized}
		}
		return internalError()
	}
	return nil
}

func (s *Server) authenticateLive(r *http.Request, id string) *apiError {
	token, err := bearerToken(r)
	if err != nil {
		return err
	}
	if storeErr := s.store.AuthenticateLive(r.Context(), id, hash(token), s.now().UTC()); storeErr != nil {
		if errors.Is(storeErr, store.ErrNotFound) {
			return &apiError{Code: "UNAUTHORIZED", Message: "invalid participant credentials", Status: http.StatusUnauthorized}
		}
		return internalError()
	}
	return nil
}

func (s *Server) toLocal(latitude, longitude float64) (float64, float64) {
	const earthRadiusM = 6371000.0
	latDelta := (latitude - s.cfg.OriginLatitude) * math.Pi / 180
	lngDelta := (longitude - s.cfg.OriginLongitude) * math.Pi / 180
	x := earthRadiusM * lngDelta * math.Cos(s.cfg.OriginLatitude*math.Pi/180)
	z := earthRadiusM * latDelta
	return x, z
}

func (s *Server) replayIdempotent(w http.ResponseWriter, r *http.Request, scope string, body []byte) bool {
	token, authErr := bearerToken(r)
	if authErr != nil {
		s.writeError(w, requestID(r), authErr)
		return true
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		s.writeError(w, requestID(r), &apiError{Code: "IDEMPOTENCY_KEY_REQUIRED", Message: "a valid Idempotency-Key header is required", Status: http.StatusBadRequest})
		return true
	}
	// Binding the receipt to credentials allows safe replay after the session was deleted.
	record, err := s.store.GetIdempotency(r.Context(), hash(scope), hash(key+"\x00"+token), s.now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		s.writeError(w, requestID(r), internalError())
		return true
	}
	if record.RequestHash != hashBytes(body) {
		s.writeError(w, requestID(r), &apiError{Code: "IDEMPOTENCY_CONFLICT", Message: "the idempotency key was already used with different content", Status: http.StatusConflict})
		return true
	}
	if len(record.ResponseBody) > 0 {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(record.ResponseStatus)
	_, _ = w.Write(record.ResponseBody)
	return true
}

func (s *Server) writeIdempotentJSON(w http.ResponseWriter, r *http.Request, scope string, requestBody []byte, status int, response any) {
	body, err := json.Marshal(response)
	if err != nil {
		s.writeError(w, requestID(r), internalError())
		return
	}
	body = append(body, '\n')
	if err := s.saveIdempotency(r, scope, requestBody, status, body); err != nil {
		s.writeError(w, requestID(r), internalError())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) saveIdempotency(r *http.Request, scope string, requestBody []byte, status int, responseBody []byte) error {
	if responseBody == nil {
		responseBody = []byte{}
	}
	token, err := bearerToken(r)
	if err != nil {
		return err
	}
	return s.store.SaveIdempotency(r.Context(), hash(scope), hash(r.Header.Get("Idempotency-Key")+"\x00"+token),
		hashBytes(requestBody), status, responseBody, s.now().UTC().Add(24*time.Hour))
}

func (s *Server) writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.writeError(w, requestID(r), &apiError{Code: "NOT_FOUND", Message: "resource was not found", Status: http.StatusNotFound})
	case errors.Is(err, store.ErrConflict):
		s.writeError(w, requestID(r), &apiError{Code: "STATE_CONFLICT", Message: "operation is not valid in the current state", Status: http.StatusConflict})
	default:
		slog.Error("database operation", "requestId", requestID(r), "error", err)
		s.writeError(w, requestID(r), internalError())
	}
}

func (s *Server) writeError(w http.ResponseWriter, requestID string, err *apiError) {
	s.writeJSON(w, err.Status, map[string]any{"error": map[string]string{
		"code": err.Code, "message": err.Message, "requestId": requestID,
	}})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) *apiError {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return &apiError{Code: "BODY_TOO_LARGE", Message: "request body is too large", Status: http.StatusRequestEntityTooLarge}
		}
		return &apiError{Code: "INVALID_JSON", Message: "request body is not valid JSON", Status: http.StatusBadRequest}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &apiError{Code: "INVALID_JSON", Message: "request body must contain one JSON object", Status: http.StatusBadRequest}
	}
	return nil
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, *apiError) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, &apiError{Code: "BODY_TOO_LARGE", Message: "request body is too large", Status: http.StatusRequestEntityTooLarge}
		}
		return nil, &apiError{Code: "INVALID_BODY", Message: "request body could not be read", Status: http.StatusBadRequest}
	}
	return body, nil
}

func decodeBytes(body []byte, target any) *apiError {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &apiError{Code: "INVALID_JSON", Message: "request body is not valid JSON", Status: http.StatusBadRequest}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &apiError{Code: "INVALID_JSON", Message: "request body must contain one JSON object", Status: http.StatusBadRequest}
	}
	return nil
}

func bearerToken(r *http.Request) (string, *apiError) {
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", &apiError{Code: "UNAUTHORIZED", Message: "a bearer participant token is required", Status: http.StatusUnauthorized}
	}
	return strings.TrimSpace(parts[1]), nil
}

func requestID(r *http.Request) string {
	value, _ := r.Context().Value(requestIDKey{}).(string)
	return value
}

func randomID(prefix string) string { return prefix + "_" + randomSecret()[:22] }

func randomSecret() string {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		panic(fmt.Sprintf("secure random source unavailable: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(buffer)
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func quantize(value, step float64) float64 { return math.Round(value/step) * step }

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func displayAlias(seed string) (string, string) {
	colors := []string{"cyan", "magenta", "amber", "violet", "lime", "coral"}
	symbols := []string{"circle", "triangle", "diamond", "star", "wave", "square"}
	sum := sha256.Sum256([]byte(seed))
	return colors[int(sum[0])%len(colors)], symbols[int(sum[1])%len(symbols)]
}

func internalError() *apiError {
	return &apiError{Code: "INTERNAL_ERROR", Message: "internal server error", Status: http.StatusInternalServerError}
}
