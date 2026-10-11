package wallet

import (
	"testing"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
)

func TestWalletMovesUpdateBalanceVersionAndLedger(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	initial, _ := money.NewExternal("100.00", "BRL")
	account, err := New("wallet-1", "player-1", initial, now)
	if err != nil {
		t.Fatal(err)
	}

	debitAmount, _ := money.NewExternal("25.00", "BRL")
	entry, err := account.Debit("ledger-1", "transaction-1", debitAmount, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertMoney(t, entry.BalanceBefore(), "100.00")
	assertMoney(t, entry.BalanceAfter(), "75.00")
	if entry.Direction() != Debit || account.Version() != 2 {
		t.Fatalf("direction=%s version=%d, want DEBIT and version 2", entry.Direction(), account.Version())
	}

	creditAmount, _ := money.NewExternal("5.00", "BRL")
	entry, err = account.Credit("ledger-2", "transaction-2", creditAmount, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertMoney(t, entry.BalanceBefore(), "75.00")
	assertMoney(t, entry.BalanceAfter(), "80.00")
	assertMoney(t, account.Balance(), "80.00")
	if entry.Direction() != Credit || account.Version() != 3 {
		t.Fatalf("direction=%s version=%d, want CREDIT and version 3", entry.Direction(), account.Version())
	}
}

func TestRejectedMovementDoesNotChangeWallet(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	initial, _ := money.NewExternal("10.00", "BRL")
	account, err := New("wallet-1", "player-1", initial, now)
	if err != nil {
		t.Fatal(err)
	}
	tooMuch, _ := money.NewExternal("11.00", "BRL")

	if _, err := account.Debit("ledger-1", "transaction-1", tooMuch, now.Add(time.Second)); err != ErrInsufficientFunds {
		t.Fatalf("Debit() error = %v, want %v", err, ErrInsufficientFunds)
	}
	assertMoney(t, account.Balance(), "10.00")
	if account.Version() != 1 || !account.UpdatedAt().Equal(now) {
		t.Fatalf("rejected movement changed wallet: version=%d updatedAt=%s", account.Version(), account.UpdatedAt())
	}
}

func TestLedgerRehydrationValidatesArithmeticAndCurrency(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	amount, _ := money.NewExternal("3.00", "BRL")
	before, _ := money.NewExternal("10.00", "BRL")
	wrongAfter, _ := money.NewExternal("14.00", "BRL")
	if _, err := RehydrateLedgerEntry("entry-1", "wallet-1", "transaction-1", Credit, amount, before, wrongAfter, now); err != ErrInvalidMovement {
		t.Fatalf("RehydrateLedgerEntry() error = %v, want %v", err, ErrInvalidMovement)
	}
	usdAfter, _ := money.NewExternal("13.00", "USD")
	if _, err := RehydrateLedgerEntry("entry-1", "wallet-1", "transaction-1", Credit, amount, before, usdAfter, now); err != money.ErrCurrencyMismatch {
		t.Fatalf("RehydrateLedgerEntry() error = %v, want %v", err, money.ErrCurrencyMismatch)
	}
}

func assertMoney(t *testing.T, value money.Money, want string) {
	t.Helper()
	got, err := value.String()
	if err != nil || got != want {
		t.Fatalf("Money.String() = %q, %v; want %q", got, err, want)
	}
}
