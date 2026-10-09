package wager

import (
	"context"
	"errors"
	"time"

	appevents "github.com/junglegaming/backend-challenge-go/internal/application/events"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wager"
)

type ProcessWin struct {
	transactor ports.Transactor
	newID      func() string
	now        func() time.Time
}

func NewProcessWin(transactor ports.Transactor, newID func() string, now func() time.Time) (*ProcessWin, error) {
	if transactor == nil || newID == nil || now == nil {
		return nil, ErrInvalidDependencies
	}
	return &ProcessWin{transactor: transactor, newID: newID, now: now}, nil
}

func (p *ProcessWin) Execute(ctx context.Context, command WinCommand) (WinResult, error) {
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
		Kind:                           wager.Win,
		Money:                          command.Money,
		ReferenceExternalTransactionID: command.ReferenceExternalTransactionID,
		CreatedAt:                      now,
	}
	transaction, err := wager.NewExternal(params)
	if err != nil {
		return WinResult{}, err
	}

	var result WinResult
	err = p.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		replayTransaction, replay, lookupErr := findReplay(txctx, unit.Wagers(), params)
		if lookupErr != nil {
			return lookupErr
		}
		if replay {
			result = WinResult{Transaction: replayTransaction, IdempotentReplay: true}
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

		if params.ReferenceExternalTransactionID != "" {
			reference, err := unit.Wagers().FindReference(txctx, params.ProviderID, params.ReferenceExternalTransactionID)
			if errors.Is(err, ports.ErrNotFound) || err == nil && (reference.Status() == wager.Pending || reference.Status() == wager.PendingReference) {
				if err := transaction.MarkPendingReference(now); err != nil {
					return err
				}
				if err := unit.Wagers().Create(txctx, transaction); err != nil {
					return err
				}
				correlationID := command.CorrelationID
				if correlationID == "" {
					correlationID = transaction.ID()
				}
				event, err := appevents.NewWagerTransactionPendingReference(p.newID(), correlationID, transaction, now)
				if err != nil {
					return err
				}
				if err := unit.Outbox().Append(txctx, event); err != nil {
					return err
				}
				result = WinResult{Transaction: transaction}
				return nil
			}
			if err != nil {
				return err
			}
			if reference.Status() != wager.Processed {
				return p.reject(txctx, unit, command, &transaction, "REFERENCE_NOT_PROCESSED", now, &result)
			}
			if err := transaction.ResolveReference(reference, nil, now); err != nil {
				switch {
				case errors.Is(err, wager.ErrInvalidReferenceKind):
					return p.reject(txctx, unit, command, &transaction, "REFERENCE_KIND_INVALID", now, &result)
				case errors.Is(err, wager.ErrReferenceMismatch):
					return p.reject(txctx, unit, command, &transaction, "REFERENCE_MISMATCH", now, &result)
				default:
					return err
				}
			}
		}

		previousVersion := account.Version()
		ledgerEntry, err := account.Credit(p.newID(), transaction.ID(), params.Money, now)
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
		if err := unit.Ledger().Append(txctx, ledgerEntry); err != nil {
			return err
		}
		correlationID := command.CorrelationID
		if correlationID == "" {
			correlationID = transaction.ID()
		}
		processedEvent, err := appevents.NewWagerTransactionProcessed(p.newID(), correlationID, transaction, now)
		if err != nil {
			return err
		}
		if err := unit.Outbox().Append(txctx, processedEvent); err != nil {
			return err
		}
		balanceEvent, err := appevents.NewWalletBalanceChanged(p.newID(), correlationID, ledgerEntry, account.Version(), now)
		if err != nil {
			return err
		}
		if err := unit.Outbox().Append(txctx, balanceEvent); err != nil {
			return err
		}
		result = WinResult{Transaction: transaction}
		return nil
	})
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, ports.ErrConflict) {
		return WinResult{}, err
	}

	var replayTransaction wager.Transaction
	var replayFound bool
	lookupErr := p.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		var err error
		replayTransaction, replayFound, err = findReplay(txctx, unit.Wagers(), params)
		return err
	})
	if lookupErr != nil {
		return WinResult{}, err
	}
	if replayFound {
		return WinResult{Transaction: replayTransaction, IdempotentReplay: true}, nil
	}
	return WinResult{}, err
}

func (p *ProcessWin) reject(ctx context.Context, unit ports.UnitOfWork, command WinCommand, transaction *wager.Transaction, code string, now time.Time, result *WinResult) error {
	if err := transaction.Reject(code, now); err != nil {
		return err
	}
	if err := unit.Wagers().Create(ctx, *transaction); err != nil {
		return err
	}
	correlationID := command.CorrelationID
	if correlationID == "" {
		correlationID = transaction.ID()
	}
	event, err := appevents.NewWagerTransactionRejected(p.newID(), correlationID, *transaction, now)
	if err != nil {
		return err
	}
	if err := unit.Outbox().Append(ctx, event); err != nil {
		return err
	}
	*result = WinResult{Transaction: *transaction}
	return nil
}
