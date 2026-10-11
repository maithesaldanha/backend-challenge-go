package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/application/events"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
)

type outboxTransactor struct {
	unit  ports.UnitOfWork
	calls int
	check func(context.Context, int) error
}

func (t *outboxTransactor) WithinTransaction(ctx context.Context, fn func(context.Context, ports.UnitOfWork) error) error {
	t.calls++
	if t.check != nil {
		if err := t.check(ctx, t.calls); err != nil {
			return err
		}
	}
	return fn(ctx, t.unit)
}

type outboxUnit struct{ repo ports.OutboxRepository }

func (u outboxUnit) Wallets() ports.WalletRepository { return nil }
func (u outboxUnit) Wagers() ports.WagerRepository   { return nil }
func (u outboxUnit) Ledger() ports.LedgerRepository  { return nil }
func (u outboxUnit) Outbox() ports.OutboxRepository  { return u.repo }
func (u outboxUnit) Inbox() ports.InboxRepository    { return nil }

type outboxRepository struct {
	record           events.OutboxRecord
	claimErr         error
	claimedAt        time.Time
	leaseUntil       time.Time
	publishedEventID string
	publishedLease   time.Time
	publishedAt      time.Time
	retryEventID     string
	retryLease       time.Time
	retryAt          time.Time
	markErr          error
	retryErr         error
}

func (r *outboxRepository) Append(context.Context, events.Event) error { return nil }
func (r *outboxRepository) ClaimDue(_ context.Context, now, leaseUntil time.Time) (events.OutboxRecord, error) {
	r.claimedAt = now
	r.leaseUntil = leaseUntil
	return r.record, r.claimErr
}
func (r *outboxRepository) MarkPublished(_ context.Context, eventID string, leaseUntil, publishedAt time.Time) error {
	r.publishedEventID = eventID
	r.publishedLease = leaseUntil
	r.publishedAt = publishedAt
	return r.markErr
}
func (r *outboxRepository) ScheduleRetry(_ context.Context, eventID string, leaseUntil, retryAt time.Time) error {
	r.retryEventID = eventID
	r.retryLease = leaseUntil
	r.retryAt = retryAt
	return r.retryErr
}

type outboxPublisherFunc func(context.Context, events.Event) error

func (f outboxPublisherFunc) Publish(ctx context.Context, event events.Event) error {
	return f(ctx, event)
}

func TestProcessOnePublishesClaimedEventAndMarksItPublished(t *testing.T) {
	claimedAt := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	publishedAt := claimedAt.Add(time.Second)
	record := &outboxRepository{record: events.OutboxRecord{
		Event:      events.Event{ID: "event-1", AggregateID: "wallet-1", Type: "WalletBalanceChanged"},
		Attempts:   1,
		LeaseUntil: claimedAt.Add(30 * time.Second),
	}}
	clockCalls := 0
	transactor := &outboxTransactor{unit: outboxUnit{repo: record}}
	dispatcher, err := NewDispatcher(transactor, outboxPublisherFunc(func(_ context.Context, event events.Event) error {
		if event.ID != "event-1" {
			t.Fatalf("published event ID = %q, want event-1", event.ID)
		}
		return nil
	}), func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			return claimedAt
		}
		return publishedAt
	})
	if err != nil {
		t.Fatal(err)
	}

	worked, err := dispatcher.ProcessOne(context.Background())

	if err != nil || !worked {
		t.Fatalf("ProcessOne() = %t, %v; want true, nil", worked, err)
	}
	if transactor.calls != 2 {
		t.Fatalf("transaction calls = %d, want 2", transactor.calls)
	}
	if record.claimedAt != claimedAt || record.leaseUntil != claimedAt.Add(30*time.Second) {
		t.Fatalf("claim times = %s, %s", record.claimedAt, record.leaseUntil)
	}
	if record.publishedEventID != "event-1" || record.publishedLease != record.leaseUntil || record.publishedAt != publishedAt {
		t.Fatalf("published state = %#v", record)
	}
}

