package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/postgres"
	applicationsqs "github.com/junglegaming/backend-challenge-go/internal/application/sqsconsumer"
	applicationwager "github.com/junglegaming/backend-challenge-go/internal/application/wager"
	applicationwallet "github.com/junglegaming/backend-challenge-go/internal/application/wallet"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	domainwager "github.com/junglegaming/backend-challenge-go/internal/domain/wager"
)

func TestSQSConsumerInboxAndWagerAreIdempotentAndAtomic(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	baseDB, err := postgres.NewDB(postgres.Config{DSN: databaseURL})
	if err != nil {
		t.Fatal(err)
	}
	defer baseDB.Close()
	if err := baseDB.PingContext(ctx); err != nil {
		t.Fatalf("connect to test PostgreSQL: %v", err)
	}

	schema := "sqs_consumer_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := baseDB.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create isolated test schema: %v", err)
	}
	defer func() { _, _ = baseDB.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`) }()
	schemaURL, err := withSearchPath(databaseURL, schema)
	if err != nil {
		t.Fatal(err)
	}
	db, err := postgres.NewDB(postgres.Config{DSN: schemaURL})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migration, err := os.ReadFile(filepath.Join("..", "..", "..", "migrations", "000001_init.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(migration)); err != nil {
		t.Fatalf("apply schema migration: %v", err)
	}

	transactor, err := postgres.NewTransactor(db)
	if err != nil {
		t.Fatal(err)
	}
	newID := uuid.NewString
	now := func() time.Time { return time.Now().UTC() }
	opening, err := applicationwallet.NewOpenWallet(transactor, newID, now)
	if err != nil {
		t.Fatal(err)
	}
	initialBalance, err := money.NewExternal("100.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	playerID := uuid.NewString()
	opened, err := opening.Execute(ctx, applicationwallet.OpenWalletCommand{PlayerID: playerID, InitialBalance: initialBalance})
	if err != nil {
		t.Fatalf("open test wallet: %v", err)
	}
	processBet, err := applicationwager.NewProcessBet(transactor, newID, now)
	if err != nil {
		t.Fatal(err)
	}
	processLoss, err := applicationwager.NewProcessLoss(transactor, newID, now)
	if err != nil {
		t.Fatal(err)
	}
	processWin, err := applicationwager.NewProcessWin(transactor, newID, now)
	if err != nil {
		t.Fatal(err)
	}
	processReversal, err := applicationwager.NewProcessReversal(transactor, newID, now)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := applicationsqs.New(transactor, processBet, processLoss, processWin, processReversal, now)
	if err != nil {
		t.Fatal(err)
	}
	amount, err := money.NewExternal("25.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	message := wagerRequestMessage{
		MessageID: "message-concurrent-1", Type: "WagerTransactionRequested", OccurredAt: now(),
		Data: wagerRequestData{
			ProviderID: "integration-provider", ExternalTransactionID: "external-concurrent-1",
			IdempotencyKey: "integration-provider:concurrent-1", PlayerID: playerID,
			WalletID: opened.Wallet.ID(), RoundID: "round-1", GameID: "game-1", Kind: string(domainwager.Bet), Money: amount,
		},
	}
	body, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}

	const deliveries = 8
	errorsByDelivery := make([]error, deliveries)
	var workers sync.WaitGroup
	for i := 0; i < deliveries; i++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			errorsByDelivery[index] = consumer.Handle(ctx, body)
		}(i)
	}
	workers.Wait()
	for i, err := range errorsByDelivery {
		if err != nil {
			t.Fatalf("delivery %d failed: %v", i, err)
		}
	}

	assertConsumerState(t, ctx, db, opened.Wallet.ID(), 7500, 1, 1)
	var inboxRows, completedRows int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*), count(*) FILTER (WHERE completed_at IS NOT NULL)
		FROM consumer_inbox
		WHERE consumer_name = 'wager-transactions-v1' AND message_id = $1`, message.MessageID,
	).Scan(&inboxRows, &completedRows); err != nil {
		t.Fatal(err)
	}
	if inboxRows != 1 || completedRows != 1 {
		t.Fatalf("inbox rows=%d completed=%d, want 1 and 1", inboxRows, completedRows)
	}

	changed := message
	changed.Data.Money, _ = money.NewExternal("26.00", "BRL")
	changedBody, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Handle(ctx, changedBody); err == nil {
		t.Fatal("reused message ID with a different body succeeded")
	}
	assertConsumerState(t, ctx, db, opened.Wallet.ID(), 7500, 1, 1)

	failed := message
	failed.MessageID = "message-rollback-1"
	failed.Data.ExternalTransactionID = "external-rollback-1"
	failed.Data.IdempotencyKey = "integration-provider:rollback-1"
	failed.Data.WalletID = uuid.NewString()
	failedBody, err := json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Handle(ctx, failedBody); err == nil {
		t.Fatal("processing wager for a missing wallet succeeded")
	}
	var failedInboxRows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM consumer_inbox WHERE message_id = $1`, failed.MessageID).Scan(&failedInboxRows); err != nil {
		t.Fatal(err)
	}
	if failedInboxRows != 0 {
		t.Fatalf("failed message left %d inbox rows, want 0 after transaction rollback", failedInboxRows)
	}

	crossAmount, err := money.NewExternal("10.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	crossMessage := wagerRequestMessage{
		MessageID: "message-http-sqs-race-1", Type: "WagerTransactionRequested", OccurredAt: now(),
		Data: wagerRequestData{
			ProviderID: "integration-provider", ExternalTransactionID: "external-http-sqs-race-1",
			IdempotencyKey: "integration-provider:http-sqs-race-1", PlayerID: playerID,
			WalletID: opened.Wallet.ID(), RoundID: "round-2", GameID: "game-1", Kind: string(domainwager.Bet), Money: crossAmount,
		},
	}
	crossBody, err := json.Marshal(crossMessage)
	if err != nil {
		t.Fatal(err)
	}
	command := applicationwager.WagerCommand{
		ProviderID: crossMessage.Data.ProviderID, ExternalTransactionID: crossMessage.Data.ExternalTransactionID,
		IdempotencyKey: crossMessage.Data.IdempotencyKey, WalletID: crossMessage.Data.WalletID,
		PlayerID: crossMessage.Data.PlayerID, RoundID: crossMessage.Data.RoundID, GameID: crossMessage.Data.GameID, Money: crossAmount,
	}
	command.PayloadHash, err = applicationwager.PayloadHash(command, string(domainwager.Bet))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	httpResult := make(chan error, 1)
	sqsResult := make(chan error, 1)
	go func() {
		<-start
		_, err := processBet.Execute(ctx, command)
		httpResult <- err
	}()
	go func() {
		<-start
		err := consumer.Handle(ctx, crossBody)
		if err != nil {
			err = consumer.Handle(ctx, crossBody)
		}
		sqsResult <- err
	}()
	close(start)
	if err := <-httpResult; err != nil {
		t.Fatalf("HTTP-side processing failed during SQS race: %v", err)
	}
	if err := <-sqsResult; err != nil {
		t.Fatalf("SQS processing failed during HTTP race and redelivery: %v", err)
	}
	assertConsumerState(t, ctx, db, opened.Wallet.ID(), 6500, 2, 2)
}

type wagerRequestMessage struct {
	MessageID  string           `json:"messageId"`
	Type       string           `json:"type"`
	OccurredAt time.Time        `json:"occurredAt"`
	Data       wagerRequestData `json:"data"`
}

type wagerRequestData struct {
	ProviderID            string      `json:"providerId"`
	ExternalTransactionID string      `json:"externalTransactionId"`
	IdempotencyKey        string      `json:"idempotencyKey"`
	PlayerID              string      `json:"playerId"`
	WalletID              string      `json:"walletId"`
	RoundID               string      `json:"roundId"`
	GameID                string      `json:"gameId"`
	Kind                  string      `json:"kind"`
	Money                 money.Money `json:"money"`
}

func assertConsumerState(t *testing.T, ctx context.Context, db *sql.DB, walletID string, expectedBalance int64, expectedBets, expectedDebits int) {
	t.Helper()
	var balance int64
	if err := db.QueryRowContext(ctx, `SELECT balance_minor FROM wallets WHERE id = $1`, walletID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != expectedBalance {
		t.Fatalf("wallet balance=%d, want %d", balance, expectedBalance)
	}
	var bets, debits int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE kind = 'BET'),
			(SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT')
		FROM wager_transactions
		WHERE wallet_id = $1 AND source = 'PROVIDER'`, walletID,
	).Scan(&bets, &debits); err != nil {
		t.Fatal(err)
	}
	if bets != expectedBets || debits != expectedDebits {
		t.Fatalf("bets=%d debits=%d, want %d and %d", bets, debits, expectedBets, expectedDebits)
	}
}
