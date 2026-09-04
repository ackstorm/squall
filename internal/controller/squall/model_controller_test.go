// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

// waitForCondition is this suite's one bounded, explicit-failure
// "Eventually" helper: every envtest assertion below that depends on the
// controller's asynchronous reconcile loop goes through it rather than a
// raw, unbounded polling loop.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("timed out after %s waiting for: %s", timeout, what)
	}
}

// wakeViolation is what sampleWakeProgress reports the instant it detects
// the AC4 burst has stopped coalescing correctly — FIX 3: a regression here
// must fail with a concrete diagnosis and elapsed time, not merely "timed
// out waiting for Waking", which teaches nothing and invites a later "fix"
// that just raises the timeout.
type wakeViolation struct {
	message string
}

// sampleWakeProgress polls every 50ms from start, bounded by stop (closed
// via the caller's t.Cleanup), and never calls any *testing.T method itself
// — only the caller's own goroutine acts on what it reports, so there is no
// write to t after the test returns and no goroutine leak. It reports at
// most one violation, the instant either of two things becomes true:
//
//   - ApplyCount(name) > 1: the coalescing guarantee itself broke — the
//     literal case the code review named.
//   - Exactly one Apply has landed in dstack (the run really is up) but
//     status.Phase still isn't Waking/Ready after gracePeriod.
//
// The second check earns its place empirically, not speculatively: mutating
// phase.go's final return to Action{Apply: true, ...} (Task 6.1's Decide,
// the "replicas up, not yet Ready, no-op" case) does NOT drive ApplyCount
// past 1 — that branch never sets Action.Current, so every reconcile
// after the first success re-Applies with a nil anchor, which permanently
// CAS-conflicts against dstack's now-nonzero deployment_num (verified via
// this exact mutation: 36/36 subsequent Reconciler errors were
// dstack.ErrResourceChanged, ApplyCount stayed at 1 throughout). The
// reconciler never reaches Status().Update again, so phase wedges below
// Waking forever — a livelock the first check cannot see by construction.
func sampleWakeProgress(name string, phaseOf func() squallv1alpha1.ModelPhase, start time.Time, gracePeriod time.Duration, stop <-chan struct{}) (<-chan wakeViolation, *sync.WaitGroup) {
	violation := make(chan wakeViolation, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		report := func(msg string) {
			select {
			case violation <- wakeViolation{message: msg}:
			default:
			}
		}
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				elapsed := time.Since(start)
				got := dstackFake.ApplyCount(runNameIn(managedNamespace, name))
				if got > 1 {
					report(fmt.Sprintf("ApplyCount(%q) hit %d at t=%s, want <= 1 (AC4 violated: reconcile storm re-applying instead of coalescing)", name, got, elapsed))
					return
				}
				if got == 1 && elapsed > gracePeriod {
					phase := phaseOf()
					if phase != squallv1alpha1.ModelPhaseWaking && phase != squallv1alpha1.ModelPhaseReady {
						report(fmt.Sprintf("dstack run applied (ApplyCount(%q)=1) but status.Phase is still %q at t=%s, want Waking (AC4 violated: reconcile is stuck failing to journal the wake, likely a CAS-conflict loop)", name, phase, elapsed))
						return
					}
				}
			}
		}
	}()
	return violation, &wg
}

// waitForConditionOrViolation is waitForCondition plus a race against a
// wakeViolation sampler: whichever happens first — cond() becomes true, a
// violation is reported, or timeout elapses — decides the outcome. Only
// this function (running on the test's own goroutine) ever calls a
// *testing.T method; sampleWakeProgress's goroutine never does.
func waitForConditionOrViolation(t *testing.T, timeout time.Duration, cond func() bool, violation <-chan wakeViolation, what string) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case v := <-violation:
			t.Fatalf("%s", v.message)
		case <-deadline:
			if cond() {
				return
			}
			t.Fatalf("timed out after %s waiting for: %s", timeout, what)
		case <-ticker.C:
			if cond() {
				return
			}
		}
	}
}

