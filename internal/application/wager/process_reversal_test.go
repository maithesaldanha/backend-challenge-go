package wager

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	domainwager "github.com/junglegaming/backend-challenge-go/internal/domain/wager"
	domainwallet "github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

func TestProcessReversalRefundCreditsAndRejectsDuplicate(t *testing.T) {
	unit := newReversalTestUnit(t, "75.00")
	bet := seedReversalReference(t, unit, domainwager.Bet, "bet-1", "25.00", "75.00", nil)
	processor := newReversalProcessor(t, unit, testTime().Add(time.Minute))

	refund, err := processor.Execute(context.Background(), domainwager.Refund, reversalCommand(t, "refund-1", "refund-key-1", "25.00", bet.ExternalTransactionID()))
	if err != nil {
		t.Fatalf("refund failed: %v", err)
	}
	if refund.Transaction.Status() != domainwager.Processed {
		t.Fatalf("refund status = %q, want PROCESSED", refund.Transaction.Status())
	}
	assertMoney(t, unit.wallets.account.Balance(), "100.00")
	if len(unit.ledger.entries) != 1 || unit.ledger.entries[0].Direction() != domainwallet.Credit {
		t.Fatalf("refund ledger = %#v, want one credit", unit.ledger.entries)
	}

	duplicate, err := processor.Execute(context.Background(), domainwager.Refund, reversalCommand(t, "refund-2", "refund-key-2", "25.00", bet.ExternalTransactionID()))
	if err != nil {
		t.Fatalf("duplicate refund failed: %v", err)
	}
	if duplicate.Transaction.Status() != domainwager.Rejected || duplicate.Transaction.FailureCode() != "DUPLICATE_REVERSAL" {
		t.Fatalf("duplicate refund = (%q, %q), want REJECTED and DUPLICATE_REVERSAL", duplicate.Transaction.Status(), duplicate.Transaction.FailureCode())
	}
	assertMoney(t, unit.wallets.account.Balance(), "100.00")
	if len(unit.ledger.entries) != 1 {
		t.Fatalf("duplicate refund added a ledger entry: got %d", len(unit.ledger.entries))
	}
}

func TestProcessReversalPreventsRefundAndRollbackFromReversingBetTwice(t *testing.T) {
	unit := newReversalTestUnit(t, "75.00")
	bet := seedReversalReference(t, unit, domainwager.Bet, "bet-conflict", "25.00", "75.00", nil)
	processor := newReversalProcessor(t, unit, testTime().Add(time.Minute))

	refund, err := processor.Execute(context.Background(), domainwager.Refund, reversalCommand(t, "refund-conflict", "refund-conflict-key", "25.00", bet.ExternalTransactionID()))
	if err != nil || refund.Transaction.Status() != domainwager.Processed {
		t.Fatalf("initial refund = (%q, %v), want PROCESSED and no error", refund.Transaction.Status(), err)
	}
	rollback, err := processor.Execute(context.Background(), domainwager.Rollback, reversalCommand(t, "rollback-conflict", "rollback-conflict-key", "25.00", bet.ExternalTransactionID()))
	if err != nil {
		t.Fatalf("rollback failed: %v", err)
	}
	if rollback.Transaction.Status() != domainwager.Rejected || rollback.Transaction.FailureCode() != "CONFLICTING_REVERSAL" {
		t.Fatalf("rollback = (%q, %q), want REJECTED and CONFLICTING_REVERSAL", rollback.Transaction.Status(), rollback.Transaction.FailureCode())
	}
	assertMoney(t, unit.wallets.account.Balance(), "100.00")
	if len(unit.ledger.entries) != 1 {
		t.Fatalf("conflicting rollback added a ledger entry: got %d", len(unit.ledger.entries))
	}
}

