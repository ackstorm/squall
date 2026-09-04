// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// TestMaxPricePerHour_AdmissionAcceptsBothSpellings is D31, verified against
// a REAL API server (envtest, Kubernetes 1.31) rather than the generated
// YAML: both a bare JSON number and a quoted JSON string must admit.
func TestMaxPricePerHour_AdmissionAcceptsBothSpellings(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "squall-price-admission"}}
	if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	accepted := []interface{}{1.20, int64(2), "1.20", "2.20"}
	for i, price := range accepted {
		t.Run(fmt.Sprintf("accepts_%v", price), func(t *testing.T) {
			us := priceAdmissionFixture(t, ns.Name, i, price)
			if err := k8sClient.Create(ctx, us); err != nil {
				t.Fatalf("maxPricePerHour=%v: Create rejected, want admitted: %v", price, err)
			}
			_ = k8sClient.Delete(ctx, us)
		})
	}
}

// TestMaxPricePerHour_AdmissionDoesNotValidateContent is D70, verified
// against a real API server: the API server admits maxPricePerHour
// regardless of content — a non-numeric string, a quantity-style "m"
// suffix Price no longer accepts, a bool, a map, and a list all pass
// admission unchanged. This is a NEGATIVE result, not a desired one — it
// documents that D70 remains open. `Price`'s schema is
// x-kubernetes-preserve-unknown-fields with NO type (needed so both a bare
// JSON number and a JSON string admit, D31), and a
// +kubebuilder:validation:XValidation CEL rule was tried to close this and
// does NOT work on that schema shape: the API server refuses to even
// INSTALL the CRD, failing with "failed to construct type information for
// x-kubernetes-validations rules: unable to convert structural schema to
// CEL declarations" — reproduced with a trivial `rule: "true"`, so this is
// a property of the untyped+PreserveUnknownFields node, not of the rule
// text. See D70 in docs/references/deviations-and-findings.md.
//
// If this test ever starts failing because one of these values is
// REJECTED, that is good news (someone found a real fix) — update the
// test, not the code, and close D70.
func TestMaxPricePerHour_AdmissionDoesNotValidateContent(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "squall-price-admission"}}
	if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	admittedButBogus := []interface{}{
		"cheap",
		"1.2.3",
		"",
		"2200m", // D31 addendum: Price.UnmarshalJSON no longer accepts this either.
		true,
		map[string]interface{}{"a": "b"},
		[]interface{}{int64(1), int64(2)},
	}
	for i, price := range admittedButBogus {
		t.Run(fmt.Sprintf("admitted_despite_bogus_%v", price), func(t *testing.T) {
			us := priceAdmissionFixture(t, ns.Name, 100+i, price)
			if err := k8sClient.Create(ctx, us); err != nil {
				t.Fatalf("maxPricePerHour=%v: Create rejected — D70 has been fixed, update this test: %v", price, err)
			}
			_ = k8sClient.Delete(ctx, us)
		})
	}
}

// TestMaxPricePerHour_TypedDecodeIsTotal_ThenValidateRejects is D70's
// resolution, verified end to end against a real API server: a Model
// carrying a garbage maxPricePerHour round-trips through a TYPED client Get
// (squallv1alpha1.Model, not unstructured) without error, preserving the
// literal text — and Price.Validate then rejects that same value. Decoding
// a Model must always succeed; judging its content is a separate, later
// step (Task 5 wires Validate into the reconciler's Schedulable condition).
func TestMaxPricePerHour_TypedDecodeIsTotal_ThenValidateRejects(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "squall-price-admission"}}
	if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	us := priceAdmissionFixture(t, ns.Name, 200, "cheap")
	if err := k8sClient.Create(ctx, us); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	defer func() { _ = k8sClient.Delete(ctx, us) }()

	got := &squallv1alpha1.Model{}
	key := types.NamespacedName{Name: us.GetName(), Namespace: ns.Name}
	if err := k8sClient.Get(ctx, key, got); err != nil {
		t.Fatalf("typed Get of a Model with maxPricePerHour=\"cheap\" errored, want it to decode (D70): %v", err)
	}
	if got.Spec.Placement.MaxPricePerHour == nil {
		t.Fatal("spec.placement.maxPricePerHour = nil after decode, want the literal text preserved")
	}
	if got.Spec.Placement.MaxPricePerHour.String() != "cheap" {
		t.Fatalf("spec.placement.maxPricePerHour = %q, want %q (decode must preserve the text verbatim)", got.Spec.Placement.MaxPricePerHour.String(), "cheap")
	}

	if err := got.Spec.Placement.MaxPricePerHour.Validate(); err == nil {
		t.Fatal("Validate() = nil for maxPricePerHour=\"cheap\", want an error")
	}
}

// priceAdmissionFixture builds an otherwise-valid Model as
// unstructured.Unstructured, with spec.placement.maxPricePerHour
// overridden to an arbitrary raw JSON value — including shapes (bool, map,
// list) the typed Price string could never hold, so admission is tested
// against exactly what the wire allows, not what the Go type permits.
func priceAdmissionFixture(t *testing.T, namespace string, seq int, price interface{}) *unstructured.Unstructured {
	t.Helper()

	obj := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("price-admission-%d", seq),
			Namespace: namespace,
		},
		Spec: exampleModelSpec(),
	}

	u, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		t.Fatalf("ToUnstructured: %v", err)
	}
	if err := unstructured.SetNestedField(u, price, "spec", "placement", "maxPricePerHour"); err != nil {
		t.Fatalf("SetNestedField: %v", err)
	}

	us := &unstructured.Unstructured{Object: u}
	us.SetAPIVersion(squallv1alpha1.GroupVersion.String())
	us.SetKind("Model")
	return us
}
