// SPDX-License-Identifier: MIT

package v1alpha1

// Condition types on a Model. Phase stays the six-value state machine the
// proxy branches on (§7's table); conditions carry WHY, which phase cannot.
//
// Deliberately NOT a new phase. `Unschedulable` reads well on a CR, but
// every phase value is an input to internal/proxy.Decide, whose two rules
// decide money and correctness — 0→1 fails open, 1→0 fails safe. Widening
// that table is not a change to make in the same commit as a diagnostic
// improvement. A condition reaches the same reader (the proxy's informer
// cache already carries status) without touching the state machine.
const (
	// ConditionSchedulable is False when this Model CANNOT provision for a
	// structural reason: a backend it names is not configured on the dstack
	// server, or no fleet admits that backend.
	//
	// A genuinely empty market (get_plan returns zero offers even though the
	// backend is configured and fleeted) is NOT one of the reasons this
	// condition can report, despite D58 naming it as a symptom that used to
	// be indistinguishable from the other two: distinguishing it needs a
	// get_plan call from a call site (preflight, ahead of the real Apply)
	// that does not exist today, specifically to avoid spending one before
	// committing to provision — see preflight's own doc comment — and
	// dstack's offer data (RunPlan.job_plans[].total_offers) is otherwise
	// unmodelled here by design (internal/dstack/CLAUDE.md: "Deliberately
	// not modelled: offer selection"). Deferred; see D58's ledger entry and
	// docs/references/deviations-and-findings.md (Gap A, block 2 review).
	ConditionSchedulable = "Schedulable"

	// ConditionProvisioning reports the latest terminal dstack attempt. It is
	// False from a failed attempt until a replacement reaches Ready.
	ConditionProvisioning = "Provisioning"

	// ConditionServedModelVerified is True once the replica's own
	// GET /v1/models confirmed it serves this Model's name (D65).
	ConditionServedModelVerified = "ServedModelVerified"

	// ConditionHealthy is False when the unhealthy teardown fired: the run was
	// up, dstack's probes were ready, requests kept arriving, and no replica
	// delivered a 2xx for spec.unhealthyAfter across at least
	// spec.unhealthyFailureThreshold failures.
	//
	// It is the ONLY way to tell that flip apart from a plain idle sleep after
	// the fact. Both end at phase Asleep on purpose, so that the proxy, the
	// wait contract and the wake path need no knowledge of this feature at all.
	ConditionHealthy = "Healthy"
)

// Reasons. Kept specific: "the backend is not configured on the server" and
// "no fleet admits it" call for opposite responses from an operator, and the
// whole point of these conditions is telling them apart. There is
// deliberately no third "the market has nothing right now" reason here —
// see ConditionSchedulable's doc comment; it is a diagnosed gap (Gap A,
// block 2 review), not an oversight.
const (
	ReasonSchedulable        = "Schedulable"
	ReasonBackendUnavailable = "BackendUnavailable"
	ReasonNoFleet            = "NoFleet"
	// ReasonCannotOverride is dstack refusing to flip a run in place because
	// the submitted spec differs from the live one beyond the replica count.
	// It is a wake that fails CLOSED, which the invariant forbids, so it is
	// reported rather than left to look like ordinary retry backoff.
	ReasonCannotOverride      = "CannotOverride"
	ReasonVerified            = "Verified"
	ReasonUnverified          = "Unverified"
	ReasonServedModelMismatch = "ServedModelMismatch"

	// ReasonInvalidPrice is set when spec.placement.maxPricePerHour does not
	// parse as a plain decimal (Price.Validate). Unlike every other
	// Schedulable=False reason, this one BLOCKS provisioning (Task 5, D70
	// carry-over from Block 1): the fails-open invariant covers uncertainty
	// about dstack's STATE, where a wrong wake merely costs a little money.
	// An unparseable cost ceiling is not state uncertainty — it is a
	// spending limit the user wrote that squall cannot honour, and
	// provisioning without it means provisioning with NO ceiling at all on
	// a marketplace where an H100 is $3.41/h. Do not soften this into a
	// warning.
	ReasonInvalidPrice = "InvalidPrice"

	// ReasonInvalidSpec is set when the cross-field rules in
	// ValidateWithWarnings reject the spec — deadline ordering, the F21
	// fleet-idle trap, the §12.3 backends allowlist. Reported rather than
	// returned as a reconcile error: a rejected spec is a fact about the
	// Model, and an error would only retry it forever with backoff.
	ReasonInvalidSpec = "InvalidSpec"

	// ReasonRunFromAnotherIncarnation is set when the dstack run found under
	// this Model's name carries a DIFFERENT Model UID — the Model was
	// deleted and recreated while its run survived. Diagnostic, not a veto.
	ReasonRunFromAnotherIncarnation = "RunFromAnotherIncarnation"

	// ReasonNoSuccessfulResponses accompanies ConditionHealthy=False: the
	// replica was taking traffic and delivering nothing.
	ReasonNoSuccessfulResponses = "NoSuccessfulResponses"
	ReasonHardStopFired         = "HardStopFired"

	// ReasonHealthy accompanies ConditionHealthy=True, set whenever a fresh run
	// is minted. A new run has never been judged, and inheriting the previous
	// run's verdict would be a statement about a different machine.
	ReasonHealthy             = "Healthy"
	ReasonProvisioningTimeout = "ProvisioningTimeout"
	ReasonProvisioned         = "Provisioned"
	ReasonInsufficientCredit  = "InsufficientCredit"
	ReasonNoCapacity          = "NoCapacity"
	ReasonBackendRateLimited  = "BackendRateLimited"
	ReasonProvisioningFailed  = "ProvisioningFailed"
)

const (
	FleetStateAdmitting    = "Admitting"
	FleetStateCreated      = "Created"
	FleetStateUnfleeted    = "Unfleeted"
	FleetStateUnconfigured = "Unconfigured"
)
