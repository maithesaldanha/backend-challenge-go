package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/postgres"
	applicationwager "github.com/junglegaming/backend-challenge-go/internal/application/wager"
	applicationwallet "github.com/junglegaming/backend-challenge-go/internal/application/wallet"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
)

func TestBetConcurrencyAcrossThreeProcesses(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if strings.TrimSpace(databaseURL) == "" {
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

	schema := "wager_concurrency_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := baseDB.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create isolated test schema: %v", err)
	}
	schemaDBURL, err := withSearchPath(databaseURL, schema)
	if err != nil {
		_, _ = baseDB.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		t.Fatal(err)
	}
	defer func() {
		_, _ = baseDB.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
	}()

	schemaDB, err := postgres.NewDB(postgres.Config{DSN: schemaDBURL})
	if err != nil {
		t.Fatal(err)
	}
	defer schemaDB.Close()
	migration, err := os.ReadFile(filepath.Join("..", "..", "..", "migrations", "000001_init.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schemaDB.ExecContext(ctx, string(migration)); err != nil {
		t.Fatalf("apply schema migration: %v", err)
	}

	transactor, err := postgres.NewTransactor(schemaDB)
	if err != nil {
		t.Fatal(err)
	}
	opening, err := applicationwallet.NewOpenWallet(transactor, uuid.NewString, time.Now().UTC)
	if err != nil {
		t.Fatal(err)
	}
	initialBalance, err := money.NewExternal("100.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := opening.Execute(ctx, applicationwallet.OpenWalletCommand{
		PlayerID:       uuid.NewString(),
		InitialBalance: initialBalance,
	})
	if err != nil {
		t.Fatalf("open test wallet: %v", err)
	}

	gatePath := filepath.Join(t.TempDir(), "start")
	playerID := opened.Wallet.PlayerID()
	walletID := opened.Wallet.ID()
	commands := make([]*exec.Cmd, 0, 3)
	outputs := make([]*strings.Builder, 0, 3)
	for _, request := range []struct {
		externalID string
		key        string
	}{
		{externalID: "bet-a", key: "bet-a-key"},
		{externalID: "bet-a", key: "bet-a-key"},
		{externalID: "bet-b", key: "bet-b-key"},
	} {
		output := &strings.Builder{}
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBetConcurrencyProcess$", "-test.v")
		command.Env = append(os.Environ(),
			"WAGER_CONCURRENCY_CHILD=1",
			"WAGER_CONCURRENCY_DATABASE_URL="+schemaDBURL,
			"WAGER_CONCURRENCY_GATE="+gatePath,
			"WAGER_CONCURRENCY_PLAYER="+playerID,
			"WAGER_CONCURRENCY_WALLET="+walletID,
			"WAGER_CONCURRENCY_EXTERNAL_ID="+request.externalID,
			"WAGER_CONCURRENCY_IDEMPOTENCY_KEY="+request.key,
		)
		command.Stdout = output
		command.Stderr = output
		if err := command.Start(); err != nil {
			for _, started := range commands {
				_ = started.Process.Kill()
			}
			t.Fatalf("start independent wager process: %v", err)
		}
		commands = append(commands, command)
		outputs = append(outputs, output)
	}
	if err := os.WriteFile(gatePath, []byte("start"), 0600); err != nil {
		for _, command := range commands {
			_ = command.Process.Kill()
		}
		t.Fatal(err)
	}
	idempotentReplay := false
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Errorf("wager process %d failed: %v\n%s", i, err, outputs[i].String())
		}
		if strings.Contains(outputs[i].String(), "replay=true") {
			idempotentReplay = true
		}
	}
	if !idempotentReplay {
		t.Fatal("duplicate process did not report an idempotent replay")
	}

	var balance int64
	if err := schemaDB.QueryRowContext(ctx, `SELECT balance_minor FROM wallets WHERE id = $1`, walletID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 2000 {
		t.Fatalf("final balance = %d minor units, want 2000", balance)
	}
	var processed, rejected, insufficient, debits int
	err = schemaDB.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'PROCESSED'),
			count(*) FILTER (WHERE status = 'REJECTED'),
			count(*) FILTER (WHERE failure_code = 'INSUFFICIENT_FUNDS'),
			(SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT')
		FROM wager_transactions
		WHERE wallet_id = $1 AND source = 'PROVIDER' AND kind = 'BET'`, walletID,
	).Scan(&processed, &rejected, &insufficient, &debits)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 1 || rejected != 1 || insufficient != 1 || debits != 1 {
		t.Fatalf("processed=%d rejected=%d insufficient=%d debits=%d, want 1, 1, 1, 1", processed, rejected, insufficient, debits)
	}
}

func TestBetConcurrencyProcess(t *testing.T) {
	if os.Getenv("WAGER_CONCURRENCY_CHILD") != "1" {
		t.Skip("child process mode only")
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(os.Getenv("WAGER_CONCURRENCY_GATE")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for concurrent start signal")
		}
		time.Sleep(5 * time.Millisecond)
	}

	db, err := postgres.NewDB(postgres.Config{DSN: os.Getenv("WAGER_CONCURRENCY_DATABASE_URL")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	transactor, err := postgres.NewTransactor(db)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := applicationwager.NewProcessBet(transactor, uuid.NewString, time.Now().UTC)
	if err != nil {
		t.Fatal(err)
	}
	amount, err := money.NewExternal("80.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	result, err := processor.Execute(context.Background(), applicationwager.BetCommand{
		ProviderID:            "concurrency-test-provider",
		ExternalTransactionID: os.Getenv("WAGER_CONCURRENCY_EXTERNAL_ID"),
		IdempotencyKey:        os.Getenv("WAGER_CONCURRENCY_IDEMPOTENCY_KEY"),
		PayloadHash:           strings.Repeat("a", 64),
		WalletID:              os.Getenv("WAGER_CONCURRENCY_WALLET"),
		PlayerID:              os.Getenv("WAGER_CONCURRENCY_PLAYER"),
		RoundID:               "concurrency-test-round",
		GameID:                "concurrency-test-game",
		Money:                 amount,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("status=%s replay=%t transaction=%s", result.Transaction.Status(), result.IdempotentReplay, result.Transaction.ID())
}

func withSearchPath(databaseURL, schema string) (string, error) {
	parsed, err := url.Parse(databaseURL)
	if err != nil || parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return "", fmt.Errorf("TEST_DATABASE_URL must be a postgres:// or postgresql:// URL")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
