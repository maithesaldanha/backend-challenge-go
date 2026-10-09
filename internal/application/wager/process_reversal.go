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

type ProcessReversal struct {
	transactor ports.Transactor
	newID      func() string
	now        func() time.Time
}

func NewProcessReversal(transactor ports.Transactor, newID func() string, now func() time.Time) (*ProcessReversal, error) {
	if transactor == nil || newID == nil || now == nil {
		return nil, ErrInvalidDependencies
	}
	return &ProcessReversal{transactor: transactor, newID: newID, now: now}, nil
}

func (p *ProcessReversal) Execute(ctx context.Context, kind wager.Kind, command ReversalCommand) (ReversalResult, error) {
	if kind != wager.Refund && kind != wager.Rollback {
		return ReversalResult{}, wager.ErrInvalidKind
	}
	now := p.now()
	params := wager.ExternalParams{
		ID:                             p.newID(),
		ProviderID:                     command.ProviderID,
		ExternalTransactionID:          command.ExternalTransactionID,
		IdempotencyKey:                 command.IdempotencyKey,
		PayloadHash:                    command.PayloadHash,
		WalletID:                       command.WalletID,
		PlayerID:                       command.PlayerID,
		RoundID:                        command.RoundID,
		GameID:                         command.GameID,
		Kind:                           kind,
		Money:                          command.Money,
		ReferenceExternalTransactionID: command.ReferenceExternalTransactionID,
		CreatedAt:                      now,
	}
	transaction, err := wager.NewExternal(params)
	if err != nil {
		return ReversalResult{}, err
	}

	var result ReversalResult
	err = p.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		replayTransaction, replay, err := findReplay(txctx, unit.Wagers(), params)
		if err != nil {
			return err
		}
		if replay {
			result = ReversalResult{Transaction: replayTransaction, IdempotentReplay: true}
			return nil
		}

		account, err := unit.Wallets().GetForUpdate(txctx, params.WalletID)
		if err != nil {
			return err
		}
		if account.PlayerID() != params.PlayerID {
			return ErrWalletOwnership
		}
		if _, err := account.Balance().Compare(params.Money); err != nil {
			return err
		}

		reference, err := unit.Wagers().FindReference(txctx, params.ProviderID, params.ReferenceExternalTransactionID)
		if errors.Is(err, ports.ErrNotFound) || err == nil && (reference.Status() == wager.Pending || reference.Status() == wager.PendingReference) {
			if err := transaction.MarkPendingReference(now); err != nil {
				return err
			}
			if err := unit.Wagers().Create(txctx, transaction); err != nil {
				return err
			}
			if err := appendPendingReferenceEvent(txctx, unit, p.newID, command.CorrelationID, transaction, now); err != nil {
				return err
			}
			result = ReversalResult{Transaction: transaction}
			return nil
		}
		if err != nil {
			return err
		}
		if reference.Status() != wager.Processed {
			return p.reject(txctx, unit, command, &transaction, "REFERENCE_NOT_PROCESSED", now, &result)
		}
		reversals, err := unit.Wagers().FindProcessedReversals(txctx, reference.ID())
		if err != nil {
			return err
		}
		if err := transaction.ResolveReference(reference, reversals, now); err != nil {
			code, ok := reversalFailureCode(err)
			if !ok {
				return err
			}
			return p.reject(txctx, unit, command, &transaction, code, now, &result)
		}

		previousVersion := account.Version()
		var entry wallet.LedgerEntry
		if kind == wager.Refund || reference.Kind() == wager.Bet {
			entry, err = account.Credit(p.newID(), transaction.ID(), params.Money, now)
		} else {
			entry, err = account.Debit(p.newID(), transaction.ID(), params.Money, now)
		}
		if kind == wager.Rollback && errors.Is(err, wallet.ErrInsufficientFunds) {
			return p.reject(txctx, unit, command, &transaction, "INSUFFICIENT_FUNDS_FOR_ROLLBACK", now, &result)
		}
		if errors.Is(err, money.ErrOverflow) {
			return p.reject(txctx, unit, command, &transaction, "BALANCE_LIMIT_EXCEEDED", now, &result)
		}
		if err != nil {
			return err
		}
		if err := transaction.MarkProcessed(account.Balance(), now); err != nil {
			return err
		}
		if err := unit.Wagers().Create(txctx, transaction); err != nil {
			return err
		}
		if err := unit.Wallets().Save(txctx, account, previousVersion); err != nil {
			return err
		}
		if err := unit.Ledger().Append(txctx, entry); err != nil {
			return err
		}
		correlationID := correlationID(command.CorrelationID, transaction.ID())
		if err := appendProcessedEvents(txctx, unit, p.newID, transaction, entry, account.Version(), now, correlationID); err != nil {
			return err
		}
		result = ReversalResult{Transaction: transaction}
		return nil
	})
	if err == nil || !errors.Is(err, ports.ErrConflict) {
		return result, err
	}

	var replayTransaction wager.Transaction
	var replayFound bool
	lookupErr := p.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		var err error
		replayTransaction, replayFound, err = findReplay(txctx, unit.Wagers(), params)
		return err
	})
	if lookupErr != nil {
		return ReversalResult{}, err
	}
	if replayFound {
		return ReversalResult{Transaction: replayTransaction, IdempotentReplay: true}, nil
	}
	return ReversalResult{}, err
}

func (p *ProcessReversal) reject(ctx context.Context, unit ports.UnitOfWork, command ReversalCommand, transaction *wager.Transaction, code string, now time.Time, result *ReversalResult) error {
	if err := transaction.Reject(code, now); err != nil {
		return err
	}
	if err := unit.Wagers().Create(ctx, *transaction); err != nil {
		return err
	}
	event, err := appevents.NewWagerTransactionRejected(p.newID(), correlationID(command.CorrelationID, transaction.ID()), *transaction, now)
	if err != nil {
		return err
	}
	if err := unit.Outbox().Append(ctx, event); err != nil {
		return err
	}
	*result = ReversalResult{Transaction: *transaction}
	return nil
}

func reversalFailureCode(err error) (string, bool) {
	switch {
	case errors.Is(err, wager.ErrInvalidReferenceKind):
		return "REFERENCE_KIND_INVALID", true
	case errors.Is(err, wager.ErrReferenceMismatch):
		return "REFERENCE_MISMATCH", true
	case errors.Is(err, wager.ErrDuplicateReversal):
		return "DUPLICATE_REVERSAL", true
	case errors.Is(err, wager.ErrConflictingReversal):
		return "CONFLICTING_REVERSAL", true
	default:
		return "", false
	}
}

func appendPendingReferenceEvent(ctx context.Context, unit ports.UnitOfWork, newID func() string, correlation string, transaction wager.Transaction, now time.Time) error {
	event, err := appevents.NewWagerTransactionPendingReference(newID(), correlationID(correlation, transaction.ID()), transaction, now)
	if err != nil {
		return err
	}
	return unit.Outbox().Append(ctx, event)
}

func correlationID(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
