// SPDX-License-Identifier: MIT
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ReaperCollector exports the orphan reaper's health. Both gauges are always
// present: the timestamp reads 0 until the first successful sweep.
type ReaperCollector struct {
	mu                       sync.Mutex
	lastSuccess              time.Time
	refused                  int
	lastSuccessDesc, refDesc *prometheus.Desc
}

func NewReaperCollector() *ReaperCollector {
	return &ReaperCollector{
		lastSuccessDesc: prometheus.NewDesc("squall_reaper_last_success_timestamp_seconds", "Unix time of the last orphan sweep that returned no error; 0 before the first.", nil, nil),
		refDesc:         prometheus.NewDesc("squall_reaper_refused_runs", "Unowned-by-name, non-terminal, past-grace dstack runs the last sweep saw and did not stop.", nil, nil),
	}
}

func (c *ReaperCollector) SweepSucceeded(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSuccess = at
}

func (c *ReaperCollector) SetRefused(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refused = n
}

func (c *ReaperCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.lastSuccessDesc
	ch <- c.refDesc
}

func (c *ReaperCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ts float64
	if !c.lastSuccess.IsZero() {
		ts = float64(c.lastSuccess.Unix())
	}
	ch <- prometheus.MustNewConstMetric(c.lastSuccessDesc, prometheus.GaugeValue, ts)
	ch <- prometheus.MustNewConstMetric(c.refDesc, prometheus.GaugeValue, float64(c.refused))
}
