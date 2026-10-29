package api_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/photo"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func TestPhotoReceiptConflictsAndSessionBoundLifecycleAcrossAPIVersions(t *testing.T) {
	for _, device := range []bool{false, true} {
		version, createKind, deleteKind := "v1", "photo", "photo"
		if device {
			version, createKind, deleteKind = "v2", "moment", "moment"
		}
		t.Run(version, func(t *testing.T) {
			f := testdb.Open(t)
			cfg := config.Default()
			cfg.RegistrationOpen = true
			cfg.PhotoMetadataEnabled, cfg.PhotoEnabled = device, !device
			photoGraphFixture(t, f, &cfg)
			var service *photo.Service
			if !device {
				service = photo.New(f.Store, newPhotoObjects(t))
			}
			h := api.New(cfg, f.Store, service)
			owner := createExperience(t, h, cfg)
			now := time.Now().UTC().Truncate(time.Microsecond)
			deadline := now.Add(2 * time.Hour)
			if _, err := f.SQL.ExecContext(t.Context(), `UPDATE memory_sessions SET expires_at=$2,delete_after=$3 WHERE id=$1`, owner.Memory.SessionID, now.Add(time.Hour), deadline); err != nil {
				t.Fatal(err)
			}
			path := "/" + version + "/memory-sessions/" + owner.Memory.SessionID + "/photos"
			bodies := []string{devicePhotoBody("one", "cp-a", now), devicePhotoBody("two", "cp-b", now)}
			if !device {
				data := photoJPEG(t)
				bodies = []string{photoRegistrationBody(t, data, "one", "cp-a"), photoRegistrationBody(t, data, "two", "cp-b")}
			}
			headers := map[string]string{"Authorization": "Bearer " + owner.Memory.ParticipantToken, "Idempotency-Key": "racing-photo-key"}
			type result struct {
				code int
				body string
			}
			results := make(chan result, 2)
			start := make(chan struct{})
			var workers sync.WaitGroup
			for _, body := range bodies {
				workers.Add(1)
				go func() {
					defer workers.Done()
					<-start
					response := request(t, api.New(cfg, f.Store, service), "POST", path, body, headers)
					results <- result{response.Code, response.Body.String()}
				}()
			}
			close(start)
			workers.Wait()
			close(results)
			var photoID string
			created, conflicts := 0, 0
			for response := range results {
				switch response.code {
				case 201:
					created++
					var value struct {
						ID string `json:"photoMomentId"`
					}
					if err := json.Unmarshal([]byte(response.body), &value); err != nil || value.ID == "" {
						t.Fatalf("created photo: %s %v", response.body, err)
					}
					photoID = value.ID
				case 409:
					conflicts++
					if !strings.Contains(response.body, `"IDEMPOTENCY_CONFLICT"`) {
						t.Fatalf("key/body race misclassified: %s", response.body)
					}
				default:
					t.Fatalf("photo key race: %d %s", response.code, response.body)
				}
			}
			if created != 1 || conflicts != 1 {
				t.Fatalf("photo key race created=%d conflicts=%d", created, conflicts)
			}
			deleteHeaders := map[string]string{"Authorization": headers["Authorization"], "Idempotency-Key": "delete-photo"}
			for range 2 {
				if response := request(t, h, "DELETE", path+"/"+photoID, "", deleteHeaders); response.Code != 204 {
					t.Fatalf("photo delete/replay: %d %s", response.Code, response.Body.String())
				}
			}
			scopes := []string{tokenHash("create:" + createKind + ":" + owner.Memory.SessionID), tokenHash("delete:" + deleteKind + ":" + owner.Memory.SessionID + ":" + photoID)}
			for _, scope := range scopes {
				var memoryID string
				var expires time.Time
				if err := f.SQL.QueryRowContext(t.Context(), `SELECT memory_session_id,expires_at FROM idempotency_records WHERE scope=$1`, scope).Scan(&memoryID, &expires); err != nil || memoryID != owner.Memory.SessionID || !expires.Equal(deadline) {
					t.Fatalf("photo receipt owner/deadline: %q %v %v", memoryID, expires, err)
				}
			}
			wholePath := "/v1/memory-sessions/" + owner.Memory.SessionID
			wholeHeaders := map[string]string{"Authorization": headers["Authorization"], "Idempotency-Key": "delete-memory"}
			for range 2 {
				if response := request(t, h, "DELETE", wholePath, "", wholeHeaders); response.Code != 204 {
					t.Fatalf("whole Memory delete/replay: %d %s", response.Code, response.Body.String())
				}
			}
			var count int
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM idempotency_records WHERE scope IN ($1,$2)`, scopes[0], scopes[1]).Scan(&count); err != nil || count != 0 {
				t.Fatalf("whole delete retained photo receipts: %d %v", count, err)
			}
			var body []byte
			var bound *string
			var expires time.Time
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT response_body,memory_session_id,expires_at FROM idempotency_records WHERE scope=$1`, tokenHash("delete:memory:"+owner.Memory.SessionID)).Scan(&body, &bound, &expires); err != nil || len(body) != 0 || bound != nil || !expires.After(now.Add(23*time.Hour)) || expires.After(time.Now().UTC().Add(24*time.Hour)) {
				t.Fatalf("whole-delete exception content/deadline: body=%s bound=%v expires=%v err=%v", body, bound, expires, err)
			}
			if response := request(t, h, "DELETE", path+"/"+photoID, "", deleteHeaders); response.Code != 401 {
				t.Fatalf("photo receipt survived owner deletion: %d %s", response.Code, response.Body.String())
			}
		})
	}
}