func TestProcessReversalRollbackDebitsWinAndRejectsInsufficientFunds(t *testing.T) {
	t.Run("credits the original bet", func(t *testing.T) {
		unit := newReversalTestUnit(t, "75.00")
		bet := seedReversalReference(t, unit, domainwager.Bet, "bet-rollback-1", "25.00", "75.00", nil)
		processor := newReversalProcessor(t, unit, testTime().Add(time.Minute))

		result, err := processor.Execute(context.Background(), domainwager.Rollback, reversalCommand(t, "rollback-bet-1", "rollback-bet-key-1", "25.00", bet.ExternalTransactionID()))
		if err != nil {
			t.Fatalf("rollback failed: %v", err)
		}
		if result.Transaction.Status() != domainwager.Processed {
			t.Fatalf("rollback status = %q, want PROCESSED", result.Transaction.Status())
		}
		assertMoney(t, unit.wallets.account.Balance(), "100.00")
		if len(unit.ledger.entries) != 1 || unit.ledger.entries[0].Direction() != domainwallet.Credit {
			t.Fatalf("rollback ledger = %#v, want one credit", unit.ledger.entries)
		}
	})

	t.Run("debits the original win", func(t *testing.T) {
		unit := newReversalTestUnit(t, "125.00")
		win := seedReversalReference(t, unit, domainwager.Win, "win-1", "25.00", "125.00", nil)
		processor := newReversalProcessor(t, unit, testTime().Add(time.Minute))

		result, err := processor.Execute(context.Background(), domainwager.Rollback, reversalCommand(t, "rollback-1", "rollback-key-1", "25.00", win.ExternalTransactionID()))
		if err != nil {
			t.Fatalf("rollback failed: %v", err)
		}
		if result.Transaction.Status() != domainwager.Processed {
			t.Fatalf("rollback status = %q, want PROCESSED", result.Transaction.Status())
		}
		assertMoney(t, unit.wallets.account.Balance(), "100.00")
		if len(unit.ledger.entries) != 1 || unit.ledger.entries[0].Direction() != domainwallet.Debit {
			t.Fatalf("rollback ledger = %#v, want one debit", unit.ledger.entries)
		}
	})

	t.Run("debits the original refund", func(t *testing.T) {
		unit := newReversalTestUnit(t, "100.00")
		bet := seedReversalReference(t, unit, domainwager.Bet, "bet-for-refund", "25.00", "75.00", nil)
		refund := seedReversalReference(t, unit, domainwager.Refund, "refund-for-rollback", "25.00", "100.00", &bet)
		processor := newReversalProcessor(t, unit, testTime().Add(2*time.Minute))

		result, err := processor.Execute(context.Background(), domainwager.Rollback, reversalCommand(t, "rollback-refund-1", "rollback-refund-key-1", "25.00", refund.ExternalTransactionID()))
		if err != nil {
			t.Fatalf("rollback failed: %v", err)
		}
		if result.Transaction.Status() != domainwager.Processed {
			t.Fatalf("rollback status = %q, want PROCESSED", result.Transaction.Status())
		}
		assertMoney(t, unit.wallets.account.Balance(), "75.00")
		if len(unit.ledger.entries) != 1 || unit.ledger.entries[0].Direction() != domainwallet.Debit {
			t.Fatalf("rollback ledger = %#v, want one debit", unit.ledger.entries)
		}
	})

	t.Run("rejects without changing balance", func(t *testing.T) {
		unit := newReversalTestUnit(t, "10.00")
		win := seedReversalReference(t, unit, domainwager.Win, "win-2", "25.00", "35.00", nil)
		processor := newReversalProcessor(t, unit, testTime().Add(time.Minute))

		result, err := processor.Execute(context.Background(), domainwager.Rollback, reversalCommand(t, "rollback-2", "rollback-key-2", "25.00", win.ExternalTransactionID()))
		if err != nil {
			t.Fatalf("rollback failed: %v", err)
		}
		if result.Transaction.Status() != domainwager.Rejected || result.Transaction.FailureCode() != "INSUFFICIENT_FUNDS_FOR_ROLLBACK" {
			t.Fatalf("rollback = (%q, %q), want REJECTED and INSUFFICIENT_FUNDS_FOR_ROLLBACK", result.Transaction.Status(), result.Transaction.FailureCode())
		}
		assertMoney(t, unit.wallets.account.Balance(), "10.00")
		if len(unit.ledger.entries) != 0 {
			t.Fatalf("rejected rollback wrote %d ledger entries", len(unit.ledger.entries))
		}
	})
}

