// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// TestModelStatus_ServedModelSurfaces_RoundTripThroughAPIServer is I1
// (Block 1 review): status.serviceURL and status.servedModel had ZERO test
// coverage of any kind — a renamed or dropped JSON tag on either field
// shipped completely undetected, through both make test-unit AND make
// test-envtest (confirmed by mutation in the block 1 review). This is that
// coverage: a real API server (envtest), a typed client, a Status().Update
// write, then a fresh typed Get — proving the wire contract, not just the
// Go struct literal.
//
// Task 4 is the first task to WRITE either field in production code (see
// model_controller.go's D65 verification block), so this belongs here
// rather than to Block 1 or to Task 5 (which writes status.serviceURL for
// real, "D25's controller half"). The Schedulable condition is included
// because it shares the exact same "added field, no writer yet, no
// coverage" shape — Task 5 populates it for real; this only proves the
// field name survives the round trip.
func TestModelStatus_ServedModelSurfaces_RoundTripThroughAPIServer(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const name = "qwen-status-wire"
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: manualNamespace},
		Spec:       exampleModelSpec(),
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	const wantServiceURL = "/proxy/services/main/" + name + "/"
	const wantServedModel = "qwen3-8-27b"

	model.Status.ServiceURL = wantServiceURL
	model.Status.ServedModel = wantServedModel
	meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
		Type:    squallv1alpha1.ConditionSchedulable,
		Status:  metav1.ConditionTrue,
		Reason:  squallv1alpha1.ReasonSchedulable,
		Message: "round trip fixture",
	})
	if err := k8sClient.Status().Update(ctx, model); err != nil {
		t.Fatalf("status update: %v", err)
	}

	got := &squallv1alpha1.Model{}
	key := types.NamespacedName{Name: name, Namespace: manualNamespace}
	if err := k8sClient.Get(ctx, key, got); err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Status.ServiceURL != wantServiceURL {
		t.Errorf("status.serviceURL = %q, want %q", got.Status.ServiceURL, wantServiceURL)
	}
	if got.Status.ServedModel != wantServedModel {
		t.Errorf("status.servedModel = %q, want %q", got.Status.ServedModel, wantServedModel)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionSchedulable)
	if cond == nil {
		t.Fatal("status.conditions carries no Schedulable condition after round trip")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != squallv1alpha1.ReasonSchedulable {
		t.Errorf("Schedulable condition = %+v, want Status=True Reason=%q", cond, squallv1alpha1.ReasonSchedulable)
	}
}
