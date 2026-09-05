// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

// These are pure (in-memory fake client.Client, no envtest) tests of Task
// 5's Schedulable wiring (D58, D67, and the Price.Validate carry-over from
// Block 1) — same convention as model_controller_unit_test.go.

// schedulableDstackClient scripts a cold-start wake (Get -> ErrNotFound,
// Apply -> a fixed Run) plus the two Task 5 preflight calls, so a test can
// assert both what Reconcile decided AND whether it actually called Apply.
type schedulableDstackClient struct {
	backendConfigured bool
	hasFleet          bool
	checkErr          error
	// ensureFleetErr, when set, makes EnsureFleet fail — LIVE-7/D83's
	// remediation of hasFleet:false must itself be able to fail, or NoFleet
	// could never be observed as a Schedulable reason again.
	ensureFleetErr error

	// getRun, when set, is what Get answers instead of the cold-start
	// dstack.ErrNotFound default — used to put a test on the 1->0 sleep
	// path (Decide's observed.Run.Replicas > 0 branch) rather than the 0->1
	// wake path every other fixture here exercises.
	getRun *dstack.Run

	applyRun *dstack.Run
	applyErr error
	applied  bool
	// lastApplyReq is what Apply was actually called with, so a test can
	// assert e.g. Replicas: 0 (a sleep) rather than only whether Apply ran
	// at all.
	lastApplyReq dstack.ApplyRequest
}

var _ dstack.Client = (*schedulableDstackClient)(nil)

func (s *schedulableDstackClient) Get(context.Context, string) (*dstack.Run, error) {
	if s.getRun != nil {
		return s.getRun, nil
	}
	return nil, dstack.ErrNotFound
}

func (s *schedulableDstackClient) Apply(_ context.Context, req dstack.ApplyRequest) (*dstack.Run, error) {
	s.applied = true
	s.lastApplyReq = req
	if s.applyErr != nil {
		return nil, s.applyErr
	}
	return s.applyRun, nil
}

func (s *schedulableDstackClient) Stop(context.Context, string) error { return nil }
func (s *schedulableDstackClient) Delete(context.Context, string) error {
	return errors.New("schedulableDstackClient: unexpected Delete call")
}
func (s *schedulableDstackClient) ListRuns(context.Context) ([]dstack.Run, error) {
	return nil, errors.New("schedulableDstackClient: unexpected ListRuns call")
}

func (s *schedulableDstackClient) BackendConfigured(context.Context, string) (bool, error) {
	return s.backendConfigured, s.checkErr
}

func (s *schedulableDstackClient) HasFleetFor(context.Context, string) (bool, error) {
	return s.hasFleet, s.checkErr
}

func (s *schedulableDstackClient) EnsureFleet(context.Context, dstack.FleetSpec) error {
	return s.ensureFleetErr
}

// pinnedColdModel is a MinReplicas:1 (always wantAwake) Model with no live
// run yet, so a single Reconcile lands on Decide's Apply:true, Replicas:1
// cold-start path — exactly where both the price gate and preflight sit.
func pinnedColdModel(name string, price *squallv1alpha1.Price) *squallv1alpha1.Model {
	spec := exampleModelSpec()
	spec.MinReplicas = 1
	spec.Placement.MaxPricePerHour = price
	return &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Finalizers: []string{ModelFinalizer}},
		Spec:       spec,
	}
}

func reconcileSchedulableFixture(t *testing.T, model *squallv1alpha1.Model, dc *schedulableDstackClient) squallv1alpha1.Model {
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

	r := &ModelReconciler{Client: fakeClient, DstackClient: dc}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}); err != nil {
		t.Fatalf("Reconcile error = %v, want nil", err)
	}

	var got squallv1alpha1.Model
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(model), &got); err != nil {
		t.Fatalf("get model after reconcile: %v", err)
	}
	return got
}

