// SPDX-License-Identifier: MIT

package metrics

import (
	"testing"
	"time"
)

func TestModelOperationalCollectorScrape(t *testing.T) {
	c := NewModelOperationalCollector()
	c.Observe(ModelObservation{
		Namespace: "squall", Name: "qwen", Phase: "Recreating", Backend: "vastai",
		RunActive: false, Replicas: 0, ProvisioningReason: "no_capacity",
		Fleets: []FleetObservation{{Backend: "vastai", Name: "vast-fleet", State: "Created"}},
	})
	c.RecordTransition("squall", "qwen", "vastai", "recreate")
	c.RecordProvisioningAttempt("squall", "qwen", "vastai")
	c.RecordProvisioningOutcome("squall", "qwen", "vastai", "failure", "no_capacity", 42*time.Second)

	mfs := gather(t, c)
	for _, name := range []string{
		"squall_model_phase", "squall_model_run_active", "squall_model_replicas",
		"squall_model_fleet_state", "squall_model_provisioning_failure",
		"squall_model_transitions_total", "squall_model_provisioning_attempts_total",
		"squall_model_provisioning_outcomes_total", "squall_model_provisioning_duration_seconds",
	} {
		if findFamily(mfs, name) == nil {
			t.Fatalf("metric family %s missing", name)
		}
	}
	phase := findFamily(mfs, "squall_model_phase").GetMetric()[0]
	if labelValue(phase, "phase") != "Recreating" || phase.GetGauge().GetValue() != 1 {
		t.Fatalf("phase metric = %+v", phase)
	}
	failure := findFamily(mfs, "squall_model_provisioning_failure").GetMetric()[0]
	if labelValue(failure, "reason") != "no_capacity" || labelValue(failure, "backend") != "vastai" {
		t.Fatalf("failure labels = %+v", failure.GetLabel())
	}
	fleet := findFamily(mfs, "squall_model_fleet_state").GetMetric()[0]
	if labelValue(fleet, "fleet") != "vast-fleet" {
		t.Fatalf("fleet labels = %+v", fleet.GetLabel())
	}
}

func TestModelOperationalCollectorForgetDropsModelGauges(t *testing.T) {
	c := NewModelOperationalCollector()
	c.Observe(ModelObservation{Namespace: "squall", Name: "qwen", Phase: "Ready"})
	c.Forget("squall", "qwen")
	if mf := findFamily(gather(t, c), "squall_model_phase"); mf != nil && len(mf.GetMetric()) != 0 {
		t.Fatalf("phase remained after Forget: %+v", mf)
	}
}
