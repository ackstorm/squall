// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/clock"
	"github.com/ackstorm/squall/internal/dstack"
)

// forbiddenDstackClient fails the test the instant any method is called —
// used to prove the finalizer-add pass (Task 8.0, T10) performs no dstack
// actuation whatsoever, not merely that AddFinalizer happened to run first.
type forbiddenDstackClient struct {
	t *testing.T
}

var _ dstack.Client = (*forbiddenDstackClient)(nil)

func (f *forbiddenDstackClient) Apply(context.Context, dstack.ApplyRequest) (*dstack.Run, error) {
	f.t.Fatal("forbiddenDstackClient: unexpected Apply call")
	return nil, nil
}

func (f *forbiddenDstackClient) Stop(context.Context, string) error {
	f.t.Fatal("forbiddenDstackClient: unexpected Stop call")
	return nil
}

func (f *forbiddenDstackClient) Get(context.Context, string) (*dstack.Run, error) {
	f.t.Fatal("forbiddenDstackClient: unexpected Get call")
	return nil, nil
}

func (f *forbiddenDstackClient) Delete(context.Context, string) error {
	f.t.Fatal("forbiddenDstackClient: unexpected Delete call")
	return nil
}

func (f *forbiddenDstackClient) ListRuns(context.Context) ([]dstack.Run, error) {
	f.t.Fatal("forbiddenDstackClient: unexpected ListRuns call")
	return nil, nil
}

func (f *forbiddenDstackClient) BackendConfigured(context.Context, string) (bool, error) {
	f.t.Fatal("forbiddenDstackClient: unexpected BackendConfigured call")
	return false, nil
}

func (f *forbiddenDstackClient) HasFleetFor(context.Context, string) (bool, error) {
	f.t.Fatal("forbiddenDstackClient: unexpected HasFleetFor call")
	return false, nil
}

func (f *forbiddenDstackClient) EnsureFleet(context.Context, dstack.FleetSpec) error {
	f.t.Fatal("forbiddenDstackClient: unexpected EnsureFleet call")
	return nil
}

// TestReconcile_FirstPass_AddsFinalizerWithoutActuating is Task 8.0 (T10): a
// live, non-deleting Model with no finalizer yet must get ModelFinalizer
// added on the very first reconcile, and that pass alone must never also
// wake it — otherwise a Delete issued the instant afterward could race a
// finalizer that was never persisted, or (worse) an un-drained teardown.
// DstackClient is forbiddenDstackClient so any actuation this pass performs
// fails the test outright, not merely fails an assertion after the fact.
func TestReconcile_FirstPass_AddsFinalizerWithoutActuating(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const name = "qwen-8-0"
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: manualNamespace},
		Spec:       exampleModelSpec(),
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	r := &ModelReconciler{Client: k8sClient, DstackClient: &forbiddenDstackClient{t: t}}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}

	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile (finalizer-add pass): %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("result = %+v, want zero Result", res)
	}

	var got squallv1alpha1.Model
	if err := k8sClient.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatalf("get model: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&got, ModelFinalizer) {
		t.Fatal("finalizer not added on the first reconcile")
	}
	if got.Status.Phase != "" {
		t.Errorf("Status.Phase = %q, want unset — the finalizer-add pass must not also actuate", got.Status.Phase)
	}
}

// countingDstackClient wraps dstack.Client and counts Delete calls. The
// drain-first finalizer's replay-idempotence claim (T15, Task 8.1) is that
// this count never exceeds 1 no matter how many reconciles observe an
// already-terminal state.
type countingDstackClient struct {
	dstack.Client
	mu          sync.Mutex
	deleteCalls int
	stopCalls   int
}

// Stop is counted separately from Delete: D56's whole point is that the
// teardown must call BOTH, in that order.
func (c *countingDstackClient) Stop(ctx context.Context, name string) error {
	c.mu.Lock()
	c.stopCalls++
	c.mu.Unlock()
	return c.Client.Stop(ctx, name)
}

