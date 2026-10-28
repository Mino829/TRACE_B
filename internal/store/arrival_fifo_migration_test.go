package store

import (
	"fmt"
	"testing"
	"time"
)

func TestFIFOQueueMigrationPreservesWaitingAndTerminalHistory(t *testing.T) {
	s, db := migrationTestStore(t)
	ctx := t.Context()
	if err := s.migrate(ctx, migrations[:12]); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	arrived := now.Add(-2 * time.Minute)
	for i, status := range []string{"queued", "completed", "skipped", "playing", "queued"} {
		memoryID := fmt.Sprintf("migration-memory-%d", i)
		if _, err := db.ExecContext(ctx, `INSERT INTO memory_sessions
 (id,consent_version,consent_language,token_hash,created_at,expires_at,delete_after)
 VALUES ($1,'memory','ja',$1,$2,$3,$4)`, memoryID, arrived, now.Add(time.Hour), now.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		var reason any
		if status == "skipped" {
			reason = "timeout"
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO arrival_events
 (id,memory_session_id,source,checkpoint_id,replay_status,skip_reason,arrived_at,photo_wait_until,replay_deadline)
 VALUES ($1,$2,'qr','booth',$3,$4,$5,$5,$6)`, fmt.Sprintf("migration-arrival-%d", i), memoryID, status, reason, arrived, arrived.Add(30*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"queued", "completed", "skipped", "playing", "queued"} {
		var status string
		if err := db.QueryRowContext(ctx, `SELECT replay_status FROM arrival_events WHERE id=$1`, fmt.Sprintf("migration-arrival-%d", i)).Scan(&status); err != nil || status != want {
			t.Fatalf("history %d: %s %v", i, status, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE arrival_events SET replay_status='playing' WHERE id='migration-arrival-4'`); err == nil {
		t.Fatal("concurrent playback accepted by database")
	}
	if _, err := db.ExecContext(ctx, `UPDATE arrival_events SET replay_status='completed' WHERE id='migration-arrival-3'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE arrival_events SET replay_status='playing' WHERE id='migration-arrival-4'`); err != nil {
		t.Fatalf("completed playback still blocks next: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM display_events`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("migration manufactured display events: %d %v", events, err)
	}
}
