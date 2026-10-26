package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

type createdExperience struct {
	ExperienceID string `json:"experienceId"`
	LinkToken    string `json:"experienceLinkToken"`
	Memory       struct {
		SessionID          string    `json:"sessionId"`
		ParticipantToken   string    `json:"participantToken"`
		ExpiresAt          time.Time `json:"expiresAt"`
		CampusGraphVersion string    `json:"campusGraphVersion"`
		DeleteAfter        time.Time `json:"deleteAfter"`
	} `json:"memorySession"`
	Live struct {
		SessionID        string    `json:"sessionId"`
		ParticipantToken string    `json:"participantToken"`
		ExpiresAt        time.Time `json:"expiresAt"`
		DeleteAfter      time.Time `json:"deleteAfter"`
	} `json:"livePresenceSession"`
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func TestEdgeSecretProtectsPublicAPIWithoutBlockingLiveness(t *testing.T) {
	cfg := config.Default()
	cfg.EdgeSharedSecret = "a-32-byte-secret-for-cloudflare-edge"
	handler := api.New(cfg, nil, nil)
	for _, tt := range []struct {
		path, secret string
		status       int
	}{
		{"/v1/public/config", "", http.StatusForbidden},
		{"/v1/public/config", "wrong", http.StatusForbidden},
		{"/health/live", "", http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)
		if tt.secret != "" {
			req.Header.Set("X-Trace-Edge-Secret", tt.secret)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != tt.status {
			t.Errorf("%s with secret %t: got %d, want %d", tt.path, tt.secret != "", response.Code, tt.status)
		}
	}
}

func TestExperienceDeleteCORSPreflight(t *testing.T) {
	cfg := config.Default()
	cfg.AllowedOrigins = []string{"https://participant.example"}
	handler := api.New(cfg, nil, nil)
	req := httptest.NewRequest(http.MethodOptions, "/v1/experiences/exp_example", nil)
	req.Header.Set("Origin", cfg.AllowedOrigins[0])
	req.Header.Set("Access-Control-Request-Method", http.MethodDelete)
	req.Header.Set("Access-Control-Request-Headers", "authorization,x-trace-live-token,idempotency-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusNoContent ||
		response.Header().Get("Access-Control-Allow-Origin") != cfg.AllowedOrigins[0] ||
		!strings.Contains(strings.ToLower(response.Header().Get("Access-Control-Allow-Headers")), "x-trace-live-token") {
		t.Fatalf("delete preflight: status=%d headers=%v", response.Code, response.Header())
	}
}

func TestEdgeSecretAllowsReadyAPI(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	cfg.EdgeSharedSecret = "a-32-byte-secret-for-cloudflare-edge"
	handler := api.New(cfg, f.Store, nil)
	response := request(t, handler, http.MethodGet, "/v1/public/config", "", map[string]string{
		"X-Trace-Edge-Secret": cfg.EdgeSharedSecret,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("valid edge secret and ready schema: %d", response.Code)
	}
}

func TestNewSchemaBlocksOldAPIWhileLivenessStaysAvailable(t *testing.T) {
	f := testdb.Open(t)
	handler := api.New(config.Default(), f.Store, nil)
	if got := request(t, handler, http.MethodGet, "/v1/public/config", "", nil).Code; got != http.StatusOK {
		t.Fatalf("initial config status: %d", got)
	}
	if _, err := f.SQL.ExecContext(t.Context(), "INSERT INTO schema_migrations (version) SELECT MAX(version)+1 FROM schema_migrations"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/health/live", "", http.StatusOK},
		{http.MethodGet, "/health/ready", "", http.StatusServiceUnavailable},
		{http.MethodGet, "/v1/public/config", "", http.StatusServiceUnavailable},
		{http.MethodPost, "/v1/experiences", `{}`, http.StatusServiceUnavailable},
	} {
		response := request(t, handler, test.method, test.path, test.body, nil)
		if response.Code != test.want {
			t.Errorf("%s %s: got %d, want %d", test.method, test.path, response.Code, test.want)
		}
	}
	var count int
	if err := f.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM experiences").Scan(&count); err != nil || count != 0 {
		t.Fatalf("outdated API modified data: count=%d err=%v", count, err)
	}
}

func TestConcurrentDeleteAcrossInstancesAndCredentialBinding(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	handler := api.New(cfg, f.Store, nil)
	second, err := store.Open(t.Context(), f.URL, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	other := api.New(cfg, second, nil)
	created := seedLegacyExperience(t, f.Store, cfg)
	headers := map[string]string{"Authorization": "Bearer " + created.Live.ParticipantToken, "Idempotency-Key": "same-delete"}
	path := "/v1/live-presence-sessions/" + created.Live.SessionID
	var wg sync.WaitGroup
	results := make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := handler
			if i%2 == 0 {
				h = other
			}
			results <- request(t, h, "DELETE", path, "", headers).Code
		}(i)
	}
	wg.Wait()
	close(results)
	for status := range results {
		if status != 204 {
			t.Fatalf("concurrent delete: %d", status)
		}
	}
	for _, token := range []string{"", created.Memory.ParticipantToken, "wrong"} {
		r := request(t, handler, "DELETE", path, "", map[string]string{"Authorization": "Bearer " + token, "Idempotency-Key": "same-delete"})
		if r.Code != 401 {
			t.Fatalf("unauthorized replay status %d", r.Code)
		}
	}
	if err := f.Store.AuthenticateMemory(t.Context(), created.Memory.SessionID, tokenHash(created.Memory.ParticipantToken), time.Now()); err != nil {
		t.Fatal("live deletion removed memory session")
	}
}

func TestLegacyStopClearsPositionWhileWritesRetiredAndDeleteRollback(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	h := api.New(cfg, f.Store, nil)
	c := seedLegacyExperience(t, f.Store, cfg)
	path := "/v1/live-presence-sessions/" + c.Live.SessionID
	now := time.Now().UTC()
	if err := f.Store.SavePosition(t.Context(), c.Live.SessionID, store.Position{
		X: 10, Z: 10, AccuracyM: 8, RecordedAt: now, ReceivedAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	done := make(chan int, 2)
	body := fmt.Sprintf(`{"point":{"latitude":35.0001,"longitude":135.0001,"accuracyM":8,"recordedAt":%q}}`, time.Now().UTC().Format(time.RFC3339Nano))
	go func() {
		<-start
		done <- request(t, h, "POST", path+"/position", body, map[string]string{"Authorization": "Bearer " + c.Live.ParticipantToken}).Code
	}()
	go func() {
		<-start
		done <- request(t, h, "POST", path+"/stop", `{"reason":"participant_request"}`, map[string]string{"Authorization": "Bearer " + c.Live.ParticipantToken, "Idempotency-Key": "stop-race"}).Code
	}()
	close(start)
	got := map[int]int{}
	for range 2 {
		got[<-done]++
	}
	if got[http.StatusGone] != 1 || got[http.StatusOK] != 1 || len(got) != 2 {
		t.Fatalf("position must be refused while stop succeeds: %v", got)
	}
	session, p, err := f.Store.LiveSession(t.Context(), c.Live.SessionID)
	if err != nil || p != nil || session.TrackingStatus != "stopped" {
		t.Fatal("position survived stop")
	}
	// Deliberately force receipt insertion to fail. The DELETE must roll back.
	if _, err := f.SQL.ExecContext(t.Context(), `ALTER TABLE idempotency_records ADD CONSTRAINT reject_delete CHECK (response_status <> 204)`); err != nil {
		t.Fatal(err)
	}
	r := request(t, h, "DELETE", "/v1/memory-sessions/"+c.Memory.SessionID, "", map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken, "Idempotency-Key": "rollback"})
	if r.Code != 500 {
		t.Fatalf("failed receipt status %d", r.Code)
	}
	if err := f.Store.AuthenticateMemory(t.Context(), c.Memory.SessionID, tokenHash(c.Memory.ParticipantToken), time.Now()); err != nil {
		t.Fatal("mutation committed without receipt")
	}
}

func TestPositionRetiredAndSessionIsolation(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	h := api.New(cfg, f.Store, nil)
	c := seedLegacyExperience(t, f.Store, cfg)
	path := "/v1/live-presence-sessions/" + c.Live.SessionID + "/position"
	for _, body := range []string{"{", `{"point":{"latitude":35,"longitude":135}}`, ""} {
		r := request(t, h, "POST", path, body, map[string]string{"Authorization": "Bearer " + c.Live.ParticipantToken})
		if r.Code != http.StatusGone || !strings.Contains(r.Body.String(), `"code":"POSITION_DISABLED"`) {
			t.Fatalf("retired position: %d %s", r.Code, r.Body.String())
		}
	}
	for _, token := range []string{c.Memory.ParticipantToken, "wrong"} {
		r := request(t, h, "POST", path, "{", map[string]string{"Authorization": "Bearer " + token})
		if r.Code != http.StatusUnauthorized {
			t.Fatalf("invalid live credential: %d %s", r.Code, r.Body.String())
		}
	}
	_, point, err := f.Store.LiveSession(t.Context(), c.Live.SessionID)
	if err != nil || point != nil {
		t.Fatalf("position persisted: %v %v", point, err)
	}
	var events int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events WHERE kind LIKE 'position.%'`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("position display events: %d %v", events, err)
	}
}

func TestLegacyExperienceStopAndDelete(t *testing.T) {
	db := testdb.Open(t).Store
	cfg := config.Default()
	handler := api.New(cfg, db, nil)
	created := seedLegacyExperience(t, db, cfg)
	if created.ExperienceID == "" || created.Memory.SessionID == "" || created.Live.SessionID == "" {
		t.Fatalf("incomplete legacy fixture: %+v", created)
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
	_, storedPosition, err := db.LiveSession(t.Context(), created.Live.SessionID)
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
	db := testdb.Open(t).Store
	cfg := config.Default()
	cfg.RegistrationOpen = true
	handler := api.New(cfg, db, nil)

	wrongConsent := fmt.Sprintf(`{"memoryConsentVersion":"old","enableLivePresence":false,"consentLanguage":%q,"clientTime":%q}`,
		cfg.ConsentLanguage, time.Now().UTC().Format(time.RFC3339Nano))
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

func TestLiveEnrollmentRejectedWithoutPersistingExperience(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	cfg.RegistrationOpen = true
	h := api.New(cfg, f.Store, nil)
	body := fmt.Sprintf(`{"memoryConsentVersion":%q,"enableLivePresence":true,"consentLanguage":%q,"clientTime":%q}`,
		cfg.MemoryConsentVersion, cfg.ConsentLanguage, time.Now().UTC().Format(time.RFC3339Nano))
	r := request(t, h, http.MethodPost, "/v1/experiences", body, nil)
	if r.Code != http.StatusConflict || !strings.Contains(r.Body.String(), `"code":"LIVE_PRESENCE_DISABLED"`) {
		t.Fatalf("live enrollment: %d %s", r.Code, r.Body.String())
	}
	var count int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM experiences`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retired enrollment created rows: count=%d err=%v", count, err)
	}
	created := createExperience(t, h, cfg)
	if created.Memory.SessionID == "" || created.Live.SessionID != "" || created.LinkToken != "" {
		t.Fatalf("memory-only registration response: %+v", created)
	}
	var liveCount int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM live_presence_sessions`).Scan(&liveCount); err != nil || liveCount != 0 {
		t.Fatalf("memory-only created live session: %d %v", liveCount, err)
	}
}

func TestCombinedDeletionRetainsPartialSuccessForRetry(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	handler := api.New(cfg, f.Store, nil)
	created := seedLegacyExperience(t, f.Store, cfg)
	path := "/v1/experiences/" + created.ExperienceID
	headers := map[string]string{
		"Authorization":      "Bearer " + created.Memory.ParticipantToken,
		"X-Trace-Live-Token": created.Live.ParticipantToken,
		"Idempotency-Key":    "delete-both",
	}
	if response := request(t, handler, http.MethodDelete, path, "{}", headers); response.Code != 400 {
		t.Fatalf("DELETE body accepted: %d", response.Code)
	}
	wrong := map[string]string{
		"Authorization":      headers["Authorization"],
		"X-Trace-Live-Token": "incorrect",
		"Idempotency-Key":    headers["Idempotency-Key"],
	}
	if response := request(t, handler, http.MethodDelete, path, "", wrong); response.Code != 404 {
		t.Fatalf("wrong live credential removed a session: %d", response.Code)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `CREATE FUNCTION reject_memory_deletion() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'deletion temporarily unavailable'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `CREATE TRIGGER reject_memory_deletion BEFORE DELETE ON memory_sessions
FOR EACH ROW EXECUTE FUNCTION reject_memory_deletion()`); err != nil {
		t.Fatal(err)
	}
	first := request(t, handler, http.MethodDelete, path, "", headers)
	var partial struct {
		Memory, Live string
		Complete     bool
	}
	if err := json.Unmarshal(first.Body.Bytes(), &partial); err != nil ||
		first.Code != 200 || partial.Memory != "pending" || partial.Live != "deleted" || partial.Complete {
		t.Fatalf("partial deletion: %d %s: %v", first.Code, first.Body.String(), err)
	}
	if err := f.Store.AuthenticateMemory(t.Context(), created.Memory.SessionID, tokenHash(created.Memory.ParticipantToken), time.Now()); err != nil {
		t.Fatal("first half deleted memory:", err)
	}
	if err := f.Store.AuthenticateLive(t.Context(), created.Live.SessionID, tokenHash(created.Live.ParticipantToken), time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("successful live deletion rolled back:", err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), "DROP TRIGGER reject_memory_deletion ON memory_sessions"); err != nil {
		t.Fatal(err)
	}
	replayed := request(t, handler, http.MethodDelete, path, "", headers)
	var completed struct {
		Memory, Live string
		Complete     bool
	}
	if err := json.Unmarshal(replayed.Body.Bytes(), &completed); err != nil ||
		replayed.Code != 200 || completed.Memory != "deleted" || completed.Live != "deleted" || !completed.Complete {
		t.Fatalf("retry did not complete: %d %s: %v", replayed.Code, replayed.Body.String(), err)
	}
	if response := request(t, handler, http.MethodDelete, path, "", headers); response.Code != 200 {
		t.Fatalf("completed retry changed status: %d", response.Code)
	}
	if response := request(t, handler, http.MethodDelete, path, "", wrong); response.Code != 404 {
		t.Fatalf("wrong credentials replayed deletion: %d", response.Code)
	}
	if err := f.Store.AuthenticateMemory(t.Context(), created.Memory.SessionID, tokenHash(created.Memory.ParticipantToken), time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("memory remains after completed deletion:", err)
	}
}

func TestRateLimitSeparatesActiveSessionQuotasAndReturnsRetryAfter(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	cfg.RegistrationOpen = true
	handler := api.New(cfg, f.Store, nil)
	first := createExperience(t, handler, cfg)
	_ = createExperience(t, handler, cfg)
	statePath := "/v1/experiences/" + first.ExperienceID
	for range 30 {
		if r := request(t, handler, http.MethodGet, statePath, "", map[string]string{
			"Authorization": "Bearer " + first.Memory.ParticipantToken,
		}); r.Code != http.StatusOK {
			t.Fatalf("normal state request: %d %s", r.Code, r.Body.String())
		}
	}
	if r := request(t, handler, http.MethodGet, statePath, "", map[string]string{
		"Authorization": "Bearer " + first.Memory.ParticipantToken,
	}); r.Code != http.StatusTooManyRequests || r.Header().Get("Retry-After") == "" {
		t.Fatalf("excess state request: %d %s", r.Code, r.Body.String())
	}
}

func seedLegacyExperience(t *testing.T, db *store.Store, cfg config.Config) createdExperience {
	t.Helper()
	now := time.Now().UTC()
	created := createdExperience{ExperienceID: fmt.Sprintf("exp_legacy_%d", now.UnixNano())}
	created.Memory.SessionID = fmt.Sprintf("mem_legacy_%d", now.UnixNano())
	created.Live.SessionID = fmt.Sprintf("live_legacy_%d", now.UnixNano())
	created.Memory.ParticipantToken = "memory-" + created.Memory.SessionID
	created.Live.ParticipantToken = "live-" + created.Live.SessionID
	created.LinkToken = "link-" + created.ExperienceID
	created.Memory.ExpiresAt = now.Add(cfg.MemorySessionTTL)
	created.Memory.DeleteAfter = created.Memory.ExpiresAt.Add(cfg.DataRetention)
	created.Live.ExpiresAt = now.Add(cfg.LiveSessionTTL)
	created.Live.DeleteAfter = created.Live.ExpiresAt.Add(cfg.DataRetention)
	if err := db.CreateExperience(t.Context(), store.NewExperience{
		ExperienceID: created.ExperienceID, MemorySessionID: created.Memory.SessionID,
		LiveSessionID: created.Live.SessionID, LinkID: "link_" + created.ExperienceID,
		MemoryTokenHash: tokenHash(created.Memory.ParticipantToken),
		LiveTokenHash:   tokenHash(created.Live.ParticipantToken), LinkTokenHash: tokenHash(created.LinkToken),
		MemoryConsent: cfg.MemoryConsentVersion, LiveConsent: cfg.LiveConsentVersion,
		ConsentLanguage: cfg.ConsentLanguage, DisplayColor: "cyan", DisplaySymbol: "circle",
		CreatedAt: now, MemoryExpiresAt: created.Memory.ExpiresAt, MemoryDeleteAfter: created.Memory.DeleteAfter,
		LiveExpiresAt: created.Live.ExpiresAt, LiveDeleteAfter: created.Live.DeleteAfter, LivePresence: true,
	}); err != nil {
		t.Fatal(err)
	}
	return created
}

func createExperience(t *testing.T, handler http.Handler, cfg config.Config) createdExperience {
	t.Helper()
	body := fmt.Sprintf(`{"memoryConsentVersion":%q,"enableLivePresence":false,"consentLanguage":%q,"clientTime":%q}`,
		cfg.MemoryConsentVersion, cfg.ConsentLanguage, time.Now().UTC().Format(time.RFC3339Nano))
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
