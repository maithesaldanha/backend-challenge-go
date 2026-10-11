package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/auth"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/awssqs"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/httpapi"
	"github.com/junglegaming/backend-challenge-go/internal/adapters/postgres"
	applicationhealth "github.com/junglegaming/backend-challenge-go/internal/application/health"
	applicationoutbox "github.com/junglegaming/backend-challenge-go/internal/application/outbox"
	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
	applicationsqs "github.com/junglegaming/backend-challenge-go/internal/application/sqsconsumer"
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
	options := []fx.Option{
		postgres.Module(config.postgres),
		fx.Supply(config.sqs),
		auth.Module(config.keycloak),
		httpapi.Module(config.http),
		fx.Provide(
			func(transactor *postgres.Transactor) ports.Transactor { return transactor },
			func(db *sql.DB) (*postgres.HealthChecker, error) { return postgres.NewHealthChecker(db) },
			func(transactor ports.Transactor) (*applicationwallet.OpenWallet, error) {
				return applicationwallet.NewOpenWallet(transactor, uuid.NewString, func() time.Time {
					return time.Now().UTC()
				})
			},
			func(transactor ports.Transactor) (*applicationwallet.ReadWallet, error) {
				return applicationwallet.NewReadWallet(transactor)
			},
			func(transactor ports.Transactor) (*applicationwallet.ReadWalletLedger, error) {
				return applicationwallet.NewReadWalletLedger(transactor)
			},
			func(transactor ports.Transactor) (*applicationwallet.ReconcileWallet, error) {
				return applicationwallet.NewReconcileWallet(transactor)
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
			func(transactor ports.Transactor) (*applicationwager.ProcessReversal, error) {
				return applicationwager.NewProcessReversal(transactor, uuid.NewString, func() time.Time {
					return time.Now().UTC()
				})
			},
			func(transactor ports.Transactor) (*applicationwager.ReadTransaction, error) {
				return applicationwager.NewReadTransaction(transactor)
			},
			func(transactor ports.Transactor) (*applicationwager.RetryPendingReferences, error) {
				return applicationwager.NewRetryPendingReferences(transactor, uuid.NewString, func() time.Time {
					return time.Now().UTC()
				})
			},
		),
		fx.Invoke(registerReferenceRetryWorker),
	}
	if config.sqsEnabled {
		options = append(options,
			fx.Provide(
				func(sqsConfig awssqs.Config) (*awssqs.Client, error) { return awssqs.New(sqsConfig) },
				func(client *awssqs.Client) applicationoutbox.Publisher { return client },
				func(database *postgres.HealthChecker, queue *awssqs.Client) (*applicationhealth.Readiness, error) {
					return applicationhealth.NewReadiness(database, queue)
				},
				func(transactor ports.Transactor, publisher applicationoutbox.Publisher) (*applicationoutbox.Dispatcher, error) {
					return applicationoutbox.NewDispatcher(transactor, publisher, func() time.Time { return time.Now().UTC() })
				},
				func(transactor ports.Transactor, processBet *applicationwager.ProcessBet, processLoss *applicationwager.ProcessLoss, processWin *applicationwager.ProcessWin, processReversal *applicationwager.ProcessReversal) (*applicationsqs.Consumer, error) {
					return applicationsqs.New(transactor, processBet, processLoss, processWin, processReversal, func() time.Time { return time.Now().UTC() })
				},
			),
			fx.Invoke(registerOutboxPublisherWorker),
			fx.Invoke(registerSQSConsumerWorker),
		)
	} else {
		options = append(options, fx.Provide(func(database *postgres.HealthChecker) (*applicationhealth.Readiness, error) {
			return applicationhealth.NewReadiness(database)
		}))
	}
	app := fx.New(options...)
	if err := app.Err(); err != nil {
		log.Printf("application setup failed: %v", err)
		os.Exit(1)
	}
	app.Run()
}

func registerOutboxPublisherWorker(lifecycle fx.Lifecycle, dispatcher *applicationoutbox.Dispatcher) {
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
					worked, err := dispatcher.ProcessOne(workerContext)
					if err != nil {
						log.Printf("outbox publish attempt failed: %v", err)
					}
					if worked {
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

func registerSQSConsumerWorker(lifecycle fx.Lifecycle, queue *awssqs.Client, consumer *applicationsqs.Consumer) {
	var cancel context.CancelFunc
	var done chan struct{}
	lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			workerContext, stop := context.WithCancel(context.Background())
			cancel = stop
			done = make(chan struct{})
			go func() {
				defer close(done)
				for {
					messages, err := queue.Receive(workerContext)
					if err != nil {
						if workerContext.Err() != nil {
							return
						}
						log.Printf("SQS receive failed: %v", err)
						select {
						case <-workerContext.Done():
							return
						case <-time.After(time.Second):
						}
						continue
					}
					for _, message := range messages {
						if err := consumer.Handle(workerContext, message.Body); err != nil {
							log.Printf("SQS message processing failed message_id=%q error=%v", message.ID, err)
							if retryErr := queue.Retry(workerContext, message.ReceiptHandle, message.ReceiveCount); retryErr != nil && workerContext.Err() == nil {
								log.Printf("SQS retry scheduling failed message_id=%q error=%v", message.ID, retryErr)
							}
							continue
						}
						if err := queue.Delete(workerContext, message.ReceiptHandle); err != nil {
							log.Printf("SQS message deletion failed message_id=%q error=%v", message.ID, err)
						}
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

func registerReferenceRetryWorker(lifecycle fx.Lifecycle, processor *applicationwager.RetryPendingReferences) {
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
						log.Printf("pending reference retry failed: %v", err)
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
	postgres   postgres.Config
	keycloak   auth.Config
	http       httpapi.ServerConfig
	sqs        awssqs.Config
	sqsEnabled bool
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
	sqsEnabled := strings.EqualFold(strings.TrimSpace(os.Getenv("SQS_ENABLED")), "true")
	queueURL := strings.TrimSpace(os.Getenv("SQS_QUEUE_URL"))
	if sqsEnabled && queueURL == "" {
		return appConfig{}, fmt.Errorf("SQS_QUEUE_URL is required when SQS_ENABLED=true")
	}
	inputQueueURL := strings.TrimSpace(os.Getenv("SQS_INPUT_QUEUE_URL"))
	if sqsEnabled && inputQueueURL == "" {
		return appConfig{}, fmt.Errorf("SQS_INPUT_QUEUE_URL is required when SQS_ENABLED=true")
	}
	region := strings.TrimSpace(os.Getenv("AWS_REGION"))
	if region == "" {
		region = "us-east-1"
	}
	sqsEndpoint := strings.TrimSpace(os.Getenv("SQS_ENDPOINT_URL"))
	address := strings.TrimSpace(os.Getenv("HTTP_ADDR"))
	if address == "" {
		address = ":8080"
	}
	return appConfig{
		postgres: postgres.Config{DSN: databaseURL},
		keycloak: auth.Config{IssuerURL: issuerURL, JWKSURL: jwksURL, Audience: audience},
		http:     httpapi.ServerConfig{Address: address},
		sqs: awssqs.Config{
			Region: region, Endpoint: sqsEndpoint, QueueURL: queueURL,
			InputQueueURL: inputQueueURL,
		},
		sqsEnabled: sqsEnabled,
	}, nil
}

func requiredEnv(key string) (string, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return value, nil
}
