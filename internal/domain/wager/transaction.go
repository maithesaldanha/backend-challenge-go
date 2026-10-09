package wager

import (
	"errors"
	"strings"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
)

var (
	ErrInvalidTransaction = errors.New("invalid wager transaction")
	ErrInvalidKind        = errors.New("invalid wager kind")
	ErrInvalidStatus      = errors.New("invalid transaction status")
	ErrInvalidTransition  = errors.New("invalid transaction state transition")
	ErrInvalidFailureCode = errors.New("invalid failure code")
	ErrTerminalState      = errors.New("transaction is terminal")
	ErrReferenceRequired  = errors.New("reference transaction is required")
	ErrResultRequired     = errors.New("result balance is required")
	ErrNegativeResult     = errors.New("result balance cannot be negative")
)

type Kind string

const (
	Opening  Kind = "OPENING"
	Bet      Kind = "BET"
	Win      Kind = "WIN"
	Loss     Kind = "LOSS"
	Refund   Kind = "REFUND"
	Rollback Kind = "ROLLBACK"
)

type Status string

const (
	Pending          Status = "PENDING"
	PendingReference Status = "PENDING_REFERENCE"
	Processed        Status = "PROCESSED"
	Rejected         Status = "REJECTED"
	Failed           Status = "FAILED"
)

type ExternalParams struct {
	ID                             string
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    string
	WalletID                       string
	PlayerID                       string
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Money                          money.Money
	ReferenceExternalTransactionID string
	CreatedAt                      time.Time
}

type Transaction struct {
	id                     string
	providerID             string
	externalTransactionID  string
	idempotencyKey         string
	payloadHash            string
	walletID               string
	playerID               string
	roundID                string
	gameID                 string
	kind                   Kind
	money                  money.Money
	referenceExternalID    string
	referenceTransactionID string
	status                 Status
	failureCode            string
	resultBalance          money.Money
	hasResultBalance       bool
	createdAt              time.Time
	updatedAt              time.Time
}

func NewExternal(params ExternalParams) (Transaction, error) {
	if !validExternalParams(params) {
		return Transaction{}, ErrInvalidTransaction
	}
	if params.Kind == Opening {
		return Transaction{}, ErrInvalidKind
	}
	if err := validateAmount(params.Kind, params.Money); err != nil {
		return Transaction{}, err
	}
	if (params.Kind == Refund || params.Kind == Rollback) && strings.TrimSpace(params.ReferenceExternalTransactionID) == "" {
		return Transaction{}, ErrReferenceRequired
	}
	if params.Kind != Win && params.Kind != Refund && params.Kind != Rollback && params.ReferenceExternalTransactionID != "" {
		return Transaction{}, ErrInvalidTransaction
	}
	if params.ReferenceExternalTransactionID != "" && strings.TrimSpace(params.ReferenceExternalTransactionID) == "" {
		return Transaction{}, ErrInvalidTransaction
	}
	return Transaction{
		id:                    params.ID,
		providerID:            params.ProviderID,
		externalTransactionID: params.ExternalTransactionID,
		idempotencyKey:        params.IdempotencyKey,
		payloadHash:           params.PayloadHash,
		walletID:              params.WalletID,
		playerID:              params.PlayerID,
		roundID:               params.RoundID,
		gameID:                params.GameID,
		kind:                  params.Kind,
		money:                 params.Money,
		referenceExternalID:   params.ReferenceExternalTransactionID,
		status:                Pending,
		createdAt:             params.CreatedAt,
		updatedAt:             params.CreatedAt,
	}, nil
}

