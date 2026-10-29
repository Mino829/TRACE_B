package store

import (
	"database/sql"
	"testing"
	"time"
)

func TestMemoryStopMigrationBindsIdentifiablePhotoReceiptsWithoutDestroyingHistory(t *testing.T) {
	s, db := migrationTestStore(t)
	ctx := t.Context()
	if err := s.migrate(ctx, migrations[:16]); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	deadline := now.Add(2 * time.Hour)
	if _, err := db.ExecContext(ctx, `INSERT INTO memory_sessions
 (id,consent_version,consent_language,token_hash,created_at,expires_at,delete_after)
 VALUES ('migration-memory','memory','ja','owner',$1,$2,$3)`, now, now.Add(time.Hour), deadline); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO photos
 (id,id_hash,memory_session_id,client_photo_id,request_hash,captured_at,created_at,delete_after,upload_expires_at,
 mime_type,sha256,size_bytes,width,height,sequence_no,upload_attempts,status)
 VALUES ('legacy-photo','legacy-hash','migration-memory','legacy-client','legacy-request',$1,$1,$2,$1,
 'image/jpeg','legacy-content-hash',1,1,1,1,1,'failed')`, now, deadline); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO photo_moments
 (id,id_hash,memory_session_id,client_photo_id,request_hash,checkpoint_id,campus_graph_version,captured_at,sequence_no)
 VALUES ('device-photo','device-hash','migration-memory','device-client','device-request','cp-a','g1',$1,2)`, now); err != nil {
		t.Fatal(err)
	}
	photoScopes := []string{"create:photo:migration-memory", "create:moment:migration-memory", "delete:photo:migration-memory:legacy-photo", "delete:moment:migration-memory:device-photo"}
	exceptionScopes := []string{"delete:memory:migration-memory", "delete:experience:live:old-experience", "delete:experience:memory:old-experience", "unidentifiable-old-photo-delete"}
	for _, scope := range append(append([]string{}, photoScopes...), exceptionScopes...) {
		if err := s.SaveIdempotency(ctx, hashScope(scope), "key", "credential-or-request-hash", 204, []byte{}, now.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	memory, err := s.MemorySession(ctx, "migration-memory")
	if err != nil || memory.StoppedAt != nil || !memory.DeleteAfter.Equal(deadline) {
		t.Fatalf("migration changed participant lifetime: %+v %v", memory, err)
	}
	for _, scope := range photoScopes {
		var owner string
		var expires time.Time
		if err := db.QueryRowContext(ctx, `SELECT memory_session_id,expires_at FROM idempotency_records WHERE scope=$1`, hashScope(scope)).Scan(&owner, &expires); err != nil || owner != memory.ID || !expires.Equal(deadline) {
			t.Fatalf("photo receipt migration %s: %q %v %v", scope, owner, expires, err)
		}
	}
	for _, scope := range exceptionScopes {
		var owner sql.NullString
		var expires time.Time
		var body []byte
		if err := db.QueryRowContext(ctx, `SELECT memory_session_id,expires_at,response_body FROM idempotency_records WHERE scope=$1`, hashScope(scope)).Scan(&owner, &expires, &body); err != nil || owner.Valid || !expires.Equal(now.Add(24*time.Hour)) || len(body) != 0 {
			t.Fatalf("legacy receipt retention changed %s: %v %v %s %v", scope, owner, expires, body, err)
		}
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM photos WHERE memory_session_id='migration-memory'`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMemory(ctx, memory.ID); err != nil {
		t.Fatal(err)
	}
	for _, scope := range photoScopes {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM idempotency_records WHERE scope=$1`, hashScope(scope)).Scan(&count); err != nil || count != 0 {
			t.Fatalf("deleted owner retained photo receipt %s: %d %v", scope, count, err)
		}
	}
	for _, scope := range exceptionScopes {
		if _, err := s.GetIdempotency(ctx, hashScope(scope), "key", now); err != nil {
			t.Fatalf("whole/old-delete receipt lost %s: %v", scope, err)
		}
	}
}
