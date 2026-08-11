package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/store"
)

type createdExperience struct {
	ExperienceID string `json:"experienceId"`
	Memory       struct {
		SessionID        string `json:"sessionId"`
		ParticipantToken string `json:"participantToken"`
	} `json:"memorySession"`
	Live struct {
		SessionID        string `json:"sessionId"`
		ParticipantToken string `json:"participantToken"`
	} `json:"livePresenceSession"`
}

func TestExperiencePositionStopAndDelete(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Default()
	handler := api.New(cfg, db)

	created := createExperience(t, handler, cfg)
	if created.ExperienceID == "" || created.Memory.SessionID == "" || created.Live.SessionID == "" {
		t.Fatalf("incomplete create response: %+v", created)
	}

	positionBody := fmt.Sprintf(`{"point":{"latitude":35.0001,"longitude":135.0001,"accuracyM":8,"recordedAt":%q}}`, time.Now().UTC().Format(time.RFC3339Nano))
	position := request(t, handler, http.MethodPost, "/v1/live-presence-sessions/"+created.Live.SessionID+"/position", positionBody, map[string]string{
		"Authorization": "Bearer " + created.Live.ParticipantToken,
	})
	if position.Code != http.StatusOK {
		t.Fatalf("position status=%d body=%s", position.Code, position.Body.String())
	}
	_, storedPosition, err := db.LiveSession(t.Context(), created.Live.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if storedPosition == nil {
		t.Fatal("position was not stored")
	}
	if storedPosition.X != 10 || storedPosition.Z != 10 {
		t.Fatalf("position was not quantized: x=%v z=%v", storedPosition.X, storedPosition.Z)
	}

	stopHeaders := map[string]string{
		"Authorization":   "Bearer " + created.Live.ParticipantToken,
		"Idempotency-Key": "stop-once",
	}
	stopBody := `{"reason":"participant_request"}`
	firstStop := request(t, handler, http.MethodPost, "/v1/live-presence-sessions/"+created.Live.SessionID+"/stop", stopBody, stopHeaders)
	secondStop := request(t, handler, http.MethodPost, "/v1/live-presence-sessions/"+created.Live.SessionID+"/stop", stopBody, stopHeaders)
	if firstStop.Code != http.StatusOK || secondStop.Code != http.StatusOK || firstStop.Body.String() != secondStop.Body.String() {
		t.Fatalf("idempotent stop mismatch: first=%d %s second=%d %s", firstStop.Code, firstStop.Body.String(), secondStop.Code, secondStop.Body.String())
	}
	conflict := request(t, handler, http.MethodPost, "/v1/live-presence-sessions/"+created.Live.SessionID+"/stop", `{"reason":"different"}`, stopHeaders)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("expected idempotency conflict, got %d: %s", conflict.Code, conflict.Body.String())
	}
	_, storedPosition, err = db.LiveSession(t.Context(), created.Live.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if storedPosition != nil {
		t.Fatal("stopping tracking did not delete the latest position")
	}

	deleteHeaders := map[string]string{
		"Authorization":   "Bearer " + created.Memory.ParticipantToken,
		"Idempotency-Key": "delete-once",
	}
	firstDelete := request(t, handler, http.MethodDelete, "/v1/memory-sessions/"+created.Memory.SessionID, "", deleteHeaders)
	secondDelete := request(t, handler, http.MethodDelete, "/v1/memory-sessions/"+created.Memory.SessionID, "", deleteHeaders)
	if firstDelete.Code != http.StatusNoContent || secondDelete.Code != http.StatusNoContent {
		t.Fatalf("idempotent delete failed: first=%d second=%d", firstDelete.Code, secondDelete.Code)
	}
}

func TestConsentAndAuthenticationFailures(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Default()
	handler := api.New(cfg, db)

	wrongConsent := fmt.Sprintf(`{"memoryConsentVersion":"old","livePresenceConsentVersion":%q,"enableLivePresence":true,"consentLanguage":"ja","clientTime":%q}`,
		cfg.LiveConsentVersion, time.Now().UTC().Format(time.RFC3339Nano))
	response := request(t, handler, http.MethodPost, "/v1/experiences", wrongConsent, nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("expected consent conflict, got %d: %s", response.Code, response.Body.String())
	}

	created := createExperience(t, handler, cfg)
	unauthorized := request(t, handler, http.MethodGet, "/v1/experiences/"+created.ExperienceID, "", map[string]string{
		"Authorization": "Bearer wrong-token",
	})
	if unauthorized.Code != http.StatusNotFound {
		t.Fatalf("expected concealed not found, got %d: %s", unauthorized.Code, unauthorized.Body.String())
	}
	authorized := request(t, handler, http.MethodGet, "/v1/experiences/"+created.ExperienceID, "", map[string]string{
		"Authorization": "Bearer " + created.Memory.ParticipantToken,
	})
	if authorized.Code != http.StatusOK {
		t.Fatalf("state status=%d body=%s", authorized.Code, authorized.Body.String())
	}
}

func createExperience(t *testing.T, handler http.Handler, cfg config.Config) createdExperience {
	t.Helper()
	body := fmt.Sprintf(`{"memoryConsentVersion":%q,"livePresenceConsentVersion":%q,"enableLivePresence":true,"consentLanguage":%q,"clientTime":%q}`,
		cfg.MemoryConsentVersion, cfg.LiveConsentVersion, cfg.ConsentLanguage, time.Now().UTC().Format(time.RFC3339Nano))
	response := request(t, handler, http.MethodPost, "/v1/experiences", body, nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var created createdExperience
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created
}

func request(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}
