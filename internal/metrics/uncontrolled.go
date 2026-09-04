// SPDX-License-Identifier: MIT

package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ackstorm/squall/internal/clock"
)

// UncontrolledCollector exports the observed duration and declared deadline
// for Models whose proxy activity evidence is unavailable.
type UncontrolledCollector struct {
	clock        clock.Clock
	mu           sync.Mutex
	entries      map[modelKey]uncontrolledEntry
	observedDesc *prometheus.Desc
	declaredDesc *prometheus.Desc
}

type uncontrolledEntry struct {
	since   time.Time
	timeout time.Duration
}

func NewUncontrolledCollector(clk clock.Clock) *UncontrolledCollector {
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &UncontrolledCollector{
		clock: clk, entries: make(map[modelKey]uncontrolledEntry),
		observedDesc: prometheus.NewDesc("squall_model_uncontrolled_seconds", "Seconds since squall lost control of Model idleness.", []string{"namespace", "name"}, nil),
		declaredDesc: prometheus.NewDesc("squall_model_uncontrolled_timeout_seconds", "Declared deadline for uncontrolled Model capacity.", []string{"namespace", "name"}, nil),
	}
}

func (c *UncontrolledCollector) Observe(namespace, name string, since *time.Time, timeout time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := uncontrolledEntry{timeout: timeout}
	if since != nil {
		e.since = *since
	}
	c.entries[modelKey{namespace, name}] = e
}

func (c *UncontrolledCollector) Forget(namespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, modelKey{namespace, name})
}

func (c *UncontrolledCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.observedDesc
	ch <- c.declaredDesc
}

func (c *UncontrolledCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	for key, e := range c.entries {
		seconds := 0.0
		if !e.since.IsZero() {
			seconds = now.Sub(e.since).Seconds()
		}
		ch <- prometheus.MustNewConstMetric(c.observedDesc, prometheus.GaugeValue, seconds, key.Namespace, key.Name)
		ch <- prometheus.MustNewConstMetric(c.declaredDesc, prometheus.GaugeValue, e.timeout.Seconds(), key.Namespace, key.Name)
	}
}
