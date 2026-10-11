package sqsconsumer

import (
	"encoding/json"
	"testing"

	"github.com/junglegaming/backend-challenge-go/internal/application/wager"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	domainwager "github.com/junglegaming/backend-challenge-go/internal/domain/wager"
)

func TestDecodeMessageBuildsWagerCommand(t *testing.T) {
	body := []byte(`{"messageId":"message-1","type":"WagerTransactionRequested","occurredAt":"2026-10-11T12:00:00Z","data":{"providerId":"provider-a","externalTransactionId":"transaction-1","idempotencyKey":"provider-a:transaction-1","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","roundId":"round-1","gameId":"game-1","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}`)

	message, command, kind, err := decodeMessage(body)
	if err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if message.MessageID != "message-1" || kind != domainwager.Bet {
		t.Fatalf("message identity and kind = (%q, %q), want message-1 and BET", message.MessageID, kind)
	}
	if command.ProviderID != "provider-a" || command.IdempotencyKey != "provider-a:transaction-1" || command.CorrelationID != message.MessageID {
		t.Fatalf("unexpected command identity: %+v", command)
	}
	if amount, _ := command.Money.String(); amount != "25.00" {
		t.Fatalf("money = %q, want 25.00", amount)
	}
}

func TestDecodeMessageRejectsMalformedMessages(t *testing.T) {
	valid := map[string]any{
		"messageId":  "message-1",
		"type":       "WagerTransactionRequested",
		"occurredAt": "2026-10-11T12:00:00Z",
		"data": map[string]any{
			"providerId": "provider-a", "externalTransactionId": "transaction-1", "idempotencyKey": "key-1",
			"playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
			"roundId": "round-1", "gameId": "game-1", "kind": "BET",
			"money": map[string]string{"amount": "25.00", "currency": "BRL"},
		},
	}
	cases := map[string]func(map[string]any){
		"unsupported type": func(message map[string]any) { message["type"] = "Unknown" },
		"unknown field":    func(message map[string]any) { message["unexpected"] = true },
		"invalid kind": func(message map[string]any) {
			message["data"].(map[string]any)["kind"] = "OPENING"
		},
		"invalid money": func(message map[string]any) {
			message["data"].(map[string]any)["money"] = map[string]string{"amount": "-1.00", "currency": "BRL"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(valid)
			if err != nil {
				t.Fatal(err)
			}
			var message map[string]any
			if err := json.Unmarshal(encoded, &message); err != nil {
				t.Fatal(err)
			}
			mutate(message)
			encoded, err = json.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := decodeMessage(encoded); err == nil {
				t.Fatal("decodeMessage succeeded, want malformed message error")
			}
		})
	}
	if _, _, _, err := decodeMessage([]byte(`{} {}`)); err == nil {
		t.Fatal("decodeMessage accepted trailing JSON")
	}
}

func TestPayloadHashMatchesBetweenTransportMetadata(t *testing.T) {
	amount, err := money.NewExternal("25.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	command := wager.WagerCommand{
		ProviderID: "provider-a", ExternalTransactionID: "transaction-1", IdempotencyKey: "key-a",
		WalletID: "wallet-1", PlayerID: "player-1", RoundID: "round-1", GameID: "game-1", Money: amount,
	}
	first, err := wager.PayloadHash(command, string(domainwager.Bet))
	if err != nil {
		t.Fatal(err)
	}
	command.IdempotencyKey = "different-key"
	command.CorrelationID = "different-correlation"
	second, err := wager.PayloadHash(command, string(domainwager.Bet))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("transport metadata changed business hash: %s != %s", first, second)
	}
	changedAmount, _ := money.NewExternal("26.00", "BRL")
	command.Money = changedAmount
	third, err := wager.PayloadHash(command, string(domainwager.Bet))
	if err != nil {
		t.Fatal(err)
	}
	if first == third {
		t.Fatal("changing the amount did not change the business hash")
	}
}
