// SPDX-License-Identifier: MIT

package proxy

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type ProxyMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight *prometheus.GaugeVec
	held     *prometheus.GaugeVec
}

const (
	metricOutcomeForwarded  = "forwarded"
	metricOutcomeRejected   = "rejected"
	metricOutcomeFailed     = "failed"
	metricOutcomeClientGone = "client_gone"
)

func NewProxyMetrics(reg prometheus.Registerer) *ProxyMetrics {
	m := &ProxyMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "squall_proxy_requests_total", Help: "Requests handled by squall-proxy."}, []string{"model", "outcome", "status_code", "held"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "squall_proxy_request_duration_seconds", Help: "End-to-end proxy request latency.", Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1200}}, []string{"model", "outcome", "held"}),
		inFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "squall_proxy_requests_in_flight", Help: "Accepted requests currently in flight."}, []string{"model"}),
		held:     prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "squall_proxy_held_requests", Help: "Requests currently held for model wake-up."}, []string{"model"}),
	}
	reg.MustRegister(m.requests, m.duration, m.inFlight, m.held)
	return m
}

func (m *ProxyMetrics) Observe(model, outcome string, status int, held bool, elapsed time.Duration) {
	heldLabel := strconv.FormatBool(held)
	m.requests.WithLabelValues(model, outcome, strconv.Itoa(status), heldLabel).Inc()
	m.duration.WithLabelValues(model, outcome, heldLabel).Observe(elapsed.Seconds())
}

func (m *ProxyMetrics) Begin(model string) func() {
	m.inFlight.WithLabelValues(model).Inc()
	return func() { m.inFlight.WithLabelValues(model).Dec() }
}

func (m *ProxyMetrics) Hold(model string) func() {
	m.held.WithLabelValues(model).Inc()
	return func() { m.held.WithLabelValues(model).Dec() }
}

func modelMetricLabel(model string, hasCR bool) string {
	if !hasCR {
		return "_unknown"
	}
	return model
}

func requestMetricOutcome(rec requestRecord) string {
	switch rec.outcome {
	case outcomeCommitted:
		return metricOutcomeForwarded
	case outcomeImmediate:
		return metricOutcomeRejected
	case outcomeWaitContract:
		if rec.reason != "" {
			return metricOutcomeFailed
		}
		return metricOutcomeRejected
	case outcomeGatewayAuth:
		return metricOutcomeFailed
	case outcomeClientGone:
		return metricOutcomeClientGone
	default:
		return metricOutcomeFailed
	}
}
