// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/clock"
	"github.com/ackstorm/squall/internal/dstack"
	"github.com/ackstorm/squall/internal/metrics"
)

// ModelReconciler reconciles a Model object: the 0<->1 replica flip in both
// directions (§5.2, §6) and drain-first teardown (§5.2, Phase 8).
type ModelReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// DstackClient is the narrow dstack API surface (internal/dstack).
	// Every real caller and every test must set it; a nil value is a
	// programmer error, not a supported "disabled" mode.
	DstackClient dstack.Client

	// Clock abstracts wall-clock time (internal/clock) so Decide's now
	// parameter is never read from time.Now() directly inside a running
	// reconcile. A nil Clock defaults to clock.RealClock{} — see now().
	Clock clock.Clock

	// ProxyService names the one cluster-wide proxy Service whose
	// EndpointSlices gatherActivity enumerates for §6's idle evidence (OQ5:
	// there is one proxy Deployment per cluster, not one per Model, so
	// this is a field set once at startup rather than a CRD field). The
	// zero value means "not configured": gatherActivity then always
	// returns nil ("not evaluated"), so an on-demand Model can never be
	// read as idle before this is wired up.
	ProxyService types.NamespacedName

	// SSHKeyNamespace is where EnsureSSHKey keeps squall's replica SSH
	// keypair. It must be a namespace squall-proxy can READ, because the
	// proxy is what dials replicas with the private half — so it defaults to
	// ProxyService.Namespace, which is already the proxy's own namespace.
	// Empty with no ProxyService configured means no key is minted at all,
	// and every Apply goes out without one: the direct data path is simply
	// not offered, and dstack's service proxy carries traffic as before.
	SSHKeyNamespace string

	// HTTPClient issues the per-replica ActivityPath queries. A nil value
	// defaults to http.DefaultClient (see activityHTTPClient).
	HTTPClient *http.Client

	// AgeMetrics and PriceMetrics record the §10/AC19 declared/observed
	// gauge pairs (internal/metrics) on every reconcile. Both are
	// optional (nil-safe): a nil collector simply means that pair is not
	// exported, e.g. in tests that don't care about metrics.
	AgeMetrics          *metrics.ModelAgeCollector
	PriceMetrics        *metrics.ModelPriceCollector
	UncontrolledMetrics *metrics.UncontrolledCollector
	IdleMetrics         *metrics.IdleCollector
	OperationalMetrics  *metrics.ModelOperationalCollector

	// IdleRequeueInterval is how often Reconcile re-evaluates an on-demand
	// Model (spec.MinReplicas == 0) that is awake (observed.Run.Replicas >
	// 0) with nothing to actuate this pass. Without this, sleepDue's §6
	// idle-timeout check (phase.go) is unreachable on a live manager: this
	// controller is level-triggered off Model watch events only, and once
	// demand stops nothing else ever mutates the Model to trigger a fresh
	// reconcile (found while wiring Phase 11's e2e loop — envtest never
	// caught it because those tests invoke Reconcile directly rather than
	// relying on the manager to fire it). A zero value defaults to 15s;
	// see idleRequeueInterval().
	IdleRequeueInterval time.Duration

	// ServedModels reads a Ready replica's own GET /v1/models (D65) to
	// confirm it answers to the name spec.model asked for. A nil value
	// (the zero value, and every test that doesn't set it) simply skips
	// the check — see its call site's own doc comment for why that is
	// the fail-open, not fail-closed, default.
	ServedModels ServedModelReader
}

// idleRequeueInterval returns r.IdleRequeueInterval, defaulting to 15s.
func (r *ModelReconciler) idleRequeueInterval() time.Duration {
	if r.IdleRequeueInterval <= 0 {
		return 15 * time.Second
	}
	return r.IdleRequeueInterval
}

func (r *ModelReconciler) now() time.Time {
	if r.Clock == nil {
		return clock.RealClock{}.Now()
	}
	return r.Clock.Now()
}

// +kubebuilder:rbac:groups=squall.ackstorm.ai,resources=models,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=squall.ackstorm.ai,resources=models/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=squall.ackstorm.ai,resources=models/finalizers,verbs=update
//
// gatherActivity below lists EndpointSlices, but
// controller-runtime's default Client serves typed Get/List through a
// cache backed by an informer — list+watch are what the informer itself
// needs to populate that cache, not just the one verb the source calls.
// get-only was found insufficient live against a real kind RBAC-enforced
// API server (envtest does not enforce RBAC, so this gap was invisible
// there): the informer's reflector failed closed with "endpointslices is
// forbidden", which folds into gatherActivity's Complete: false path
// (safe — see its own doc comment) but permanently, silently defeating
// §6 idle-sleep for every real deployment on this RBAC.
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// D63: spec.secretEnv resolution. get only for reads — the controller
// reads one referenced key and never lists or watches Secrets, so a
// compromised controller cannot enumerate the namespace's credentials.
// create is for EnsureSSHKey's one squall-owned keypair Secret (D125: the
// hand-written Helm Role had it, this marker did not, so the generated
// kustomize RBAC silently lost the SSH fast path — EnsureSSHKey fails
// open, so the only symptom was permanently slower routing).
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create

