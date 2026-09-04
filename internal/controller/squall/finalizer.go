// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"errors"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

// ModelFinalizer blocks a Model's deletion until the drain-first teardown
// sequence (§5.2) has run to completion: an idempotent Draining status
// write, a bounded in-flight drain, dstack.Delete, and only then finalizer
// removal. Invented; the spec names no finalizer key (block 7+8 plan §8,
// docs/references/deviations-and-findings.md).
const ModelFinalizer = "squall.ackstorm.ai/model-cleanup"

// reconcileDelete is the drain-first finalizer sequence (§5.2, Task 8.1) for
// a Model with a non-zero DeletionTimestamp. Every step is safe to replay
// from scratch on a crash: the phase write is idempotent, the drain
// evidence is re-gathered fresh every pass (never assumed from a prior
// pass), and dstack.Delete's ErrNotFound is success, not failure.
//
// §5.2's "deregister from discovery -> stop accepting" (steps 1+2) has no
// separate RPC to call — squall never calls the LiteLLM API (AC18) and
// discovery is pull-based — so both collapse into the single Draining
// status write below (D5 in the deviations ledger).
func (r *ModelReconciler) reconcileDelete(ctx context.Context, model *squallv1alpha1.Model, runName string, now time.Time) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(model, ModelFinalizer) {
		// Nothing this controller ever guarded: an old CR predating the
		// finalizer, or a replay after the finalizer was already removed
		// and the object is merely awaiting garbage collection. Either
		// way there is nothing left to do.
		return ctrl.Result{}, nil
	}

	// T11: Draining must be observable BEFORE any destructive action, and
	// idempotently (never re-write the same value, so a replay is a no-op
	// here rather than a needless conflict-prone write).
	//
	// D110: Patch, never Status().Update — the same lesson D76/LIVE-1
	// already paid for in Reconcile. Update's whole-object optimistic lock
	// loses to squall-proxy's demand-annotation merge patches (every
	// ~500ms per replica while a request is held), and this particular
	// starvation is SELF-SUSTAINING: the proxy only stops holding and
	// signalling once it observes phase Draining, and Draining is
	// precisely the write being starved. A merge patch carries no
	// resourceVersion precondition and touches only the phase key.
	if model.Status.Phase != squallv1alpha1.ModelPhaseDraining {
		original := model.DeepCopy()
		model.Status.Phase = squallv1alpha1.ModelPhaseDraining
		if err := r.Status().Patch(ctx, model, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, fmt.Errorf("write Draining status for %q: %w", model.Name, err)
		}
	}

	run, err := r.DstackClient.Get(ctx, runName)
	switch {
	case errors.Is(err, dstack.ErrNotFound):
		run = nil
		// Nothing left to drain — a CR that never woke, or a crash-replay
		// after Delete already succeeded (T15). Drain is moot; fall
		// through to the idempotent Delete/finalizer-removal below.
	case err != nil:
		// T14: an unreadable dstack (or, via gatherActivity below, an
		// unreadable API server) must produce zero destructive actions —
		// returning the error here does exactly that: no Delete, no
		// finalizer removal, requeue and re-observe from scratch.
		return ctrl.Result{}, fmt.Errorf("observe dstack state for %q during teardown: %w", model.Name, err)
	default:
		// The deadline is anchored once, immutably, by the API server's
		// own DeletionTimestamp — never recomputed relative to "now" at
		// finalizer-add time, so it survives a crash/replay unchanged.
		pastDeadline := now.Sub(model.DeletionTimestamp.Time) > drainTimeoutOrDefault(model.Spec)

		if run.Replicas > 0 && !pastDeadline {
			// T12: bounded in-flight drain — never cut a live generation
			// while evidence is unclean and the deadline has not yet
			// passed. Evidence is re-gathered fresh every pass (Task
			// 7.1/7.2's gatherActivity), never assumed from a prior
			// reconcile.
			if !drainEvidenceClean(r.gatherActivity(ctx, model.Name, now)) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
		}
		// T13: past the deadline, or nothing running to drain — proceed
		// to Delete regardless of evidence. The drain wait is bounded,
		// unlike the sleep path's unconditional wait (block 7+8 plan §5.2:
		// "drain vs sleep evidence asymmetry").
	}

	// D56: dstack refuses to delete a run that is not already terminal —
	// HTTP 400 "Cannot delete active runs". Calling Delete on a live run
	// does not fail loudly and stop; it fails, gets retried, fails again,
	// and the rented instance bills forever while the CR sits in Draining.
	// Measured on Vast.ai 2026-08-27.
	//
	// So: stop first, then delete once terminal. Stop is issued at most
	// once per state — a terminal run is not re-stopped — and the requeue
	// is short because every second here is a second of GPU we are paying
	// for. Both steps treat ErrNotFound as success: the run being gone is
	// the outcome we want.
	if run != nil && !run.IsTerminal() {
		if err := r.DstackClient.Stop(ctx, runName); err != nil && !errors.Is(err, dstack.ErrNotFound) {
			return ctrl.Result{}, fmt.Errorf("stop dstack run for %q before delete: %w", model.Name, err)
		}
		// Re-observe rather than assume: the finalizer must not be removed
		// until dstack itself reports the run terminal, or we would drop
		// our only handle on a machine that is still running.
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if err := r.DstackClient.Delete(ctx, runName); err != nil && !errors.Is(err, dstack.ErrNotFound) {
		return ctrl.Result{}, fmt.Errorf("delete dstack run for %q: %w", model.Name, err)
	}

	controllerutil.RemoveFinalizer(model, ModelFinalizer)
	if err := r.Update(ctx, model); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer for %q: %w", model.Name, err)
	}

	if r.AgeMetrics != nil {
		r.AgeMetrics.Forget(model.Namespace, model.Name)
	}
	if r.PriceMetrics != nil {
		r.PriceMetrics.Forget(model.Namespace, model.Name)
	}
	if r.UncontrolledMetrics != nil {
		r.UncontrolledMetrics.Forget(model.Namespace, model.Name)
	}
	if r.IdleMetrics != nil {
		r.IdleMetrics.Forget(model.Namespace, model.Name)
	}
	if r.OperationalMetrics != nil {
		r.OperationalMetrics.RecordTransition(model.Namespace, model.Name, metricBackend(model), "delete")
		r.OperationalMetrics.Forget(model.Namespace, model.Name)
	}

	return ctrl.Result{}, nil
}

