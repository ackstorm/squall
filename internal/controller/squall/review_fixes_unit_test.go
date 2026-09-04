// SPDX-License-Identifier: MIT

// Regression tests for the 2026-08-31 whole-branch review's controller-side
// findings (ledger D104, D110, D112, D115). Pure unit tests: fake client,
// stub dstack, real HTTP only for the in-process activity servers — no
// control plane, so all of this runs under test-unit with no skip.
package squall

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

// notReadyEndpointSlicesFor is endpointSlicesForServers' D104 sibling: each
// entry carries serving=true and ready=false — a replica whose readiness probe
// is failing, or that is draining through srv.Shutdown, but whose process
// (and in-flight generations) may be very much alive.
func notReadyEndpointSlicesFor(t *testing.T, readyAddrPorts, notReadyAddrPorts []string) []client.Object {
	t.Helper()
	slices := make([]client.Object, 0, len(readyAddrPorts)+len(notReadyAddrPorts))
	for _, hp := range readyAddrPorts {
		slices = append(slices, endpointSliceFor(t, hp))
	}
	for _, hp := range notReadyAddrPorts {
		slice := endpointSliceFor(t, hp)
		ready, serving := false, true
		slice.Endpoints[0].Conditions = discoveryv1.EndpointConditions{Ready: &ready, Serving: &serving}
		slices = append(slices, slice)
	}
	return slices
}

// TestGatherActivity_NotReadyReplicaWithInFlightWork_IsSeen is D104's core
// case: a proxy replica whose readiness probe dipped under load moves to
// NotReadyAddresses while still HOLDING live generations. Excluding it from
// the expected set made those generations invisible — the ready survivor
// reports idle, sleepDue fires, and replicas flip to 0 under streaming
// work, the exact `1->0 fails safe` violation. Mutation check: reverting
// gatherActivity to iterate subset.Addresses only turns this red via
// AllIdle (the busy replica simply vanishes from the evidence).
func TestGatherActivity_NotReadyReplicaWithInFlightWork_IsSeen(t *testing.T) {
	now := time.Now().UTC()
	idle := activityServer(t, squallv1alpha1.ActivityReport{
		Models: map[string]squallv1alpha1.ModelActivity{
			"qwen": {InFlight: 0, LastRequestAt: now.Add(-time.Hour)},
		},
	})
	busyNotReady := activityServer(t, squallv1alpha1.ActivityReport{
		Models: map[string]squallv1alpha1.ModelActivity{
			"qwen": {InFlight: 40, LastRequestAt: now},
		},
	})

	endpoints := notReadyEndpointSlicesFor(t,
		[]string{idle.Listener.Addr().String()},
		[]string{busyNotReady.Listener.Addr().String()},
	)
	r := &ModelReconciler{
		Client:       newFakeClient(t, endpoints...),
		ProxyService: types.NamespacedName{Namespace: testProxyNamespace, Name: testProxyService},
	}

	got := r.gatherActivity(context.Background(), "qwen", now.Add(time.Hour))
	if got == nil || !got.Complete {
		t.Fatalf("gatherActivity = %+v, want Complete: true — the not-ready replica ANSWERED", got)
	}
	if got.AllIdle {
		t.Fatal("AllIdle = true with 40 generations streaming on the not-ready replica; sleep would kill them")
	}
}

// TestGatherActivity_UnreachableNotReadyReplica_BlocksCompleteness is
// D104's fail-safe half: a not-ready replica that refuses the query might
// be crashlooping (no work) or draining through srv.Shutdown (work very
// much in flight) — the two are indistinguishable at the socket, so the
// evidence must read INCOMPLETE, never idle. The cost (a crashlooping
// proxy pod blocks sleep while it lingers) is a visible bill, which the
// invariant prefers over an invisible kill.
func TestGatherActivity_UnreachableNotReadyReplica_BlocksCompleteness(t *testing.T) {
	now := time.Now().UTC()
	idle := activityServer(t, squallv1alpha1.ActivityReport{
		Models: map[string]squallv1alpha1.ModelActivity{
			"qwen": {InFlight: 0, LastRequestAt: now.Add(-time.Hour)},
		},
	})
	// A port with nothing listening: bind, capture, close.
	dead := activityServer(t, squallv1alpha1.ActivityReport{})
	deadAddr := dead.Listener.Addr().String()
	dead.Close()

	endpoints := notReadyEndpointSlicesFor(t,
		[]string{idle.Listener.Addr().String()},
		[]string{deadAddr},
	)
	r := &ModelReconciler{
		Client:       newFakeClient(t, endpoints...),
		ProxyService: types.NamespacedName{Namespace: testProxyNamespace, Name: testProxyService},
	}

	got := r.gatherActivity(context.Background(), "qwen", now.Add(time.Hour))
	if got == nil {
		t.Fatal("gatherActivity = nil, want non-nil incomplete evidence")
	}
	if got.Complete {
		t.Fatal("Complete = true with an unreachable not-ready replica; a drain would proceed on absent evidence")
	}
}

