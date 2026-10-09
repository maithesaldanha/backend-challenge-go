package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/auth"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/httpapi"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/postgres"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	applicationwager "github.com/junglegaming/backend-challenge-go/internal/application/wager"
	applicationwallet "github.com/junglegaming/backend-challenge-go/internal/application/wallet"
	"go.uber.org/fx"
)

func main() {
	config, err := loadConfig()
	if err != nil {
		log.Printf("invalid configuration: %v", err)
		os.Exit(1)
	}
	app := fx.New(
		postgres.Module(config.postgres),
		auth.Module(config.keycloak),
		httpapi.Module(config.http),
		fx.Provide(
			func(transactor *postgres.Transactor) ports.Transactor { return transactor },
			func(transactor ports.Transactor) (*applicationwallet.OpenWallet, error) {
				return applicationwallet.NewOpenWallet(transactor, uuid.NewString, func() time.Time {
					return time.Now().UTC()
				})
			},
			func(transactor ports.Transactor) (*applicationwager.ProcessBet, error) {
				return applicationwager.NewProcessBet(transactor, uuid.NewString, func() time.Time {
					return time.Now().UTC()
				})
			},
			func(transactor ports.Transactor) (*applicationwager.ProcessLoss, error) {
				return applicationwager.NewProcessLoss(transactor, uuid.NewString, func() time.Time {
					return time.Now().UTC()
				})
			},
			func(transactor ports.Transactor) (*applicationwager.ProcessWin, error) {
				return applicationwager.NewProcessWin(transactor, uuid.NewString, func() time.Time {
					return time.Now().UTC()
				})
			},
			func(transactor ports.Transactor) (*applicationwager.RetryPendingWins, error) {
				return applicationwager.NewRetryPendingWins(transactor, uuid.NewString, func() time.Time {
					return time.Now().UTC()
				})
			},
		),
		fx.Invoke(registerReferenceRetryWorker),
	)
	if err := app.Err(); err != nil {
		log.Printf("application setup failed: %v", err)
		os.Exit(1)
	}
	app.Run()
}

func registerReferenceRetryWorker(lifecycle fx.Lifecycle, processor *applicationwager.RetryPendingWins) {
	var cancel context.CancelFunc
	var done chan struct{}
	lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			workerContext, stop := context.WithCancel(context.Background())
			cancel = stop
			done = make(chan struct{})
			go func() {
				defer close(done)
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					count, err := processor.ProcessBatch(workerContext, 50)
					if err != nil {
						log.Printf("pending WIN retry failed: %v", err)
					}
					if count == 50 {
						continue
					}
					select {
					case <-workerContext.Done():
						return
					case <-ticker.C:
					}
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			if cancel == nil {
				return nil
			}
			cancel()
			select {
			case <-done:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
}

type appConfig struct {
	postgres postgres.Config
	keycloak auth.Config
	http     httpapi.ServerConfig
}

func loadConfig() (appConfig, error) {
	databaseURL, err := requiredEnv("DATABASE_URL")
	if err != nil {
		return appConfig{}, err
	}
	issuerURL, err := requiredEnv("KEYCLOAK_ISSUER_URL")
	if err != nil {
		return appConfig{}, err
	}
	jwksURL, err := requiredEnv("KEYCLOAK_JWKS_URL")
	if err != nil {
		return appConfig{}, err
	}
	audience, err := requiredEnv("OIDC_AUDIENCE")
	if err != nil {
		return appConfig{}, err
	}
	address := strings.TrimSpace(os.Getenv("HTTP_ADDR"))
	if address == "" {
		address = ":8080"
	}
	return appConfig{
		postgres: postgres.Config{DSN: databaseURL},
		keycloak: auth.Config{IssuerURL: issuerURL, JWKSURL: jwksURL, Audience: audience},
		http:     httpapi.ServerConfig{Address: address},
	}, nil
}

func requiredEnv(key string) (string, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return value, nil
}