// Reconcile drives one Model toward Decide's prescribed phase (Task 6.1,
// phase.go). It is level-triggered and idempotent (§5.2): every reconcile
// re-reads dstack state fresh via DstackClient.Get, so a duplicate demand
// patch, a watch replay, or a requeue after a crash all converge on the
// same single actuation rather than compounding it (AC4, AC13, PoC 3).
//
// On a dstack error — including dstack.ErrResourceChanged, the losing side
// of the F18 CAS race — Reconcile returns the error without writing status
// and without retrying within this call. squall's ApplyRequest has no
// Force field to retry with in the first place (§5.2, AC13); returning the
// error is exactly controller-runtime's requeue-with-backoff path, so the
// next attempt re-observes dstack from scratch rather than compounding a
// stale decision.
func (r *ModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) { //nolint:gocyclo // reconciliation deliberately sequences independent safety gates
	logger := log.FromContext(ctx)

	var model squallv1alpha1.Model
	if err := r.Get(ctx, req.NamespacedName, &model); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	now := r.now()

	// F1: resolve the dstack run's name BEFORE anything reads or writes it,
	// deletion included — the finalizer must stop the run that exists, not
	// the one this version would have named.
	runName := runNameFor(&model)

	// Deletion's drain-first finalizer sequence is a separate code path
	// (Task 8.1), driven by the API server's deletion timestamp rather
	// than by Decide — see phase.go's doc comment and finalizer.go.
	if !model.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &model, runName, now)
	}

	// Task 8.0: a live, non-deleting Model must carry ModelFinalizer
	// before anything else happens, so a Delete issued the instant after
	// this reconcile is guaranteed to hit the drain-first path above
	// rather than completing un-drained. Adding it here and returning
	// immediately (rather than falling through into the same pass) keeps
	// this Update() the only mutating call in this branch — the very next
	// reconcile, triggered by this Update's own watch event, resumes the
	// wake/sleep evaluation below with a fresh Get.
	if !controllerutil.ContainsFinalizer(&model, ModelFinalizer) {
		controllerutil.AddFinalizer(&model, ModelFinalizer)
		if err := r.Update(ctx, &model); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer for %q: %w", model.Name, err)
		}
		return ctrl.Result{}, nil
	}

	// LIVE-1: snapshot the object as read, BEFORE any status mutation below,
	// so the eventual status write can be a merge Patch computed against
	// this snapshot rather than an Update gated on model's resourceVersion
	// — see the Patch call at the end of this function for why.
	original := model.DeepCopy()
	uncontrolledSince := model.Status.UncontrolledSince.DeepCopy()

	model.Status.RunName = runName

	observed, err := r.observe(ctx, runName, model.Name, model.Spec, now)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("observe dstack state for %q: %w", model.Name, err)
	}

	// LIVE-2: reconcile status.runID/deploymentNum/serviceURL from whatever
	// THIS pass observed, not only from a run this pass happened to Apply.
	// Before this fix these three fields were written exclusively inside
	// the `action.Apply` block below (model.Status.RunID = run.RunID etc.),
	// so any single lost status write (LIVE-1) was PERMANENT amnesia: a
	// later, unrelated status write could still advance Phase to Ready
	// while runID/serviceURL stayed empty forever, even with a healthy
	// dstack run the whole time (live Vast.ai run, 2026-08-28). That is a
	// level-triggered-idempotence violation (this package's CLAUDE.md):
	// every pass that observes a fact must re-assert it, not assume a past
	// pass already wrote it down. This does NOT feed the F18 CAS — Decide
	// sets action.Current directly from observed.Run (phase.go), never from
	// these status fields, so nothing here can hand Apply a stale base.
	if observed.Run != nil {
		model.Status.RunID = observed.Run.RunID
		model.Status.DeploymentNum = int64(observed.Run.DeploymentNum)
		model.Status.ServiceURL = observed.Run.ServiceURL
		model.Status.Replica = replicaStatus(observed.Run.Replica)
	}
	demand := hasDemand(&model, now)
	metricUncontrolledSince := updateActivityStatus(&model, observed, demand, now)

	r.reconcileRunOwnership(&model, observed.Run, runName, logger)

	newPhase, action := Decide(observed, model.Status, model.Spec, demand, now)

	if action.Apply && action.Replicas > 0 {
		r.checkSchedulable(ctx, &model, &action, &newPhase, logger)
	}

	if action.Apply {
		applyEnv, sshKeyPub, err := r.applyEnvFor(ctx, &model, action)
		if err != nil {
			return ctrl.Result{}, err
		}
		r.recordProvisioningAttempt(&model, action)
		idle, hard := applyDurationsFor(&model, action)
		run, err := r.DstackClient.Apply(ctx, dstack.ApplyRequest{
			Name:     runName,
			Replicas: action.Replicas,
			Image:    model.Spec.Image,
			Port:     enginePort(model.Spec.Engine),
			Probe:    engineProbe(model.Spec),
			// F1's second half: stamp WHO this run is for. The run name is
			// stable across a Model being deleted and recreated, so without
			// this there is nothing to tell a surviving run apart from one
			// this Model minted (see ownership.go).
			Env:          applyEnv,
			Args:         engineCommands(model.Spec, model.Name),
			Resources:    engineResources(model.Spec.Resources),
			Placement:    enginePlacement(model.Spec.Placement),
			Current:      action.Current,
			SSHKeyPub:    sshKeyPub,
			IdleDuration: idle,
			MaxDuration:  hard,
		})
		if err != nil {
			r.recoverFromOverrideRefusal(ctx, logger, &model, original, action, runName, err)
			return ctrl.Result{}, fmt.Errorf("apply dstack run for %q: %w", model.Name, err)
		}
		model.Status.RunID = run.RunID
		model.Status.DeploymentNum = int64(run.DeploymentNum)
		// D25's controller half: this is what makes status.serviceURL live
		// at all — Task 4's D65 check reads it to reach a replica's own GET
		// /v1/models. It is NOT the only writer (LIVE-2 correction: an
		// earlier version of this comment claimed it was): the block above
		// reconciles the same field from observed.Run on every pass, Apply
		// or not. This assignment stays because `run` here is the FRESH
		// post-Apply response, which is strictly newer than observed.Run —
		// e.g. the mint-a-fresh-run case has observed.Run == nil, so the
		// block above never ran this pass at all.
		model.Status.ServiceURL = run.ServiceURL

		// D65: action.Current == nil is F20's mint-a-fresh-run case (a cold
		// first wake or a Dead recreate) — a run with a new identity, whose
		// replica hasn't been asked anything yet. A prior run generation's
		// served-model answer says nothing about this one, so it must be
		// forgotten here rather than surviving as stale evidence until the
		// next Ready reconcile re-verifies it (this IS the wherever-RunID-
		// changes clear; RunID itself has no separate "cleared" state to
		// key off, since it is always immediately overwritten by the new
		// run's id in the same act, above).
		//
		// The condition must be forgotten alongside the field (I3, block 2
		// review follow-up): the verification gate below now keys off
		// ConditionServedModelVerified == True, not status.servedModel == ""
		// (fixing I3's latch), so a stale True left over from the DEAD run
		// would silently skip verifying the new one.
		if action.Current == nil {
			model.Status.ServedModel = ""
			// D112: ForwardModel is the field the proxy actually rewrites
			// request bodies to, and it must be forgotten with its sibling —
			// a reclaimed run's id left here makes the proxy rewrite every
			// request to a name the NEW run may not serve, and each engine
			// 400 is then charged to the healthy replica's failure count.
			model.Status.ForwardModel = ""
			meta.RemoveStatusCondition(&model.Status.Conditions, squallv1alpha1.ConditionServedModelVerified)
			// A new run has never been judged. Inheriting the previous run's
			// health verdict would be a statement about a different machine.
			meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
				Type: squallv1alpha1.ConditionHealthy, Status: metav1.ConditionTrue,
				Reason: squallv1alpha1.ReasonHealthy,
			})
		}

		// §5.2: journal the provisioningTimeout anchor in the same act as the
		// 0->1 actuation — only the actuation site knows the moment. It is set
		// ONCE per wake and must not be rewritten on later reconciles of the
		// same wake, or the deadline would never expire. Task 8.3 consumes it;
		// this block implements no timeout behaviour.
		if action.Replicas > 0 && model.Status.WakeStartedAt == nil {
			model.Status.WakeStartedAt = &metav1.Time{Time: now}
		}
		if action.Replicas == 0 {
			model.Status.WakeStartedAt = nil
			model.Status.UncontrolledSince = nil
		}
	}

	if action.Alarm {
		// F20: an uncommanded death. Phase 10 wires a real alert; logging
		// here at least keeps Dead->Recreating from being a silent
		// transition in the meantime.
		//
		// The two outcomes must not share a message. Decide answers a death
		// with NO demand as Dead + Alarm and NO Apply (phase.go): correct —
		// nothing is asking for the Model, so recreating it would buy a GPU
		// for nobody. But this line claimed "recreating" for that case too,
		// and on 2026-08-29 it repeated that claim once every ~9h (the
		// informer's resync, the only thing that reconciles a Dead Model
		// with no demand) for two days while status.runId never changed.
		// Reading the log, the controller looked broken; it was not, it was
		// waiting. Say which one happened.
		if action.Apply {
			logger.Info("uncommanded dstack run death detected, recreating",
				"model", model.Name, "priorRunId", model.Status.RunID)
		} else {
			logger.Info("uncommanded dstack run death detected; no demand, NOT recreating until a request arrives",
				"model", model.Name, "priorRunId", model.Status.RunID)
		}
	}

	if action.Throttled {
		// D163. Deliberately V(1): there IS demand and squall IS still
		// trying, so at default verbosity this is not news — and it repeats
		// every reconcile, which is exactly how the original loop drowned
		// the real cause in log noise.
		logger.V(1).Info("provisioning failed at the backend; pacing the next recreate",
			"model", model.Name, "priorRunId", model.Status.RunID,
			"nextAttemptAt", action.ThrottledUntil.UTC().Format(time.RFC3339))
	}
	reportProvisioningCondition(&model, observed.ProvisioningFailure, newPhase)

	if action.Unhealthy {
		// LOUD. This spent money and then stopped spending it; an operator who
		// finds a Model Asleep must be able to see it was pushed, not that it
		// simply went quiet.
		logger.Error(nil, "replica took traffic and delivered nothing; scaling to zero and waiting for a request",
			"model", model.Name,
			"unhealthyAfter", model.Spec.Health.UnhealthyAfter.Duration.String(),
			"failureThreshold", model.Spec.Health.FailureThreshold)
		meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
			Type: squallv1alpha1.ConditionHealthy, Status: metav1.ConditionFalse,
			Reason: squallv1alpha1.ReasonNoSuccessfulResponses,
			Message: fmt.Sprintf("no replica delivered a 2xx for %s while requests were arriving",
				model.Spec.Health.UnhealthyAfter.Duration),
		})
	}

	reportProvisioningTimeout(logger, action, &model)
	// D65: a probe proves an HTTP server answered, not which model it was.
	// Ask the replica directly, ONCE per run generation ONCE VERIFIED (skip
	// only when a prior reconcile already reached ConditionServedModelVerified
	// == True — cleared implicitly above whenever a fresh run was minted this
	// pass, since a new run's condition has never been set to True).
	//
	// This is deliberately NOT keyed off status.servedModel being non-empty
	// (I3, block 2 review): a mismatch also writes status.servedModel (to
	// show what the replica actually answers), and gating on that field
	// latched a mismatch forever — an early race, a slow `ollama cp`, or a
	// stale answer during a rolling replacement would never re-check and
	// never clear even once the replica came to agree. A mismatch is not a
	// verdict about the future the way a match is: 1->0 fails safe, so an
	// unresolved disagreement must stay live and be re-evaluated every Ready
	// reconcile until it either resolves or the run is replaced.
	if newPhase == squallv1alpha1.ModelPhaseReady && r.ServedModels != nil &&
		!meta.IsStatusConditionTrue(model.Status.Conditions, squallv1alpha1.ConditionServedModelVerified) {
		want := engineServedName(model.Spec, model.Name)
		served, err := r.ServedModels.ServedModels(ctx, model.Status.ServiceURL)
		switch {
		case err != nil:
			// FAILS OPEN. A verification that could not run is not a
			// verdict, and refusing to serve because a check timed out
			// would break the wake path for a diagnostic.
			logger.Info("could not verify served model; continuing", "error", err)
			meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
				Type: squallv1alpha1.ConditionServedModelVerified, Status: metav1.ConditionUnknown,
				Reason: squallv1alpha1.ReasonUnverified, Message: err.Error(),
			})
		case want != "" && !servedModelMatches(served, want):
			// LOUD. This is the case that shipped a 0.6B model as a 27B one.
			logger.Error(nil, "REPLICA IS SERVING THE WRONG MODEL",
				"want", want, "served", served)
			// servedModel stays the honest diagnostic — D65's whole point is
			// that `kubectl get models` shows a 0.6B standing in for a 27B.
			// forwardModel is the separate, single-valued thing the proxy
			// rewrites requests to, and on a mismatch there is none (D100).
			model.Status.ServedModel = strings.Join(served, ",")
			model.Status.ForwardModel = servedModelToForward(served, want)
			meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
				Type: squallv1alpha1.ConditionServedModelVerified, Status: metav1.ConditionFalse,
				Reason:  squallv1alpha1.ReasonServedModelMismatch,
				Message: fmt.Sprintf("replica serves %v, expected %q", served, want),
			})
		default:
			model.Status.ServedModel = strings.Join(served, ",")
			// D100: the ONE id the proxy may forward under, never the join.
			model.Status.ForwardModel = servedModelToForward(served, want)
			meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
				Type: squallv1alpha1.ConditionServedModelVerified, Status: metav1.ConditionTrue,
				Reason:  squallv1alpha1.ReasonVerified,
				Message: fmt.Sprintf("replica serves %v", served),
			})
		}
	}

	model.Status.Phase = newPhase

	// LIVE-1: Patch, not Update. Status().Update carries an optimistic-lock
	// precondition on the object's WHOLE resourceVersion, but squall-proxy
	// rewrites the SAME object's metadata (the demand-since annotation,
	// squallv1alpha1.DemandAnnotation) as often as every RefreshInterval
	// (LIVE-3) for as long as any request is held — from up to
	// len(replicas) proxies independently. Measured live: resourceVersion
	// advanced every 1-2s while a request was held, and Status().Update
	// lost the optimistic-lock race on EVERY attempt for the request's
	// entire duration — the controller could not write status AT ALL while
	// the GPU it provisioned kept billing (proven by killing the held
	// request: conflicts dropped to zero immediately). A metadata-only
	// change carries no information about whether this reconcile's OWN view
	// of .status is stale, so failing the write on that precondition is a
	// pure false positive, not a real conflict.
	//
	// client.MergeFrom(original) computes a JSON merge patch from the diff
	// between `original` (captured before this reconcile touched anything,
	// above) and `model` now — carrying no resourceVersion precondition, and
	// containing ONLY the status keys this pass actually changed. That
	// second property is what keeps this safe if a second status writer is
	// ever introduced: a field this pass never touched is simply absent
	// from the patch body, so it cannot clobber a concurrent change to that
	// OTHER field. It cannot protect a concurrent write to the SAME field —
	// last-write-wins with no read-check — but this reconciler is the sole
	// writer of the status subresource today (this package's CLAUDE.md), so
	// there is no such writer to race against. If one is ever added, that
	// is the point to replace this with retry.RetryOnConflict around a
	// re-Get-Decide-Patch loop, not to widen this comment.
	if err := r.Status().Patch(ctx, &model, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for %q: %w", model.Name, err)
	}

	// The observed price comes from the run we just looked at, so a Model
	// whose run is gone reports no price rather than the last one it had.
	var observedPerHour float64
	if observed.Run != nil {
		observedPerHour = observed.Run.PricePerHour
	}
	logProvisioningTimeout(logger, action, &model, observedPerHour)
	logUncontrolledIfNeeded(logger, action, uncontrolledSince, &model, observedPerHour)
	r.recordMetrics(&model, observedPerHour, metricUncontrolledSince, observed.Run != nil && observed.Run.Replicas > 0, now)
	r.recordOperationalMetrics(&model, observed, original.Status, now)

	// See IdleRequeueInterval's doc comment: an awake, on-demand Model
	// with nothing to actuate this pass must be re-evaluated later on a
	// timer, or the §6 idle-sleep flip never fires once demand stops.
	if observed.Run != nil && observed.Run.Replicas > 0 && model.Spec.MinReplicas == 0 {
		return ctrl.Result{RequeueAfter: r.idleRequeueInterval()}, nil
	}

	return ctrl.Result{}, nil
}

