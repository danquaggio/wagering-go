package telemetry

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

type Metrics struct {
	Processed, Rejected, Pending, Failed, Duplicates, Retries, DLQ, Conflicts, Reconciliation, Requests, Nanos, OutboxAge atomic.Int64
}

func New() *Metrics { return &Metrics{} }
func (m *Metrics) Observe(status string, replay bool, start time.Time) {
	m.Requests.Add(1)
	m.Nanos.Add(time.Since(start).Nanoseconds())
	if replay {
		m.Duplicates.Add(1)
		return
	}
	switch status {
	case "PROCESSED":
		m.Processed.Add(1)
	case "REJECTED":
		m.Rejected.Add(1)
	case "PENDING_REFERENCE":
		m.Pending.Add(1)
	case "FAILED":
		m.Failed.Add(1)
	}
}
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for _, x := range []struct {
		n string
		v int64
	}{
		{`wager_results_total{status="PROCESSED"}`, m.Processed.Load()}, {`wager_results_total{status="REJECTED"}`, m.Rejected.Load()}, {`wager_results_total{status="PENDING_REFERENCE"}`, m.Pending.Load()}, {`wager_results_total{status="FAILED"}`, m.Failed.Load()},
		{"wager_duplicates_total", m.Duplicates.Load()}, {"wager_retries_total", m.Retries.Load()}, {"wager_dlq_messages", m.DLQ.Load()}, {"wager_concurrency_conflicts_total", m.Conflicts.Load()}, {"wager_reconciliation_divergences_total", m.Reconciliation.Load()}, {"wager_processing_duration_seconds_count", m.Requests.Load()}, {"wager_outbox_oldest_seconds", m.OutboxAge.Load()}} {
		_, _ = fmt.Fprintf(w, "%s %d\n", x.n, x.v)
	}
	n := m.Nanos.Load()
	_, _ = fmt.Fprintf(w, "wager_processing_duration_seconds_sum %d.%09d\n", n/int64(time.Second), n%int64(time.Second))
}
