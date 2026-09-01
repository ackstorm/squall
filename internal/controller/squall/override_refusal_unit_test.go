// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

// overrideRefusingClient answers every Apply with dstack's "cannot override
// active run" 400 and records whether Stop was called.
//
// It is a hand-written double on purpose: internal/dstack/mock does NOT model
// the override rejection, so a test written against the fake would take the
// happy path and pass while asserting nothing (the vacuous-test failure mode
// this project keeps hitting).
type overrideRefusingClient struct {
	run        *dstack.Run
	stopped    []string
	reconciler *ModelReconciler
	model      *squallv1alpha1.Model
}

var _ dstack.Client = (*overrideRefusingClient)(nil)

func (c *overrideRefusingClient) Apply(context.Context, dstack.ApplyRequest) (*dstack.Run, error) {
	return nil, dstack.ErrCannotOverride
}
func (c *overrideRefusingClient) Get(context.Context, string) (*dstack.Run, error) {
	return c.run, nil
}
func (c *overrideRefusingClient) Stop(_ context.Context, name string) error {
	c.stopped = append(c.stopped, name)
	return nil
}
func (c *overrideRefusingClient) Delete(context.Context, string) error { return nil }
func (c *overrideRefusingClient) ListRuns(context.Context) ([]dstack.Run, error) {
	return []dstack.Run{*c.run}, nil
}
func (c *overrideRefusingClient) BackendConfigured(context.Context, string) (bool, error) {
	return true, nil
}
func (c *overrideRefusingClient) HasFleetFor(context.Context, string) (bool, error) {
	return true, nil
}
func (c *overrideRefusingClient) EnsureFleet(context.Context, dstack.FleetSpec) error { return nil }

// overrideFixtureModel is an ordinary wake-able Model: minReplicas 0, with the
// finalizer pre-seeded so Reconcile reaches the Apply rather than spending the
// pass adding it.
func overrideFixtureModel(t *testing.T, name string) *squallv1alpha1.Model {
	t.Helper()
	return &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Finalizers:  []string{ModelFinalizer},
			Annotations: map[string]string{squallv1alpha1.DemandAnnotation: metav1.Now().UTC().Format(time.RFC3339)},
		},
		Spec: exampleModelSpec(),
	}
}

// callRecover exercises recoverFromOverrideRefusal DIRECTLY rather than through
// Reconcile. Going through Reconcile made the "must not stop" assertion
// UNREACHABLE: with a run already at replicas 1 and demand present, Decide is a
// level-triggered no-op, so no Apply happens, so no refusal happens, and the
// test passed while proving nothing. Mutation M1 (dropping both guards) stayed
// green and exposed it.
func callRecover(t *testing.T, action Action, runReplicas int) *overrideRefusingClient {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	model := overrideFixtureModel(t, "override-fixture")
	dc := &overrideRefusingClient{}
	r := &ModelReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&squallv1alpha1.Model{}).WithObjects(model).Build(),
		DstackClient: dc,
	}
	action.Current = &dstack.Run{Name: "default-override-fixture", RunID: "run-1", Replicas: runReplicas}
	r.recoverFromOverrideRefusal(context.Background(), logr.Discard(), model, model.DeepCopy(), action,
		"default-override-fixture", dstack.ErrCannotOverride)
	dc.reconciler, dc.model = r, model
	return dc
}

// persistedSchedulable re-reads the Model from the API and returns the
// Schedulable condition as STORED. Asserting on the in-memory object would
// pass even if the write never landed, which is exactly the failure LIVE-1
// documents: Status().Update loses the optimistic lock for the whole life of
// a held request, and this code path only ever runs while one is held.
func (c *overrideRefusingClient) persistedSchedulable(t *testing.T) *metav1.Condition {
	t.Helper()
	var stored squallv1alpha1.Model
	if err := c.reconciler.Get(context.Background(), client.ObjectKeyFromObject(c.model), &stored); err != nil {
		t.Fatalf("re-read model: %v", err)
	}
	return meta.FindStatusCondition(stored.Status.Conditions, squallv1alpha1.ConditionSchedulable)
}

// TestOverrideRefusal_WakeOnAnIdleRunRecreatesIt is D140. dstack refuses to flip
// a run whose spec differs beyond the replica count, and retrying can never
// succeed -- so a routine `spec.env` edit would otherwise leave a Model that
// never wakes again. Nothing is serving at replicas 0, so stopping the run
// destroys no work and the next pass mints a fresh one through F20's ordinary
// Recreating path.
func TestOverrideRefusal_WakeOnAnIdleRunRecreatesIt(t *testing.T) {
	dc := callRecover(t, Action{Apply: true, Replicas: 1}, 0)
	if len(dc.stopped) != 1 {
		t.Fatalf("an un-flippable run at replicas 0 must be stopped so it can be recreated; stopped=%v", dc.stopped)
	}
}