// TestReconcile_ColdModelWithDemand_WakesOnce is Task 6.2: create a Model
// with no demand (settles at Asleep, zero Applies), then patch in the
// demand annotation and assert exactly one Apply reaches the fake,
// status.phase becomes Waking, and status.runId is journaled.
func TestReconcile_ColdModelWithDemand_WakesOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const name = "qwen-6-2"
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: managedNamespace},
		Spec:       exampleModelSpec(),
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	key := client.ObjectKeyFromObject(model)

	// Let the controller settle at Asleep before patching demand, so the
	// single Apply asserted below is unambiguously attributable to the
	// patch rather than a race with the CR's own creation reconcile.
	waitForCondition(t, 5*time.Second, func() bool {
		got := &squallv1alpha1.Model{}
		if err := k8sClient.Get(ctx, key, got); err != nil {
			return false
		}
		return got.Status.Phase == squallv1alpha1.ModelPhaseAsleep
	}, "status.phase == Asleep before any demand")

	if got := dstackFake.ApplyCount(runNameIn(managedNamespace, name)); got != 0 {
		t.Fatalf("ApplyCount(%q) = %d before demand, want 0", name, got)
	}

	original := model.DeepCopy()
	patched := original.DeepCopy()
	patched.Annotations = map[string]string{squallv1alpha1.DemandAnnotation: time.Now().UTC().Format(time.RFC3339)}
	if err := k8sClient.Patch(ctx, patched, client.MergeFrom(original)); err != nil {
		t.Fatalf("patch demand annotation: %v", err)
	}

	var final squallv1alpha1.Model
	waitForCondition(t, 5*time.Second, func() bool {
		if err := k8sClient.Get(ctx, key, &final); err != nil {
			return false
		}
		return final.Status.Phase == squallv1alpha1.ModelPhaseWaking || final.Status.Phase == squallv1alpha1.ModelPhaseReady
	}, "status.phase == Waking or Ready after demand patch")

	if final.Status.RunID == "" {
		t.Error("status.runId not journaled")
	}
	if got := dstackFake.ApplyCount(runNameIn(managedNamespace, name)); got != 1 {
		t.Errorf("ApplyCount(%q) = %d, want exactly 1", name, got)
	}
}

// TestReconcile_FiftyConcurrentDemandPatches_WakesOnce is Task 6.3 (AC4):
// 50 concurrent demand patches against a cold Model must still produce
// exactly one dstack Apply. The coalescing mechanism under test is
// controller-runtime's real per-object-key workqueue (this package does
// not reimplement it) plus Decide's own idempotent level-trigger, which is
// what stops every reconcile after the first settled one from re-applying
// once dstack reports replicas > 0.
func TestReconcile_FiftyConcurrentDemandPatches_WakesOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const name = "qwen-6-3"
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: managedNamespace},
		Spec:       exampleModelSpec(),
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	key := client.ObjectKeyFromObject(model)

	waitForCondition(t, 5*time.Second, func() bool {
		got := &squallv1alpha1.Model{}
		if err := k8sClient.Get(ctx, key, got); err != nil {
			return false
		}
		return got.Status.Phase == squallv1alpha1.ModelPhaseAsleep
	}, "status.phase == Asleep before the burst")

	// FIX 3: sample wake progress throughout the burst so a coalescing
	// regression reports a concrete diagnosis and elapsed time instead of
	// the pre-existing failure mode — the whole test hanging to its 10s
	// waitForCondition timeout, which teaches nothing. See
	// sampleWakeProgress's doc comment for why this needs two detection
	// branches, not just "ApplyCount > 1".
	phaseOf := func() squallv1alpha1.ModelPhase {
		got := &squallv1alpha1.Model{}
		if err := k8sClient.Get(ctx, key, got); err != nil {
			return ""
		}
		return got.Status.Phase
	}
	sampleStop := make(chan struct{})
	violation, samplerWG := sampleWakeProgress(name, phaseOf, time.Now(), 3*time.Second, sampleStop)
	t.Cleanup(func() {
		close(sampleStop)
		samplerWG.Wait()
	})

	const burst = 50
	original := model.DeepCopy()
	var wg sync.WaitGroup
	errs := make(chan error, burst)
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			patched := original.DeepCopy()
			patched.Annotations = map[string]string{squallv1alpha1.DemandAnnotation: time.Now().UTC().Format(time.RFC3339)}
			// client.MergeFrom diffs against `original` (captured once,
			// before the burst) and does not send a resourceVersion
			// precondition, so all 50 of these are independent, conflict-free
			// PATCH requests — deliberately, since a coalesced demand signal
			// is exactly this: many callers patching the same intent, not a
			// single caller retrying.
			if err := k8sClient.Patch(ctx, patched, client.MergeFrom(original)); err != nil {
				errs <- fmt.Errorf("patch %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	waitForConditionOrViolation(t, 10*time.Second, func() bool {
		return phaseOf() == squallv1alpha1.ModelPhaseWaking || phaseOf() == squallv1alpha1.ModelPhaseReady
	}, violation, "status.phase == Waking or Ready after the 50-patch burst")

	// Let any remaining queued/coalesced reconciles drain before the final
	// count — Decide's level-trigger (Replicas > 0 -> no-op) must hold for
	// every one of them. Still racing the sampler, so a late storm fails
	// fast here too rather than only being caught by the assertion below.
	select {
	case v := <-violation:
		t.Fatalf("%s (while draining)", v.message)
	case <-time.After(500 * time.Millisecond):
	}

	if got := dstackFake.ApplyCount(runNameIn(managedNamespace, name)); got != 1 {
		t.Fatalf("ApplyCount(%q) = %d, want exactly 1 (AC4: 50 concurrent demand signals must coalesce into one wake)", name, got)
	}
}

