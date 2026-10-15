package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version int
	sql     string
}

var migrations, migrationsErr = loadMigrations(migrationFiles)

func loadMigrations(files fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(files, "migrations")
	if err != nil {
		return nil, err
	}
	var result []migration
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		if entry.IsDir() || len(name) < 9 || name[3] != '_' {
			return nil, fmt.Errorf("invalid migration filename: %s", name)
		}
		version, err := strconv.Atoi(name[:3])
		if err != nil || version != len(result)+1 {
			return nil, fmt.Errorf("migration sequence must start at 001 with no gaps or duplicates: %s", name)
		}
		data, err := fs.ReadFile(files, "migrations/"+name)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(data)) == "" {
			return nil, fmt.Errorf("empty migration: %s", name)
		}
		result = append(result, migration{version: version, sql: string(data)})
	}
	if len(result) == 0 {
		return nil, errors.New("no database migrations")
	}
	return result, nil
}

func schemaVersion(ctx context.Context, q executor) (count, oldest, newest int, err error) {
	err = q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(version), 0),
		COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&count, &oldest, &newest)
	return
}

func (s *Store) Migrate(ctx context.Context) error {
	if migrationsErr != nil {
		return migrationsErr
	}
	return s.migrate(ctx, migrations)
}

func (s *Store) migrate(ctx context.Context, steps []migration) error {
	return s.Transaction(ctx, "trace:schema-migrations", func(tx *Store) error {
		if _, err := tx.q.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY,applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP)"); err != nil {
			return err
		}
		count, oldest, newest, err := schemaVersion(ctx, tx.q)
		if err != nil {
			return err
		}
		if count > len(steps) {
			return errors.New("database schema is newer than this application")
		}
		if count != 0 && (oldest != 1 || newest != count) {
			return errors.New("database schema has missing or invalid migration versions")
		}
		for _, step := range steps[count:] {
			// Simple protocol permits complete SQL files (including procedural bodies)
			// without splitting on semicolons inside strings or functions.
			if _, err := tx.q.ExecContext(ctx, step.sql, pgx.QueryExecModeSimpleProtocol); err != nil {
				return fmt.Errorf("migration %03d: %w", step.version, err)
			}
			if _, err := tx.q.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", step.version); err != nil {
				return err
			}
		}
		return nil
	})
}
