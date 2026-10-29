package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func credentialHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func TestOperatorStoreRejectsMissingActorWithoutMutation(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC()
	if err := f.Store.CloseRegistration(t.Context(), now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("untrusted registration: %v", err)
	}
	if err := f.Store.StopNotifications(t.Context(), now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("untrusted notifications: %v", err)
	}
	if _, err := f.Store.SetFallback(t.Context(), true, now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("untrusted fallback: %v", err)
	}
	var closed, stopped bool
	var mode string
	var audits int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT (SELECT closed_at IS NOT NULL FROM registration_control),(SELECT stopped_at IS NOT NULL FROM notification_control),(SELECT mode FROM display_fallback_control),(SELECT COUNT(*) FROM arrival_operator_audit)`).Scan(&closed, &stopped, &mode, &audits); err != nil || closed || stopped || mode != "normal" || audits != 0 {
		t.Fatalf("unauthorized store changes: %t %t %s %d %v", closed, stopped, mode, audits, err)
	}
}

func TestOperatorCredentialAuditFailureRollsBackIssueAndRevocation(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC()
	actor := credentialHash("synthetic-google-actor")
	ctx := store.WithOperatorActor(t.Context(), actor)
	c := store.OperatorCredential{ID: "opc_atomic", TokenHash: credentialHash("synthetic-bearer"), ActorID: actor, EmailHash: credentialHash("synthetic-email"), Origin: "https://operator.example.com", Role: "operator", CreatedAt: now, ExpiresAt: now.Add(2 * time.Minute)}
	if err := f.Store.CreateOperatorCredential(t.Context(), c); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("untrusted issuance: %v", err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `ALTER TABLE arrival_operator_audit ADD CONSTRAINT reject_issue CHECK(action<>'credential.issue')`); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.CreateOperatorCredential(ctx, c); err == nil {
		t.Fatal("issue succeeded without audit")
	}
	if _, err := f.Store.LookupOperatorCredential(ctx, c.TokenHash, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("uncommitted issue persisted: %v", err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `ALTER TABLE arrival_operator_audit DROP CONSTRAINT reject_issue`); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.CreateOperatorCredential(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `ALTER TABLE arrival_operator_audit ADD CONSTRAINT reject_revoke CHECK(action NOT IN ('credential.revoke','credential.revoke_all'))`); err != nil {
		t.Fatal(err)
	}
	for _, all := range []bool{false, true} {
		if err := f.Store.RevokeOperatorCredentials(ctx, c.ID, all, now); err == nil {
			t.Fatal("revocation succeeded without audit")
		}
		if _, err := f.Store.LookupOperatorCredential(ctx, c.TokenHash, now); err != nil {
			t.Fatalf("failed revocation removed credential: %v", err)
		}
	}
	if err := f.Store.RevokeOperatorCredentials(store.WithOperatorActor(t.Context(), credentialHash("different actor")), c.ID, true, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign actor revocation: %v", err)
	}
}