func (r *ModelReconciler) recordProvisioningAttempt(model *squallv1alpha1.Model, action Action) {
	if action.Replicas > 0 && r.OperationalMetrics != nil {
		r.OperationalMetrics.RecordProvisioningAttempt(model.Namespace, model.Name, metricBackend(model))
	}
}

func (r *ModelReconciler) recordOperationalMetrics(model *squallv1alpha1.Model, observed Observed, prior squallv1alpha1.ModelStatus, now time.Time) {
	if r.OperationalMetrics == nil {
		return
	}
	backend := metricBackend(model)
	fleets := make([]metrics.FleetObservation, 0, len(model.Status.Fleet))
	for _, f := range model.Status.Fleet {
		fleets = append(fleets, metrics.FleetObservation{Backend: f.Backend, Name: f.Name, State: f.State})
	}
	failureReason := ""
	currentFailure := meta.FindStatusCondition(model.Status.Conditions, squallv1alpha1.ConditionProvisioning)
	if currentFailure != nil && currentFailure.Status == metav1.ConditionFalse {
		failureReason = metricProvisioningReason(currentFailure.Reason)
	}
	replicas, active := 0, false
	if observed.Run != nil {
		replicas, active = observed.Run.Replicas, true
	}
	r.OperationalMetrics.Observe(metrics.ModelObservation{
		Namespace: model.Namespace, Name: model.Name, Phase: string(model.Status.Phase), Backend: backend,
		RunActive: active, Replicas: replicas, Fleets: fleets, ProvisioningReason: failureReason,
	})

	if prior.Phase != model.Status.Phase {
		transition := ""
		switch model.Status.Phase {
		case squallv1alpha1.ModelPhaseRecreating:
			transition = "recreate"
		case squallv1alpha1.ModelPhaseWaking:
			transition = "wake"
		case squallv1alpha1.ModelPhaseAsleep:
			transition = "sleep"
		}
		if transition != "" {
			r.OperationalMetrics.RecordTransition(model.Namespace, model.Name, backend, transition)
		}
	}
	duration := time.Duration(0)
	if model.Status.WakeStartedAt != nil {
		duration = now.Sub(model.Status.WakeStartedAt.Time)
	}
	if prior.Phase != squallv1alpha1.ModelPhaseReady && model.Status.Phase == squallv1alpha1.ModelPhaseReady {
		r.OperationalMetrics.RecordProvisioningOutcome(model.Namespace, model.Name, backend, "success", "", duration)
	}
	priorFailure := meta.FindStatusCondition(prior.Conditions, squallv1alpha1.ConditionProvisioning)
	if currentFailure != nil && currentFailure.Status == metav1.ConditionFalse &&
		(priorFailure == nil || priorFailure.Status != metav1.ConditionFalse || priorFailure.Message != currentFailure.Message) {
		r.OperationalMetrics.RecordProvisioningOutcome(model.Namespace, model.Name, backend, "failure", metricProvisioningReason(currentFailure.Reason), duration)
	}
}

