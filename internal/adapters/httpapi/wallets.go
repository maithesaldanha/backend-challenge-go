package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	applicationwager "github.com/junglegaming/backend-challenge-go/internal/application/wager"
	applicationwallet "github.com/junglegaming/backend-challenge-go/internal/application/wallet"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	domainwager "github.com/junglegaming/backend-challenge-go/internal/domain/wager"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

type Handler struct {
	openWallet *applicationwallet.OpenWallet
	processBet *applicationwager.ProcessBet
	auth       ports.Authenticator
}

type openWalletRequest struct {
	PlayerID       string      `json:"playerId"`
	InitialBalance money.Money `json:"initialBalance"`
}

type openWalletResponse struct {
	ID       string      `json:"id"`
	PlayerID string      `json:"playerId"`
	Balance  money.Money `json:"balance"`
	Version  int64       `json:"version"`
}

type betRequest struct {
	ExternalTransactionID string      `json:"externalTransactionId"`
	PlayerID              string      `json:"playerId"`
	WalletID              string      `json:"walletId"`
	RoundID               string      `json:"roundId"`
	GameID                string      `json:"gameId"`
	Money                 money.Money `json:"money"`
}

type betHashPayload struct {
	ExternalTransactionID string      `json:"externalTransactionId"`
	GameID                string      `json:"gameId"`
	Kind                  string      `json:"kind"`
	Money                 money.Money `json:"money"`
	PlayerID              string      `json:"playerId"`
	ProviderID            string      `json:"providerId"`
	RoundID               string      `json:"roundId"`
	WalletID              string      `json:"walletId"`
}

type betResponse struct {
	TransactionID    string       `json:"transactionId"`
	Status           string       `json:"status"`
	Balance          *money.Money `json:"balance,omitempty"`
	FailureCode      string       `json:"failureCode,omitempty"`
	IdempotentReplay bool         `json:"idempotentReplay"`
}

func NewHandler(openWallet *applicationwallet.OpenWallet, processBet *applicationwager.ProcessBet, authenticator ports.Authenticator) (*Handler, error) {
	if openWallet == nil || processBet == nil || authenticator == nil {
		return nil, errors.New("wallet opener, bet processor, and authenticator are required")
	}
	return &Handler{openWallet: openWallet, processBet: processBet, auth: authenticator}, nil
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /wallets", http.HandlerFunc(h.openWalletEndpoint))
	mux.Handle("POST /wagering/transactions", http.HandlerFunc(h.processBetEndpoint))
	return mux
}

func (h *Handler) openWalletEndpoint(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authorize(w, r, "wallet:write"); !ok {
		return
	}
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, errorResponse{Error: "content_type_must_be_application_json"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request openWalletRequest
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
		return
	}
	if _, err := uuid.Parse(request.PlayerID); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
		return
	}

	result, err := h.openWallet.Execute(r.Context(), applicationwallet.OpenWalletCommand{
		PlayerID:       request.PlayerID,
		InitialBalance: request.InitialBalance,
	})
	if err != nil {
		writeOpenWalletError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, openWalletResponse{
		ID:       result.Wallet.ID(),
		PlayerID: result.Wallet.PlayerID(),
		Balance:  result.Wallet.Balance(),
		Version:  result.Wallet.Version(),
	})
}

func (h *Handler) processBetEndpoint(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.authorize(w, r, "wager:write")
	if !ok {
		return
	}
	if strings.TrimSpace(principal.ProviderID) == "" {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "provider_identity_required"})
		return
	}
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, errorResponse{Error: "content_type_must_be_application_json"})
		return
	}
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if strings.TrimSpace(idempotencyKey) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "idempotency_key_required"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request betRequest
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
		return
	}
	if _, err := uuid.Parse(request.PlayerID); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
		return
	}
	if _, err := uuid.Parse(request.WalletID); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
		return
	}
	payload, err := json.Marshal(betHashPayload{
		ProviderID:            principal.ProviderID,
		ExternalTransactionID: request.ExternalTransactionID,
		PlayerID:              request.PlayerID,
		WalletID:              request.WalletID,
		RoundID:               request.RoundID,
		GameID:                request.GameID,
		Kind:                  string(domainwager.Bet),
		Money:                 request.Money,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
		return
	}
	hash := sha256.Sum256(payload)
	result, err := h.processBet.Execute(r.Context(), applicationwager.BetCommand{
		ProviderID:            principal.ProviderID,
		ExternalTransactionID: request.ExternalTransactionID,
		IdempotencyKey:        idempotencyKey,
		PayloadHash:           hex.EncodeToString(hash[:]),
		WalletID:              request.WalletID,
		PlayerID:              request.PlayerID,
		RoundID:               request.RoundID,
		GameID:                request.GameID,
		Money:                 request.Money,
	})
	if err != nil {
		writeBetError(w, err)
		return
	}
	response := betResponse{
		TransactionID:    result.Transaction.ID(),
		Status:           string(result.Transaction.Status()),
		FailureCode:      result.Transaction.FailureCode(),
		IdempotentReplay: result.IdempotentReplay,
	}
	status := http.StatusCreated
	if balance, exists := result.Transaction.ResultBalance(); exists {
		response.Balance = &balance
	}
	if result.Transaction.Status() == domainwager.Rejected {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, response)
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, role string) (ports.Principal, bool) {
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeUnauthorized(w)
		return ports.Principal{}, false
	}
	principal, err := h.auth.Authenticate(r.Context(), token)
	if err != nil {
		writeUnauthorized(w)
		return ports.Principal{}, false
	}
	if !principal.HasRole(role) {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "forbidden"})
		return ports.Principal{}, false
	}
	return principal, true
}

type errorResponse struct {
	Error string `json:"error"`
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	return parts[1], true
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="backend"`)
	writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
}

func writeOpenWalletError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ports.ErrUnavailable):
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "temporarily_unavailable"})
	case errors.Is(err, ports.ErrConflict):
		writeJSON(w, http.StatusConflict, errorResponse{Error: "wallet_already_exists"})
	case errors.Is(err, money.ErrInvalidAmount),
		errors.Is(err, money.ErrInvalidCurrency),
		errors.Is(err, money.ErrNegativeAmount),
		errors.Is(err, money.ErrUninitializedMoney),
		errors.Is(err, wallet.ErrInvalidIdentity),
		errors.Is(err, wallet.ErrInvalidTimestamp),
		errors.Is(err, wallet.ErrNegativeBalance):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
	default:
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal_error"})
	}
}

func writeBetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ports.ErrUnavailable):
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "temporarily_unavailable"})
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, applicationwager.ErrWalletOwnership):
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "wallet_not_found"})
	case errors.Is(err, ports.ErrConflict), errors.Is(err, applicationwager.ErrRequestConflict):
		writeJSON(w, http.StatusConflict, errorResponse{Error: "idempotency_conflict"})
	case errors.Is(err, money.ErrInvalidAmount), errors.Is(err, money.ErrInvalidCurrency),
		errors.Is(err, money.ErrNegativeAmount), errors.Is(err, money.ErrUninitializedMoney),
		errors.Is(err, money.ErrCurrencyMismatch), errors.Is(err, domainwager.ErrInvalidTransaction),
		errors.Is(err, domainwager.ErrInvalidKind):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
	default:
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal_error"})
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
