package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	applicationwager "github.com/junglegaming/backend-challenge-go/internal/application/wager"
	applicationwallet "github.com/junglegaming/backend-challenge-go/internal/application/wallet"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	domainwager "github.com/junglegaming/backend-challenge-go/internal/domain/wager"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

type Handler struct {
	openWallet                *applicationwallet.OpenWallet
	processBet                *applicationwager.ProcessBet
	processLoss               *applicationwager.ProcessLoss
	processWin                *applicationwager.ProcessWin
	processReversal           *applicationwager.ProcessReversal
	readWallet                *applicationwallet.ReadWallet
	readLedger                *applicationwallet.ReadWalletLedger
	readTransaction           *applicationwager.ReadTransaction
	reconcileWallet           *applicationwallet.ReconcileWallet
	auth                      ports.Authenticator
	reconciliationDivergences atomic.Uint64
}

type ledgerCursorPayload struct {
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
}

type ledgerEntryResponse struct {
	ID            string      `json:"id"`
	TransactionID string      `json:"transactionId"`
	Direction     string      `json:"direction"`
	Amount        money.Money `json:"amount"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	CreatedAt     time.Time   `json:"createdAt"`
}

type ledgerResponse struct {
	Entries    []ledgerEntryResponse `json:"entries"`
	NextCursor string                `json:"nextCursor,omitempty"`
}

type walletReadResponse struct {
	ID        string      `json:"id"`
	PlayerID  string      `json:"playerId"`
	Balance   money.Money `json:"balance"`
	Version   int64       `json:"version"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

type transactionReadResponse struct {
	TransactionID                  string       `json:"transactionId"`
	ProviderID                     string       `json:"providerId,omitempty"`
	ExternalTransactionID          string       `json:"externalTransactionId,omitempty"`
	WalletID                       string       `json:"walletId"`
	PlayerID                       string       `json:"playerId"`
	RoundID                        string       `json:"roundId,omitempty"`
	GameID                         string       `json:"gameId,omitempty"`
	Kind                           string       `json:"kind"`
	Money                          money.Money  `json:"money"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         string       `json:"referenceTransactionId,omitempty"`
	Status                         string       `json:"status"`
	FailureCode                    string       `json:"failureCode,omitempty"`
	ResultBalance                  *money.Money `json:"resultBalance,omitempty"`
	CreatedAt                      time.Time    `json:"createdAt"`
	UpdatedAt                      time.Time    `json:"updatedAt"`
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

type wagerRequest struct {
	Kind                           string      `json:"kind"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	PlayerID                       string      `json:"playerId"`
	WalletID                       string      `json:"walletId"`
	RoundID                        string      `json:"roundId"`
	GameID                         string      `json:"gameId"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
	Money                          money.Money `json:"money"`
}

type wagerHashPayload struct {
	ExternalTransactionID          string      `json:"externalTransactionId"`
	GameID                         string      `json:"gameId"`
	Kind                           string      `json:"kind"`
	Money                          money.Money `json:"money"`
	PlayerID                       string      `json:"playerId"`
	ProviderID                     string      `json:"providerId"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
	RoundID                        string      `json:"roundId"`
	WalletID                       string      `json:"walletId"`
}

type wagerResponse struct {
	TransactionID    string       `json:"transactionId"`
	Status           string       `json:"status"`
	Balance          *money.Money `json:"balance,omitempty"`
	FailureCode      string       `json:"failureCode,omitempty"`
	IdempotentReplay bool         `json:"idempotentReplay"`
}

func NewHandler(openWallet *applicationwallet.OpenWallet, processBet *applicationwager.ProcessBet, processLoss *applicationwager.ProcessLoss, processWin *applicationwager.ProcessWin, processReversal *applicationwager.ProcessReversal, readWallet *applicationwallet.ReadWallet, readLedger *applicationwallet.ReadWalletLedger, readTransaction *applicationwager.ReadTransaction, reconcileWallet *applicationwallet.ReconcileWallet, authenticator ports.Authenticator) (*Handler, error) {
	if openWallet == nil || processBet == nil || processLoss == nil || processWin == nil || processReversal == nil || readWallet == nil || readLedger == nil || readTransaction == nil || reconcileWallet == nil || authenticator == nil {
		return nil, errors.New("wallet and wager use cases and authenticator are required")
	}
	return &Handler{openWallet: openWallet, processBet: processBet, processLoss: processLoss, processWin: processWin, processReversal: processReversal, readWallet: readWallet, readLedger: readLedger, readTransaction: readTransaction, reconcileWallet: reconcileWallet, auth: authenticator}, nil
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /wallets", http.HandlerFunc(h.openWalletEndpoint))
	mux.Handle("GET /wallets/{walletID}", http.HandlerFunc(h.readWalletEndpoint))
	mux.Handle("GET /wallets/{walletID}/ledger", http.HandlerFunc(h.readLedgerEndpoint))
	mux.Handle("POST /wagering/transactions", http.HandlerFunc(h.processWagerEndpoint))
	mux.Handle("GET /wagering/transactions/{transactionID}", http.HandlerFunc(h.readTransactionEndpoint))
	mux.Handle("GET /providers/{providerID}/wagering/transactions/{externalTransactionID}", http.HandlerFunc(h.readProviderTransactionEndpoint))
	mux.Handle("POST /wallets/{walletID}/reconciliation", http.HandlerFunc(h.reconcileWalletEndpoint))
	mux.Handle("GET /metrics", http.HandlerFunc(h.metricsEndpoint))
	return mux
}

type reconciliationResponse struct {
	WalletID          string      `json:"walletId"`
	StoredBalance     money.Money `json:"storedBalance"`
	CalculatedBalance money.Money `json:"calculatedBalance"`
	Difference        money.Money `json:"difference"`
	Consistent        bool        `json:"consistent"`
	CheckedEntries    int64       `json:"checkedEntries"`
}

func (h *Handler) reconcileWalletEndpoint(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authorize(w, r, "wallet:write"); !ok {
		return
	}
	walletID := r.PathValue("walletID")
	if _, err := uuid.Parse(walletID); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
		return
	}
	result, err := h.reconcileWallet.Execute(r.Context(), walletID)
	if err != nil {
		writeReadError(w, err)
		return
	}
	if !result.Consistent {
		h.reconciliationDivergences.Add(1)
		stored, _ := result.StoredBalance.String()
		calculated, _ := result.CalculatedBalance.String()
		difference, _ := result.Difference.String()
		slog.Error("wallet reconciliation divergence", "wallet_id", result.WalletID, "stored_balance", stored, "calculated_balance", calculated, "difference", difference, "checked_entries", result.CheckedEntries)
	}
	writeJSON(w, http.StatusOK, reconciliationResponse{WalletID: result.WalletID, StoredBalance: result.StoredBalance, CalculatedBalance: result.CalculatedBalance, Difference: result.Difference, Consistent: result.Consistent, CheckedEntries: result.CheckedEntries})
}

