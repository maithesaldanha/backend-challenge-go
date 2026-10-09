package wager

import (
	"context"
	"errors"

	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wager"
)

var ErrInvalidReadDependencies = errors.New("invalid wager read dependencies")

type ReadTransaction struct{ transactor ports.Transactor }

func NewReadTransaction(transactor ports.Transactor) (*ReadTransaction, error) {
	if transactor == nil {
		return nil, ErrInvalidReadDependencies
	}
	return &ReadTransaction{transactor: transactor}, nil
}

func (r *ReadTransaction) ByID(ctx context.Context, id string) (wager.Transaction, error) {
	var result wager.Transaction
	err := r.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		var err error
		result, err = unit.Wagers().GetByID(txctx, id)
		return err
	})
	return result, err
}

func (r *ReadTransaction) ByProviderExternalID(ctx context.Context, providerID, externalID string) (wager.Transaction, error) {
	var result wager.Transaction
	err := r.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		var err error
		result, err = unit.Wagers().FindByExternalTransactionID(txctx, providerID, externalID)
		return err
	})
	return result, err
}
