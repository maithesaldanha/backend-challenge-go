package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/application/events"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wager"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

type unitOfWork struct {
	wallets walletRepository
	wagers  wagerRepository
	ledger  ledgerRepository
	outbox  outboxRepository
}

func newUnitOfWork(tx *sql.Tx) *unitOfWork {
	return &unitOfWork{
		wallets: walletRepository{tx: tx},
		wagers:  wagerRepository{tx: tx},
		ledger:  ledgerRepository{tx: tx},
		outbox:  outboxRepository{tx: tx},
	}
}

func (u *unitOfWork) Wallets() ports.WalletRepository { return u.wallets }
func (u *unitOfWork) Wagers() ports.WagerRepository   { return u.wagers }
func (u *unitOfWork) Ledger() ports.LedgerRepository  { return u.ledger }
func (u *unitOfWork) Outbox() ports.OutboxRepository  { return u.outbox }

type walletRepository struct{ tx *sql.Tx }

func (r walletRepository) Get(ctx context.Context, id string) (wallet.Wallet, error) {
	return r.get(ctx, id, false)
}

func (r walletRepository) GetForUpdate(ctx context.Context, id string) (wallet.Wallet, error) {
	return r.get(ctx, id, true)
}

func (r walletRepository) get(ctx context.Context, id string, lock bool) (wallet.Wallet, error) {
	var playerID, currency string
	var balance, version int64
	var createdAt, updatedAt time.Time
	query := `
		SELECT player_id::text, currency, balance_minor, version, created_at, updated_at
		FROM wallets WHERE id = $1`
	if lock {
		query += ` FOR UPDATE`
	}
	err := r.tx.QueryRowContext(ctx, query, id).Scan(&playerID, &currency, &balance, &version, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return wallet.Wallet{}, ports.ErrNotFound
	}
	if err != nil {
		return wallet.Wallet{}, mapError(err)
	}
	amount, err := money.FromMinorUnits(balance, strings.TrimSpace(currency))
	if err != nil {
		return wallet.Wallet{}, err
	}
	return wallet.Rehydrate(id, playerID, amount, version, createdAt.UTC(), updatedAt.UTC())
}

func (r walletRepository) Create(ctx context.Context, account wallet.Wallet) error {
	minor, err := account.Balance().AmountMinor()
	if err != nil {
		return err
	}
	currency, err := account.Balance().Currency()
	if err != nil {
		return err
	}
	_, err = r.tx.ExecContext(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		account.ID(), account.PlayerID(), currency, minor, account.Version(), account.CreatedAt(), account.UpdatedAt())
	return mapError(err)
}

func (r walletRepository) Save(ctx context.Context, account wallet.Wallet, expectedVersion int64) error {
	minor, err := account.Balance().AmountMinor()
	if err != nil {
		return err
	}
	currency, err := account.Balance().Currency()
	if err != nil {
		return err
	}
	result, err := r.tx.ExecContext(ctx, `
		UPDATE wallets
		SET balance_minor = $1, version = $2, updated_at = $3
		WHERE id = $4 AND currency = $5 AND version = $6`,
		minor, account.Version(), account.UpdatedAt(), account.ID(), currency, expectedVersion)
	if err != nil {
		return mapError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ports.ErrOptimisticLock
	}
	return nil
}

type wagerRepository struct{ tx *sql.Tx }

const wagerColumns = `id::text, source, provider_id, external_transaction_id, idempotency_key, payload_hash,
	wallet_id::text, player_id::text, round_id, game_id, kind, amount_minor, currency,
	reference_external_transaction_id, reference_transaction_id::text, status, failure_code,
	result_balance_minor, created_at, updated_at`

func (r wagerRepository) GetByID(ctx context.Context, id string) (wager.Transaction, error) {
	return r.findOne(ctx, `SELECT `+wagerColumns+` FROM wager_transactions WHERE id = $1`, id)
}

