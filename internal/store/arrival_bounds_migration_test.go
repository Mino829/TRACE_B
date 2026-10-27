package store

import (
	"fmt"
	"testing"
	"time"
)

func TestArrivalBoundsMigrationPreservesArrivalAndDisplayState(t *testing.T) {
	store, db := migrationTestStore(t)
	ctx := t.Context()
	if err := store.migrate(ctx, migrations[:11]); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.ExecContext(ctx, "UPDATE display_cursor SET last_id=1 WHERE singleton=TRUE"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO display_events(id,kind,data,occurred_at)
 VALUES (1,'existing.event','{}',$1)`, now); err != nil {
		t.Fatal(err)
	}
	for i := range 14 {
		memoryID := fmt.Sprintf("old-memory-%d", i)
		arrived := now.Add(-time.Second)
		if i < 2 {
			arrived = now.Add(-3 * time.Minute)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO memory_sessions
 (id,consent_version,consent_language,token_hash,created_at,expires_at,delete_after)
 VALUES ($1,'old-consent','ja',$1,$2,$3,$4)`, memoryID, arrived, now.Add(time.Hour), now.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO arrival_events
 (id,memory_session_id,source,checkpoint_id,replay_status,arrived_at,photo_wait_until,replay_deadline)
 VALUES ($1,$2,'qr','booth','queued',$3,$4,$5)`, fmt.Sprintf("old-arrival-%02d", i), memoryID,
			arrived, arrived.Add(15*time.Second), arrived.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.migrate(ctx, migrations[:12]); err != nil {
		t.Fatal(err)
	}
	var total, queued, timedOut, full, events int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE replay_status='queued'),
 COUNT(*) FILTER (WHERE skip_reason='timeout'), COUNT(*) FILTER (WHERE skip_reason='queue_full')
 FROM arrival_events`).Scan(&total, &queued, &timedOut, &full); err != nil {
		t.Fatal(err)
	}
	if total != 14 || queued != 10 || timedOut != 2 || full != 2 {
		t.Fatalf("arrival migration: total=%d queued=%d timeout=%d full=%d", total, queued, timedOut, full)
	}
	var deadline time.Time
	if err := db.QueryRowContext(ctx, "SELECT replay_deadline FROM arrival_events WHERE id='old-arrival-02'").Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	if !deadline.Equal(now.Add(29 * time.Second)) {
		t.Fatalf("first unexpired arrival deadline: %s", deadline)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM display_events
 WHERE kind='arrival.replay.skipped' AND data->>'reason' IN ('timeout','queue_full')`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 4 {
		t.Fatalf("skipped render events: %d", events)
	}
	var cursor, firstSkippedID int
	if err := db.QueryRowContext(ctx, "SELECT last_id FROM display_cursor WHERE singleton=TRUE").Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT MIN(id) FROM display_events WHERE kind='arrival.replay.skipped'").Scan(&firstSkippedID); err != nil {
		t.Fatal(err)
	}
	if cursor != 5 || firstSkippedID != 2 {
		t.Fatalf("render cursor continuity: cursor=%d first skip=%d", cursor, firstSkippedID)
	}
	if err := store.migrate(ctx, migrations[:12]); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM display_events").Scan(&events); err != nil || events != 5 {
		t.Fatalf("repeated migration emitted %d render events: %v", events, err)
	}
}
