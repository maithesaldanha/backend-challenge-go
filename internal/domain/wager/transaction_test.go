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
	bet := newProcessedTransaction(t, Bet, "transaction-1", "bet-external-1", "10.00", "", "", "90.00")
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
	wrongBet := newProcessedTransaction(t, Bet, "wrong-bet", "bet-external-1", "10.00", "", "", "90.00")
	wrongBet.playerID = "different-player"
	if err := transaction.ResolveReference(wrongBet, nil, createdAt.Add(2*time.Second)); !errors.Is(err, ErrReferenceMismatch) {
		t.Fatalf("ResolveReference() error = %v, want %v", err, ErrReferenceMismatch)
	}
	if transaction.Status() != PendingReference {
		t.Fatal("failed reference resolution must preserve the pending reference state")
	}
	if err := transaction.ResolveReference(bet, nil, createdAt.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if transaction.Status() != Pending || transaction.ReferenceTransactionID() != "transaction-1" {
		t.Fatalf("resolved transaction has status %s and reference %q", transaction.Status(), transaction.ReferenceTransactionID())
	}
	if err := transaction.MarkProcessed(mustMoney(t, "110.00", "BRL"), createdAt.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestWinMayReferenceBetWithoutMatchingBetAmount(t *testing.T) {
	createdAt := testTime()
	win, err := NewExternal(newExternalParams(t, Win, "15.00", "bet-external"))
	if err != nil {
		t.Fatal(err)
	}
	if err := win.MarkProcessed(mustMoney(t, "115.00", "BRL"), createdAt.Add(time.Second)); !errors.Is(err, ErrReferenceRequired) {
		t.Fatalf("MarkProcessed() error = %v, want %v", err, ErrReferenceRequired)
	}
	if err := win.MarkPendingReference(createdAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	bet := newProcessedTransaction(t, Bet, "bet-internal", "bet-external", "25.00", "", "", "75.00")
	if err := win.ResolveReference(bet, nil, createdAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := win.MarkProcessed(mustMoney(t, "115.00", "BRL"), createdAt.Add(3*time.Second)); err != nil {
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

	_, err = RehydrateExternal(params, Pending, "", "", mustMoney(t, "90.00", "BRL"), false, updatedAt)
	if !errors.Is(err, ErrInvalidStatus) {
		t.Fatalf("RehydrateExternal() error = %v, want %v", err, ErrInvalidStatus)
	}

	winParams := newExternalParams(t, Win, "10.00", "")
	_, err = RehydrateExternal(winParams, Pending, "unexpected-reference", "", money.Money{}, false, updatedAt)
	if !errors.Is(err, ErrInvalidTransaction) {
		t.Fatalf("RehydrateExternal() error = %v, want %v", err, ErrInvalidTransaction)
	}
}

func TestRehydrateOpeningKeepsInternalProcessedState(t *testing.T) {
	createdAt := testTime()
	updatedAt := createdAt.Add(time.Minute)
	transaction, err := RehydrateOpening("opening-id", "wallet-id", "player-id", mustMoney(t, "100.00", "BRL"), createdAt, updatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if transaction.Kind() != Opening || transaction.Status() != Processed || !transaction.UpdatedAt().Equal(updatedAt) {
		t.Fatalf("rehydrated opening has kind %s, status %s and updatedAt %s", transaction.Kind(), transaction.Status(), transaction.UpdatedAt())
	}
}

func TestValidateReferenceRequiresMatchingProcessedOperation(t *testing.T) {
	bet := newProcessedTransaction(t, Bet, "bet-internal", "bet-external", "25.00", "", "", "75.00")
	refund := newPendingTransaction(t, Refund, "refund-internal", "refund-external", "provider-a", "player-id", "wallet-id", "round-id", "25.00", "bet-external")

	if err := refund.ValidateReference(bet, nil); err != nil {
		t.Fatalf("ValidateReference() error = %v", err)
	}

	unprocessedBet, err := NewExternal(newExternalWithIDs(t, Bet, "pending-bet", "pending-bet-external", "provider-a", "player-id", "wallet-id", "round-id", "25.00", ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := refund.ValidateReference(unprocessedBet, nil); !errors.Is(err, ErrReferenceNotProcessed) {
		t.Fatalf("ValidateReference() error = %v, want %v", err, ErrReferenceNotProcessed)
	}

	wrongPlayer := newProcessedTransaction(t, Bet, "other-bet", "bet-external", "25.00", "", "", "75.00")
	wrongPlayer.playerID = "other-player"
	if err := refund.ValidateReference(wrongPlayer, nil); !errors.Is(err, ErrReferenceMismatch) {
		t.Fatalf("ValidateReference() error = %v, want %v", err, ErrReferenceMismatch)
	}

	wrongAmount := newProcessedTransaction(t, Bet, "other-amount", "bet-external", "24.99", "", "", "75.01")
	if err := refund.ValidateReference(wrongAmount, nil); !errors.Is(err, ErrReferenceMismatch) {
		t.Fatalf("ValidateReference() error = %v, want %v", err, ErrReferenceMismatch)
	}

	win := newProcessedTransaction(t, Win, "win-internal", "win-external", "25.00", "", "", "125.00")
	refundForWin := newPendingTransaction(t, Refund, "refund-for-win", "refund-for-win-external", "provider-a", "player-id", "wallet-id", "round-id", "25.00", "win-external")
	if err := refundForWin.ValidateReference(win, nil); !errors.Is(err, ErrInvalidReferenceKind) {
		t.Fatalf("ValidateReference() error = %v, want %v", err, ErrInvalidReferenceKind)
	}
}

func TestValidateReferencePreventsDuplicateAndConflictingBetReversals(t *testing.T) {
	bet := newProcessedTransaction(t, Bet, "bet-internal", "bet-external", "25.00", "", "", "75.00")
	refund := newProcessedTransaction(t, Refund, "refund-internal", "refund-external", "25.00", "bet-external", bet.ID(), "100.00")
	secondRefund := newPendingTransaction(t, Refund, "refund-2-internal", "refund-2-external", "provider-a", "player-id", "wallet-id", "round-id", "25.00", "bet-external")
	if err := secondRefund.ValidateReference(bet, []Transaction{refund}); !errors.Is(err, ErrDuplicateReversal) {
		t.Fatalf("ValidateReference() error = %v, want %v", err, ErrDuplicateReversal)
	}

	rollbackBet := newPendingTransaction(t, Rollback, "rollback-bet", "rollback-bet-external", "provider-a", "player-id", "wallet-id", "round-id", "25.00", "bet-external")
	if err := rollbackBet.ValidateReference(bet, []Transaction{refund}); !errors.Is(err, ErrConflictingReversal) {
		t.Fatalf("ValidateReference() error = %v, want %v", err, ErrConflictingReversal)
	}
}

func TestRollbackOfRefundIsAllowed(t *testing.T) {
	bet := newProcessedTransaction(t, Bet, "bet-internal", "bet-external", "25.00", "", "", "75.00")
	refund := newProcessedTransaction(t, Refund, "refund-internal", "refund-external", "25.00", "bet-external", bet.ID(), "100.00")
	rollback := newPendingTransaction(t, Rollback, "rollback-refund", "rollback-refund-external", "provider-a", "player-id", "wallet-id", "round-id", "25.00", "refund-external")

	if err := rollback.ValidateReference(refund, []Transaction{refund}); err != nil {
		t.Fatalf("ValidateReference() error = %v", err)
	}
}

func newExternalParams(t *testing.T, kind Kind, amount, reference string) ExternalParams {
	t.Helper()
	return newExternalWithIDs(t, kind, "transaction-id", "external-id", "provider-a", "player-id", "wallet-id", "round-id", amount, reference)
}

func newExternalWithIDs(t *testing.T, kind Kind, id, externalID, providerID, playerID, walletID, roundID, amount, reference string) ExternalParams {
	t.Helper()
	return ExternalParams{
		ID:                             id,
		ProviderID:                     providerID,
		ExternalTransactionID:          externalID,
		IdempotencyKey:                 providerID + ":" + externalID,
		PayloadHash:                    "payload-hash",
		WalletID:                       walletID,
		PlayerID:                       playerID,
		RoundID:                        roundID,
		GameID:                         "game-id",
		Kind:                           kind,
		Money:                          mustMoney(t, amount, "BRL"),
		ReferenceExternalTransactionID: reference,
		CreatedAt:                      testTime(),
	}
}

func newProcessedTransaction(t *testing.T, kind Kind, id, externalID, amount, referenceExternalID, referenceInternalID, balance string) Transaction {
	t.Helper()
	params := newExternalWithIDs(t, kind, id, externalID, "provider-a", "player-id", "wallet-id", "round-id", amount, referenceExternalID)
	if kind == Refund || kind == Rollback {
		transaction, err := RehydrateExternal(params, Processed, referenceInternalID, "", mustMoney(t, balance, "BRL"), true, testTime().Add(3*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		return transaction
	}
	transaction, err := NewExternal(params)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.MarkProcessed(mustMoney(t, balance, "BRL"), testTime().Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	return transaction
}

func newPendingTransaction(t *testing.T, kind Kind, id, externalID, providerID, playerID, walletID, roundID, amount, reference string) Transaction {
	t.Helper()
	transaction, err := NewExternal(newExternalWithIDs(t, kind, id, externalID, providerID, playerID, walletID, roundID, amount, reference))
	if err != nil {
		t.Fatal(err)
	}
	return transaction
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
