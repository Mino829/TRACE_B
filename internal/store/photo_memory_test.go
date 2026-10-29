package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/route"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// A second real database connection commits a mutation after the first result
// has been consumed, before the story's next read. No sleeps or production hooks.
type photoMemoryInterleave struct {
	mutate func() error
	err    error
}

func (p *photoMemoryInterleave) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (p *photoMemoryInterleave) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if p.mutate != nil && data.Err == nil && data.CommandTag.Select() {
		mutate := p.mutate
		p.mutate = nil
		p.err = mutate()
	}
}

func TestPhotoMomentMemorySnapshotDuringMutation(t *testing.T) {
	for _, change := range []string{"insert", "delete", "expiry"} {
		t.Run(change, func(t *testing.T) {
			writer, inspect := migrationTestStore(t)
			if err := writer.Migrate(t.Context()); err != nil {
				t.Fatal(err)
			}
			graph, err := route.Parse([]byte(`{"schemaVersion":1,"campusGraphVersion":"g1","coordinateVersion":"c1","threeDMapVersion":"m1","nodes":[` +
				`{"id":"a","lat":35,"lon":139,"local":{"east":0,"north":0},"selectable":true},` +
				`{"id":"b","lat":35,"lon":139.0004387,"local":{"east":40,"north":0},"selectable":true}],` +
				`"edges":[{"id":"ab","from":"a","to":"b","kind":"ground","lengthMetres":40,"passable":true}]}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.ConfigureRoutes(graph, "c1", "g1", "m1"); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			const memoryID = "snapshot-memory"
			if err := writer.CreateExperience(t.Context(), NewExperience{
				ExperienceID: "snapshot-experience", MemorySessionID: memoryID, MemoryTokenHash: "snapshot-token",
				MemoryConsent: "memory-v1", ConsentLanguage: "ja", PhotoMode: PhotoModeDevice,
				DisplayColor: "cyan", DisplaySymbol: "circle", CampusGraphVersion: "g1", CreatedAt: now,
				MemoryExpiresAt: now.Add(time.Hour), MemoryDeleteAfter: now.Add(2 * time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			middle := PhotoMoment{ID: "middle", MemorySessionID: memoryID, ClientPhotoID: "client-middle",
				RequestHash: "request-middle", CheckpointID: "b", CapturedAt: now.Add(time.Minute)}
			for _, p := range []PhotoMoment{
				{ID: "first", MemorySessionID: memoryID, ClientPhotoID: "client-first", RequestHash: "request-first", CheckpointID: "a", CapturedAt: now},
				{ID: "last", MemorySessionID: memoryID, ClientPhotoID: "client-last", RequestHash: "request-last", CheckpointID: "b", CapturedAt: now.Add(2 * time.Minute)},
			} {
				if _, _, err := writer.CreatePhotoMoment(t.Context(), p, now); err != nil {
					t.Fatal(err)
				}
			}
			before, after := []string{"first", "last"}, []string{"first", "middle", "last"}
			interleave := &photoMemoryInterleave{}
			switch change {
			case "insert":
				interleave.mutate = func() error {
					_, _, err := writer.CreatePhotoMoment(t.Context(), middle, now)
					return err
				}
			case "delete":
				if _, _, err := writer.CreatePhotoMoment(t.Context(), middle, now); err != nil {
					t.Fatal(err)
				}
				before, after = after, before
				interleave.mutate = func() error { return writer.DeletePhotoMoment(t.Context(), memoryID, middle.ID, now) }
			case "expiry":
				after = nil
				interleave.mutate = func() error { return writer.Cleanup(t.Context(), now.Add(3*time.Hour)) }
			}
			cfg, err := pgx.ParseConfig(os.Getenv("TRACE_TEST_DATABASE_URL"))
			if err != nil {
				t.Fatal("invalid isolated PostgreSQL test configuration")
			}
			var schema string
			if err := inspect.QueryRowContext(t.Context(), "SELECT current_schema()").Scan(&schema); err != nil {
				t.Fatal(err)
			}
			cfg.RuntimeParams["search_path"] = schema
			cfg.RuntimeParams["statement_timeout"] = "10000"
			cfg.Tracer = interleave
			db := stdlib.OpenDB(*cfg)
			t.Cleanup(func() { db.Close() })
			reader := &Store{db: db, q: db}
			moments, segments, err := reader.PhotoMomentMemory(t.Context(), memoryID)
			if interleave.err != nil {
				t.Fatal("interleaved mutation:", interleave.err)
			}
			if err != nil {
				t.Fatal(err)
			}
			assertPhotoMemoryAdjacency(t, moments, segments, before)
			moments, segments, err = reader.PhotoMomentMemory(t.Context(), memoryID)
			if err != nil {
				t.Fatal(err)
			}
			assertPhotoMemoryAdjacency(t, moments, segments, after)
		})
	}
}

func assertPhotoMemoryAdjacency(t *testing.T, moments []PhotoMoment, segments []RouteSegment, want []string) {
	t.Helper()
	if len(moments) != len(want) || len(segments) != max(0, len(want)-1) {
		t.Fatalf("story is not one committed state: moments=%v segments=%+v want=%v", moments, segments, want)
	}
	for i, id := range want {
		if moments[i].ID != id {
			t.Fatalf("photo order[%d]=%q, want %q", i, moments[i].ID, id)
		}
		if i < len(segments) {
			segment := segments[i]
			if segment.FromPhotoMomentID != id || segment.ToPhotoMomentID == nil || *segment.ToPhotoMomentID != want[i+1] {
				t.Fatalf("route does not join adjacent returned photos: %+v, want %s -> %s", segment, id, want[i+1])
			}
		}
	}
}
