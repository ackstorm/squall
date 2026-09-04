// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

func TestUpdateActivityStatus_LastRequestAtNeverGoesBackwards(t *testing.T) {
	old := metav1.NewTime(time.Unix(200, 0))
	model := &squallv1alpha1.Model{Status: squallv1alpha1.ModelStatus{LastRequestAt: &old}}
	updateActivityStatus(model, Observed{Activity: &ActivityEvidence{
		Complete: true, AnyData: true, NewestLastRequestAt: time.Unix(100, 0),
	}}, false, time.Unix(300, 0))
	if !model.Status.LastRequestAt.Equal(&old) {
		t.Fatalf("last request moved backwards: got %v want %v", model.Status.LastRequestAt, old)
	}
}

func TestApplyDurationsFor_DirectionAndHardStop(t *testing.T) {
	onDemand := &squallv1alpha1.Model{Spec: squallv1alpha1.ModelSpec{MinReplicas: 0, HardStop: metav1.Duration{Duration: 24 * time.Hour}, Fleet: squallv1alpha1.ModelFleet{IdleDuration: metav1.Duration{Duration: 10 * time.Minute}}}}
	pinned := &squallv1alpha1.Model{Spec: squallv1alpha1.ModelSpec{MinReplicas: 1, HardStop: metav1.Duration{Duration: 24 * time.Hour}, Fleet: squallv1alpha1.ModelFleet{IdleDuration: metav1.Duration{Duration: 10 * time.Minute}}}}
	if idle, hard := applyDurationsFor(onDemand, Action{Replicas: 0, Current: &dstack.Run{Replicas: 1}}); idle != 0 || hard != 0 {
		t.Fatalf("pre-upgrade sleep = %v/%v", idle, hard)
	}
	if idle, hard := applyDurationsFor(onDemand, Action{Replicas: 0, Current: &dstack.Run{Replicas: 1, IdleDuration: 5 * time.Minute, MaxDuration: 12 * time.Hour}}); idle != 5*time.Minute || hard != 12*time.Hour {
		t.Fatalf("stored sleep = %v/%v", idle, hard)
	}
	if idle, hard := applyDurationsFor(onDemand, Action{Replicas: 1, Current: &dstack.Run{Replicas: 0}}); idle != 10*time.Minute || hard != 24*time.Hour {
		t.Fatalf("wake = %v/%v", idle, hard)
	}
	if _, hard := applyDurationsFor(pinned, Action{Replicas: 1}); hard != 0 {
		t.Fatalf("pinned hard = %v", hard)
	}
}

func TestReportProvisioningCondition_MaxDurationIsAHardStop(t *testing.T) {
	model := &squallv1alpha1.Model{}
	reportProvisioningCondition(model, &dstack.ProvisioningFailure{RunID: "r1", Reason: "max_duration_exceeded", Message: "max duration exceeded"}, squallv1alpha1.ModelPhaseDead)
	for _, c := range model.Status.Conditions {
		if c.Type == squallv1alpha1.ConditionProvisioning && c.Reason == squallv1alpha1.ReasonHardStopFired {
			return
		}
	}
	t.Fatal("max_duration must classify as HardStopFired")
}

func TestUpdateActivityStatus_WakeSeedsAnchor(t *testing.T) {
	woke := metav1.NewTime(time.Unix(100, 0))
	model := &squallv1alpha1.Model{Status: squallv1alpha1.ModelStatus{WakeStartedAt: &woke}}

	updateActivityStatus(model, Observed{Run: &dstack.Run{Replicas: 1}, Activity: &ActivityEvidence{
		Complete: true, AllIdle: true,
	}}, false, time.Unix(200, 0))

	if model.Status.LastRequestAt == nil || !model.Status.LastRequestAt.Equal(&woke) {
		t.Fatalf("LastRequestAt = %v, want wake anchor %v", model.Status.LastRequestAt, woke)
	}
}