// applyRecordingDstackClient answers Get with a fixed live run and records
// what Apply was asked to do — the seam D115's assertions read.
type applyRecordingDstackClient struct {
	run     *dstack.Run
	applied *dstack.ApplyRequest
}

var _ dstack.Client = (*applyRecordingDstackClient)(nil)

type errorOnSecretClient struct {
	client.Client
	err error
}

func (c *errorOnSecretClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		return c.err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestApplyEnvFor_WakeKeepsCurrentKeyWhenEnsureSSHKeyFails(t *testing.T) {
	model := &squallv1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "qwen", Namespace: "default"}}
	model.Spec.Env = map[string]string{"FRESH": "resolved"}
	current := &dstack.Run{SSHKeyPub: "ssh-ed25519 AAAA-current", Env: map[string]string{"OLD": "stale"}}
	r := &ModelReconciler{
		Client:       &errorOnSecretClient{Client: newFakeClient(t), err: errors.New("secret API unavailable")},
		ProxyService: types.NamespacedName{Namespace: testProxyNamespace, Name: testProxyService},
	}

	gotEnv, gotKey, err := r.applyEnvFor(context.Background(), model, Action{Replicas: 1, Current: current})
	if err != nil {
		t.Fatalf("applyEnvFor: %v", err)
	}
	if gotKey != current.SSHKeyPub {
		t.Fatalf("SSH key = %q, want current run key %q", gotKey, current.SSHKeyPub)
	}
	if gotEnv["FRESH"] != "resolved" || gotEnv["OLD"] != "" {
		t.Fatalf("env = %#v, want freshly resolved env, not current run env", gotEnv)
	}
}

func (a *applyRecordingDstackClient) Get(context.Context, string) (*dstack.Run, error) {
	return a.run, nil
}

func (a *applyRecordingDstackClient) Apply(_ context.Context, req dstack.ApplyRequest) (*dstack.Run, error) {
	a.applied = &req
	return a.run, nil
}

func (a *applyRecordingDstackClient) Stop(context.Context, string) error {
	return errors.New("applyRecordingDstackClient: unexpected Stop call")
}

func (a *applyRecordingDstackClient) Delete(context.Context, string) error {
	return errors.New("applyRecordingDstackClient: unexpected Delete call")
}

func (a *applyRecordingDstackClient) ListRuns(context.Context) ([]dstack.Run, error) {
	return nil, errors.New("applyRecordingDstackClient: unexpected ListRuns call")
}

func (a *applyRecordingDstackClient) BackendConfigured(context.Context, string) (bool, error) {
	return true, nil
}

func (a *applyRecordingDstackClient) HasFleetFor(context.Context, string) (bool, error) {
	return true, nil
}

func (a *applyRecordingDstackClient) EnsureFleet(context.Context, dstack.FleetSpec) error {
	return errors.New("applyRecordingDstackClient: unexpected EnsureFleet call")
}

