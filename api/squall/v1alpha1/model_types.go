// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelEngine selects the serving container template. It is the one
// legitimately per-*engine* element in the CRD (spec §5.1/§8): it picks the
// health-check path and warmup request shape that feed the §6 Ready state
// machine. There is no per-*provider* equivalent — one OCI image serves
// every dstack backend (F34).
type ModelEngine string

const (
	// ModelEngineVLLM is the preferred default: the throughput-per-euro
	// doctrine on rented GPUs (§8).
	ModelEngineVLLM ModelEngine = "vllm"
	// ModelEngineLlamaCpp serves GGUF weights (§8, F13).
	ModelEngineLlamaCpp ModelEngine = "llama-cpp"
	// ModelEngineOllama is admitted not by preference but by capability:
	// some hybrid architectures are served by Ollama today and by nothing
	// else in the stack (§8).
	ModelEngineOllama ModelEngine = "ollama"
)

// ModelFeature is a capability a Model serves, surfaced verbatim on
// squall-proxy's /v1/models listing. The values mirror KubeAI's, because
// `type: kubeai` discovery consumes that listing (F27, F30).
// +kubebuilder:validation:Enum=TextGeneration;TextEmbedding;SpeechToText
type ModelFeature string

const (
	// ModelFeatureTextGeneration is chat/completions — the only feature
	// v0.1's engines actually serve (§8).
	ModelFeatureTextGeneration ModelFeature = "TextGeneration"
	// ModelFeatureTextEmbedding is accepted by the schema so a CR author can
	// declare it, but no v0.1 engine template serves it yet (§8).
	ModelFeatureTextEmbedding ModelFeature = "TextEmbedding"
	// ModelFeatureSpeechToText is accepted by the schema on the same terms
	// as ModelFeatureTextEmbedding.
	ModelFeatureSpeechToText ModelFeature = "SpeechToText"
)

// GPUSpec is dstack's own resource selector, passed through verbatim (F33)
// — there is no invented GPU schema and no translation layer. VRAM alone
// cannot express a bandwidth requirement (an A10G and an RTX3090 are both
// 24 GB and differ ~50% in bandwidth, and decode is bandwidth-bound), so a
// card list stated as hardware (Name) carries what a GiB number cannot.
// Count, Memory and TotalMemory mirror dstack's native "Range" syntax
// (e.g. "1..2", "24GB..32GB") and are kept as opaque strings for the same
// passthrough reason: squall does not parse or validate dstack's range
// grammar, dstack does.
type GPUSpec struct {
	// Vendor filters by GPU vendor (e.g. "nvidia", "amd", "google",
	// "intel"). Optional — dstack defaults to nvidia.
	// +optional
	Vendor string `json:"vendor,omitempty"`

	// Name is a backend-agnostic list of acceptable card names (e.g.
	// [A10G, RTX3090, RTX5090]) — the bandwidth requirement, stated as
	// hardware rather than a memory figure.
	// +optional
	Name []string `json:"name,omitempty"`

	// Count is dstack's Range syntax for the number of GPUs (e.g. "1",
	// "1..2").
	// +optional
	Count string `json:"count,omitempty"`

	// Memory is dstack's native range for per-GPU VRAM (e.g.
	// "24GB..32GB").
	// +optional
	Memory string `json:"memory,omitempty"`

	// TotalMemory is dstack's native range for aggregate VRAM across all
	// requested GPUs.
	// +optional
	TotalMemory string `json:"totalMemory,omitempty"`

	// ComputeCapability is the minimum CUDA compute capability dstack
	// should filter on (e.g. "7.5").
	// +optional
	ComputeCapability string `json:"computeCapability,omitempty"`
}

// CPUSpec mirrors dstack's own CPUSpec. Count is dstack's Range syntax as
// an opaque string, for the same passthrough reason as GPUSpec's ranges.
type CPUSpec struct {
	// Arch is the CPU architecture dstack should filter on. Optional —
	// dstack does not constrain architecture when this is empty.
	// +kubebuilder:validation:Enum=x86;arm
	// +optional
	Arch string `json:"arch,omitempty"`

	// Count is dstack's Range syntax for the number of cores (e.g. "2",
	// "4..8"). Empty defers to dstack's own default, which is a MINIMUM of
	// 2 (DEFAULT_CPU_COUNT, core/models/resources.py) — not "unconstrained".
	// +optional
	Count string `json:"count,omitempty"`
}