// TestReconcile_InvalidPrice_RefusesToProvision is the one deliberate
// blocking preflight path (the additional requirement carried from Block
// 1, D70): an unparseable maxPricePerHour must veto the Apply entirely,
// unlike every other Schedulable=False reason.
func TestReconcile_InvalidPrice_RefusesToProvision(t *testing.T) {
	bad := squallv1alpha1.Price("not-a-number")
	model := pinnedColdModel("bad-price", &bad)
	dc := &schedulableDstackClient{backendConfigured: true, hasFleet: true, applyRun: &dstack.Run{Name: model.Name, RunID: "should-not-happen"}}

	got := reconcileSchedulableFixture(t, model, dc)

	if dc.applied {
		t.Fatal("Apply was called with an invalid price: the one preflight path that must block, did not")
	}
	if got.Status.RunID != "" {
		t.Fatalf("RunID = %q, want empty: no run was ever applied", got.Status.RunID)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionSchedulable)
	if cond == nil {
		t.Fatal("Schedulable condition not set")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != squallv1alpha1.ReasonInvalidPrice {
		t.Fatalf("Schedulable condition = %+v, want False/InvalidPrice", cond)
	}
	if !strings.Contains(cond.Message, "not-a-number") {
		t.Fatalf("condition message = %q, want it to name the bad price", cond.Message)
	}
	// I2(b), block 2 review: nothing was actuated this pass (Apply was
	// vetoed above), so status.phase must stay at whatever was last
	// persisted (empty, for a brand-new Model) rather than Decide's
	// Waking — writing Waking here would claim a run exists with an empty
	// runID, and squall-proxy's Decide would then hold every request for
	// the full holdTimeout against a run that was never applied.
	if got.Status.Phase != "" {
		t.Fatalf("Status.Phase = %q, want unchanged (empty) from before the veto: "+
			"a vetoed wake must not claim Waking with no run applied", got.Status.Phase)
	}
}

func TestReconcile_TerminalRunReportsProvisioningFailure(t *testing.T) {
	model := pinnedColdModel("failed-provision", nil)
	model.Status.RunID = "failed-run"
	model.Status.Phase = squallv1alpha1.ModelPhaseWaking
	dc := &schedulableDstackClient{
		backendConfigured: true,
		hasFleet:          true,
		getRun: &dstack.Run{
			Name: "failed-provision", RunID: "failed-run", Status: "failed",
			ProvisioningFailure: &dstack.ProvisioningFailure{
				RunID: "failed-run", Reason: "failed_to_start_due_to_no_capacity",
				Message: `{"error":"insufficient_credit"}: 429 Too Many Requests`,
			},
		},
		applyRun: &dstack.Run{Name: "failed-provision", RunID: "replacement-run", Replicas: 1},
	}

	got := reconcileSchedulableFixture(t, model, dc)

	cond := meta.FindStatusCondition(got.Status.Conditions, "Provisioning")
	if cond == nil {
		t.Fatal("Provisioning condition not set")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != "InsufficientCredit" {
		t.Fatalf("Provisioning condition = %+v, want False/InsufficientCredit", cond)
	}
	if !strings.Contains(cond.Message, "failed-run") || !strings.Contains(cond.Message, "insufficient_credit") {
		t.Fatalf("condition message = %q, want failed run and dstack cause", cond.Message)
	}
}

func TestReconcile_ReadyRunClearsProvisioningFailure(t *testing.T) {
	model := pinnedColdModel("recovered-provision", nil)
	model.Status.RunID = "replacement-run"
	model.Status.Phase = squallv1alpha1.ModelPhaseRecreating
	meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
		Type: "Provisioning", Status: metav1.ConditionFalse, Reason: "NoCapacity",
	})
	dc := &schedulableDstackClient{
		getRun: &dstack.Run{
			Name: "recovered-provision", RunID: "replacement-run", Status: "running",
			Replicas: 1, ProbesReady: true,
		},
	}

	got := reconcileSchedulableFixture(t, model, dc)

	cond := meta.FindStatusCondition(got.Status.Conditions, "Provisioning")
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Provisioned" {
		t.Fatalf("Provisioning condition = %+v, want True/Provisioned", cond)
	}
}

