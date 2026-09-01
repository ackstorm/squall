// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

// These are pure (in-memory fake client.Client, no envtest) tests of D65's
// Reconcile wiring (model_controller.go's ServedModel verification block),
// same convention as model_controller_unit_test.go.

// stubServedModelReader is a scriptable ServedModelReader double. calls
// counts invocations so a test can assert the ONCE-per-run-generation guard
// actually skips the call, not merely that its outcome happened to match.
type stubServedModelReader struct {
	models []string
	err    error
	calls  int
}

func (s *stubServedModelReader) ServedModels(context.Context, string) ([]string, error) {
	s.calls++
	return s.models, s.err
}

// deadRecreateDstackClient simulates F20's uncommanded-death recreate path:
// Get always reports no live run (ErrNotFound), so Decide mints a fresh run
// via Apply. Every other method fails the test if called.
type deadRecreateDstackClient struct {
	newRun *dstack.Run
}

var _ dstack.Client = (*deadRecreateDstackClient)(nil)

func (d *deadRecreateDstackClient) Apply(context.Context, dstack.ApplyRequest) (*dstack.Run, error) {
	return d.newRun, nil
}
func (d *deadRecreateDstackClient) Get(context.Context, string) (*dstack.Run, error) {
	return nil, dstack.ErrNotFound
}
func (d *deadRecreateDstackClient) Stop(context.Context, string) error {
	return errors.New("deadRecreateDstackClient: unexpected Stop call")
}
func (d *deadRecreateDstackClient) Delete(context.Context, string) error {
	return errors.New("deadRecreateDstackClient: unexpected Delete call")
}
func (d *deadRecreateDstackClient) ListRuns(context.Context) ([]dstack.Run, error) {
	return nil, errors.New("deadRecreateDstackClient: unexpected ListRuns call")
}

// BackendConfigured and HasFleetFor default to "everything is fine": this
// stub's whole point is exercising F20's recreate path (Apply:true,
// Replicas:1), which now also runs preflight — the recreate must not be
// vetoed by a diagnostic that has nothing to do with the death being
// tested here.
func (d *deadRecreateDstackClient) BackendConfigured(context.Context, string) (bool, error) {
	return true, nil
}

func (d *deadRecreateDstackClient) HasFleetFor(context.Context, string) (bool, error) {
	return true, nil
}

// EnsureFleet is unreachable: HasFleetFor always reports true above, so
// preflight's remediation branch (LIVE-7/D83) never fires for this stub.
func (d *deadRecreateDstackClient) EnsureFleet(context.Context, dstack.FleetSpec) error {
	return errors.New("deadRecreateDstackClient: unexpected EnsureFleet call")
}

// newServedModelFixture builds a pinned (MinReplicas: 1, always wantAwake),
// already-Ready-evidence Model, so a single Reconcile call lands on
// ModelPhaseReady and the D65 verification block is reached.
func newServedModelFixture(name string) *squallv1alpha1.Model {
	spec := exampleModelSpec()
	spec.MinReplicas = 1
	spec.Model = "Qwen/Qwen3-8-27B-FP8" // non-empty: engineServedName returns the Model's own name.
	return &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			Finalizers: []string{ModelFinalizer},
		},
		Spec: spec,
		Status: squallv1alpha1.ModelStatus{
			Phase:      squallv1alpha1.ModelPhaseWaking,
			RunID:      "run-1",
			ServiceURL: "/proxy/services/main/" + name + "/",
		},
	}
}

func reconcileServedModelFixture(t *testing.T, model *squallv1alpha1.Model, reader ServedModelReader) squallv1alpha1.Model {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model).
		Build()

	r := &ModelReconciler{
		Client: fakeClient,
		DstackClient: &fixedRunDstackClient{run: &dstack.Run{
			Name: model.Name, RunID: "run-1", Replicas: 1, ProbesReady: true,
		}},
		ServedModels: reader,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}); err != nil {
		t.Fatalf("Reconcile error = %v, want nil", err)
	}

	var got squallv1alpha1.Model
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(model), &got); err != nil {
		t.Fatalf("get model after reconcile: %v", err)
	}
	return got
}