func TestUpdateActivityStatus_RealRequestReplacesSeededAnchor(t *testing.T) {
	woke := metav1.NewTime(time.Unix(100, 0))
	requestAt := time.Unix(200, 0)
	model := &squallv1alpha1.Model{Status: squallv1alpha1.ModelStatus{WakeStartedAt: &woke}}

	updateActivityStatus(model, Observed{Run: &dstack.Run{Replicas: 1}, Activity: &ActivityEvidence{
		Complete: true, AnyData: true, AllIdle: true, NewestLastRequestAt: requestAt,
	}}, false, requestAt.Add(time.Minute))

	if model.Status.LastRequestAt == nil || !model.Status.LastRequestAt.Time.Equal(requestAt) {
		t.Fatalf("LastRequestAt = %v, want genuine request anchor %v", model.Status.LastRequestAt, requestAt)
	}
}

func TestUpdateActivityStatus_FreshDemandResetsStatusButPreservesMetricAnchor(t *testing.T) {
	now := time.Unix(300, 0)
	since := metav1.NewTime(now.Add(-time.Hour))
	model := &squallv1alpha1.Model{Status: squallv1alpha1.ModelStatus{UncontrolledSince: &since}}
	metricSince := updateActivityStatus(model, Observed{Run: &dstack.Run{Replicas: 1}}, true, now)
	if model.Status.UncontrolledSince != nil {
		t.Fatal("fresh demand must reset the uncontrolled clock")
	}
	if metricSince == nil || !metricSince.Equal(&since) {
		t.Fatalf("metric anchor = %v, want original uncontrolled timestamp %v", metricSince, since)
	}
}

func TestUpdateActivityStatus_UncontrolledHandshake(t *testing.T) {
	now := time.Unix(300, 0)
	model := &squallv1alpha1.Model{Status: squallv1alpha1.ModelStatus{}}
	updateActivityStatus(model, Observed{Run: &dstack.Run{Replicas: 1}}, false, now)
	if model.Status.UncontrolledSince == nil || !model.Status.UncontrolledSince.Equal(&metav1.Time{Time: now}) {
		t.Fatal("incomplete evidence must start the stamp")
	}
	updateActivityStatus(model, Observed{Run: &dstack.Run{Replicas: 1}, Activity: &ActivityEvidence{Complete: true}}, false, now.Add(time.Minute))
	if model.Status.UncontrolledSince != nil {
		t.Fatal("complete evidence must clear the stamp")
	}
	updateActivityStatus(model, Observed{Run: &dstack.Run{Replicas: 1}}, false, now.Add(2*time.Minute))
	if !model.Status.UncontrolledSince.Equal(&metav1.Time{Time: now.Add(2 * time.Minute)}) {
		t.Fatal("incomplete evidence must restart the stamp")
	}
}

// These tests are pure (an in-memory fake client.Client, no envtest control
// plane) and belong in `test-unit`, unlike model_controller_test.go's
// envtest-backed cases.

// erroringDstackClient is a stub dstack.Client whose Get always returns a
// fixed, non-sentinel error — simulating dstack being unreachable or timing
// out, as opposed to answering a clean 404 (dstack.ErrNotFound). Every other
// method fails the test if called: a Get error must short-circuit Reconcile
// well before any of them would be reached.
type erroringDstackClient struct {
	getErr error
}

var _ dstack.Client = (*erroringDstackClient)(nil)

func (e *erroringDstackClient) Apply(context.Context, dstack.ApplyRequest) (*dstack.Run, error) {
	return nil, errors.New("erroringDstackClient: unexpected Apply call")
}

func (e *erroringDstackClient) Stop(context.Context, string) error {
	return errors.New("erroringDstackClient: unexpected Stop call")
}

func (e *erroringDstackClient) Get(context.Context, string) (*dstack.Run, error) {
	return nil, e.getErr
}

// fixedRunDstackClient is a stub dstack.Client whose Get always answers a
// fixed, non-nil Run — simulating dstack reporting a run already up and
// idle, with nothing this pass needs to Apply. Every other method fails the
// test if called.
type fixedRunDstackClient struct {
	run *dstack.Run
}

var _ dstack.Client = (*fixedRunDstackClient)(nil)

func (f *fixedRunDstackClient) Apply(context.Context, dstack.ApplyRequest) (*dstack.Run, error) {
	return nil, errors.New("fixedRunDstackClient: unexpected Apply call")
}

func (f *fixedRunDstackClient) Stop(context.Context, string) error {
	return errors.New("fixedRunDstackClient: unexpected Stop call")
}