// TestReconcile_SleepFlip_DoesNotNeedTheSecret is D115. A Model with
// spec.secretEnv whose referenced Secret has been rotated away is idle and
// due to sleep; the 1->0 flip needs no credential, so it must actuate —
// before the fix, resolveEnv sat on the single shared Apply call site and
// its error pinned the GPU awake forever ("the GPU bills indefinitely
// because a credential the sleep flip does not need could not be read").
// The flip re-sends the run's CURRENT env verbatim, so the applied config
// is byte-identical and the D102 ownership stamp survives.
func TestReconcile_SleepFlip_DoesNotNeedTheSecret(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}

	now := time.Now().UTC()
	spec := exampleModelSpec()
	spec.SecretEnv = map[string]squallv1alpha1.SecretKeyRef{
		"HF_TOKEN": {Name: "rotated-away", Key: "token"},
	}
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "qwen-sleep",
			Namespace:  "default",
			Finalizers: []string{ModelFinalizer},
		},
		Spec:   spec,
		Status: squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"},
	}

	// Clean, complete, aged idle evidence: one proxy replica, idle, last
	// request an hour ago — far past the 300s scaleDownDelaySeconds.
	idle := activityServer(t, squallv1alpha1.ActivityReport{
		Models: map[string]squallv1alpha1.ModelActivity{
			model.Name: {InFlight: 0, LastRequestAt: now.Add(-time.Hour)},
		},
	})
	endpoints := endpointSliceFor(t, idle.Listener.Addr().String())

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model, endpoints).
		Build()

	runEnv := map[string]string{"OLLAMA_FLASH_ATTENTION": "1", ModelUIDEnvKey: "prior-uid"}
	dc := &applyRecordingDstackClient{run: &dstack.Run{
		Name: "default-qwen-sleep", RunID: "run-1", Replicas: 1, ProbesReady: true,
		Env:       runEnv,
		SSHKeyPub: "ssh-ed25519 AAAA-the-runs-own-key",
	}}

	r := &ModelReconciler{
		Client:       fakeClient,
		DstackClient: dc,
		ProxyService: types.NamespacedName{Namespace: testProxyNamespace, Name: testProxyService},
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(model),
	}); err != nil {
		t.Fatalf("Reconcile error = %v, want nil — a missing Secret must never block the sleep flip (D115)", err)
	}
	if dc.applied == nil {
		t.Fatal("no Apply recorded: the sleep flip never actuated")
	}
	if dc.applied.Replicas != 0 {
		t.Fatalf("Apply.Replicas = %d, want 0 (the sleep flip)", dc.applied.Replicas)
	}
	if dc.applied.Env[ModelUIDEnvKey] != "prior-uid" {
		t.Fatalf("Apply.Env = %v, want the run's CURRENT env verbatim — the flip must not rewrite configuration it could not resolve", dc.applied.Env)
	}
	// D115 addendum, MEASURED LIVE 2026-08-31: dstack refuses ANY spec
	// difference on an active run beyond replicas ("Cannot override active
	// run"), and the ssh key is part of the spec. A sleep that sent "" here
	// was rejected for two hours while the GPU billed.
	if dc.applied.SSHKeyPub != "ssh-ed25519 AAAA-the-runs-own-key" {
		t.Fatalf("Apply.SSHKeyPub = %q, want the run's OWN key verbatim — anything else makes the sleep flip an 'override' dstack refuses", dc.applied.SSHKeyPub)
	}
}

// TestReconcile_FreshRun_ForgetsForwardModel is D112. The Current == nil
// mint-a-fresh-run block forgets status.servedModel and the verification
// condition, but left status.forwardModel — the ONE field the proxy
// actually rewrites request bodies to. A reclaimed run's id surviving
// there makes the proxy rewrite every request to a name the new run may
// not serve, and each engine 400 is charged to the healthy replica.
func TestReconcile_FreshRun_ForgetsForwardModel(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "qwen-forward",
			Namespace:  "default",
			Finalizers: []string{ModelFinalizer},
		},
		Spec: exampleModelSpec(),
		Status: squallv1alpha1.ModelStatus{
			Phase:        squallv1alpha1.ModelPhaseReady,
			RunID:        "dead-run",
			ServedModel:  "stale:latest,qwen2.5:0.5b",
			ForwardModel: "stale:latest",
		},
	}
	// Demand keeps the recreate warranted; without it Dead+no-demand
	// alarms without recreating (F20) and no fresh run is minted.
	model.Annotations = map[string]string{
		squallv1alpha1.DemandAnnotation: time.Now().UTC().Format(time.RFC3339),
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model).
		Build()

	r := &ModelReconciler{
		Client:       fakeClient,
		DstackClient: &deadRecreateDstackClient{newRun: &dstack.Run{Name: "default-qwen-forward", RunID: "fresh-run", Replicas: 1}},
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(model),
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got squallv1alpha1.Model
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(model), &got); err != nil {
		t.Fatalf("Get after Reconcile: %v", err)
	}
	if got.Status.ForwardModel != "" {
		t.Fatalf("status.forwardModel = %q after a fresh run was minted, want empty — the proxy would rewrite bodies to a dead run's id", got.Status.ForwardModel)
	}
	if got.Status.ServedModel != "" {
		t.Fatalf("status.servedModel = %q, want empty (pre-existing behaviour this test also pins)", got.Status.ServedModel)
	}
}

// raceOnModelGetClient wraps the fake client and, immediately after the
// FIRST successful Get of a Model, writes a demand-annotation metadata
// patch through the inner client — squall-proxy's concurrent write, landed
// in the exact window between Reconcile's read and reconcileDelete's
// Draining status write (D110).
type raceOnModelGetClient struct {
	client.Client
	raced bool
}

