package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type operatorActorKey struct{}
type operatorEmailKey struct{}

// WithOperatorActor carries a trusted, pseudonymous actor established by authentication.
func WithOperatorActor(ctx context.Context, actorID string) context.Context {
	return context.WithValue(ctx, operatorActorKey{}, actorID)
}

func OperatorActor(ctx context.Context) string {
	actor, _ := ctx.Value(operatorActorKey{}).(string)
	return actor
}

// WithOperatorIdentity attaches only the verified pseudonymous identity hashes.
func WithOperatorIdentity(ctx context.Context, actorID, emailHash string) context.Context {
	return context.WithValue(WithOperatorActor(ctx, actorID), operatorEmailKey{}, emailHash)
}

func OperatorEmailHash(ctx context.Context) string {
	emailHash, _ := ctx.Value(operatorEmailKey{}).(string)
	return emailHash
}

type OperatorCredential struct {
	ID, TokenHash, ActorID, EmailHash, Origin, Role string
	CreatedAt, ExpiresAt                            time.Time
}

// CreateOperatorCredential accepts hashes only, never Google identity or bearer material.
func (s *Store) CreateOperatorCredential(ctx context.Context, credential OperatorCredential) error {
	if OperatorActor(ctx) == "" || OperatorActor(ctx) != credential.ActorID {
		return ErrConflict
	}
	return s.Transaction(ctx, "operator:"+credential.ActorID, func(tx *Store) error {
		_, err := tx.q.ExecContext(ctx, `INSERT INTO operator_credentials
 (id,token_hash,actor_id,email_hash,origin,role,created_at,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			credential.ID, credential.TokenHash, credential.ActorID, credential.EmailHash, credential.Origin,
			credential.Role, credential.CreatedAt, credential.ExpiresAt)
		if err != nil {
			return err
		}
		return tx.auditOperator(ctx, "credential.issue", hashScope(credential.ID)[:12], "issued", credential.CreatedAt)
	})
}

func (s *Store) LookupOperatorCredential(ctx context.Context, tokenHash string, now time.Time) (OperatorCredential, error) {
	var c OperatorCredential
	err := s.q.QueryRowContext(ctx, `SELECT id,token_hash,actor_id,email_hash,origin,role,created_at,expires_at
 FROM operator_credentials WHERE token_hash=$1 AND expires_at>$2`, tokenHash, now).
		Scan(&c.ID, &c.TokenHash, &c.ActorID, &c.EmailHash, &c.Origin, &c.Role, &c.CreatedAt, &c.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// RevokeOperatorCredentials atomically removes the current credential or all of its actor's credentials.
func (s *Store) RevokeOperatorCredentials(ctx context.Context, currentID string, all bool, now time.Time) error {
	actor := OperatorActor(ctx)
	if actor == "" || currentID == "" {
		return ErrConflict
	}
	return s.Transaction(ctx, "operator:"+actor, func(tx *Store) error {
		var found string
		err := tx.q.QueryRowContext(ctx, `SELECT id FROM operator_credentials WHERE id=$1 AND actor_id=$2 AND expires_at>$3 FOR UPDATE`, currentID, actor, now).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		action, target := "credential.revoke", hashScope(currentID)[:12]
		if all {
			_, err = tx.q.ExecContext(ctx, `DELETE FROM operator_credentials WHERE actor_id=$1`, actor)
			action, target = "credential.revoke_all", "own_credentials"
		} else {
			_, err = tx.q.ExecContext(ctx, `DELETE FROM operator_credentials WHERE id=$1 AND actor_id=$2`, currentID, actor)
		}
		if err != nil {
			return err
		}
		return tx.auditOperator(ctx, action, target, "revoked", now)
	})
}

func (s *Store) CleanupOperatorCredentials(ctx context.Context, now time.Time) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM operator_credentials WHERE expires_at<=$1`, now)
	return err
}