// SecretKeyRef points at one key of a Secret in the Model's own namespace.
// It is how a credential reaches a replica WITHOUT ever being written into
// the CR, and therefore without being committed to Git — which is the only
// part of the exposure squall can actually remove (D63).
//
// Read this before using it. The weights are fetched ON the rented host, so
// a token for a gated model MUST reach a machine we do not control. §12.3
// is explicit that a marketplace host is not a data processor. This field
// does not make that safe; it makes it auditable and rotatable. §12.4's
// rule stands: fine-grained, read-only, scoped to the one repo that needs
// it. "The question is never whether one leaks; it is how little it hurts
// when it does."
type SecretKeyRef struct {
	// Name is the Secret's name, in the Model's namespace. Cross-namespace
	// references are deliberately not supported: a Model must not be able
	// to read credentials out of a namespace its author cannot see.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key is the key within that Secret.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// ModelProbe is dstack's HTTP ProbeConfig, passed through (F33, same rule
// as ModelResources). It is what §6 evidence (a) is actually made of: the
// controller promotes a Model to Ready when dstack reports ReadyAfter
// consecutive successes of THIS probe.
//
// Every field is optional and defaults sensibly, but the defaults are
// squall's, not dstack's — see the field docs. The reason this exists as a
// CR field at all is that a probe path is engine-specific AND
// deployment-specific: vLLM serves /health, Ollama serves /, and a model
// behind an auth-required gateway may serve neither.
type ModelProbe struct {
	// Path is the HTTP path dstack probes. Empty derives it from
	// spec.engine (vLLM and llama.cpp: /health, Ollama: /), which is right
	// for a stock engine image and wrong for anything customised.
	// +optional
	Path string `json:"path,omitempty"`

	// Method is the HTTP method. Empty defers to dstack's own default (GET).
	// +kubebuilder:validation:Enum=GET;POST;HEAD
	// +optional
	Method string `json:"method,omitempty"`

	// Timeout bounds a single probe attempt. Empty defers to dstack.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Interval is how often dstack probes. Empty means 5s. It bounds how
	// stale evidence (a) can be, so it interacts with holdTimeout: a wake
	// cannot be observed faster than Interval * ReadyAfter.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// ReadyAfter is how many CONSECUTIVE successes mark the replica ready.
	// Empty means 2. Raising it buys confidence and costs wake latency
	// (Interval * ReadyAfter before a held request can be served); 0 is
	// rejected rather than treated as "always ready" — §6 does not allow
	// "dstack says the job is running" to stand in for readiness.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ReadyAfter *int32 `json:"readyAfter,omitempty"`
}

// ModelResources is dstack's ResourcesSpec, passed through verbatim (F33)
// — no invented schema and no translation layer, the same rule GPUSpec
// already followed.
//
// Every field here is optional, and EVERY OMITTED FIELD MEANS A DSTACK
// DEFAULT, never "unconstrained" (all from core/models/resources.py, dstack
// 0.21.2):
//
//	cpu     -> minimum 2 cores
//	memory  -> minimum 8GB
//	disk    -> minimum 100GB   (DEFAULT_DISK)
//	gpu     -> count minimum 0 (DEFAULT_GPU_SPEC — i.e. NO GPU REQUIRED)
//
// That last one is why this type exists in this shape: a Model that leaves
// gpu unset does not get "whatever GPU is cheapest", it gets an offer that
// may have no GPU at all. Leaving disk unset does not get "whatever disk
// the image needs", it silently constrains offer selection to >=100GB and
// bills for it. See ledger D55.
type ModelResources struct {
	// CPU is dstack's CPUSpec.
	// +optional
	CPU *CPUSpec `json:"cpu,omitempty"`

	// Memory is dstack's native range for system RAM (e.g. "8GB..",
	// "16GB..32GB").
	// +optional
	Memory string `json:"memory,omitempty"`

	// ShmSize is the size of /dev/shm (e.g. "8GB"). A single size, not a
	// range — dstack's shm_size is a scalar. Engines that shard across
	// GPUs with NCCL, or that use PyTorch dataloader workers, need this
	// raised above the container default.
	// +optional
	ShmSize string `json:"shmSize,omitempty"`

	// GPU is dstack's GPUSpec, passed through verbatim (F33).
	// +optional
	GPU *GPUSpec `json:"gpu,omitempty"`

	// Disk is dstack's native range for disk size (e.g. "100GB..").
	// This is where the model weights land, so it is a real sizing
	// decision: a 70B model in fp16 is ~140GB before any cache.
	// +optional
	Disk string `json:"disk,omitempty"`
}

// ModelPlacement scopes and cost-bounds where dstack is allowed to
// provision this model.
type ModelPlacement struct {
	// Backends is the compliance allowlist (§12.3): the mandatory
	// workload-eligibility classification (marketplace vs AWS vs
	// DigitalOcean) is enforced through this field, not documented beside
	// it. It MUST NOT be empty — an empty list would let the controller
	// pick any backend, silently defeating the eligibility table.
	// +kubebuilder:validation:MinItems=1
	Backends []string `json:"backends"`

	// Regions is a backend-native filter (e.g. an EU geo constraint on
	// Vast). Semantics are defined by the backend, not by squall.
	// +optional
	Regions []string `json:"regions,omitempty"`

	// MaxPricePerHour is a cost ceiling, enforced by dstack BEFORE
	// provisioning. Write it however you like — `1.20` and `"1.20"` are both
	// accepted (D31).
	// +optional
	MaxPricePerHour *Price `json:"maxPricePerHour,omitempty"`
}

// ModelFleet configures the dstack fleet backing this model's runs.
type ModelFleet struct {
	// IdleDuration is how long the underlying machine is kept after the
	// job-layer flip to replicas: 0 before the fleet releases it (§6).
	// It is REQUIRED with NO default: dstack's own default is 3 days
	// (F21), the single most expensive footgun in the system — an
	// operator who forgets this field pays for an idle GPU for three days,
	// not minutes. It also directly conditions §7: `hold` is only viable
	// inside this warm window.
	// +kubebuilder:validation:Required
	IdleDuration metav1.Duration `json:"idleDuration"`
}

// ModelSpec defines the desired state of Model.
//
// Shaped deliberately after kubeai.org/v1 Model (F27) so a reviewer sees
// the same shape in an MR as the in-cluster models beside it, and moving a
// model in-cluster ↔ external is mostly a placement diff (spec §5.1).
//
// There is deliberately NO `weights` field: spec v0.17 cut it. Weights ride
// inside Args/Env in each engine's own vocabulary (`--model` for vLLM,
// `-hf` for llama.cpp, an env var or pull command for Ollama) — there is no
// translation table (§8). There is likewise no `scaling:`-style block, no
// `budget`, `routerClass`, `suspend`, or `firstRequestPolicy`: the
// controller MUST NOT implement load-based 1..N scaling (§3, §5.2); those
// fields were deliberately cut in earlier spec revisions and reintroducing
// any of them contradicts the controller contract.
type ModelSpec struct {
	// Engine selects the serving container template (vllm | llama-cpp |
	// ollama). See ModelEngine.
	// +kubebuilder:validation:Enum=vllm;llama-cpp;ollama
	Engine ModelEngine `json:"engine"`

	// Image is the pinned image digest for this model (§8, §9). Engine
	// version is a per-model attribute; there is no "the platform's vLLM
	// version". Images MUST be userspace-only — GPU drivers come from the
	// backend (dstack's AMI on AWS/DO, the marketplace host on Vast), so an
	// image bundling its own driver stack works on one path and fails on
	// the other.
	// +kubebuilder:validation:Pattern=`^.+@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`

	// Model is WHICH model to serve — the weights, not the engine.
	//
	// This is the field whose absence made an Ollama Model unrunnable
	// (ledger D62): spec.image names the ENGINE (`ollama/ollama`,
	// `vllm/vllm-openai`), and for Ollama and llama.cpp nothing else in the
	// CR could say what to load, so the CR started an empty server. Each
	// engine interprets it in its own dialect, because there is no shared
	// registry and pretending otherwise would be a lie:
	//
	//   vllm      a HuggingFace repo    Qwen/Qwen3.8-27B-FP8
	//   ollama    an Ollama tag         qwen3:8b
	//   llama-cpp a HuggingFace repo    bartowski/Qwen3-8B-GGUF
	//
	// Whatever it names, squall makes the replica ALSO answer to this
	// Model's metadata.name, so callers address every engine identically
	// (vLLM via --served-model-name, Ollama via `ollama cp`). That name is
	// what goes in the OpenAI request body.
	//
	// Optional: an image with the weights baked in needs nothing here, and
	// spec.args can always say it the engine's own way instead.
	// +optional
	Model string `json:"model,omitempty"`

	// Features declares what this model can serve, and is surfaced verbatim
	// as the `features` array on squall-proxy's /v1/models listing. It is a
	// declared property of the model, not something squall infers: the CR
	// author knows whether an image serves text generation, embeddings or
	// speech, and nothing squall can observe would tell it.
	//
	// Mirrors KubeAI's `spec.features`, because `type: kubeai` discovery
	// consumes this listing (F27, F30).
	// +kubebuilder:validation:MinItems=1
	Features []ModelFeature `json:"features"`

	// Owner is surfaced verbatim as `owned_by` on the /v1/models listing.
	// Empty is normal and is what KubeAI emits by default.
	// +optional
	Owner string `json:"owner,omitempty"`

	// Args is the ordered, engine-native argument list (e.g. vLLM flags).
	// +optional
	Args []string `json:"args,omitempty"`

	// Env is the engine-native environment map.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// Resources carries the dstack resource ask for the run.
	// +optional
	Resources ModelResources `json:"resources,omitempty"`

	// SecretEnv is environment variables whose VALUES come from a Secret in
	// this Model's namespace, resolved by the controller at Apply time. Use
	// it for anything that must not appear in Git — HF_TOKEN for a gated
	// model being the motivating case (D63).
	//
	// Keys here are environment variable names, exactly as in Env. A name
	// present in BOTH is rejected rather than silently resolved one way:
	// which one won would be invisible in the CR and catastrophic to guess
	// wrong.
	//
	// A missing Secret or key FAILS the reconcile loudly. It must never
	// degrade into sending an empty value — an engine started with
	// HF_TOKEN="" provisions, bills, and then fails to download, which
	// reads as a broken model rather than a missing credential.
	// +optional
	SecretEnv map[string]SecretKeyRef `json:"secretEnv,omitempty"`

	// Probe is the readiness probe dstack runs against the replica — §6
	// evidence (a). Omitted, the path is derived from spec.engine and the
	// timings default to 5s / 2 successes.
	// +optional
	Probe *ModelProbe `json:"probe,omitempty"`

	// Placement scopes and cost-bounds where dstack may provision.
	Placement ModelPlacement `json:"placement"`

	// MinReplicas is the pin switch: 0 means on-demand (the controller
	// flips 0↔1 with demand, §6), 1 means PINNED — the controller holds
	// replicas: 1 and never sleeps it. There is no range and no `scaling:`
	// block; external services are always declared to dstack with a fixed
	// replica count (F15, F16).
	// +kubebuilder:validation:Enum=0;1
	MinReplicas int32 `json:"minReplicas"`

	// HoldTimeout bounds how long squall-proxy blocks a cold request
	// waiting for the model to wake before it falls back (§7). 0 means
	// answer immediately without holding.
	// +optional
	HoldTimeout metav1.Duration `json:"holdTimeout,omitempty"`

	// ScaleDownDelaySeconds is the job-layer idle window: once in-flight
	// requests are 0 and the newest request is older than this many
	// seconds, the controller flips replicas: 0 (§6). This releases the
	// *job*, not the machine — see ModelFleet.IdleDuration for that layer.
	//
	// Defaulted (D105): this value is also hasDemand's TTL, so a zero here
	// makes an on-demand Model permanently unwakeable — the annotation the
	// proxy writes expires the instant it lands. The default is the spec's
	// own §5.1 example value, and because omitempty drops an explicit 0
	// from serialization, even a literal `scaleDownDelaySeconds: 0` reads
	// back as 300 rather than as "never wake".
	// +kubebuilder:default=300
	// +optional
	ScaleDownDelaySeconds int32 `json:"scaleDownDelaySeconds,omitempty"`

	// UncontrolledTimeout bounds how long capacity may stay up while idle
	// evidence is unavailable. Nil defaults to min(4x the idle window + 15m,
	// 2h); an explicit value must be in (0, 24h], and zero is rejected: there
	// is no opt-out from a bound on a GPU squall cannot see (D154).
	// +optional
	UncontrolledTimeout *metav1.Duration `json:"uncontrolledTimeout,omitempty"`

	// Health is the liveness policy for a replica that is already UP: how
	// squall decides it is taking traffic and delivering nothing, and tears it
	// down. Deliberately NOT part of spec.probe, which is a straight passthrough
	// to dstack's own HTTP probe — these thresholds count REAL completions, not
	// probe results, and conflating the two would suggest squall probes
	// something itself. It does not.
	// +kubebuilder:default={}
	// +optional
	Health ModelHealth `json:"health,omitempty"`

	// Fleet configures the dstack fleet backing this model, in particular
	// the machine-release idle window.
	Fleet ModelFleet `json:"fleet"`

	// DrainTimeout bounds the in-flight drain the deletion finalizer
	// performs before runs are deleted and the fleet released (§5.2).
	//
	// Defaulted (D123): with a zero here, `pastDeadline` is true on the
	// finalizer's FIRST pass and T12's bounded drain never runs — a delete
	// cuts any generation in flight immediately. The default is the spec's
	// own §5.1 example value.
	// +kubebuilder:default="120s"
	// +optional
	DrainTimeout metav1.Duration `json:"drainTimeout,omitempty"`

	// ProvisioningTimeout is the single age-based DESTRUCTIVE trigger in
	// the whole controller contract (§5.2): a run that never reaches Ready
	// within this window is destroyed, alarmed, and marked Dead. No other
	// timeout in this spec destroys anything — see MaxLifetime.
	ProvisioningTimeout metav1.Duration `json:"provisioningTimeout"`

	// MaxLifetime is ALERT-ONLY (§5.2) — it is exported as a metric pair
	// (§10) and never actuated, and specifically never implemented via
	// dstack's native max_duration (F14, F20: a native hard stop is a
	// scheduled outage plus a full recreate). It stays meaningful even
	// under MinReplicas: 1, because that is precisely the safety net for a
	// pinned GPU everyone forgot about.
	// +optional
	MaxLifetime metav1.Duration `json:"maxLifetime,omitempty"`
}

// ModelHealth is squall's answer to "is this replica worth paying for?" for a
// run that is already up and whose probes are ready.
//
// It exists because in-flight requests are what block the idle flip, which
// gives a failing replica an inverted incentive: the worse it performs, the
// more requests pile up inside it, and the more "in use" it looks. MEASURED
// 2026-08-29 — 57 requests hung ~305s each all night, and the Model was never
// eligible to sleep for one second of it.
//
// The two fields are ANDed and are different in kind on purpose. UnhealthyAfter
// says "this has gone on long enough to not be a blip"; FailureThreshold says
// "we asked enough times to be sure". Either alone has a cheap counter-example.
//
// This does NOT catch a merely SLOW replica (D94). On the night that produced
// it, gaps between successful responses were p50 2s / p90 59s / max 1030s —
// correct output, too slowly, which is a capacity problem and not a liveness
// one. Do not retune these fields to try to cover it.
type ModelHealth struct {
	// UnhealthyAfter is how long requests may keep arriving with nothing
	// delivered before the run is flipped to zero replicas and left Asleep
	// until the next request wakes a fresh one.
	//
	// The clock is anchored at the LATER of the newest success and
	// status.wakeStartedAt, so a freshly woken run that has not served anything
	// yet is measured from its own wake and not from the epoch.
	//
	// Zero DISABLES the whole check. The default is 15m and must stay
	// comfortably above the slowest legitimate generation: an unfinished
	// request is not a success, so a window below the worst-case generation
	// time would tear down a working GPU mid-answer.
	// +kubebuilder:default="15m"
	// +optional
	UnhealthyAfter metav1.Duration `json:"unhealthyAfter,omitempty"`

	// FailureThreshold is how many requests must have failed since the last
	// success before UnhealthyAfter is allowed to fire — Kubernetes'
	// failureThreshold by another name, and the symmetric twin of the probe's
	// readyAfter. Time alone has a cheap counter-example: a Model that served
	// fine, went quiet for twenty minutes and then failed ONE request has a
	// twenty-minute-old last success and current traffic, and would be torn
	// down on the evidence of that single failure.
	//
	// A failure is a committed non-2xx, or a Model advertised Ready whose
	// gateway would not serve. A client disconnecting mid-stream is not the
	// replica's fault and is not counted. The count RESETS on every success, so
	// it is consecutive failures, not a lifetime total.
	//
	// It CANNOT be disabled. A value <= 0 is read as the default 3 rather than
	// as "fire with no evidence": 1->0 fails safe, and the one setting that must
	// never be reachable is a destructive trigger with no floor under it. To
	// turn the whole check off, set unhealthyAfter to 0.
	// +kubebuilder:default=3
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`
}

// ModelPhase is the controller's sole-writer status enum (§5.2, §6).
type ModelPhase string

const (
	// ModelPhaseAsleep is replicas: 0, still registered and routable, the
	// gateway answers 503. The action on demand is a flip, not a recreate
	// — the run/fleet identity survives (F17, F20). Asleep and Dead are
	// deliberately different states with different actions (F20).
	ModelPhaseAsleep ModelPhase = "Asleep"
	// ModelPhaseWaking is set the moment the controller applies
	// replicas: 1, before dstack reports the run ready.
	ModelPhaseWaking ModelPhase = "Waking"
	// ModelPhaseReady has one definition and one writer (§6): engine
	// health endpoint succeeded, warmup request succeeded, and dstack
	// reports the replica ready. "dstack job running" is never Ready.
	ModelPhaseReady ModelPhase = "Ready"
	// ModelPhaseDraining is the finalizer's bounded in-flight drain
	// (DrainTimeout) before runs are deleted (§5.2).
	ModelPhaseDraining ModelPhase = "Draining"
	// ModelPhaseRecreating is set when a Dead model is being replaced by a
	// fresh run generation.
	ModelPhaseRecreating ModelPhase = "Recreating"
	// ModelPhaseDead is terminal and deregistered — the gateway answers
	// 404. The action is a recreate (new run, full provisioning latency),
	// plus an alarm when death was uncommanded (F20). status.runId is
	// invalidated on entry to this phase.
	ModelPhaseDead ModelPhase = "Dead"
)

// DemandAnnotation is the coalesced proxy signal (§5.2): "there is demand
// for this Model since t" (§7). squall-proxy (Phase 9) WRITES this
// annotation on the data path; squall-controller READS it (hasDemand in
// internal/controller/squall/model_controller.go). It lives here, not in
// internal/controller/squall, because that package pulls in
// controller-runtime and client-go — dependencies squall-proxy must not
// carry (it is a thin, stateless data-path binary in a separate failure
// domain). This package is the lightweight contract both binaries already
// import. The value MUST be an RFC3339 timestamp: hasDemand treats it as
// demand only while now is within spec.ScaleDownDelaySeconds of that
// instant (block 7+8 plan §2's self-expiry) — a stale value the proxy
// failed to clear must not pin a Model awake forever, and a value that
// fails to parse resolves to "no demand", never to the old
// presence-forever reading.
const DemandAnnotation = "squall.ackstorm.ai/demand-since"

// ReplicaEndpoint is the SSH path to a running replica's engine port.
//
// Everything here comes from dstack's own run record — job_provisioning_data
// for the SSH endpoint, job_spec.service_port for the far end — so it is
// reported, never invented. The engine port is NOT publicly reachable
// (measured: :8000 refuses, the SSH port answers), which is both why a tunnel
// is required and why a rented GPU is not an open endpoint.
type ReplicaEndpoint struct {
	// Host is the replica's publicly reachable SSH host.
	Host string `json:"host"`

	// SSHPort is its SSH port. On marketplace backends this is a per-instance
	// mapped port, not 22.
	SSHPort int32 `json:"sshPort"`

	// User is the SSH user dstack provisioned the container for — "root" on
	// every backend squall supports as of 0.21.2.
	User string `json:"user"`

	// ServicePort is where the engine listens inside the container: the far
	// end of the forward, NOT a port reachable from outside.
	ServicePort int32 `json:"servicePort"`
}

// FleetStatus reports, per backend this Model may draw instances from, whether
// dstack actually has a pool that admits it.
type FleetStatus struct {
	// Backend is the dstack backend name, e.g. "vastai".
	Backend string `json:"backend"`

	// Name is the fleet squall created (dstack.FleetName). Empty when the
	// backend is not configured, and empty on Admitting: dstack only reports
	// THAT some active fleet admits the backend, not which one, and naming
	// the auto fleet here misled operators whose own declared fleet was the
	// one admitting (D149).
	// +optional
	Name string `json:"name,omitempty"`

	// State is what squall found for this backend.
	// +kubebuilder:validation:Enum=Admitting;Created;Unfleeted;Unconfigured
	State string `json:"state"`
}

// ModelStatus defines the observed state of Model.
type ModelStatus struct {
	// Phase is the controller's sole-writer state; see ModelPhase. The
	// proxy branches on this value and never probes the engine itself.
	// +optional
	Phase ModelPhase `json:"phase,omitempty"`

	// RunId is a MUTABLE POINTER, not an identity (F17, F20): it stays
	// stable across 0↔1 flips within one run generation, and is
	// invalidated when the model enters a terminal state. Identity is
	// owned by metadata.uid, carried as a tag/label on every dstack run,
	// fleet, and provider resource — never by this field.
	// +optional
	RunID string `json:"runId,omitempty"`

	// RunName is the name this Model's dstack run is filed under. It is
	// normally "<namespace>-<name>": a dstack run name is global to the
	// server, so keying it on the bare Model name let two Models of the
	// same name in different namespaces silently drive ONE run (F1).
	//
	// It is recorded rather than always recomputed because a run minted
	// before F1 carries the bare name, and that run is a paid GPU. Once
	// resolved, this field pins the identity for the Model's lifetime, so
	// teardown and the orphan reaper target the run that actually exists.
	// +optional
	RunName string `json:"runName,omitempty"`

	// RunUID is the Model UID that the dstack run under RunName was minted
	// for, as read back from the run itself. It normally equals this
	// object's own metadata.uid; when it does not, the run outlived a
	// previous Model of the same namespace/name.
	// +optional
	RunUID string `json:"runUID,omitempty"`

	// DeploymentNum is the idempotency token within a run generation: it
	// lets the controller recognize its own prior actuation (e.g. across a
	// restart or a watch replay) without creating a second run.
	// +optional
	DeploymentNum int64 `json:"deploymentNum,omitempty"`

	// ServiceURL is where a Ready replica's traffic goes, as dstack reported
	// it — a path relative to the dstack server's base URL, e.g.
	// "/proxy/services/main/qwen3-8-27b/".
	//
	// This closes D25. Before it, squall-proxy resolved a backend from a
	// printf template in an env var, which meant an installed chart could not
	// reach a real replica until an operator hand-edited it to match dstack's
	// routing. The controller already receives this on every Apply
	// (dstack.Run.ServiceURL); it just never wrote it down. Publishing it
	// here means the proxy learns it from the informer cache it is ALREADY
	// running — no new watch, no new API call on the request path.
	// +optional
	ServiceURL string `json:"serviceURL,omitempty"`

	// Replica is how to reach the live replica's engine port DIRECTLY, over
	// an SSH tunnel, without dstack's own service proxy in the request path.
	//
	// Measured 2026-08-28 against a live Vast.ai GPU (see
	// decisions-and-open-items.md): the direct path served 128 concurrent
	// requests with zero failures at 1856 tok/s, where the same load through
	// dstack failed 31 of 128 at 407 tok/s — because dstack's proxy pins two
	// database connections for the whole lifetime of every streamed
	// response, making its connection pool, not the GPU, the ceiling.
	//
	// nil is normal and always safe: nothing running, or a topology needing
	// more than one SSH hop (a Kubernetes jump pod, a dockerized backend).
	// squall-proxy then falls back to ServiceURL above, which works
	// everywhere. It is never a half-answer — an endpoint that cannot be
	// dialled would send real user traffic into a tunnel that cannot exist.
	// +optional
	Replica *ReplicaEndpoint `json:"replica,omitempty"`

	// ServedModel is what the replica said it serves, read from its own
	// GET /v1/models once the run first probes green.
	//
	// This exists because a probe is not evidence about WHICH model answered.
	// On 2026-08-27 a run whose args were dropped served vLLM's built-in
	// default, Qwen/Qwen3-0.6B, with success_streak climbing and /health
	// returning 200 the whole time — a 0.6B model standing in for a 27B one,
	// with nothing anywhere disagreeing (ledger D65). This field is the
	// disagreement.
	// +optional
	ServedModel string `json:"servedModel,omitempty"`

	// ForwardModel is the SINGLE served id squall-proxy may rewrite an
	// outbound request's "model" field to, or empty when there is no safe
	// single answer (a mismatch, or several served ids with no expectation
	// to disambiguate them).
	//
	// It exists because ServedModel above must stay the honest diagnostic —
	// D65's protection is that `kubectl get models` shows a 0.6B model
	// standing in for a 27B one — while the proxy needs exactly one name.
	// D100, measured live 2026-08-31: with one field serving both purposes,
	// an Ollama replica (which reports the `ollama cp` alias AND the source
	// weights) had every request rewritten to
	// "ollama-tiny:latest,qwen2.5:0.5b" and answered 400. The check passing
	// is what broke serving.
	// +optional
	ForwardModel string `json:"forwardModel,omitempty"`

	// WakeStartedAt is when the controller actuated the most recent 0->1
	// flip — the anchor provisioningTimeout measures from (§5.2). It is
	// deliberately NOT dstack's submitted_at: an in-place flip (F17) reuses
	// the run, so submitted_at may date from the run's first creation and
	// would fire the deadline instantly on a re-wake. Cleared when the
	// model sleeps.
	// +optional
	WakeStartedAt *metav1.Time `json:"wakeStartedAt,omitempty"`

	// Fleet reports the dstack pools backing this Model, in placement order.
	// It is a mirror, never a source of truth.
	// +optional
	// +listType=map
	// +listMapKey=backend
	Fleet []FleetStatus `json:"fleet,omitempty"`

	// LastRequestAt is the newest request instant reported by squall-proxy.
	// It survives proxy rollouts and only moves forward.
	// +optional
	LastRequestAt *metav1.Time `json:"lastRequestAt,omitempty"`

	// UncontrolledSince records when idle evidence became incomplete.
	// +optional
	UncontrolledSince *metav1.Time `json:"uncontrolledSince,omitempty"`

	// Conditions carries richer, standard status detail alongside Phase.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.placement.backends[*]`
// +kubebuilder:printcolumn:name="Engine",type=string,JSONPath=`.spec.engine`
// +kubebuilder:printcolumn:name="Run",type=string,JSONPath=`.status.runId`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Schedulable",type=string,JSONPath=`.status.conditions[?(@.type=="Schedulable")].status`
// +kubebuilder:printcolumn:name="Fleet",type=string,JSONPath=`.status.fleet[*].state`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Served",type=string,JSONPath=`.status.servedModel`,priority=1
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Schedulable")].reason`,priority=1

// Model is the Schema for the models API.
type Model struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelSpec   `json:"spec,omitempty"`
	Status ModelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelList contains a list of Model.
type ModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Model `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Model{}, &ModelList{})
}