func (h *Handler) metricsEndpoint(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "# TYPE wallet_reconciliation_divergences_total counter\nwallet_reconciliation_divergences_total %d\n", h.reconciliationDivergences.Load())
}

func (h *Handler) readWalletEndpoint(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authorize(w, r, "wallet:write"); !ok {
		return
	}
	id := r.PathValue("walletID")
	if _, err := uuid.Parse(id); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
		return
	}
	account, err := h.readWallet.Execute(r.Context(), id)
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, walletReadResponse{ID: account.ID(), PlayerID: account.PlayerID(), Balance: account.Balance(), Version: account.Version(), CreatedAt: account.CreatedAt(), UpdatedAt: account.UpdatedAt()})
}

func (h *Handler) readLedgerEndpoint(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authorize(w, r, "wallet:write"); !ok {
		return
	}
	walletID := r.PathValue("walletID")
	if _, err := uuid.Parse(walletID); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_limit"})
			return
		}
		limit = parsed
	}
	var cursor *ports.LedgerCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		var payload ledgerCursorPayload
		if err != nil || json.Unmarshal(decoded, &payload) != nil || payload.CreatedAt.IsZero() || strings.TrimSpace(payload.ID) == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_cursor"})
			return
		}
		if _, err := uuid.Parse(payload.ID); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_cursor"})
			return
		}
		cursor = &ports.LedgerCursor{CreatedAt: payload.CreatedAt.UTC(), ID: payload.ID}
	}
	page, err := h.readLedger.Execute(r.Context(), walletID, cursor, limit)
	if err != nil {
		writeReadError(w, err)
		return
	}
	response := ledgerResponse{Entries: make([]ledgerEntryResponse, 0, len(page.Entries))}
	for _, entry := range page.Entries {
		response.Entries = append(response.Entries, ledgerEntryResponse{ID: entry.ID(), TransactionID: entry.TransactionID(), Direction: string(entry.Direction()), Amount: entry.Money(), BalanceBefore: entry.BalanceBefore(), BalanceAfter: entry.BalanceAfter(), CreatedAt: entry.CreatedAt()})
	}
	if page.HasMore && len(page.Entries) > 0 {
		last := page.Entries[len(page.Entries)-1]
		encoded, _ := json.Marshal(ledgerCursorPayload{CreatedAt: last.CreatedAt(), ID: last.ID()})
		response.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) readTransactionEndpoint(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authorize(w, r, "wallet:write"); !ok {
		return
	}
	id := r.PathValue("transactionID")
	if _, err := uuid.Parse(id); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
		return
	}
	transaction, err := h.readTransaction.ByID(r.Context(), id)
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransactionReadResponse(transaction))
}

