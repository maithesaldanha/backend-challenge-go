package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
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
	"github.com/junglegaming/backend-challenge-go/internal/observability"
	"go.uber.org/fx"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	config, err := loadConfig()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	options := []fx.Option{
		postgres.Module(config.postgres),
		fx.Supply(config.sqs),
		fx.Provide(observability.NewMetrics),
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
				func(transactor ports.Transactor, publisher applicationoutbox.Publisher, metrics *observability.Metrics) (*applicationoutbox.Dispatcher, error) {
					return applicationoutbox.NewDispatcher(transactor, publisher, func() time.Time { return time.Now().UTC() }, metrics)
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
		slog.Error("application setup failed", "error", err)
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
						slog.Error("outbox publish attempt failed", "error", err)
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

func registerSQSConsumerWorker(lifecycle fx.Lifecycle, queue *awssqs.Client, consumer *applicationsqs.Consumer, metrics *observability.Metrics) {
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
						slog.Error("SQS receive failed", "error", err)
						select {
						case <-workerContext.Done():
							return
						case <-time.After(time.Second):
						}
						continue
					}
					dlqContext, dlqCancel := context.WithTimeout(workerContext, 2*time.Second)
					if depth, depthErr := queue.DLQDepth(dlqContext); depthErr != nil {
						slog.Error("SQS dead-letter queue depth query failed", "error", depthErr)
					} else {
						metrics.ObserveSQSDLQDepth(depth)
					}
					dlqCancel()
					for _, message := range messages {
						if err := consumer.Handle(workerContext, message.Body); err != nil {
							if workerContext.Err() != nil {
								releaseContext, releaseCancel := context.WithTimeout(context.WithoutCancel(workerContext), 2*time.Second)
								if releaseErr := queue.Release(releaseContext, message.ReceiptHandle); releaseErr != nil {
									slog.Error("SQS message release failed during shutdown", "message_id", message.ID, "error", releaseErr)
								}
								releaseCancel()
								return
							}
							metrics.ObserveSQSRetry()
							slog.Error("SQS message processing failed", "message_id", message.ID, "receive_count", message.ReceiveCount, "error", err)
							if retryErr := queue.Retry(workerContext, message.ReceiptHandle, message.ReceiveCount); retryErr != nil && workerContext.Err() == nil {
								slog.Error("SQS retry scheduling failed", "message_id", message.ID, "error", retryErr)
							}
							continue
						}
						deleteContext, deleteCancel := context.WithTimeout(context.WithoutCancel(workerContext), 2*time.Second)
						if err := queue.Delete(deleteContext, message.ReceiptHandle); err != nil {
							slog.Error("SQS message deletion failed", "message_id", message.ID, "error", err)
						}
						deleteCancel()
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
						slog.Error("pending reference retry failed", "error", err)
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
	dlqQueueURL := strings.TrimSpace(os.Getenv("SQS_DLQ_QUEUE_URL"))
	if sqsEnabled && dlqQueueURL == "" {
		return appConfig{}, fmt.Errorf("SQS_DLQ_QUEUE_URL is required when SQS_ENABLED=true")
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
			InputQueueURL: inputQueueURL, DLQQueueURL: dlqQueueURL,
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
