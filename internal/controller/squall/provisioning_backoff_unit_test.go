// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"fmt"
	"testing"
	"time"

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

// TestReconcile_NoCapacity_PacesRecreates drives Reconcile pass after pass
// against a backend that can never satisfy the run, and asserts D163's
// ProvisioningRetryBackoff actually bounds how often a fresh run is minted.
// Without the backoff this loop recreates every failAfter (15s): ~20 runs
// in five minutes.
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

	const window = 5 * time.Minute
	for elapsed := time.Duration(0); elapsed < window; elapsed += 5 * time.Second {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}); err != nil {
			t.Fatalf("Reconcile at +%v: %v", elapsed, err)
		}
		clk.Advance(5 * time.Second)
	}

	// One attempt per (failAfter + backoff) plus the first, with slack for
	// the 5s pass granularity.
	maxApplies := int(window/(dc.failAfter+ProvisioningRetryBackoff)) + 2
	if dc.applies > maxApplies {
		t.Fatalf("minted %d runs in %v against a backend with no capacity, want <= %d: the D163 backoff is not pacing recreates",
			dc.applies, window, maxApplies)
	}
	if dc.applies < 2 {
		t.Fatalf("minted %d runs, want >= 2: wake must stay fail-open and keep retrying", dc.applies)
	}
}
