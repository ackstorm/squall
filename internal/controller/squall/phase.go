// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

// Observed is what the reconciler learned about dstack's actual state for
// this Model's run — translated from a single dstack.Client.Get call, into
// plain data. No clients, no context: Decide (and this type) must stay
// pure so the state machine is table-testable with zero envtest (Task
// 6.1).
type Observed struct {
	// Run is nil when there is no LIVE run under this name (F20): either
	// Get returned dstack.ErrNotFound (never applied, or explicitly
	// deleted), or Get succeeded but the run is terminal
	// (Run.IsTerminal()) — measured against a real server, a dead run does
	// NOT read back as ErrNotFound, so observe() (model_controller.go)
	// folds that case to nil here too, preserving this package's existing
	// "dead is not asleep" dispatch unchanged.
	Run *dstack.Run

	// Ready is engine-health evidence (§6) that promotes Waking or
	// Recreating to Ready. A running dstack job is never enough by itself:
	// callers set this from dstack's probe state or a fresh successful
	// forwarded request.
	Ready bool

	// Activity is the aggregated §6 idle evidence gathered fresh this
	// reconcile pass (internal/controller/squall's gatherActivity, over
	// the proxy Service's EndpointSlices) — nil means "not evaluated" and MUST
	// NOT be read as idle. Populated only when it can matter (Run != nil
	// && Run.Replicas > 0); Decide never uses it to decide whether to
	// wake, only whether to sleep (§6: "wake may tolerate uncertainty;
	// sleep must not").
	Activity *ActivityEvidence
}

// ActivityEvidence is one reconcile pass's answer to "is every proxy
// replica idle for this Model, and has it been long enough?" (§6). Any
// replica unreachable, stale, or ambiguous must produce Complete: false —
// there is no fourth outcome and no "assume idle" branch (see the block
// 7+8 plan's "Fresh / Complete / Ambiguous" definitions).
type ActivityEvidence struct {
	// Complete is true only when every address the proxy Service's
	// EndpointSlices listed, in this same pass, answered fresh and
	// unambiguously. False covers every failure mode: an EndpointSlice list
	// error, a per-address timeout/non-200/unparseable body, a negative
	// InFlight, or a future LastRequestAt. A well-formed report with no key
	// for this Model is valid no-data evidence of zero in-flight work.
	Complete bool

	// AllIdle is meaningful only when Complete is true: every address
	// reported InFlight == 0 for this Model.
	AllIdle bool

	// AnyData is true when at least one replica contributed a timestamp. When
	// false, Complete still means every replica answered and proved no work in
	// flight; only the last-request timestamp must come from status.
	AnyData bool

	// NewestLastRequestAt is meaningful only when Complete && AllIdle:
	// the most recent LastRequestAt across every replica, the anchor
	// Decide compares against spec.ScaleDownDelaySeconds.
	NewestLastRequestAt time.Time

	// NewestLastSuccessAt is the newest committed-forward instant across
	// replicas — §6's evidence (b). It is NOT part of the Complete/AllIdle
	// determination: a replica that omits the field (an older build
	// mid-rollout) decodes it as the zero value, which is legitimate "no
	// success observed here", never ambiguity. Making it required would
	// wedge sleep cluster-wide during any rollout.
	NewestLastSuccessAt time.Time

	// FailuresSinceSuccess is the SUM across replicas of requests failed since
	// each last delivered a success — the evidence floor under unhealthyDue.
	// Like NewestLastSuccessAt it is NOT part of the Complete/AllIdle
	// determination: an older proxy omits it, it decodes to 0, and a zero can
	// only ever PREVENT a teardown.
	FailuresSinceSuccess int
}

