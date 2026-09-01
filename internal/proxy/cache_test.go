// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

func TestCache_SetGetDeleteList(t *testing.T) {
	c := NewCache()

	if _, ok := c.Get("qwen"); ok {
		t.Fatalf("Get on empty cache: ok = true, want false")
	}

	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady})
	snap, ok := c.Get("qwen")
	if !ok || snap.Phase != squallv1alpha1.ModelPhaseReady {
		t.Fatalf("Get() = %+v, %v, want Ready, true", snap, ok)
	}

	if got := c.List(); len(got) != 1 || got[0] != "qwen" {
		t.Fatalf("List() = %v, want [qwen]", got)
	}

	c.Delete("qwen")
	if _, ok := c.Get("qwen"); ok {
		t.Fatalf("Get after Delete: ok = true, want false")
	}
	if got := c.List(); len(got) != 0 {
		t.Fatalf("List after Delete = %v, want empty", got)
	}
}

func TestCache_Subscribe_NotifiesOnSetAndDelete(t *testing.T) {
	c := NewCache()
	notify, cancel := c.Subscribe("qwen")
	t.Cleanup(cancel)

	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseWaking})
	select {
	case <-notify:
	case <-time.After(time.Second):
		t.Fatal("no notification within 1s of Set")
	}

	c.Delete("qwen")
	select {
	case <-notify:
	case <-time.After(time.Second):
		t.Fatal("no notification within 1s of Delete")
	}
}

// TestCache_Subscribe_IsKeyedByModel is D119: Await's notification branch
// runs a full attemptForward, so a GLOBAL broadcast made every unrelated
// Model status write in the cluster fire a forward from every held request
// — a forward storm against a replica that is still waking. A subscriber
// hears only its own Model.
func TestCache_Subscribe_IsKeyedByModel(t *testing.T) {
	c := NewCache()
	notify, cancel := c.Subscribe("qwen")
	t.Cleanup(cancel)

	c.Set("unrelated", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady})
	c.Delete("unrelated")
	select {
	case <-notify:
		t.Fatal("an unrelated model's write notified this subscriber")
	default:
	}

	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseWaking})
	select {
	case <-notify:
	case <-time.After(time.Second):
		t.Fatal("no notification within 1s of the subscribed model's own Set")
	}
}

func TestCache_Subscribe_CoalescesBurst(t *testing.T) {
	c := NewCache()
	notify, cancel := c.Subscribe("qwen")
	t.Cleanup(cancel)

	for i := 0; i < 5; i++ {
		c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseWaking})
	}

	select {
	case <-notify:
	case <-time.After(time.Second):
		t.Fatal("no notification after burst")
	}
	select {
	case <-notify:
		t.Fatal("second notification received: burst should coalesce to one pending slot")
	default:
	}
}

func TestCache_Subscribe_CancelStopsDelivery(t *testing.T) {
	c := NewCache()
	notify, cancel := c.Subscribe("qwen")
	cancel()

	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady})
	select {
	case _, ok := <-notify:
		if ok {
			t.Fatal("received a notification after cancel")
		}
	case <-time.After(50 * time.Millisecond):
	}
}

func modelUnstructured(name, phase, holdTimeout string) *unstructured.Unstructured {
	obj := map[string]interface{}{
		"apiVersion": "squall.ackstorm.ai/v1alpha1",
		"kind":       "Model",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"holdTimeout": holdTimeout,
		},
		"status": map[string]interface{}{
			"phase": phase,
		},
	}
	return &unstructured.Unstructured{Object: obj}
}

// modelUnstructuredWithConditions is modelUnstructured plus an optional
// status.conditions slice, for exercising the Schedulable-condition parsing
// in RunInformerCache's upsert (D25 / the Task 6 short-circuit). Phase is
// fixed at "Asleep" — this helper is only ever used to probe Schedulable
// parsing, never phase parsing.
func modelUnstructuredWithConditions(name string, conditions []interface{}) *unstructured.Unstructured {
	u := modelUnstructured(name, "Asleep", "1h")
	if conditions != nil {
		status := u.Object["status"].(map[string]interface{})
		status["conditions"] = conditions
	}
	return u
}

// TestRunInformerCache_SchedulableDefaultsTrueUnlessConditionSaysFalse pins
// D25/Task 6's fails-open invariant at the informer boundary: a Model the
// controller has not evaluated (no Schedulable condition at all, or one that
// is merely Unknown) must read as schedulable, exactly like a Model with no
// condition ever written. Only an explicit status:"False" flips it.
func TestRunInformerCache_SchedulableDefaultsTrueUnlessConditionSaysFalse(t *testing.T) {
	scheme := runtime.NewScheme()
	objs := []runtime.Object{
		modelUnstructuredWithConditions("no-conditions", nil),
		modelUnstructuredWithConditions("unknown", []interface{}{
			map[string]interface{}{"type": "Schedulable", "status": "Unknown"},
		}),
		modelUnstructuredWithConditions("unschedulable", []interface{}{
			map[string]interface{}{"type": "Schedulable", "status": "False"},
		}),
		modelUnstructuredWithConditions("explicitly-true", []interface{}{
			map[string]interface{}{"type": "Schedulable", "status": "True"},
		}),
	}
	fakeClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{ModelGVR: "ModelList"},
		objs...,
	)

	cache := NewCache()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = RunInformerCache(ctx, fakeClient, "", cache) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := cache.Get("no-conditions"); ok {
			if _, ok := cache.Get("unknown"); ok {
				if _, ok := cache.Get("unschedulable"); ok {
					if _, ok := cache.Get("explicitly-true"); ok {
						break
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("informer never synced all four objects within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	tests := []struct {
		name string
		want bool
	}{
		{"no-conditions", true},
		{"unknown", true},
		{"unschedulable", false},
		{"explicitly-true", true},
	}
	for _, tc := range tests {
		snap, _ := cache.Get(tc.name)
		if snap.Schedulable != tc.want {
			t.Fatalf("%s: Schedulable = %v, want %v", tc.name, snap.Schedulable, tc.want)
		}
	}
}

// TestRunInformerCache_SyncsSetAndDelete drives RunInformerCache over a fake
// dynamic client (in-process, no envtest) and asserts the cache reflects an
// initial object, an update, and a delete.
func TestRunInformerCache_SyncsSetAndDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	initial := modelUnstructured("qwen", "Waking", "30s")
	fakeClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			ModelGVR: "ModelList",
		},
		initial,
	)

	cache := NewCache()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() { errCh <- RunInformerCache(ctx, fakeClient, "", cache) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if snap, ok := cache.Get("qwen"); ok && snap.Phase == squallv1alpha1.ModelPhaseWaking {
			if snap.HoldTimeout != 30*time.Second {
				t.Fatalf("HoldTimeout = %v, want 30s", snap.HoldTimeout)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("informer never synced initial object within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	updated := modelUnstructured("qwen", "Ready", "30s")
	updated.SetResourceVersion(initial.GetResourceVersion())
	if _, err := fakeClient.Resource(ModelGVR).Namespace("default").Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for {
		if snap, ok := cache.Get("qwen"); ok && snap.Phase == squallv1alpha1.ModelPhaseReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("informer never observed the update within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := fakeClient.Resource(ModelGVR).Namespace("default").Delete(ctx, "qwen", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, ok := cache.Get("qwen"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("informer never observed the delete within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Fatalf("RunInformerCache returned %v, want nil or context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunInformerCache did not return within 5s of ctx cancel")
	}
}
