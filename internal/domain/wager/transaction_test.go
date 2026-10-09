package wager

import (
	"errors"
	"testing"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
)

func TestNewExternalValidatesKindsAndAmounts(t *testing.T) {
	tests := []struct {
		name      string
		kind      Kind
		amount    string
		reference string
		wantErr   error
	}{
		{name: "bet requires positive amount", kind: Bet, amount: "10.00"},
		{name: "win requires positive amount", kind: Win, amount: "10.00"},
		{name: "loss requires zero", kind: Loss, amount: "0.00"},
		{name: "refund requires reference", kind: Refund, amount: "10.00", reference: "bet-1"},
		{name: "rollback requires reference", kind: Rollback, amount: "10.00", reference: "bet-1"},
		{name: "opening is internal", kind: Opening, amount: "10.00", wantErr: ErrInvalidKind},
		{name: "bet cannot be zero", kind: Bet, amount: "0.00", wantErr: ErrInvalidTransaction},
		{name: "loss cannot be positive", kind: Loss, amount: "0.01", wantErr: ErrInvalidTransaction},
		{name: "refund requires reference", kind: Refund, amount: "10.00", wantErr: ErrReferenceRequired},
		{name: "bet cannot carry reference", kind: Bet, amount: "10.00", reference: "bet-1", wantErr: ErrInvalidTransaction},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := newExternalParams(t, test.kind, test.amount, test.reference)
			_, err := NewExternal(params)
			if test.wantErr == nil && err != nil {
				t.Fatalf("NewExternal() error = %v", err)
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("NewExternal() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestTransactionTransitionsAndTerminalState(t *testing.T) {
	createdAt := testTime()
	transaction, err := NewExternal(newExternalParams(t, Bet, "10.00", ""))
	if err != nil {
		t.Fatal(err)
	}
	if transaction.Status() != Pending {
		t.Fatalf("initial status = %s, want %s", transaction.Status(), Pending)
	}

	balance := mustMoney(t, "90.00", "BRL")
	if err := transaction.MarkProcessed(balance, createdAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if transaction.Status() != Processed {
		t.Fatalf("status = %s, want %s", transaction.Status(), Processed)
	}
	result, ok := transaction.ResultBalance()
	if !ok {
		t.Fatal("processed transaction must retain its result balance")
	}
	if compare, err := result.Compare(balance); err != nil || compare != 0 {
		t.Fatalf("result balance = %v, want %v; error = %v", result, balance, err)
	}
	if err := transaction.Reject("INSUFFICIENT_FUNDS", createdAt.Add(2*time.Second)); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("Reject() on terminal transaction error = %v, want %v", err, ErrTerminalState)
	}
}

func TestReferenceWaitCanResumeProcessing(t *testing.T) {
	createdAt := testTime()
	transaction, err := NewExternal(newExternalParams(t, Refund, "10.00", "bet-external-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.MarkPendingReference(createdAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if transaction.Status() != PendingReference {
		t.Fatalf("status = %s, want %s", transaction.Status(), PendingReference)
	}
	if err := transaction.ResolveReference("transaction-1", createdAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if transaction.Status() != Pending || transaction.ReferenceTransactionID() != "transaction-1" {
		t.Fatalf("resolved transaction has status %s and reference %q", transaction.Status(), transaction.ReferenceTransactionID())
	}
	if err := transaction.MarkProcessed(mustMoney(t, "110.00", "BRL"), createdAt.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestTransactionRejectsInvalidTransitionsAndResults(t *testing.T) {
	createdAt := testTime()
	t.Run("non reversal cannot wait for reference", func(t *testing.T) {
		transaction, err := NewExternal(newExternalParams(t, Bet, "10.00", ""))
		if err != nil {
			t.Fatal(err)
		}
		if err := transaction.MarkPendingReference(createdAt.Add(time.Second)); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("MarkPendingReference() error = %v, want %v", err, ErrInvalidTransition)
		}
	})

	t.Run("incompatible result currency", func(t *testing.T) {
		transaction, err := NewExternal(newExternalParams(t, Bet, "10.00", ""))
		if err != nil {
			t.Fatal(err)
		}
		err = transaction.MarkProcessed(mustMoney(t, "90.00", "USD"), createdAt.Add(time.Second))
		if !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("MarkProcessed() error = %v, want %v", err, money.ErrCurrencyMismatch)
		}
	})

	t.Run("failure code must be stable uppercase identifier", func(t *testing.T) {
		transaction, err := NewExternal(newExternalParams(t, Bet, "10.00", ""))
		if err != nil {
			t.Fatal(err)
		}
		if err := transaction.Reject("insufficient funds", createdAt.Add(time.Second)); !errors.Is(err, ErrInvalidFailureCode) {
			t.Fatalf("Reject() error = %v, want %v", err, ErrInvalidFailureCode)
		}
	})
}

func TestRehydrateExternalDoesNotReplayTransitions(t *testing.T) {
	params := newExternalParams(t, Bet, "10.00", "")
	updatedAt := params.CreatedAt.Add(time.Minute)
	transaction, err := RehydrateExternal(params, Processed, "", "", mustMoney(t, "90.00", "BRL"), true, updatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if transaction.Status() != Processed || !transaction.UpdatedAt().Equal(updatedAt) {
		t.Fatalf("rehydrated transaction has status %s and updatedAt %s", transaction.Status(), transaction.UpdatedAt())
	}

	_, err = RehydrateExternal(params, Processed, "", "", money.Money{}, false, updatedAt)
	if !errors.Is(err, ErrResultRequired) {
		t.Fatalf("RehydrateExternal() error = %v, want %v", err, ErrResultRequired)
	}
}

func newExternalParams(t *testing.T, kind Kind, amount, reference string) ExternalParams {
	t.Helper()
	return ExternalParams{
		ID:                             "transaction-id",
		ProviderID:                     "provider-a",
		ExternalTransactionID:          "external-id",
		IdempotencyKey:                 "provider-a:external-id",
		PayloadHash:                    "payload-hash",
		WalletID:                       "wallet-id",
		PlayerID:                       "player-id",
		RoundID:                        "round-id",
		GameID:                         "game-id",
		Kind:                           kind,
		Money:                          mustMoney(t, amount, "BRL"),
		ReferenceExternalTransactionID: reference,
		CreatedAt:                      testTime(),
	}
}

func mustMoney(t *testing.T, amount, currency string) money.Money {
	t.Helper()
	value, err := money.Parse(amount, currency)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func testTime() time.Time {
	return time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
}
