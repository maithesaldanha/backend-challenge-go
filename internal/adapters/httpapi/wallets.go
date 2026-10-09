package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	applicationwallet "github.com/junglegaming/backend-challenge-go/internal/application/wallet"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

type Handler struct {
	openWallet *applicationwallet.OpenWallet
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

func NewHandler(openWallet *applicationwallet.OpenWallet, authenticator ports.Authenticator) (*Handler, error) {
	if openWallet == nil || authenticator == nil {
		return nil, errors.New("wallet opener and authenticator are required")
	}
	return &Handler{openWallet: openWallet, auth: authenticator}, nil
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /wallets", http.HandlerFunc(h.openWalletEndpoint))
	return mux
}

func (h *Handler) openWalletEndpoint(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeUnauthorized(w)
		return
	}
	principal, err := h.auth.Authenticate(r.Context(), token)
	if err != nil {
		writeUnauthorized(w)
		return
	}
	if !principal.HasRole("wallet:write") {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "forbidden"})
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

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
