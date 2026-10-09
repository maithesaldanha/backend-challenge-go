package wager

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/application/events"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	domainwager "github.com/junglegaming/backend-challenge-go/internal/domain/wager"
	domainwallet "github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

func TestProcessBetProcessesAndReplaysWithoutAnotherDebit(t *testing.T) {
	account, err := domainwallet.New("wallet-1", "player-1", mustMoney(t, "100.00"), testTime())
	if err != nil {
		t.Fatal(err)
	}
	unit := newMemoryUnit(account)
	processor := newTestProcessor(t, unit)
	command := testBetCommand(t, "external-1", "key-1", "25.00")

	first, err := processor.Execute(context.Background(), command)
	if err != nil {
		t.Fatalf("first execution failed: %v", err)
	}
	if first.Transaction.ID() == "" || first.Transaction.Status() != domainwager.Processed {
		t.Fatalf("first transaction = (%q, %q), want non-empty ID and PROCESSED", first.Transaction.ID(), first.Transaction.Status())
	}
	if first.IdempotentReplay {
		t.Fatal("first execution unexpectedly reported a replay")
	}
	assertMoney(t, unit.wallets.account.Balance(), "75.00")
	if len(unit.ledger.entries) != 1 || len(unit.outbox.events) != 2 {
		t.Fatalf("first execution wrote %d ledger entries and %d outbox events, want 1 and 2", len(unit.ledger.entries), len(unit.outbox.events))
	}

	replay, err := processor.Execute(context.Background(), command)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if replay.Transaction.ID() != first.Transaction.ID() || !replay.IdempotentReplay {
		t.Fatalf("replay = (%q, %t), want original transaction %q and replay true", replay.Transaction.ID(), replay.IdempotentReplay, first.Transaction.ID())
	}
	assertMoney(t, unit.wallets.account.Balance(), "75.00")
	if len(unit.ledger.entries) != 1 || len(unit.outbox.events) != 2 {
		t.Fatalf("replay changed ledger or outbox: %d entries and %d events", len(unit.ledger.entries), len(unit.outbox.events))
	}
}

func TestProcessBetRejectsInsufficientFundsWithoutChangingWallet(t *testing.T) {
	account, err := domainwallet.New("wallet-1", "player-1", mustMoney(t, "20.00"), testTime())
	if err != nil {
		t.Fatal(err)
	}
	unit := newMemoryUnit(account)
	processor := newTestProcessor(t, unit)

	result, err := processor.Execute(context.Background(), testBetCommand(t, "external-2", "key-2", "25.00"))
	if err != nil {
		t.Fatalf("execution failed: %v", err)
	}
	if result.Transaction.Status() != domainwager.Rejected || result.Transaction.FailureCode() != "INSUFFICIENT_FUNDS" {
		t.Fatalf("transaction = (%q, %q), want REJECTED and INSUFFICIENT_FUNDS", result.Transaction.Status(), result.Transaction.FailureCode())
	}
	assertMoney(t, unit.wallets.account.Balance(), "20.00")
	if len(unit.ledger.entries) != 0 || len(unit.outbox.events) != 1 {
		t.Fatalf("rejected bet wrote %d ledger entries and %d outbox events, want 0 and 1", len(unit.ledger.entries), len(unit.outbox.events))
	}
}