func (r wagerRepository) FindByIdempotencyKey(ctx context.Context, providerID, key string) (wager.Transaction, error) {
	return r.findOne(ctx, `SELECT `+wagerColumns+` FROM wager_transactions WHERE source = 'PROVIDER' AND provider_id = $1 AND idempotency_key = $2`, providerID, key)
}

func (r wagerRepository) FindByExternalTransactionID(ctx context.Context, providerID, externalID string) (wager.Transaction, error) {
	return r.findOne(ctx, `SELECT `+wagerColumns+` FROM wager_transactions WHERE source = 'PROVIDER' AND provider_id = $1 AND external_transaction_id = $2`, providerID, externalID)
}

func (r wagerRepository) FindReference(ctx context.Context, providerID, externalID string) (wager.Transaction, error) {
	return r.FindByExternalTransactionID(ctx, providerID, externalID)
}

func (r wagerRepository) FindDuePendingReference(ctx context.Context, now time.Time) (wager.Transaction, error) {
	return r.findOne(ctx, `SELECT `+wagerColumns+`
		FROM wager_transactions
		WHERE kind IN ('WIN', 'REFUND', 'ROLLBACK') AND status = 'PENDING_REFERENCE'
			AND next_attempt_at <= $1 AND (lease_until IS NULL OR lease_until <= $1)
		ORDER BY next_attempt_at, created_at
		LIMIT 1`, now)
}

func (r wagerRepository) ClaimPendingReference(ctx context.Context, id string, now, leaseUntil time.Time) (int, error) {
	var attempts int
	err := r.tx.QueryRowContext(ctx, `
		UPDATE wager_transactions
		SET attempt_count = attempt_count + 1, lease_until = $1
		WHERE id = $2 AND kind IN ('WIN', 'REFUND', 'ROLLBACK') AND status = 'PENDING_REFERENCE'
			AND next_attempt_at <= $3 AND (lease_until IS NULL OR lease_until <= $3)
		RETURNING attempt_count`, leaseUntil, id, now).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ports.ErrConflict
	}
	return attempts, mapError(err)
}

func (r wagerRepository) SchedulePendingReference(ctx context.Context, id string, nextAttemptAt time.Time) error {
	result, err := r.tx.ExecContext(ctx, `
		UPDATE wager_transactions
		SET next_attempt_at = $1, lease_until = NULL
		WHERE id = $2 AND kind IN ('WIN', 'REFUND', 'ROLLBACK') AND status = 'PENDING_REFERENCE'`, nextAttemptAt, id)
	if err != nil {
		return mapError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ports.ErrConflict
	}
	return nil
}