func TestReportProvisioningConditionClassifiesFailure(t *testing.T) {
	tests := []struct {
		name    string
		failure dstack.ProvisioningFailure
		want    string
	}{
		{name: "credit", failure: dstack.ProvisioningFailure{Reason: "failed_to_start_due_to_no_capacity", Message: "insufficient_credit"}, want: squallv1alpha1.ReasonInsufficientCredit},
		{name: "capacity", failure: dstack.ProvisioningFailure{Reason: "failed_to_start_due_to_no_capacity"}, want: squallv1alpha1.ReasonNoCapacity},
		{name: "rate limit", failure: dstack.ProvisioningFailure{Message: "429 Too Many Requests"}, want: squallv1alpha1.ReasonBackendRateLimited},
		{name: "other", failure: dstack.ProvisioningFailure{Reason: "server_error"}, want: squallv1alpha1.ReasonProvisioningFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &squallv1alpha1.Model{}
			reportProvisioningCondition(model, &tt.failure, squallv1alpha1.ModelPhaseDead)
			cond := meta.FindStatusCondition(model.Status.Conditions, squallv1alpha1.ConditionProvisioning)
			if cond == nil || cond.Reason != tt.want {
				t.Fatalf("Provisioning condition = %+v, want reason %s", cond, tt.want)
			}
		})
	}
}

// TestReconcile_InvalidPrice_DoesNotBlockSleep is I2(a), block 2 review: the
// price gate's veto (action.Apply = false) must never reach a 1->0 sleep.
// checkSchedulable is only entered on a wake (model_controller.go's
// `action.Apply && action.Replicas > 0` guard) — removing the
// `action.Replicas > 0` half would let an invalid price veto a sleep flip
// too, and a Model with a garbage price would then never be able to scale to
// zero: the GPU would bill forever. 1->0 fails safe; this proves the veto
// cannot leak into that side.
func TestReconcile_InvalidPrice_DoesNotBlockSleep(t *testing.T) {
	bad := squallv1alpha1.Price("cheap")
	spec := exampleModelSpec()
	spec.MinReplicas = 0
	spec.IdleTimeout = metav1.Duration{Duration: time.Second}
	spec.Placement.MaxPricePerHour = &bad
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "sleep-bad-price", Namespace: "default", Finalizers: []string{ModelFinalizer}},
		Spec:       spec,
	}

	// Clean, complete, idle activity evidence aged well past
	// idleTimeout — sleepDue's condition (phase.go) for the
	// 1->0 flip.
	idle := activityServer(t, squallv1alpha1.ActivityReport{
		Models: map[string]squallv1alpha1.ModelActivity{
			model.Name: {InFlight: 0, LastRequestAt: time.Now().UTC().Add(-time.Hour)},
		},
	})
	endpoints := endpointSlicesForServers(t, idle)

	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add clientgo scheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&squallv1alpha1.Model{}).
		WithObjects(append([]client.Object{model}, endpoints...)...).
		Build()

	dc := &schedulableDstackClient{
		// Already awake (Replicas > 0), so Decide reaches the sleep-check
		// branch instead of the cold-start wake this file's other fixtures
		// exercise.
		getRun:   &dstack.Run{Name: model.Name, RunID: "run-awake", Replicas: 1},
		applyRun: &dstack.Run{Name: model.Name, RunID: "run-awake", Replicas: 0},
	}
	r := &ModelReconciler{
		Client:       fakeClient,
		DstackClient: dc,
		ProxyService: types.NamespacedName{Namespace: testProxyNamespace, Name: testProxyService},
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}); err != nil {
		t.Fatalf("Reconcile error = %v, want nil", err)
	}

	if !dc.applied {
		t.Fatal("Apply was NOT called: an invalid price must not block a 1->0 sleep (1->0 fails safe)")
	}
	if dc.lastApplyReq.Replicas != 0 {
		t.Fatalf("Apply called with Replicas = %d, want 0 (sleep): an invalid price vetoed the sleep flip", dc.lastApplyReq.Replicas)
	}
}