// TestReconcile_ServedModel_VerifiedOnMatch is the happy path: the replica
// answers to the name spec.model asked for.
func TestReconcile_ServedModel_VerifiedOnMatch(t *testing.T) {
	model := newServedModelFixture("qwen-served-match")
	reader := &stubServedModelReader{models: []string{model.Name}}

	got := reconcileServedModelFixture(t, model, reader)

	if got.Status.Phase != squallv1alpha1.ModelPhaseReady {
		t.Fatalf("Phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.ServedModel != model.Name {
		t.Fatalf("ServedModel = %q, want %q", got.Status.ServedModel, model.Name)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionServedModelVerified)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != squallv1alpha1.ReasonVerified {
		t.Fatalf("ServedModelVerified condition = %+v, want True/Verified", cond)
	}
	if reader.calls != 1 {
		t.Fatalf("ServedModels called %d times, want exactly 1", reader.calls)
	}
}

// TestReconcile_ServedModel_MismatchReported is the D65 scenario exactly:
// the replica is healthy and answering, and it answers to the WRONG name.
// The wake must still proceed to Ready (a mismatch is reported, not acted
// on — 1->0 fails safe, and tearing down a running generation over a
// diagnostic disagreement is exactly the wrong trade, ambiguity resolution
// 2).
func TestReconcile_ServedModel_MismatchReported(t *testing.T) {
	model := newServedModelFixture("qwen-served-mismatch")
	reader := &stubServedModelReader{models: []string{"Qwen/Qwen3-0.6B"}}

	got := reconcileServedModelFixture(t, model, reader)

	if got.Status.Phase != squallv1alpha1.ModelPhaseReady {
		t.Fatalf("Phase = %q, want Ready (a mismatch must not block the wake)", got.Status.Phase)
	}
	if got.Status.ServedModel != "Qwen/Qwen3-0.6B" {
		t.Fatalf("ServedModel = %q, want the replica's actual (wrong) answer recorded", got.Status.ServedModel)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionServedModelVerified)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != squallv1alpha1.ReasonServedModelMismatch {
		t.Fatalf("ServedModelVerified condition = %+v, want False/ServedModelMismatch", cond)
	}
}

// TestReconcile_ServedModel_FailsOpenOnError covers every /v1/models
// transport failure ServedModelReader can report (timeout, connection
// refused, non-200, unparseable body — served.go turns every one of them
// into a plain error, never an empty list): the wake must proceed exactly
// as if D65's check did not exist, with the disagreement recorded as
// Unknown rather than either verdict.
func TestReconcile_ServedModel_FailsOpenOnError(t *testing.T) {
	model := newServedModelFixture("qwen-served-unreachable")
	reader := &stubServedModelReader{err: errors.New("boom: replica unreachable")}

	got := reconcileServedModelFixture(t, model, reader)

	if got.Status.Phase != squallv1alpha1.ModelPhaseReady {
		t.Fatalf("Phase = %q, want Ready — a diagnostic failure must never block the wake (0->1 fails open)", got.Status.Phase)
	}
	if got.Status.ServedModel != "" {
		t.Fatalf("ServedModel = %q, want empty — an unreachable replica is not evidence of anything", got.Status.ServedModel)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionServedModelVerified)
	if cond == nil || cond.Status != metav1.ConditionUnknown || cond.Reason != squallv1alpha1.ReasonUnverified {
		t.Fatalf("ServedModelVerified condition = %+v, want Unknown/Unverified", cond)
	}
}

// TestReconcile_ServedModel_SkipsOnceAlreadySet is the "ONCE per run
// generation" half: a Model whose ConditionServedModelVerified is already
// True for this run must not re-verify on every level-triggered Ready
// reconcile. The skip is keyed off the CONDITION, not status.servedModel
// (I3, block 2 review): gating on the field alone made a reported mismatch
// (which also sets the field) latch forever, so setting only the field here
// — without the condition a real prior verified reconcile would also leave
// — would no longer exercise the skip this test is named for.
func TestReconcile_ServedModel_SkipsOnceAlreadySet(t *testing.T) {
	model := newServedModelFixture("qwen-served-already-set")
	model.Status.ServedModel = model.Name // pretend a prior reconcile already verified it.
	meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
		Type: squallv1alpha1.ConditionServedModelVerified, Status: metav1.ConditionTrue,
		Reason: squallv1alpha1.ReasonVerified,
	})
	reader := &stubServedModelReader{err: errors.New("must not be called")}

	got := reconcileServedModelFixture(t, model, reader)

	if reader.calls != 0 {
		t.Fatalf("ServedModels called %d times, want 0 (already verified this run generation)", reader.calls)
	}
	if got.Status.ServedModel != model.Name {
		t.Fatalf("ServedModel = %q, want unchanged %q", got.Status.ServedModel, model.Name)
	}
}