// Action is Decide's prescription for what the reconciler must actuate.
type Action struct {
	// Apply is true when the reconciler must call dstack Client.Apply
	// with Replicas: 1. False covers every level-triggered no-op case
	// (§5.2: "phase Waking/Ready already? -> no-op") and the fail-safe
	// refusal to ever flip 1->0 from this function — Phase 6 implements
	// only the wake ("0->1, fails open") half of the flip; Phase 7 owns
	// the opposite, fail-safe direction.
	Apply bool

	// Current is the CAS anchor (F18) to send with Apply when Apply is true:
	// dstack's real apply compares the WHOLE previous Run object, not an
	// integer, so this is always exactly Observed.Run — set when a live run
	// already exists, left nil (its zero value) when Observed.Run is nil
	// (a cold first wake or a Dead recreate, F20), which is what tells
	// dstack to mint a fresh run rather than CAS against nothing.
	Current *dstack.Run

	// Replicas is the value the reconciler must send with Apply when
	// Apply is true: 1 for every wake, 0 for the sleep flip (§6). Before
	// this field existed, model_controller.go hardcoded Replicas: 1,
	// which made a wake and a sleep Apply indistinguishable at the call
	// site — see the block 7+8 plan §1.
	Replicas int

	// Alarm is true exactly when Decide detects an uncommanded death: a
	// run this reconciler previously journaled (prior.RunID != "") is now
	// gone from dstack (F20). The finalizer's own drain-then-delete path
	// (Phase 8) clears the CR entirely before the run disappears, so that
	// path never produces this — Alarm only ever fires for a run that
	// vanished out from under a CR that is still supposed to exist.
	Alarm bool

	// Unhealthy is true when this pass's flip to Replicas: 0 was the "traffic
	// but no successful responses" verdict rather than a plain idle sleep. Both
	// produce the identical actuation, so the reconciler needs this to report
	// WHICH happened on the Healthy condition. Mirrors Alarm: a diagnosis
	// carried out of the pure layer, never an instruction.
	Unhealthy bool

	// Uncontrolled marks a deadline-driven 1->0 flip taken without idle
	// evidence.
	Uncontrolled bool

	// ProvisioningTimedOut marks the destructive provisioning deadline.
	ProvisioningTimedOut bool

	// At is the moment Decide computed this Action — threaded from the
	// caller's now, never read from a clock inside this file (Task 6.1:
	// pure function, no clock reads). Callers use it to timestamp status
	// conditions.
	At time.Time
}

