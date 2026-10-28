package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func TestEnrollmentCannotPassUncommittedRegistrationStop(t *testing.T) {
	f := testdb.Open(t)
	stop, err := f.SQL.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop.Rollback()
	now := time.Now().UTC()
	if _, err := stop.ExecContext(t.Context(), `UPDATE registration_control SET closed_at=$1 WHERE singleton=TRUE`, now); err != nil {
		t.Fatal(err)
	}
	value := store.NewExperience{
		ExperienceID: "exp_race", MemorySessionID: "mem_race", MemoryTokenHash: "hash",
		MemoryConsent: "consent", ConsentLanguage: "ja", DisplayColor: "blue", DisplaySymbol: "circle",
		CreatedAt: now, MemoryExpiresAt: now.Add(time.Hour), MemoryDeleteAfter: now.Add(2 * time.Hour),
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := f.Store.CreateExperience(ctx, value); err == nil {
		t.Fatal("enrollment committed while the registration stop was pending")
	}
	if err := stop.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.CreateExperience(t.Context(), value); !errors.Is(err, store.ErrRegistrationClosed) {
		t.Fatalf("enrollment after committed stop: %v", err)
	}
	var memories, experiences int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM memory_sessions), (SELECT COUNT(*) FROM experiences)`).Scan(&memories, &experiences); err != nil || memories != 0 || experiences != 0 {
		t.Fatalf("stopped enrollment left records: memories=%d experiences=%d err=%v", memories, experiences, err)
	}
}