func (c *countingDstackClient) StopCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopCalls
}

func (c *countingDstackClient) Delete(ctx context.Context, name string) error {
	c.mu.Lock()
	c.deleteCalls++
	c.mu.Unlock()
	return c.Client.Delete(ctx, name)
}

func (c *countingDstackClient) DeleteCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deleteCalls
}

// TestReconcile_Delete_DrainFirstTeardown is Task 8.1 (T11, T12, T13): once
// deletion begins on a Model with a live run, status.phase must observably
// become Draining BEFORE any destructive call (T11); with no ProxyService
// configured, Activity evidence is always nil ("not evaluated"), so the
// in-flight run must not be cut while drainTimeout has not yet elapsed
// (T12); once a FakeClock advance carries `now` past the DeletionTimestamp
// + drainTimeout deadline, Delete must proceed anyway despite the still-
// unclean evidence (T13, the drain-vs-sleep asymmetry: drain's wait is
// bounded, sleep's is not). No test may use time.Sleep (internal/clock's
// doc comment) — FakeClock.Advance substitutes for a real wait.
func TestReconcile_Delete_DrainFirstTeardown(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const name = "qwen-8-1"
	spec := exampleModelSpec()
	// Comfortably clear of metav1.Time's 1-second truncation of
	// DeletionTimestamp, so "before the deadline" is unambiguous below.
	spec.DrainTimeout = metav1.Duration{Duration: 5 * time.Second}
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  manualNamespace,
			Finalizers: []string{ModelFinalizer}, // Task 8.0's add path is covered separately.
		},
		Spec: spec,
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	if _, err := dstackClient.Apply(ctx, dstack.ApplyRequest{Name: runNameIn(manualNamespace, name), Replicas: 1}); err != nil {
		t.Fatalf("seed dstack run: %v", err)
	}

	spy := &countingDstackClient{Client: dstackClient}
	fakeClock := clock.NewFakeClock(time.Now())
	r := &ModelReconciler{Client: k8sClient, DstackClient: spy, Clock: fakeClock}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}

	if err := k8sClient.Delete(ctx, model); err != nil {
		t.Fatalf("delete model: %v", err)
	}

	// Pass 1: well within drainTimeout. No ProxyService is configured, so
	// gatherActivity always returns nil and drainEvidenceClean is always
	// false — this must produce a bounded requeue, not a Delete (T12).
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile (drain pass 1): %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want > 0 (bounded drain wait)", res.RequeueAfter)
	}
	var draining squallv1alpha1.Model
	if err := k8sClient.Get(ctx, req.NamespacedName, &draining); err != nil {
		t.Fatalf("get model after pass 1: %v", err)
	}
	if draining.Status.Phase != squallv1alpha1.ModelPhaseDraining {
		t.Fatalf("Status.Phase = %q, want Draining observable before Delete (T11)", draining.Status.Phase)
	}
	if got := spy.DeleteCalls(); got != 0 {
		t.Fatalf("Delete calls = %d after pass 1, want 0 — drainTimeout has not elapsed", got)
	}
	if run, err := dstackClient.Get(ctx, runNameIn(manualNamespace, name)); err != nil || run.Replicas != 1 {
		t.Fatalf("dstack run after pass 1: run=%+v err=%v, want an untouched, live run", run, err)
	}

	// Advance the fake clock past the deadline. Evidence is still unclean
	// (still no proxy configured) — T13 says Delete must proceed anyway.
	fakeClock.Advance(spec.DrainTimeout.Duration + 5*time.Second)

	// Past the deadline the drain gate opens, but the run is still LIVE, so
	// this pass STOPS it rather than deleting it (D56: real dstack answers
	// 400 to a Delete on an active run). The Delete lands on the pass after,
	// once dstack reports the run terminal.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (drain pass 2, past deadline): %v", err)
	}
	if got := spy.StopCalls(); got != 1 {
		t.Fatalf("Stop calls = %d once the drain gate opened, want exactly 1", got)
	}
	if got := spy.DeleteCalls(); got != 0 {
		t.Fatalf("Delete calls = %d while the run was still active, want 0", got)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (delete pass): %v", err)
	}
	if got := spy.DeleteCalls(); got != 1 {
		t.Fatalf("Delete calls = %d after the run went terminal, want exactly 1", got)
	}
	if _, err := dstackClient.Get(ctx, runNameIn(manualNamespace, name)); !errors.Is(err, dstack.ErrNotFound) {
		t.Fatalf("dstack Get after teardown: err = %v, want ErrNotFound", err)
	}
	if err := k8sClient.Get(ctx, req.NamespacedName, &squallv1alpha1.Model{}); !apierrors.IsNotFound(err) {
		t.Fatalf("model Get after teardown: err = %v, want NotFound (finalizer removed)", err)
	}
}