func metricBackend(model *squallv1alpha1.Model) string {
	switch len(model.Spec.Placement.Backends) {
	case 0:
		return "_none"
	case 1:
		return model.Spec.Placement.Backends[0]
	default:
		return "_multiple"
	}
}

func metricProvisioningReason(reason string) string {
	switch reason {
	case squallv1alpha1.ReasonHardStopFired:
		return "hard_stop"
	case squallv1alpha1.ReasonNoCapacity:
		return "no_capacity"
	case squallv1alpha1.ReasonInsufficientCredit:
		return "insufficient_credit"
	case squallv1alpha1.ReasonBackendRateLimited:
		return "rate_limited"
	default:
		return "other"
	}
}

// reportOverrideRefusal surfaces dstack's "cannot override active run" on the
// Schedulable condition instead of leaving it inside controller-runtime's
// backoff, where it is indistinguishable from ordinary slowness.
//
// That distinction is the point. A CAS conflict SHOULD be retried; this can
// never succeed on retry, because the submitted spec differs from the live
// run's in something other than the replica count. So a routine `spec.env`
// edit or a rotated secret silently turns into a Model that never wakes again
// -- a `0->1` that fails CLOSED, which the invariant forbids. Measured as the
// D115 addendum: two hours of billing behind a mute retry loop.
//
// Split out of Reconcile only to keep its cyclomatic complexity inside
// qa-lint's bound, exactly as checkSchedulable was.
func (r *ModelReconciler) recoverFromOverrideRefusal(ctx context.Context, logger logr.Logger,
	model, original *squallv1alpha1.Model, action Action, runName string, err error) {
	if !errors.Is(err, dstack.ErrCannotOverride) {
		return
	}

	// Recover, do not merely report -- but ONLY where recovering destroys
	// nothing. Two guards, and both are load-bearing:
	//
	//   action.Replicas > 0     this is a WAKE. `0->1` fails open, so trading
	//                           a run identity for a Model that can serve is
	//                           the right trade. On the SLEEP path the same
	//                           refusal must be left alone: stopping there is
	//                           `1->0` acting on an error, and the cost of not
	//                           acting is money, which is the tolerable one.
	//
	//   Current.Replicas == 0   nothing is serving. Stop on a run at zero
	//                           replicas terminates no generation. With
	//                           replicas > 0 this branch would kill live work
	//                           on the strength of a 400.
	//
	// Stop rather than re-Apply with Current: nil, because that reuses
	// machinery that already exists and is already tested: a terminated run
	// folds to Observed{} in observe(), Decide returns Recreating, and the
	// next pass mints a fresh run through F20's ordinary path. Nothing new
	// decides anything here.
	if action.Replicas > 0 && action.Current != nil && action.Current.Replicas == 0 {
		logger.Info("RECREATING A RUN DSTACK REFUSES TO FLIP: the spec differs beyond the replica "+
			"count, so this run can never wake again in place; stopping it so the next pass mints a "+
			"fresh one. Nothing is serving (replicas 0), so this destroys no work",
			"model", model.Name, "run", runName)
		if serr := r.DstackClient.Stop(ctx, runName); serr != nil && !errors.Is(serr, dstack.ErrNotFound) {
			// Leave the condition below to say so; the requeue retries.
			logger.Error(serr, "could not stop the un-flippable run", "model", model.Name, "run", runName)
		}
	}
	meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
		Type: squallv1alpha1.ConditionSchedulable, Status: metav1.ConditionFalse,
		Reason: squallv1alpha1.ReasonCannotOverride,
		Message: "dstack refuses to flip this run in place because the spec differs " +
			"beyond the replica count; the run must be recreated before it can wake again",
	})
	// LIVE-1 again, and it matters MORE here than at the main write. This
	// refusal fires only while a wake is being demanded -- which means only
	// while squall-proxy is holding a request and refreshing the demand
	// annotation every 1-2s. Status().Update is gated on resourceVersion, so
	// it would lose that race for the entire life of the held request; and
	// once the caller gives up, demand expires, no further Apply is attempted
	// and the function is never called again. The condition would never be
	// persisted at all -- the surfacing fix defeating itself in the one
	// scenario it exists for. Patch against the pass's opening snapshot,
	// exactly as the main write does.
	if serr := r.Status().Patch(ctx, model, client.MergeFrom(original)); serr != nil {
		logger.Error(serr, "could not surface the override refusal", "model", model.Name)
	}
}

