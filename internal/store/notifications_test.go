package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/store"
	"github.com/Mino829/trace_Backend/internal/testdb"
)

func TestNotificationStopWaitsForInflightDeliveryAndFencesCachedCandidates(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	owner := fixtureExperience(t, f, now)
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE live_presence_sessions SET expires_at=$2 WHERE id=$1`, owner.LiveSessionID, now.Add(50*time.Minute)); err != nil {
		t.Fatal(err)
	}
	p, err := f.Store.PutPush(t.Context(), pushFixture(owner.MemorySessionID, 30), now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Open(t.Context(), f.URL, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	done := make(chan error, 1)
	go func() {
		done <- f.Store.DeliverPush(t.Context(), owner.MemorySessionID, p.NextSendAt, func(store.PushSubscription) (int, error) {
			close(started)
			<-release
			return 201, nil
		})
	}()
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("delivery failed to start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("delivery did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := other.StopNotifications(store.WithOperatorActor(ctx, "verified-test-operator"), p.NextSendAt); err == nil {
		t.Fatal("stop succeeded while provider request was still in flight")
	}
	if stopped, err := other.PushStopped(t.Context()); err != nil || stopped || countPush(t, f) != 1 {
		t.Fatalf("timed-out stop changed state: %t %v", stopped, err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := other.StopNotifications(store.WithOperatorActor(t.Context(), "verified-test-operator"), p.NextSendAt); err != nil {
		t.Fatal(err)
	}
	sent := false
	if err := f.Store.DeliverPush(t.Context(), owner.MemorySessionID, p.NextSendAt.Add(time.Minute), func(store.PushSubscription) (int, error) { sent = true; return 201, nil }); err != nil || sent || countPush(t, f) != 0 {
		t.Fatalf("cached candidate delivered after stop: sent=%t err=%v", sent, err)
	}
	if ids, err := f.Store.DuePushIDs(t.Context(), p.NextSendAt.Add(time.Minute), 50); err != nil || len(ids) != 0 {
		t.Fatalf("stop retained scheduled sends: %v %v", ids, err)
	}
}

func TestNotificationStopAuditFailureRollsBackStopAndCredentialRemoval(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	owner := fixtureExperience(t, f, now)
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE live_presence_sessions SET expires_at=$2 WHERE id=$1`, owner.LiveSessionID, now.Add(50*time.Minute)); err != nil {
		t.Fatal(err)
	}
	p, err := f.Store.PutPush(t.Context(), pushFixture(owner.MemorySessionID, 30), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.SQL.ExecContext(t.Context(), `ALTER TABLE arrival_operator_audit ADD CONSTRAINT reject_notification_stop CHECK (action<>'notifications.stop')`); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.StopNotifications(store.WithOperatorActor(t.Context(), "verified-test-operator"), now); err == nil {
		t.Fatal("audit failure reported success")
	}
	if stopped, err := f.Store.PushStopped(t.Context()); err != nil || stopped || countPush(t, f) != 1 {
		t.Fatalf("failed audit changed state: stopped=%t err=%v", stopped, err)
	}
	sent := false
	if err := f.Store.DeliverPush(t.Context(), owner.MemorySessionID, p.NextSendAt, func(p store.PushSubscription) (int, error) {
		sent = true
		if string(p.EndpointEncrypted) != "encrypted-endpoint" || string(p.KeysEncrypted) != "encrypted-keys" {
			t.Error("rollback lost credentials")
		}
		return 201, nil
	}); err != nil || !sent {
		t.Fatalf("failed stop prevented scheduled delivery: sent=%t err=%v", sent, err)
	}
}

func TestPushMutationsCannotPassUncommittedNotificationStop(t *testing.T) {
	f := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	owner := fixtureExperience(t, f, now)
	if _, err := f.SQL.ExecContext(t.Context(), `UPDATE live_presence_sessions SET expires_at=$2 WHERE id=$1`, owner.LiveSessionID, now.Add(50*time.Minute)); err != nil {
		t.Fatal(err)
	}
	p, err := f.Store.PutPush(t.Context(), pushFixture(owner.MemorySessionID, 30), now)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := f.SQL.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop.Rollback()
	if _, err := stop.ExecContext(t.Context(), `UPDATE notification_control SET stopped_at=$1 WHERE singleton=TRUE`, now); err != nil {
		t.Fatal(err)
	}
	active := "active"
	mutations := []struct {
		name   string
		change func(context.Context) error
	}{
		{"put", func(ctx context.Context) error {
			_, err := f.Store.PutPush(ctx, pushFixture(owner.MemorySessionID, 30), now.Add(time.Minute))
			return err
		}},
		{"resume", func(ctx context.Context) error {
			_, err := f.Store.PatchPush(ctx, owner.MemorySessionID, nil, &active, now.Add(time.Minute))
			return err
		}},
	}
	for _, mutation := range mutations {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		err := mutation.change(ctx)
		cancel()
		if err == nil {
			t.Fatalf("%s committed while stop was pending", mutation.name)
		}
	}
	if err := stop.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range mutations {
		if err := mutation.change(t.Context()); !errors.Is(err, store.ErrPushStopped) {
			t.Fatalf("%s bypassed committed stop: %v", mutation.name, err)
		}
	}
	var next time.Time
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT next_send_at FROM push_subscriptions WHERE memory_session_id=$1`, owner.MemorySessionID).Scan(&next); err != nil || !next.Equal(p.NextSendAt) {
		t.Fatalf("blocked mutation changed schedule: %v %v", next, err)
	}
	// Delivery checks the latch itself, not just the absence of credentials.
	sent := false
	if err := f.Store.DeliverPush(t.Context(), owner.MemorySessionID, p.NextSendAt, func(store.PushSubscription) (int, error) { sent = true; return 201, nil }); err != nil || sent {
		t.Fatalf("delivery bypassed stop latch: sent=%t err=%v", sent, err)
	}
	if ids, err := f.Store.DuePushIDs(t.Context(), p.NextSendAt, 50); err != nil || len(ids) != 0 {
		t.Fatalf("stopped subscriptions returned as due: %v %v", ids, err)
	}
}
