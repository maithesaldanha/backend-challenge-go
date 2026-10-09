package events

import (
	"encoding/json"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wager"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

type Event struct {
	ID            string
	AggregateID   string
	Type          string
	CorrelationID string
	OccurredAt    time.Time
	Version       int
	Payload       json.RawMessage
}

type WagerTransactionProcessedData struct {
	TransactionID         string      `json:"transactionId"`
	ProviderID            string      `json:"providerId,omitempty"`
	ExternalTransactionID string      `json:"externalTransactionId,omitempty"`
	Kind                  wager.Kind  `json:"kind"`
	Money                 money.Money `json:"money"`
	ResultBalance         money.Money `json:"resultBalance"`
}

type WagerTransactionRejectedData struct {
	TransactionID         string     `json:"transactionId"`
	ProviderID            string     `json:"providerId"`
	ExternalTransactionID string     `json:"externalTransactionId"`
	Kind                  wager.Kind `json:"kind"`
	FailureCode           string     `json:"failureCode"`
}

type WagerTransactionPendingReferenceData struct {
	TransactionID                  string     `json:"transactionId"`
	ProviderID                     string     `json:"providerId"`
	ExternalTransactionID          string     `json:"externalTransactionId"`
	ReferenceExternalTransactionID string     `json:"referenceExternalTransactionId"`
	Kind                           wager.Kind `json:"kind"`
}

type WalletBalanceChangedData struct {
	WalletID      string           `json:"walletId"`
	TransactionID string           `json:"transactionId"`
	Direction     wallet.Direction `json:"direction"`
	Money         money.Money      `json:"money"`
	BalanceBefore money.Money      `json:"balanceBefore"`
	BalanceAfter  money.Money      `json:"balanceAfter"`
	WalletVersion int64            `json:"walletVersion"`
}

func NewWagerTransactionProcessed(id, correlationID string, transaction wager.Transaction, occurredAt time.Time) (Event, error) {
	resultBalance, ok := transaction.ResultBalance()
	if !ok {
		return Event{}, wager.ErrResultRequired
	}
	payload, err := json.Marshal(WagerTransactionProcessedData{
		TransactionID:         transaction.ID(),
		ProviderID:            transaction.ProviderID(),
		ExternalTransactionID: transaction.ExternalTransactionID(),
		Kind:                  transaction.Kind(),
		Money:                 transaction.Money(),
		ResultBalance:         resultBalance,
	})
	if err != nil {
		return Event{}, err
	}
	return Event{
		ID:            id,
		AggregateID:   transaction.ID(),
		Type:          "WagerTransactionProcessed",
		CorrelationID: correlationID,
		OccurredAt:    occurredAt,
		Version:       1,
		Payload:       payload,
	}, nil
}

func NewWagerTransactionRejected(id, correlationID string, transaction wager.Transaction, occurredAt time.Time) (Event, error) {
	payload, err := json.Marshal(WagerTransactionRejectedData{
		TransactionID:         transaction.ID(),
		ProviderID:            transaction.ProviderID(),
		ExternalTransactionID: transaction.ExternalTransactionID(),
		Kind:                  transaction.Kind(),
		FailureCode:           transaction.FailureCode(),
	})
	if err != nil {
		return Event{}, err
	}
	return Event{
		ID:            id,
		AggregateID:   transaction.ID(),
		Type:          "WagerTransactionRejected",
		CorrelationID: correlationID,
		OccurredAt:    occurredAt,
		Version:       1,
		Payload:       payload,
	}, nil
}

func NewWagerTransactionPendingReference(id, correlationID string, transaction wager.Transaction, occurredAt time.Time) (Event, error) {
	payload, err := json.Marshal(WagerTransactionPendingReferenceData{
		TransactionID:                  transaction.ID(),
		ProviderID:                     transaction.ProviderID(),
		ExternalTransactionID:          transaction.ExternalTransactionID(),
		ReferenceExternalTransactionID: transaction.ReferenceExternalTransactionID(),
		Kind:                           transaction.Kind(),
	})
	if err != nil {
		return Event{}, err
	}
	return Event{
		ID:            id,
		AggregateID:   transaction.ID(),
		Type:          "WagerTransactionPendingReference",
		CorrelationID: correlationID,
		OccurredAt:    occurredAt,
		Version:       1,
		Payload:       payload,
	}, nil
}

func NewWalletBalanceChanged(id, correlationID string, entry wallet.LedgerEntry, walletVersion int64, occurredAt time.Time) (Event, error) {
	payload, err := json.Marshal(WalletBalanceChangedData{
		WalletID:      entry.WalletID(),
		TransactionID: entry.TransactionID(),
		Direction:     entry.Direction(),
		Money:         entry.Money(),
		BalanceBefore: entry.BalanceBefore(),
		BalanceAfter:  entry.BalanceAfter(),
		WalletVersion: walletVersion,
	})
	if err != nil {
		return Event{}, err
	}
	return Event{
		ID:            id,
		AggregateID:   entry.WalletID(),
		Type:          "WalletBalanceChanged",
		CorrelationID: correlationID,
		OccurredAt:    occurredAt,
		Version:       1,
		Payload:       payload,
	}, nil
}
