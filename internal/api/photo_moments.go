package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
)

type photoMomentRequest struct {
	ClientPhotoID string    `json:"clientPhotoId"`
	CheckpointID  string    `json:"checkpointId"`
	CapturedAt    time.Time `json:"capturedAt"`
}

type photoMomentResponse struct {
	ID            string    `json:"photoMomentId"`
	ClientPhotoID string    `json:"clientPhotoId"`
	CheckpointID  string    `json:"checkpointId"`
	CapturedAt    time.Time `json:"capturedAt"`
	SequenceNo    int       `json:"sequenceNo"`
}

func momentResponse(p store.PhotoMoment) photoMomentResponse {
	return photoMomentResponse{ID: p.ID, ClientPhotoID: p.ClientPhotoID, CheckpointID: p.CheckpointID,
		CapturedAt: p.CapturedAt, SequenceNo: p.SequenceNo}
}

func (s *Server) devicePhotoAvailable(w http.ResponseWriter, r *http.Request) (store.MemorySession, bool) {
	if !s.cfg.PhotoMetadataEnabled {
		s.writeError(w, requestID(r), &apiError{Code: "PHOTO_DISABLED", Message: "device photo metadata is not enabled", Status: http.StatusServiceUnavailable})
		return store.MemorySession{}, false
	}
	memoryID := r.PathValue("memorySessionId")
	if err := s.authenticateMemory(r, memoryID); err != nil {
		s.writeError(w, requestID(r), err)
		return store.MemorySession{}, false
	}
	memory, err := s.store.MemorySession(r.Context(), memoryID)
	if err != nil {
		s.photoError(w, r, err)
		return store.MemorySession{}, false
	}
	if memory.PhotoMode != store.PhotoModeDevice {
		s.photoError(w, r, store.ErrNotFound)
		return store.MemorySession{}, false
	}
	return memory, true
}

// Decode exactly the three permitted keys, including rejecting duplicate keys.
// A struct decoder alone accepts case-folded and repeated JSON keys.
func decodePhotoMomentRequest(body []byte) (photoMomentRequest, *apiError) {
	invalid := &apiError{Code: "INVALID_JSON", Message: "request body must contain only clientPhotoId, checkpointId and capturedAt", Status: http.StatusBadRequest}
	var input photoMomentRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return input, invalid
	}
	var seen uint8
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return input, invalid
		}
		name, ok := key.(string)
		if !ok {
			return input, invalid
		}
		var bit uint8
		switch name {
		case "clientPhotoId":
			bit = 1
			err = decoder.Decode(&input.ClientPhotoID)
		case "checkpointId":
			bit = 2
			err = decoder.Decode(&input.CheckpointID)
		case "capturedAt":
			bit = 4
			err = decoder.Decode(&input.CapturedAt)
		default:
			return input, invalid
		}
		if err != nil || seen&bit != 0 {
			return input, invalid
		}
		seen |= bit
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || seen != 7 {
		return input, invalid
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return input, invalid
	}
	return input, nil
}

