// SPDX-License-Identifier: Apache-2.0

// Package dstack is a narrow client over the dstack server's run-management
// API — Apply, Get, Delete, ListRuns.
// Not an SDK: one HTTP round trip per call, no retries, backoff, circuit
// breakers or metrics. Fields on Run may be added when a spec section names
// the state they carry, and only once that field is confirmed against a real
// dstack server (ledger D1 / the Tier-1 e2e-local suite).
// Gateway probing belongs to the proxy (Phase 6), not here.
//
// The one hard requirement (§5.2, AC13): this client can never send force.
// That is enforced by construction, not by a runtime guard — ApplyRequest
// has no Force field to set, so there is nothing to validate. The losing
// side of dstack's optimistic-concurrency CAS (F18) must fail loudly and
// requeue, never retry with force.
//
// This package MUST NOT import internal/dstack/mock. Production code
// importing a test double is the exact layering violation Phase 4 fixed by
// hoisting Clock into internal/clock (see that package's doc comment). This
// package and the mock stay independent implementations that agree only on
// HTTP status codes and JSON field names; only client_test.go is allowed to
// import the mock, to drive this client over a real HTTP round trip.
//
// The wire shape (wire.go, http.go) is modelled against dstack 0.21.2,
// measured from upstream source and a real Kubernetes-backend server — see
// docs/references/dstack-real-api.md. Fields not confirmed by that
// measurement are not added speculatively.
package dstack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// ErrNotFound is returned by Get and Delete when dstack no longer knows the
// run: it was never registered, or it went terminal and was deregistered
// (F20). Dead is not asleep — a caller must not treat this the same as
// Asleep (replicas: 0, which the server answers normally, not as an error).
var ErrNotFound = errors.New("dstack: run not found")

// ErrResourceChanged is returned by Apply when dstack's whole-object CAS
// (F18) rejects the request: the previous Run passed as ApplyRequest.Current
// no longer matches current state. This is the CAS on the actuation side
// (§5.2) — the losing side of a race must fail loudly and requeue, never
// retry with force (AC13). ApplyRequest has no Force field to retry with.
var ErrResourceChanged = errors.New("dstack: resource has been changed; re-read and re-plan, never retry with force")

// ErrCannotOverride is dstack refusing an apply because the submitted spec
// differs from the live run's in something other than the replica count.
//
// It is separated from ErrResourceChanged because the correct response is the
// opposite one. A CAS conflict means "you raced; re-read and re-plan", and
// retrying is right. This means "this run cannot be flipped in place at all",
// and retrying is guaranteed to fail forever -- which turns a routine
// `spec.env` edit into a Model that never wakes again, silently, behind a
// backoff that looks like ordinary slowness. `0->1` must fail OPEN, so a
// caller seeing this has to either surface it or mint a fresh run; it must
// never simply retry.
var ErrCannotOverride = errors.New("dstack: cannot override active run; the spec differs beyond replicas, so this run cannot be flipped in place")

