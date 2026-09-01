// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// ModelPriceCollector implements the AC19 declared/observed pair for cost:
// squall_model_price_per_hour (observed, from dstack) vs
// squall_model_max_price_per_hour (declared, spec.placement.maxPricePerHour).
//
// D26 is closed: the controller now feeds Observe's observed value from the
// current live replica's dstack provisioning price. Both sides are still
// required before either gauge is emitted — a declared-only series would
// misrepresent "no data yet" as "observed == 0", i.e. a price crash, or vice
// versa.
type ModelPriceCollector struct {
	mu      sync.Mutex
	entries map[modelKey]priceEntry

	observedDesc *prometheus.Desc
	declaredDesc *prometheus.Desc
}

type priceEntry struct {
	observedPerHour float64
	hasObserved     bool
	declaredPerHour float64
	hasDeclared     bool
}

func NewModelPriceCollector() *ModelPriceCollector {
	return &ModelPriceCollector{
		entries: make(map[modelKey]priceEntry),
		observedDesc: prometheus.NewDesc(
			"squall_model_price_per_hour",
			"Observed hourly price of the Model's current run, from dstack (spec §10, AC19).",
			[]string{"namespace", "name"}, nil,
		),
		declaredDesc: prometheus.NewDesc(
			"squall_model_max_price_per_hour",
			"spec.placement.maxPricePerHour declared for this Model (spec §10, AC19).",
			[]string{"namespace", "name"}, nil,
		),
	}
}

// Observe records model's current observed/declared price. Either side may
// be absent (hasObserved/hasDeclared false); the pair is only emitted once
// both are present (see Collect).
func (c *ModelPriceCollector) Observe(namespace, name string, observedPerHour float64, hasObserved bool, declaredPerHour float64, hasDeclared bool) {
	key := modelKey{namespace, name}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !hasObserved && !hasDeclared {
		delete(c.entries, key)
		return
	}
	c.entries[key] = priceEntry{observedPerHour, hasObserved, declaredPerHour, hasDeclared}
}

// Forget removes model, e.g. once its finalizer completes. Safe to call
// when the entry is absent.
func (c *ModelPriceCollector) Forget(namespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, modelKey{namespace, name})
}

func (c *ModelPriceCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.observedDesc
	ch <- c.declaredDesc
}

func (c *ModelPriceCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.entries {
		if !e.hasObserved || !e.hasDeclared {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.observedDesc, prometheus.GaugeValue, e.observedPerHour, key.Namespace, key.Name)
		ch <- prometheus.MustNewConstMetric(c.declaredDesc, prometheus.GaugeValue, e.declaredPerHour, key.Namespace, key.Name)
	}
}
