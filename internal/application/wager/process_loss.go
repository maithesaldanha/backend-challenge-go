package wager

import (
	"context"
	"errors"
	"time"

	appevents "github.com/junglegaming/backend-challenge-go/internal/application/events"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wager"
)

type ProcessLoss struct {
	transactor ports.Transactor
	newID      func() string
	now        func() time.Time
}

func NewProcessLoss(transactor ports.Transactor, newID func() string, now func() time.Time) (*ProcessLoss, error) {
	if transactor == nil || newID == nil || now == nil {
		return nil, ErrInvalidDependencies
	}
	return &ProcessLoss{transactor: transactor, newID: newID, now: now}, nil
}

func (p *ProcessLoss) Execute(ctx context.Context, command LossCommand) (LossResult, error) {
	now := p.now()
	params := wager.ExternalParams{
		ID:                    p.newID(),
		ProviderID:            command.ProviderID,
		ExternalTransactionID: command.ExternalTransactionID,
		IdempotencyKey:        command.IdempotencyKey,
		PayloadHash:           command.PayloadHash,
		WalletID:              command.WalletID,
		PlayerID:              command.PlayerID,
		RoundID:               command.RoundID,
		GameID:                command.GameID,
		Kind:                  wager.Loss,
		Money:                 command.Money,
		CreatedAt:             now,
	}
	transaction, err := wager.NewExternal(params)
	if err != nil {
		return LossResult{}, err
	}

	var result LossResult
	err = p.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		replayTransaction, replay, lookupErr := findReplay(txctx, unit.Wagers(), params)
		if lookupErr != nil {
			return lookupErr
		}
		if replay {
			transaction = replayTransaction
			result = LossResult{Transaction: replayTransaction, IdempotentReplay: true}
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
		if err := transaction.MarkProcessed(account.Balance(), now); err != nil {
			return err
		}
		if err := unit.Wagers().Create(txctx, transaction); err != nil {
			return err
		}
		correlationID := command.CorrelationID
		if correlationID == "" {
			correlationID = transaction.ID()
		}
		event, err := appevents.NewWagerTransactionProcessed(p.newID(), correlationID, transaction, now)
		if err != nil {
			return err
		}
		if err := unit.Outbox().Append(txctx, event); err != nil {
			return err
		}
		result = LossResult{Transaction: transaction}
		return nil
	})
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, ports.ErrConflict) {
		return LossResult{}, err
	}

	var replayTransaction wager.Transaction
	var replayFound bool
	lookupErr := p.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		var err error
		replayTransaction, replayFound, err = findReplay(txctx, unit.Wagers(), params)
		return err
	})
	if lookupErr != nil {
		return LossResult{}, err
	}
	if replayFound {
		return LossResult{Transaction: replayTransaction, IdempotentReplay: true}, nil
	}
	return LossResult{}, err
}
