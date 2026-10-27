package store

import (
	"testing"
	"time"
)

func TestPushIntervalMigrationPreservesExistingSubscription(t *testing.T) {
	db, inspect := migrationTestStore(t)
	ctx := t.Context()
	if err := db.migrate(ctx, migrations[:10]); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := inspect.ExecContext(ctx, `INSERT INTO memory_sessions
 (id,consent_version,consent_language,token_hash,created_at,expires_at,delete_after)
 VALUES ('existing-memory','old-consent','ja','old-token',$1,$2,$3)`, now, now.Add(time.Hour), now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	previous := now.Add(15 * time.Minute)
	_, err = inspect.ExecContext(ctx, `INSERT INTO push_subscriptions
 (memory_session_id,endpoint_encrypted,keys_encrypted,interval_minutes,status,next_send_at,expires_at)
 VALUES ('existing-memory',$1,$2,15,'active',$3,$4)`, []byte("encrypted-endpoint"), []byte("encrypted-keys"), previous, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var interval int
	var next time.Time
	var endpoint []byte
	if err := inspect.QueryRowContext(ctx, `SELECT interval_minutes,next_send_at,endpoint_encrypted
 FROM push_subscriptions WHERE memory_session_id='existing-memory'`).Scan(&interval, &next, &endpoint); err != nil {
		t.Fatal(err)
	}
	if interval != 30 || !next.Equal(previous.Add(15*time.Minute)) || string(endpoint) != "encrypted-endpoint" {
		t.Fatalf("migration altered retained subscription incorrectly: interval=%d next=%s", interval, next)
	}
	if _, err := inspect.ExecContext(ctx, `UPDATE push_subscriptions SET interval_minutes=15 WHERE memory_session_id='existing-memory'`); err == nil {
		t.Fatal("retired 15-minute delivery remains writable")
	}
}
