package api

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/Mino829/trace_Backend/internal/photo"
	"github.com/Mino829/trace_Backend/internal/photoasset"
	"github.com/Mino829/trace_Backend/internal/store"
)

type createPhotoRequest struct {
	ClientPhotoID string    `json:"clientPhotoId"`
	CapturedAt    time.Time `json:"capturedAt"`
	Location      *struct {
		Latitude   float64   `json:"latitude"`
		Longitude  float64   `json:"longitude"`
		AccuracyM  float64   `json:"accuracyM"`
		RecordedAt time.Time `json:"recordedAt"`
	} `json:"location"`
	MIMEType  string `json:"mimeType"`
	SizeBytes int64  `json:"sizeBytes"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	SHA256    string `json:"sha256"`
}

func (s *Server) photoAvailable(w http.ResponseWriter, r *http.Request, writing bool) bool {
	if s.photos == nil || (writing && !s.cfg.PhotoEnabled) {
		s.writeError(w, requestID(r), &apiError{Code: "PHOTO_DISABLED", Message: "photo storage is not enabled", Status: http.StatusServiceUnavailable})
		return false
	}
	if err := s.authenticateMemory(r, r.PathValue("memorySessionId")); err != nil {
		s.writeError(w, requestID(r), err)
		return false
	}
	return true
}

func (s *Server) photoError(w http.ResponseWriter, r *http.Request, err error) {
	failure := &apiError{Code: "PHOTO_STORAGE_UNAVAILABLE", Message: "photo operation could not be completed; retry later", Status: http.StatusServiceUnavailable}
	switch {
	case errors.Is(err, store.ErrNotFound):
		failure = &apiError{Code: "NOT_FOUND", Message: "photo was not found", Status: http.StatusNotFound}
	case errors.Is(err, store.ErrPhotoLimit):
		failure = &apiError{Code: "PHOTO_LIMIT_REACHED", Message: "the session photo limit was reached", Status: http.StatusConflict}
	case errors.Is(err, store.ErrUploadLimit):
		failure = &apiError{Code: "UPLOAD_URL_LIMIT_REACHED", Message: "the upload URL issuance limit was reached", Status: http.StatusConflict}
	case errors.Is(err, photo.ErrUploadIncomplete):
		failure = &apiError{Code: "UPLOAD_INCOMPLETE", Message: "upload the image before completing it", Status: http.StatusConflict}
	case errors.Is(err, store.ErrConflict):
		failure = &apiError{Code: "STATE_CONFLICT", Message: "photo state or request content conflicts", Status: http.StatusConflict}
	}
	// Storage and database errors can contain object keys, hashes or GPS values.
	// Return only the fixed error envelope, never the underlying error string.
	s.writeError(w, requestID(r), failure)
}

func validIdempotencyKey(r *http.Request) bool {
	key := r.Header.Get("Idempotency-Key")
	return key != "" && len(key) <= 200
}

func (s *Server) createPhoto(w http.ResponseWriter, r *http.Request) {
	if !s.photoAvailable(w, r, true) {
		return
	}
	if !validIdempotencyKey(r) {
		s.writeError(w, requestID(r), &apiError{Code: "IDEMPOTENCY_KEY_REQUIRED", Message: "a valid Idempotency-Key header is required", Status: http.StatusBadRequest})
		return
	}
	body, bodyErr := readBody(w, r)
	if bodyErr != nil {
		s.writeError(w, requestID(r), bodyErr)
		return
	}
	var input createPhotoRequest
	if err := decodeBytes(body, &input); err != nil {
		s.writeError(w, requestID(r), err)
		return
	}
	digest, err := hex.DecodeString(input.SHA256)
	if input.ClientPhotoID == "" || len(input.ClientPhotoID) > 200 || input.CapturedAt.IsZero() ||
		(input.MIMEType != "image/jpeg" && input.MIMEType != "image/webp") || input.SizeBytes <= 0 || input.SizeBytes > photoasset.MaxBytes ||
		input.Width <= 0 || input.Height <= 0 || input.Width > photoasset.MaxLongEdge || input.Height > photoasset.MaxLongEdge || err != nil || len(digest) != 32 {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_PHOTO_METADATA", Message: "photo metadata is invalid", Status: http.StatusBadRequest})
		return
	}
	var location *store.PhotoLocation
	if input.Location != nil {
		p := input.Location
		if math.IsNaN(p.Latitude) || math.IsInf(p.Latitude, 0) || math.IsNaN(p.Longitude) || math.IsInf(p.Longitude, 0) ||
			math.IsNaN(p.AccuracyM) || math.IsInf(p.AccuracyM, 0) || p.AccuracyM <= 0 || p.RecordedAt.IsZero() ||
			p.Latitude < s.cfg.MinLatitude || p.Latitude > s.cfg.MaxLatitude || p.Longitude < s.cfg.MinLongitude || p.Longitude > s.cfg.MaxLongitude {
			s.writeError(w, requestID(r), &apiError{Code: "INVALID_PHOTO_LOCATION", Message: "photo location is invalid or outside the campus", Status: http.StatusBadRequest})
			return
		}
		location = &store.PhotoLocation{Latitude: p.Latitude, Longitude: p.Longitude, AccuracyM: p.AccuracyM, RecordedAt: p.RecordedAt.UTC()}
	}
	now := s.now().UTC()
	memoryID := r.PathValue("memorySessionId")
	token, _ := bearerToken(r)
	scope, key := hash("create:photo:"+memoryID), hash(r.Header.Get("Idempotency-Key")+"\x00"+token)
	requestHash := hashBytes(body)
	var result store.Photo
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
			result, err = tx.PhotoByHash(r.Context(), memoryID, locator.PhotoHash)
			return err
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		memory, err := tx.MemorySession(r.Context(), memoryID)
		if err != nil {
			return err
		}
		expires := now.Add(5 * time.Minute)
		if expires.After(memory.ExpiresAt) {
			expires = memory.ExpiresAt
		}
		id := randomID("photo")
		result, err = tx.CreatePhoto(r.Context(), store.Photo{
			ID: id, MemorySessionID: memoryID, ClientPhotoID: input.ClientPhotoID, RequestHash: requestHash,
			CapturedAt: input.CapturedAt.UTC(), CreatedAt: now, UploadExpiresAt: expires, Location: location,
			MIMEType: input.MIMEType, SizeBytes: input.SizeBytes, Width: input.Width, Height: input.Height,
			SHA256: strings.ToLower(input.SHA256), StageKey: "uploads/" + randomSecret(), ObjectKey: "photos/" + randomSecret(),
		}, now)
		if err != nil {
			return err
		}
		created = result.ID == id
		// Keep only a one-way locator in the 24h receipt, not photo IDs,
		// signed capabilities, image hashes, GPS, or complete request content.
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
	s.writePhotoUpload(w, r, status, result, now)
}

func (s *Server) writePhotoUpload(w http.ResponseWriter, r *http.Request, status int, p store.Photo, now time.Time) {
	response := map[string]any{"photoMomentId": p.ID, "status": p.Status, "uploadExpiresAt": p.UploadExpiresAt}
	if p.Status == "pending" && now.Before(p.UploadExpiresAt) {
		upload, err := s.photos.Upload(r.Context(), p, now)
		if err != nil {
			s.photoError(w, r, err)
			return
		}
		response["uploadUrl"], response["uploadHeaders"] = upload.URL, upload.Headers
	}
	s.writeJSON(w, status, response)
}

func (s *Server) renewPhotoUpload(w http.ResponseWriter, r *http.Request) {
	if !s.photoAvailable(w, r, true) {
		return
	}
	if !s.emptyPhotoBody(w, r) {
		return
	}
	now := s.now().UTC()
	memoryID := r.PathValue("memorySessionId")
	memory, err := s.store.MemorySession(r.Context(), memoryID)
	if err != nil {
		s.photoError(w, r, err)
		return
	}
	expires := now.Add(5 * time.Minute)
	if expires.After(memory.ExpiresAt) {
		expires = memory.ExpiresAt
	}
	p, err := s.store.RenewPhotoUpload(r.Context(), memoryID, r.PathValue("photoMomentId"), expires, now)
	if err != nil {
		s.photoError(w, r, err)
		return
	}
	s.writePhotoUpload(w, r, http.StatusOK, p, now)
}

func (s *Server) completePhoto(w http.ResponseWriter, r *http.Request) {
	if !s.photoAvailable(w, r, true) {
		return
	}
	if !s.emptyPhotoBody(w, r) {
		return
	}
	p, err := s.photos.Complete(r.Context(), r.PathValue("memorySessionId"), r.PathValue("photoMomentId"), s.now().UTC())
	if err != nil {
		s.photoError(w, r, err)
		return
	}
	response := map[string]any{"photoMomentId": p.ID, "status": p.Status}
	if p.FailureCode != "" {
		response["failureCode"] = p.FailureCode
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) emptyPhotoBody(w http.ResponseWriter, r *http.Request) bool {
	body, err := readBody(w, r)
	if err != nil {
		s.writeError(w, requestID(r), err)
		return false
	}
	if len(bytes.TrimSpace(body)) != 0 {
		s.writeError(w, requestID(r), &apiError{Code: "INVALID_BODY", Message: "request body must be empty", Status: http.StatusBadRequest})
		return false
	}
	return true
}

func (s *Server) listPhotos(w http.ResponseWriter, r *http.Request) {
	if !s.photoAvailable(w, r, false) {
		return
	}
	photos, err := s.store.Photos(r.Context(), r.PathValue("memorySessionId"))
	if err != nil {
		s.photoError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(photos))
	for _, p := range photos {
		item := map[string]any{"photoMomentId": p.ID, "clientPhotoId": p.ClientPhotoID, "sequenceNo": p.SequenceNo, "capturedAt": p.CapturedAt, "status": p.Status}
		if p.FailureCode != "" {
			item["failureCode"] = p.FailureCode
		}
		items = append(items, item)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"photos": items})
}

func (s *Server) photoMemory(w http.ResponseWriter, r *http.Request) {
	if !s.photoAvailable(w, r, false) {
		return
	}
	memoryID := r.PathValue("memorySessionId")
	memory, err := s.store.MemorySession(r.Context(), memoryID)
	if err != nil {
		s.photoError(w, r, err)
		return
	}
	if memory.Deleting {
		s.photoError(w, r, store.ErrConflict)
		return
	}
	photos, err := s.store.Photos(r.Context(), memoryID)
	if err != nil {
		s.photoError(w, r, err)
		return
	}
	moments := make([]map[string]any, 0, len(photos))
	for _, p := range photos {
		if p.Status != "ready" {
			continue
		}
		url, err := s.photos.View(r.Context(), p, s.now().UTC())
		if err != nil {
			s.photoError(w, r, err)
			return
		}
		confidence := "low"
		if p.Location != nil && p.Location.AccuracyM <= 30 {
			confidence = "high"
		} else if p.Location != nil && p.Location.AccuracyM <= 100 {
			confidence = "medium"
		}
		moments = append(moments, map[string]any{"photoMomentId": p.ID, "capturedAt": p.CapturedAt, "imageUrl": url, "locationConfidence": confidence})
	}
	// RouteSegment generation is implemented separately by Backend Issue #7.
	// No route is fabricated for uploaded photos.
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "moments": moments, "segments": []any{}, "deleteAfter": memory.DeleteAfter})
}

func (s *Server) deletePhoto(w http.ResponseWriter, r *http.Request) {
	if !s.photoAvailable(w, r, false) {
		return
	}
	if !s.emptyPhotoBody(w, r) {
		return
	}
	if !validIdempotencyKey(r) {
		s.writeError(w, requestID(r), &apiError{Code: "IDEMPOTENCY_KEY_REQUIRED", Message: "a valid Idempotency-Key header is required", Status: http.StatusBadRequest})
		return
	}
	memoryID, photoID := r.PathValue("memorySessionId"), r.PathValue("photoMomentId")
	token, _ := bearerToken(r)
	scope, key := hash("delete:photo:"+memoryID+":"+photoID), hash(r.Header.Get("Idempotency-Key")+"\x00"+token)
	now := s.now().UTC()
	complete := false
	err := s.store.Transaction(r.Context(), memoryID, func(tx *store.Store) error {
		receipt, err := tx.GetIdempotency(r.Context(), scope, key, now)
		if err == nil && receipt.ResponseStatus == http.StatusNoContent {
			complete = true
			return nil
		}
		intent := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		_, err = tx.Photo(r.Context(), memoryID, photoID)
		if errors.Is(err, store.ErrNotFound) && intent {
			complete = true
			return nil
		}
		if err != nil {
			return err
		}
		if err := tx.QueuePhotoDeletion(r.Context(), memoryID, photoID, "deleted", ""); err != nil {
			return err
		}
		return tx.SaveIdempotency(r.Context(), scope, key, hashBytes(nil), http.StatusAccepted, []byte{}, now.Add(24*time.Hour))
	})
	if err != nil {
		s.photoError(w, r, err)
		return
	}
	if !complete {
		if err := s.photos.DeletePhoto(r.Context(), memoryID, photoID); err != nil {
			s.photoError(w, r, err)
			return
		}
	}
	if err := s.store.FinishPhotoDeleteReceipt(r.Context(), scope, key, now); err != nil {
		s.photoError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Keep the existing atomic deletion/receipt implementation, but erase photos
// before it can remove the MemorySession and its authentication material.
func (s *Server) prepareMemoryPhotoDeletion(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.photos == nil {
			next(w, r)
			return
		}
		if !s.emptyPhotoBody(w, r) {
			return
		}
		if s.replayIdempotent(w, r, "delete:memory:"+r.PathValue("memorySessionId"), nil) {
			return
		}
		if err := s.authenticateMemory(r, r.PathValue("memorySessionId")); err != nil {
			s.writeError(w, requestID(r), err)
			return
		}
		if err := s.photos.DeleteMemoryPhotos(r.Context(), r.PathValue("memorySessionId")); err != nil {
			s.photoError(w, r, err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(nil))
		next(w, r)
	}
}