// TestReconcile_PreflightDiagnosesButNeverVetoes proves every OTHER
// preflight failure is diagnosis, not enforcement: 0->1 fails open, so
// Apply must still be called and RunID still written even though the
// Schedulable condition reports False. LIVE-7/D83: a missing fleet is now
// remediated (EnsureFleet), so to still observe ReasonNoFleet here the
// remediation attempt itself must also fail — proving the fail-open
// contract holds even for the doubly-failed case.
func TestReconcile_PreflightDiagnosesButNeverVetoes(t *testing.T) {
	model := pinnedColdModel("no-fleet", nil)
	dc := &schedulableDstackClient{
		backendConfigured: true, hasFleet: false, // configured, but no fleet admits it
		ensureFleetErr: errors.New("dstack rejected fleet create"), // and squall cannot create one either
		applyRun:       &dstack.Run{Name: model.Name, RunID: "run-1", ServiceURL: "/proxy/services/main/no-fleet/"},
	}

	got := reconcileSchedulableFixture(t, model, dc)

	if !dc.applied {
		t.Fatal("Apply was NOT called: a diagnostic must never veto a wake")
	}
	if got.Status.RunID != "run-1" {
		t.Fatalf("RunID = %q, want the applied run's id", got.Status.RunID)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionSchedulable)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != squallv1alpha1.ReasonNoFleet {
		t.Fatalf("Schedulable condition = %+v, want False/NoFleet", cond)
	}
}

// TestReconcile_MissingFleetIsRemediatedNotJustDiagnosed is LIVE-7/D83's
// Reconcile-level regression test: a backend with no admitting fleet must
// get one CREATED via EnsureFleet before the Schedulable verdict is formed,
// so a Model whose only problem was a missing fleet ends up schedulable in
// the very same reconcile — no separate remediation pass, no manual fleet
// declaration required.
func TestReconcile_MissingFleetIsRemediatedNotJustDiagnosed(t *testing.T) {
	model := pinnedColdModel("auto-fleet", nil)
	dc := &schedulableDstackClient{
		backendConfigured: true, hasFleet: false, // configured, but no fleet admits it (yet)
		applyRun: &dstack.Run{Name: model.Name, RunID: "run-auto", ServiceURL: "/proxy/services/main/auto-fleet/"},
	}

	got := reconcileSchedulableFixture(t, model, dc)

	if !dc.applied {
		t.Fatal("Apply was NOT called: the wake must proceed once the fleet is created")
	}
	if got.Status.RunID != "run-auto" {
		t.Fatalf("RunID = %q, want the applied run's id", got.Status.RunID)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionSchedulable); cond != nil && cond.Status == metav1.ConditionFalse {
		t.Fatalf("Schedulable condition = %+v, want no False verdict: EnsureFleet succeeded", cond)
	}
}

// TestReconcile_PreflightErrorStillApplies enumerates the fail-open path
// explicitly at the reconcile level (preflight_test.go proves preflight
// itself fails open; this proves Reconcile's wiring doesn't reintroduce a
// veto): a preflight call that errors (dstack unreachable) must still let
// Apply through, and must not report Schedulable=False off an inconclusive
// check.
func TestReconcile_PreflightErrorStillApplies(t *testing.T) {
	model := pinnedColdModel("dstack-flaky", nil)
	dc := &schedulableDstackClient{
		checkErr: errors.New("dstack unreachable"),
		applyRun: &dstack.Run{Name: model.Name, RunID: "run-2"},
	}

	got := reconcileSchedulableFixture(t, model, dc)

	if !dc.applied {
		t.Fatal("Apply was NOT called: an unreachable diagnostic must not veto a wake")
	}
	if got.Status.RunID != "run-2" {
		t.Fatalf("RunID = %q, want the applied run's id", got.Status.RunID)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionSchedulable); cond != nil && cond.Status == metav1.ConditionFalse {
		t.Fatalf("Schedulable condition = %+v, want no False verdict from an inconclusive check", cond)
	}
}