// ApplyRequest is the input to Apply. There is deliberately no Force field.
// Real dstack makes `force` a REQUIRED field on the apply body, so the
// literal false is encoded at the single encode site in wire.go and no
// caller has anything to set (§5.2, AC13).
type ApplyRequest struct {
	Name     string
	Replicas int

	// SSHKeyPub is squall's OWN public key, in authorized_keys form. dstack's
	// vastai backend authorises BOTH this and its project key on the
	// container, so supplying it here is what lets squall-proxy reach the
	// replica directly without ever handling dstack's project private key
	// (D47). Empty is valid and means "no direct path" — dstack substitutes
	// the calling user's key and squall keeps using dstack's own proxy.
	SSHKeyPub string

	// Image is spec.image, the pinned digest (§8, §9). Port is where the
	// engine listens, derived from spec.engine by enginePort — the CRD does
	// NOT carry a port field and this plan does not add one.
	Image string
	Port  int

	// Env is spec.env: engine-native configuration, passed through as
	// dstack's `env` map. For Ollama this is where the serving shape lives
	// — OLLAMA_CONTEXT_LENGTH, OLLAMA_KV_CACHE_TYPE — and F29's whole VRAM
	// budget is computed assuming those are set. Dropping them does not
	// fail: the engine starts with its own defaults (4k context, f16 KV)
	// and then OOMs the card the CR asked for.
	Env map[string]string

	// Args is spec.args, sent as dstack's `commands`. NOTE the semantics:
	// dstack's commands REPLACE the image entrypoint, they are not appended
	// to it. That is correct for vLLM (whose image entrypoint is the server
	// binary and whose args are flags to it) but a caller writing bare
	// flags for an image with a wrapper entrypoint will get a container
	// that does not start. Empty is the common case.
	Args []string

	// Resources is spec.resources, passed through verbatim (F33). nil means
	// squall sends no resources block AT ALL, which is not "unconstrained":
	// dstack then applies its own defaults — 2 cores, 8GB RAM, 100GB disk,
	// and a GPU count minimum of ZERO. A GPU-serving model with a nil
	// Resources will happily land on a machine with no GPU. See ledger D55.
	Resources *Resources

	// Probe is §6 evidence (a): the readiness probe dstack runs against the
	// replica. nil submits squall's defaults (path /health, 5s, 2 successes)
	// — which is right for a stock vLLM and wrong for anything else, exactly
	// the reason the CR can override it.
	Probe *Probe

	// Placement is spec.placement. Backends is §12.3's compliance
	// allowlist and the CRD makes it mandatory (MinItems=1) — an empty
	// Backends here means the eligibility table is not being enforced,
	// which is the exact failure its own CRD comment warns about.
	Placement Placement

	// Current is the CAS anchor (F18). dstack's apply compares the ENTIRE
	// previous Run object, not a deployment number: "Errors if the expected
	// current resource from the plan does not match the current resource."
	// Pass the Run a previous Get returned; nil means "expect no run to
	// exist", which is what creates one. The losing side of a race gets
	// ErrResourceChanged and must re-read and re-plan, never force.
	Current *Run
}

// Resources mirrors dstack's ResourcesSpec. Ranges stay opaque strings:
// squall does not parse or validate dstack's range grammar, dstack does
// (F33).
type Resources struct {
	CPUArch  string
	CPUCount string
	Memory   string
	ShmSize  string
	Disk     string
	GPU      *GPU
}

// GPU mirrors dstack's GPUSpec.
type GPU struct {
	Vendor            string
	Name              []string
	Count             string
	Memory            string
	TotalMemory       string
	ComputeCapability string
}

// Probe mirrors dstack's HTTP ProbeConfig. IntervalSeconds and ReadyAfter
// are the two that matter to §6: a wake cannot be observed faster than
// IntervalSeconds * ReadyAfter, and ReadyAfter is what probesReady compares
// each success streak against.
type Probe struct {
	Path            string
	Method          string
	TimeoutSeconds  int
	IntervalSeconds int
	ReadyAfter      int
}

// Placement carries dstack's profile-level provisioning constraints. These
// are NOT part of `resources` on the wire — dstack merges them into the
// configuration.
type Placement struct {
	Backends []string
	Regions  []string
	MaxPrice string
}

// FleetSpec is EnsureFleet's input: the minimum a fleet needs to admit runs
// on Backends. There is deliberately no Resources field — EnsureFleet
// always submits a fixed, wide-open floor (see http.go), never a per-Model
// one. dstack's fleet/run combination INTERSECTS resource ranges rather
// than overriding them (server/services/requirements/combine.py,
// _combine_resources): a fleet floor tighter than some future Model's own
// ask would silently narrow that Model below what it actually requested.
// Pinning Resources to one caller's Model would be exactly that bug.
type FleetSpec struct {
	// Name should be built with FleetName so two callers asking for the
	// same Backends land on the identical fleet instead of each minting
	// their own.
	Name     string
	Backends []string
}

// FleetName derives EnsureFleet's fleet name from a single backend.
//
// Branch B (LIVE-7, docs/references/dstack-real-api.md §9.8): a dstack run's
// `backends` is an ALLOW-SET, not a must-all-satisfy list — a candidate
// fleet only needs to intersect ONE of them
// (server/services/requirements/combine.py's _intersect_lists_optional).
// preflight already checks fleet coverage per INDIVIDUAL backend (one
// BackendConfigured/HasFleetFor pair per entry in spec.placement.backends),
// so a fleet keyed by a single backend composes correctly with any Model's
// placement that names it, whatever else that Model's own backends list
// contains — and two Models that both name, say, "vastai" always land on
// the SAME fleet ("squall-auto-vastai") rather than each minting a
// redundant one. That is deliberately simpler than keying by a Model's
// whole backend set: it needs no set-canonicalisation, no dedup step, and
// cannot collide on backend-list ordering.
func FleetName(backend string) string {
	return "squall-auto-" + backend
}