func TestProcessOneSchedulesExponentialRetryAfterPublishFailure(t *testing.T) {
	claimedAt := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	publishErr := errors.New("broker unavailable")
	record := &outboxRepository{record: events.OutboxRecord{
		Event:      events.Event{ID: "event-2"},
		Attempts:   4,
		LeaseUntil: claimedAt.Add(30 * time.Second),
	}}
	clockCalls := 0
	transactor := &outboxTransactor{unit: outboxUnit{repo: record}}
	dispatcher, err := NewDispatcher(transactor, outboxPublisherFunc(func(context.Context, events.Event) error {
		return publishErr
	}), func() time.Time {
		clockCalls++
		return claimedAt
	})
	if err != nil {
		t.Fatal(err)
	}

	worked, err := dispatcher.ProcessOne(context.Background())

	if !worked || !errors.Is(err, publishErr) {
		t.Fatalf("ProcessOne() = %t, %v; want true and broker error", worked, err)
	}
	if record.retryEventID != "event-2" || record.retryLease != record.leaseUntil || record.retryAt != claimedAt.Add(8*time.Second) {
		t.Fatalf("retry state = event %q lease %s at %s", record.retryEventID, record.retryLease, record.retryAt)
	}
}

func TestProcessOnePersistsRetryAfterParentCancellation(t *testing.T) {
	claimedAt := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	record := &outboxRepository{record: events.OutboxRecord{
		Event:      events.Event{ID: "event-3"},
		Attempts:   1,
		LeaseUntil: claimedAt.Add(30 * time.Second),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	transactor := &outboxTransactor{
		unit: outboxUnit{repo: record},
		check: func(ctx context.Context, call int) error {
			if call == 2 && ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		},
	}
	dispatcher, err := NewDispatcher(transactor, outboxPublisherFunc(func(context.Context, events.Event) error {
		cancel()
		return errors.New("publish interrupted")
	}), func() time.Time { return claimedAt })
	if err != nil {
		t.Fatal(err)
	}

	worked, err := dispatcher.ProcessOne(ctx)

	if !worked || err == nil {
		t.Fatalf("ProcessOne() = %t, %v; want worked and publish error", worked, err)
	}
	if record.retryEventID != "event-3" || record.retryAt != claimedAt.Add(time.Second) {
		t.Fatalf("retry not persisted after cancellation: event=%q retryAt=%s", record.retryEventID, record.retryAt)
	}
}

func TestProcessOneReturnsIdleWhenNoEventIsDue(t *testing.T) {
	record := &outboxRepository{claimErr: ports.ErrNotFound}
	transactor := &outboxTransactor{unit: outboxUnit{repo: record}}
	published := false
	dispatcher, err := NewDispatcher(transactor, outboxPublisherFunc(func(context.Context, events.Event) error {
		published = true
		return nil
	}), func() time.Time { return time.Now().UTC() })
	if err != nil {
		t.Fatal(err)
	}

	worked, err := dispatcher.ProcessOne(context.Background())

	if worked || err != nil || published || transactor.calls != 1 {
		t.Fatalf("ProcessOne() = %t, %v; published=%t transactions=%d", worked, err, published, transactor.calls)
	}
}

func TestOutboxBackoffCapsAtOneHour(t *testing.T) {
	for _, tt := range []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 0, want: time.Second},
		{attempt: 1, want: time.Second},
		{attempt: 2, want: 2 * time.Second},
		{attempt: 13, want: time.Hour},
		{attempt: 20, want: time.Hour},
	} {
		if got := outboxBackoff(tt.attempt); got != tt.want {
			t.Errorf("outboxBackoff(%d) = %s, want %s", tt.attempt, got, tt.want)
		}
	}
}

var _ ports.OutboxRepository = (*outboxRepository)(nil)
var _ ports.UnitOfWork = outboxUnit{}
var _ ports.Transactor = (*outboxTransactor)(nil)
var _ Publisher = outboxPublisherFunc(nil)