func RehydrateExternal(params ExternalParams, status Status, referenceTransactionID, failureCode string, resultBalance money.Money, hasResultBalance bool, updatedAt time.Time) (Transaction, error) {
	transaction, err := NewExternal(params)
	if err != nil {
		return Transaction{}, err
	}
	if !validUTC(updatedAt) || updatedAt.Before(params.CreatedAt) {
		return Transaction{}, ErrInvalidTransaction
	}
	if referenceTransactionID != "" && (params.Kind != Win && params.Kind != Refund && params.Kind != Rollback) {
		return Transaction{}, ErrInvalidTransaction
	}
	switch status {
	case Pending:
		if failureCode != "" || hasResultBalance {
			return Transaction{}, ErrInvalidStatus
		}
	case PendingReference:
		if (params.Kind != Refund && params.Kind != Rollback) || referenceTransactionID != "" || failureCode != "" || hasResultBalance {
			return Transaction{}, ErrInvalidStatus
		}
	case Processed:
		if !hasResultBalance {
			return Transaction{}, ErrResultRequired
		}
		if failureCode != "" {
			return Transaction{}, ErrInvalidStatus
		}
		minor, balanceErr := resultBalance.AmountMinor()
		if balanceErr != nil {
			return Transaction{}, balanceErr
		}
		if minor < 0 {
			return Transaction{}, ErrNegativeResult
		}
		if _, balanceErr = transaction.money.Compare(resultBalance); balanceErr != nil {
			return Transaction{}, balanceErr
		}
		if (params.Kind == Refund || params.Kind == Rollback) && referenceTransactionID == "" {
			return Transaction{}, ErrReferenceRequired
		}
	case Rejected, Failed:
		if !validFailureCode(failureCode) || hasResultBalance {
			return Transaction{}, ErrInvalidStatus
		}
	default:
		return Transaction{}, ErrInvalidStatus
	}
	transaction.status = status
	transaction.referenceTransactionID = referenceTransactionID
	transaction.failureCode = failureCode
	transaction.resultBalance = resultBalance
	transaction.hasResultBalance = hasResultBalance
	transaction.updatedAt = updatedAt
	return transaction, nil
}

func NewOpening(id, walletID, playerID string, amount money.Money, now time.Time) (Transaction, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(walletID) == "" || strings.TrimSpace(playerID) == "" || !validUTC(now) {
		return Transaction{}, ErrInvalidTransaction
	}
	minor, err := amount.AmountMinor()
	if err != nil {
		return Transaction{}, err
	}
	if minor <= 0 {
		return Transaction{}, ErrInvalidTransaction
	}
	return Transaction{
		id:               id,
		walletID:         walletID,
		playerID:         playerID,
		kind:             Opening,
		money:            amount,
		status:           Processed,
		resultBalance:    amount,
		hasResultBalance: true,
		createdAt:        now,
		updatedAt:        now,
	}, nil
}

func (t *Transaction) MarkPendingReference(now time.Time) error {
	if err := t.canTransition(now); err != nil {
		return err
	}
	if t.status != Pending || (t.kind != Refund && t.kind != Rollback) || t.referenceTransactionID != "" {
		return ErrInvalidTransition
	}
	t.status = PendingReference
	t.updatedAt = now
	return nil
}

func (t *Transaction) ResolveReference(referenceTransactionID string, now time.Time) error {
	if err := t.canTransition(now); err != nil {
		return err
	}
	if t.status != PendingReference || strings.TrimSpace(referenceTransactionID) == "" {
		return ErrInvalidTransition
	}
	t.referenceTransactionID = referenceTransactionID
	t.status = Pending
	t.updatedAt = now
	return nil
}

func (t *Transaction) MarkProcessed(balance money.Money, now time.Time) error {
	if err := t.canTransition(now); err != nil {
		return err
	}
	if t.status != Pending && t.status != PendingReference {
		return ErrInvalidTransition
	}
	minor, err := balance.AmountMinor()
	if err != nil {
		return err
	}
	if minor < 0 {
		return ErrNegativeResult
	}
	if _, err := t.money.Compare(balance); err != nil {
		return err
	}
	if (t.kind == Refund || t.kind == Rollback) && t.referenceTransactionID == "" {
		return ErrReferenceRequired
	}
	t.status = Processed
	t.resultBalance = balance
	t.hasResultBalance = true
	t.updatedAt = now
	return nil
}

