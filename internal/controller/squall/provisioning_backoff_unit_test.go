// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/clock"
	"github.com/ackstorm/squall/internal/dstack"
)

// noCapacityDstack mints a run on every Apply and reports it dead with
// failed_to_start_due_to_no_capacity failAfter later — the shape measured
// on Vast.ai (issue #4, D163) and on kind (D172).
type noCapacityDstack struct {
	schedulableDstackClient
	clk       *clock.FakeClock
	failAfter time.Duration
	applies   int
	cur       *dstack.Run
	createdAt time.Time
}

func (d *noCapacityDstack) Get(context.Context, string) (*dstack.Run, error) {
	if d.cur == nil {
		return nil, dstack.ErrNotFound
	}
	if d.clk.Now().Sub(d.createdAt) < d.failAfter {
		r := *d.cur
		return &r, nil
	}
	return &dstack.Run{
		Name: d.cur.Name, RunID: d.cur.RunID, Status: "failed",
		ProvisioningFailure: &dstack.ProvisioningFailure{
			RunID: d.cur.RunID, Reason: "failed_to_start_due_to_no_capacity",
			Message: "No offers matching the run requirements",
		},
	}, nil
}

func (d *noCapacityDstack) Apply(_ context.Context, req dstack.ApplyRequest) (*dstack.Run, error) {
	d.applies++
	d.createdAt = d.clk.Now()
	d.cur = &dstack.Run{Name: req.Name, RunID: fmt.Sprintf("run-%d", d.applies), Status: "submitted", Replicas: 1}
	r := *d.cur
	return &r, nil
}

// TestReconcile_NoCapacity_PacesRecreates drives Reconcile the way
// controller-runtime does — again at once after a status write (the Model
// watch has no predicate), after RequeueAfter when one is asked for, and
// NEVER otherwise: dstack is not a Kubernetes object, so nothing else wakes
// the controller when a run dies. Against a backend that can never satisfy
// the run it asserts both halves of issue #4: retries keep happening (wake
// fails open; a pinned Model once sat Waking on a dead run because no pass
// requeued), and D163's backoff bounds them.
func TestReconcile_NoCapacity_PacesRecreates(t *testing.T) {
	model := pinnedColdModel("no-capacity", nil)
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).WithObjects(model).Build()
	clk := clock.NewFakeClock(time.Now())
	dc := &noCapacityDstack{
		schedulableDstackClient: schedulableDstackClient{backendConfigured: true, hasFleet: true},
		clk:                     clk, failAfter: 15 * time.Second,
	}
	r := &ModelReconciler{Client: c, DstackClient: dc, Clock: clk}
	key := client.ObjectKeyFromObject(model)
	// Status content, not resourceVersion: the fake client bumps the
	// version even for an empty merge patch, the real API server does not.
	status := func() squallv1alpha1.ModelStatus {
		var m squallv1alpha1.Model
		if err := c.Get(context.Background(), key, &m); err != nil {
			t.Fatal(err)
		}
		return m.Status
	}

	const window = 5 * time.Minute
	start := clk.Now()
	for pass := 0; clk.Now().Sub(start) < window; pass++ {
		if pass > 500 {
			t.Fatal("more than 500 passes in the window: a status write is retriggering Reconcile in a hot loop")
		}
		before := status()
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("Reconcile at +%v: %v", clk.Now().Sub(start), err)
		}
		switch {
		case !equality.Semantic.DeepEqual(status(), before):
			// Status write: the watch event re-enqueues immediately.
		case res.RequeueAfter > 0:
			clk.Advance(res.RequeueAfter)
		default:
			t.Fatalf("Reconcile at +%v asked for no requeue while the pinned Model wants a run (phase %q, %d runs minted): "+
				"nothing else will ever revisit it", clk.Now().Sub(start), mustPhase(t, c, key), dc.applies)
		}
	}

	// One attempt per (failAfter + backoff) plus the first, with slack for
	// the requeue granularity.
	maxApplies := int(window/(dc.failAfter+ProvisioningRetryBackoff)) + 2
	if dc.applies > maxApplies {
		t.Fatalf("minted %d runs in %v against a backend with no capacity, want <= %d: the D163 backoff is not pacing recreates",
			dc.applies, window, maxApplies)
	}
	if dc.applies < 3 {
		t.Fatalf("minted %d runs in %v, want >= 3: wake must stay fail-open and keep retrying", dc.applies, window)
	}
}

func mustPhase(t *testing.T, c client.Client, key client.ObjectKey) squallv1alpha1.ModelPhase {
	t.Helper()
	var m squallv1alpha1.Model
	if err := c.Get(context.Background(), key, &m); err != nil {
		t.Fatal(err)
	}
	return m.Status.Phase
}
