package api_test

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/api"
	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/photo"
	"github.com/Mino829/trace_Backend/internal/route"
	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func TestPhotoMemoryDeliversPrivateVersionedProjectedRoutes(t *testing.T) {
	f := testdb.Open(t)
	lonStep := 20 / (6378137 * math.Cos(35*math.Pi/180) * math.Pi / 180)
	raw := fmt.Sprintf(`{"schemaVersion":1,"coordinateVersion":"c1","campusGraphVersion":"g1","threeDMapVersion":"m1","nodes":[{"id":"a","lat":35,"lon":139,"local":{"east":0,"north":0},"gpsSnap":true},{"id":"b","lat":35,"lon":%.9f,"local":{"east":20,"north":0},"gpsSnap":true},{"id":"c","lat":35,"lon":%.9f,"local":{"east":40,"north":0},"gpsSnap":true}],"edges":[{"id":"ab","from":"a","to":"b","lengthMetres":20,"passable":true},{"id":"bc","from":"b","to":"c","lengthMetres":20,"passable":true}]}`, 139+lonStep, 139+lonStep*2)
	g, err := route.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.PhotoEnabled = true
	cfg.CoordinateVersion, cfg.CampusGraphVersion, cfg.ThreeDMapVersion = "c1", "g1", "m1"
	if err := f.Store.ConfigureRoutes(g, cfg.CoordinateVersion, cfg.CampusGraphVersion, cfg.ThreeDMapVersion); err != nil {
		t.Fatal(err)
	}
	assets := newPhotoObjects(t)
	h := api.New(cfg, f.Store, photo.New(f.Store, assets))
	c := createExperience(t, h, cfg)
	now := time.Now().UTC()
	for i, x := range []float64{5, 30} {
		name := fmt.Sprintf("route-photo-%d", i)
		p := store.Photo{ID: name, MemorySessionID: c.Memory.SessionID, ClientPhotoID: name, RequestHash: name,
			CapturedAt: now.Add(time.Duration(i) * time.Minute), UploadExpiresAt: now.Add(4 * time.Minute),
			Location: &store.PhotoLocation{Latitude: 35, Longitude: 139 + lonStep*x/20, AccuracyM: 1, RecordedAt: now},
			MIMEType: "image/jpeg", SizeBytes: 500, Width: 16, Height: 12, SHA256: "digest", StageKey: "stage-" + name, ObjectKey: "object-" + name}
		if _, err := f.Store.CreatePhoto(t.Context(), p, now); err != nil {
			t.Fatal(err)
		}
		if err := f.Store.ReadyPhoto(t.Context(), c.Memory.SessionID, name, now); err != nil {
			t.Fatal(err)
		}
	}
	r := request(t, h, "GET", "/v1/memory-sessions/"+c.Memory.SessionID+"/memory", "", map[string]string{"Authorization": "Bearer " + c.Memory.ParticipantToken})
	if r.Code != 200 {
		t.Fatalf("memory response: %d %s", r.Code, r.Body.String())
	}
	var body struct {
		Segments []store.RouteSegment `json:"segments"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil || len(body.Segments) != 1 {
		t.Fatalf("segment response: %s %v", r.Body.String(), err)
	}
	s := body.Segments[0]
	if s.Confidence == "low" || s.AlgorithmVersion != route.AlgorithmVersion || s.CampusGraphVersion != "g1" || len(s.Path) < 2 || strings.Contains(r.Body.String(), `"latitude"`) || strings.Contains(r.Body.String(), `"longitude"`) {
		t.Fatalf("story omitted graph path or leaked raw GPS: %s", r.Body.String())
	}
}