func reportProvisioningTimeout(logger logr.Logger, action Action, model *squallv1alpha1.Model) {
	if !action.ProvisioningTimedOut {
		return
	}
	logger.Error(nil, "PROVISIONING DEADLINE EXCEEDED: this wake never reached Ready, so squall is destroying it rather than paying for an instance that may never serve",
		"model", model.Name, "wakeStartedAt", model.Status.WakeStartedAt,
		"provisioningTimeout", model.Spec.ProvisioningTimeout.Duration)
	meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
		Type: squallv1alpha1.ConditionHealthy, Status: metav1.ConditionFalse,
		Reason:  squallv1alpha1.ReasonProvisioningTimeout,
		Message: "provisioning deadline exceeded before the replica became Ready",
	})
}

func reportProvisioningCondition(model *squallv1alpha1.Model, failure *dstack.ProvisioningFailure, phase squallv1alpha1.ModelPhase) {
	if failure == nil {
		if phase == squallv1alpha1.ModelPhaseReady {
			meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
				Type: squallv1alpha1.ConditionProvisioning, Status: metav1.ConditionTrue,
				Reason: squallv1alpha1.ReasonProvisioned,
			})
		}
		return
	}

	detail := strings.TrimSpace(failure.Message)
	if detail == "" {
		detail = failure.Reason
	}
	text := strings.ToLower(failure.Reason + " " + detail)
	reason := squallv1alpha1.ReasonProvisioningFailed
	switch {
	case strings.Contains(text, "max_duration"):
		reason = squallv1alpha1.ReasonHardStopFired
	case strings.Contains(text, "insufficient_credit"), strings.Contains(text, "insufficient credit"):
		reason = squallv1alpha1.ReasonInsufficientCredit
	case strings.Contains(text, "429"), strings.Contains(text, "too many requests"), strings.Contains(text, "rate limit"):
		reason = squallv1alpha1.ReasonBackendRateLimited
	case strings.Contains(text, "no_capacity"), strings.Contains(text, "no capacity"):
		reason = squallv1alpha1.ReasonNoCapacity
	}
	message := fmt.Sprintf("dstack run %s: %s", failure.RunID, detail)
	// D163: meta.SetStatusCondition refreshes LastTransitionTime only when
	// Status FLIPS, so a Model failing the same way run after run would keep
	// the timestamp of its FIRST failure forever. provisioningBackoff reads
	// that field as "when this failure was recorded" and would let the
	// window lapse permanently after one interval, restoring the very hammer
	// it exists to stop. A different run failing is a different failure:
	// drop the condition so the next Set writes a fresh timestamp.
	if existing := meta.FindStatusCondition(model.Status.Conditions, squallv1alpha1.ConditionProvisioning); existing != nil &&
		existing.Status == metav1.ConditionFalse && existing.Message != message {
		meta.RemoveStatusCondition(&model.Status.Conditions, squallv1alpha1.ConditionProvisioning)
	}
	meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
		Type:    squallv1alpha1.ConditionProvisioning,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
}

func logProvisioningTimeout(logger logr.Logger, action Action, model *squallv1alpha1.Model, pricePerHour float64) {
	if !action.ProvisioningTimedOut {
		return
	}
	logger.Error(nil, "PROVISIONING DEADLINE EXCEEDED: destroying paid capacity that never became Ready",
		"model", model.Name, "pricePerHour", pricePerHour)
}

// uncontrolledSince is passed in rather than read from the Model: the status
// block clears the field earlier in this same pass, so reading it here always
// reported nil -- and this log line is the only forensic record of why the
// flip fired (finding #5, 2026-09-01).
func logUncontrolledIfNeeded(logger logr.Logger, action Action, uncontrolledSince *metav1.Time, model *squallv1alpha1.Model, pricePerHour float64) {
	if !action.Uncontrolled {
		return
	}
	logger.Info("SLEEPING UNMEASURABLE CAPACITY: unable to judge idleness past deadline",
		"model", model.Name, "uncontrolledSince", uncontrolledSince,
		"timeout", uncontrolledTimeoutFor(model.Spec), "pricePerHour", pricePerHour)
}

func updateActivityStatus(model *squallv1alpha1.Model, observed Observed, hasDemand bool, now time.Time) *metav1.Time {
	priorUncontrolledSince := model.Status.UncontrolledSince
	if observed.Activity != nil && observed.Activity.AnyData && !observed.Activity.NewestLastRequestAt.IsZero() &&
		(model.Status.LastRequestAt == nil || observed.Activity.NewestLastRequestAt.After(model.Status.LastRequestAt.Time)) {
		t := metav1.NewTime(observed.Activity.NewestLastRequestAt)
		model.Status.LastRequestAt = &t
	}
	// A wake is caused by demand, so it is the earliest honest "something
	// happened" instant. Without this, a Model that woke and was never
	// forwarded to has no anchor at all: sleepDue cannot form a verdict and
	// the uncontrolled deadline never starts, because the evidence is
	// perfectly complete -- it just says nothing. Finding #1, 2026-09-01.
	//
	// It ADVANCES the anchor rather than only seeding a missing one, and that
	// half is load-bearing. sleepDue has no hasDemand guard (phase.go), so on
	// a RE-wake an anchor left over from the previous cycle is already past
	// scaleDownDelay: between the flip to Replicas 1 and the first forward,
	// AnyData is false, the stale anchor wins, and the Model sleeps again in
	// the middle of its own wake. The held request that caused the wake then
	// re-triggers demand and the two oscillate, buying a 2-6 minute cold
	// start every lap. Monotonic by construction -- it can only ever move the
	// anchor forward, never manufacture an idle window that did not happen.
	if w := model.Status.WakeStartedAt; w != nil &&
		(model.Status.LastRequestAt == nil || w.Time.After(model.Status.LastRequestAt.Time)) {
		model.Status.LastRequestAt = w.DeepCopy()
	}
	if observed.Run == nil || observed.Run.Replicas == 0 || (observed.Activity != nil && observed.Activity.Complete) {
		model.Status.UncontrolledSince = nil
		return nil
	}
	if hasDemand {
		metricSince := priorUncontrolledSince
		if metricSince == nil {
			metricSince = &metav1.Time{Time: now}
		}
		model.Status.UncontrolledSince = nil
		return metricSince
	}
	if model.Status.UncontrolledSince == nil {
		t := metav1.NewTime(now)
		model.Status.UncontrolledSince = &t
	}
	return model.Status.UncontrolledSince
}