func (h *Handler) readProviderTransactionEndpoint(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.authorize(w, r, "wager:write")
	if !ok {
		return
	}
	providerID := r.PathValue("providerID")
	if strings.TrimSpace(principal.ProviderID) == "" || principal.ProviderID != providerID {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "transaction_not_found"})
		return
	}
	externalID := r.PathValue("externalTransactionID")
	if strings.TrimSpace(externalID) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
		return
	}
	transaction, err := h.readTransaction.ByProviderExternalID(r.Context(), providerID, externalID)
	if err != nil {
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransactionReadResponse(transaction))
}

func toTransactionReadResponse(transaction domainwager.Transaction) transactionReadResponse {
	response := transactionReadResponse{TransactionID: transaction.ID(), ProviderID: transaction.ProviderID(), ExternalTransactionID: transaction.ExternalTransactionID(), WalletID: transaction.WalletID(), PlayerID: transaction.PlayerID(), RoundID: transaction.RoundID(), GameID: transaction.GameID(), Kind: string(transaction.Kind()), Money: transaction.Money(), ReferenceExternalTransactionID: transaction.ReferenceExternalTransactionID(), ReferenceTransactionID: transaction.ReferenceTransactionID(), Status: string(transaction.Status()), FailureCode: transaction.FailureCode(), CreatedAt: transaction.CreatedAt(), UpdatedAt: transaction.UpdatedAt()}
	if balance, ok := transaction.ResultBalance(); ok {
		response.ResultBalance = &balance
	}
	return response
}

func writeReadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ports.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "not_found"})
	case errors.Is(err, ports.ErrUnavailable):
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "temporarily_unavailable"})
	default:
		slog.Error("read request failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal_error"})
	}
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

func (h *Handler) processWagerEndpoint(w http.ResponseWriter, r *http.Request) {
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
	var request wagerRequest
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
	kind := request.Kind
	if kind == "" {
		kind = string(domainwager.Bet)
	}
	if kind != string(domainwager.Bet) && kind != string(domainwager.Loss) && kind != string(domainwager.Win) && kind != string(domainwager.Refund) && kind != string(domainwager.Rollback) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "unsupported_transaction_kind"})
		return
	}
	payload, err := json.Marshal(wagerHashPayload{
		ProviderID:                     principal.ProviderID,
		ExternalTransactionID:          request.ExternalTransactionID,
		PlayerID:                       request.PlayerID,
		WalletID:                       request.WalletID,
		RoundID:                        request.RoundID,
		GameID:                         request.GameID,
		Kind:                           kind,
		Money:                          request.Money,
		ReferenceExternalTransactionID: request.ReferenceExternalTransactionID,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
		return
	}
	hash := sha256.Sum256(payload)
	command := applicationwager.WagerCommand{
		ProviderID:                     principal.ProviderID,
		ExternalTransactionID:          request.ExternalTransactionID,
		IdempotencyKey:                 idempotencyKey,
		PayloadHash:                    hex.EncodeToString(hash[:]),
		WalletID:                       request.WalletID,
		PlayerID:                       request.PlayerID,
		RoundID:                        request.RoundID,
		GameID:                         request.GameID,
		Money:                          request.Money,
		ReferenceExternalTransactionID: request.ReferenceExternalTransactionID,
	}
	var result applicationwager.WagerResult
	if kind == string(domainwager.Loss) {
		result, err = h.processLoss.Execute(r.Context(), command)
	} else if kind == string(domainwager.Win) {
		result, err = h.processWin.Execute(r.Context(), command)
	} else if kind == string(domainwager.Refund) || kind == string(domainwager.Rollback) {
		result, err = h.processReversal.Execute(r.Context(), domainwager.Kind(kind), command)
	} else {
		result, err = h.processBet.Execute(r.Context(), command)
	}
	if err != nil {
		writeWagerError(w, err)
		return
	}
	response := wagerResponse{
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
	} else if result.Transaction.Status() == domainwager.PendingReference {
		status = http.StatusAccepted
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

func writeWagerError(w http.ResponseWriter, err error) {
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
		errors.Is(err, domainwager.ErrInvalidKind), errors.Is(err, domainwager.ErrReferenceRequired):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request"})
	default:
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) {
			slog.Error("wager processing failed", "sqlstate", postgresError.Code, "constraint", postgresError.ConstraintName, "message", postgresError.Message)
		} else {
			slog.Error("wager processing failed", "error", err.Error())
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal_error"})
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
