package outbox

import (
	"context"
	"errors"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/application/events"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	"github.com/junglegaming/backend-challenge-go/internal/observability"
)

var ErrInvalidDependencies = errors.New("invalid outbox dispatcher dependencies")

type Publisher interface {
	Publish(context.Context, events.Event) error
}

type Dispatcher struct {
	transactor    ports.Transactor
	publisher     Publisher
	now           func() time.Time
	leaseDuration time.Duration
	metrics       *observability.Metrics
}

func NewDispatcher(transactor ports.Transactor, publisher Publisher, now func() time.Time, metrics ...*observability.Metrics) (*Dispatcher, error) {
	if transactor == nil || publisher == nil || now == nil || len(metrics) > 1 {
		return nil, ErrInvalidDependencies
	}
	var collector *observability.Metrics
	if len(metrics) == 1 {
		collector = metrics[0]
	}
	return &Dispatcher{transactor: transactor, publisher: publisher, now: now, leaseDuration: 30 * time.Second, metrics: collector}, nil
}

func (d *Dispatcher) ProcessOne(ctx context.Context) (bool, error) {
	claimedAt := d.now().UTC()
	leaseUntil := claimedAt.Add(d.leaseDuration)
	var record events.OutboxRecord
	err := d.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		var err error
		record, err = unit.Outbox().ClaimDue(txctx, claimedAt, leaseUntil)
		return err
	})
	if errors.Is(err, ports.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	d.metrics.ObserveOutboxPublishLag(d.now().UTC().Sub(record.Event.OccurredAt))

	publishCtx, cancelPublish := context.WithTimeout(ctx, 10*time.Second)
	publishErr := d.publisher.Publish(publishCtx, record.Event)
	cancelPublish()
	if publishErr != nil {
		d.metrics.ObserveOutboxRetry()
		retryAt := d.now().UTC().Add(outboxBackoff(record.Attempts))
		updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		updateErr := d.transactor.WithinTransaction(updateCtx, func(txctx context.Context, unit ports.UnitOfWork) error {
			return unit.Outbox().ScheduleRetry(txctx, record.Event.ID, record.LeaseUntil, retryAt)
		})
		return true, errors.Join(publishErr, updateErr)
	}

	publishedAt := d.now().UTC()
	updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err = d.transactor.WithinTransaction(updateCtx, func(txctx context.Context, unit ports.UnitOfWork) error {
		return unit.Outbox().MarkPublished(txctx, record.Event.ID, record.LeaseUntil, publishedAt)
	})
	return true, err
}

func outboxBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return time.Second
	}
	if attempt >= 13 {
		return time.Hour
	}
	return time.Duration(uint64(1)<<uint(attempt-1)) * time.Second
}