// applyEnvFor selects one Apply's env and SSH key by DIRECTION.
//
// Wake (Replicas > 0): D63 — resolve secret-backed env from the Model's
// namespace, and a failure must abort the Apply: never provision with a
// credential missing, that bills for a run that cannot work. The SSH key
// is squall's own, so squall-proxy can later reach the replica directly;
// EnsureSSHKey fails open because a data-path optimisation must never
// block a wake.
//
// Sleep (Replicas == 0): D115 — the 1->0 flip needs no credential at all,
// and this guard used to sit on the single shared Apply call site, so a
// rotated or deleted Secret pinned a GPU awake forever ("the GPU bills
// indefinitely because a credential the sleep flip does not need could
// not be read"). The flip re-sends the run's CURRENT env AND ssh key
// verbatim: byte-identical configuration, no secret read, and the D102
// ownership stamp survives untouched. Both halves are load-bearing —
// MEASURED LIVE 2026-08-31 (D115 addendum): a first version of this sent
// SSHKeyPub "" on sleep, dstack answered `400 Cannot override active run`
// to every flip (any spec difference beyond replicas is an override), and
// the GPU billed for two hours while the controller retried.
func (r *ModelReconciler) applyEnvFor(ctx context.Context, model *squallv1alpha1.Model, action Action) (map[string]string, string, error) {
	if action.Replicas > 0 {
		resolvedEnv, err := resolveEnv(ctx, r.Client, model)
		if err != nil {
			return nil, "", fmt.Errorf("resolve env for %q: %w", model.Name, err)
		}
		key := EnsureSSHKey(ctx, r.Client, r.operatorNamespace())
		if key == "" && action.Current != nil {
			// Never send an empty key over a LIVE run: dstack reads any spec
			// difference beyond replicas as an override and answers 400 (D115
			// addendum, measured). EnsureSSHKey fails open to "", so a transient
			// RBAC or API error would otherwise make every wake fail persistently.
			key = action.Current.SSHKeyPub
		}
		return withModelUID(resolvedEnv, model), key, nil
	}
	if action.Current != nil {
		return action.Current.Env, action.Current.SSHKeyPub, nil
	}
	return withModelUID(nil, model), "", nil
}

func applyDurationsFor(model *squallv1alpha1.Model, action Action) (idle, hard time.Duration) {
	if action.Replicas == 0 && action.Current != nil {
		return action.Current.IdleDuration, action.Current.MaxDuration
	}
	// Always zero. dstack's fleet idle window is a second idle window that
	// bills exactly like the first one and buys strictly less: it keeps the
	// machine but drops the weights, so a wake inside it still reloads. The
	// whole budget belongs in idleTimeout, which the controller gates on
	// in-flight evidence. See the single-idle-window design.
	idle = 0
	if model.Spec.MinReplicas == 0 {
		hard = model.Spec.HardStop.Duration
	}
	return idle, hard
}

// checkSchedulable runs Task 5's two wake-time diagnostics (D58, D67, and
// the Price.Validate carry-over from Block 1) and reports on the
// Schedulable condition. It mutates action and newPhase in place because
// exactly one of the two checks — the invalid price — must veto the Apply
// that follows in Reconcile; every other outcome here is diagnosis, never
// enforcement (0->1 fails open). Split out of Reconcile to keep that
// function's own cyclomatic complexity within qa-lint's bound; the checks
// themselves are unchanged.
func (r *ModelReconciler) checkSchedulable(ctx context.Context, model *squallv1alpha1.Model, action *Action, newPhase *squallv1alpha1.ModelPhase, logger logr.Logger) {
	// Money safety valve (Block 1 carry-over, D70): CEL cannot validate
	// spec.placement.maxPricePerHour's content at admission (see
	// Price's doc comment — every attempt made the API server refuse to
	// install the CRD), so a Model with a garbage price always decodes
	// and reaches here unvalidated. This is the ONE preflight check
	// that VETOES a wake instead of merely reporting on it, and it must
	// stay that way: the 0->1-fails-open invariant covers uncertainty
	// about dstack's STATE, where a wrong wake only costs a little
	// money. An unparseable cost ceiling is not state uncertainty — it
	// is an explicit spending limit the user wrote that squall cannot
	// honour, and provisioning without it means provisioning with NO
	// ceiling at all on a marketplace where an H100 rents for $3.41/h.
	// Unbounded spend is the one thing squall must never do by
	// accident; do not "fix" this into a warning.
	if p := model.Spec.Placement.MaxPricePerHour; p != nil {
		if err := p.Validate(); err != nil {
			logger.Error(nil, "refusing to provision: invalid maxPricePerHour",
				"model", model.Name, "error", err)
			meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
				Type: squallv1alpha1.ConditionSchedulable, Status: metav1.ConditionFalse,
				Reason: squallv1alpha1.ReasonInvalidPrice, Message: err.Error(),
			})
			action.Apply = false
			// Nothing was actuated this pass — reporting Decide's
			// Waking/Recreating phase would claim a run exists that was
			// never applied. Keep whatever was last persisted.
			*newPhase = model.Status.Phase
			return
		}
	}

	// F2: the cross-field rules existed but nothing called them — `Validate`
	// and `ValidateWithWarnings` had zero non-test callers, so a Model with
	// holdTimeout > provisioningTimeout (a hold that can outlive the
	// destructive deadline, and therefore can never be satisfied by a
	// successful wake) reconciled happily.
	//
	// Reported as Schedulable=False rather than returned as a reconcile
	// error, and deliberately: an error would retry the same rejected spec
	// forever with backoff, logging noise on every pass and telling the
	// operator nothing they can see with kubectl. A spec that cannot work is
	// a FACT about the Model, so it belongs in status. This one DOES veto the
	// Apply -- unlike the preflight below -- because it is not uncertainty
	// about dstack's state: the user wrote two numbers that contradict each
	// other, and provisioning anyway spends money on a wake that cannot be
	// honoured.
	if warnings, err := ValidateWithWarnings(model.Spec); err != nil {
		logger.Error(nil, "refusing to provision: invalid spec", "model", model.Name, "error", err)
		meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
			Type: squallv1alpha1.ConditionSchedulable, Status: metav1.ConditionFalse,
			Reason: squallv1alpha1.ReasonInvalidSpec, Message: err.Error(),
		})
		action.Apply = false
		// Nothing was actuated, so claiming Decide's Waking/Recreating phase
		// would name a run that was never applied.
		*newPhase = model.Status.Phase
		return
	} else {
		for _, w := range warnings {
			// Advisory by design (§5.1's warm-window rule): legal, and worth
			// saying out loud once per pass rather than silently costing a
			// cold start on most wakes.
			logger.Info("model spec warning", "model", model.Name, "warning", w)
		}
	}

	// D58, D67: tell an unconfigured backend, a backend no fleet
	// admits, and a genuinely empty market apart BEFORE spending a
	// get_plan on it — all three used to be the same silent "zero
	// offers, no error" from outside (see preflight's own doc
	// comment). This is diagnosis, NOT a veto: 0->1 fails open, so a
	// preflight that could not run, or that ran clean, never blocks
	// the Apply below.
	if reason, msg, fleets := preflight(ctx, r.DstackClient, enginePlacement(model.Spec.Placement).Backends); reason != "" {
		if fleets != nil {
			model.Status.Fleet = fleets
		}
		// The operator log, because a condition nobody reads is still
		// silence — and this is the failure a first-time user hits.
		logger.Error(nil, "model cannot be scheduled", "reason", reason, "detail", msg)
		meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
			Type: squallv1alpha1.ConditionSchedulable, Status: metav1.ConditionFalse,
			Reason: reason, Message: msg,
		})
	} else {
		if fleets != nil {
			model.Status.Fleet = fleets
		}
		meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
			Type: squallv1alpha1.ConditionSchedulable, Status: metav1.ConditionTrue,
			Reason: squallv1alpha1.ReasonSchedulable,
		})
	}
}