func (f *fixedRunDstackClient) Get(context.Context, string) (*dstack.Run, error) {
	return f.run, nil
}

func (f *fixedRunDstackClient) Delete(context.Context, string) error {
	return errors.New("fixedRunDstackClient: unexpected Delete call")
}

func (f *fixedRunDstackClient) ListRuns(context.Context) ([]dstack.Run, error) {
	return nil, errors.New("fixedRunDstackClient: unexpected ListRuns call")
}

// BackendConfigured and HasFleetFor are unreachable here too: every fixture
// using this stub keeps observed.Run.Replicas > 0 with no sleep-due
// evidence, so Decide never sets action.Apply and preflight is never
// called.
func (f *fixedRunDstackClient) BackendConfigured(context.Context, string) (bool, error) {
	return false, errors.New("fixedRunDstackClient: unexpected BackendConfigured call")
}

func (f *fixedRunDstackClient) HasFleetFor(context.Context, string) (bool, error) {
	return false, errors.New("fixedRunDstackClient: unexpected HasFleetFor call")
}

func (f *fixedRunDstackClient) EnsureFleet(context.Context, dstack.FleetSpec) error {
	return errors.New("fixedRunDstackClient: unexpected EnsureFleet call")
}

func (e *erroringDstackClient) Delete(context.Context, string) error {
	return errors.New("erroringDstackClient: unexpected Delete call")
}

func (e *erroringDstackClient) ListRuns(context.Context) ([]dstack.Run, error) {
	return nil, errors.New("erroringDstackClient: unexpected ListRuns call")
}

// BackendConfigured and HasFleetFor are unreachable here too: Get's error
// short-circuits Reconcile in observe() before Decide ever runs, so
// preflight (gated on action.Apply) is never called.
func (e *erroringDstackClient) BackendConfigured(context.Context, string) (bool, error) {
	return false, errors.New("erroringDstackClient: unexpected BackendConfigured call")
}

func (e *erroringDstackClient) HasFleetFor(context.Context, string) (bool, error) {
	return false, errors.New("erroringDstackClient: unexpected HasFleetFor call")
}

func (e *erroringDstackClient) EnsureFleet(context.Context, dstack.FleetSpec) error {
	return errors.New("erroringDstackClient: unexpected EnsureFleet call")
}

// statusSpyClient wraps a client.Client and records whether Status().Update
// was ever invoked, so a test can assert "never wrote status" directly
// instead of inferring it from the CR's final state (which cannot tell
// "never wrote" from "wrote the same value twice").
type statusSpyClient struct {
	client.Client
	updateCalled bool
}

func (s *statusSpyClient) Status() client.SubResourceWriter {
	return &statusSpyWriter{SubResourceWriter: s.Client.Status(), spy: s}
}

type statusSpyWriter struct {
	client.SubResourceWriter
	spy *statusSpyClient
}

func (w *statusSpyWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.spy.updateCalled = true
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// TestObserve_GenericDstackError_ReturnsErrorUnchanged is FIX 2 (wake path's
// half of the project's central invariant, phase.go's doc comment and
// internal/dstack/CLAUDE.md: "a transport error must never be mistaken for
// ErrNotFound"). observe's generic-error branch must propagate the error,
// not fold it into Observed{} the way dstack.ErrNotFound legitimately is.
//
// The mutation this guards against: changing observe's `case err != nil:`
// branch from `return Observed{}, err` to `return Observed{}, nil` would
// silently turn "dstack timed out" into "dstack says no run exists" —
// exactly the misread that mints a duplicate run under demand (F20, AC13).
func TestObserve_GenericDstackError_ReturnsErrorUnchanged(t *testing.T) {
	wantErr := errors.New("boom")
	r := &ModelReconciler{DstackClient: &erroringDstackClient{getErr: wantErr}}

	observed, err := r.observe(context.Background(), "squall-some-model", "some-model", squallv1alpha1.ModelSpec{}, time.Now())

	if !errors.Is(err, wantErr) {
		t.Fatalf("observe error = %v, want %v unchanged", err, wantErr)
	}
	if observed != (Observed{}) {
		t.Errorf("observed = %+v, want zero value", observed)
	}
}

// TestReconcile_UncertainDstackGetError_ReturnsErrorWithoutStatusWrite is
// FIX 2's Reconcile-level half. Reconcile is correct today because it
// short-circuits before Decide is ever called on a dstack Get error — but
// nothing defended that. This asserts it directly: on a generic (not
// ErrNotFound) Get error, Reconcile must return that error and must never
// reach Status().Update, i.e. it must not journal any phase transition
// based on an uncertain read.
func TestReconcile_UncertainDstackGetError_ReturnsErrorWithoutStatusWrite(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "qwen-unit",
			Namespace:  "default",
			Finalizers: []string{ModelFinalizer}, // pre-seeded: Reconcile's finalizer-add pass is exercised elsewhere (Task 8.0).
		},
		Spec: exampleModelSpec(),
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model).
		Build()
	spy := &statusSpyClient{Client: fakeClient}

	wantErr := errors.New("boom")
	r := &ModelReconciler{
		Client:       spy,
		DstackClient: &erroringDstackClient{getErr: wantErr},
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(model),
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("Reconcile error = %v, want %v wrapped", err, wantErr)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("Reconcile result = %+v, want zero Result", res)
	}
	if spy.updateCalled {
		t.Error("Status().Update was called on an uncertain dstack read; an unreachable dstack must never be journaled as a phase transition")
	}
}

