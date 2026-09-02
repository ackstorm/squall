// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type FleetObservation struct{ Backend, Name, State string }

type ModelObservation struct {
	Namespace, Name, Phase, Backend, ProvisioningReason string
	RunActive                                           bool
	Replicas                                            int
	Fleets                                              []FleetObservation
}

type ModelOperationalCollector struct {
	mu          sync.Mutex
	models      map[modelKey]ModelObservation
	phase       *prometheus.Desc
	active      *prometheus.Desc
	replicas    *prometheus.Desc
	fleet       *prometheus.Desc
	failure     *prometheus.Desc
	transitions *prometheus.CounterVec
	attempts    *prometheus.CounterVec
	outcomes    *prometheus.CounterVec
	duration    *prometheus.HistogramVec
}

func NewModelOperationalCollector() *ModelOperationalCollector {
	return &ModelOperationalCollector{
		models:      make(map[modelKey]ModelObservation),
		phase:       prometheus.NewDesc("squall_model_phase", "Current Model phase.", []string{"namespace", "name", "phase"}, nil),
		active:      prometheus.NewDesc("squall_model_run_active", "Whether dstack currently has a live run for the Model.", []string{"namespace", "name"}, nil),
		replicas:    prometheus.NewDesc("squall_model_replicas", "Live replicas observed by Squall.", []string{"namespace", "name"}, nil),
		fleet:       prometheus.NewDesc("squall_model_fleet_state", "Current fleet admission state by backend.", []string{"namespace", "name", "backend", "fleet", "state"}, nil),
		failure:     prometheus.NewDesc("squall_model_provisioning_failure", "Latest unresolved provisioning failure.", []string{"namespace", "name", "backend", "reason"}, nil),
		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "squall_model_transitions_total", Help: "Model lifecycle transitions observed by this controller process."}, []string{"namespace", "name", "backend", "transition"}),
		attempts:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "squall_model_provisioning_attempts_total", Help: "Provisioning attempts made by this controller process."}, []string{"namespace", "name", "backend"}),
		outcomes:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "squall_model_provisioning_outcomes_total", Help: "Provisioning outcomes observed by this controller process."}, []string{"namespace", "name", "backend", "outcome", "reason"}),
		duration:    prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "squall_model_provisioning_duration_seconds", Help: "Time from wake actuation to provisioning outcome.", Buckets: []float64{15, 30, 60, 120, 300, 600, 1200, 1800}}, []string{"namespace", "name", "backend", "outcome"}),
	}
}

func (c *ModelOperationalCollector) Observe(o ModelObservation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models[modelKey{o.Namespace, o.Name}] = o
}

func (c *ModelOperationalCollector) Forget(namespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.models, modelKey{namespace, name})
}

func (c *ModelOperationalCollector) RecordTransition(namespace, name, backend, transition string) {
	c.transitions.WithLabelValues(namespace, name, backend, transition).Inc()
}

func (c *ModelOperationalCollector) RecordProvisioningAttempt(namespace, name, backend string) {
	c.attempts.WithLabelValues(namespace, name, backend).Inc()
}

func (c *ModelOperationalCollector) RecordProvisioningOutcome(namespace, name, backend, outcome, reason string, duration time.Duration) {
	c.outcomes.WithLabelValues(namespace, name, backend, outcome, reason).Inc()
	c.duration.WithLabelValues(namespace, name, backend, outcome).Observe(duration.Seconds())
}

func (c *ModelOperationalCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.phase, c.active, c.replicas, c.fleet, c.failure} {
		ch <- d
	}
	c.transitions.Describe(ch)
	c.attempts.Describe(ch)
	c.outcomes.Describe(ch)
	c.duration.Describe(ch)
}

func (c *ModelOperationalCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, o := range c.models {
		ch <- prometheus.MustNewConstMetric(c.phase, prometheus.GaugeValue, 1, o.Namespace, o.Name, o.Phase)
		active := 0.0
		if o.RunActive {
			active = 1
		}
		ch <- prometheus.MustNewConstMetric(c.active, prometheus.GaugeValue, active, o.Namespace, o.Name)
		ch <- prometheus.MustNewConstMetric(c.replicas, prometheus.GaugeValue, float64(o.Replicas), o.Namespace, o.Name)
		for _, f := range o.Fleets {
			ch <- prometheus.MustNewConstMetric(c.fleet, prometheus.GaugeValue, 1, o.Namespace, o.Name, f.Backend, f.Name, f.State)
		}
		if o.ProvisioningReason != "" {
			ch <- prometheus.MustNewConstMetric(c.failure, prometheus.GaugeValue, 1, o.Namespace, o.Name, o.Backend, o.ProvisioningReason)
		}
	}
	c.transitions.Collect(ch)
	c.attempts.Collect(ch)
	c.outcomes.Collect(ch)
	c.duration.Collect(ch)
}