// priceAsFloat64 converts a Price into the plain dollars/hour a gauge
// needs. Price decoding is TOTAL (D70) and does not validate content, so a
// Price arriving here may not be a plain decimal at all — this metrics path
// does not call Price.Validate (that is Task 5's job, on the Schedulable
// condition), so a parse failure reports 0 rather than a fabricated value.
// "2200m" is one such rejected spelling, not accepted here either (ledger
// D31 addendum: a 1000x price ambiguity with no installed base to be
// compatible with).
func priceAsFloat64(p squallv1alpha1.Price) float64 {
	v, _ := strconv.ParseFloat(p.String(), 64)
	return v
}

// recordMetrics feeds the §10/AC19 declared/observed gauge pairs
// (internal/metrics) from this reconcile's fresh state. Age's observed
// side is real (in-memory since-RunID-changed, per ModelAgeCollector's own
// doc comment); price's observed side comes from dstack's live replica
// provisioning data, and is absent only when dstack reports no price.
func (r *ModelReconciler) recordMetrics(model *squallv1alpha1.Model, observedPerHour float64, uncontrolledSince *metav1.Time, runActive bool, now time.Time) {
	if r.AgeMetrics != nil {
		r.AgeMetrics.Observe(model.Namespace, model.Name, model.Status.RunID, model.Spec.MaxLifetime.Duration, now)
	}
	if r.PriceMetrics != nil {
		var declared float64
		hasDeclared := model.Spec.Placement.MaxPricePerHour != nil
		if hasDeclared {
			declared = priceAsFloat64(*model.Spec.Placement.MaxPricePerHour)
		}
		// D26 closed: the observed half used to be a hard-coded (0, false), so
		// squall_model_price_per_hour never had a value and nothing in this
		// system could state what it was spending. dstack was returning the
		// number the whole time -- see replicaPricePerHour.
		//
		// hasObserved is "> 0", not "run exists": a scaled-to-zero Model has no
		// price, and emitting 0 would read as a price crash against the
		// declared cap rather than as absence.
		r.PriceMetrics.Observe(model.Namespace, model.Name, observedPerHour, observedPerHour > 0, declared, hasDeclared)
	}
	if r.UncontrolledMetrics != nil {
		var since *time.Time
		if uncontrolledSince != nil {
			t := uncontrolledSince.Time
			since = &t
		}
		r.UncontrolledMetrics.Observe(model.Namespace, model.Name, since, uncontrolledTimeoutFor(model.Spec))
	}
	if r.IdleMetrics != nil {
		var last *time.Time
		if model.Status.LastRequestAt != nil {
			t := model.Status.LastRequestAt.Time
			last = &t
		}
		r.IdleMetrics.Observe(model.Namespace, model.Name, last, runActive, time.Duration(model.Spec.ScaleDownDelaySeconds)*time.Second)
	}
}

// observe translates a dstack.Client.Get call, plus (when the run is up)
// a fresh §6 activity sweep, into phase.go's pure Observed shape — the only
// place in this file allowed to talk to dstack or the proxy, so Decide
// itself stays free of clients and context (Task 6.1). Activity is only
// ever gathered when it can matter (Run != nil && Run.Replicas > 0):
// gathering it for a sleeping or nonexistent run would be wasted work and,
// worse, an empty/absent proxy fleet would then have to be special-cased
// to avoid being misread as "zero replicas answered, so idle."
//
// F20, measured against a real server: a dead run does NOT come back as
// ErrNotFound — Get succeeds, with Run.IsTerminal() true. That case is
// folded to Observed{} here too, so Decide's existing "no live run"
// dispatch (phase.go) keeps working unchanged against the corrected wire
// contract.
// observe takes TWO names because they are two different identities, and
// F1 pulled them apart: runName is what dstack files the run under
// ("<namespace>-<name>"), while activityKey is what callers put in a
// request body and therefore what squall-proxy accounts under — the bare
// Model name. Passing the run name to gatherActivity would query a key no
// proxy has ever seen, which reads as "no data" and quietly disables §6.
func (r *ModelReconciler) observe(ctx context.Context, runName, activityKey string, spec squallv1alpha1.ModelSpec, now time.Time) (Observed, error) {
	run, err := r.DstackClient.Get(ctx, runName)
	switch {
	case errors.Is(err, dstack.ErrNotFound):
		return Observed{}, nil
	case err != nil:
		return Observed{}, err
	}
	if run.IsTerminal() {
		failure := run.ProvisioningFailure
		if failure == nil {
			failure = &dstack.ProvisioningFailure{RunID: run.RunID, Reason: run.Status}
		}
		return Observed{ProvisioningFailure: failure}, nil
	}

	observed := Observed{Run: run}
	if run.Replicas > 0 {
		observed.Activity = r.gatherActivity(ctx, activityKey, now)
		// §6: Ready has two named evidences, whichever arrives first —
		// (a) dstack's own probe state (F35), (b) a first-party forward
		// success reported by the proxy. Squall probes nothing itself.
		observed.Ready = run.ProbesReady || freshSuccess(observed.Activity, now, time.Duration(spec.ScaleDownDelaySeconds)*time.Second)
	}
	return observed, nil
}