func newTestProcessor(t *testing.T, unit *memoryUnit) *ProcessBet {
	t.Helper()
	ids := 0
	processor, err := NewProcessBet(&memoryTransactor{unit: unit}, func() string {
		ids++
		return fmt.Sprintf("generated-%d", ids)
	}, testTime)
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func testBetCommand(t *testing.T, externalID, key, amount string) BetCommand {
	t.Helper()
	return BetCommand{
		ProviderID:            "provider-1",
		ExternalTransactionID: externalID,
		IdempotencyKey:        key,
		PayloadHash:           strings.Repeat("a", 64),
		WalletID:              "wallet-1",
		PlayerID:              "player-1",
		RoundID:               "round-1",
		GameID:                "game-1",
		Money:                 mustMoney(t, amount),
	}
}

func mustMoney(t *testing.T, amount string) money.Money {
	t.Helper()
	value, err := money.NewExternal(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func assertMoney(t *testing.T, actual money.Money, expected string) {
	t.Helper()
	value, err := actual.String()
	if err != nil {
		t.Fatal(err)
	}
	if value != expected {
		t.Fatalf("money = %s, want %s", value, expected)
	}
}

func testTime() time.Time {
	return time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC)
}

type memoryTransactor struct{ unit *memoryUnit }

func (t *memoryTransactor) WithinTransaction(ctx context.Context, fn func(context.Context, ports.UnitOfWork) error) error {
	return fn(ctx, t.unit)
}

type memoryUnit struct {
	wallets *memoryWallets
	wagers  *memoryWagers
	ledger  *memoryLedger
	outbox  *memoryOutbox
}

func newMemoryUnit(account domainwallet.Wallet) *memoryUnit {
	return &memoryUnit{
		wallets: &memoryWallets{account: account},
		wagers:  &memoryWagers{byKey: make(map[string]domainwager.Transaction), byExternalID: make(map[string]domainwager.Transaction), attempts: make(map[string]int), retryAt: make(map[string]time.Time), leaseUntil: make(map[string]time.Time)},
		ledger:  &memoryLedger{},
		outbox:  &memoryOutbox{},
	}
}

func (u *memoryUnit) Wallets() ports.WalletRepository { return u.wallets }
func (u *memoryUnit) Wagers() ports.WagerRepository   { return u.wagers }
func (u *memoryUnit) Ledger() ports.LedgerRepository  { return u.ledger }
func (u *memoryUnit) Outbox() ports.OutboxRepository  { return u.outbox }

type memoryWallets struct{ account domainwallet.Wallet }

func (r *memoryWallets) GetForUpdate(_ context.Context, id string) (domainwallet.Wallet, error) {
	if id != r.account.ID() {
		return domainwallet.Wallet{}, ports.ErrNotFound
	}
	return r.account, nil
}

func (r *memoryWallets) Create(_ context.Context, account domainwallet.Wallet) error {
	r.account = account
	return nil
}

func (r *memoryWallets) Save(_ context.Context, account domainwallet.Wallet, expectedVersion int64) error {
	if r.account.Version() != expectedVersion {
		return ports.ErrOptimisticLock
	}
	r.account = account
	return nil
}

type memoryWagers struct {
	byKey        map[string]domainwager.Transaction
	byExternalID map[string]domainwager.Transaction
	attempts     map[string]int
	retryAt      map[string]time.Time
	leaseUntil   map[string]time.Time
}

func (r *memoryWagers) GetByID(_ context.Context, id string) (domainwager.Transaction, error) {
	for _, transaction := range r.byKey {
		if transaction.ID() == id {
			return transaction, nil
		}
	}
	return domainwager.Transaction{}, ports.ErrNotFound
}

func (r *memoryWagers) FindByIdempotencyKey(_ context.Context, providerID, key string) (domainwager.Transaction, error) {
	transaction, ok := r.byKey[providerID+"/"+key]
	if !ok {
		return domainwager.Transaction{}, ports.ErrNotFound
	}
	return transaction, nil
}

func (r *memoryWagers) FindByExternalTransactionID(_ context.Context, providerID, externalID string) (domainwager.Transaction, error) {
	transaction, ok := r.byExternalID[providerID+"/"+externalID]
	if !ok {
		return domainwager.Transaction{}, ports.ErrNotFound
	}
	return transaction, nil
}

func (r *memoryWagers) FindReference(ctx context.Context, providerID, externalID string) (domainwager.Transaction, error) {
	return r.FindByExternalTransactionID(ctx, providerID, externalID)
}

func (r *memoryWagers) FindDuePendingReference(_ context.Context, now time.Time) (domainwager.Transaction, error) {
	for _, transaction := range r.byKey {
		if transaction.Status() != domainwager.PendingReference {
			continue
		}
		nextAttempt, exists := r.retryAt[transaction.ID()]
		if !exists {
			nextAttempt = transaction.UpdatedAt()
		}
		if nextAttempt.After(now) || r.leaseUntil[transaction.ID()].After(now) {
			continue
		}
		return transaction, nil
	}
	return domainwager.Transaction{}, ports.ErrNotFound
}

func (r *memoryWagers) ClaimPendingReference(ctx context.Context, id string, now, leaseUntil time.Time) (int, error) {
	transaction, err := r.GetByID(ctx, id)
	if err != nil || transaction.Status() != domainwager.PendingReference {
		return 0, ports.ErrConflict
	}
	nextAttempt, exists := r.retryAt[id]
	if exists && nextAttempt.After(now) || r.leaseUntil[id].After(now) {
		return 0, ports.ErrConflict
	}
	r.attempts[id]++
	r.leaseUntil[id] = leaseUntil
	return r.attempts[id], nil
}

func (r *memoryWagers) SchedulePendingReference(_ context.Context, id string, nextAttempt time.Time) error {
	r.retryAt[id] = nextAttempt
	r.leaseUntil[id] = time.Time{}
	return nil
}

func (r *memoryWagers) FindProcessedReversals(_ context.Context, referenceID string) ([]domainwager.Transaction, error) {
	var transactions []domainwager.Transaction
	for _, transaction := range r.byKey {
		if transaction.Status() == domainwager.Processed && transaction.ReferenceTransactionID() == referenceID &&
			(transaction.Kind() == domainwager.Refund || transaction.Kind() == domainwager.Rollback) {
			transactions = append(transactions, transaction)
		}
	}
	return transactions, nil
}

func (r *memoryWagers) Create(_ context.Context, transaction domainwager.Transaction) error {
	key := transaction.ProviderID() + "/" + transaction.IdempotencyKey()
	externalID := transaction.ProviderID() + "/" + transaction.ExternalTransactionID()
	if _, exists := r.byKey[key]; exists {
		return ports.ErrConflict
	}
	if _, exists := r.byExternalID[externalID]; exists {
		return ports.ErrConflict
	}
	r.byKey[key] = transaction
	r.byExternalID[externalID] = transaction
	if transaction.Status() == domainwager.PendingReference {
		r.retryAt[transaction.ID()] = transaction.UpdatedAt()
	}
	return nil
}

func (r *memoryWagers) Save(_ context.Context, transaction domainwager.Transaction) error {
	key := transaction.ProviderID() + "/" + transaction.IdempotencyKey()
	if _, exists := r.byKey[key]; !exists {
		return ports.ErrNotFound
	}
	r.byKey[key] = transaction
	r.byExternalID[transaction.ProviderID()+"/"+transaction.ExternalTransactionID()] = transaction
	r.retryAt[transaction.ID()] = transaction.UpdatedAt()
	r.leaseUntil[transaction.ID()] = time.Time{}
	return nil
}

type memoryLedger struct{ entries []domainwallet.LedgerEntry }

func (r *memoryLedger) Append(_ context.Context, entry domainwallet.LedgerEntry) error {
	r.entries = append(r.entries, entry)
	return nil
}

type memoryOutbox struct{ events []events.Event }

func (r *memoryOutbox) Append(_ context.Context, event events.Event) error {
	r.events = append(r.events, event)
	return nil
}
