// SPDX-License-Identifier: MIT

// Wire types for dstack's real API, measured against 0.21.2. Keep this file
// a mirror of upstream and nothing else: no behaviour, no defaults beyond
// what the server itself applies. When dstack changes, this is the file to
// diff. See docs/references/dstack-real-api.md.
package dstack

import (
	"encoding/json"
	"fmt"
	"time"
)

// defaultReadyAfter mirrors dstack's own ProbeConfig default and is the
// value squall submits, so probesReady can compare against it without
// re-reading the plan.
const defaultReadyAfter = 2

// defaultProbePath is the fallback when neither the CR nor the engine
// derivation supplies one. /health is vLLM's and llama.cpp's.
const defaultProbePath = "/health"

// probeIntervalSeconds is how often dstack runs the probe. It bounds how
// stale evidence (a) can be, so it belongs next to the deadline reasoning
// in §5.1, not in a caller.
const probeIntervalSeconds = 5

type replicasWire struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type probeConfigWire struct {
	Type       string `json:"type"`
	URL        string `json:"url"`
	Method     string `json:"method,omitempty"`
	Timeout    int    `json:"timeout,omitempty"`
	Interval   int    `json:"interval"`
	ReadyAfter int    `json:"ready_after"`
}

// cpuWire / resourcesWire mirror dstack's CPUSpec and ResourcesSpec. Every
// field is omitempty because dstack applies its OWN default to anything we
// omit — and those defaults are not "unconstrained": 2 cores, 8GB RAM,
// 100GB disk, and a GPU count minimum of ZERO. Sending a partial block is
// therefore a real choice, not a no-op. See ledger D55.
type cpuWire struct {
	Arch  string `json:"arch,omitempty"`
	Count string `json:"count,omitempty"`
}

type diskWire struct {
	Size string `json:"size"`
}

type gpuWire struct {
	Vendor            string   `json:"vendor,omitempty"`
	Name              []string `json:"name,omitempty"`
	Count             string   `json:"count,omitempty"`
	Memory            string   `json:"memory,omitempty"`
	TotalMemory       string   `json:"total_memory,omitempty"`
	ComputeCapability string   `json:"compute_capability,omitempty"`
}

type resourcesWire struct {
	CPU     *cpuWire  `json:"cpu,omitempty"`
	Memory  string    `json:"memory,omitempty"`
	ShmSize string    `json:"shm_size,omitempty"`
	GPU     *gpuWire  `json:"gpu,omitempty"`
	Disk    *diskWire `json:"disk,omitempty"`
}

