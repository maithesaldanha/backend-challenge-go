package wager

import (
	"context"
	"errors"
	"time"

	appevents "github.com/junglegaming/backend-challenge-go/internal/application/events"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wager"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

const (
	defaultReferenceRetryAttempts = 12
	referenceRetryLease           = 30 * time.Second
)

type RetryPendingReferences struct {
	transactor  ports.Transactor
	newID       func() string
	now         func() time.Time
	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration
}

func NewRetryPendingReferences(transactor ports.Transactor, newID func() string, now func() time.Time) (*RetryPendingReferences, error) {
	if transactor == nil || newID == nil || now == nil {
		return nil, ErrInvalidDependencies
	}
	return &RetryPendingReferences{
		transactor:  transactor,
		newID:       newID,
		now:         now,
		maxAttempts: defaultReferenceRetryAttempts,
		baseDelay:   time.Second,
		maxDelay:    time.Hour,
	}, nil
}

func (p *RetryPendingReferences) ProcessBatch(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	processed := 0
	for processed < limit {
		didWork, err := p.processOne(ctx)
		if err != nil {
			return processed, err
		}
		if !didWork {
			return processed, nil
		}
		processed++
	}
	return processed, nil
}

func (p *RetryPendingReferences) processOne(ctx context.Context) (bool, error) {
	now := p.now()
	didWork := false
	err := p.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		candidate, err := unit.Wagers().FindDuePendingReference(txctx, now)
		if errors.Is(err, ports.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}

		account, err := unit.Wallets().GetForUpdate(txctx, candidate.WalletID())
		if err != nil {
			return err
		}
		if account.PlayerID() != candidate.PlayerID() {
			return ErrWalletOwnership
		}
		if _, err := account.Balance().Compare(candidate.Money()); err != nil {
			return err
		}

		attempts, err := unit.Wagers().ClaimPendingReference(txctx, candidate.ID(), now, now.Add(referenceRetryLease))
		if errors.Is(err, ports.ErrConflict) {
			return nil
		}
		if err != nil {
			return err
		}
		transaction, err := unit.Wagers().GetByID(txctx, candidate.ID())
		if err != nil {
			return err
		}
		if transaction.Status() != wager.PendingReference {
			return ports.ErrConflict
		}
		didWork = true

		reference, referenceErr := unit.Wagers().FindReference(txctx, transaction.ProviderID(), transaction.ReferenceExternalTransactionID())
		if errors.Is(referenceErr, ports.ErrNotFound) {
			if attempts >= p.maxAttempts {
				return p.reject(txctx, unit, transaction, "REFERENCE_NOT_FOUND", now)
			}
			return unit.Wagers().SchedulePendingReference(txctx, transaction.ID(), now.Add(p.retryDelay(attempts)))
		}
		if referenceErr != nil {
			return referenceErr
		}
		if reference.Status() == wager.Pending || reference.Status() == wager.PendingReference {
			if attempts >= p.maxAttempts {
				return p.reject(txctx, unit, transaction, "REFERENCE_NOT_PROCESSED", now)
			}
			return unit.Wagers().SchedulePendingReference(txctx, transaction.ID(), now.Add(p.retryDelay(attempts)))
		}
		if reference.Status() != wager.Processed {
			return p.reject(txctx, unit, transaction, "REFERENCE_NOT_PROCESSED", now)
		}
		var reversals []wager.Transaction
		if transaction.Kind() == wager.Refund || transaction.Kind() == wager.Rollback {
			reversals, err = unit.Wagers().FindProcessedReversals(txctx, reference.ID())
			if err != nil {
				return err
			}
		}
		if err := transaction.ResolveReference(reference, reversals, now); err != nil {
			switch {
			case errors.Is(err, wager.ErrInvalidReferenceKind):
				return p.reject(txctx, unit, transaction, "REFERENCE_KIND_INVALID", now)
			case errors.Is(err, wager.ErrReferenceMismatch):
				return p.reject(txctx, unit, transaction, "REFERENCE_MISMATCH", now)
			case errors.Is(err, wager.ErrDuplicateReversal):
				return p.reject(txctx, unit, transaction, "DUPLICATE_REVERSAL", now)
			case errors.Is(err, wager.ErrConflictingReversal):
				return p.reject(txctx, unit, transaction, "CONFLICTING_REVERSAL", now)
			default:
				return err
			}
		}

		previousVersion := account.Version()
		var ledgerEntry wallet.LedgerEntry
		if transaction.Kind() == wager.Rollback && reference.Kind() != wager.Bet {
			ledgerEntry, err = account.Debit(p.newID(), transaction.ID(), transaction.Money(), now)
			if errors.Is(err, wallet.ErrInsufficientFunds) {
				return p.reject(txctx, unit, transaction, "INSUFFICIENT_FUNDS_FOR_ROLLBACK", now)
			}
		} else {
			ledgerEntry, err = account.Credit(p.newID(), transaction.ID(), transaction.Money(), now)
		}
		if errors.Is(err, money.ErrOverflow) {
			return p.reject(txctx, unit, transaction, "BALANCE_LIMIT_EXCEEDED", now)
		}
		if err != nil {
			return err
		}
		if err := transaction.MarkProcessed(account.Balance(), now); err != nil {
			return err
		}
		if err := unit.Wagers().Save(txctx, transaction); err != nil {
			return err
		}
		if err := unit.Wallets().Save(txctx, account, previousVersion); err != nil {
			return err
		}
		if err := unit.Ledger().Append(txctx, ledgerEntry); err != nil {
			return err
		}
		if err := appendProcessedEvents(txctx, unit, p.newID, transaction, ledgerEntry, account.Version(), now, transaction.ID()); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return didWork, nil
}

func (p *RetryPendingReferences) reject(ctx context.Context, unit ports.UnitOfWork, transaction wager.Transaction, code string, now time.Time) error {
	if err := transaction.Reject(code, now); err != nil {
		return err
	}
	if err := unit.Wagers().Save(ctx, transaction); err != nil {
		return err
	}
	event, err := appevents.NewWagerTransactionRejected(p.newID(), transaction.ID(), transaction, now)
	if err != nil {
		return err
	}
	return unit.Outbox().Append(ctx, event)
}

func (p *RetryPendingReferences) retryDelay(attempt int) time.Duration {
	delay := p.baseDelay
	for i := 1; i < attempt && delay < p.maxDelay; i++ {
		if delay > p.maxDelay/2 {
			return p.maxDelay
		}
		delay *= 2
	}
	if delay > p.maxDelay {
		return p.maxDelay
	}
	return delay
}

func appendProcessedEvents(ctx context.Context, unit ports.UnitOfWork, newID func() string, transaction wager.Transaction, entry wallet.LedgerEntry, walletVersion int64, now time.Time, correlation string) error {
	processedEvent, err := appevents.NewWagerTransactionProcessed(newID(), correlation, transaction, now)
	if err != nil {
		return err
	}
	if err := unit.Outbox().Append(ctx, processedEvent); err != nil {
		return err
	}
	balanceEvent, err := appevents.NewWalletBalanceChanged(newID(), correlation, entry, walletVersion, now)
	if err != nil {
		return err
	}
	return unit.Outbox().Append(ctx, balanceEvent)
}