// TestReconcile_Delete_ReplayAfterAlreadyGone_TreatsNotFoundAsSuccess is
// Task 8.1's T15: dstack.ErrNotFound from Get (nothing left to drain) and
// then from Delete (nothing left to delete) must both be read as "already
// torn down", not as a hard error — otherwise a replay after the run is
// already gone would wedge the finalizer forever. A Model that never woke
// reproduces this shape without needing an actual prior successful Delete:
// dstack.Get already answers ErrNotFound on the very first drain pass.
func TestReconcile_Delete_ReplayAfterAlreadyGone_TreatsNotFoundAsSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const name = "qwen-8-1-replay"
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  manualNamespace,
			Finalizers: []string{ModelFinalizer},
		},
		Spec: exampleModelSpec(),
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create Model: %v", err)
	}
	if err := k8sClient.Delete(ctx, model); err != nil {
		t.Fatalf("delete model: %v", err)
	}

	r := &ModelReconciler{Client: k8sClient, DstackClient: dstackClient}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (delete, nothing to drain): %v", err)
	}

	if err := k8sClient.Get(ctx, req.NamespacedName, &squallv1alpha1.Model{}); !apierrors.IsNotFound(err) {
		t.Fatalf("model Get after teardown: err = %v, want NotFound — ErrNotFound from Get and Delete must both be treated as success", err)
	}
}

// TestReconcile_Delete_UnreadableProxyEndpoints_NoDestructiveCalls is Task
// 8.2 (T14): an unreadable API server for the proxy's Endpoints must never
// be read as "nothing observed, safe to proceed" — it must produce
// incomplete (never vacuously clean) drain evidence, so the finalizer keeps
// requeuing instead of deleting, across every reconcile the injected error
// persists for. Uses client/interceptor against the real envtest apiserver,
// rather than tearing down the shared testEnv, per the block 7+8 plan §5.
func TestReconcile_Delete_UnreadableProxyEndpoints_NoDestructiveCalls(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add squall scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}
	watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("new watch client: %v", err)
	}

	wantErr := errors.New("api server unreachable")
	intercepted := interceptor.NewClient(watchClient, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*discoveryv1.EndpointSliceList); ok {
				return wantErr
			}
			return c.List(ctx, list, opts...)
		},
	})

	const name = "qwen-8-2"
	spec := exampleModelSpec()
	spec.DrainTimeout = metav1.Duration{Duration: 5 * time.Second}
	proxyKey := types.NamespacedName{Namespace: manualNamespace, Name: "squall-proxy-8-2"}
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
	if _, err := dstackClient.Apply(ctx, dstack.ApplyRequest{Name: runNameIn(manualNamespace, name), Replicas: 1}); err != nil {
		t.Fatalf("seed dstack run: %v", err)
	}
	if err := k8sClient.Delete(ctx, model); err != nil {
		t.Fatalf("delete model: %v", err)
	}

	spy := &countingDstackClient{Client: dstackClient}
	r := &ModelReconciler{Client: intercepted, DstackClient: spy, ProxyService: proxyKey}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}

	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile (unreadable proxy Endpoints, pass %d): %v", i, err)
		}
	}

	if got := spy.DeleteCalls(); got != 0 {
		t.Fatalf("Delete calls = %d across 3 reconciles with an unreadable proxy, want 0 (T14: fail closed)", got)
	}
	if run, err := dstackClient.Get(ctx, runNameIn(manualNamespace, name)); err != nil || run.Replicas != 1 {
		t.Fatalf("dstack run: run = %+v, err = %v, want an untouched, live run", run, err)
	}
	if err := k8sClient.Get(ctx, req.NamespacedName, &squallv1alpha1.Model{}); err != nil {
		t.Fatalf("model Get: %v, want it to still exist (finalizer still present)", err)
	}
}

