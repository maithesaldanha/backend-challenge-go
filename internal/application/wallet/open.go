package wallet

import (
	"context"
	"errors"
	"time"

	appevents "github.com/junglegaming/backend-challenge-go/internal/application/events"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wager"
	domainwallet "github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

var ErrInvalidDependencies = errors.New("invalid open wallet dependencies")

type OpenWalletCommand struct {
	PlayerID       string
	InitialBalance money.Money
}

type OpenWalletResult struct {
	Wallet             domainwallet.Wallet
	OpeningTransaction *wager.Transaction
}

type OpenWallet struct {
	transactor ports.Transactor
	newID      func() string
	now        func() time.Time
}

func NewOpenWallet(transactor ports.Transactor, newID func() string, now func() time.Time) (*OpenWallet, error) {
	if transactor == nil || newID == nil || now == nil {
		return nil, ErrInvalidDependencies
	}
	return &OpenWallet{transactor: transactor, newID: newID, now: now}, nil
}

func (o *OpenWallet) Execute(ctx context.Context, command OpenWalletCommand) (OpenWalletResult, error) {
	now := o.now()
	account, err := domainwallet.New(o.newID(), command.PlayerID, command.InitialBalance, now)
	if err != nil {
		return OpenWalletResult{}, err
	}
	result := OpenWalletResult{Wallet: account}

	err = o.transactor.WithinTransaction(ctx, func(txctx context.Context, unit ports.UnitOfWork) error {
		if err := unit.Wallets().Create(txctx, account); err != nil {
			return err
		}
		if account.Balance().IsZero() {
			return nil
		}

		opening, err := wager.NewOpening(o.newID(), account.ID(), account.PlayerID(), account.Balance(), now)
		if err != nil {
			return err
		}
		entry, err := domainwallet.NewOpeningEntry(o.newID(), account.ID(), opening.ID(), account.Balance(), now)
		if err != nil {
			return err
		}
		if err := unit.Wagers().Create(txctx, opening); err != nil {
			return err
		}
		if err := unit.Ledger().Append(txctx, entry); err != nil {
			return err
		}
		processedEvent, err := appevents.NewWagerTransactionProcessed(o.newID(), account.ID(), opening, now)
		if err != nil {
			return err
		}
		if err := unit.Outbox().Append(txctx, processedEvent); err != nil {
			return err
		}
		balanceEvent, err := appevents.NewWalletBalanceChanged(o.newID(), account.ID(), entry, account.Version(), now)
		if err != nil {
			return err
		}
		if err := unit.Outbox().Append(txctx, balanceEvent); err != nil {
			return err
		}
		result.OpeningTransaction = &opening
		return nil
	})
	if err != nil {
		return OpenWalletResult{}, err
	}
	return result, nil
}