// TestReconcile_ServedModel_MismatchDoesNotLatch is I3's second half: a
// reported mismatch must be re-evaluated on the NEXT Ready reconcile (not
// skipped, the way a True verification is), and must clear once the replica
// comes to agree — e.g. a slow `ollama cp` finishing between the two
// reconciles this test drives by hand.
func TestReconcile_ServedModel_MismatchDoesNotLatch(t *testing.T) {
	model := newServedModelFixture("qwen-served-heals")
	reader := &stubServedModelReader{models: []string{"Qwen/Qwen3-0.6B"}} // wrong, at first.

	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model).
		Build()
	r := &ModelReconciler{
		Client: fakeClient,
		DstackClient: &fixedRunDstackClient{run: &dstack.Run{
			Name: model.Name, RunID: "run-1", Replicas: 1, ProbesReady: true,
		}},
		ServedModels: reader,
	}
	key := client.ObjectKeyFromObject(model)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first Reconcile error = %v, want nil", err)
	}
	var afterFirst squallv1alpha1.Model
	if err := fakeClient.Get(context.Background(), key, &afterFirst); err != nil {
		t.Fatalf("get model after first reconcile: %v", err)
	}
	cond := meta.FindStatusCondition(afterFirst.Status.Conditions, squallv1alpha1.ConditionServedModelVerified)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != squallv1alpha1.ReasonServedModelMismatch {
		t.Fatalf("after first reconcile: ServedModelVerified = %+v, want False/ServedModelMismatch", cond)
	}
	if reader.calls != 1 {
		t.Fatalf("after first reconcile: ServedModels called %d times, want 1", reader.calls)
	}

	// The replica now agrees (the `ollama cp` that was still running has
	// finished, say). A mismatch must not have latched: the next Ready
	// reconcile must call ServedModels AGAIN and must be able to clear it.
	reader.models = []string{model.Name}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("second Reconcile error = %v, want nil", err)
	}
	var afterSecond squallv1alpha1.Model
	if err := fakeClient.Get(context.Background(), key, &afterSecond); err != nil {
		t.Fatalf("get model after second reconcile: %v", err)
	}
	if reader.calls != 2 {
		t.Fatalf("after second reconcile: ServedModels called %d times, want 2 (a mismatch must not latch)", reader.calls)
	}
	cond = meta.FindStatusCondition(afterSecond.Status.Conditions, squallv1alpha1.ConditionServedModelVerified)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != squallv1alpha1.ReasonVerified {
		t.Fatalf("after second reconcile: ServedModelVerified = %+v, want True/Verified (must clear once the replica agrees)", cond)
	}
	if afterSecond.Status.ServedModel != model.Name {
		t.Fatalf("after second reconcile: ServedModel = %q, want %q", afterSecond.Status.ServedModel, model.Name)
	}
}

