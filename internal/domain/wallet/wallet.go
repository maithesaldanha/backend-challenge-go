package wallet

import (
	"errors"
	"math"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
)

var (
	ErrInvalidIdentity   = errors.New("invalid wallet identity")
	ErrInvalidTimestamp  = errors.New("invalid wallet timestamp")
	ErrNegativeBalance   = errors.New("wallet balance cannot be negative")
	ErrInsufficientFunds = errors.New("insufficient funds")
	ErrInvalidMovement   = errors.New("movement must be positive")
	ErrVersionOverflow   = errors.New("wallet version overflow")
)

type Wallet struct {
	id        string
	playerID  string
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

type LedgerEntry struct {
	id            string
	walletID      string
	transactionID string
	direction     Direction
	money         money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	createdAt     time.Time
}

type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

func New(id, playerID string, initialBalance money.Money, now time.Time) (Wallet, error) {
	if id == "" || playerID == "" {
		return Wallet{}, ErrInvalidIdentity
	}
	if now.IsZero() || now.Location() != time.UTC {
		return Wallet{}, ErrInvalidTimestamp
	}
	if _, err := initialBalance.AmountMinor(); err != nil {
		return Wallet{}, err
	}
	if minor, _ := initialBalance.AmountMinor(); minor < 0 {
		return Wallet{}, ErrNegativeBalance
	}
	return Wallet{
		id:        id,
		playerID:  playerID,
		balance:   initialBalance,
		version:   1,
		createdAt: now,
		updatedAt: now,
	}, nil
}

func Rehydrate(id, playerID string, balance money.Money, version int64, createdAt, updatedAt time.Time) (Wallet, error) {
	wallet, err := New(id, playerID, balance, createdAt)
	if err != nil {
		return Wallet{}, err
	}
	if version < 1 {
		return Wallet{}, ErrInvalidIdentity
	}
	if updatedAt.IsZero() || updatedAt.Location() != time.UTC || updatedAt.Before(createdAt) {
		return Wallet{}, ErrInvalidTimestamp
	}
	wallet.version = version
	wallet.updatedAt = updatedAt
	return wallet, nil
}

func (w Wallet) ID() string { return w.id }

func (w Wallet) PlayerID() string { return w.playerID }

func (w Wallet) Balance() money.Money { return w.balance }

func (w Wallet) Version() int64 { return w.version }

func (w Wallet) CreatedAt() time.Time { return w.createdAt }

func (w Wallet) UpdatedAt() time.Time { return w.updatedAt }

func (w *Wallet) Debit(ledgerID, transactionID string, amount money.Money, now time.Time) (LedgerEntry, error) {
	return w.move(ledgerID, transactionID, amount, now, Debit)
}

func (w *Wallet) Credit(ledgerID, transactionID string, amount money.Money, now time.Time) (LedgerEntry, error) {
	return w.move(ledgerID, transactionID, amount, now, Credit)
}

func (w *Wallet) move(ledgerID, transactionID string, amount money.Money, now time.Time, direction Direction) (LedgerEntry, error) {
	if w.id == "" || ledgerID == "" || transactionID == "" {
		return LedgerEntry{}, ErrInvalidIdentity
	}
	if now.IsZero() || now.Location() != time.UTC || now.Before(w.updatedAt) {
		return LedgerEntry{}, ErrInvalidTimestamp
	}
	minor, err := amount.AmountMinor()
	if err != nil {
		return LedgerEntry{}, err
	}
	if minor <= 0 {
		return LedgerEntry{}, ErrInvalidMovement
	}
	before := w.balance
	var after money.Money
	if direction == Debit {
		after, err = before.Subtract(amount)
		if err == nil {
			if value, _ := after.AmountMinor(); value < 0 {
				err = ErrInsufficientFunds
			}
		}
	} else {
		after, err = before.Add(amount)
	}
	if err != nil {
		return LedgerEntry{}, err
	}
	if w.version == math.MaxInt64 {
		return LedgerEntry{}, ErrVersionOverflow
	}
	entry := LedgerEntry{
		id:            ledgerID,
		walletID:      w.id,
		transactionID: transactionID,
		direction:     direction,
		money:         amount,
		balanceBefore: before,
		balanceAfter:  after,
		createdAt:     now,
	}
	w.balance = after
	w.version++
	w.updatedAt = now
	return entry, nil
}

func (e LedgerEntry) ID() string { return e.id }

func (e LedgerEntry) WalletID() string { return e.walletID }

func (e LedgerEntry) TransactionID() string { return e.transactionID }

func (e LedgerEntry) Direction() Direction { return e.direction }

func (e LedgerEntry) Money() money.Money { return e.money }

func (e LedgerEntry) BalanceBefore() money.Money { return e.balanceBefore }

func (e LedgerEntry) BalanceAfter() money.Money { return e.balanceAfter }

func (e LedgerEntry) CreatedAt() time.Time { return e.createdAt }
