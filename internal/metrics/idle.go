// SPDX-License-Identifier: Apache-2.0
package metrics

import (
	"github.com/ackstorm/squall/internal/clock"
	"github.com/prometheus/client_golang/prometheus"
	"sync"
	"time"
)

type IdleCollector struct {
	clock                      clock.Clock
	mu                         sync.Mutex
	entries                    map[modelKey]idleEntry
	observedDesc, declaredDesc *prometheus.Desc
}
type idleEntry struct {
	lastRequestAt time.Time
	window        time.Duration
}

func NewIdleCollector(clk clock.Clock) *IdleCollector {
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &IdleCollector{clock: clk, entries: make(map[modelKey]idleEntry),
		observedDesc: prometheus.NewDesc("squall_model_idle_seconds", "Seconds since the last request for a Model with live capacity.", []string{"namespace", "name"}, nil),
		declaredDesc: prometheus.NewDesc("squall_model_scale_down_delay_seconds", "Declared idle window after which capacity should sleep.", []string{"namespace", "name"}, nil)}
}
func (c *IdleCollector) Observe(ns, name string, last *time.Time, active bool, window time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := modelKey{ns, name}
	if !active || last == nil || last.IsZero() {
		delete(c.entries, k)
		return
	}
	c.entries[k] = idleEntry{*last, window}
}
func (c *IdleCollector) Forget(ns, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, modelKey{ns, name})
}
func (c *IdleCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.observedDesc
	ch <- c.declaredDesc
}
func (c *IdleCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	for k, e := range c.entries {
		ch <- prometheus.MustNewConstMetric(c.observedDesc, prometheus.GaugeValue, now.Sub(e.lastRequestAt).Seconds(), k.Namespace, k.Name)
		ch <- prometheus.MustNewConstMetric(c.declaredDesc, prometheus.GaugeValue, e.window.Seconds(), k.Namespace, k.Name)
	}
}