// TestReconcile_AwakeOnDemandNoAction_RequeuesForIdleCheck guards
// IdleRequeueInterval's whole reason for existing (see its doc comment on
// ModelReconciler): an on-demand Model (MinReplicas == 0) that dstack
// already reports awake, with nothing for this pass to Apply, MUST come
// back on a timer — a live manager only re-invokes Reconcile on a Model
// watch event, and once demand stops nothing else ever changes the object.
// Without RequeueAfter here, sleepDue's §6 idle-timeout (phase.go) can
// never fire outside a test that calls Reconcile directly.
func TestReconcile_AwakeOnDemandNoAction_RequeuesForIdleCheck(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	spec := exampleModelSpec()
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "qwen-idle-requeue",
			Namespace:  "default",
			Finalizers: []string{ModelFinalizer},
		},
		Spec: spec,
		Status: squallv1alpha1.ModelStatus{
			Phase: squallv1alpha1.ModelPhaseWaking,
			RunID: "run-1",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model).
		Build()

	r := &ModelReconciler{
		Client:              fakeClient,
		DstackClient:        &fixedRunDstackClient{run: &dstack.Run{Name: model.Name, RunID: "run-1", Replicas: 1}},
		IdleRequeueInterval: 7 * time.Second,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(model),
	})
	if err != nil {
		t.Fatalf("Reconcile error = %v, want nil", err)
	}
	if res.RequeueAfter != 7*time.Second {
		t.Errorf("Reconcile result = %+v, want RequeueAfter = 7s so the idle-sleep check can fire on its own", res)
	}
}

// TestReconcile_PinnedAwake_DoesNotRequeue is the AC17 counterpart: a
// pinned Model (MinReplicas == 1) never sleeps (Decide, phase.go), so
// polling it on a timer for an idle check it can never act on would just be
// wasted reconciles.
func TestReconcile_PinnedAwake_DoesNotRequeue(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	spec := exampleModelSpec()
	spec.MinReplicas = 1
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "qwen-pinned",
			Namespace:  "default",
			Finalizers: []string{ModelFinalizer},
		},
		Spec: spec,
		Status: squallv1alpha1.ModelStatus{
			Phase: squallv1alpha1.ModelPhaseWaking,
			RunID: "run-1",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model).
		Build()

	r := &ModelReconciler{
		Client:       fakeClient,
		DstackClient: &fixedRunDstackClient{run: &dstack.Run{Name: model.Name, RunID: "run-1", Replicas: 1}},
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(model),
	})
	if err != nil {
		t.Fatalf("Reconcile error = %v, want nil", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("Reconcile result = %+v, want zero Result (pinned models never sleep, so polling for idle is wasted work)", res)
	}
}