func (c *raceOnModelGetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if err != nil {
		return err
	}
	if _, isModel := obj.(*squallv1alpha1.Model); isModel && !c.raced {
		c.raced = true
		var live squallv1alpha1.Model
		if err := c.Client.Get(ctx, key, &live); err != nil {
			return err
		}
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations[squallv1alpha1.DemandAnnotation] = time.Now().UTC().Format(time.RFC3339)
		if err := c.Client.Update(ctx, &live); err != nil {
			return err
		}
	}
	return err
}

// TestReconcileDelete_ConcurrentMetadataWrite_DrainingStillPersists is
// D110: reconcileDelete wrote Draining with Status().Update — the same
// whole-object optimistic lock D76/LIVE-1 measured losing on EVERY attempt
// while a request was held. Worse than D76, the starvation here is
// self-sustaining: the proxy only stops signalling demand once it SEES
// Draining, and Draining is precisely the write being starved, so the GPU
// bills for as long as traffic arrives. The fix (Status().Patch with
// MergeFrom) must land Draining despite the interleaved metadata write.
func TestReconcileDelete_ConcurrentMetadataWrite_DrainingStillPersists(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	spec := exampleModelSpec()
	spec.DrainTimeout = metav1.Duration{Duration: 20 * time.Minute}
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "qwen-drain-race",
			Namespace:  "default",
			Finalizers: []string{ModelFinalizer},
		},
		Spec:   spec,
		Status: squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"},
	}

	inner := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model).
		Build()
	racey := &raceOnModelGetClient{Client: inner}

	// Delete sets deletionTimestamp; the finalizer keeps the object.
	if err := inner.Delete(context.Background(), model); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// A live, non-terminal run with no ProxyService configured: the drain
	// gate sees no clean evidence and requeues, so this pass performs the
	// Draining write and NOTHING destructive — isolating exactly the write
	// under test.
	dc := &applyRecordingDstackClient{run: &dstack.Run{
		Name: "default-qwen-drain-race", RunID: "run-1", Replicas: 1, Status: "running",
	}}

	r := &ModelReconciler{Client: racey, DstackClient: dc}
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(model),
	})
	if err != nil {
		t.Fatalf("Reconcile error = %v, want nil — a concurrent metadata write must never starve the Draining write (D110)", err)
	}
	if !racey.raced {
		t.Fatal("test setup bug: the simulated concurrent metadata write never fired")
	}
	if res.RequeueAfter == 0 {
		t.Fatal("want a drain-gate requeue; the fixture was meant to stop before anything destructive")
	}

	var got squallv1alpha1.Model
	if err := inner.Get(context.Background(), client.ObjectKeyFromObject(model), &got); err != nil {
		t.Fatalf("Get after Reconcile: %v", err)
	}
	if got.Status.Phase != squallv1alpha1.ModelPhaseDraining {
		t.Fatalf("status.Phase = %v, want Draining — the write the whole teardown gates on", got.Status.Phase)
	}
	if got.Annotations[squallv1alpha1.DemandAnnotation] == "" {
		t.Fatal("test setup bug: the concurrent metadata mutation itself did not persist")
	}
}

// TestReconcileDelete_ExplicitZeroDrainTimeout_StillDrains is D123's
// typed-client half: metav1.Duration's omitempty never drops the struct,
// so a typed create (or an explicit `drainTimeout: "0s"`) reaches the
// finalizer with a PRESENT zero that admission defaulting rightly left
// alone. Zero must fold to the same 120s default, not to "past the
// deadline on the first pass" — which cut every in-flight generation the
// moment a Model was deleted. Mutation check: reverting pastDeadline to
// read spec.DrainTimeout.Duration directly turns this red via the Stop
// call (the stub errors on it).
func TestReconcileDelete_ExplicitZeroDrainTimeout_StillDrains(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	spec := exampleModelSpec()
	spec.DrainTimeout = metav1.Duration{} // the explicit zero.
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "qwen-zero-drain",
			Namespace:  "default",
			Finalizers: []string{ModelFinalizer},
		},
		Spec:   spec,
		Status: squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model).
		Build()
	if err := fakeClient.Delete(context.Background(), model); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// A live run and no proxy evidence: with the 120s fallback the drain
	// gate must requeue; with a zero deadline it would go straight to Stop
	// — which this stub turns into a test failure.
	dc := &applyRecordingDstackClient{run: &dstack.Run{
		Name: "default-qwen-zero-drain", RunID: "run-1", Replicas: 1, Status: "running",
	}}
	r := &ModelReconciler{Client: fakeClient, DstackClient: dc}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(model),
	})
	if err != nil {
		t.Fatalf("Reconcile error = %v, want a drain-gate requeue — an explicit 0s drainTimeout must not mean zero drain (D123)", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("no requeue: the finalizer went past the drain gate on a zero deadline")
	}
}