func (s *Server) createPhotoMoment(w http.ResponseWriter, r *http.Request) {
	memory, ok := s.devicePhotoAvailable(w, r)
	if !ok {
		return
	}
	if !validIdempotencyKey(r) {
		s.writeError(w, requestID(r), &apiError{Code: "IDEMPOTENCY_KEY_REQUIRED", Message: "a valid Idempotency-Key header is required", Status: http.StatusBadRequest})
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_BODY", Message: "Content-Type must be application/json", Status: http.StatusBadRequest})
		return
	}
	body, bodyErr := readBody(w, r)
	if bodyErr != nil {
		s.writeError(w, requestID(r), bodyErr)
		return
	}
	input, bodyErr := decodePhotoMomentRequest(body)
	if bodyErr != nil {
		s.writeError(w, requestID(r), bodyErr)
		return
	}
	if input.ClientPhotoID == "" || len(input.ClientPhotoID) > 200 || input.CapturedAt.IsZero() {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_PHOTO_METADATA", Message: "photo metadata is invalid", Status: http.StatusBadRequest})
		return
	}
	if input.CheckpointID == "" {
		s.photoError(w, r, store.ErrInvalidCheckpoint)
		return
	}
	input.CapturedAt = input.CapturedAt.UTC()
	canonical, err := json.Marshal(input)
	if err != nil {
		s.writeError(w, requestID(r), internalError())
		return
	}
	requestHash := hashBytes(canonical)
	// PostgreSQL retains microseconds; keep create and replay responses identical.
	input.CapturedAt = input.CapturedAt.Truncate(time.Microsecond)
	memoryID := memory.ID
	token, _ := bearerToken(r)
	scope, key := hash("create:moment:"+memoryID), hash(r.Header.Get("Idempotency-Key")+"\x00"+token)
	now := s.now().UTC()
	var result store.PhotoMoment
	created := false
	err = s.store.Transaction(r.Context(), memoryID, func(tx *store.Store) error {
		receipt, err := tx.GetIdempotency(r.Context(), scope, key, now)
		if err == nil {
			if receipt.RequestHash != requestHash {
				return store.ErrConflict
			}
			var locator struct {
				PhotoHash string `json:"photoHash"`
			}
			if err := json.Unmarshal(receipt.ResponseBody, &locator); err != nil {
				return err
			}
			returned, err := tx.PhotoMomentByHash(r.Context(), memoryID, locator.PhotoHash)
			result = returned
			return err
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		id := randomID("photo")
		result, created, err = tx.CreatePhotoMoment(r.Context(), store.PhotoMoment{
			ID: id, MemorySessionID: memoryID, ClientPhotoID: input.ClientPhotoID,
			CheckpointID: input.CheckpointID, CapturedAt: input.CapturedAt,
			CampusGraphVersion: memory.CampusGraphVersion, RequestHash: requestHash,
		}, now)
		if err != nil {
			return err
		}
		// A receipt is a one-way lookup key, not a copy of the moment or its metadata.
		locator, err := json.Marshal(map[string]string{"photoHash": hash(result.ID)})
		if err != nil {
			return err
		}
		return tx.SaveIdempotency(r.Context(), scope, key, requestHash, http.StatusCreated, locator, now.Add(24*time.Hour))
	})
	if err != nil {
		s.photoError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, momentResponse(result))
}

func (s *Server) listPhotoMoments(w http.ResponseWriter, r *http.Request) {
	memory, ok := s.devicePhotoAvailable(w, r)
	if !ok {
		return
	}
	moments, err := s.store.PhotoMoments(r.Context(), memory.ID)
	if err != nil {
		s.photoError(w, r, err)
		return
	}
	photos := make([]photoMomentResponse, 0, len(moments))
	for _, moment := range moments {
		photos = append(photos, momentResponse(moment))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"photos": photos})
}

func (s *Server) deletePhotoMoment(w http.ResponseWriter, r *http.Request) {
	memory, ok := s.devicePhotoAvailable(w, r)
	if !ok {
		return
	}
	if !s.emptyPhotoBody(w, r) {
		return
	}
	if !validIdempotencyKey(r) {
		s.writeError(w, requestID(r), &apiError{Code: "IDEMPOTENCY_KEY_REQUIRED", Message: "a valid Idempotency-Key header is required", Status: http.StatusBadRequest})
		return
	}
	photoID := r.PathValue("photoMomentId")
	token, _ := bearerToken(r)
	scope, key := hash("delete:moment:"+memory.ID+":"+photoID), hash(r.Header.Get("Idempotency-Key")+"\x00"+token)
	now := s.now().UTC()
	err := s.store.Transaction(r.Context(), memory.ID, func(tx *store.Store) error {
		receipt, err := tx.GetIdempotency(r.Context(), scope, key, now)
		if err == nil {
			if receipt.RequestHash != hashBytes(nil) || receipt.ResponseStatus != http.StatusNoContent {
				return store.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := tx.DeletePhotoMoment(r.Context(), memory.ID, photoID, now); err != nil {
			return err
		}
		return tx.SaveIdempotency(r.Context(), scope, key, hashBytes(nil), http.StatusNoContent, []byte{}, now.Add(24*time.Hour))
	})
	if err != nil {
		s.photoError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) photoMomentMemory(w http.ResponseWriter, r *http.Request) {
	memory, ok := s.devicePhotoAvailable(w, r)
	if !ok {
		return
	}
	moments, err := s.store.PhotoMoments(r.Context(), memory.ID)
	if err != nil {
		s.photoError(w, r, err)
		return
	}
	segments, err := s.store.MomentRouteSegments(r.Context(), memory.ID)
	if err != nil {
		s.photoError(w, r, err)
		return
	}
	story := make([]map[string]any, 0, len(moments))
	for _, p := range moments {
		story = append(story, map[string]any{
			"photoMomentId": p.ID, "clientPhotoId": p.ClientPhotoID,
			"checkpointId": p.CheckpointID, "capturedAt": p.CapturedAt,
		})
	}
	paths := make([]map[string]any, 0, len(segments))
	for _, segment := range segments {
		paths = append(paths, map[string]any{
			"routeSegmentId": segment.ID, "fromPhotoMomentId": segment.FromPhotoMomentID,
			"toPhotoMomentId": segment.ToPhotoMomentID, "graphVersion": segment.CampusGraphVersion,
			"pathNodeIds": segment.PathNodeIDs,
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"moments": story, "segments": paths, "deleteAfter": memory.DeleteAfter})
}