// configurationWire is a dstack service configuration. Squall submits a
// FIXED replica count (min == max), never a range: a range would require a
// `scaling` block and hand the replica count to dstack's RPS autoscaler.
// With `scaling` absent dstack selects ManualScaler, which ignores request
// stats entirely — that is what makes §10's two-lane rule hold by
// construction rather than by policy.
//
// Backends/Regions/MaxPrice are dstack PROFILE parameters, which dstack
// merges into the configuration rather than into `resources`. Backends is
// §12.3's compliance allowlist and the CRD makes it mandatory, so it is the
// one field here that must never be silently absent.
type configurationWire struct {
	Type         string            `json:"type"`
	Name         string            `json:"name"`
	Image        string            `json:"image"`
	Port         int               `json:"port"`
	Replicas     int               `json:"replicas"`
	Probes       []probeConfigWire `json:"probes,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Commands     []string          `json:"commands,omitempty"`
	Resources    *resourcesWire    `json:"resources,omitempty"`
	Backends     []string          `json:"backends,omitempty"`
	Regions      []string          `json:"regions,omitempty"`
	MaxPrice     string            `json:"max_price,omitempty"`
	IdleDuration *int              `json:"idle_duration,omitempty"`
	MaxDuration  string            `json:"max_duration,omitempty"`
}

// dstackDuration renders dstack's whole-second duration wire format.
func dstackDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return fmt.Sprintf("%ds", int64(d/time.Second))
}

// dstackSeconds renders a duration as dstack's whole-second integer form.
//
// It returns a NON-NIL pointer for a zero duration on purpose. Zero is a
// meaningful value — "release on the first idle pass" — and it has to reach
// the wire, while a nil pointer means "unset" and lets dstack apply its own
// defaults: DEFAULT_RUN_TERMINATION_IDLE_TIME = 5m and
// DEFAULT_FLEET_TERMINATION_IDLE_TIME = 3d (core/models/profiles.py). The
// string form with `omitempty` could not express that difference, which is
// how a Go zero came to mean "three days of paid idle instance" (D166).
func dstackSeconds(d time.Duration) *int {
	if d < 0 {
		d = 0
	}
	s := int(d / time.Second)
	return &s
}

func wireSeconds(v *int) time.Duration {
	if v == nil {
		return 0
	}
	return time.Duration(*v) * time.Second
}

type runSpecWire struct {
	RunName       string            `json:"run_name"`
	Configuration configurationWire `json:"configuration"`

	// SSHKeyPub is squall's OWN public key. dstack's vastai backend builds
	// the container's authorized_keys from BOTH this and the project key
	// (core/backends/vastai/compute.py), which is what lets squall-proxy
	// tunnel to the replica without ever touching dstack's project private
	// key (D47). omitempty matters: unset, dstack substitutes the calling
	// user's key and nothing changes.
	//
	// get_plan echoes run_spec back RAW and apply replays that echo
	// verbatim, so setting it here carries it through both legs.
	SSHKeyPub string `json:"ssh_key_pub,omitempty"`
}

type getPlanRequest struct {
	RunSpec runSpecWire `json:"run_spec"`
}

// runPlanWire is get_plan's response. Only the fields apply needs are
// decoded; the run_spec is kept RAW because apply must echo back exactly
// what the server normalised (replicas 1 -> {min:1,max:1}, interval "5s" ->
// 5), not our re-serialisation of it.
type runPlanWire struct {
	RunSpec         json.RawMessage `json:"run_spec"`
	CurrentResource json.RawMessage `json:"current_resource"`
	Action          string          `json:"action"`
}

// applyPlanInput carries the plan back. current_resource is raw for the
// same reason: the CAS compares the whole object.
type applyPlanInput struct {
	RunSpec         json.RawMessage `json:"run_spec"`
	CurrentResource json.RawMessage `json:"current_resource,omitempty"`
}

// applyRequest is the ONLY place `force` is encoded, and it is encoded as
// the literal false. dstack requires the field; squall requires it to be
// false. There is no Go-level way for a caller to reach it (AC13).
type applyRequest struct {
	Plan  applyPlanInput `json:"plan"`
	Force bool           `json:"force"`
}

func newApplyRequest(plan applyPlanInput) applyRequest {
	return applyRequest{Plan: plan, Force: false}
}

type getRunRequest struct {
	RunName string `json:"run_name"`
}

// fleetFloorResources is EnsureFleet's fixed resource floor, deliberately
// the SAME numbers `deploy/helm/squall`'s own kind-fleet example already
// ships (values.yaml) — not a new invented constant. It is a FLOOR, not a
// target: dstack's fleet/run resource combination INTERSECTS ranges rather
// than overriding them (combine.py's _combine_resources), so a floor this
// low never narrows what an actual Model asks for, only ever widens the
// fleet's own minimum viable instance. No GPU field: leaving GPU absent
// means a GPU-needing Model's own requirement passes through the
// intersection untouched (_combine_gpu_optional returns the non-nil side
// verbatim when the other is nil) instead of being widened OR narrowed by
// a guess made here.
var fleetFloorResources = resourcesWire{
	CPU:    &cpuWire{Count: "1.."},
	Memory: "512MB..",
	Disk:   &diskWire{Size: "10GB.."},
}

// fleetConfigurationWire is a dstack fleet configuration — the create-fleet
// analogue of configurationWire. Nodes stays the opaque range string
// (F33's own convention): FleetNodesSpec's own `parse_nodes` validator
// accepts "min..max" directly over the wire, exactly like the CLI-parsed
// YAML form squall's fleet Job already uses.
type fleetConfigurationWire struct {
	Type         string        `json:"type"`
	Name         string        `json:"name"`
	Nodes        string        `json:"nodes"`
	Resources    resourcesWire `json:"resources"`
	Backends     []string      `json:"backends,omitempty"`
	IdleDuration *int          `json:"idle_duration,omitempty"`
}

// fleetSpecWire mirrors dstack's FleetSpec. Profile is a required field on
// the wire but every one of ITS OWN fields defaults server-side (measured:
// core/models/profiles.py's ProfileProps.name defaults to "" and .default
// to false), so an empty object is a valid, meaningful "no profile
// overrides" rather than a placeholder.
type fleetSpecWire struct {
	Configuration fleetConfigurationWire `json:"configuration"`
	Profile       struct{}               `json:"profile"`
}

type getFleetPlanRequest struct {
	Spec fleetSpecWire `json:"spec"`
}

// fleetPlanWire is fleets/get_plan's response. Spec/EffectiveSpec stay RAW
// for the same reason runPlanWire's run_spec does: apply must echo back
// exactly what the server normalised. FleetPlan.get_effective_spec()
// (core/models/fleets.py) falls back to Spec whenever EffectiveSpec is
// absent — EnsureFleet only ever creates a fleet by a name it just proved
// (via fleets/get) does not exist yet, so EffectiveSpec is not expected to
// diverge from Spec in practice, but the fallback is kept to match upstream
// exactly rather than assume that.
type fleetPlanWire struct {
	Spec          json.RawMessage `json:"spec"`
	EffectiveSpec json.RawMessage `json:"effective_spec"`
}

// applyFleetPlanInput carries the plan back, RAW for the same reason
// applyPlanInput's run_spec is: dstack's whole-object CAS compares against
// exactly what it echoed. CurrentResource is always omitted here —
// EnsureFleet only calls this on the create path (see http.go), where
// omitting it is what tells dstack to mint a new fleet rather than expect
// one already there.
type applyFleetPlanInput struct {
	Spec            json.RawMessage `json:"spec"`
	CurrentResource json.RawMessage `json:"current_resource,omitempty"`
}

// applyFleetPlanRequest is the only place fleets/apply's `force` is
// encoded, and — exactly like runs' applyRequest — it is encoded as the
// literal false. Squall never forces a fleet apply any more than it forces
// a run apply (AC13's discipline applied to the new surface): there is no
// Go-level way for a caller to reach the field.
type applyFleetPlanRequest struct {
	Plan  applyFleetPlanInput `json:"plan"`
	Force bool                `json:"force"`
}

func newApplyFleetPlanRequest(plan applyFleetPlanInput) applyFleetPlanRequest {
	return applyFleetPlanRequest{Plan: plan, Force: false}
}

// getFleetRequest is fleets/get's body — EnsureFleet's existence check.
// MEASURED against a live dstack 0.21.2 (2026-08-28): a name it does not
// know answers `{"detail":[{"msg":"Resource not found","code":
// "resource_not_exists"}]}`, which classifyError already maps to
// ErrNotFound — the same discriminator used everywhere else in this
// package, confirming it is server-wide and not run-specific.
type getFleetRequest struct {
	Name string `json:"name"`
}

type deleteRunsRequest struct {
	RunsNames []string `json:"runs_names"`
}

type stopRunsRequest struct {
	RunsNames []string `json:"runs_names"`
	Abort     bool     `json:"abort"`
}

type serviceWire struct {
	URL string `json:"url"`
}

type runWire struct {
	ID                  string             `json:"id"`
	SubmittedAt         time.Time          `json:"submitted_at"`
	Status              string             `json:"status"`
	StatusMessage       string             `json:"status_message"`
	TerminationReason   string             `json:"termination_reason"`
	Error               string             `json:"error"`
	DeploymentNum       int                `json:"deployment_num"`
	Jobs                []jobWire          `json:"jobs"`
	LatestJobSubmission *jobSubmissionWire `json:"latest_job_submission"`
	Service             *serviceWire       `json:"service"`
	RunSpec             struct {
		RunName       string `json:"run_name"`
		Configuration struct {
			Replicas     replicasWire `json:"replicas"`
			IdleDuration *int         `json:"idle_duration"`
			MaxDuration  *int         `json:"max_duration"`
			// dstack echoes the probes it was created with. Reading
			// ready_after from HERE rather than from a constant means
			// readiness is judged against what this run actually has, even
			// if the CR has since changed and not yet been re-applied.
			Probes []probeConfigWire `json:"probes"`
			// Echoed back the same way, and read for the same reason: it is
			// the only place a fact about WHO created this run survives the
			// Kubernetes object being deleted and recreated. dstack has no
			// labels or tags, so env is the carrier (see Run.Env).
			Env map[string]string `json:"env"`
		} `json:"configuration"`
		// Echoed too (verified against a live run 2026-08-31). Read back so
		// a replica-count-only Apply can re-send the run's OWN key verbatim:
		// dstack refuses to update an ACTIVE run whose submitted spec
		// differs in ANYTHING but replicas ("Cannot override active run"),
		// so the sleep flip must reproduce the spec byte-identically — see
		// applyEnvFor (D115 addendum).
		SSHKeyPub string `json:"ssh_key_pub"`
	} `json:"run_spec"`
}
