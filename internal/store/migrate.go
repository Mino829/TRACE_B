package store

import (
	"context"
	_ "embed"
	"errors"
	"strings"
)

//go:embed migrations/001_initial.sql
var initialSchema string

func (s *Store) Migrate(ctx context.Context) error {
	return s.Transaction(ctx, "trace:schema-migrations", func(tx *Store) error {
		if _, err := tx.q.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY,applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP)"); err != nil {
			return err
		}
		var latest int
		if err := tx.q.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&latest); err != nil {
			return err
		}
		if latest > 1 {
			return errors.New("database schema is newer than this application")
		}
		if latest == 1 {
			return nil
		}
		// Plain DDL only; this migration has no procedural bodies or semicolons in literals.
		for _, statement := range strings.Split(initialSchema, ";") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err := tx.q.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		_, err := tx.q.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES (1)")
		return err
	})
}