// Run is the client's view of dstack run state.
// ReplicaEndpoint is how to reach a running replica's engine port WITHOUT
// going through dstack's own service proxy — the measured 0.2.0 data path
// (see decisions-and-open-items.md). nil means squall must not try: either
// nothing is running, or the topology needs more hops than one.
type ReplicaEndpoint struct {
	// Host and SSHPort are the replica's SSH endpoint, reachable from
	// anywhere. The ENGINE port itself is not exposed — measured, :8000
	// refuses and :ssh_port answers — so SSH is the only way in.
	Host    string
	SSHPort int
	User    string

	// ServicePort is where the engine listens INSIDE the container, i.e. the
	// far end of the forward.
	ServicePort int
}

type Run struct {
	Name          string
	RunID         string
	DeploymentNum int
	Replicas      int

	// Replica is the direct path to this run's live replica, or nil when
	// there is none to offer. See ReplicaEndpoint.
	Replica *ReplicaEndpoint

	// ServiceURL is dstack's own forward target for this service —
	// measured shape: "/proxy/services/{project}/{run}/", a path relative
	// to the dstack server base URL when no gateway is provisioned. This is
	// what squall-proxy's Backend resolves to (closes ledger D25).
	ServiceURL string

	// PricePerHour is what the live replica ACTUALLY costs, as the backend
	// quoted it (D26). 0 means "not observed" -- a scaled-to-zero run, or a
	// backend that quoted nothing -- never "free".
	PricePerHour float64

	// ProbesReady is §6 evidence (a), DERIVED: dstack exposes only
	// `success_streak` per probe, so readiness is
	// `success_streak >= ready_after` across every probe of every replica's
	// latest job submission. Squall probes nothing, ever. "Replicas > 0" is
	// NEVER Ready by itself (§6).
	ProbesReady bool

	// Env is the configuration env dstack echoes back for this run. Squall
	// reads exactly one key from it, SQUALL_MODEL_UID, because a dstack run
	// name is stable across a Model being deleted and recreated while the
	// Kubernetes UID is not — so this is what tells "my run" apart from "a
	// run left by a previous incarnation of a Model with the same name".
	Env map[string]string

	// SSHKeyPub is the run's OWN key, echoed off run_spec. Read back so a
	// replica-count-only Apply (the sleep flip) can reproduce the active
	// run's spec byte-identically: dstack answers "Cannot override active
	// run" to any other difference (D115 addendum, measured live
	// 2026-08-31 — a sleep that sent "" here was refused for hours while
	// the GPU billed).
	SSHKeyPub string

	// Status is dstack's own run status. MEASURED, and load-bearing for
	// F20: a slept run settles at "pending" with zero live replicas, while
	// a dead one is "terminated"/"failed". Both look identical through the
	// service proxy, which answers 404 for either — so this is the only
	// thing that tells asleep from dead.
	Status string

	// SubmittedAt is when dstack accepted the run. Informational: the
	// provisioningTimeout anchor is status.wakeStartedAt, journaled by the
	// controller at actuation (§5.2), because an in-place flip (F17) reuses
	// the run and SubmittedAt dates from its first creation.
	SubmittedAt time.Time

	// raw is the verbatim JSON dstack returned for this run. It exists ONLY
	// to be handed back as apply's `current_resource`: the CAS compares the
	// whole object, so any field we drop on decode would corrupt the
	// comparison. Never read it for state — that is what the typed fields
	// above are for.
	raw json.RawMessage
}

// finishedRunStatuses mirrors dstack's own finished_statuses() (§7,
// measured §9.4): terminated/failed/done. A run in one of these settles
// there permanently — it never comes back on its own.
var finishedRunStatuses = map[string]bool{
	"terminated": true,
	"failed":     true,
	"done":       true,
}