// TestOverrideRefusal_NeverStopsARunThatIsServing is the guard that matters
// most. The same 400 against a run with live replicas must not be recovered
// from by stopping it: that kills whatever generation is in flight on the
// strength of an HTTP error.
func TestOverrideRefusal_NeverStopsARunThatIsServing(t *testing.T) {
	dc := callRecover(t, Action{Apply: true, Replicas: 1}, 1)
	if len(dc.stopped) != 0 {
		t.Fatalf("stopped %v while replicas > 0: a 400 is never grounds for killing live work", dc.stopped)
	}
}

// TestOverrideRefusal_NeverRecoversOnTheSleepPath is the other half of the same
// invariant. `0->1` fails open, so trading a run identity for a Model that can
// serve is right. `1->0` fails safe, so a refused SLEEP must be left alone:
// acting on it stops a live run, and the cost of NOT acting is only money.
func TestOverrideRefusal_NeverRecoversOnTheSleepPath(t *testing.T) {
	dc := callRecover(t, Action{Apply: true, Replicas: 0}, 1)
	if len(dc.stopped) != 0 {
		t.Fatalf("stopped %v on the sleep path: 1->0 must never act on an HTTP error", dc.stopped)
	}
}

// TestOverrideRefusal_SleepOnAnIdleRunStillDoesNothing pins the DIRECTION
// guard, which the serving guard alone does not cover.
//
// Found by mutation: dropping `action.Replicas > 0` left the suite green,
// because the only sleep case tested also had replicas > 0, so the serving
// guard decided it. Here nothing is serving, so stopping would be harmless --
// and it must still not happen. `1->0` does not act on an HTTP error, ever,
// not even when acting would cost nothing. The rule is the invariant, not a
// case-by-case judgement about how much damage a particular slip would do.
func TestOverrideRefusal_SleepOnAnIdleRunStillDoesNothing(t *testing.T) {
	dc := callRecover(t, Action{Apply: true, Replicas: 0}, 0)
	if len(dc.stopped) != 0 {
		t.Fatalf("stopped %v on a sleep flip: the 1->0 direction never recovers by destroying", dc.stopped)
	}
}

// TestOverrideRefusal_PersistsTheCondition is the assertion the first version of
// these tests was missing entirely: no-op the whole function body and everything
// still passed, because only the Stop call was pinned. What an operator actually
// needs is the condition to REACH the API server, so this re-reads it rather
// than trusting the in-memory object.
func TestOverrideRefusal_PersistsTheCondition(t *testing.T) {
	dc := callRecover(t, Action{Apply: true, Replicas: 1}, 0)
	cond := dc.persistedSchedulable(t)
	if cond == nil {
		t.Fatal("no Schedulable condition was persisted: the refusal is invisible to an operator")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != squallv1alpha1.ReasonCannotOverride {
		t.Fatalf("persisted condition = %s/%s, want False/%s",
			cond.Status, cond.Reason, squallv1alpha1.ReasonCannotOverride)
	}
}

// TestOverrideRefusal_PersistsUnderResourceVersionChurn pins LIVE-1 for this
// path specifically, and it is the assertion that separates Patch from Update.
//
// The refusal fires ONLY while a wake is demanded, which means only while
// squall-proxy is holding a request and re-stamping the demand annotation every
// 1-2s. So the object's resourceVersion is moving under us for the entire
// window in which this code can run. Status().Update is gated on that version
// and loses every time; once the caller gives up, demand expires, no further
// Apply is attempted and this never runs again -- so the condition would never
// be written at all. Here the stored object is advanced out of band first, so
// the in-memory copy is stale exactly as it would be in production.
func TestOverrideRefusal_PersistsUnderResourceVersionChurn(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	model := overrideFixtureModel(t, "churn")
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).WithObjects(model).Build()

	// Someone else writes first -- the proxy refreshing demand-since.
	var churn squallv1alpha1.Model
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(model), &churn); err != nil {
		t.Fatalf("get: %v", err)
	}
	churn.Annotations[squallv1alpha1.DemandAnnotation] = metav1.Now().Add(time.Second).UTC().Format(time.RFC3339)
	if err := c.Update(context.Background(), &churn); err != nil {
		t.Fatalf("churn update: %v", err)
	}

	// `model` is now stale, which is the whole point.
	r := &ModelReconciler{Client: c, DstackClient: &overrideRefusingClient{}}
	r.recoverFromOverrideRefusal(context.Background(), logr.Discard(), model, model.DeepCopy(),
		Action{Apply: true, Replicas: 1, Current: &dstack.Run{Name: "default-churn", Replicas: 0}},
		"default-churn", dstack.ErrCannotOverride)

	var stored squallv1alpha1.Model
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(model), &stored); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	cond := meta.FindStatusCondition(stored.Status.Conditions, squallv1alpha1.ConditionSchedulable)
	if cond == nil || cond.Reason != squallv1alpha1.ReasonCannotOverride {
		t.Fatalf("the refusal was lost to a concurrent write (cond=%v); it must be a Patch, "+
			"not an Update gated on resourceVersion", cond)
	}
}