func (t *Transaction) Reject(failureCode string, now time.Time) error {
	return t.finish(Rejected, failureCode, now)
}

func (t *Transaction) Fail(failureCode string, now time.Time) error {
	return t.finish(Failed, failureCode, now)
}

func (t *Transaction) finish(status Status, failureCode string, now time.Time) error {
	if err := t.canTransition(now); err != nil {
		return err
	}
	if t.status != Pending && t.status != PendingReference {
		return ErrInvalidTransition
	}
	if !validFailureCode(failureCode) {
		return ErrInvalidFailureCode
	}
	t.status = status
	t.failureCode = failureCode
	t.updatedAt = now
	return nil
}

func (t *Transaction) canTransition(now time.Time) error {
	if t == nil || strings.TrimSpace(t.id) == "" || !validUTC(now) || now.Before(t.updatedAt) {
		return ErrInvalidTransaction
	}
	if isTerminal(t.status) {
		return ErrTerminalState
	}
	return nil
}

func (t Transaction) ID() string                             { return t.id }
func (t Transaction) ProviderID() string                     { return t.providerID }
func (t Transaction) ExternalTransactionID() string          { return t.externalTransactionID }
func (t Transaction) IdempotencyKey() string                 { return t.idempotencyKey }
func (t Transaction) PayloadHash() string                    { return t.payloadHash }
func (t Transaction) WalletID() string                       { return t.walletID }
func (t Transaction) PlayerID() string                       { return t.playerID }
func (t Transaction) RoundID() string                        { return t.roundID }
func (t Transaction) GameID() string                         { return t.gameID }
func (t Transaction) Kind() Kind                             { return t.kind }
func (t Transaction) Money() money.Money                     { return t.money }
func (t Transaction) ReferenceExternalTransactionID() string { return t.referenceExternalID }
func (t Transaction) ReferenceTransactionID() string         { return t.referenceTransactionID }
func (t Transaction) Status() Status                         { return t.status }
func (t Transaction) FailureCode() string                    { return t.failureCode }
func (t Transaction) CreatedAt() time.Time                   { return t.createdAt }
func (t Transaction) UpdatedAt() time.Time                   { return t.updatedAt }

func (t Transaction) ResultBalance() (money.Money, bool) {
	return t.resultBalance, t.hasResultBalance
}

func validateAmount(kind Kind, amount money.Money) error {
	minor, err := amount.AmountMinor()
	if err != nil {
		return err
	}
	switch kind {
	case Bet, Win, Refund, Rollback:
		if minor <= 0 {
			return ErrInvalidTransaction
		}
	case Loss:
		if minor != 0 {
			return ErrInvalidTransaction
		}
	default:
		return ErrInvalidKind
	}
	return nil
}

func validExternalParams(params ExternalParams) bool {
	return strings.TrimSpace(params.ID) != "" &&
		strings.TrimSpace(params.ProviderID) != "" &&
		strings.TrimSpace(params.ExternalTransactionID) != "" &&
		strings.TrimSpace(params.IdempotencyKey) != "" &&
		strings.TrimSpace(params.PayloadHash) != "" &&
		strings.TrimSpace(params.WalletID) != "" &&
		strings.TrimSpace(params.PlayerID) != "" &&
		strings.TrimSpace(params.RoundID) != "" &&
		strings.TrimSpace(params.GameID) != "" &&
		validUTC(params.CreatedAt)
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}

func validFailureCode(code string) bool {
	if len(code) == 0 || code[0] < 'A' || code[0] > 'Z' {
		return false
	}
	for i := 1; i < len(code); i++ {
		if (code[i] < 'A' || code[i] > 'Z') && (code[i] < '0' || code[i] > '9') && code[i] != '_' {
			return false
		}
	}
	return true
}

func isTerminal(status Status) bool {
	return status == Processed || status == Rejected || status == Failed
}
