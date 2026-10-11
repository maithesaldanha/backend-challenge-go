package observability

import (
	"strings"
	"testing"
	"time"
)

func TestMetricsRenderBoundedOperationalSignals(t *testing.T) {
	metrics := NewMetrics()
	metrics.ObserveHTTPRequest("POST", "/wagering/transactions", 201, 25*time.Millisecond)
	metrics.ObserveWager("PROCESSED", false)
	metrics.ObserveWager("PROCESSED", true)
	metrics.ObserveIdempotencyConflict()
	metrics.ObserveConcurrencyConflict()
	metrics.ObserveOutboxRetry()
	metrics.ObserveSQSRetry()
	metrics.ObserveSQSRetry()
	metrics.ObserveSQSDLQDepth(7)
	metrics.ObserveOutboxPublishLag(3 * time.Second)
	metrics.ObserveOutboxPublishLag(time.Second)

	output := metrics.PrometheusText(4)
	for _, expected := range []string{
		`http_requests_total{method="POST",route="/wagering/transactions",status="201"} 1`,
		`wager_transactions_total{status="PROCESSED"} 2`,
		"wager_idempotent_replays_total 1",
		"wager_idempotency_conflicts_total 1",
		"wager_concurrency_conflicts_total 1",
		"outbox_publish_retries_total 1",
		"sqs_retries_total 2",
		"sqs_dlq_approximate_messages 7",
		"outbox_publish_lag_seconds 3.000000000",
		"wallet_reconciliation_divergences_total 4",
	} {
		if !strings.Contains(output, expected) {
			t.Errorf("metrics output does not contain %q:\n%s", expected, output)
		}
	}
}

func TestNilMetricsCanBeUsedByOptionalConsumers(t *testing.T) {
	var metrics *Metrics
	metrics.ObserveHTTPRequest("GET", "/health/live", 200, time.Millisecond)
	metrics.ObserveWager("PROCESSED", false)
	metrics.ObserveOutboxRetry()
	metrics.ObserveSQSRetry()
	if output := metrics.PrometheusText(0); !strings.Contains(output, "wallet_reconciliation_divergences_total 0") {
		t.Fatalf("nil metrics output = %q", output)
	}
}
