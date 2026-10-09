package wager

import (
	"context"
	"errors"
	"time"

	appevents "github.com/junglegaming/backend-challenge-go/internal/application/events"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	domainwager "github.com/junglegaming/backend-challenge-go/internal/domain/wager"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

var (
	ErrInvalidDependencies = errors.New("invalid process bet dependencies")
	ErrRequestConflict     = errors.New("request conflicts with an existing transaction")
	ErrWalletOwnership     = errors.New("wallet does not belong to the player")
)

type BetCommand struct {
	ProviderID            string
	ExternalTransactionID string
	IdempotencyKey        string
	PayloadHash           string
	CorrelationID         string
	WalletID              string
	PlayerID              string
	RoundID               string
	GameID                string
	Money                 money.Money
}

type BetResult struct {
	Transaction      domainwager.Transaction
	IdempotentReplay bool
}

type ProcessBet struct {
	transactor ports.Transactor
	newID      func() string
	now        func() time.Time
}

func NewProcessBet(transactor ports.Transactor, newID func() string, now func() time.Time) (*ProcessBet, error) {
	if transactor == nil || newID == nil || now == nil {
		return nil, ErrInvalidDependencies
	}
	return &ProcessBet{transactor: transactor, newID: newID, now: now}, nil
}

func (p *ProcessBet) Execute(ctx context.Context, command BetCommand) (BetResult, error) {
	now := p.now()
	params := domainwager.ExternalParams{
		ID:                    p.newID(),
		ProviderID:            command.ProviderID,
		ExternalTransactionID: command.ExternalTransactionID,
		IdempotencyKey:        command.IdempotencyKey,
		PayloadHash:           command.PayloadHash,
		WalletID:              command.WalletID,
		PlayerID:              command.PlayerID,
		RoundID:               command.RoundID,
		GameID:                command.GameID,
		Kind:                  domainwager.Bet,
		Money:                 command.Money,
		CreatedAt:             now,
	}
	transaction, err := domainwager.NewExternal(params)
	if err != nil {
		return BetResult{}, err
	}

	var result BetResult
	err = p.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		var replay bool
		var lookupErr error
		transaction, replay, lookupErr = findReplay(txctx, unit.Wagers(), params)
		if lookupErr != nil {
			return lookupErr
		}
		if replay {
			result = BetResult{Transaction: transaction, IdempotentReplay: true}
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

		previousVersion := account.Version()
		ledgerEntry, debitErr := account.Debit(p.newID(), transaction.ID(), params.Money, now)
		if errors.Is(debitErr, wallet.ErrInsufficientFunds) {
			if err := transaction.Reject("INSUFFICIENT_FUNDS", now); err != nil {
				return err
			}
			if err := unit.Wagers().Create(txctx, transaction); err != nil {
				return err
			}
			correlationID := command.CorrelationID
			if correlationID == "" {
				correlationID = transaction.ID()
			}
			event, err := appevents.NewWagerTransactionRejected(p.newID(), correlationID, transaction, now)
			if err != nil {
				return err
			}
			if err := unit.Outbox().Append(txctx, event); err != nil {
				return err
			}
			result = BetResult{Transaction: transaction}
			return nil
		}
		if debitErr != nil {
			return debitErr
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
		result = BetResult{Transaction: transaction}
		return nil
	})
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, ports.ErrConflict) {
		return BetResult{}, err
	}

	var replayTransaction domainwager.Transaction
	var replayFound bool
	lookupErr := p.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		var err error
		replayTransaction, replayFound, err = findReplay(txctx, unit.Wagers(), params)
		return err
	})
	if lookupErr != nil {
		return BetResult{}, err
	}
	if replayFound {
		return BetResult{Transaction: replayTransaction, IdempotentReplay: true}, nil
	}
	return BetResult{}, err
}

func findReplay(ctx context.Context, repository ports.WagerRepository, params domainwager.ExternalParams) (domainwager.Transaction, bool, error) {
	transaction, err := repository.FindByIdempotencyKey(ctx, params.ProviderID, params.IdempotencyKey)
	if err == nil {
		if !sameRequest(transaction, params) {
			return domainwager.Transaction{}, false, ErrRequestConflict
		}
		return transaction, true, nil
	}
	if !errors.Is(err, ports.ErrNotFound) {
		return domainwager.Transaction{}, false, err
	}

	transaction, err = repository.FindByExternalTransactionID(ctx, params.ProviderID, params.ExternalTransactionID)
	if err == nil {
		return domainwager.Transaction{}, false, ErrRequestConflict
	}
	if !errors.Is(err, ports.ErrNotFound) {
		return domainwager.Transaction{}, false, err
	}
	return domainwager.Transaction{}, false, nil
}

func sameRequest(transaction domainwager.Transaction, params domainwager.ExternalParams) bool {
	if transaction.ProviderID() != params.ProviderID ||
		transaction.ExternalTransactionID() != params.ExternalTransactionID ||
		transaction.IdempotencyKey() != params.IdempotencyKey ||
		transaction.PayloadHash() != params.PayloadHash ||
		transaction.WalletID() != params.WalletID ||
		transaction.PlayerID() != params.PlayerID ||
		transaction.RoundID() != params.RoundID ||
		transaction.GameID() != params.GameID ||
		transaction.Kind() != params.Kind ||
		transaction.ReferenceExternalTransactionID() != params.ReferenceExternalTransactionID {
		return false
	}
	comparison, err := transaction.Money().Compare(params.Money)
	return err == nil && comparison == 0
}