// freshSuccess reports whether a first-party forward succeeded recently
// enough to count as readiness evidence (§6 evidence (b)). Incomplete
// evidence is never a success: an unreachable replica must not be read as
// proof that anything is serving.
func freshSuccess(ev *ActivityEvidence, now time.Time, window time.Duration) bool {
	if ev == nil || !ev.Complete || ev.NewestLastSuccessAt.IsZero() {
		return false
	}
	return now.Sub(ev.NewestLastSuccessAt) <= window
}

// gatherActivity is Task 7.1's impure layer: it enumerates the proxy
// Service's EndpointSlices fresh (no caching — §6 requires a complete, fresh
// answer every pass, Task 7.2), queries each address's ActivityPath for
// this Model, and folds the results with aggregateActivity. It returns nil
// ("not evaluated") rather than an error, because a §6 evidence-gathering
// failure must never abort the reconcile — it must simply fail to produce
// clean evidence, so sleepDue's nil check keeps the Model awake (Decide
// never uses Activity to decide whether to wake).
//
// r.ProxyService's zero value means "not configured": returning nil here
// unconditionally in that case is what keeps sleep unreachable rather than
// silently reading an empty EndpointSlice list as "zero replicas, all idle".
func (r *ModelReconciler) gatherActivity(ctx context.Context, name string, now time.Time) *ActivityEvidence {
	if r.ProxyService == (types.NamespacedName{}) {
		return nil
	}

	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices,
		client.InNamespace(r.ProxyService.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: r.ProxyService.Name}); err != nil {
		// T4: an EndpointSlice list error must produce incomplete evidence,
		// never a vacuous "zero addresses expected -> complete" reading.
		// aggregateActivity(nil, nil, now) already returns Complete: false
		// for an empty expected set, so this is safe to fold through it.
		evidence := aggregateActivity(nil, nil, now)
		return &evidence
	}

	// Each slice carries its own Ports slice; a slice with no ports (a
	// misconfigured proxy Service) still contributes its addresses so
	// completeness isn't silently bypassed — the query below simply fails
	// to connect, which aggregateActivity folds into Complete: false.
	//
	// D104: serving, not ready. A proxy replica whose readiness probe dips
	// under load, or that is terminating through srv.Shutdown on a rolling
	// update, can still HOLD live generations. EndpointSlice retains it while
	// serving=true and terminating=true; excluding it made those generations
	// invisible to the sleep decision and to the finalizer's drain gate. A
	// not-ready or terminating replica that refuses the query folds into
	// Complete: false, which blocks sleep; not-ready still counts, and now
	// terminating does too. Refused means ambiguous, not idle.
	var addrs []string
	for _, slice := range slices.Items {
		port := ""
		if len(slice.Ports) > 0 && slice.Ports[0].Port != nil {
			port = strconv.Itoa(int(*slice.Ports[0].Port))
		}
		for _, endpoint := range slice.Endpoints {
			if endpoint.Conditions.Serving != nil && !*endpoint.Conditions.Serving {
				continue
			}
			for _, addr := range endpoint.Addresses {
				if port == "" {
					addrs = append(addrs, addr)
				} else {
					addrs = append(addrs, net.JoinHostPort(addr, port))
				}
			}
		}
	}

	queries := make([]ActivityQuery, 0, len(addrs))
	for _, addr := range addrs {
		queries = append(queries, r.queryActivity(ctx, addr, name))
	}

	evidence := aggregateActivity(addrs, queries, now)
	return &evidence
}

// queryActivity fetches one replica's ActivityPath and decodes this
// Model's entry out of it. Any transport failure, non-200, or decode error
// resolves to OK: false rather than panicking or propagating — the caller
// folds that into "incomplete" via aggregateActivity, never into "idle".
func (r *ModelReconciler) queryActivity(ctx context.Context, address, modelName string) ActivityQuery {
	q := ActivityQuery{Address: address}

	url := fmt.Sprintf("http://%s%s", address, squallv1alpha1.ActivityPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return q // OK stays false.
	}

	resp, err := r.activityHTTPClient().Do(req)
	if err != nil {
		return q
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close on a read-only GET.

	if resp.StatusCode != http.StatusOK {
		return q
	}

	var report squallv1alpha1.ActivityReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		return q
	}

	activity, ok := report.Models[modelName]
	if !ok {
		q.OK = true
		q.NoData = true
		return q
	}

	q.OK = true
	q.InFlight = activity.InFlight
	q.FailuresSinceSuccess = activity.FailuresSinceSuccess
	q.LastRequestAt = activity.LastRequestAt
	q.LastSuccessAt = activity.LastSuccessAt
	return q
}

// defaultActivityClient bounds the per-replica activity probe. Same 10s
// and same reasoning as served.go's servedModelsClient (D114): this runs
// serially inside a reconcile, and with MaxConcurrentReconciles: 1 a
// single half-open socket on http.DefaultClient (Timeout: 0) would wedge
// the ENTIRE Model controller — no wakes, no sleeps, no teardowns —
// while every provisioned GPU keeps billing.
var defaultActivityClient = &http.Client{Timeout: 10 * time.Second}

func (r *ModelReconciler) activityHTTPClient() *http.Client {
	if r.HTTPClient == nil {
		return defaultActivityClient
	}
	return r.HTTPClient
}

// hasDemand reports whether the proxy's coalesced demand annotation (see
// squallv1alpha1.DemandAnnotation) is present AND not yet self-expired:
// its value is the RFC3339 instant demand was last coalesced, and it only
// counts while now is within ScaleDownDelaySeconds of that instant — a
// stale annotation the proxy failed to clear (or never will, e.g. after a
// proxy crash) must not pin a Model awake forever (block 7+8 plan §2). A
// missing key, or a value that fails to parse as RFC3339, both resolve to
// false: fail toward no-demand, never toward the old always-present bug.
func hasDemand(m *squallv1alpha1.Model, now time.Time) bool {
	value, ok := m.Annotations[squallv1alpha1.DemandAnnotation]
	if !ok {
		return false
	}
	demandSince, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return false
	}
	ttl := time.Duration(m.Spec.ScaleDownDelaySeconds) * time.Second
	return now.Sub(demandSince) < ttl
}

// SetupWithManager sets up the controller with the Manager.
func (r *ModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&squallv1alpha1.Model{}).
		Named("squall-model").
		Complete(r)
}

// operatorNamespace is where squall's own Secrets live. See SSHKeyNamespace.
func (r *ModelReconciler) operatorNamespace() string {
	if r.SSHKeyNamespace != "" {
		return r.SSHKeyNamespace
	}
	return r.ProxyService.Namespace
}

// replicaStatus maps dstack's reported endpoint onto the CRD's. nil in, nil
// out: "no direct path" must survive the trip, because a stale endpoint left
// on status would send user traffic at a replica that no longer exists.
func replicaStatus(e *dstack.ReplicaEndpoint) *squallv1alpha1.ReplicaEndpoint {
	if e == nil {
		return nil
	}
	return &squallv1alpha1.ReplicaEndpoint{
		Host:        e.Host,
		SSHPort:     int32(e.SSHPort),
		User:        e.User,
		ServicePort: int32(e.ServicePort),
	}
}
