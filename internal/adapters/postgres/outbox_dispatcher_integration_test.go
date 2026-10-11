package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/postgres"
	"github.com/junglegaming/backend-challenge-go/internal/application/events"
	"github.com/junglegaming/backend-challenge-go/internal/application/outbox"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
)

func TestOutboxDispatcherClaimsEventOnlyOnceAcrossDispatchers(t *testing.T) {
	ctx, db, transactor := newOutboxIntegrationDatabase(t)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	event := newOutboxIntegrationEvent(now)
	appendOutboxEvent(t, ctx, transactor, event)

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unlockPublisher := func() { releaseOnce.Do(func() { close(release) }) }
	var firstCalls atomic.Int32
	firstPublisher := publisherFunc(func(ctx context.Context, _ events.Event) error {
		firstCalls.Add(1)
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	var secondCalls atomic.Int32
	secondPublisher := publisherFunc(func(context.Context, events.Event) error {
		secondCalls.Add(1)
		return nil
	})
	first, err := outbox.NewDispatcher(transactor, firstPublisher, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	second, err := outbox.NewDispatcher(transactor, secondPublisher, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	firstResult := make(chan error, 1)
	go func() {
		_, err := first.ProcessOne(ctx)
		firstResult <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first dispatcher did not reach publisher")
	}
	defer unlockPublisher()

	worked, err := second.ProcessOne(ctx)
	if err != nil {
		t.Fatalf("second dispatcher failed: %v", err)
	}
	if worked {
		t.Fatal("second dispatcher claimed an event with an active lease")
	}
	if secondCalls.Load() != 0 {
		t.Fatalf("second publisher called %d times, want 0", secondCalls.Load())
	}
	unlockPublisher()
	if err := <-firstResult; err != nil {
		t.Fatalf("first dispatcher failed: %v", err)
	}
	if firstCalls.Load() != 1 {
		t.Fatalf("first publisher called %d times, want 1", firstCalls.Load())
	}
	assertOutboxState(t, ctx, db, event.ID, 1, true, false)
}

func TestOutboxDispatcherPersistsRetryAndPublishesAfterBackoff(t *testing.T) {
	ctx, db, transactor := newOutboxIntegrationDatabase(t)
	firstAttemptAt := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	event := newOutboxIntegrationEvent(firstAttemptAt)
	appendOutboxEvent(t, ctx, transactor, event)

	currentTime := firstAttemptAt
	var calls atomic.Int32
	publisher := publisherFunc(func(context.Context, events.Event) error {
		if calls.Add(1) == 1 {
			return errors.New("temporary publish failure")
		}
		return nil
	})
	dispatcher, err := outbox.NewDispatcher(transactor, publisher, func() time.Time { return currentTime })
	if err != nil {
		t.Fatal(err)
	}
	worked, err := dispatcher.ProcessOne(ctx)
	if !worked || err == nil {
		t.Fatalf("first ProcessOne() = (%t, %v), want (true, publish error)", worked, err)
	}
	assertOutboxState(t, ctx, db, event.ID, 1, false, false)

	worked, err = dispatcher.ProcessOne(ctx)
	if err != nil || worked {
		t.Fatalf("ProcessOne() before backoff = (%t, %v), want (false, nil)", worked, err)
	}
	currentTime = firstAttemptAt.Add(2 * time.Second)
	worked, err = dispatcher.ProcessOne(ctx)
	if err != nil || !worked {
		t.Fatalf("ProcessOne() after backoff = (%t, %v), want (true, nil)", worked, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("publisher called %d times, want 2", calls.Load())
	}
	assertOutboxState(t, ctx, db, event.ID, 2, true, false)
}

type publisherFunc func(context.Context, events.Event) error

func (f publisherFunc) Publish(ctx context.Context, event events.Event) error {
	return f(ctx, event)
}

func newOutboxIntegrationDatabase(t *testing.T) (context.Context, *sql.DB, ports.Transactor) {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	baseDB, err := postgres.NewDB(postgres.Config{DSN: databaseURL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = baseDB.Close() })
	if err := baseDB.PingContext(ctx); err != nil {
		t.Fatalf("connect to test PostgreSQL: %v", err)
	}
	schema := "outbox_dispatcher_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := baseDB.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create isolated test schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = baseDB.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
	})
	schemaURL, err := withSearchPath(databaseURL, schema)
	if err != nil {
		t.Fatal(err)
	}
	db, err := postgres.NewDB(postgres.Config{DSN: schemaURL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migration, err := os.ReadFile(filepath.Join("..", "..", "..", "migrations", "000001_init.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(migration)); err != nil {
		t.Fatalf("apply schema migration: %v", err)
	}
	transactor, err := postgres.NewTransactor(db)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, db, transactor
}

func newOutboxIntegrationEvent(now time.Time) events.Event {
	return events.Event{
		ID: uuid.NewString(), AggregateID: uuid.NewString(), Type: "IntegrationTestEvent",
		CorrelationID: uuid.NewString(), OccurredAt: now, Version: 1, Payload: json.RawMessage(`{"value":"test"}`),
	}
}

func appendOutboxEvent(t *testing.T, ctx context.Context, transactor ports.Transactor, event events.Event) {
	t.Helper()
	err := transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		return unit.Outbox().Append(txctx, event)
	})
	if err != nil {
		t.Fatalf("append outbox event: %v", err)
	}
}

func assertOutboxState(t *testing.T, ctx context.Context, db *sql.DB, eventID string, wantAttempts int, wantPublished, wantLeased bool) {
	t.Helper()
	var attempts int
	var publishedAt, leaseUntil sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT attempts, published_at, lease_until FROM outbox_events WHERE event_id = $1`, eventID).Scan(&attempts, &publishedAt, &leaseUntil); err != nil {
		t.Fatal(err)
	}
	if attempts != wantAttempts || publishedAt.Valid != wantPublished || leaseUntil.Valid != wantLeased {
		t.Fatalf("outbox state attempts=%d published=%t leased=%t, want %d %t %t", attempts, publishedAt.Valid, leaseUntil.Valid, wantAttempts, wantPublished, wantLeased)
	}
}