// TestReconcile_SuccessfulApply_WritesServiceURL closes D25's controller
// half: status.serviceURL had a field and a reader (Task 4's D65 check) but
// no writer, so it was inert in production. Apply's run.ServiceURL must
// land on the Model.
func TestReconcile_SuccessfulApply_WritesServiceURL(t *testing.T) {
	model := pinnedColdModel("serviceurl", nil)
	dc := &schedulableDstackClient{
		backendConfigured: true, hasFleet: true,
		applyRun: &dstack.Run{Name: model.Name, RunID: "run-3", ServiceURL: "/proxy/services/main/serviceurl/"},
	}

	got := reconcileSchedulableFixture(t, model, dc)

	if got.Status.ServiceURL != "/proxy/services/main/serviceurl/" {
		t.Fatalf("ServiceURL = %q, want the applied run's ServiceURL", got.Status.ServiceURL)
	}
}

// TestReconcile_InvalidSpec_ReportsSchedulableFalseAndDoesNotApply is F2:
// ValidateWithWarnings had zero non-test callers, so a spec whose holdTimeout
// outlived its own provisioningTimeout — a hold that can never be satisfied by
// a successful wake — reconciled happily and provisioned anyway.
func TestReconcile_InvalidSpec_ReportsSchedulableFalseAndDoesNotApply(t *testing.T) {
	model := pinnedColdModel("qwen-invalid-spec", nil)
	model.Spec.HoldTimeout = metav1.Duration{Duration: 40 * time.Minute}
	model.Spec.ProvisioningTimeout = metav1.Duration{Duration: 30 * time.Minute}

	dc := &schedulableDstackClient{backendConfigured: true, hasFleet: true}
	got := reconcileSchedulableFixture(t, model, dc)

	if dc.applied {
		t.Fatal("Apply ran on a spec that cannot work: a wake bought against contradictory deadlines is money spent on a hold that can never be satisfied")
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionSchedulable)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != squallv1alpha1.ReasonInvalidSpec {
		t.Fatalf("Schedulable = %+v, want False/InvalidSpec", cond)
	}
	if !strings.Contains(cond.Message, "holdTimeout") {
		t.Errorf("message = %q, want it to name the offending field", cond.Message)
	}
}

// TestReconcile_ValidSpec_StillSchedulable guards the other direction: the
// live Models all pass these rules, and a validation gate that rejected a
// working spec would strand real serving capacity.
func TestReconcile_ValidSpec_StillSchedulable(t *testing.T) {
	model := pinnedColdModel("qwen-valid-spec", nil)
	model.Spec.HoldTimeout = metav1.Duration{Duration: 20 * time.Minute}
	model.Spec.ProvisioningTimeout = metav1.Duration{Duration: 30 * time.Minute}

	// This path really does Apply, so the fake has to answer with a run.
	dc := &schedulableDstackClient{
		backendConfigured: true, hasFleet: true,
		applyRun: &dstack.Run{Name: model.Name, RunID: "run-1", Status: "provisioning", Replicas: 1},
	}
	got := reconcileSchedulableFixture(t, model, dc)

	if !dc.applied {
		t.Fatal("Apply did not run on a valid spec: the validation gate must not veto a Model that satisfies every rule")
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, squallv1alpha1.ConditionSchedulable)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Schedulable = %+v, want True — this spec satisfies every rule", cond)
	}
}