// barrierClient wraps a dstack.Client and rendezvous-blocks in Get: both
// racing Reconcile calls are guaranteed to have observed dstack's prior
// state before either proceeds to Apply, deterministically reproducing
// the F18 CAS conflict instead of depending on goroutine-scheduling luck.
type barrierClient struct {
	dstack.Client
	barrier *sync.WaitGroup
}

func (b *barrierClient) Get(ctx context.Context, name string) (*dstack.Run, error) {
	run, err := b.Client.Get(ctx, name)
	b.barrier.Done()
	b.barrier.Wait()
	return run, err
}

// applyOutcomeSpy wraps a dstack.Client and records the ground truth of
// what Apply itself returned, independent of what the caller's Reconcile
// then does with that return value (FIX 4). This is needed because the
// named mutation under test — Reconcile swallowing Apply's error — would
// otherwise erase the only signal that told winner and loser apart.
type applyOutcomeSpy struct {
	dstack.Client
	mu  sync.Mutex
	err error
}

func (s *applyOutcomeSpy) Apply(ctx context.Context, req dstack.ApplyRequest) (*dstack.Run, error) {
	run, err := s.Client.Apply(ctx, req)
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	return run, err
}

// TestReconcile_ConcurrentFlips_LoserFailsLoudlyAndRequeues is Task 6.4
// (AC13): two Reconcile calls racing on the same cold Model must produce
// exactly one dstack.ErrResourceChanged (the F18 CAS loser failing loudly,
// never retried with force — ApplyRequest has no Force field to retry
// with) and exactly one success, with the loser's Reconcile returning that
// error and a zero ctrl.Result — precisely the signal controller-runtime
// treats as "requeue", rather than swallowing the error or resolving it by
// any means other than a fresh reconcile.
//
// Both reconciles are invoked directly (not through the shared manager)
// against manualNamespace, which the shared manager deliberately does not
// watch (see suite_test.go) — this test supplies its own, deterministic
// race instead of relying on scheduler luck inside the real workqueue.
func TestReconcile_ConcurrentFlips_LoserFailsLoudlyAndRequeues(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const name = "qwen-6-4"
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   manualNamespace,
			Annotations: map[string]string{squallv1alpha1.DemandAnnotation: time.Now().UTC().Format(time.RFC3339)},
			Finalizers:  []string{ModelFinalizer}, // pre-seeded: Reconcile's finalizer-add pass is exercised elsewhere (Task 8.0).
		},
		Spec: exampleModelSpec(),
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	var barrier sync.WaitGroup
	barrier.Add(2)

	type outcome struct {
		res          ctrl.Result
		err          error
		applyErr     error
		updateCalled bool
	}
	outcomes := make(chan outcome, 2)

	for i := 0; i < 2; i++ {
		go func() {
			applySpy := &applyOutcomeSpy{Client: &barrierClient{Client: dstackClient, barrier: &barrier}}
			statusSpy := &statusSpyClient{Client: k8sClient}
			r := &ModelReconciler{
				Client:       statusSpy,
				DstackClient: applySpy,
			}
			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)})
			applySpy.mu.Lock()
			applyErr := applySpy.err
			applySpy.mu.Unlock()
			outcomes <- outcome{res: res, err: err, applyErr: applyErr, updateCalled: statusSpy.updateCalled}
		}()
	}

	var got [2]outcome
	for i := range got {
		got[i] = <-outcomes
	}

	// Ground truth for who actually lost the F18 CAS, read from a spy on
	// dstack.Client.Apply itself rather than from what Reconcile's err
	// return says — this must hold regardless of what Reconcile then does
	// with that return value.
	var applySuccesses, applyFailures int
	for _, o := range got {
		switch {
		case o.applyErr == nil:
			applySuccesses++
		case errors.Is(o.applyErr, dstack.ErrResourceChanged):
			applyFailures++
		default:
			t.Fatalf("unexpected dstack Apply error: %v", o.applyErr)
		}
	}
	if applySuccesses != 1 || applyFailures != 1 {
		t.Fatalf("got %d dstack Apply successes and %d ErrResourceChanged failures, want exactly 1 of each (AC13)", applySuccesses, applyFailures)
	}

	// FIX 4: assert directly, per goroutine, that the CAS loser's Reconcile
	// issued zero Status().Update calls — not inferred from the CR's final
	// state, which cannot tell "never wrote" apart from "wrote and happened
	// to lose a race that left the same values behind". The mutation this
	// guards against is Reconcile swallowing Apply's error and falling
	// through to Status().Update anyway.
	for _, o := range got {
		if errors.Is(o.applyErr, dstack.ErrResourceChanged) && o.updateCalled {
			t.Fatalf("the CAS loser's Reconcile issued a Status().Update call; Apply's error must short-circuit Reconcile before Status().Update is ever reached, never be swallowed")
		}
	}

	var successes, failures int
	for _, o := range got {
		switch {
		case o.err == nil:
			successes++
			if o.res != (ctrl.Result{}) {
				t.Errorf("winning Reconcile returned %+v, want zero Result", o.res)
			}
		case errors.Is(o.err, dstack.ErrResourceChanged):
			failures++
			if o.res != (ctrl.Result{}) {
				t.Errorf("losing Reconcile returned %+v alongside its error, want zero Result so controller-runtime requeues rather than the caller resolving it another way", o.res)
			}
		case apierrors.IsConflict(o.err):
			t.Fatalf("Reconcile returned a status-subresource 409 (%v): the loser attempted a status write it should never have reached — Apply's error must short-circuit Reconcile before Status().Update, not be swallowed", o.err)
		default:
			t.Fatalf("unexpected Reconcile error: %v", o.err)
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("got %d successes and %d dstack.ErrResourceChanged failures, want exactly 1 of each (AC13)", successes, failures)
	}

	if got := dstackFake.ApplyCount(runNameIn(manualNamespace, name)); got != 1 {
		t.Errorf("ApplyCount(%q) = %d, want exactly 1 (the loser's Apply must be rejected, not double-applied)", name, got)
	}

	final := &squallv1alpha1.Model{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(model), final); err != nil {
		t.Fatalf("get Model: %v", err)
	}
	if final.Status.Phase != squallv1alpha1.ModelPhaseWaking {
		t.Errorf("status.phase = %q, want Waking (only the winner writes status; the loser returned before reaching Status().Update)", final.Status.Phase)
	}
	if final.Status.RunID == "" {
		t.Error("status.runId not journaled by the winner")
	}
}