// defaultDrainTimeout mirrors the CRD's admission default (D123, the
// spec's own §5.1 example value). The admission default covers a CR that
// OMITS the field; this constant covers the value it cannot: metav1.
// Duration is a struct, so `omitempty` never drops it, and every typed
// client (and any explicit `drainTimeout: "0s"`) sends a present zero the
// API server rightly leaves alone. A zero deadline made `pastDeadline`
// true on the finalizer's FIRST pass — T12's bounded drain never ran and a
// delete cut any generation in flight immediately.
const defaultDrainTimeout = 120 * time.Second

// drainTimeoutOrDefault reads spec.drainTimeout with the zero case folded
// to the same default admission applies. Zero cannot mean "no drain": the
// spec's own asymmetry (§5.2) is that the drain is BOUNDED, not absent.
func drainTimeoutOrDefault(spec squallv1alpha1.ModelSpec) time.Duration {
	if spec.DrainTimeout.Duration <= 0 {
		return defaultDrainTimeout
	}
	return spec.DrainTimeout.Duration
}

// drainEvidenceClean reports whether the drain gate may proceed to Delete.
// Complete && AllIdle includes replicas with no key: Begin inserts the key
// before upstream work, so no key proves nothing is in flight. A replica that
// does not answer is still incomplete and blocks teardown.
func drainEvidenceClean(activity *ActivityEvidence) bool {
	return activity != nil && activity.Complete && activity.AllIdle
}
