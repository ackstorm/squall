// SPDX-License-Identifier: MIT

package proxy

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestProxyMetricsScrapeAndUnknownModelBound(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewProxyMetrics(reg)
	done := m.Begin(modelMetricLabel("qwen", true))
	release := m.Hold(modelMetricLabel("qwen", true))
	m.Observe(modelMetricLabel("attacker-controlled-value", false), "rejected", 404, false, 20*time.Millisecond)
	release()
	done()

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"squall_proxy_requests_total": false, "squall_proxy_request_duration_seconds": false,
		"squall_proxy_requests_in_flight": false, "squall_proxy_held_requests": false,
	}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
		for _, metric := range mf.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetValue() == "attacker-controlled-value" {
					t.Fatalf("caller-controlled model leaked into %s label", mf.GetName())
				}
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric family %s missing", name)
		}
	}
}

func TestRequestMetricOutcome(t *testing.T) {
	for _, tc := range []struct {
		record requestRecord
		want   string
	}{
		{requestRecord{outcome: outcomeCommitted}, "forwarded"},
		{requestRecord{outcome: outcomeImmediate}, "rejected"},
		{requestRecord{outcome: outcomeWaitContract}, "rejected"},
		{requestRecord{outcome: outcomeWaitContract, reason: "upstream refused"}, "failed"},
		{requestRecord{outcome: outcomeGatewayAuth}, "failed"},
		{requestRecord{outcome: outcomeClientGone}, "client_gone"},
	} {
		if got := requestMetricOutcome(tc.record); got != tc.want {
			t.Errorf("requestMetricOutcome(%+v) = %q, want %q", tc.record, got, tc.want)
		}
	}
}