func (r wagerRepository) FindProcessedReversals(ctx context.Context, referenceID string) ([]wager.Transaction, error) {
	rows, err := r.tx.QueryContext(ctx, `SELECT `+wagerColumns+`
		FROM wager_transactions
		WHERE reference_transaction_id = $1 AND status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK')`, referenceID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	transactions := make([]wager.Transaction, 0)
	for rows.Next() {
		transaction, err := scanWager(rows)
		if err != nil {
			return nil, err
		}
		transactions = append(transactions, transaction)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return transactions, nil
}

func (r wagerRepository) Create(ctx context.Context, transaction wager.Transaction) error {
	return r.write(ctx, transaction, true)
}

func (r wagerRepository) Save(ctx context.Context, transaction wager.Transaction) error {
	return r.write(ctx, transaction, false)
}

func (r wagerRepository) findOne(ctx context.Context, query string, args ...any) (wager.Transaction, error) {
	transaction, err := scanWager(r.tx.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return wager.Transaction{}, ports.ErrNotFound
	}
	if err != nil {
		return wager.Transaction{}, mapError(err)
	}
	return transaction, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanWager(row rowScanner) (wager.Transaction, error) {
	var id, source, kind, status string
	var providerID, externalID, idempotencyKey, payloadHash sql.NullString
	var walletID, playerID string
	var roundID, gameID sql.NullString
	var currency string
	var referenceExternalID, referenceID, failureCode sql.NullString
	var amount int64
	var resultBalance sql.NullInt64
	var createdAt, updatedAt time.Time
	err := row.Scan(
		&id, &source, &providerID, &externalID, &idempotencyKey, &payloadHash,
		&walletID, &playerID, &roundID, &gameID, &kind, &amount, &currency,
		&referenceExternalID, &referenceID, &status, &failureCode, &resultBalance, &createdAt, &updatedAt,
	)
	if err != nil {
		return wager.Transaction{}, err
	}
	amountValue, err := money.FromMinorUnits(amount, strings.TrimSpace(currency))
	if err != nil {
		return wager.Transaction{}, err
	}
	if source == "INTERNAL" {
		return wager.RehydrateOpening(id, walletID, playerID, amountValue, createdAt.UTC(), updatedAt.UTC())
	}
	params := wager.ExternalParams{
		ID:                             id,
		ProviderID:                     providerID.String,
		ExternalTransactionID:          externalID.String,
		IdempotencyKey:                 idempotencyKey.String,
		PayloadHash:                    payloadHash.String,
		WalletID:                       walletID,
		PlayerID:                       playerID,
		RoundID:                        roundID.String,
		GameID:                         gameID.String,
		Kind:                           wager.Kind(kind),
		Money:                          amountValue,
		ReferenceExternalTransactionID: referenceExternalID.String,
		CreatedAt:                      createdAt.UTC(),
	}
	var resultValue money.Money
	if resultBalance.Valid {
		resultValue, err = money.FromMinorUnits(resultBalance.Int64, strings.TrimSpace(currency))
		if err != nil {
			return wager.Transaction{}, err
		}
	}
	return wager.RehydrateExternal(params, wager.Status(status), referenceID.String, failureCode.String, resultValue, resultBalance.Valid, updatedAt.UTC())
}

func (r wagerRepository) write(ctx context.Context, transaction wager.Transaction, create bool) error {
	amount, err := transaction.Money().AmountMinor()
	if err != nil {
		return err
	}
	currency, err := transaction.Money().Currency()
	if err != nil {
		return err
	}
	var source, providerID, externalID, key, hash, roundID, gameID any
	if transaction.Kind() == wager.Opening {
		source = "INTERNAL"
	} else {
		source = "PROVIDER"
		providerID = transaction.ProviderID()
		externalID = transaction.ExternalTransactionID()
		key = transaction.IdempotencyKey()
		hash = transaction.PayloadHash()
		roundID = transaction.RoundID()
		gameID = transaction.GameID()
	}
	var referenceExternalID, referenceID, referenceKind, failureCode, resultBalance any
	if value := transaction.ReferenceExternalTransactionID(); value != "" {
		referenceExternalID = value
	}
	if value := transaction.ReferenceTransactionID(); value != "" {
		referenceID = value
		err := r.tx.QueryRowContext(ctx, `SELECT kind FROM wager_transactions WHERE id = $1`, value).Scan(&referenceKind)
		if errors.Is(err, sql.ErrNoRows) {
			return ports.ErrNotFound
		}
		if err != nil {
			return mapError(err)
		}
	}
	if value := transaction.FailureCode(); value != "" {
		failureCode = value
	}
	if balance, ok := transaction.ResultBalance(); ok {
		resultBalance, err = balance.AmountMinor()
		if err != nil {
			return err
		}
	}

	if create {
		_, err = r.tx.ExecContext(ctx, `
			INSERT INTO wager_transactions (
				id, source, provider_id, external_transaction_id, idempotency_key, payload_hash,
				wallet_id, player_id, round_id, game_id, kind, amount_minor, currency,
				reference_external_transaction_id, reference_transaction_id, reference_kind,
				status, failure_code, result_balance_minor, next_attempt_at, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)`,
			transaction.ID(), source, providerID, externalID, key, hash,
			transaction.WalletID(), transaction.PlayerID(), roundID, gameID, transaction.Kind(), amount, currency,
			referenceExternalID, referenceID, referenceKind, transaction.Status(), failureCode, resultBalance,
			transaction.UpdatedAt(), transaction.CreatedAt(), transaction.UpdatedAt())
		return mapError(err)
	}
	result, err := r.tx.ExecContext(ctx, `
		UPDATE wager_transactions
		SET reference_transaction_id = $1, reference_kind = $2, status = $3,
			failure_code = $4, result_balance_minor = $5, next_attempt_at = $6,
			lease_until = NULL, updated_at = $6
		WHERE id = $7 AND status IN ('PENDING', 'PENDING_REFERENCE')`,
		referenceID, referenceKind, transaction.Status(), failureCode, resultBalance,
		transaction.UpdatedAt(), transaction.ID())
	if err != nil {
		return mapError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ports.ErrConflict
	}
	return nil
}

type ledgerRepository struct{ tx *sql.Tx }

func (r ledgerRepository) ListByWallet(ctx context.Context, walletID string, cursor *ports.LedgerCursor, limit int) ([]wallet.LedgerEntry, error) {
	query := `SELECT id::text, wallet_id::text, transaction_id::text, direction, amount_minor, currency,
		balance_before_minor, balance_after_minor, created_at
		FROM wallet_ledger_entries WHERE wallet_id = $1`
	args := []any{walletID}
	if cursor != nil {
		query += ` AND (created_at, id) < ($2, $3)`
		args = append(args, cursor.CreatedAt, cursor.ID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, limit)
	rows, err := r.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	entries := make([]wallet.LedgerEntry, 0, limit)
	for rows.Next() {
		var id, rowWalletID, transactionID, direction, currency string
		var amountMinor, beforeMinor, afterMinor int64
		var createdAt time.Time
		if err := rows.Scan(&id, &rowWalletID, &transactionID, &direction, &amountMinor, &currency, &beforeMinor, &afterMinor, &createdAt); err != nil {
			return nil, mapError(err)
		}
		amount, err := money.FromMinorUnits(amountMinor, strings.TrimSpace(currency))
		if err != nil {
			return nil, err
		}
		before, err := money.FromMinorUnits(beforeMinor, strings.TrimSpace(currency))
		if err != nil {
			return nil, err
		}
		after, err := money.FromMinorUnits(afterMinor, strings.TrimSpace(currency))
		if err != nil {
			return nil, err
		}
		entry, err := wallet.RehydrateLedgerEntry(id, rowWalletID, transactionID, wallet.Direction(direction), amount, before, after, createdAt.UTC())
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return entries, nil
}

func (r ledgerRepository) Append(ctx context.Context, entry wallet.LedgerEntry) error {
	amount, err := entry.Money().AmountMinor()
	if err != nil {
		return err
	}
	currency, err := entry.Money().Currency()
	if err != nil {
		return err
	}
	before, err := entry.BalanceBefore().AmountMinor()
	if err != nil {
		return err
	}
	after, err := entry.BalanceAfter().AmountMinor()
	if err != nil {
		return err
	}
	_, err = r.tx.ExecContext(ctx, `
		INSERT INTO wallet_ledger_entries (
			id, wallet_id, transaction_id, direction, amount_minor, currency,
			balance_before_minor, balance_after_minor, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		entry.ID(), entry.WalletID(), entry.TransactionID(), entry.Direction(), amount, currency, before, after, entry.CreatedAt())
	return mapError(err)
}

type outboxRepository struct{ tx *sql.Tx }

func (r outboxRepository) Append(ctx context.Context, event events.Event) error {
	_, err := r.tx.ExecContext(ctx, `
		INSERT INTO outbox_events (
			event_id, aggregate_id, event_type, correlation_id, occurred_at,
			event_version, payload, next_attempt_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $5, $5)`,
		event.ID, event.AggregateID, event.Type, event.CorrelationID, event.OccurredAt,
		event.Version, string(event.Payload))
	return mapError(err)
}
