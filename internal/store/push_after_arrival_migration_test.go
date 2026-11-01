package store

import (
	"testing"
	"time"
)

func TestPushAfterArrivalMigrationDefaultsFalseWithoutPromotingExistingParticipants(t *testing.T) {
	s, db := migrationTestStore(t)
	ctx := t.Context()
	if err := s.migrate(ctx, migrations[:19]); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for _, participant := range []struct {
		id, consent string
		stopped     bool
	}{
		{"old-active", "2026-10-31-v1", false},
		{"old-arrived", "2026-10-31-v1", true},
		{"matching-new-version", "reviewed-memory", false},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO memory_sessions
 (id,consent_version,consent_language,token_hash,created_at,expires_at,delete_after,push_stopped)
 VALUES ($1,$2,'ja',$1,$3,$4,$5,$6)`, participant.id, participant.consent, now, now.Add(time.Hour), now.Add(2*time.Hour), participant.stopped); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO arrival_events
 (id,replay_id,memory_session_id,source,checkpoint_id,replay_status,arrived_at,queued_at,photo_wait_until)
 VALUES ('old-arrival','old-arrival','old-arrived','qr','booth','queued',$1,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO push_subscriptions
 (memory_session_id,endpoint_encrypted,keys_encrypted,interval_minutes,status,next_send_at,expires_at)
 VALUES ('old-active',$1,$2,30,'active',$3,$4)`, []byte("endpoint"), []byte("keys"), now.Add(30*time.Minute), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old-active", "old-arrived", "matching-new-version"} {
		var policy, stopped bool
		var consent string
		var expires, deleteAfter time.Time
		if err := db.QueryRowContext(ctx, `SELECT push_after_arrival,push_stopped,consent_version,expires_at,delete_after
 FROM memory_sessions WHERE id=$1`, id).Scan(&policy, &stopped, &consent, &expires, &deleteAfter); err != nil || policy || stopped != (id == "old-arrived") || !expires.Equal(now.Add(time.Hour)) || !deleteAfter.Equal(now.Add(2*time.Hour)) {
			t.Fatalf("migration changed participant %s: policy=%t stopped=%t consent=%s expires=%v deleteAfter=%v err=%v", id, policy, stopped, consent, expires, deleteAfter, err)
		}
		wantConsent := "2026-10-31-v1"
		if id == "matching-new-version" {
			wantConsent = "reviewed-memory"
		}
		if consent != wantConsent {
			t.Fatalf("migration rewrote consent: %s", consent)
		}
	}
	var endpoint, keys []byte
	var next time.Time
	if err := db.QueryRowContext(ctx, `SELECT endpoint_encrypted,keys_encrypted,next_send_at FROM push_subscriptions WHERE memory_session_id='old-active'`).Scan(&endpoint, &keys, &next); err != nil || string(endpoint) != "endpoint" || string(keys) != "keys" || !next.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("migration changed existing subscription: endpoint=%q keys=%q next=%v err=%v", endpoint, keys, next, err)
	}
	// A writer omitting the new column must not extend its participants' consent.
	if _, err := db.ExecContext(ctx, `INSERT INTO memory_sessions
 (id,consent_version,consent_language,token_hash,created_at,expires_at,delete_after)
 VALUES ('old-writer','reviewed-memory','ja','old-token',$1,$2,$3)`, now, now.Add(time.Hour), now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var policy bool
	if err := db.QueryRowContext(ctx, `SELECT push_after_arrival FROM memory_sessions WHERE id='old-writer'`).Scan(&policy); err != nil || policy {
		t.Fatalf("old writer opted participant in: policy=%t err=%v", policy, err)
	}
	var nullable string
	if err := db.QueryRowContext(ctx, `SELECT is_nullable FROM information_schema.columns
 WHERE table_schema=current_schema() AND table_name='memory_sessions' AND column_name='push_after_arrival'`).Scan(&nullable); err != nil || nullable != "NO" {
		t.Fatalf("policy column must be required: nullable=%s err=%v", nullable, err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT push_after_arrival FROM memory_sessions WHERE id='old-writer'`).Scan(&policy); err != nil || policy {
		t.Fatalf("repeated migration promoted old writer: policy=%t err=%v", policy, err)
	}
}