// TestReconcile_ServedModel_ClearedOnFreshRunMint is F20's recreate case: a
// dead run's prior served-model answer must not survive as stale evidence
// about a brand-new run generation. This mutation-guards the `if
// action.Current == nil { model.Status.ServedModel = "" }` line in
// model_controller.go — dropping it would leave the OLD run's answer on the
// CR after a recreate, silently skipping re-verification of the new run
// forever (the "skip when already set" guard would never fire again).
func TestReconcile_ServedModel_ClearedOnFreshRunMint(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	spec := exampleModelSpec()
	spec.MinReplicas = 1
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "qwen-recreate",
			Namespace:  "default",
			Finalizers: []string{ModelFinalizer},
		},
		Spec: spec,
		Status: squallv1alpha1.ModelStatus{
			Phase:       squallv1alpha1.ModelPhaseReady,
			RunID:       "old-run", // prior.RunID != "" -> Decide reads this as "died", F20.
			ServedModel: "stale-answer-from-the-dead-run",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model).
		Build()

	r := &ModelReconciler{
		Client:       fakeClient,
		DstackClient: &deadRecreateDstackClient{newRun: &dstack.Run{Name: model.Name, RunID: "new-run", DeploymentNum: 1}},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}); err != nil {
		t.Fatalf("Reconcile error = %v, want nil", err)
	}

	var got squallv1alpha1.Model
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(model), &got); err != nil {
		t.Fatalf("get model after reconcile: %v", err)
	}
	if got.Status.Phase != squallv1alpha1.ModelPhaseRecreating {
		t.Fatalf("Phase = %q, want Recreating (F20)", got.Status.Phase)
	}
	if got.Status.RunID != "new-run" {
		t.Fatalf("RunID = %q, want the freshly minted run", got.Status.RunID)
	}
	if got.Status.ServedModel != "" {
		t.Fatalf("ServedModel = %q, want cleared on a fresh run mint, not the dead run's stale answer", got.Status.ServedModel)
	}
}

// TestReconcile_ServedModel_ConditionClearedOnFreshRunMint is F20's recreate
// case again, but — unlike TestReconcile_ServedModel_ClearedOnFreshRunMint,
// which has no ServedModels reader wired and so never reaches the D65 gate
// at all — this one wires a reader and starts from a condition already
// True, then drives a SECOND reconcile once the new run reports Ready, so
// it actually exercises the gate (Decide's observed.Run==nil branch always
// returns Recreating/Waking on the very reconcile that mints the run — see
// phase.go — so the gate can only fire on the next one). It mutation-guards
// the `meta.RemoveStatusCondition(...)` line added alongside I3's fix:
// without it, the DEAD run's stale True verification would satisfy the new
// condition-based gate and silently skip verifying the fresh run.
func TestReconcile_ServedModel_ConditionClearedOnFreshRunMint(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	spec := exampleModelSpec()
	spec.MinReplicas = 1
	spec.Model = "Qwen/Qwen3-8-27B-FP8"
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "qwen-recreate-condition",
			Namespace:  "default",
			Finalizers: []string{ModelFinalizer},
		},
		Spec: spec,
		Status: squallv1alpha1.ModelStatus{
			Phase:       squallv1alpha1.ModelPhaseReady,
			RunID:       "old-run", // prior.RunID != "" -> Decide reads this as "died", F20.
			ServedModel: "qwen-recreate-condition",
		},
	}
	meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
		Type: squallv1alpha1.ConditionServedModelVerified, Status: metav1.ConditionTrue,
		Reason: squallv1alpha1.ReasonVerified,
	})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model).
		Build()
	key := client.ObjectKeyFromObject(model)
	reader := &stubServedModelReader{models: []string{model.Name}}

	// Pass 1: dstack has no live run under the old id -> F20 recreate mints
	// "new-run". This is where the condition-clear under test must run.
	r1 := &ModelReconciler{
		Client:       fakeClient,
		DstackClient: &deadRecreateDstackClient{newRun: &dstack.Run{Name: model.Name, RunID: "new-run", DeploymentNum: 1}},
		ServedModels: reader,
	}
	if _, err := r1.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first Reconcile error = %v, want nil", err)
	}
	if reader.calls != 0 {
		t.Fatalf("ServedModels called %d times on the mint pass, want 0 (newPhase is Recreating, not Ready, on this pass)", reader.calls)
	}

	// Pass 2: the fresh run now reports ready. If the stale True condition
	// had survived pass 1, this pass would skip the D65 check entirely.
	r2 := &ModelReconciler{
		Client: fakeClient,
		DstackClient: &fixedRunDstackClient{run: &dstack.Run{
			Name: model.Name, RunID: "new-run", DeploymentNum: 1, Replicas: 1, ProbesReady: true,
		}},
		ServedModels: reader,
	}
	if _, err := r2.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("second Reconcile error = %v, want nil", err)
	}

	if reader.calls != 1 {
		t.Fatalf("ServedModels called %d times after the run came up, want 1 (the stale True condition must not skip re-verification of the fresh run)", reader.calls)
	}

	var got squallv1alpha1.Model
	if err := fakeClient.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get model after reconcile: %v", err)
	}
	if got.Status.RunID != "new-run" {
		t.Fatalf("RunID = %q, want the freshly minted run", got.Status.RunID)
	}
	if got.Status.Phase != squallv1alpha1.ModelPhaseReady {
		t.Fatalf("Phase = %q, want Ready", got.Status.Phase)
	}
}

