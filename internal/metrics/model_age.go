// SPDX-License-Identifier: MIT

// Package metrics implements spec Section 10's declared/observed gauge
// pairs (AC19): squall_model_age_seconds vs squall_model_max_lifetime_seconds,
// and squall_model_price_per_hour vs squall_model_max_price_per_hour. Both
// pairs share one generic PromQL "observed > declared" alert shape
// (config/prometheus), parameterised only by which two metric names it
// compares.
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ackstorm/squall/internal/clock"
)

type modelKey struct{ Namespace, Name string }

// ModelAgeCollector implements the AC19 declared/observed pair for
// MaxLifetime: squall_model_age_seconds (observed) vs
// squall_model_max_lifetime_seconds (declared), labelled by
// namespace/name.
//
// "Age" is wall-clock time since this controller process first observed
// the Model's current status.runId, kept in memory only — ledger D7
// records that no persisted "since-when" anchor exists yet
// (status.runStartedAt stays deliberately unimplemented, blocking task
// 8.3's DESTRUCTIVE timeout). MaxLifetime is explicitly ALERT-ONLY and
// never actuated (spec §5.2), so an in-memory baseline that resets on
// controller restart is an accepted undercount for an advisory metric,
// not a control path.
type ModelAgeCollector struct {
	clock clock.Clock

	mu      sync.Mutex
	entries map[modelKey]ageEntry

	observedDesc *prometheus.Desc
	declaredDesc *prometheus.Desc
}

type ageEntry struct {
	runID       string
	since       time.Time
	maxLifetime time.Duration
	declared    bool
}

// NewModelAgeCollector builds a collector with zero entries. clk defaults
// to clock.RealClock{} when nil.
func NewModelAgeCollector(clk clock.Clock) *ModelAgeCollector {
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &ModelAgeCollector{
		clock:   clk,
		entries: make(map[modelKey]ageEntry),
		observedDesc: prometheus.NewDesc(
			"squall_model_age_seconds",
			"Wall-clock age of the Model's current run generation, observed by the controller (spec §10, AC19).",
			[]string{"namespace", "name"}, nil,
		),
		declaredDesc: prometheus.NewDesc(
			"squall_model_max_lifetime_seconds",
			"spec.maxLifetime declared for this Model (spec §10, AC19). Alert-only: never actuated.",
			[]string{"namespace", "name"}, nil,
		),
	}
}

// Observe records model's current run identity and declared MaxLifetime.
// Call on every reconcile. The age baseline resets whenever runID changes
// — a new run generation (F20) or a fresh run after having none — so age
// never survives a recreate. runID == "" (no live run) forgets the entry:
// there is nothing to alarm on age for a model with no run.
func (c *ModelAgeCollector) Observe(namespace, name, runID string, maxLifetime time.Duration, now time.Time) {
	key := modelKey{namespace, name}
	c.mu.Lock()
	defer c.mu.Unlock()
	if runID == "" {
		delete(c.entries, key)
		return
	}
	e, ok := c.entries[key]
	if !ok || e.runID != runID {
		e = ageEntry{runID: runID, since: now}
	}
	e.maxLifetime = maxLifetime
	e.declared = maxLifetime > 0
	c.entries[key] = e
}

// Forget removes model, e.g. once its finalizer completes. Safe to call
// when the entry is absent.
func (c *ModelAgeCollector) Forget(namespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, modelKey{namespace, name})
}

func (c *ModelAgeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.observedDesc
	ch <- c.declaredDesc
}

// Collect emits both gauges only for entries with a declared MaxLifetime:
// declared == 0 means "no threshold set", and must never be emitted — an
// `observed > declared` rule over a 0 declared value would immediately
// misfire for every model that has not opted in.
func (c *ModelAgeCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	for key, e := range c.entries {
		if !e.declared {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.observedDesc, prometheus.GaugeValue, now.Sub(e.since).Seconds(), key.Namespace, key.Name)
		ch <- prometheus.MustNewConstMetric(c.declaredDesc, prometheus.GaugeValue, e.maxLifetime.Seconds(), key.Namespace, key.Name)
	}
}
