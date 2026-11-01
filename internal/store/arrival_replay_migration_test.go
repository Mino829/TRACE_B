package store

import (
	"strings"
	"testing"
	"time"
)

func TestReplayMigrationBackfillsPublicGenerationAndFIFOWithRequiredColumns(t *testing.T) {
	s, db := migrationTestStore(t)
	ctx := t.Context()
	if err := s.migrate(ctx, migrations[:18]); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.ExecContext(ctx, `INSERT INTO memory_sessions
 (id,consent_version,consent_language,token_hash,created_at,expires_at,delete_after)
 VALUES ('repeat-memory','v1','ja','hash',$1,$2,$3)`, now, now.Add(time.Hour), now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// This fixture intentionally targets the pre-019 schema.
	if _, err := db.ExecContext(ctx, `INSERT INTO arrival_events
 (id,memory_session_id,source,checkpoint_id,replay_status,arrived_at,photo_wait_until)
 VALUES ('root','repeat-memory','qr','booth','completed',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var replay string
	var queued time.Time
	if err := db.QueryRowContext(ctx, `SELECT replay_id,queued_at FROM arrival_events WHERE id='root'`).Scan(&replay, &queued); err != nil || replay != "root" || !queued.Equal(now) {
		t.Fatalf("backfill: %s %v %v", replay, queued, err)
	}
	for _, column := range []string{"replay_id", "queued_at"} {
		var nullable string
		if err := db.QueryRowContext(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='arrival_events' AND column_name=$1`, column).Scan(&nullable); err != nil || nullable != "NO" {
			t.Fatalf("required %s: %s %v", column, nullable, err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO arrival_proof_uses(memory_session_id,proof_fingerprint) VALUES ('repeat-memory',$1)`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO arrival_replay_receipts(memory_session_id,key_hash,request_hash,response_body) VALUES ('repeat-memory','key','request',$1)`, []byte(`{"arrivalId":"new","status":"queued"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM memory_sessions WHERE id='repeat-memory'`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"arrival_events", "arrival_proof_uses", "arrival_replay_receipts"} {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("cascade %s: %d %v", table, count, err)
		}
	}
}
