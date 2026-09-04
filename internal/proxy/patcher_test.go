// SPDX-License-Identifier: MIT

package proxy

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

func TestDynamicPatcher_PatchDemand_SetsAnnotation(t *testing.T) {
	scheme := runtime.NewScheme()
	initial := modelUnstructured("qwen", "Asleep", "30s")
	fakeClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{ModelGVR: "ModelList"},
		initial,
	)

	p := &DynamicPatcher{Client: fakeClient, Namespace: "default"}
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	if err := p.PatchDemand(context.Background(), "qwen", at); err != nil {
		t.Fatalf("PatchDemand: %v", err)
	}

	u, err := fakeClient.Resource(ModelGVR).Namespace("default").Get(context.Background(), "qwen", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	ann := u.GetAnnotations()
	if ann[squallv1alpha1.DemandAnnotation] != at.UTC().Format(time.RFC3339) {
		t.Fatalf("annotation %q = %q, want %q", squallv1alpha1.DemandAnnotation, ann[squallv1alpha1.DemandAnnotation], at.UTC().Format(time.RFC3339))
	}
}
