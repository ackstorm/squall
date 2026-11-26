// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

func TestDecide_ProvisioningDeadlineMeasuresContinuousUnreadiness(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) *metav1.Time { return &metav1.Time{Time: now.Add(-d)} }
	readyCond := func(s metav1.ConditionStatus, since time.Duration) []metav1.Condition {
		return []metav1.Condition{{
			Type: squallv1alpha1.ConditionReady, Status: s, Reason: "x",
			LastTransitionTime: *ago(since),
		}}
	}
	spec := squallv1alpha1.ModelSpec{ProvisioningTimeout: metav1.Duration{Duration: 20 * time.Minute}}
	run := &dstack.Run{Name: "squall-qwen", Replicas: 1}

	for _, tc := range []struct {
		name     string
		prior    squallv1alpha1.ModelStatus
		wantDead bool
	}{
		{"was Ready on the prior pass", squallv1alpha1.ModelStatus{
			WakeStartedAt: ago(5 * time.Hour), Conditions: readyCond(metav1.ConditionTrue, time.Hour)}, false},
		{"recently lost readiness", squallv1alpha1.ModelStatus{
			WakeStartedAt: ago(5 * time.Hour), Conditions: readyCond(metav1.ConditionFalse, 5*time.Minute)}, false},
		{"unready past the timeout since the transition", squallv1alpha1.ModelStatus{
			WakeStartedAt: ago(5 * time.Hour), Conditions: readyCond(metav1.ConditionFalse, 30*time.Minute)}, true},
		{"never Ready, wake past the timeout", squallv1alpha1.ModelStatus{
			WakeStartedAt: ago(30 * time.Minute)}, true},
		{"never Ready, condition False since before the wake", squallv1alpha1.ModelStatus{
			WakeStartedAt: ago(5 * time.Minute), Conditions: readyCond(metav1.ConditionFalse, time.Hour)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phase, action := Decide(Observed{Run: run}, tc.prior, spec, true, now)
			if tc.wantDead {
				if phase != squallv1alpha1.ModelPhaseDead || action.Replicas != 0 || !action.ProvisioningTimedOut {
					t.Fatalf("want Dead + ProvisioningTimedOut, got %s %+v", phase, action)
				}
				return
			}
			if phase == squallv1alpha1.ModelPhaseDead || action.Apply {
				t.Fatalf("must not stop the run, got %s %+v", phase, action)
			}
		})
	}
}

func newReadyTestModel(name string, spec squallv1alpha1.ModelSpec, status squallv1alpha1.ModelStatus) *squallv1alpha1.Model {
	return &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Finalizers: []string{ModelFinalizer}},
		Spec:       spec,
		Status:     status,
	}
}

func newReadyTestClient(t *testing.T, model *squallv1alpha1.Model) client.Client {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).WithObjects(model).Build()
}

func TestReconcile_ReadyConditionFollowsObservation(t *testing.T) {
	model := newReadyTestModel("qwen-ready", exampleModelSpec(),
		squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"})
	c := newReadyTestClient(t, model)
	dc := &applyRecordingDstackClient{run: &dstack.Run{Name: "default-qwen-ready", RunID: "run-1", Replicas: 1, ProbesReady: true}}
	r := &ModelReconciler{Client: c, DstackClient: dc}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}

	for _, step := range []struct {
		probesReady bool
		want        metav1.ConditionStatus
	}{{true, metav1.ConditionTrue}, {false, metav1.ConditionFalse}, {true, metav1.ConditionTrue}} {
		dc.run.ProbesReady = step.probesReady
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		var got squallv1alpha1.Model
		if err := c.Get(context.Background(), req.NamespacedName, &got); err != nil {
			t.Fatalf("Get: %v", err)
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionReady)
		if cond == nil || cond.Status != step.want {
			t.Fatalf("probesReady=%v: Ready condition = %+v, want %s", step.probesReady, cond, step.want)
		}
	}
}

func TestReconcile_RecreateReanchorsWakeStartedAt(t *testing.T) {
	start := time.Now()
	spec := exampleModelSpec()
	stale := metav1.NewTime(start.Add(-3 * time.Hour))
	model := newReadyTestModel("qwen-reanchor", spec, squallv1alpha1.ModelStatus{
		Phase: squallv1alpha1.ModelPhaseReady, RunID: "dead-run", WakeStartedAt: &stale,
	})
	model.Annotations = map[string]string{squallv1alpha1.DemandAnnotation: start.UTC().Format(time.RFC3339)}
	c := newReadyTestClient(t, model)
	r := &ModelReconciler{
		Client:       c,
		DstackClient: &deadRecreateDstackClient{newRun: &dstack.Run{Name: "default-qwen-reanchor", RunID: "fresh-run", Replicas: 1}},
	}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got squallv1alpha1.Model
	if err := c.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.WakeStartedAt == nil || got.Status.WakeStartedAt.Time.Before(start.Add(-time.Second)) {
		t.Fatalf("wakeStartedAt = %v, want re-anchored to the recreate", got.Status.WakeStartedAt)
	}
	phase, action := Decide(
		Observed{Run: &dstack.Run{Name: "default-qwen-reanchor", RunID: "fresh-run", Replicas: 1}},
		got.Status, spec, true, time.Now())
	if phase == squallv1alpha1.ModelPhaseDead || action.ProvisioningTimedOut {
		t.Fatalf("the fresh run was timed out on the stale anchor: %s %+v", phase, action)
	}
}
