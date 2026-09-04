// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

// unhealthyFixture builds the 2026-08-29 incident in miniature: a live run, a
// proxy replica reporting current traffic and a last success older than the
// window, and whatever failure count the case is about.
//
// ScaleDownDelaySeconds is deliberately HUGE. It does three things at once and
// all three matter: the idle flip cannot fire (the newest request is not aged
// past it), traffic reads as current to unhealthyDue, and Ready is satisfied by
// freshSuccess — evidence (b) — so the test needs no probe-delay clock
// gymnastics against the shared fake.
func unhealthyFixture(t *testing.T, name string, failures int, unhealthyAfter time.Duration) (*squallv1alpha1.Model, *ModelReconciler, ctrl.Request) {
	t.Helper()
	ctx := context.Background()

	spec := exampleModelSpec()
	spec.MinReplicas = 0
	spec.ScaleDownDelaySeconds = 3600
	spec.Health = squallv1alpha1.ModelHealth{
		UnhealthyAfter:   metav1.Duration{Duration: unhealthyAfter},
		FailureThreshold: 3,
	}

	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  manualNamespace,
			Finalizers: []string{ModelFinalizer},
		},
		Spec: spec,
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create Model: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), model)
	})

	if _, err := dstackClient.Apply(ctx, dstack.ApplyRequest{Name: runNameIn(manualNamespace, name), Replicas: 1}); err != nil {
		t.Fatalf("seed dstack run: %v", err)
	}

	now := time.Now().UTC()
	nonLoopback := nonLoopbackIP(t)
	srv, _ := nonLoopbackActivityServer(t, nonLoopback, squallv1alpha1.ActivityReport{
		Models: map[string]squallv1alpha1.ModelActivity{
			name: {
				InFlight:             0,
				LastRequestAt:        now,
				LastSuccessAt:        now.Add(-10 * time.Second),
				FailuresSinceSuccess: failures,
			},
		},
	})

	proxyKey := types.NamespacedName{Namespace: manualNamespace, Name: "squall-proxy-" + name}
	endpoints := endpointSliceForAddr(t, proxyKey.Name+"-a", proxyKey.Namespace, srv.Listener.Addr().String(), proxyKey.Name)
	if err := k8sClient.Create(ctx, endpoints); err != nil {
		t.Fatalf("create EndpointSlice: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), endpoints)
	})

	r := &ModelReconciler{Client: k8sClient, DstackClient: dstackClient, ProxyService: proxyKey}
	return model, r, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}
}

// TestReconcile_UnhealthyReplicaIsScaledToZero is the 2026-08-29 incident as a
// test. Requests are arriving RIGHT NOW and the last delivered 2xx is older
// than the window, so the idle flip cannot fire — in-flight and recent traffic
// are exactly what kept that GPU alive all night. The unhealthy flip must.
func TestReconcile_UnhealthyReplicaIsScaledToZero(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const name = "qwen-unhealthy"
	model, r, req := unhealthyFixture(t, name, 5, time.Second)

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	run, err := dstackClient.Get(ctx, runNameIn(manualNamespace, name))
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Replicas != 0 {
		t.Fatalf("Replicas = %d, want 0: the replica was taking traffic and delivering nothing", run.Replicas)
	}

	var got squallv1alpha1.Model
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(model), &got); err != nil {
		t.Fatalf("re-get Model: %v", err)
	}
	if got.Status.Phase != squallv1alpha1.ModelPhaseAsleep {
		t.Errorf("phase = %q, want Asleep", got.Status.Phase)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionHealthy)
	if cond == nil {
		t.Fatal("no Healthy condition: an operator finding this Model Asleep cannot tell it was pushed")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Healthy = %q, want False", cond.Status)
	}
	if cond.Reason != squallv1alpha1.ReasonNoSuccessfulResponses {
		t.Errorf("Healthy reason = %q, want %q", cond.Reason, squallv1alpha1.ReasonNoSuccessfulResponses)
	}
}

// TestReconcile_ThinFailureEvidenceDoesNotTearDown is the guard that matters
// most in production and the one a time-only rule gets wrong: a Model that
// served fine, went quiet, and then failed a COUPLE of requests must keep its
// GPU. Same fixture, same aged last-success, two failures instead of five.
func TestReconcile_ThinFailureEvidenceDoesNotTearDown(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const name = "qwen-thin-evidence"
	model, r, req := unhealthyFixture(t, name, 2, time.Second)

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	run, err := dstackClient.Get(ctx, runNameIn(manualNamespace, name))
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Replicas != 1 {
		t.Fatalf("Replicas = %d, want 1: two failures is not enough evidence to spend a teardown on", run.Replicas)
	}

	var got squallv1alpha1.Model
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(model), &got); err != nil {
		t.Fatalf("re-get Model: %v", err)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionHealthy); cond != nil &&
		cond.Status == metav1.ConditionFalse {
		t.Error("Healthy = False on thin evidence: the Model was slandered, not diagnosed")
	}
}
