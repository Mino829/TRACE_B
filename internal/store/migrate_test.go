package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func migrationTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("TRACE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PostgreSQL integration test: set TRACE_TEST_DATABASE_URL (see README)")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("TRACE_TEST_DATABASE_URL must be a PostgreSQL URL")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "trace_migration_test_" + hex.EncodeToString(random[:])
	ctx := t.Context()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	store, err := Open(ctx, u.String(), 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	inspect, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inspect.Close() })
	return store, inspect
}

func migrationSteps(t *testing.T, extras ...string) []migration {
	t.Helper()
	initial, err := fs.ReadFile(migrationFiles, "migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{"migrations/001_initial.sql": &fstest.MapFile{Data: initial}}
	for i, statement := range extras {
		name := fmt.Sprintf("migrations/%03d_test.sql", i+2)
		files[name] = &fstest.MapFile{Data: []byte(statement)}
	}
	steps, err := loadMigrations(files)
	if err != nil {
		t.Fatal(err)
	}
	return steps
}

func TestMigrationIncrementalAndReadiness(t *testing.T) {
	store, db := migrationTestStore(t)
	ctx := t.Context()
	if err := store.Ping(ctx); err == nil {
		t.Fatal("unmigrated DB reported ready")
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	steps := migrationSteps(t,
		"CREATE TABLE migration_probe (id integer PRIMARY KEY); INSERT INTO migration_probe VALUES (1);",
		"CREATE FUNCTION migration_label() RETURNS text LANGUAGE sql AS $$ SELECT 'a;b'::text $$; CREATE TABLE migration_events (label text NOT NULL);",
	)
	if err := store.migrate(ctx, steps); err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx, steps); err != nil {
		t.Fatal("repeat migration:", err)
	}
	var count, probe int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil || count != 3 {
		t.Fatalf("migration history: count=%d err=%v", count, err)
	}
	if err := db.QueryRowContext(ctx, "SELECT id FROM migration_probe").Scan(&probe); err != nil || probe != 1 {
		t.Fatalf("new schema/data: id=%d err=%v", probe, err)
	}
	var label string
	if err := db.QueryRowContext(ctx, "SELECT migration_label()").Scan(&label); err != nil || label != "a;b" {
		t.Fatalf("procedural SQL: label=%q err=%v", label, err)
	}
	if err := store.Ping(ctx); err == nil {
		t.Fatal("older application reported ready against a newer schema")
	}
	if err := store.pingVersion(ctx, 3); err != nil {
		t.Fatal("matching application should be ready:", err)
	}
	if err := store.migrate(ctx, steps[:2]); err == nil {
		t.Fatal("older migration binary accepted a newer schema")
	}
}

func TestFirstMigrationFailureLeavesNoSchema(t *testing.T) {
	store, db := migrationTestStore(t)
	ctx := t.Context()
	bad := migrationSteps(t, "CREATE TABLE migration_probe (missing bogus_type);")
	if err := store.migrate(ctx, bad); err == nil {
		t.Fatal("broken initial migration committed")
	}
	for _, name := range []string{"schema_migrations", "memory_sessions", "migration_probe"} {
		var table sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT to_regclass($1)", name).Scan(&table); err != nil || table.Valid {
			t.Fatalf("partial initial schema %s: table=%v err=%v", name, table, err)
		}
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal("retry after initial failure:", err)
	}
}

func TestMigrationFailureRollsBackAndRetries(t *testing.T) {
	store, db := migrationTestStore(t)
	ctx := t.Context()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	bad := migrationSteps(t, "CREATE TABLE migration_probe (id integer PRIMARY KEY);", "CREATE TABLE migration_events (missing bogus_type);")
	if err := store.migrate(ctx, bad); err == nil {
		t.Fatal("broken migration committed")
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration history changed: count=%d err=%v", count, err)
	}
	var table sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT to_regclass('migration_probe')").Scan(&table); err != nil || table.Valid {
		t.Fatalf("partial DDL survived: table=%v err=%v", table, err)
	}
	if err := store.Ping(ctx); err != nil {
		t.Fatal("previous schema not usable after rollback:", err)
	}
	fixed := migrationSteps(t, "CREATE TABLE migration_probe (id integer PRIMARY KEY);", "CREATE TABLE migration_events (label text NOT NULL);")
	if err := store.migrate(ctx, fixed); err != nil {
		t.Fatal("retry:", err)
	}
	if err := store.pingVersion(ctx, 3); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentMigrationAndCorruptHistory(t *testing.T) {
	store, db := migrationTestStore(t)
	ctx := t.Context()
	steps := migrationSteps(t, "CREATE TABLE migration_probe (id integer PRIMARY KEY);")
	var workers sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			results <- store.migrate(ctx, steps)
		}()
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := store.pingVersion(ctx, 2); err != nil {
		t.Fatal("concurrent migration left invalid schema:", err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version=1"); err != nil {
		t.Fatal(err)
	}
	if err := store.pingVersion(ctx, 2); err == nil {
		t.Fatal("missing migration version reported ready")
	}
	if err := store.migrate(ctx, steps); err == nil {
		t.Fatal("migration advanced past missing history")
	}
}

func TestMigrationFilesRequireContiguousVersions(t *testing.T) {
	for _, names := range [][]string{{"migrations/001_first.sql", "migrations/003_third.sql"}, {"migrations/001_first.sql", "migrations/001_second.sql"}, {"migrations/001_first.sql", "migrations/002_empty.sql"}} {
		files := fstest.MapFS{}
		for _, name := range names {
			content := "SELECT 1;"
			if strings.Contains(name, "empty") {
				content = "  "
			}
			files[name] = &fstest.MapFile{Data: []byte(content)}
		}
		if _, err := loadMigrations(files); err == nil {
			t.Fatalf("invalid migration files accepted: %v", names)
		}
	}
}