// TestDecide_ReadyFromProbes is §6 evidence (a).
func TestDecide_ReadyFromProbes(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	spec := exampleModelSpec()

	notReady := Observed{Run: &dstack.Run{Name: "qwen", Replicas: 1, ProbesReady: false}, Ready: false}
	if phase, _ := Decide(notReady, squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseWaking}, spec, true, now); phase == squallv1alpha1.ModelPhaseReady {
		t.Fatal("phase = Ready with probes failing: 'dstack job running' is never Ready (§6)")
	}

	ready := Observed{Run: &dstack.Run{Name: "qwen", Replicas: 1, ProbesReady: true}, Ready: true}
	if phase, _ := Decide(ready, squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseWaking}, spec, true, now); phase != squallv1alpha1.ModelPhaseReady {
		t.Fatalf("phase = %v with probes passing, want Ready", phase)
	}
}

// TestPriceAsFloat64 is the metrics-side counterpart to
// TestEnginePlacement_SendsTheComplianceAllowlist: recordMetrics needs a
// dollars/hour float out of a Price. "2200m" is NOT a legacy spelling to
// preserve (ledger D31 addendum) — there is no released Squall and no
// installed base of CRs, and reading it as a Quantity-style milli suffix
// would be a 1000x price ambiguity. A Price that is not a plain decimal
// cannot reach here past CRD admission, but if it did, 0 is reported rather
// than a fabricated value.
func TestPriceAsFloat64(t *testing.T) {
	tests := []struct {
		in   squallv1alpha1.Price
		want float64
	}{
		{"0.80", 0.80},
		{"2", 2},
		{"2200m", 0},
	}
	for _, tc := range tests {
		if got := priceAsFloat64(tc.in); got != tc.want {
			t.Errorf("priceAsFloat64(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// raceyGetDstackClient reproduces LIVE-1's race deterministically, without
// real goroutines or a real clock: its Get — called from inside
// Reconcile's observe(), well after Reconcile's own initial r.Get and well
// before its final status write — ALSO mutates the SAME object's metadata
// directly through apiClient, exactly once. That models squall-proxy
// rewriting the demand-since annotation (squallv1alpha1.DemandAnnotation)
// on the object's metadata while a reconcile is still in flight (live
// Vast.ai run, 2026-08-28: resourceVersion advanced every 1-2s from two
// proxy replicas, for as long as a request was held).
type raceyGetDstackClient struct {
	run       *dstack.Run
	apiClient client.Client
	key       client.ObjectKey
	raced     bool
}

var _ dstack.Client = (*raceyGetDstackClient)(nil)

func (r *raceyGetDstackClient) Get(ctx context.Context, _ string) (*dstack.Run, error) {
	if !r.raced {
		r.raced = true
		var live squallv1alpha1.Model
		if err := r.apiClient.Get(ctx, r.key, &live); err != nil {
			return nil, err
		}
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations[squallv1alpha1.DemandAnnotation] = time.Now().UTC().Format(time.RFC3339)
		if err := r.apiClient.Update(ctx, &live); err != nil {
			return nil, err
		}
	}
	return r.run, nil
}

func (r *raceyGetDstackClient) Apply(context.Context, dstack.ApplyRequest) (*dstack.Run, error) {
	return nil, errors.New("raceyGetDstackClient: unexpected Apply call")
}

func (r *raceyGetDstackClient) Stop(context.Context, string) error {
	return errors.New("raceyGetDstackClient: unexpected Stop call")
}

func (r *raceyGetDstackClient) Delete(context.Context, string) error {
	return errors.New("raceyGetDstackClient: unexpected Delete call")
}

func (r *raceyGetDstackClient) ListRuns(context.Context) ([]dstack.Run, error) {
	return nil, errors.New("raceyGetDstackClient: unexpected ListRuns call")
}

func (r *raceyGetDstackClient) BackendConfigured(context.Context, string) (bool, error) {
	return false, errors.New("raceyGetDstackClient: unexpected BackendConfigured call")
}

func (r *raceyGetDstackClient) HasFleetFor(context.Context, string) (bool, error) {
	return false, errors.New("raceyGetDstackClient: unexpected HasFleetFor call")
}

func (r *raceyGetDstackClient) EnsureFleet(context.Context, dstack.FleetSpec) error {
	return errors.New("raceyGetDstackClient: unexpected EnsureFleet call")
}

// TestReconcile_ConcurrentMetadataWrite_StatusStillPersists is LIVE-1's
// regression test. Before the fix, the final Status().Update carried an
// optimistic-lock precondition on the whole object's resourceVersion; a
// metadata-only write landing between Reconcile's initial Get and that
// final write (exactly what raceyGetDstackClient simulates) made every
// single reconcile fail with a 409 Conflict for as long as the metadata
// kept changing — proven live by killing a held proxy request and watching
// conflicts drop to zero immediately. The fix (Status().Patch with
// client.MergeFrom) must let a concurrent METADATA change through
// untouched while the STATUS write still lands.
func TestReconcile_ConcurrentMetadataWrite_StatusStillPersists(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	spec := exampleModelSpec()
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "qwen-race",
			Namespace:  "default",
			Finalizers: []string{ModelFinalizer},
		},
		Spec: spec,
		Status: squallv1alpha1.ModelStatus{
			Phase: squallv1alpha1.ModelPhaseWaking,
			RunID: "run-1",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model).
		Build()

	dstackClient := &raceyGetDstackClient{
		run:       &dstack.Run{Name: model.Name, RunID: "run-1", Replicas: 1, ProbesReady: true},
		apiClient: fakeClient,
		key:       client.ObjectKeyFromObject(model),
	}

	r := &ModelReconciler{
		Client:       fakeClient,
		DstackClient: dstackClient,
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(model),
	}); err != nil {
		t.Fatalf("Reconcile error = %v, want nil — a concurrent metadata write must never defeat the status write (LIVE-1)", err)
	}
	if !dstackClient.raced {
		t.Fatal("test setup bug: the simulated concurrent metadata write never fired")
	}

	var got squallv1alpha1.Model
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(model), &got); err != nil {
		t.Fatalf("Get after Reconcile: %v", err)
	}
	if got.Status.Phase != squallv1alpha1.ModelPhaseReady {
		t.Errorf("status.Phase = %v after Reconcile, want Ready — the status write must persist despite the interleaved metadata mutation", got.Status.Phase)
	}
	if got.Annotations[squallv1alpha1.DemandAnnotation] == "" {
		t.Fatal("test setup bug: the concurrent metadata mutation itself did not persist")
	}
}

// TestReconcile_ObservedRunWithoutApply_PopulatesStatusFields is LIVE-2's
// regression test. Before the fix, status.runID/deploymentNum/serviceURL
// were written ONLY inside the action.Apply branch, so a pass that merely
// observes an already-up, already-Ready run (no Apply needed — the
// level-triggered no-op case) left them exactly as a prior LOST status
// write (LIVE-1) left them: empty, forever, even though dstack had a
// healthy run the whole time. Seeding status.Phase == Ready here fakes
// that exact prior amnesia.
func TestReconcile_ObservedRunWithoutApply_PopulatesStatusFields(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	spec := exampleModelSpec()
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "qwen-live2",
			Namespace:  "default",
			Finalizers: []string{ModelFinalizer},
		},
		Spec: spec,
		Status: squallv1alpha1.ModelStatus{
			Phase: squallv1alpha1.ModelPhaseReady, // amnesia: Ready, but nothing else was ever recorded.
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model).
		Build()

	run := &dstack.Run{
		Name:          model.Name,
		RunID:         "run-observed",
		DeploymentNum: 3,
		Replicas:      1,
		ProbesReady:   true,
		ServiceURL:    "/proxy/services/main/qwen3-8-27b/",
	}

	r := &ModelReconciler{
		Client:       fakeClient,
		DstackClient: &fixedRunDstackClient{run: run},
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(model),
	}); err != nil {
		t.Fatalf("Reconcile error = %v, want nil", err)
	}

	var got squallv1alpha1.Model
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(model), &got); err != nil {
		t.Fatalf("Get after Reconcile: %v", err)
	}
	if got.Status.RunID != run.RunID {
		t.Errorf("status.RunID = %q, want %q — must be reconciled from the observed run even when no Apply happened this pass", got.Status.RunID, run.RunID)
	}
	if got.Status.DeploymentNum != int64(run.DeploymentNum) {
		t.Errorf("status.DeploymentNum = %d, want %d", got.Status.DeploymentNum, run.DeploymentNum)
	}
	if got.Status.ServiceURL != run.ServiceURL {
		t.Errorf("status.ServiceURL = %q, want %q", got.Status.ServiceURL, run.ServiceURL)
	}
}