// IsTerminal reports whether dstack itself considers this run finished
// (§7's finished_statuses(), F20's "dead"). Measured (§9.4): a dead run
// still answers Get successfully — with Status "terminated"/"failed" and
// zero live replicas — it is NOT ErrNotFound. Asleep (F17's replicas: 0
// flip) settles at Status "pending" instead, which IsTerminal correctly
// reports false for. Callers needing F20's "dead is not asleep" distinction
// must check this, not just Replicas == 0.
func (r Run) IsTerminal() bool {
	return finishedRunStatuses[r.Status]
}

// Client is the narrow interface the controller needs — nothing more
// (YAGNI; this is not a dstack SDK).
type Client interface {
	// Apply flips the replica count in place. It NEVER sends force: the
	// losing side of a race must fail loudly (F18, §5.2, AC13).
	Apply(ctx context.Context, req ApplyRequest) (*Run, error)
	// Get returns current run state, or ErrNotFound if deregistered (F20).
	Get(ctx context.Context, name string) (*Run, error)
	// Stop asks dstack to terminate the run. It MUST be called before
	// Delete on any run that is not already terminal: dstack refuses to
	// delete an active run with HTTP 400 "Cannot delete active runs", and a
	// teardown that ignores that leaves the instance billing forever
	// (D56, measured on Vast.ai). Stop is idempotent and returns
	// ErrNotFound for a run that is already gone.
	Stop(ctx context.Context, name string) error
	// Delete removes the run. It only succeeds on a TERMINAL run — see
	// Stop. Fleet instance release is dstack's own job via
	// fleet.idleDuration (F21); Delete does not and must not model that.
	Delete(ctx context.Context, name string) error
	// ListRuns backs the reconcile loop's orphan diff (§5.2).
	ListRuns(ctx context.Context) ([]Run, error)

	// BackendConfigured reports whether backend is configured on the dstack
	// server for this project (D67). MEASURED (2026-08-27, dstack 0.21.2):
	// POST /api/project/{p}/backends/{name}/config_info answers 200 when it
	// is and 400 when it is not — vastai/kubernetes -> 200, aws -> 400 on
	// the server this was checked against. dstack reconciles its backend
	// list against server/config.yml on every boot and silently deletes any
	// backend the file no longer mentions, which is exactly what produced
	// D67: a helm upgrade dropped the vastai overlay and the backend
	// vanished with no diagnostic anywhere.
	BackendConfigured(ctx context.Context, backend string) (bool, error)

	// HasFleetFor reports whether some active fleet admits backend. A run
	// needs a fleet on EVERY backend, not just Kubernetes, and without one
	// get_plan returns zero offers silently (D58) — indistinguishable from
	// an empty market unless this is checked separately.
	HasFleetFor(ctx context.Context, backend string) (bool, error)

	// EnsureFleet idempotently makes sure a fleet named spec.Name exists
	// admitting spec.Backends. LIVE-7 Branch B: dstack 0.21.2 never
	// auto-creates a fleet for a run with none (§9.8), so a run whose
	// backends are all configured but admitted by no fleet still gets a
	// silent zero-offer NO_CAPACITY termination (D58). This turns that gap
	// into remediation.
	//
	// CREATE-ONLY, LEVEL-TRIGGERED: an existing fleet by that name is left
	// untouched — never updated, never deleted. Fleet lifecycle beyond
	// initial creation (resizing, decommissioning an orphan) is explicitly
	// out of scope; see ledger D83. A caller that wants different
	// placement on an already-existing auto fleet must edit or delete it
	// by hand.
	EnsureFleet(ctx context.Context, spec FleetSpec) error
}

// HTTPClient is the production Client: a thin wrapper over the dstack
// server's HTTP API, modelled against dstack 0.21.2 (see the package doc).
// Zero value is not usable — construct with NewHTTPClient.
type HTTPClient struct {
	baseURL    string
	project    string
	token      string
	httpClient *http.Client
}

// NewHTTPClient constructs a Client against baseURL, scoped to project (dstack
// has no server-wide run namespace — every path is under
// /api/project/{project}/...), authorizing every request with token as a
// Bearer header. httpClient may be nil, in which case http.DefaultClient is
// used.
func NewHTTPClient(baseURL, project, token string, httpClient *http.Client) *HTTPClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &HTTPClient{baseURL: baseURL, project: project, token: token, httpClient: httpClient}
}

var _ Client = (*HTTPClient)(nil)
