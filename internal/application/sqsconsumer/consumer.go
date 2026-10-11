package sqsconsumer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	"github.com/junglegaming/backend-challenge-go/internal/application/wager"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	domainwager "github.com/junglegaming/backend-challenge-go/internal/domain/wager"
)

const consumerName = "wager-transactions-v1"

var ErrInvalidMessage = errors.New("invalid wager message")

type Consumer struct {
	transactor  ports.Transactor
	processBet  *wager.ProcessBet
	processLoss *wager.ProcessLoss
	processWin  *wager.ProcessWin
	processRev  *wager.ProcessReversal
	now         func() time.Time
}

func New(transactor ports.Transactor, processBet *wager.ProcessBet, processLoss *wager.ProcessLoss, processWin *wager.ProcessWin, processRev *wager.ProcessReversal, now func() time.Time) (*Consumer, error) {
	if transactor == nil || processBet == nil || processLoss == nil || processWin == nil || processRev == nil || now == nil {
		return nil, errors.New("invalid SQS consumer dependencies")
	}
	return &Consumer{transactor: transactor, processBet: processBet, processLoss: processLoss, processWin: processWin, processRev: processRev, now: now}, nil
}

func (c *Consumer) Handle(ctx context.Context, body []byte) error {
	processingContext, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	message, command, kind, err := decodeMessage(body)
	if err != nil {
		return err
	}
	command.PayloadHash, err = wager.PayloadHash(command, string(kind))
	if err != nil {
		return fmt.Errorf("hash wager payload: %w", err)
	}
	bodyHash := sha256.Sum256(body)
	messageHash := hex.EncodeToString(bodyHash[:])
	receivedAt := c.now().UTC()

	return c.transactor.WithinTransaction(processingContext, func(txctx context.Context, unit ports.UnitOfWork) error {
		completed, err := unit.Inbox().Register(txctx, consumerName, message.MessageID, messageHash, receivedAt)
		if err != nil {
			return err
		}
		if completed {
			return nil
		}
		if err := c.process(txctx, kind, command); err != nil {
			return err
		}
		return unit.Inbox().Complete(txctx, consumerName, message.MessageID, c.now().UTC())
	})
}

func (c *Consumer) process(ctx context.Context, kind domainwager.Kind, command wager.WagerCommand) error {
	switch kind {
	case domainwager.Bet:
		_, err := c.processBet.Execute(ctx, command)
		return err
	case domainwager.Loss:
		_, err := c.processLoss.Execute(ctx, command)
		return err
	case domainwager.Win:
		_, err := c.processWin.Execute(ctx, command)
		return err
	case domainwager.Refund, domainwager.Rollback:
		_, err := c.processRev.Execute(ctx, kind, command)
		return err
	default:
		return ErrInvalidMessage
	}
}

type messageEnvelope struct {
	MessageID  string    `json:"messageId"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurredAt"`
	Data       wagerData `json:"data"`
}

type wagerData struct {
	ProviderID                     string          `json:"providerId"`
	ExternalTransactionID          string          `json:"externalTransactionId"`
	IdempotencyKey                 string          `json:"idempotencyKey"`
	PlayerID                       string          `json:"playerId"`
	WalletID                       string          `json:"walletId"`
	RoundID                        string          `json:"roundId"`
	GameID                         string          `json:"gameId"`
	Kind                           string          `json:"kind"`
	ReferenceExternalTransactionID string          `json:"referenceExternalTransactionId"`
	Money                          json.RawMessage `json:"money"`
}

func decodeMessage(body []byte) (messageEnvelope, wager.WagerCommand, domainwager.Kind, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var message messageEnvelope
	if err := decoder.Decode(&message); err != nil {
		return messageEnvelope{}, wager.WagerCommand{}, "", errors.Join(ErrInvalidMessage, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return messageEnvelope{}, wager.WagerCommand{}, "", ErrInvalidMessage
	}
	if strings.TrimSpace(message.MessageID) == "" || message.Type != "WagerTransactionRequested" || message.OccurredAt.IsZero() {
		return messageEnvelope{}, wager.WagerCommand{}, "", ErrInvalidMessage
	}
	var kind domainwager.Kind
	if message.Data.Kind == "" {
		kind = domainwager.Bet
	} else {
		kind = domainwager.Kind(message.Data.Kind)
	}
	switch kind {
	case domainwager.Bet, domainwager.Win, domainwager.Loss, domainwager.Refund, domainwager.Rollback:
	default:
		return messageEnvelope{}, wager.WagerCommand{}, "", ErrInvalidMessage
	}
	var amount struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(message.Data.Money, &amount); err != nil {
		return messageEnvelope{}, wager.WagerCommand{}, "", errors.Join(ErrInvalidMessage, err)
	}
	value, err := money.NewExternal(amount.Amount, amount.Currency)
	if err != nil {
		return messageEnvelope{}, wager.WagerCommand{}, "", errors.Join(ErrInvalidMessage, err)
	}
	command := wager.WagerCommand{
		ProviderID:                     message.Data.ProviderID,
		ExternalTransactionID:          message.Data.ExternalTransactionID,
		IdempotencyKey:                 message.Data.IdempotencyKey,
		CorrelationID:                  message.MessageID,
		WalletID:                       message.Data.WalletID,
		PlayerID:                       message.Data.PlayerID,
		RoundID:                        message.Data.RoundID,
		GameID:                         message.Data.GameID,
		ReferenceExternalTransactionID: message.Data.ReferenceExternalTransactionID,
		Money:                          value,
	}
	return message, command, kind, nil
}
