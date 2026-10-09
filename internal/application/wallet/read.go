package wallet

import (
	"context"
	"errors"

	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	domainwallet "github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

type ReconciliationResult struct {
	WalletID          string
	StoredBalance     money.Money
	CalculatedBalance money.Money
	Difference        money.Money
	Consistent        bool
	CheckedEntries    int64
}

type ReconcileWallet struct{ transactor ports.Transactor }

func NewReconcileWallet(transactor ports.Transactor) (*ReconcileWallet, error) {
	if transactor == nil {
		return nil, ErrInvalidDependencies
	}
	return &ReconcileWallet{transactor: transactor}, nil
}

func (r *ReconcileWallet) Execute(ctx context.Context, walletID string) (ReconciliationResult, error) {
	var result ReconciliationResult
	err := r.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		snapshot, err := unit.Wallets().Reconcile(txctx, walletID)
		if err != nil {
			return err
		}
		difference, err := snapshot.StoredBalance.Subtract(snapshot.CalculatedBalance)
		if err != nil {
			return err
		}
		result = ReconciliationResult{
			WalletID:          walletID,
			StoredBalance:     snapshot.StoredBalance,
			CalculatedBalance: snapshot.CalculatedBalance,
			Difference:        difference,
			Consistent:        difference.IsZero(),
			CheckedEntries:    snapshot.CheckedEntries,
		}
		return nil
	})
	return result, err
}

var ErrInvalidReadLimit = errors.New("invalid ledger read limit")

type ReadWallet struct{ transactor ports.Transactor }

func NewReadWallet(transactor ports.Transactor) (*ReadWallet, error) {
	if transactor == nil {
		return nil, ErrInvalidDependencies
	}
	return &ReadWallet{transactor: transactor}, nil
}

func (r *ReadWallet) Execute(ctx context.Context, id string) (domainwallet.Wallet, error) {
	var result domainwallet.Wallet
	err := r.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		var err error
		result, err = unit.Wallets().Get(txctx, id)
		return err
	})
	return result, err
}

type LedgerPage struct {
	Entries []domainwallet.LedgerEntry
	HasMore bool
}

type ReadWalletLedger struct{ transactor ports.Transactor }

func NewReadWalletLedger(transactor ports.Transactor) (*ReadWalletLedger, error) {
	if transactor == nil {
		return nil, ErrInvalidDependencies
	}
	return &ReadWalletLedger{transactor: transactor}, nil
}

func (r *ReadWalletLedger) Execute(ctx context.Context, walletID string, cursor *ports.LedgerCursor, limit int) (LedgerPage, error) {
	if limit < 1 || limit > 100 {
		return LedgerPage{}, ErrInvalidReadLimit
	}
	var page LedgerPage
	err := r.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		if _, err := unit.Wallets().Get(txctx, walletID); err != nil {
			return err
		}
		entries, err := unit.Ledger().ListByWallet(txctx, walletID, cursor, limit+1)
		if err != nil {
			return err
		}
		if len(entries) > limit {
			page.HasMore = true
			entries = entries[:limit]
		}
		page.Entries = entries
		return nil
	})
	return page, err
}
