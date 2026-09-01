// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// TestReconcile_PinToggle_NoRecreate is Task 7.3's envtest half (T8, AC17):
// toggling spec.MinReplicas 1->0->1 on an already-awake Model must take
// effect without a recreate — RunID and DeploymentNum must stay exactly
// what the first wake minted, because once Observed.Run.Replicas > 0 the
// toggle can only ever land in phase.go's level-triggered no-op branch
// (no Activity evidence is gathered here, since ProxyService is left
// unset, so sleepDue is always false regardless of the unpinned window).
// A Decide bug that routed either toggle through the mint-fresh-run branch
// (observed.Run == nil path) would either surface as a CAS rejection (the
// fresh-run branch sends Current: nil against an already-live run)
// or, if the fake ever tolerated that, a changed RunID — either way this
// test's before/after comparison catches it.
func TestReconcile_PinToggle_NoRecreate(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const name = "qwen-7-3"
	spec := exampleModelSpec()
	spec.MinReplicas = 1 // pinned: wakes without a demand annotation (AC17).
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  manualNamespace,
			Finalizers: []string{ModelFinalizer}, // pre-seeded: Reconcile's finalizer-add pass is exercised elsewhere (Task 8.0).
		},
		Spec: spec,
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	r := &ModelReconciler{Client: k8sClient, DstackClient: dstackClient}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}

	// First reconcile: no run exists yet -> mints one (the only Apply this
	// whole test expects).
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (initial pinned wake): %v", err)
	}
	var afterWake squallv1alpha1.Model
	if err := k8sClient.Get(ctx, req.NamespacedName, &afterWake); err != nil {
		t.Fatalf("get model after wake: %v", err)
	}
	if afterWake.Status.RunID == "" {
		t.Fatal("Status.RunID empty after the initial pinned wake")
	}
	wantRunID := afterWake.Status.RunID
	wantDeploymentNum := afterWake.Status.DeploymentNum

	// Unpin: minReplicas 1 -> 0. No demand annotation is set and no
	// ProxyService is configured on r, so Activity stays nil and sleepDue
	// stays false — this must land in the no-op branch, not a flip.
	toUnpin := afterWake.DeepCopy()
	toUnpin.Spec.MinReplicas = 0
	if err := k8sClient.Update(ctx, toUnpin); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (unpinned): %v", err)
	}
	var afterUnpin squallv1alpha1.Model
	if err := k8sClient.Get(ctx, req.NamespacedName, &afterUnpin); err != nil {
		t.Fatalf("get model after unpin: %v", err)
	}
	if afterUnpin.Status.RunID != wantRunID || afterUnpin.Status.DeploymentNum != wantDeploymentNum {
		t.Fatalf("after unpinning: RunID=%q DeploymentNum=%d, want RunID=%q DeploymentNum=%d unchanged (no recreate)",
			afterUnpin.Status.RunID, afterUnpin.Status.DeploymentNum, wantRunID, wantDeploymentNum)
	}

	// Re-pin: minReplicas 0 -> 1. Observed.Run.Replicas is still > 0 from
	// dstack's point of view (nothing ever flipped it), so this must also
	// be a no-op, not a fresh mint.
	toRepin := afterUnpin.DeepCopy()
	toRepin.Spec.MinReplicas = 1
	if err := k8sClient.Update(ctx, toRepin); err != nil {
		t.Fatalf("re-pin: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (re-pinned): %v", err)
	}
	var afterRepin squallv1alpha1.Model
	if err := k8sClient.Get(ctx, req.NamespacedName, &afterRepin); err != nil {
		t.Fatalf("get model after re-pin: %v", err)
	}
	if afterRepin.Status.RunID != wantRunID || afterRepin.Status.DeploymentNum != wantDeploymentNum {
		t.Fatalf("after re-pinning: RunID=%q DeploymentNum=%d, want RunID=%q DeploymentNum=%d unchanged (no recreate)",
			afterRepin.Status.RunID, afterRepin.Status.DeploymentNum, wantRunID, wantDeploymentNum)
	}

	run, err := dstackClient.Get(ctx, runNameIn(manualNamespace, name))
	if err != nil {
		t.Fatalf("get dstack run: %v", err)
	}
	if run.Replicas != 1 {
		t.Fatalf("dstack Replicas = %d after the pin/unpin/re-pin round trip, want 1 (never flipped)", run.Replicas)
	}
}