// Decide is the phase state machine (§5.2, §6): a pure function of what
// the reconciler observed in dstack, the Model's last-written status, its
// declared spec, and whether demand exists right now (a coalesced proxy
// annotation, or spec.MinReplicas == 1 pinning) — plus now, supplied by
// the caller rather than read from a clock. No clients, no context, no
// I/O: table-testable with zero envtest (Task 6.1).
//
// Phase 6 implemented only the wake ("0→1, fails open") direction. This
// block (7+8) adds the opposite, fail-safe direction: once observed.Run is
// up, an on-demand Model (spec.MinReplicas == 0) with clean, complete,
// idle Activity evidence aged past ScaleDownDelaySeconds flips back to
// Replicas: 0 (Asleep, F17) — never on hasDemand's absence alone, only on
// the aggregation (§6). A pinned Model (MinReplicas == 1) never takes this
// branch (AC17). provisioningTimeout's destructive trigger is still Phase
// 8's Task 8.3 (blocked on OQ3). Deletion's Draining sequence is a
// separate code path (the finalizer, Phase 8) driven by the API server's
// deletion timestamp rather than by this state machine, so Draining never
// appears in this table.
func Decide(
	observed Observed,
	prior squallv1alpha1.ModelStatus,
	spec squallv1alpha1.ModelSpec,
	hasDemand bool,
	now time.Time,
) (squallv1alpha1.ModelPhase, Action) {
	// Pinning (minReplicas: 1) IS demand — a pinned model wakes without
	// waiting for a proxy request (§6, AC17).
	wantAwake := spec.MinReplicas == 1 || hasDemand

	if observed.Run == nil {
		// F20: dstack has no live run under this name. died distinguishes
		// "we never applied one" from "the one we tracked is gone" — both
		// read back as the same 404 from dstack (and, for the proxy, from
		// the gateway, F23), but only the memory of prior.RunID tells
		// them apart on the controller side.
		died := prior.RunID != ""

		if !wantAwake {
			if died {
				return squallv1alpha1.ModelPhaseDead, Action{Alarm: true, At: now}
			}
			return squallv1alpha1.ModelPhaseAsleep, Action{At: now}
		}

		phase := squallv1alpha1.ModelPhaseWaking
		if died {
			phase = squallv1alpha1.ModelPhaseRecreating
		}
		// Either way this mints a brand-new run (F20) — there is nothing
		// live to CAS against, so Current is left nil.
		return phase, Action{Apply: true, Replicas: 1, Alarm: died, At: now}
	}

	if observed.Run.Replicas == 0 {
		// Asleep: registered, addressable, gateway 503 (F17, F23) — the
		// in-place flip case, distinct from F20's mint-a-new-run case
		// above.
		if !wantAwake {
			return squallv1alpha1.ModelPhaseAsleep, Action{At: now}
		}
		return squallv1alpha1.ModelPhaseWaking, Action{
			Apply:    true,
			Replicas: 1,
			Current:  observed.Run,
			At:       now,
		}
	}

	// observed.Run.Replicas > 0: dstack already reports the run up.
	//
	// The provisioning deadline is the only destructive age-based trigger in
	// §5.2. It must win before the three Asleep flips below: those describe a
	// replica that reached Ready at some point, while this describes a wake
	// that never landed. Dead, not Asleep (F20), so the next attempt mints a
	// fresh run rather than flipping replicas on a husk.
	if provisioningDue(prior.WakeStartedAt, observed.Ready, spec.ProvisioningTimeout.Duration, now) {
		return squallv1alpha1.ModelPhaseDead, Action{
			Apply: true, Replicas: 0, Current: observed.Run, Alarm: true,
			ProvisioningTimedOut: true, At: now,
		}
	}

	// Sleep check first, ahead of the Ready/Recreating/Waking dispatch
	// below: the aggregation is the sole signal (§6), independent of
	// hasDemand and independent of engine-readiness evidence this
	// controller has no way to observe yet (Observed.Ready is always
	// false today — see its doc comment). Gating this on Ready would make
	// the sleep path unreachable in the running system, not merely
	// cautious.
	if spec.MinReplicas == 0 && sleepDue(observed.Activity, prior.LastRequestAt, spec.ScaleDownDelaySeconds, now) {
		return squallv1alpha1.ModelPhaseAsleep, Action{
			Apply:    true,
			Replicas: 0,
			Current:  observed.Run,
			At:       now,
		}
	}

	// Second fail-safe flip, checked AFTER the idle one so a Model that is both
	// idle and unsuccessful is reported as the cheaper, more ordinary of the
	// two. Same actuation, different diagnosis — and note it fires regardless of
	// hasDemand: demand is not a veto on a replica that cannot serve it.
	if spec.MinReplicas == 0 && unhealthyDue(observed.Activity, observed.Ready, prior.WakeStartedAt,
		spec.Health.UnhealthyAfter.Duration, spec.Health.FailureThreshold, spec.ScaleDownDelaySeconds, now) {
		return squallv1alpha1.ModelPhaseAsleep, Action{
			Apply:     true,
			Replicas:  0,
			Current:   observed.Run,
			Unhealthy: true,
			At:        now,
		}
	}
	if spec.MinReplicas == 0 && uncontrolledDue(prior.UncontrolledSince, uncontrolledTimeoutFor(spec), hasDemand, now) {
		return squallv1alpha1.ModelPhaseAsleep, Action{
			Apply: true, Replicas: 0, Current: observed.Run, Uncontrolled: true, At: now,
		}
	}

	// Level-triggered no-op otherwise — this block never flips 1->0
	// except through the sleep check just above.
	if observed.Ready {
		return squallv1alpha1.ModelPhaseReady, Action{Replicas: 1, At: now}
	}
	if prior.Phase == squallv1alpha1.ModelPhaseRecreating {
		// Preserve the Recreating label until Ready — an operator reading
		// status should still see this run came from a death (F20), not
		// a plain wake.
		return squallv1alpha1.ModelPhaseRecreating, Action{Replicas: 1, At: now}
	}
	return squallv1alpha1.ModelPhaseWaking, Action{Replicas: 1, At: now}
}

const DefaultUncontrolledGrace = 15 * time.Minute
const MaxUncontrolledTimeout = 2 * time.Hour

func uncontrolledTimeoutFor(spec squallv1alpha1.ModelSpec) time.Duration {
	if spec.UncontrolledTimeout != nil {
		return spec.UncontrolledTimeout.Duration
	}
	d := 4*time.Duration(spec.ScaleDownDelaySeconds)*time.Second + DefaultUncontrolledGrace
	if d > MaxUncontrolledTimeout {
		d = MaxUncontrolledTimeout
	}
	return d
}

func uncontrolledDue(since *metav1.Time, timeout time.Duration, hasDemand bool, now time.Time) bool {
	return !hasDemand && timeout > 0 && since != nil && !since.IsZero() && now.Sub(since.Time) > timeout
}

