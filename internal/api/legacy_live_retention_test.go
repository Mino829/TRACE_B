package api_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func TestLegacyLiveStopAfterExpiryDoesNotExtendOwnerWindow(t *testing.T) {
	f := testdb.Open(t)
	cfg := config.Default()
	h := api.New(cfg, f.Store, nil)
	owner := seedLegacyExperience(t, f.Store, cfg)
	if err := f.Store.DeleteMemory(t.Context(), owner.Memory.SessionID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE live_presence_sessions SET expires_at=$2,tracking_status='expired',presence_phase='ended' WHERE id=$1`, owner.Live.SessionID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	path := "/v1/live-presence-sessions/" + owner.Live.SessionID
	headers := map[string]string{"Authorization": "Bearer " + owner.Live.ParticipantToken, "Idempotency-Key": "expired-live-stop"}
	if response := request(t, h, "GET", path, "", headers); response.Code != 200 || !strings.Contains(response.Body.String(), `"trackingStatus":"expired"`) {
		t.Fatalf("expired legacy state: %d %s", response.Code, response.Body.String())
	}
	if response := request(t, h, "GET", path, "", map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken}); response.Code != 401 {
		t.Fatalf("Memory bearer authorized retained Live state: %d %s", response.Code, response.Body.String())
	}
	var first string
	for range 2 {
		response := request(t, h, "POST", path+"/stop", `{"reason":"participant_request"}`, headers)
		if response.Code != 200 || (first != "" && first != response.Body.String()) || !strings.Contains(response.Body.String(), `"trackingStatus":"stopped"`) {
			t.Fatalf("expired legacy stop/replay: %d %s", response.Code, response.Body.String())
		}
		first = response.Body.String()
	}
	var expiresAt, deleteAfter time.Time
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT expires_at,delete_after FROM live_presence_sessions WHERE id=$1`, owner.Live.SessionID).Scan(&expiresAt, &deleteAfter); err != nil || !expiresAt.Equal(now.Add(-time.Minute)) || !deleteAfter.Equal(owner.Live.DeleteAfter.Truncate(time.Microsecond)) {
		t.Fatalf("legacy stop changed deadlines: %v %v %v", expiresAt, deleteAfter, err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE live_presence_sessions SET delete_after=$2 WHERE id=$1`, owner.Live.SessionID, now.Add(-time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	if response := request(t, h, "POST", path+"/stop", `{"reason":"participant_request"}`, headers); response.Code != 401 {
		t.Fatalf("stop receipt extended owner window: %d %s", response.Code, response.Body.String())
	}
	if response := request(t, h, "GET", path, "", headers); response.Code != 401 {
		t.Fatalf("legacy read survived owner window: %d %s", response.Code, response.Body.String())
	}
}
