package auth

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	"go.uber.org/fx"
)

var ErrInvalidConfig = errors.New("invalid keycloak configuration")

type Config struct {
	IssuerURL string
	JWKSURL   string
	Audience  string
	ClockSkew time.Duration
}

type realmAccess struct {
	Roles []string `json:"roles"`
}

type accessTokenClaims struct {
	AuthorizedParty string      `json:"azp"`
	ProviderID      string      `json:"provider_id"`
	RealmAccess     realmAccess `json:"realm_access"`
	jwt.RegisteredClaims
}

type KeycloakAuthenticator struct {
	config Config
	mu     sync.RWMutex
	keys   keyfunc.Keyfunc
	cancel context.CancelFunc
}

func Module(config Config) fx.Option {
	return fx.Module("keycloak-auth",
		fx.Supply(config),
		fx.Provide(NewKeycloakAuthenticator),
	)
}

func NewKeycloakAuthenticator(lifecycle fx.Lifecycle, config Config) (ports.Authenticator, error) {
	if !validEndpoint(config.IssuerURL) || !validEndpoint(config.JWKSURL) ||
		strings.TrimSpace(config.Audience) == "" || config.ClockSkew < 0 {
		return nil, ErrInvalidConfig
	}
	authenticator := &KeycloakAuthenticator{config: config}
	lifecycle.Append(fx.Hook{
		OnStart: authenticator.start,
		OnStop:  authenticator.stop,
	})
	return authenticator, nil
}

func (a *KeycloakAuthenticator) start(context.Context) error {
	keyCtx, cancel := context.WithCancel(context.Background())
	keys, err := keyfunc.NewDefaultCtx(keyCtx, []string{a.config.JWKSURL})
	if err != nil {
		cancel()
		return err
	}
	a.mu.Lock()
	a.keys = keys
	a.cancel = cancel
	a.mu.Unlock()
	return nil
}

func (a *KeycloakAuthenticator) stop(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.keys = nil
	return nil
}

func (a *KeycloakAuthenticator) Authenticate(ctx context.Context, rawToken string) (ports.Principal, error) {
	if strings.TrimSpace(rawToken) == "" {
		return ports.Principal{}, ports.ErrUnauthenticated
	}
	a.mu.RLock()
	keys := a.keys
	a.mu.RUnlock()
	if keys == nil {
		return ports.Principal{}, ports.ErrUnauthenticated
	}
	claims := &accessTokenClaims{}
	options := []jwt.ParserOption{
		jwt.WithIssuer(a.config.IssuerURL),
		jwt.WithAudience(a.config.Audience),
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	}
	if a.config.ClockSkew > 0 {
		options = append(options, jwt.WithLeeway(a.config.ClockSkew))
	}
	token, err := jwt.ParseWithClaims(rawToken, claims, keys.KeyfuncCtx(ctx), options...)
	if err != nil || token == nil || !token.Valid || claims.Subject == "" || claims.AuthorizedParty == "" {
		return ports.Principal{}, ports.ErrUnauthenticated
	}
	return ports.Principal{
		Subject:    claims.Subject,
		ClientID:   claims.AuthorizedParty,
		ProviderID: claims.ProviderID,
		Roles:      append([]string(nil), claims.RealmAccess.Roles...),
	}, nil
}

func validEndpoint(raw string) bool {
	parsed, err := url.ParseRequestURI(raw)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != ""
}