// TestReconcile_Delete_StopsBeforeDeletingAnActiveRun is D56, the billing
// leak, measured on Vast.ai before it was fixed.
//
// Real dstack refuses to delete a run that is not already terminal — HTTP
// 400 "Cannot delete active runs". The teardown used to call Delete
// directly, so the reconcile failed, retried, failed again, and the CR sat
// in Draining with its finalizer while the rented GPU billed indefinitely.
//
// Every existing teardown test deletes a Model whose run is already
// terminal, which is exactly why none of them saw it. This one deletes a
// Model whose run is LIVE, and asserts the two things that matter: Stop is
// called, and Delete is not called until the run has actually gone
// terminal.
func TestReconcile_Delete_StopsBeforeDeletingAnActiveRun(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const name = "d56-active-run"
	spec := exampleModelSpec()
	// Minimal drain timeout: this test is about stop-before-delete, not
	// about the drain gate, so the deadline must never be what holds it
	// up. A nanosecond (not zero — D123 folds zero to the 120s default,
	// exactly so a delete can never mean zero drain) is past by the time
	// the first reconcile runs.
	spec.DrainTimeout = metav1.Duration{Duration: time.Nanosecond}
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
	if _, err := dstackClient.Apply(ctx, dstack.ApplyRequest{Name: runNameIn(manualNamespace, name), Replicas: 1}); err != nil {
		t.Fatalf("seed a LIVE dstack run: %v", err)
	}

	spy := &countingDstackClient{Client: dstackClient}
	r := &ModelReconciler{Client: k8sClient, DstackClient: spy}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}

	if err := k8sClient.Delete(ctx, model); err != nil {
		t.Fatalf("delete model: %v", err)
	}

	// Pass 1: the run is live. Stop must be called, Delete must NOT — a
	// Delete here is the 400 that produced the leak.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (stop pass): %v", err)
	}
	if got := spy.StopCalls(); got != 1 {
		t.Fatalf("Stop calls = %d after the first pass, want 1 — an active run must be stopped before it can be deleted", got)
	}
	if got := spy.DeleteCalls(); got != 0 {
		t.Fatalf("Delete calls = %d while the run was still active, want 0 — real dstack answers 400 and the instance keeps billing", got)
	}

	// The finalizer must still be there: nothing may drop our only handle
	// on a machine that is still running.
	var draining squallv1alpha1.Model
	if err := k8sClient.Get(ctx, req.NamespacedName, &draining); err != nil {
		t.Fatalf("the CR is gone while its run was still live: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&draining, ModelFinalizer) {
		t.Fatal("finalizer removed while the run was still active — the leak this test exists to prevent")
	}

	// Pass 2: the run is terminal now (the fake's Stop terminated it), so
	// the Delete may proceed and the finalizer may go.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (delete pass): %v", err)
	}
	if got := spy.DeleteCalls(); got != 1 {
		t.Fatalf("Delete calls = %d once the run was terminal, want 1", got)
	}
	err := k8sClient.Get(ctx, req.NamespacedName, &squallv1alpha1.Model{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Model still present after teardown completed: err = %v", err)
	}
}