// TestReconcile_ServedModel_NotCalledBeforeReady is the lifecycle decision
// called out in the task brief: weights can take 10-15 minutes to arrive
// (measured), and an engine that is up but hasn't loaded them yet is
// legitimately "not serving it yet" — a Waking reconcile (dstack probes not
// yet green) must not call ServedModels at all, let alone read an early
// non-match as a mismatch.
func TestReconcile_ServedModel_NotCalledBeforeReady(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	model := newServedModelFixture("qwen-not-ready-yet")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(model).
		Build()

	reader := &stubServedModelReader{err: errors.New("must not be called before Ready")}
	r := &ModelReconciler{
		Client: fakeClient,
		DstackClient: &fixedRunDstackClient{run: &dstack.Run{
			Name: model.Name, RunID: "run-1", Replicas: 1, ProbesReady: false, // still coming up.
		}},
		ServedModels: reader,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}); err != nil {
		t.Fatalf("Reconcile error = %v, want nil", err)
	}

	var got squallv1alpha1.Model
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(model), &got); err != nil {
		t.Fatalf("get model after reconcile: %v", err)
	}
	if got.Status.Phase == squallv1alpha1.ModelPhaseReady {
		t.Fatalf("Phase = Ready with ProbesReady false, want Waking/Recreating (test fixture bug, not the code under test)")
	}
	if reader.calls != 0 {
		t.Fatalf("ServedModels called %d times before Ready, want 0", reader.calls)
	}
}

// TestReconcile_ServedModel_NoExpectationWhenSpecModelUnset covers the
// image-with-baked-in-weights case (engineServedName returns "" when
// spec.model is empty, engine.go): there is nothing to compare the
// replica's answer against, so it must never be reported as a mismatch no
// matter what /v1/models says.
func TestReconcile_ServedModel_NoExpectationWhenSpecModelUnset(t *testing.T) {
	model := newServedModelFixture("qwen-no-spec-model")
	model.Spec.Model = "" // baked-in image: nothing for D65 to compare against.
	reader := &stubServedModelReader{models: []string{"whatever-the-image-bakes-in"}}

	got := reconcileServedModelFixture(t, model, reader)

	cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionServedModelVerified)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != squallv1alpha1.ReasonVerified {
		t.Fatalf("ServedModelVerified condition = %+v, want True/Verified — no spec.model means no expectation to violate", cond)
	}
}

// TestReconcile_ServedModel_FailsOpenWhenServiceURLEmpty is today's real
// production path: status.serviceURL is written by Task 5 (D25's
// controller half), not this task, so until that lands every Ready
// reconcile hits this exact case. Using the REAL HTTPServedModelReader
// (not a stub) proves served.go's own "no service URL" guard is what fails
// open here, end to end.
func TestReconcile_ServedModel_FailsOpenWhenServiceURLEmpty(t *testing.T) {
	model := newServedModelFixture("qwen-no-service-url")
	model.Status.ServiceURL = ""

	got := reconcileServedModelFixture(t, model, HTTPServedModelReader{BaseURL: "http://unused.invalid"})

	if got.Status.Phase != squallv1alpha1.ModelPhaseReady {
		t.Fatalf("Phase = %q, want Ready — a missing serviceURL must not block the wake", got.Status.Phase)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionServedModelVerified)
	if cond == nil || cond.Status != metav1.ConditionUnknown || cond.Reason != squallv1alpha1.ReasonUnverified {
		t.Fatalf("ServedModelVerified condition = %+v, want Unknown/Unverified", cond)
	}
}

