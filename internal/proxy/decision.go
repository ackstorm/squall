// SPDX-License-Identifier: MIT

// Package proxy is squall-proxy's data-path logic (spec §7): the six-row
// decision table, the blocking hold, coalesced+refreshed demand patches,
// the /v1/models discovery surface and the internal activity endpoint.
// Deliberately dependency-thin (spec §11): no controller-runtime manager,
// client or cache anywhere in this package or cmd/proxy — only the plain
// squallv1alpha1 wire types, a small client-go informer cache
// (internal/proxy/cache.go), and the transitive
// sigs.k8s.io/controller-runtime/pkg/scheme helper that api/squall/v1alpha1's
// generated groupversion_info.go pulls in for scheme registration (see
// docs/references/deviations-and-findings.md D35) — a thin type-registration
// utility, not manager/controller machinery.
package proxy

import (
	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// WaitState names the "state" field of §7's 503 wait-contract JSON body.
type WaitState string

const (
	WaitAsleep     WaitState = "asleep"
	WaitWaking     WaitState = "waking"
	WaitRecreating WaitState = "recreating"
)

// GatewayCode is the HTTP status observed from an actually-attempted
// forward to a Ready Model's backend — the network-level signal F23
// describes ("gateway 503/404/403"), a different axis from status.phase
// (the informer cache's view, which Decide also takes). Zero means "no
// forward attempted yet / not applicable"; Decide only ever consults it
// when phase is Ready, because that is the only phase in which a forward is
// ever attempted.
type GatewayCode int

// Action is Decide's prescription for one incoming request. Exactly one of
// Forward, Block or a non-zero ImmediateStatus applies.
type Action struct {
	// Forward is true when the proxy should forward the request to the
	// Model's backend right now (Ready, no gateway fault yet observed).
	Forward bool

	// Block is true when the proxy should hold the request open — writing
	// nothing to the client — up to spec.HoldTimeout, refreshing demand
	// while it waits (task 9.3), then answer DeadlineStatus/DeadlineState.
	Block bool

	// DemandPatch is true when the proxy must write/refresh the coalesced
	// demand annotation for this Model (§7's "demand patch" / "demand
	// coalesced").
	DemandPatch bool

	// ImmediateStatus, when non-zero, is the status the proxy answers right
	// now: no forward, no block. Covers Draining's new-request 404, the
	// no-CR 404, and the gateway-403 auth-fault 502.
	ImmediateStatus int

	// DeadlineStatus/DeadlineState are what Block answers when holdTimeout
	// elapses — or immediately, when holdTimeout is 0 (§7).
	DeadlineStatus int
	DeadlineState  WaitState

	// Alarm is true only for the gateway-403 auth-fault row (F23): the
	// proxy observed this directly by attempting a forward, so it is the
	// one alarm this package raises itself. The Dead-phase "alarm when the
	// death was uncommanded" (§7) is already raised by squall-controller
	// (phase.go's Action.Alarm, fired the reconcile that first observed the
	// death) — the proxy has no memory of a prior run id, so it cannot
	// itself judge "uncommanded" and must not duplicate a guess.
	Alarm bool
}

// Decide is §7's six-row decision table (spec v0.17-RC §7), transcribed
// verbatim as a pure function: no clients, no I/O, no clock reads — one
// case per row, no exceptions (task 9.1). Two rows carry behaviour that is
// easy to get subtly wrong:
//
//   - Draining: an in-flight forward already streaming never re-enters
//     Decide (it is running on its own goroutine against the response it
//     already started). A request that arrives WHILE Draining is by
//     definition new, so it must never block — it is answered 404
//     immediately; blocking it would hold a connection open for capacity
//     that is being torn down.
//   - Dead: unlike Asleep, waking from Dead is a full cold start (a new run
//     generation, F20) rather than an in-place flip, so its deadline
//     answers "recreating", never "asleep" — even though the mechanical
//     hold uses the same spec.HoldTimeout as every other blocked phase.
//
// hasCR false means the informer cache has no Model by this name at all —
// "no such Model CR in desired state" (§7) — answered 404 unconditionally;
// phase and gatewayCode are meaningless in that case.
func Decide(phase squallv1alpha1.ModelPhase, hasCR bool, gatewayCode GatewayCode) Action {
	if !hasCR {
		return Action{ImmediateStatus: 404}
	}

	switch phase {
	case squallv1alpha1.ModelPhaseReady:
		if gatewayCode == 403 {
			// F23: 403 is an auth fault, never a wake signal.
			return Action{Alarm: true, ImmediateStatus: 502}
		}
		return Action{Forward: true}

	case squallv1alpha1.ModelPhaseAsleep:
		return Action{DemandPatch: true, Block: true, DeadlineStatus: 503, DeadlineState: WaitAsleep}

	case squallv1alpha1.ModelPhaseWaking:
		return Action{DemandPatch: true, Block: true, DeadlineStatus: 503, DeadlineState: WaitWaking}

	case squallv1alpha1.ModelPhaseRecreating:
		return Action{DemandPatch: true, Block: true, DeadlineStatus: 503, DeadlineState: WaitRecreating}

	case squallv1alpha1.ModelPhaseDead:
		// Demand here drives the controller's recreate (phase.go: no live
		// run + wantAwake -> Waking/Recreating, minting a fresh run id).
		return Action{DemandPatch: true, Block: true, DeadlineStatus: 503, DeadlineState: WaitRecreating}

	case squallv1alpha1.ModelPhaseDraining:
		return Action{ImmediateStatus: 404}

	default:
		// An unrecognized phase value must fail toward the safest known
		// action, never toward forwarding blindly: treat it like "no CR".
		return Action{ImmediateStatus: 404}
	}
}
