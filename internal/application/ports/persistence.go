package ports

import (
	"context"
	"errors"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/application/events"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wager"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

var (
	ErrNotFound       = errors.New("record not found")
	ErrConflict       = errors.New("record conflict")
	ErrOptimisticLock = errors.New("concurrent update detected")
	ErrUnavailable    = errors.New("persistence unavailable")
)

type Transactor interface {
	WithinTransaction(context.Context, func(context.Context, UnitOfWork) error) error
}

type HealthChecker interface {
	Name() string
	Check(context.Context) error
}

type UnitOfWork interface {
	Wallets() WalletRepository
	Wagers() WagerRepository
	Ledger() LedgerRepository
	Outbox() OutboxRepository
}

type WalletRepository interface {
	Get(context.Context, string) (wallet.Wallet, error)
	Reconcile(context.Context, string) (WalletReconciliationSnapshot, error)
	GetForUpdate(context.Context, string) (wallet.Wallet, error)
	Create(context.Context, wallet.Wallet) error
	Save(context.Context, wallet.Wallet, int64) error
}

type WalletReconciliationSnapshot struct {
	StoredBalance     money.Money
	CalculatedBalance money.Money
	CheckedEntries    int64
}

type WagerRepository interface {
	GetByID(context.Context, string) (wager.Transaction, error)
	FindByIdempotencyKey(context.Context, string, string) (wager.Transaction, error)
	FindByExternalTransactionID(context.Context, string, string) (wager.Transaction, error)
	FindReference(context.Context, string, string) (wager.Transaction, error)
	FindDuePendingReference(context.Context, time.Time) (wager.Transaction, error)
	ClaimPendingReference(context.Context, string, time.Time, time.Time) (int, error)
	SchedulePendingReference(context.Context, string, time.Time) error
	FindProcessedReversals(context.Context, string) ([]wager.Transaction, error)
	Create(context.Context, wager.Transaction) error
	Save(context.Context, wager.Transaction) error
}

type LedgerRepository interface {
	Append(context.Context, wallet.LedgerEntry) error
	ListByWallet(context.Context, string, *LedgerCursor, int) ([]wallet.LedgerEntry, error)
}

type LedgerCursor struct {
	CreatedAt time.Time
	ID        string
}

type OutboxRepository interface {
	Append(context.Context, events.Event) error
}