func TestProcessReversalPendingReferenceIsProcessedByRetryWorker(t *testing.T) {
	unit := newReversalTestUnit(t, "75.00")
	processor := newReversalProcessor(t, unit, testTime())
	command := reversalCommand(t, "refund-pending", "refund-pending-key", "25.00", "bet-arrives-later")

	pending, err := processor.Execute(context.Background(), domainwager.Refund, command)
	if err != nil {
		t.Fatalf("refund submission failed: %v", err)
	}
	if pending.Transaction.Status() != domainwager.PendingReference {
		t.Fatalf("pending refund status = %q, want PENDING_REFERENCE", pending.Transaction.Status())
	}
	assertMoney(t, unit.wallets.account.Balance(), "75.00")
	if len(unit.ledger.entries) != 0 {
		t.Fatalf("pending refund wrote %d ledger entries", len(unit.ledger.entries))
	}

	seedReversalReference(t, unit, domainwager.Bet, "bet-arrives-later", "25.00", "75.00", nil)
	ids := 0
	worker, err := NewRetryPendingReferences(&memoryTransactor{unit: unit}, func() string {
		ids++
		return fmt.Sprintf("retry-event-%d", ids)
	}, func() time.Time { return testTime().Add(2 * time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.ProcessBatch(context.Background(), 1)
	if err != nil {
		t.Fatalf("retry batch failed: %v", err)
	}
	if processed != 1 {
		t.Fatalf("retry batch processed %d transactions, want 1", processed)
	}
	resolved, err := unit.wagers.FindByExternalTransactionID(context.Background(), "provider-1", "refund-pending")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status() != domainwager.Processed {
		t.Fatalf("retried refund status = %q, want PROCESSED", resolved.Status())
	}
	assertMoney(t, unit.wallets.account.Balance(), "100.00")
	if len(unit.ledger.entries) != 1 || len(unit.outbox.events) != 3 {
		t.Fatalf("retry wrote %d ledger entries and %d outbox events, want 1 and 3", len(unit.ledger.entries), len(unit.outbox.events))
	}
}

func newReversalTestUnit(t *testing.T, balance string) *memoryUnit {
	t.Helper()
	account, err := domainwallet.New("wallet-1", "player-1", mustMoney(t, balance), testTime())
	if err != nil {
		t.Fatal(err)
	}
	return newMemoryUnit(account)
}

func newReversalProcessor(t *testing.T, unit *memoryUnit, now time.Time) *ProcessReversal {
	t.Helper()
	ids := 0
	processor, err := NewProcessReversal(&memoryTransactor{unit: unit}, func() string {
		ids++
		return fmt.Sprintf("reversal-generated-%d", ids)
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func reversalCommand(t *testing.T, externalID, key, amount, referenceExternalID string) ReversalCommand {
	t.Helper()
	command := testBetCommand(t, externalID, key, amount)
	command.ReferenceExternalTransactionID = referenceExternalID
	return command
}

func seedReversalReference(t *testing.T, unit *memoryUnit, kind domainwager.Kind, externalID, amount, resultBalance string, reference *domainwager.Transaction) domainwager.Transaction {
	t.Helper()
	params := domainwager.ExternalParams{
		ID:                    "seed-" + externalID,
		ProviderID:            "provider-1",
		ExternalTransactionID: externalID,
		IdempotencyKey:        "seed-key-" + externalID,
		PayloadHash:           strings.Repeat("b", 64),
		WalletID:              "wallet-1",
		PlayerID:              "player-1",
		RoundID:               "round-1",
		GameID:                "game-1",
		Kind:                  kind,
		Money:                 mustMoney(t, amount),
		CreatedAt:             testTime(),
	}
	if reference != nil {
		params.ReferenceExternalTransactionID = reference.ExternalTransactionID()
	}
	transaction, err := domainwager.NewExternal(params)
	if err != nil {
		t.Fatal(err)
	}
	if reference != nil {
		if err := transaction.ResolveReference(*reference, nil, testTime().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := transaction.MarkProcessed(mustMoney(t, resultBalance), testTime().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := unit.wagers.Create(context.Background(), transaction); err != nil {
		t.Fatal(err)
	}
	return transaction
}
