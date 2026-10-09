package wager

import (
	"context"
	"testing"

	domainwager "github.com/junglegaming/backend-challenge-go/internal/domain/wager"
	domainwallet "github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

func TestProcessLossRecordsTransactionWithoutChangingWallet(t *testing.T) {
	account, err := domainwallet.New("wallet-1", "player-1", mustMoney(t, "100.00"), testTime())
	if err != nil {
		t.Fatal(err)
	}
	unit := newMemoryUnit(account)
	processor, err := NewProcessLoss(&memoryTransactor{unit: unit}, func() string {
		return "loss-generated-id"
	}, testTime)
	if err != nil {
		t.Fatal(err)
	}
	command := LossCommand(testBetCommand(t, "external-loss-1", "key-loss-1", "0.00"))

	result, err := processor.Execute(context.Background(), command)
	if err != nil {
		t.Fatalf("loss execution failed: %v", err)
	}
	if result.Transaction.Kind() != domainwager.Loss || result.Transaction.Status() != domainwager.Processed {
		t.Fatalf("transaction = (%q, %q), want LOSS and PROCESSED", result.Transaction.Kind(), result.Transaction.Status())
	}
	assertMoney(t, unit.wallets.account.Balance(), "100.00")
	if unit.wallets.account.Version() != 1 {
		t.Fatalf("wallet version = %d, want unchanged version 1", unit.wallets.account.Version())
	}
	if len(unit.ledger.entries) != 0 || len(unit.outbox.events) != 1 {
		t.Fatalf("loss wrote %d ledger entries and %d outbox events, want 0 and 1", len(unit.ledger.entries), len(unit.outbox.events))
	}

	replay, err := processor.Execute(context.Background(), command)
	if err != nil {
		t.Fatalf("loss replay failed: %v", err)
	}
	if replay.Transaction.ID() != result.Transaction.ID() || !replay.IdempotentReplay {
		t.Fatalf("replay = (%q, %t), want original transaction %q and replay true", replay.Transaction.ID(), replay.IdempotentReplay, result.Transaction.ID())
	}
	if unit.wallets.account.Version() != 1 || len(unit.ledger.entries) != 0 || len(unit.outbox.events) != 1 {
		t.Fatal("loss replay changed wallet version, ledger, or outbox")
	}
}