// spyApplyClient wraps a dstack.Client and records the last ApplyRequest it
// sent, so a test can assert exactly what CAS token reached dstack
// regardless of whether the call itself succeeded or failed.
type spyApplyClient struct {
	dstack.Client
	mu        sync.Mutex
	lastApply dstack.ApplyRequest
}

func (s *spyApplyClient) Apply(ctx context.Context, req dstack.ApplyRequest) (*dstack.Run, error) {
	s.mu.Lock()
	s.lastApply = req
	s.mu.Unlock()
	return s.Client.Apply(ctx, req)
}

// TestReconcile_StaleCachedStatus_ApplyUsesFreshlyObservedDeploymentNum is
// FIX 1 (F18, §5.2, AC13): the CAS token Apply sends must be the
// DeploymentNum this reconcile actually observed in dstack right now —
// never model.Status.DeploymentNum as last journaled. Every other test in
// this package is a cold start where the cached status and the fresh
// observation are both zero and can never diverge; this test manufactures
// the divergence directly: an existing run whose dstack deployment_num has
// advanced out-of-band beyond what the CR's status last recorded, then a
// second wake.
//
// The mutation this guards against: wiring Apply's Current from a Run
// synthesised out of model.Status (the stale cached DeploymentNum) instead
// of action.Current (Observed.Run, freshly read this pass). A stale anchor
// either spuriously conflicts (as it does here) or, worse, coincides with a
// wrong assumption about current state and defeats the CAS entirely — the
// failure mode AC13 exists to prevent: a second run minted for a model
// already alive, a duplicate GPU and a duplicate bill.
func TestReconcile_StaleCachedStatus_ApplyUsesFreshlyObservedDeploymentNum(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const name = "qwen-6-5"
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  manualNamespace,
			Finalizers: []string{ModelFinalizer}, // pre-seeded: Reconcile's finalizer-add pass is exercised elsewhere (Task 8.0).
		},
		Spec: exampleModelSpec(),
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	// Establish an existing run and advance it out-of-band, exactly as an
	// external actor (not this reconciler) would: apply once to mint it
	// (deployment_num 1, awake), then flip it back asleep (deployment_num
	// 2). Neither call goes through Reconcile, so nothing has journaled
	// these generations into status yet.
	run, err := dstackClient.Apply(ctx, dstack.ApplyRequest{Name: runNameIn(manualNamespace, name), Replicas: 1})
	if err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	if _, err := dstackClient.Apply(ctx, dstack.ApplyRequest{Name: runNameIn(manualNamespace, name), Replicas: 0, Current: run}); err != nil {
		t.Fatalf("seed flip-to-asleep Apply: %v", err)
	}
	freshRun, err := dstackClient.Get(ctx, runNameIn(manualNamespace, name))
	if err != nil {
		t.Fatalf("get seeded run: %v", err)
	}
	if freshRun.DeploymentNum != 2 {
		t.Fatalf("seeded DeploymentNum = %d, want 2 (sanity check on the fake's own increment behaviour)", freshRun.DeploymentNum)
	}

	// Journal a STALE status by hand: RunID matches, but DeploymentNum is
	// one generation behind the dstack state above — the CR's own last
	// status write never saw the out-of-band flip.
	toUpdate := &squallv1alpha1.Model{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(model), toUpdate); err != nil {
		t.Fatalf("get Model before status seed: %v", err)
	}
	toUpdate.Status.Phase = squallv1alpha1.ModelPhaseAsleep
	toUpdate.Status.RunID = freshRun.RunID
	toUpdate.Status.DeploymentNum = int64(freshRun.DeploymentNum) - 1 // stale on purpose
	if err := k8sClient.Status().Update(ctx, toUpdate); err != nil {
		t.Fatalf("seed stale status: %v", err)
	}

	// Second wake: patch demand, then reconcile directly (manualNamespace
	// is not watched by the shared manager — see suite_test.go) through a
	// spy that records exactly what Current anchor reached dstack.
	demanded := toUpdate.DeepCopy()
	demanded.Annotations = map[string]string{squallv1alpha1.DemandAnnotation: time.Now().UTC().Format(time.RFC3339)}
	if err := k8sClient.Patch(ctx, demanded, client.MergeFrom(toUpdate)); err != nil {
		t.Fatalf("patch demand: %v", err)
	}

	spy := &spyApplyClient{Client: dstackClient}
	r := &ModelReconciler{Client: k8sClient, DstackClient: spy}
	_, reconcileErr := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)})

	spy.mu.Lock()
	gotCurrent := spy.lastApply.Current
	spy.mu.Unlock()

	if gotCurrent == nil || gotCurrent.DeploymentNum != freshRun.DeploymentNum {
		t.Fatalf("Apply's Current.DeploymentNum = %v, want %d (the freshly observed dstack state), not %d (the stale cached status.DeploymentNum); Reconcile error was: %v",
			gotCurrent, freshRun.DeploymentNum, toUpdate.Status.DeploymentNum, reconcileErr)
	}
	if reconcileErr != nil {
		t.Fatalf("Reconcile: %v (CASing against the freshly observed DeploymentNum matches current dstack state and this Apply should succeed)", reconcileErr)
	}
}