// TestServedModelToForward is D100's unit: status.servedModel is what
// squall-proxy rewrites a request's "model" field to, so it must be ONE
// name or nothing. It used to be strings.Join(served, ","), and the join
// only looked harmless because vLLM reports a single id.
func TestServedModelToForward(t *testing.T) {
	tests := []struct {
		name   string
		served []string
		want   string
		expect string
	}{
		{
			// THE LIVE BUG, 2026-08-31. Ollama reports the `ollama cp` alias
			// AND the source weights (D62), so the joined field became
			// "ollama-tiny:latest,qwen2.5:0.5b" and every request after
			// verification passed was answered 400 "model is required".
			name:   "ollama reports alias plus source weights -> forward under the alias only",
			served: []string{"ollama-tiny:latest", "qwen2.5:0.5b"},
			want:   "ollama-tiny",
			expect: "ollama-tiny:latest",
		},
		{
			name:   "vllm's single served name",
			served: []string{"qwen3-8-27b"},
			want:   "qwen3-8-27b",
			expect: "qwen3-8-27b",
		},
		{
			// A mismatch has no safe name. Publishing anything here would
			// have the proxy rewrite requests to a model nobody asked for.
			name:   "mismatch -> nothing to forward under",
			served: []string{"Qwen/Qwen3-0.6B"},
			want:   "qwen3-8-27b",
			expect: "",
		},
		{
			// No spec.model: one id is unambiguous, so the proxy can still
			// bridge the caller's Model name to the engine's own name.
			name:   "no expectation, one served id -> forward under it",
			served: []string{"whatever-the-image-bakes-in"},
			want:   "",
			expect: "whatever-the-image-bakes-in",
		},
		{
			// No expectation and several ids is a guess, and guessing routes
			// a payload at a model nobody chose.
			name:   "no expectation, several served ids -> refuse to guess",
			served: []string{"a", "b"},
			want:   "",
			expect: "",
		},
		{
			name:   "replica serves nothing",
			served: nil,
			want:   "qwen",
			expect: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := servedModelToForward(tt.served, tt.want); got != tt.expect {
				t.Errorf("servedModelToForward(%v, %q) = %q, want %q", tt.served, tt.want, got, tt.expect)
			}
			// Whatever comes out must never be a list: the proxy writes this
			// verbatim into the request body's "model" field.
			if got := servedModelToForward(tt.served, tt.want); strings.Contains(got, ",") {
				t.Errorf("servedModelToForward returned a list %q; the proxy would forward under a name that exists nowhere", got)
			}
		})
	}
}

// TestReconcile_ServedModel_MultiModelReplica_PublishesOneName is D100 through
// the real Reconcile path: an Ollama-shaped replica must leave
// status.servedModel holding a single forwardable id, not the joined report.
func TestReconcile_ServedModel_MultiModelReplica_PublishesOneName(t *testing.T) {
	model := newServedModelFixture("ollama-tiny")
	reader := &stubServedModelReader{models: []string{"ollama-tiny:latest", "qwen2.5:0.5b"}}

	got := reconcileServedModelFixture(t, model, reader)

	cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionServedModelVerified)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ServedModelVerified = %+v, want True (the alias does satisfy the expectation)", cond)
	}
	if got.Status.ForwardModel != "ollama-tiny:latest" {
		t.Fatalf("ForwardModel = %q, want %q — the proxy rewrites the request's model field to this verbatim",
			got.Status.ForwardModel, "ollama-tiny:latest")
	}
	// D65's diagnostic survives untouched: the full report stays visible in
	// status.servedModel and in the printer column built from it.
	if got.Status.ServedModel != "ollama-tiny:latest,qwen2.5:0.5b" {
		t.Errorf("ServedModel = %q, want the full report kept for diagnosis", got.Status.ServedModel)
	}
	if strings.Contains(got.Status.ForwardModel, ",") {
		t.Errorf("ForwardModel = %q is a list; the proxy would forward under a name that exists nowhere", got.Status.ForwardModel)
	}
}