func provisioningDue(wakeStartedAt *metav1.Time, ready bool, timeout time.Duration, now time.Time) bool {
	if ready || timeout <= 0 || wakeStartedAt == nil || wakeStartedAt.IsZero() {
		return false
	}
	return now.Sub(wakeStartedAt.Time) > timeout
}

// sleepDue is §6's fail-safe flip condition: clean, complete, idle
// evidence, aged past ScaleDownDelaySeconds. activity == nil ("not
// evaluated") and activity.Complete == false both correctly return false —
// there is no default-to-idle branch. The time comparison is a strict
// inequality kept separate from AllIdle on purpose (T1 in the block 7+8
// plan: a mutation that fires on AllIdle alone, dropping this comparison,
// must turn red).
func sleepDue(activity *ActivityEvidence, durableLastRequestAt *metav1.Time, scaleDownDelaySeconds int32, now time.Time) bool {
	if activity == nil || !activity.Complete || !activity.AllIdle {
		return false
	}
	anchor := activity.NewestLastRequestAt
	if !activity.AnyData && activity.NewestLastRequestAt.IsZero() {
		if durableLastRequestAt == nil || durableLastRequestAt.IsZero() {
			return false
		}
		anchor = durableLastRequestAt.Time
	}
	return now.Sub(anchor) > time.Duration(scaleDownDelaySeconds)*time.Second
}

// DefaultUnhealthyFailureThreshold is the evidence floor used when a Model
// carries no explicit spec.unhealthyFailureThreshold — an object stored before
// the field existed, or an explicit 0. Three, matching Kubernetes' own probe
// default. It is a floor, never a target: its only job is to keep one or two
// failed requests from being read as a verdict.
const DefaultUnhealthyFailureThreshold int32 = 3

// unhealthyDue is the second fail-safe flip (2026-08-31): the run is up, its
// probes are ready, requests keep arriving, and nothing has delivered a 2xx in
// full for unhealthyAfter. That is a replica taking traffic and returning
// nothing, and it must not be kept alive by the very requests it is failing —
// in-flight requests are what block sleepDue, so a failing replica otherwise
// looks busier the worse it gets.
//
// Ready is REQUIRED and is what keeps this out of provisioning's way: before
// the engine answers its own health probe, spec.provisioningTimeout owns the
// timeout, not this. (provisioningTimeout's destructive trigger is still
// unimplemented — Task 8.3, OQ3 — so today nothing bounds a Model stuck Waking.
// That gap is real, was observed live on 2026-08-31, and is NOT this
// function's job.)
//
// Recent traffic is REQUIRED so the two flips stay distinguishable: with no
// traffic sleepDue already owns the decision, and reporting "unhealthy" would
// slander a Model nobody is using.
//
// The failure THRESHOLD is the second, independent condition, and the two are
// different in kind on purpose. Time says "this has gone on long enough to not
// be a blip"; the count says "we asked enough times to be sure". Either alone
// has a cheap counter-example — notably a Model that served fine, went quiet
// for twenty minutes and then failed a single request. The threshold cannot be
// disabled; a non-positive value reads as the default, never as "no floor".
// Both live under spec.health.
//
// The anchor is the LATER of the newest success and the wake instant. Without
// the wake term a run that has never succeeded would measure from the zero time
// and be condemned the instant it came up. Without any anchor at all — never
// succeeded, no wake journalled — there is no verdict: 1->0 fails safe, and a
// missing timestamp is not evidence of failure.
func unhealthyDue(
	activity *ActivityEvidence,
	ready bool,
	wakeStartedAt *metav1.Time,
	unhealthyAfter time.Duration,
	threshold int32,
	scaleDownDelaySeconds int32,
	now time.Time,
) bool {
	if !ready || unhealthyAfter <= 0 {
		return false
	}
	if activity == nil || !activity.Complete {
		return false
	}
	if now.Sub(activity.NewestLastRequestAt) > time.Duration(scaleDownDelaySeconds)*time.Second {
		return false // no current traffic: sleepDue's business, not this one's.
	}
	if threshold <= 0 {
		threshold = DefaultUnhealthyFailureThreshold
	}
	if activity.FailuresSinceSuccess < int(threshold) {
		return false // not enough evidence yet; one bad request is not a verdict.
	}
	anchor := activity.NewestLastSuccessAt
	if wakeStartedAt != nil && wakeStartedAt.Time.After(anchor) {
		anchor = wakeStartedAt.Time
	}
	if anchor.IsZero() {
		return false
	}
	return now.Sub(anchor) > unhealthyAfter
}
