package observability

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type requestKey struct {
	method string
	route  string
	status int
}

type Metrics struct {
	mu                         sync.Mutex
	requests                   map[requestKey]uint64
	requestDurationCount       uint64
	requestDurationNanoseconds uint64
	wagerResults               map[string]uint64
	idempotentReplays          uint64
	idempotencyConflicts       uint64
	concurrencyConflicts       uint64
	outboxRetries              uint64
	sqsRetries                 uint64
	sqsDLQDepth                 int64
	outboxPublishLagSeconds    float64
}

func NewMetrics() *Metrics {
	return &Metrics{
		requests:     make(map[requestKey]uint64),
		wagerResults: make(map[string]uint64),
	}
}

func (m *Metrics) ObserveHTTPRequest(method, route string, status int, duration time.Duration) {
	if m == nil {
		return
	}
	if route == "" {
		route = "unmatched"
	}
	if duration < 0 {
		duration = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[requestKey{method: method, route: route, status: status}]++
	m.requestDurationCount++
	m.requestDurationNanoseconds += uint64(duration)
}

func (m *Metrics) ObserveWager(status string, replay bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wagerResults[status]++
	if replay {
		m.idempotentReplays++
	}
}

func (m *Metrics) ObserveIdempotencyConflict() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.idempotencyConflicts++
	m.mu.Unlock()
}

func (m *Metrics) ObserveConcurrencyConflict() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.concurrencyConflicts++
	m.mu.Unlock()
}

func (m *Metrics) ObserveOutboxRetry() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.outboxRetries++
	m.mu.Unlock()
}

func (m *Metrics) ObserveSQSRetry() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.sqsRetries++
	m.mu.Unlock()
}

func (m *Metrics) ObserveSQSDLQDepth(depth int64) {
	if m == nil {
		return
	}
	if depth < 0 {
		depth = 0
	}
	m.mu.Lock()
	m.sqsDLQDepth = depth
	m.mu.Unlock()
}

func (m *Metrics) ObserveOutboxPublishLag(lag time.Duration) {
	if m == nil {
		return
	}
	if lag < 0 {
		lag = 0
	}
	seconds := lag.Seconds()
	m.mu.Lock()
	if seconds > m.outboxPublishLagSeconds {
		m.outboxPublishLagSeconds = seconds
	}
	m.mu.Unlock()
}

func (m *Metrics) PrometheusText(reconciliationDivergences uint64) string {
	if m == nil {
		return fmt.Sprintf("# TYPE wallet_reconciliation_divergences_total counter\nwallet_reconciliation_divergences_total %d\n", reconciliationDivergences)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var output strings.Builder
	keys := make([]requestKey, 0, len(m.requests))
	for key := range m.requests {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].method != keys[j].method {
			return keys[i].method < keys[j].method
		}
		if keys[i].route != keys[j].route {
			return keys[i].route < keys[j].route
		}
		return keys[i].status < keys[j].status
	})
	output.WriteString("# TYPE http_requests_total counter\n")
	for _, key := range keys {
		fmt.Fprintf(&output, "http_requests_total{method=%q,route=%q,status=%q} %d\n", key.method, key.route, fmt.Sprint(key.status), m.requests[key])
	}
	output.WriteString("# TYPE http_request_duration_seconds summary\n")
	fmt.Fprintf(&output, "http_request_duration_seconds_count %d\n", m.requestDurationCount)
	fmt.Fprintf(&output, "http_request_duration_seconds_sum %.9f\n", float64(m.requestDurationNanoseconds)/float64(time.Second))
	output.WriteString("# TYPE wager_transactions_total counter\n")
	statuses := make([]string, 0, len(m.wagerResults))
	for status := range m.wagerResults {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	for _, status := range statuses {
		fmt.Fprintf(&output, "wager_transactions_total{status=%q} %d\n", status, m.wagerResults[status])
	}
	output.WriteString("# TYPE wager_idempotent_replays_total counter\n")
	fmt.Fprintf(&output, "wager_idempotent_replays_total %d\n", m.idempotentReplays)
	output.WriteString("# TYPE wager_idempotency_conflicts_total counter\n")
	fmt.Fprintf(&output, "wager_idempotency_conflicts_total %d\n", m.idempotencyConflicts)
	output.WriteString("# TYPE wager_concurrency_conflicts_total counter\n")
	fmt.Fprintf(&output, "wager_concurrency_conflicts_total %d\n", m.concurrencyConflicts)
	output.WriteString("# TYPE outbox_publish_retries_total counter\n")
	fmt.Fprintf(&output, "outbox_publish_retries_total %d\n", m.outboxRetries)
	output.WriteString("# TYPE sqs_retries_total counter\n")
	fmt.Fprintf(&output, "sqs_retries_total %d\n", m.sqsRetries)
	output.WriteString("# TYPE sqs_dlq_approximate_messages gauge\n")
	fmt.Fprintf(&output, "sqs_dlq_approximate_messages %d\n", m.sqsDLQDepth)
	output.WriteString("# TYPE outbox_publish_lag_seconds gauge\n")
	fmt.Fprintf(&output, "outbox_publish_lag_seconds %.9f\n", m.outboxPublishLagSeconds)
	output.WriteString("# TYPE wallet_reconciliation_divergences_total counter\n")
	fmt.Fprintf(&output, "wallet_reconciliation_divergences_total %d\n", reconciliationDivergences)
	return output.String()
}
