package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
)

type authenticatorFunc func(context.Context, string) (ports.Principal, error)

func (f authenticatorFunc) Authenticate(ctx context.Context, token string) (ports.Principal, error) {
	return f(ctx, token)
}

func TestProtectedWalletRouteRejectsMissingInvalidAndInsufficientAuthentication(t *testing.T) {
	tests := []struct {
		name           string
		authorization  string
		principal      ports.Principal
		authErr        error
		wantStatus     int
		wantAuthHeader string
		wantAuthCalls  int
	}{
		{
			name:           "missing bearer token",
			wantStatus:     http.StatusUnauthorized,
			wantAuthHeader: `Bearer realm="backend"`,
		},
		{
			name:           "invalid bearer token",
			authorization:  "Bearer invalid-token",
			authErr:        ports.ErrUnauthenticated,
			wantStatus:     http.StatusUnauthorized,
			wantAuthHeader: `Bearer realm="backend"`,
			wantAuthCalls:  1,
		},
		{
			name:          "missing required role",
			authorization: "Bearer valid-token",
			principal:     ports.Principal{Roles: []string{"wager:write"}},
			wantStatus:    http.StatusForbidden,
			wantAuthCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			handler := (&Handler{auth: authenticatorFunc(func(context.Context, string) (ports.Principal, error) {
				calls++
				return tt.principal, tt.authErr
			})}).Routes()
			request := httptest.NewRequest(http.MethodGet, "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37", nil)
			if tt.authorization != "" {
				request.Header.Set("Authorization", tt.authorization)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.wantStatus, response.Body.String())
			}
			if got := response.Header().Get("WWW-Authenticate"); got != tt.wantAuthHeader {
				t.Fatalf("WWW-Authenticate = %q, want %q", got, tt.wantAuthHeader)
			}
			if calls != tt.wantAuthCalls {
				t.Fatalf("authenticator calls = %d, want %d", calls, tt.wantAuthCalls)
			}
		})
	}
}

func TestProviderTransactionLookupHidesOtherProviders(t *testing.T) {
	handler := (&Handler{auth: authenticatorFunc(func(context.Context, string) (ports.Principal, error) {
		return ports.Principal{ProviderID: "provider-a", Roles: []string{"wager:write"}}, nil
	})}).Routes()
	request := httptest.NewRequest(http.MethodGet, "/providers/provider-b/wagering/transactions/external-1", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"error":"transaction_not_found"`) {
		t.Fatalf("status=%d body=%s, want hidden transaction not found", response.Code, response.Body.String())
	}
}

func TestWagerRequestCannotOverrideProviderIdentity(t *testing.T) {
	handler := (&Handler{auth: authenticatorFunc(func(context.Context, string) (ports.Principal, error) {
		return ports.Principal{ProviderID: "provider-from-token", Roles: []string{"wager:write"}}, nil
	})}).Routes()
	request := httptest.NewRequest(http.MethodPost, "/wagering/transactions", strings.NewReader(`{"providerId":"provider-from-body"}`))
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "key-1")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"error":"invalid_request"`) {
		t.Fatalf("status=%d body=%s, want invalid request", response.Code, response.Body.String())
	}
}

func TestAuthenticatorErrorsAreNotExposed(t *testing.T) {
	handler := (&Handler{auth: authenticatorFunc(func(context.Context, string) (ports.Principal, error) {
		return ports.Principal{}, errors.New("secret verifier detail")
	})}).Routes()
	request := httptest.NewRequest(http.MethodGet, "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37", nil)
	request.Header.Set("Authorization", "Bearer invalid-token")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), "secret verifier detail") {
		t.Fatalf("status=%d body=%s, want generic unauthorized response", response.Code, response.Body.String())
	}
}