// TestReconcile_ObservedRunWithoutApply_PopulatesStatusFields_RealWireDecode
// is LIVE-5, non-vacuous: the test above builds its *dstack.Run BY HAND
// through fixedRunDstackClient, which proves the controller's field-copy
// logic but proves NOTHING about internal/dstack's own JSON decode — a bug
// there (e.g. a struct tag drifting from the real wire key) would leave
// this hand-built fixture green while a real dstack response still decoded
// wrong. This test drives the SAME reconcile through dstack.NewHTTPClient
// against an httptest server answering the measured shape (a top-level
// "id", per dstack's own Run.id: UUID4 — docs/references/dstack-real-api.md
// §8.3's "service" field is captured from a run carrying this shape too),
// so a decode-layer regression has somewhere to surface.
func TestReconcile_ObservedRunWithoutApply_PopulatesStatusFields_RealWireDecode(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	spec := exampleModelSpec()
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "qwen-live5",
			Namespace:  "default",
			Finalizers: []string{ModelFinalizer},
		},
		Spec: spec,
		Status: squallv1alpha1.ModelStatus{
			Phase: squallv1alpha1.ModelPhaseReady, // amnesia: Ready, but nothing else was ever recorded.
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model).
		Build()

	const wantRunID = "22222222-2222-2222-2222-222222222222"
	const wantServiceURL = "/proxy/services/main/qwen-live5/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/runs/get") {
			t.Errorf("unexpected dstack call: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"id": %q,
			"submitted_at": "2026-08-27T09:00:00Z",
			"status": "running",
			"deployment_num": 3,
			"jobs": [{"job_submissions": [{"deployment_num": 3, "status": "running", "probes": [{"success_streak": 5}]}]}],
			"service": {"url": %q, "model": null, "options": {}},
			"run_spec": {"run_name": %q, "configuration": {"replicas": {"min": 1, "max": 1}, "probes": [{"ready_after": 1}]}}
		}`, wantRunID, wantServiceURL, model.Name)
	}))
	t.Cleanup(srv.Close)

	r := &ModelReconciler{
		Client:       fakeClient,
		DstackClient: dstack.NewHTTPClient(srv.URL, "main", "unused", nil),
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(model),
	}); err != nil {
		t.Fatalf("Reconcile error = %v, want nil", err)
	}

	var got squallv1alpha1.Model
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(model), &got); err != nil {
		t.Fatalf("Get after Reconcile: %v", err)
	}
	if got.Status.RunID != wantRunID {
		t.Errorf("status.RunID = %q, want %q — a real dstack \"id\" must decode and reach status through the observed-run path", got.Status.RunID, wantRunID)
	}
	if got.Status.DeploymentNum != 3 {
		t.Errorf("status.DeploymentNum = %d, want 3", got.Status.DeploymentNum)
	}
	if got.Status.ServiceURL != wantServiceURL {
		t.Errorf("status.ServiceURL = %q, want %q", got.Status.ServiceURL, wantServiceURL)
	}
}

// TestUpdateActivityStatus_ReWakeAdvancesTheAnchor is the re-wake half of
// finding #1, and it is the half a seed-only guard leaves open.
//
// The Model served, slept, and woke again. status.lastRequestAt still holds the
// PREVIOUS cycle's timestamp, which is by definition older than
// scaleDownDelay. Between the flip to Replicas 1 and the first forward no
// replica holds a key, so AnyData is false and that stale anchor is what
// sleepDue reads -- and sleepDue has no hasDemand guard. Leaving the anchor
// unadvanced puts the Model back to sleep in the middle of its own wake, while
// the request that caused the wake is still being held; that request then
// re-triggers demand and the two oscillate at a cold start per lap.
func TestUpdateActivityStatus_ReWakeAdvancesTheAnchor(t *testing.T) {
	previousCycle := metav1.NewTime(time.Unix(100, 0))
	woke := metav1.NewTime(time.Unix(9000, 0))
	model := &squallv1alpha1.Model{Status: squallv1alpha1.ModelStatus{
		LastRequestAt: &previousCycle,
		WakeStartedAt: &woke,
	}}

	updateActivityStatus(model, Observed{Run: &dstack.Run{Replicas: 1}, Activity: &ActivityEvidence{
		Complete: true, AllIdle: true, // complete, idle, and carrying NO data
	}}, true, time.Unix(9001, 0))

	if model.Status.LastRequestAt == nil || !model.Status.LastRequestAt.Equal(&woke) {
		t.Fatalf("re-wake left the anchor at %v; it must advance to the wake instant %v, "+
			"or the Model sleeps mid-wake", model.Status.LastRequestAt, woke)
	}
}

// TestUpdateActivityStatus_WakeNeverRewindsTheAnchor is the guard on the fix
// above: advancing must stay monotonic. A wake instant OLDER than a real
// forwarded request must never pull the anchor back and manufacture an idle
// window that did not happen.
func TestUpdateActivityStatus_WakeNeverRewindsTheAnchor(t *testing.T) {
	served := metav1.NewTime(time.Unix(9000, 0))
	woke := metav1.NewTime(time.Unix(100, 0))
	model := &squallv1alpha1.Model{Status: squallv1alpha1.ModelStatus{
		LastRequestAt: &served,
		WakeStartedAt: &woke,
	}}

	updateActivityStatus(model, Observed{Run: &dstack.Run{Replicas: 1}, Activity: &ActivityEvidence{
		Complete: true, AllIdle: true,
	}}, false, time.Unix(9001, 0))

	if !model.Status.LastRequestAt.Equal(&served) {
		t.Fatalf("anchor rewound to %v; a stale wake instant must never move it back from %v",
			model.Status.LastRequestAt, served)
	}
}

// TestReportProvisioningCondition_RefreshesTimestampPerFailedRun guards the
// half of D163's fix that has no visible behaviour of its own.
//
// provisioningBackoff (phase.go) reads Provisioning.LastTransitionTime as
// "when this failure was recorded". meta.SetStatusCondition refreshes that
// field only when Status FLIPS, so a Model failing the same way run after
// run would keep its FIRST failure's timestamp forever — one backoff window
// would elapse and every later pass would sail straight through, restoring
// the exact hammer the backoff exists to stop. Nothing else in the suite
// would notice: the condition still reads correct, and the pacing test
// passes because it builds its own timestamps.
func TestReportProvisioningCondition_RefreshesTimestampPerFailedRun(t *testing.T) {
	model := &squallv1alpha1.Model{}
	stale := metav1.NewTime(time.Now().Add(-time.Hour))

	report := func(runID string) metav1.Condition {
		reportProvisioningCondition(model, &dstack.ProvisioningFailure{
			RunID: runID, Reason: "failed_to_start_due_to_no_capacity", Message: "no offers",
		}, squallv1alpha1.ModelPhaseRecreating)
		return *meta.FindStatusCondition(model.Status.Conditions, squallv1alpha1.ConditionProvisioning)
	}

	if got := report("run-1"); got.Reason != squallv1alpha1.ReasonNoCapacity {
		t.Fatalf("Reason = %q, want %q", got.Reason, squallv1alpha1.ReasonNoCapacity)
	}

	// Backdate, then observe the SAME run failing again: a re-read of one
	// failure is not a new failure, and must not push the window out.
	meta.FindStatusCondition(model.Status.Conditions, squallv1alpha1.ConditionProvisioning).LastTransitionTime = stale
	if got := report("run-1"); !got.LastTransitionTime.Equal(&stale) {
		t.Errorf("LastTransitionTime = %v after re-observing the SAME run, want it left at %v: level-triggered reconciles would otherwise extend the backoff forever", got.LastTransitionTime, stale)
	}

	// A DIFFERENT run failing is a new failure and must restart the window.
	meta.FindStatusCondition(model.Status.Conditions, squallv1alpha1.ConditionProvisioning).LastTransitionTime = stale
	got := report("run-2")
	if got.LastTransitionTime.Equal(&stale) {
		t.Errorf("LastTransitionTime = %v after a DIFFERENT run failed, want a fresh timestamp: the backoff would lapse permanently and recreate every reconcile", got.LastTransitionTime)
	}
	if time.Since(got.LastTransitionTime.Time) > time.Minute {
		t.Errorf("LastTransitionTime = %v, want approximately now", got.LastTransitionTime)
	}
}
