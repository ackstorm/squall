// SPDX-License-Identifier: MIT

package squall

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
	"github.com/ackstorm/squall/internal/metrics"
)

func TestRecordOperationalMetricsCountsReadyTransitionOnce(t *testing.T) {
	c := metrics.NewModelOperationalCollector()
	r := ModelReconciler{OperationalMetrics: c}
	now := time.Now().UTC()
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Namespace: "squall", Name: "qwen"},
		Spec:       squallv1alpha1.ModelSpec{Placement: squallv1alpha1.ModelPlacement{Backends: []string{"vastai"}}},
		Status:     squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, WakeStartedAt: &metav1.Time{Time: now.Add(-time.Minute)}},
	}
	observed := Observed{Run: &dstack.Run{RunID: "run", Replicas: 1}}

	r.recordOperationalMetrics(model, observed, squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseWaking}, now)
	r.recordOperationalMetrics(model, observed, squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady}, now)

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "squall_model_provisioning_outcomes_total" {
			if got := mf.GetMetric()[0].GetCounter().GetValue(); got != 1 {
				t.Fatalf("success outcomes = %v, want 1", got)
			}
			return
		}
	}
	t.Fatal("squall_model_provisioning_outcomes_total missing")
}

func TestRecordOperationalMetricsCountsProvisioningFailureOnce(t *testing.T) {
	c := metrics.NewModelOperationalCollector()
	r := ModelReconciler{OperationalMetrics: c}
	now := time.Now().UTC()
	failure := metav1.Condition{
		Type: squallv1alpha1.ConditionProvisioning, Status: metav1.ConditionFalse,
		Reason: squallv1alpha1.ReasonNoCapacity, Message: "dstack run qwen: no capacity",
	}
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Namespace: "squall", Name: "qwen"},
		Spec:       squallv1alpha1.ModelSpec{Placement: squallv1alpha1.ModelPlacement{Backends: []string{"vastai"}}},
		Status: squallv1alpha1.ModelStatus{
			Phase: squallv1alpha1.ModelPhaseRecreating, WakeStartedAt: &metav1.Time{Time: now.Add(-time.Minute)},
			Conditions: []metav1.Condition{failure},
		},
	}

	r.recordOperationalMetrics(model, Observed{}, squallv1alpha1.ModelStatus{}, now)
	r.recordOperationalMetrics(model, Observed{}, squallv1alpha1.ModelStatus{Conditions: []metav1.Condition{failure}}, now)

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "squall_model_provisioning_outcomes_total" {
			if got := mf.GetMetric()[0].GetCounter().GetValue(); got != 1 {
				t.Fatalf("failure outcomes = %v, want 1", got)
			}
			return
		}
	}
	t.Fatal("squall_model_provisioning_outcomes_total missing")
}

// An asleep on-demand Model keeps its status.runId (F17), so age must not
// be reported for it: otherwise squall_model_age_seconds grows at $0 and
// SquallModelExceededMaxLifetime fires for a Model that has not run a GPU
// in weeks (issue #23).
func TestRecordMetrics_AgeOnlyWhileAwake(t *testing.T) {
	c := metrics.NewModelAgeCollector(nil)
	r := ModelReconciler{AgeMetrics: c}
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Namespace: "squall", Name: "ollama-tiny"},
		Spec:       squallv1alpha1.ModelSpec{MaxLifetime: metav1.Duration{Duration: 4 * time.Hour}},
		Status:     squallv1alpha1.ModelStatus{RunID: "run-1"},
	}
	ageSamples := func() int {
		reg := prometheus.NewRegistry()
		reg.MustRegister(c)
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, mf := range mfs {
			if mf.GetName() == "squall_model_age_seconds" {
				return len(mf.GetMetric())
			}
		}
		return 0
	}

	r.recordMetrics(model, 0, nil, true, time.Now())
	if got := ageSamples(); got != 1 {
		t.Fatalf("awake: age samples = %d, want 1", got)
	}
	r.recordMetrics(model, 0, nil, false, time.Now())
	if got := ageSamples(); got != 0 {
		t.Fatalf("asleep with runId kept: age samples = %d, want 0", got)
	}
}
