# Squall — External GPU Capacity for Model Serving — SPEC v0.18

**Status:** RC — implementation baseline; build against this. Graduates to FINAL when PoC 0 closes and PoC 4 verifies per-backend instance release (esp. Vast, F21 caveat)
**Date:** 2026-08-26
**Supersedes:** v0.17; v0.2 (KubeAI + Liqo execution cells) remains parked — see §2
**Project name:** Squall
**Scope:** PoC / v0.1 implementation target
**Use case:** Model serving. Only model serving.
**Desired state:** `squall.ackstorm.ai/v1alpha1 Model` CRs in Git, reconciled by Flux — the platform's established pattern (F27)
**Components (ours):** `squall-controller` (Go, controller-runtime) + `squall-proxy` (Go, data path)
**Front plane:** LiteLLM — vanilla, zero custom hooks; registration via alitellm-operator discovery of the proxy's `/v1/models` (F27)
**Provisioner / actuator:** dstack (self-hosted server + gateway)
**Providers (v0.1):** Vast.ai, AWS (remote region; G-family suffices — F29), and DigitalOcean — reinstated by decision (Appendix A)
**Engines:** vLLM (preferred), llama.cpp server, Ollama — as templates behind one protocol boundary (§8)
**Scale-from-zero:** request-triggered; controller-owned fixed-replica flip (§6)
**Scale-to-zero:** controller-owned flip on idle signal; `minReplicas: 1` pins capacity awake; gateway mandatory (server-enforced, F1)
**Networking:** private client→LiteLLM→proxy→gateway path; no public HTTP ingress; provider transport is either routed-private (AWS) or outbound SSH tunnel over the public Internet (marketplace/other public-SSH backends)
**Kubernetes involvement:** intent and coordination only — no K8s in the serving compute

---

## 0. Summary

Squall serves LLMs on GPU capacity that does not exist in the primary region,
provisioned on demand from multiple providers, waking from zero on the first
request and disappearing when idle — declared exactly the way this platform
already declares models: a CR in Git, discovered into LiteLLM, reconciled by
a controller.

```text
                       Git (Model CRs, Flux)            ◄── DESIRED STATE
                              │
                              ▼
                      squall-controller ──────────► dstack server API
                      (reconcile, flip 0↔1,         (runs, fleets — no force)
                       status, finalizers)
                              │ writes status
                              ▼
   client ──► LiteLLM ──► squall-proxy ──► dstack gateway ──► SSH ──► replica
              (vanilla;   (per request:      (in VPC,          (outbound)  (vLLM /
               registered  forward | 503 |    public_ip:false)             llama.cpp /
               via         blocking hold;                                    Ollama)
               discovery)  demand patches)
```

Division of labour, fixed:

- **Git + Flux** — where a human declares a model: one `Model` CR per model,
  reviewable next to its in-cluster siblings (F27).
- **squall-controller** — reconciliation only: the **0↔1 replica flip** (wake
  on demand, sleep on idle, pinned when `minReplicas: 1`), drain-first
  teardown via finalizer, orphan reconciliation, status as the shared truth.
- **squall-proxy** — the data path decision, per request: forward when Ready;
  answer the wait honestly when not (§7); emit coalesced demand. It is where
  "is it warm?" is answered — from status, never by probing.
- **alitellm-operator** — the only component that touches LiteLLM, exactly as
  today: a discovery source reads the proxy's `/v1/models` and generates
  `LiteLLMModel` CRs with the external router profile (F27, F28).
- **dstack** — multi-provider provisioning, offer/price selection, run
  execution, fleets/instances, SSH tunnels, ingress via gateway. Its
  autoscaler is out of the loop (§6).
- **Engines** — vLLM / llama.cpp / Ollama, one protocol boundary (§8).

Squall builds no scheduler, no load-based autoscaler, no VPN, no Kubernetes
federation, no second OpenAI mux — and touches neither the LiteLLM API nor
Redis (§11).

---

## 1. Goals

v0.1 MUST allow a `Model` CR merged to Git to be served from external GPU
capacity such that:

1. A request to a cold model triggers provisioning (wake-on-request).
2. The first-request experience follows the per-model `holdTimeout` (§7):
   block up to it, then answer the wait contract truthfully — `0` answers
   immediately and lets LiteLLM's router fall back.
3. Subsequent requests are served normally through LiteLLM.
4. v0.1 capacity is a fixed-replica `0↔1` flip; load-based `1..N` is
   deferred and, when needed, is a designed change to §6.
5. Capacity scales to zero when idle — at the instance layer (F21) — and
   idle genuinely bills zero (§10). `minReplicas: 1` (pinned) declares the
   opposite intent: a fixed GPU that never sleeps, alive as long as its CR
   exists, guarded by the `maxLifetime` age alert (§5).
6. Deleting the CR (including Flux prune) tears capacity down safely and
   drain-first via finalizer; an unreadable or partially-read desired state
   MUST NEVER be interpreted as empty.
7. Provider choice is a configuration detail per model. The CR states the
   requirement as dstack's own `gpu:` spec (F33) — named cards where
   bandwidth matters, memory as a native range — plus a €/h ceiling; v0.1
   configures all three backends, each resolving the card list to its own
   catalog (F12: a DO leg needs a card DO sells — L40S-class for the
   24–48 GB target).
8. Client → LiteLLM → proxy → gateway remains private. No provider exposes a
   public HTTP inference endpoint. Marketplace traffic crosses the public
   Internet only inside the outbound dstack SSH tunnel.
9. A burst of N simultaneous requests to a cold model produces exactly one
   effective wake action.
10. No component of ours holds long-lived provider credentials outside the
    dstack server. Orphan detection at v0.1 scale is a manual drill (AC7);
    automated billing triangulation and its read-only identity are §15.

---

## 2. Disposition of prior designs

- **v0.2 (KubeAI + Liqo)** solved a different problem — external GPUs as
  Kubernetes nodes for arbitrary workloads. Parked, not deleted; revisit only
  if that requirement materializes.
- **KubeAI the product** remains rejected (control+data plane coupling, shim
  required). **KubeAI's shape is adopted**: a CRD, a discovery routine, and a
  queueing proxy between the router and the capacity — built by us, sized for
  exactly this platform. The earlier conclusion that queueing belongs in a
  LiteLLM pre-call hook is withdrawn: a hook cannot own the request
  lifecycle — block, answer, or time out on its own terms — and correctness
  should not depend on LiteLLM hook semantics. LiteLLM runs **vanilla**.
- **§5.1's registry-as-desired-state (v0.8–v0.10)** is withdrawn: it inverted
  the platform's production pattern (F27). Intent lives in Git as a CR;
  LiteLLM is a generated artifact of discovery, never a place humans edit.

---

## 3. Non-goals

Outside v0.1 (and mostly outside this platform, period):

- KubeAI, Liqo, k3s execution cells, EKS Hybrid Nodes, virtual kubelets;
- any custom **load-based `1..N`** scheduler or autoscaler ("doing Karpenter"
  remains forbidden). The demand/idle `0↔1` flip is the wake/sleep function,
  not an autoscaler — and dstack's RPS autoscaler is deliberately not used
  (§6, F15);
- LiteLLM custom hooks, callbacks, or direct LiteLLM API calls from squall —
  LiteLLM is vanilla and registration belongs to alitellm-operator (F27);
- Redis, external or embedded, in v0.1 (§11);
- our own VPN/WireGuard lifecycle (delegate or don't);
- training / fine-tuning (future via dstack tasks, §15);
- multi-node / distributed inference;
- serving regulated-client traffic from marketplace hosts (§12);
- GitOps reconciliation of **dstack objects** (runs/fleets/gateway are
  API-actuated by the controller; Git holds intent, not dstack state);
- billing-granularity idle optimization (per-second Vast makes it a no-op on
  the primary backend — deferred to §15 with its per-provider table);
- distributed cache coherence for model artifacts.

---

## 4. Verified platform facts (evidence, not assumptions)

The following were verified against dstack documentation and source code
(2026-08; dstack 0.21.x), plus provider documentation where noted, and are
load-bearing for this design:

| # | Fact | Source |
|---|------|--------|
| F1 | Auto-scaling (`replicas.min != max`) without a gateway is rejected by **server-side validation**; a gateway is mandatory for anything but fixed gateway-less replicas | source: `server/services/services/__init__.py` (`_register_service_in_server`) |
| F2 | Gateway supports `public_ip: false` (aws, gcp) → private gateway; `certificate: null` (HTTP) or `acm` (TLS at ALB; with private IP requires private subnets + NAT) | docs/concepts/gateways |
| F3 | Gateway supports up to 3 replicas (`GATEWAY_MAX_REPLICAS`; docs still label the feature experimental). The ACM requirement is for **HTTPS**, not for replication: validation rejects `replicas > 1` **only** for `lets-encrypt`, and upstream's own error text names `certificate: null` as a supported value for a replicated gateway. Balancing is external — multi-value DNS or an LB outside dstack | source: `server/services/gateways/__init__.py` (verified) + docs/concepts/gateways |
| F4 | Native backends include `vastai`, `aws`, `digitalocean`; offers are compared across configured backends. Gateways can be **hosted** only on aws/gcp/azure/kubernetes (`ComputeWithGatewaySupport`), but per the official gateways doc a gateway on one backend fronts services on any other, "including backends where gateways themselves cannot be created" — smoke-tested in PoC 0 | docs/concepts/backends, docs/concepts/gateways, source: `backends/*/compute.py` |
| F5 | AWS backend supports `public_ips: false` → replicas in private subnets (NAT for egress) | docs/concepts/backends |
| F6 | All connections are **outbound from our side**: server/gateway SSH → instance. Vast backend requires offers with `direct_port_count >= 1` (mapped SSH port) | source: `backends/vastai/compute.py` |
| F7 | SSH auth uses a **project-wide RSA keypair** generated at project creation and stored in the server DB; only **public** keys are installed on instances | source: `server/services/projects.py`, `backends/base/compute.py` |
| F8 | Host key verification is disabled: `StrictHostKeyChecking=no` + `UserKnownHostsFile=/dev/null` hardcoded | source: `core/services/ssh/tunnel.py` |
| F9 | Volumes exist to warm cold starts; support varies by backend | dstack blog/docs |
| F10 | Vast offer ordering is a `backend_options` option `offer_order: score \| price`. **Default is `score`** (the Vast console's composite ranking, highest score first), NOT cheapest-first. `price` selects lowest cost first; upstream's own docstring warns lower-cost offers are often less reliable and recommends stricter filters with `price`. Companion knobs: `min_reliability` (default 0.9) and `min_score`. Cost-minimizing pools MUST set `offer_order: price` explicitly and SHOULD compensate with raised `min_reliability` / `min_score` | source: `backends/vastai/profile_options.py` (verified in source) |
| F11 | Private AWS replicas still require routed network reachability from the dstack server/gateway to the target private subnets; dstack does not create cross-region network fabric | docs/concepts/backends / deployment topology |
| F12 | **Single-A100 exists nowhere in scope.** AWS sells A100 only as 8× (p4d/p4de, live `describe-instance-types` across 7 regions); single-H100 = `p5.4xlarge`, only eu-west-2/us-east-1/us-west-2. Platform region eu-west-1 offers only g5 (A10G) and 8×-A100 p4d. DigitalOcean (2026-08): H100/H200/L40S/RTX Ada/MI300X — no A100; EU GPU = Amsterdam (AMS3, H100) only | live AWS API (implementer, 2026-08-25); docs.digitalocean.com |
| F13 | llama.cpp `llama-server` exposes OpenAI-compatible `/v1/chat/completions`, a `/health` liveness endpoint, and Prometheus `/metrics` behind `--metrics`; upstream makes "no strong claims" of full OpenAI-spec compatibility (e.g. no function calling) | llama.cpp tools/server README |
| F14 | dstack native `max_duration` stops a run after its **running-state** time elapses and explicitly **excludes provisioning and pulling**; `utilization_policy` (`min_gpu_utilization` over `time_window`) terminates low-utilization runs | source: `core/models/profiles.py` (verified in source) |
| F15 | Replica/scaling validation is **symmetric**: a range requires `scaling`, and a fixed count **forbids** `scaling` ("To use `scaling`, `replicas` must be set to a range") | source: `core/models/configurations.py` `validate_scaling` (verified in source) |
| F16 | With `scaling:` present, the RPS autoscaler scales up **immediately** at 0 replicas, and returns the current count when gateway stats are absent; with `scaling:` absent, `ManualScaler` only clamps and never raises a count | source: `server/services/services/autoscalers.py` (verified in source) |
| F17 | `replicas` is an **in-place** updatable service field: apply updates the same `RunModel.id` and increments `deployment_num` — no new run. Fixed `replicas: 0` is accepted and yields a **registered, routable service with zero jobs** (asleep-but-addressable is first-class) | source: `server/services/runs/spec.py`, `runs/__init__.py` (verified in source) |
| F18 | `apply_plan` enforces optimistic concurrency: an apply computed against changed state fails ("Resource has been changed. Try again or use force apply") unless `force=True` | source: `server/services/runs/__init__.py:676` (verified in source) |
| F19 | Gateway-less services are proxied by the dstack **server itself** at `/proxy/services/{project}/{run}/` — a second front path | source: `server/services/services/__init__.py` |
| F20 | The run id survives flips (`deployment_num`++ in place) but **not terminal states**: a terminal run is **deregistered from the gateway (404, not 503)** and the next apply falls through `apply_plan`'s `is_finished()` branch into `submit_run`, minting a **new run id**. Dead ≠ asleep | measured (PoC 0-CPU) + source: `runs/__init__.py` is_finished branches |
| F21 | Runs land on **fleets**; with no matching fleet the run fails (`failed_to_start_due_to_no_capacity`). Flipping `replicas` to 0 terminates the **job** but the **instance** is released only by fleet `idle_duration` — defaults are `5m` for runs and **`3d` for fleets**; on reuse the fleet's value applies, on fresh provisioning the shorter of the two. **Since the single-idle-window change, squall always sends an explicit `idle_duration: 0` (D166), so neither default is ever reached — this is a fact about dstack, not a Model author's knob** | measured (319 s release with 5m set) + source: `core/models/profiles.py` |
| F21b | **"Only applied for VM-based backends" is a hard gate, not a caveat: on a non-dockerized backend `idle_duration` is never read.** `_create_instance_model_for_job` branches on `job_provisioning_data.dockerized` — false forces `termination_idle_time = 0` (release on the first pass after the job stops) and only the true branch calls `get_termination(profile, ...)`. `dockerized=True` on AWS ("because `dstack-shim` is used"); **`False` on Vast.ai and on Kubernetes**. So on both backends squall ships against there is **no warm pool**: every wake is a full cold provision, and `idle_duration: -1` (`DONT_DESTROY`) is unreachable too. §7's warm-window premise holds only on VM backends — see D158. **Moot since the single-idle-window change: squall sends `idle_duration: 0` on every backend regardless of `dockerized`, so the dockerized branch is never the one that matters** | source: `vastai/compute.py:173`, `kubernetes/compute.py:339`, `aws/compute.py:418`, `jobs_submitted.py:1793`, `instances/check.py:92` |
| F22 | The gateway dials **out** to the replica (SSH local forward of the replica's `localhost:app_port` onto a gateway Unix socket) using the **project private key resident on the gateway**. Replicas never initiate toward the gateway; a private gateway needs no public IP and no certificate. Marketplace data path = outbound TCP/22 from the gateway | source: `proxy/lib/services/service_connection.py` (verified) + measured |
| F23 | Gateway responses are immediate (~10–20 ms), never held: registered + 0 replicas + auth → **503**; unregistered/terminal → **404**; bad/missing token → **403**. The gateway never wakes a `ManualScaler` service | measured (PoC 0-CPU) |
| F24 | A `certificate: null` gateway requires `https: false` on every service; the server refuses otherwise | measured (PoC 0-CPU) |
| F25 | The server logs the **admin token in plaintext to stdout on every start** (`app.py:196`), from where log pipelines ingest it | source + measured; unmitigated upstream |
| F26 | The gateway defaults to `t3.micro` (`DEFAULT_GATEWAY_INSTANCE_TYPE`) — 2 vCPU burstable, 1 GB RAM; `instance_type` is configurable on `GatewayConfiguration`. Burstable instances throttle to baseline once CPU credits are exhausted | source: `core/backends/aws/compute.py` (verified in source) |
| F27 | The platform's established seam is **CRD → discovery → LiteLLM**: `kubeai.org/v1 Model` CRs in Git (Flux), and the alitellm-operator's `kubeai-discovery` generates `LiteLLMModel` CRs from them. LiteLLM is never hand-edited. Squall plugs into the same mechanism by exposing `/v1/models` for a discovery source to consume | platform: gitops-genai-blueprint + live cluster (10 models in production) |
| F28 | Generated router params for in-cluster models are `timeout: 300`, `stream_timeout: 60`, `rpm: 25` — an order of magnitude below external cold starts. External models need a **distinct generated router profile**; `hold` without it exceeds the router's own limits before any client is reached | platform (generated `LiteLLMModel` params) + measured |
| F29 | The target model (qwen3.8-27b, Q4_K_M) measures ~16 GiB weights on GPU with 32 KiB/token KV at q8_0 (16/64 full-attention layers) → a 64k window fits a **24–32 GB card**. Live EU Vast inventory: RTX3090 24 GB from ~$0.26/h, RTX5090 32 GB from ~$0.63/h. The external target class is 24–48 GB, not 80 GB | measured from the serving engine + live Vast inventory |
| F30 | alitellm-operator discovery `spec.params` is a **verbatim pass-through bag propagated to every child** of one discovery source (MDISC-23); only `model` and (bedrock) `aws_region_name` are overlaid — **there is no per-model router param**. Per-model rate ceilings require class-partitioned discovery (one CR per class; `spec.filters` matches the model ID / distinct `baseUrl`). `type: kubeai` against any OpenAI-compatible listing yields the `baseUrl → params.api_base` overlay and `hosted_vllm/<id>` naming — the seam needs **no new operator code** | platform source: `api/litellm/v1alpha1/modeldiscovery_types.go`, `internal/providers/kubeai.go` |
| F31 | Discovery refresh is atomic and fail-closed (D-09): an unreachable source writes status and returns — no enumeration, no diff, no deletions; existing children stay untouched. A squall-proxy outage does **not** deregister external models from LiteLLM | platform source (alitellm-operator) |
| F32 | KubeAI's hold **blocks**: `AwaitBestAddress(ctx, …)` writes nothing to the client while waiting and answers `504` on `context.DeadlineExceeded`. LiteLLM applies no wall-clock deadline to a request — httpx bounds the gap between chunks — so raised `timeout`/`stream_timeout` suffice at that hop | source: kubeai `internal/modelproxy/handler.go` (verified in source) + litellm internals (implementer-verified) |
| F33 | dstack's native `GPUSpec` carries `vendor`, `name: List[str]` (backend-agnostic), `count: Range`, `memory: Range` (`"24GB..32GB"`), `total_memory`, `compute_capability` — the CR passes it through; no invented GPU schema and no translation layer | source: `core/models/resources.py` (verified in source) |
| F35 | dstack services carry **first-class HTTP probes** (`ProbeConfig`: `ready_after`, `until_ready`), and **per-probe state is exposed on the API** the client already consumes: `JobSubmission.probes: list[Probe]`, alongside `submitted_at`/`age`. The §6 readiness chain exists end-to-end in dstack; squall only reads it | source: `core/models/configurations.py`, `core/models/runs.py` (verified in source) |
| F36 | `max_duration` stops a fixed `replicas: 1` service; it does not respawn. `MAX_DURATION_EXCEEDED` maps to `JobStatus.TERMINATED` (not `FAILED`), so no retry event is produced and the run transitions to `TERMINATING`. Enforcement is in the runner inside the container, not a server background task | source-verified in deployed dstack 0.21.2; wire confirmed `max_duration: "2m"` → `job_spec.max_duration: 120` |
| F34 | Launch is **one OCI image everywhere; how it lands differs per backend and matters to nothing declared**: Vast is `dockerized=False` — the model image *is* the instance (`create_instance(image_name=…, onstart=…, registry_auth=…)`); AWS and DigitalOcean are `dockerized=True` — dstack's AMI boots, `dstack-shim` pulls and runs the image inside the VM. Vast-console "templates" are unused. Consequences: images MUST be userspace-only (drivers come from the AMI or the marketplace host), cold-start curves differ in shape (pull-at-create vs boot-then-pull), and `registry_auth` travels to the provider host on Vast | source: `backends/vastai/compute.py`, `backends/aws/compute.py` ("because `dstack-shim` is used"), `backends/digitalocean_base/compute.py` (verified in source) |

Consequences of F7/F8/F11 are handled in §12; F12 in §12.3 and AC1; F13 in §8/PoC 8; F14 in §5.2 and §6; F15–F18 define §6's flip mechanism and §5.2's concurrency rules; F19 in §12.2.6; F20 in §5.2/§7; F21 in §5.1/§6/PoC 4; F22 in §12.2; F23 in §7; F24 in §8; F25 in §10; F26 in §12.1 (and applies to the squall-proxy); F27–F28 in §5/§7; F29 re-derives §12.3's region analysis and closes the P-family quota question; F30–F31 shape PoC 10; F32 is the basis of §7's blocking hold; F33 is §5.1's GPU passthrough; F34 is §8's launch model and §9's registry rule; F35 is §6's `Ready` evidence and §5.2's age anchor.

**Wake ownership decision (v0.1):** the reconciler owns `0→1`. This is a
product-level consequence of the first-request policies: `fallback` and
`fail_fast` deliberately do not send the cold request to the target, and
`hold` waits for readiness before forwarding. Therefore a gateway request
cannot be the only wake signal. PoC 2 still characterizes dstack's native
zero-replica behavior because it affects failure semantics and future
simplification, but it does not change v0.1 wake ownership.

---

## 5. Desired state: the `Model` CRD

### 5.1 Shape

Group `squall.ackstorm.ai/v1alpha1`, kind `Model`, deliberately shaped after
`kubeai.org/v1 Model` (F27) so a reviewer sees the same thing in the MR as
the ten in-cluster models beside it, and moving a model in-cluster ↔ external
is mostly a placement diff. Files live with their siblings
(`workloads/models/external/*.yaml`) under a Flux Kustomization.

```yaml
apiVersion: squall.ackstorm.ai/v1alpha1
kind: Model
metadata:
  name: qwen3-8-27b
  namespace: squall
spec:
  engine: ollama                    # vllm | llama-cpp | ollama (§8)
  image: ollama/ollama@sha256:...   # pinned digest, per model (§8, §9)
  args: []                          # ordered, engine-native (e.g. vLLM flags)
  env:                              # map, engine-native
    OLLAMA_CONTEXT_LENGTH: "65536"
    OLLAMA_FLASH_ATTENTION: "1"
    OLLAMA_KV_CACHE_TYPE: q8_0
  resources:
    gpu:                            # dstack GPUSpec, passed through (F33)
      name: [A10G, RTX3090, RTX5090]  # bandwidth requirement, stated as hardware
      memory: 24GB..32GB              # native range
  placement:
    backends: [vastai]              # compliance allowlist (§12.3)
    regions: []                     # backend-native filters (e.g. EU geo on Vast)
    maxPricePerHour: 0.80           # the cost control: enforced before provisioning
  minReplicas: 0                    # 0 = on-demand flip; 1 = PINNED (never sleeps)
  holdTimeout: 20m                  # 0 = answer immediately (router falls back)
  idleTimeout: 5m                   # the ONE idle window: releases job AND machine (§6)
  drainTimeout: 120s
  provisioningTimeout: 45m          # the one destructive safety (§5.2)
  maxLifetime: 168h                 # ALERT-ONLY; exported as a metric pair (§10)
  hardStop: 24h                     # DEAD-MAN'S SWITCH; on-demand only (§5.2, F36)
status:
  phase: Asleep | Waking | Ready | Draining | Recreating | Dead
  runId: "..."                      # mutable pointer (F20)
  wakeStartedAt: "..."              # journaled at 0→1 actuation; provisioningTimeout anchor
  deploymentNum: 7                  # idempotency token within a run generation
  conditions: [...]
```

Validation (webhook/CEL), enforced not documented:

- `engine`/`args`/`env`/`image` are the three shapes production models
  actually need (F27): an ordered args list, an env map, a per-model image
  pin. `resources.gpu` is dstack's own `GPUSpec` passed through (F33) — the
  same passthrough principle, because VRAM GiB cannot express a bandwidth
  requirement and a card list can.
- Deadlines are ordered: `holdTimeout ≤ provisioningTimeout`, and PoC 10
  verifies the external router profile's `timeout`/`stream_timeout` ≥
  `holdTimeout` (F28, F32).
- Reject `idleTimeout <= 0` and `provisioningTimeout <= 0`: the first makes an
  on-demand Model permanently unwakeable (it is also `hasDemand`'s TTL), the
  second removes the primary bound on a run that never reaches Ready.
- Warn when `holdTimeout` is short enough that most wakes — which are always
  a full cold start, on every backend — cannot finish inside it:
  `holdTimeout < provisioningTimeout / 2`. Coarse and named as a heuristic;
  it is the threshold that separates a measured 9m53s cold start against a
  30m `provisioningTimeout` from a `holdTimeout` short enough to answer
  `503` to every cold request.
- `minReplicas: 1` disables the idle flip; `maxLifetime` stays — it is
  precisely the safety net for a pinned GPU everyone forgot.
- `scaling:`-style fields do not exist in the CRD at all.

### 5.2 Controller contract

One reconciler (Go, controller-runtime — the house pattern), watching
`Model` CRs. Its only inputs: CRs, demand patches, dstack API state.

- **Level-triggered and idempotent.** Demand is a coalesced annotation patch
  from the proxy ("there is demand for X since t"), never an instruction.
  Requeues, watch replays, and duplicate patches converge to the same single
  actuation. Kubernetes optimistic concurrency (`resourceVersion`) is the CAS
  on the intent side; dstack's no-force apply (F18) is the CAS on the
  actuation side. Both failures mean re-read and re-plan, never overwrite.
- **Identity is ours; the run id is a pointer.** `metadata.uid` of the CR is
  the owned identity, carried as tag/label on every run, fleet, and provider
  resource. `status.runId` is the mutable pointer beneath it — stable across
  flips (F17), invalidated by terminal states (F20). `status.deploymentNum`
  is the idempotency token within a run generation.
- **Asleep and dead are different states with different actions.** Asleep =
  `replicas: 0`, registered, gateway 503 → the action is a flip. Dead =
  terminal, deregistered, gateway 404 → the action is a **recreate** (new
  run, full provisioning latency) plus an alarm when death was uncommanded.
  `status.phase` carries the distinction so the proxy never has to guess.
- **Deletion is a finalizer, and it is drain-first.** Every removal path —
  `kubectl delete`, Flux prune, a reverted MR — passes through the squall
  finalizer: deregister from discovery → stop accepting (proxy answers 404:
  not in desired state) → bounded in-flight drain (`drainTimeout`) → delete
  runs → release fleet → remove finalizer. A CR deletion is explicit intent
  *observed through the API server*, which dissolves v0.10's multi-cycle
  absence machinery: there is no "maybe I misread the registry" — but
  **API-server unreadability still means no destructive reconciliation** —
  and the flip rule is directional: **`0→1` fails open** (wake is permitted
  under uncertainty — the cost of a wrong wake is money), **`1→0` fails
  safe** (sleep is forbidden under any uncertainty — stale or partial
  activity data, an unreachable proxy replica, an API-server outage — because
  the cost of a wrong sleep is a killed generation). This supersedes the
  earlier "replica-count changes are non-destructive" reading: a `1→0` on a
  serving model is destructive to its in-flight work, and is gated on the §6
  activity evidence.
- **The controller journals `status.wakeStartedAt` in the same act as every
  `0→1` actuation** — it is the age anchor for `provisioningTimeout`
  (`JobSubmission.submitted_at`, F35, is the secondary witness). No external
  timer, no Deployment `progressDeadlineSeconds`, no new component: a
  timestamp written by the component that already owns the actuation.
- **`provisioningTimeout` is the single age-based destructive trigger** —
  runs that never reached ready; no drain applies; destroy, alarm, and set
  phase Dead. **`maxLifetime` is ALERT-ONLY**, never destructive, never
  implemented via dstack's native `max_duration` (F14, F20: a native hard
  stop is a scheduled outage plus full recreate).
  **`hardStop` is a separate, on-demand-only exception:** it is dstack's
  `max_duration`, enforced by the runner inside the container, so it survives
  the controller dying (F36). Firing is an incident, not routine operation.
- **Reconcile loop** (periodic, independent of events): diff CRs ↔ dstack
  runs/fleets and converge. Provider-billing triangulation is a **manual
  drill at v0.1 scale** — a human against the invoice and `dstack ps`, the
  gateway instance included (§11); automating it, with its own read-only
  identity, is §15.

The controller MUST NOT implement load-based `1..N` scaling (§3), MUST NOT
route, and MUST NOT act as a request-level meter. Consuming aggregate idle
signals for the sleep flip is explicitly in scope and is not metering — §6.

---

## 6. Wake and sleep (the 0↔1 flip)

External services are declared to dstack with a **fixed replica count — never
a range, never a `scaling:` block** (F15 makes this enforced, not policy; F16
makes native wake unreachable by construction). Asleep = `replicas: 0` —
registered and routable with zero jobs, a first-class dstack state (F17).
Awake = `replicas: 1`. Pinned (`minReplicas: 1` in the CR) = the controller
holds `replicas: 1` and never sleeps it.

```text
request for cold model X
        │
        ▼
squall-proxy (≥2 replicas, stateless)
  - status.phase == Ready (informer cache)? → forward
  - else → coalesced demand patch on Model X (rate-limited per cooldown)
           + block up to holdTimeout, then answer (§7)
        │
        ▼
squall-controller (singleton reconcile per Model)
  - phase Waking/Ready already? → no-op                (level-trigger)
  - else: ONE in-place apply replicas: 1 — never force (F17, F18)
  - status: Waking; journal runId/deploymentNum
        │
        ▼
dstack provisions within backends/regions/maxPricePerHour, resolving the
CR's `gpu:` spec per backend (F33, F12): Vast → RTX3090/5090 by filter,
AWS → g5/g6e (G-family — F29 closed the P-family question), DO → L40S.
(Vast cost-first: offer_order: price + raised min_reliability — F10)
        │
        ▼
running → engine health OK → warmup done → Ready → proxy forwards
        │
        ▼                    (later)
in-flight == 0 and last request older than idleTimeout
                                             [skipped when pinned]
        │
        ▼
controller: ONE in-place apply replicas: 0 → Asleep, still addressable
(squall always sends dstack idle_duration: 0, so the machine is released
 with the job, on every backend — F21, F21b)
```

**Scale-to-zero has ONE window, and it is one explicit CR field:**

| Knob | Layer | Releases | The next wake pays |
|---|---|---|---|
| `idleTimeout` | job + instance (controller flip, and dstack via `idle_duration: 0`) | **the job and the machine** | provisioning + image + weights |
| `holdTimeout` | proxy | nothing | bounds the wait, not the capacity |

Flipping to 0 releases the instance too: squall always sends dstack an
explicit `idle_duration: 0` (D166), on every backend, so there is no
surviving warm pool for a later wake to skip provisioning against. A
CR that named two windows here through v0.18 — `scaleDownDelaySeconds` for
the job and `fleet.idleDuration` for the instance — billed identically
either way, since the instance is rented for as long as either window
holds it: the first just kept the weights in VRAM and answered instantly,
the second only kept an empty machine and needed the engine restarted and
the weights reloaded. That first window **dominates** the second at equal
price — there is no traffic pattern where moving time from it into the
second helps — so the whole budget now lives in `idleTimeout`, the window
squall controls precisely and gates on in-flight evidence. See
`docs/plans/2026-09-05-single-idle-window-design.md` for the full argument
and the measurement that closed it (D165: a run flipped to zero replicas
while still provisioning released its instance in 24 seconds).

**Idle signal: the proxy, aggregated correctly across replicas.** Each proxy
replica carries per-model `inFlight` and `lastRequestAt` as a side effect of
blocking on every request — but per-process counters are not a global
aggregate, and the proxy runs ≥2 replicas. The smallest safe primitive,
Kubernetes-native: every replica exposes those two values per model on an
internal endpoint; the controller enumerates the proxy Service's Endpoints
and requires a **complete, fresh answer from every replica**. Sleep fires
only when all replicas report `inFlight == 0` and the newest
`lastRequestAt` is older than `idleTimeout`. Any replica
unreachable, stale, or ambiguous → **stay awake**.

> **Wake may tolerate uncertainty; sleep must not.** Paying for a GPU a
> little longer is always preferable to terminating an active generation.
> The same evidence gates the drain step of teardown (§5.2).

No synthetic traffic, no metering — the two-lane rule (§10) holds; the
gateway-stats branch stays dropped.

**`Ready` has one definition, one writer, and two named evidences (F35).**
The template's entrypoint orders the ground truth — start engine → engine
health endpoint succeeds → warmup request succeeds → only then open the
endpoint the dstack probe targets — and the controller, sole writer of
`status`, sets `phase: Ready` from either evidence, whichever arrives first:
(a) **dstack probe state**, read from `JobSubmission.probes` on the API the
client already consumes (primary); (b) **first-party forward success**,
reported by the proxy as `lastSuccessAt` on the same activity endpoint the
controller already reads (§7's held-request retry — confirmation, and the
serving path's own oracle). "dstack job running" is never `Ready`; squall
probes nothing, ever — dstack's probes are dstack's own machinery and exist
with or without us.

Serving templates keep dstack's `utilization_policy` **off** (F14).

**Wake ownership rule:** exactly one `0↔1` owner — the controller. The proxy
signals; it never actuates.

**Crash consistency rule:** delivery of demand is at-least-once (watch
replay, requeue). A controller restart at any boundary MUST converge to the
same single run by re-reading `status` + dstack state via the CR's uid tags;
replays MUST NOT create a second instance or bill. The no-force CAS pair
(§5.2) makes the losing side of any residual race fail loudly.

---

## 7. First-request policy and the squall-proxy

The proxy is the component KubeAI's shape demanded (§2): one owned Go process
on the request path that decides, per request — forward, wait, or answer "not
yet". The wait mechanism is KubeAI's own, adopted after reading it (F32): the
proxy **blocks** the request while awaiting `status: Ready`, writes nothing
to the client, and answers a real status code when its deadline expires. The
v0.11 SSE-keepalive design is withdrawn — it restricted `hold` to streaming
clients, depended on how LiteLLM's stream parser treats comment lines, and,
decisively, put `200` and headers on the wire before the outcome was known,
silently opting `hold` models out of the `fallback` chain. Blocking has none
of those artifacts.

The policy enum is gone (v0.13): what the proxy does is one thing — **block
until Ready or `holdTimeout`, then answer** — and `holdTimeout: 0` answers
immediately. The block is not passive: **the held real request is the
serving path's readiness oracle.** While holding, the proxy periodically
retries the actual forward against the gateway — 503, 502, or
connection-refused mean "still waking", keep holding; success streams to the
client and reports `lastSuccessAt` to the controller (§6). No synthetic
traffic exists anywhere in this: **first-party traffic is never a probe**,
and a token can be served even before `phase: Ready` is written. Whether an immediate 503 becomes a fallback response or a client
retry is LiteLLM **router-level** configuration (`router_settings.fallbacks`
— a router parameter, not a per-deployment param, so F30's verbatim bag
cannot carry it): it lives in the LiteLLM deployment's Helm values, in Git,
reviewed — owned by the platform, outside squall's v0.1 scope. Encoding three
names for two deadlines was schema without behavior. Ready in time → forward, any client, streaming or not.

The block applies on **every non-Ready phase** — Asleep, Waking, Recreating —
so the second request of a burst waits like the first instead of being turned
away while the first is held (v0.11 defect). Held capacity is bounded by one
**global proxy setting** (`maxPendingPerModel`, generous default): beyond it
the proxy answers the wait contract immediately instead of blocking. Not a
CRD field in v0.1; PoC 3's recorded peak tunes the default.

**Wait contract:** `503` + `Retry-After` + a JSON body naming the state
(`asleep` | `waking` | `recreating`). `404` means exactly one
thing: no such Model CR in desired state. Internally (F23): gateway 503 =
asleep → demand; gateway 404 on a model that should exist = dead → demand
(recreate) + alarm; 403 = auth fault, never a wake.

| status.phase | Proxy action | On deadline (immediate when `holdTimeout: 0`) |
|---|---|---|
| Ready | forward | — |
| Asleep | demand patch; block | 503 `asleep` |
| Waking / Recreating | block (demand coalesced) | 503 `waking` / `recreating` |
| Dead | demand patch (recreate) + alarm if uncommanded; block | 503 `recreating` — full cold-start expectations |
| Draining | in-flight forwards until `drainTimeout`; **new requests never block** | **404** (leaving desired state) |
| — (no CR) | — | **404** |
| gateway 403 | alarm; never wake | 502 auth fault |

**The cost of blocking — the one thing that must be right (F28, F32):** no
bytes flow during the wait, so every read/idle timeout between the client and
the proxy must exceed the intended hold. At the LiteLLM hop this is solvable
by configuration alone: there is no wall-clock deadline in its path and httpx
bounds only the inter-chunk gap (F32), so the generated **external router
profile** carries `timeout`/`stream_timeout` at the magnitude of the
provisioning budget — the order of `provisioningTimeout`, not `300`/`60`.
Validation orders the deadlines (`holdTimeout ≤ provisioningTimeout`;
`holdTimeout ≤ external router timeout ≤ every upstream idle timeout`), and
PoC 2 inventories the idle timeout of **every hop** — client, LiteLLM, and
anything between — confirming each can be raised past the measured cold
start. Keepalives return only as a targeted mitigation for a specific hop
that cannot be raised, never as the default.

**One discovery CR, one profile (F30):** router params propagate verbatim to
every child of a discovery source, and v0.1 needs exactly one source with one
large timeout. Per-model `rpm`/`tpm` is not a v0.1 need; when a second
hardware class genuinely wants different ceilings, a second discovery CR
against its own proxy path is config, not design (§15).

---

## 8. Engines

- **The architectural boundary is unchanged: any OpenAI-compatible HTTP
  container; engines are reviewed templates, not platform features.** v0.1
  ships three templates: **vLLM** (preferred — the throughput-per-euro
  doctrine on rented GPUs stands as the default choice), **llama.cpp server**
  (GGUF; F13 empirical), and **Ollama** — admitted not by preference but by
  **engine capability**: the target model's hybrid architecture is served by
  Ollama today and by nothing else in the stack. Doctrine intact, exclusion
  clause dropped; the decision is recorded with that reservation.
- **Per-model engine configuration is first-class** (§5.1): ordered `args`,
  `env` map, pinned image digest. Engine version is a per-model attribute;
  there is no "the platform's vLLM version".
- **What a template is, exactly:** the four fields §5.1 carries — `engine`,
  a pinned `image` digest, an ordered `args` list, an `env` map — rendered by
  the controller into the dstack service configuration. Weights ride inside
  `args`/`env` in each engine's own vocabulary (`--model` for vLLM, `-hf` for
  llama.cpp, env/pull for Ollama); there is no `weights:` field and no
  translation table. `engine` is the one legitimately **per-engine** element:
  it selects the health path and warmup shape that feed §6's `Ready`
  machine. The claim F34 actually earned, and the one that matters: there is
  **no per-provider anything** — one OCI image serves every backend
  (image-as-instance on Vast, shim-in-VM on AWS/DO), no per-provider
  container, manifest, or Vast-console template.
- **The review criterion that breaks loudly and late if missed: images are
  userspace-only.** Drivers come from dstack's AMI on AWS/DO and from the
  marketplace host on Vast (F34) — an image that bundles its own driver
  stack works on one path and fails on the other. Upstream engine images
  (`vllm/vllm-openai`, `ollama/ollama`, `ghcr.io/ggml-org/llama.cpp`) are
  the pattern.
- Templates MUST bind the engine to localhost, expose only through the dstack
  app port, set `https: false` behind a `certificate: null` gateway (F24),
  ship the telemetry agent (§10), and issue a **warmup request** before
  reporting warm. Telemetry surface differs per engine (vLLM's Prometheus
  endpoint is the richest; Ollama's is not equivalent) — PoC 8 validates
  parity per engine or documents the gap; cost accounting needs only the
  OpenAI `usage` block, which all three return through the same path.

---

## 9. Artifacts and cold-start economics

- **Weights:** `hf://` is the canonical origin for public models — no S3
  egress in the path. This doctrine governs how each model's `args`/`env`
  are written (§5.1 has no `weights:` field; the engine's own flags carry
  the origin). Private/fine-tuned weights: object storage chosen by
  egress cost per target (S3 only for intra-AWS targets; egress-free stores
  for marketplace targets).
- **Warm weights:** dstack volumes where the backend supports them (EBS on
  AWS is the trivial case). The trigger to build a per-provider cache is a
  measured threshold — when `(wake frequency × model GB)` for some model
  hurts in latency or invoice — not a date. `model_download_seconds` is
  recorded from day 1 so the decision takes itself.
- **Cold-start curves differ in shape per backend, not just magnitude
  (F34):** on Vast the pull is part of instance creation; on AWS/DO the AMI
  boots first and the shim pulls afterwards. PoC 2 records the per-phase
  breakdown so a slow AWS wall-clock is read as the shape it is, not as a
  provider problem.
- **Image:** the image is the other half of the cold start. Slim, pinned by
  digest, mirrored per region (ECR pull-through cache on AWS; on
  marketplace hosts, measure and accept). Docker Hub rate limits are a
  production incident waiting to happen; do not depend on it. **Marketplace
  targets default to public images**: `registry_auth` travels to the Vast
  host at instance creation (F34), so a private registry there puts pull
  credentials on hardware we do not control — if ever unavoidable, they join
  §12.4's minimal/scoped/rotatable set.
- **Offer filters MUST include** disk (≥ image + weights + headroom) and
  minimum download bandwidth (`inet_down` on Vast) — both dominate the wake
  time more than GPU choice does.

---

## 10. Observability

- **Two telemetry lanes, each with the cheapest correct home.**
  **Request-level** (tokens, requests, latency/TTFT, per-model rates) is
  measured at the **squall-proxy** — already in the data path for every
  request, already reading the OpenAI `usage` block all three engines return
  — and exposed on `/metrics`, scraped normally in-cluster. No push
  pipeline, no per-engine Prometheus parity problem, no request-level
  credentials on rented hosts. **Host-level** (DCGM/GPU, host logs) can only
  come from the instance: the runtime environment ships a telemetry agent
  (e.g. Alloy) → remote_write / Loki with **write-only, tenant-scoped**
  credentials — its scope now reduced to what only the host can produce.
  A dead cell that shipped its telemetry is an incident; one that didn't is
  a forensic mystery.
- **The admin token must not reach the log stack.** The server prints it in
  plaintext on every start (F25) and it fronts provisioning-capable
  credentials. A log-pipeline exclusion for that line is mandatory before the
  server's stdout is shipped; an upstream patch joins the host-key PR
  (§12.2.4) in the backlog.
- **Thresholds live in the CR; alerts are one generic rule.** The controller
  exports declared/observed pairs as labelled gauges —
  `squall_model_age_seconds` vs `squall_model_max_lifetime_seconds`,
  `squall_model_price_per_hour` (observed, from dstack) vs
  `squall_model_max_price_per_hour` (spec) — so one `observed > declared`
  rule covers every model and the per-model threshold sits in Git next to the
  model it governs. This is the cost visibility v0.1 ships: the rate alert
  fires on the thing actually declared, and no spend **control** exists
  (§15) — the integral itself is free visibility: PromQL `sum_over_time`
  over the exported price gauge, zero Go code.
- **Monitoring MUST NOT generate demand.** The two-lane rule: nothing
  synthetic may traverse the gateway data path. Clarifier, not exception:
  **the held real request retried against the gateway (§7) is demand, not
  monitoring** — first-party traffic is never a probe. dstack's own service
  probes are dstack's internal machinery, exist with or without squall, run
  only against live replicas, and do not count against this rule. LiteLLM background health
  checks are **disabled for external models**; blackbox probes at the gateway
  are forbidden, and the squall-proxy never probes — it reads Model CR
  `status` via informers. Status assists request routing, but
  "is capacity asleep?" is answered from dstack/reconciler state rather than
  synthetic requests through the data path.
- **The scale-to-zero acceptance test is the invoice:** 48 h without model
  traffic → **model-serving GPU capacity cost = 0** (the platform baseline —
  gateway, dstack server — is accounted separately, AC5). If capacity cost
  isn't zero, something synthetic is waking it, and that is a bug.
- **Cost accounting separates infrastructure from usage.** Infrastructure
  cost = time integral of active replica price; usage = LiteLLM input/output
  tokens and requests. Derived efficiency metrics include €/model/day,
  €/1M tokens, tokens/GPU-hour, idle €, cold-start €, and fallback €. The
  alerts that ship are §10's `observed > declared` metric pairs (AC19).
- dstack events/metrics (`dstack metrics`, events API) feed the reconciler's
  view; the Model CR carries standard Kubernetes conditions, populated by
  the controller from dstack run/replica states.

---

## 11. Operating dstack

- **The server is a stateful plane**: server + Postgres, in the VPC,
  versioned, backed up, upgraded with its documented discipline (changelog
  review, backup, staging first). It holds backend credentials, the Vast API
  key, and the project SSH private key — **the server DB is tier-0**:
  encrypted, access-restricted, seriously backed up (F7).
- **dstack objects are API-actuated, never Git-reconciled.** The controller
  applies runs/fleets/gateway against the dstack API; Git holds intent
  (`Model` CRs), never dstack state — there is deliberately no Git↔dstack
  drift problem.
- **Blast-radius partitioning option:** one dstack project per environment
  yields separate project SSH keys for free (F7; no built-in rotation).
- **Squall components are two processes, deliberately.** `squall-controller`
  (Go, controller-runtime — the house pattern) owns reconciliation; the
  `squall-proxy` (Go) owns the data path. Separate binaries, separate
  Deployments, separate failure domains and deploy cadences. The proxy is
  stateless and runs **≥2 replicas from day 1**: shared state is the Model CR
  `status` (watched via informers) and demand travels as coalesced patches —
  the Kubernetes API server is the coordination bus at wake-rate frequencies,
  so v0.1 has **no Redis dependency**, external or embedded (miniredis-class
  embedded implementations are test doubles: single-process, no persistence —
  they cannot back multi-replica shared state). External Redis returns only
  if signal frequency outgrows the API server (§15).
- **Ownership split, one interpretation only:** Model reconciliation owns
  runs and fleets; the shared gateway is **platform-scoped Squall
  infrastructure with an independent lifecycle** — created and destroyed by
  the documented operator procedure below, never by a Model reconcile.
- **Gateway lifecycle is explicit.** The gateway is a dstack API object owned
  by neither Git nor Terraform. Its configuration is version-controlled, its
  creation and deletion are a documented sequence, and decommissioning
  external capacity means: delete runs → delete fleets → delete the gateway →
  only then remove the control plane. Destroying the server first orphans the
  gateway EC2 and takes the record of it with the database. Billing
  triangulation (§5.2) MUST cover the gateway instance, not only
  reconciler-created replicas.
- **Vendor risk, accepted in writing:** dstack is a startup with a managed
  product. The exit seam is concrete (F34): the portable artifact is a plain
  OCI image plus `args`/`env` — runnable by anything that runs containers on
  a GPU — behind OpenAI-compatible endpoints. Exit cost is moderate and
  mostly re-plumbing, not re-authoring. Reviewed yearly.

---

## 12. Security and trust model

### 12.1 Topology (all verified, §4)

- dstack **server** and **gateway** run in our VPC. Gateway:
  `public_ip: false`, certificate `acm` (TLS at ALB, private subnets + NAT)
  or `null` (HTTP inside the VPC). Wildcard DNS in a **private** Route53
  zone. Services behind a `certificate: null` gateway set `https: false`
  explicitly; `https: auto` applies only where the gateway terminates TLS via
  ACM (F24).
- **Front path invariant:** client → LiteLLM → squall-proxy → dstack
  gateway is private.
  There is no public HTTP inference listener.
- **Availability property, stated:** v0.1 runs a **single gateway replica** —
  an accepted, documented SPOF for all external inference (it is the data
  path for every byte, F22). Its mitigation is a config change, not a
  redesign: up to 3 replicas work today under `certificate: null` (F3),
  balanced by multi-value records in the private zone we already control —
  no ACM, no ALB. Enabling them is a drill (PoC 6), not future work.
- **The gateway's `instance_type` is sized deliberately, never defaulted
  (F26).** It terminates and re-proxies every inference stream for every
  model on every backend; the `t3.micro` default is burstable and throttles
  to baseline under sustained streaming — a platform-wide latency incident,
  not a per-model one.
- **The squall-proxy is in the same data path and inherits the same
  discipline (F26's logic):** every external token crosses it. Unlike the
  gateway, we own it — so it has no single-replica excuse: stateless, ≥2
  replicas, resource-sized for sustained streaming plus SSE keepalives, and
  part of the §12.1 availability statement.
- **Provider transport is classified, not hand-waved:**
  - `routed-private`: AWS replicas with `public_ips: false`; the platform
    network must already provide reachability from server/gateway to the
    remote private subnets (e.g. cross-region peering/TGW/Cloud WAN or an
    equivalent routed design). dstack provisions compute, not that fabric.
  - `ssh-tunnel-public`: Vast and any other backend reached through a public
    SSH endpoint. Inference/control bytes may cross the public Internet, but
    only inside the outbound dstack SSH tunnel; the replica exposes no public
    HTTP endpoint.
- For marketplace/public-SSH backends the intended public surface is exactly
  one SSH port per replica. AWS private replicas expose none.
- **Outbound TCP/22 from the gateway is the marketplace data plane** (F22):
  every inference byte from a Vast/DO replica traverses an SSH tunnel the
  gateway opens outbound. If platform egress (currently a shared Transit
  Gateway we do not control) filters port 22, there is **no data path at
  all** — not a degraded one. Verifying that egress path is a PoC 0
  prerequisite and a standing platform dependency. **Status: VERIFIED
  (2026-08-26)** — platform egress permits the outbound SSH data path; the
  dependency stays documented so any future egress-architecture change
  re-checks it.

### 12.2 Rules

1. **No public HTTP endpoint exists.** With that invariant holding, the
   gateway token MAY be omitted; it SHOULD be kept anyway (it costs one
   config line in LiteLLM). **Gateway rate limits MUST be configured** — in
   a no-auth-private posture they are the only brake on internal abuse and
   accidental wake loops.
2. **Exactly one public door, and prove it:** external nmap against a live
   marketplace replica MUST show exactly one open port (the mapped SSH). The
   runtime MAY listen on an internal container/service interface as required
   by dstack forwarding, but templates request zero extra public port mappings.
   This check is **automated on every template change** — the security boundary
   is provider exposure, not the runtime process bind address.
3. **Keys (F7, F22):** the project private key never touches a provider host
   (only public keys are installed) — but it is **resident on the gateway**,
   which opens every data-plane tunnel with it (F22), not only in the server
   DB. The gateway VM is therefore tier-0 alongside the DB: hardened,
   access-restricted, and in scope for the same backup/rotation posture
   (§11). "Random port" is noise
   hygiene, not security, and MUST NOT be described as a control.
4. **Host keys (F8): accepted residual risk, scoped.** dstack performs no
   host identity verification on any connection. On AWS private paths the
   MITM surface is ≈0; on marketplace paths an on-path attacker can
   impersonate a replica and observe tunnel contents (job spec env vars,
   inference traffic). Compensating controls: workload eligibility (12.3),
   minimal scoped secrets (12.4). An upstream PR (`accept-new` + host-key
   pinning via provider API where available) is on the backlog — it removes
   the risk and positions us as contributors.
5. **The gateway's private listener is source-restricted to the
   squall-proxy** (SG source = the proxy's SG, never the whole VPC CIDR) —
   the proxy is the component that actually opens the connection; LiteLLM
   talks only to the proxy. With
   native wake now closed at the validation layer (§6, F15/F16), this returns
   to defense-in-depth — kept because it costs one SG rule and pairs with
   rule 6.
6. **The dstack server API is the second door.** Gateway-less services are
   proxied by the server itself at `/proxy/services/...` (F19). External
   services are always gateway-fronted, and server API reachability is
   SG-restricted to the reconciler and admins. PoC 5 extends accordingly:
   prove the server proxy serves nothing usable for our models to ordinary
   VPC workloads.

### 12.3 Workload eligibility (mandatory classification)

- **Marketplace backends (Vast):** internal, non-regulated workloads only.
  Prompts and completions execute in RAM on hardware we do not control;
  `location: EU` does not make an anonymous host a GDPR processor.
- **AWS backend:** eligible for anything our normal AWS posture allows;
  the private-subnet path (F5/F11) is the required configuration for anything
  client-adjacent.
- **DigitalOcean backend:** in v0.1 by decision. Its workload eligibility
  MUST be explicitly classified after PoC transport/security verification —
  it MUST NOT inherit AWS trust merely for being a conventional cloud — and
  F12 governs `gpu.name` lists and data-residency reasoning (no A100; EU
  GPU = Amsterdam/H100 only).
- **Regulated client traffic:** AWS backend only, with its own review — or
  not at all. This table is written down and enforced via the per-model
  `backends:` allowlist (§5.1), not via convention.

### 12.4 Secrets on replicas

Everything that lands on a provider host is minimal, scoped, and rotatable:
HF fine-grained read-only token, write-only tenant metrics credentials —
and, only if a marketplace target ever uses a private registry (§9 says
don't), pull-only `registry_auth` (F34). Nothing else. The question is never whether one leaks; it is
how little it hurts when it does.

### 12.5 Provider API identities

- The dstack server is the only component allowed to hold credentials capable
  of provisioning, mutating, or deleting provider compute.
- If provider-side billing/orphan triangulation cannot be obtained through
  dstack, the reconciler/FinOps observer MAY receive a separate read-only
  identity limited to inventory and billing APIs. It MUST NOT be able to
  create, stop, resize, or delete compute.
- Read-only observer identity and dstack provisioning identity MUST be
  independently revocable and auditable.

---

## 13. PoC plan — empirical unknowns, in order

Each PoC kills exactly one unknown. "Easy" items live here with owners, not
in a someday list.

**PoC 0 — Cross-backend fronting + flip mechanics (first; gates everything).**
AWS-hosted gateway fronting a live Vast replica end-to-end. Documented in the
gateways doc; smoke-tested anyway because it is the load-bearing premise — if
this fails, stop and escalate: it is the one outcome that forces a structural
rewrite. In the same PoC, validate live what source reading established:
fixed `replicas: 0` registers and routes (F17); the 0↔1 in-place flip
preserves `RunModel.id`; and the two unchased update-path checks
(`_check_dynamo_in_place_update_compatibility`,
`_check_can_update_configuration`) do not constrain `replicas` flips.
Sequencing per quota reality: Vast/DO track first; the AWS routed-private leg
runs **last**, pending the P-family quota decision (§13 note); fallback-policy
validation runs now on eu-west-1 g5/A10G within existing quota.
Status 2026-08-25: flip mechanics, `replicas: 0` registration, in-place
identity across flips, gateway response codes, and llama.cpp inference
through the full private path all validated live on CPU (c7i.large, <$0.30
total). Update 2026-08-26: the TGW egress check has **passed**, the Vast
account is live, and the first GPU instance has been launched through
dstack. Remaining formality before PoC 0 closes: one end-to-end token
through gateway → tunnel → GPU replica, if the launch test did not already
cover it.

**PoC 1 — Front-path privacy + provider transports.** Gateway with
`public_ip: false` + ACM/ALB in private subnets; private Route53 wildcard;
LiteLLM→gateway entirely in-VPC. In parallel prove one `routed-private` AWS
path and one `ssh-tunnel-public` provider path. Pass: no public HTTP listener
exists; AWS traffic stays on the routed private fabric; marketplace traffic
reaches only the SSH port and inference is carried inside the tunnel. PoC
models are specced so every listed backend can satisfy their `gpu:` (F12 —
e.g. L40S or H100 in `gpu.name` for the DigitalOcean leg; a card list
without a DO-sold card can never
match there and the allowlist will hide that silently).

**PoC 2 — Flip wall-clock per backend.** Question (a) — behavior at zero —
is answered: immediate 503, never held (F23), and the proxy's decision table
(§7) is fixed accordingly. What remains is (b): the wall-clock from the
`replicas: 1` apply to ready + warm, per backend and per model class — both
from a cold fleet (full floor + weights) and from inside the warm window
(F21) — the numbers that decide where `hold` is tolerable UX. PoC 2 also inventories
the idle/read timeout of **every hop** between client and proxy and confirms
each can be raised above the measured cold start (§7, F32) — keepalives exist
only as a fallback for a hop that cannot be. Additionally verify that no path
except the squall-proxy reaches the gateway listener (§12.2.5).

**PoC 3 — Wake coalescing + crash consistency.** Drive a burst of 50
concurrent requests at a cold model. Pass: exactly **one effective wake
action** — one provisioning operation, one instance, one bill. Then
kill/restart the controller at each real boundary of the current design:
(a) after the demand patch lands, before actuation; (b) after a successful
dstack `replicas: 1` apply, before the status update; (c) after the status
update, before the reconcile completes; (d) two deliberately concurrent
reconciles — the CAS loser (K8s `resourceVersion` or dstack no-force, F18)
fails loudly, `force` never sent. In every case requeue/watch-replay
converges on the already-created run via the CR-uid tags: replays are
no-ops, never a second instance or bill. Record the peak held-connection
count during the burst against `maxPendingPerModel` (§7).

**PoC 4 — Scale-to-zero for real, at the instance layer.** Health checks
suppressed per §10; 48 h without model traffic. Pass criteria are measured on
**instances and invoices, not replica counts** (F21), and scoped to
**model-serving GPU capacity** — the platform baseline (gateway EC2, dstack
server) is accounted separately and is expected to persist: fleet instances
terminate promptly after the flip to 0 — squall always sends dstack an
explicit `idle_duration: 0` (D166), so there is no idle window left to wait
out; measured at 24 s on Vast.ai (D165) — verified per backend,
including Vast's container-based instances — and the external capacity
invoice for the window is zero.

**PoC 5 — Single-door proof.** Automated external nmap on a live Vast
replica. Pass: one open port. Wire into template CI.

**PoC 6 — Failure drills.** Kill the host mid-generation on vast, aws,
and digitalocean; observe dstack's replica replacement per backend. Stop the
dstack server; verify whether the gateway keeps routing existing replicas.
Document spot/interruptible behavior differences per backend. Include a
never-ready run (e.g. an unpullable image digest): pass = destroyed at
`provisioningTimeout`, alarm raised, no drain attempted. Gateway availability
drill: enable 2 replicas under `certificate: null` (F3), verify multi-value
DNS routing, kill one replica mid-stream and document behavior; verify the
single-replica recovery runbook while replication remains doc-experimental.

**PoC 7 — Warm starts.** Volumes per backend (EBS on AWS first); measure
wake time delta. Decide per-provider cache thresholds with data (§9).

**PoC 8 — Runtime protocol parity.** Run one vLLM model and one llama.cpp
server model through the same LiteLLM → gateway → dstack path. Pass: both use
the same OpenAI-compatible front contract; only the runtime template differs;
readiness, warmup, telemetry, and zero public HTTP exposure are validated for
both. Scope: **one backend only** — a correct boundary, not a compromise:
the image is provider-independent by construction (F34), so a runtimes ×
providers matrix would test dstack's backend code, not ours. Status: the llama.cpp half is
**done** (PoC 0-CPU; 200 OK end-to-end through the private gateway and SSH
tunnel, OpenAI-shaped response with a `usage` block — F13 is now empirical).
Remaining: the vLLM leg on GPU.

**PoC 9 — Desired-state deletion safety.** Simulate API-server
unavailability and partial watch data while live capacity exists: pass = zero
destructive actions (§5.2 fail-closed). Delete the CR both ways —
`kubectl delete` and Flux prune — during an in-flight generation and verify
the finalizer's drain-first teardown, never an instant kill (and note the
platform's real emergency stop: `flux suspend kustomization` + delete —
kubectl alone is resurrected by Flux). Rename the LiteLLM model and dstack
service; the CR uid keeps ownership stable.

**PoC 10 — Discovery seam (a config change, not an operator feature).**
Create a `LiteLLMModelDiscovery` with `type: kubeai` and
`baseUrl: http://squall-proxy.squall.svc/v1` (F30): the
existing provider path calls `/models`, tolerates an empty API key, overlays
`baseUrl → params.api_base` on every child, and names children
`hosted_vllm/<id>` — the same shape as the ten in-cluster models. Pass: a
Model CR merged to Git appears in LiteLLM with the **external router
profile** (timeouts at provisioning-budget magnitude, ≥ `holdTimeout`);
revert removes the model with drain; **kill the proxy** and verify F31 —
discovery goes fail-closed and deregisters nothing; squall never calls the
LiteLLM API.

---

## 14. v0.1 acceptance criteria

1. A `Model` CR merged to Git serves inference end-to-end from Vast, AWS,
   and DigitalOcean with the same control/config shape, using models whose `gpu:` spec each listed backend can actually
   satisfy (F12); backend-specific differences remain behind dstack
   configuration.
2. Client → LiteLLM → gateway is private and no public HTTP inference endpoint
   exists. AWS private transport and marketplace/public-SSH tunnel transport
   are both demonstrated and documented; PoC 5 is green and automated.
3. Wake-on-request works across `holdTimeout` values: `0` answers the wait
   contract immediately and correctly (503 + `Retry-After` + state body) —
   the CR's actual contract; a large value holds and forwards. Router-level
   fallback chains are `router_settings` in the LiteLLM Helm values
   (Git-reviewed), outside squall's v0.1 scope (§7).
4. 50-request cold burst → exactly 1 effective reconciler-owned wake action
   and exactly one resulting run/bill (PoC 3).
5. 48 h without model traffic → **model-serving GPU capacity cost = 0**,
   verified at the instance layer: fleet instances terminate promptly after
   the flip to 0, because squall always sends `idle_duration: 0` (F21, D166;
   measured 24 s on Vast.ai, D165); replica count alone proves nothing.
   The platform baseline (gateway, dstack server) is accounted separately —
   the AC is about capacity, not about the control plane being free.
6. Deletion — `kubectl` or Flux prune — tears down through the finalizer
   deterministically **and drain-first** (no mid-stream cut within
   `drainTimeout`); API-server unavailability or partial reads cause zero
   destructive actions. No generic instance-age TTL substitutes for
   trustworthy desired state.
7. Orphan drill (manual at v0.1): a human sweep of provider invoice vs
   `dstack ps` catches an instance created outside the reconciler's
   knowledge — including one whose **owner record was destroyed** (the
   gateway after a server-first teardown). Automation is §15.
8. Infrastructure cost, token/request usage, and derived €/1M-tokens /
   tokens-per-GPU-hour metrics are visible in Grafana; the AC19
   `observed > declared` alerts fire in a test.
9. Kill-host drill passes on all three backends with documented behavior.
10. No write-capable/provisioning provider credential exists outside the
    dstack server DB; any direct provider inventory/billing identity used by
    the reconciler is read-only and separately scoped. DB is encrypted and
    backed up; restore tested.
11. Workload-eligibility table exists, is written down, and the per-model
    `backends:` allowlist enforces it.
12. Everything the reconciler creates is identifiable by the
    immutable CR uid plus naming/tags; renames do not create ownership
    ambiguity.
13. Reconciler crash/replay drills before ACK and immediately after dstack
    actuation still produce exactly one run and one bill — backed server-side
    by the no-force CAS rule (F18).
14. vLLM and llama.cpp server both pass through the same OpenAI-compatible
    control/data path with runtime-specific behavior isolated to templates —
    on one backend; explicitly not a runtimes × providers matrix.
15. A run that never reaches ready is destroyed at `provisioningTimeout` with
    an alarm; a healthy run older than `maxLifetime` raises an age alert and
    is NOT destroyed.
16. Proxy and controller act on the gateway's real signal set (F23): 503 →
    wake; 404 → recreate from desired state, alarmed when uncommanded; 403 →
    auth fault, never a wake. Asleep and dead are distinguished everywhere
    they differ (F20).
17. `minReplicas: 1` (pinned) never sleeps: it survives idle windows, raises
    the `maxLifetime` age alert instead of any destructive action, and
    toggling pinned ↔ on-demand in the CR takes effect without a recreate.
18. Discovery end-to-end (PoC 10): CR merged → model in LiteLLM with the
    external router profile → revert → drained removal; squall never calls
    the LiteLLM API; proxy outage deregisters nothing (F31).
19. Metric pairs (§10): declared and observed gauges exist for
    `maxLifetime` and `maxPricePerHour`, and the generic `observed >
    declared` rule fires in a test for both.
20. `hardStop` reaches dstack as `max_duration` on every wake of an on-demand
    Model and on no apply for a pinned one; a run that hits it goes terminal
    without a replacement replica and is `Dead` with an alarm, not `Asleep`.

---

## 15. Future direction (explicitly not v0.1)

- **Billing-granularity-aware idle** — per-backend property the controller
  reads (Vast: per-second, nothing to wait out; AWS: 60 s minimum; DO:
  hourly — the one place "wait out the paid interval" pays). Deferred
  because it is a no-op on the primary backend.
- **Shared warm pools / fleets across models** of the same GPU class.
- **Load-based `1..N`** as a designed change to §6.
- **Customer on-prem GPUs** via dstack SSH fleets — same control plane.
- **Fine-tuning / batch** via dstack tasks on the same backends.
- **Upstream PRs:** host-key verification (§12.2.4) and the admin-token
  stdout line (F25).
- **Gateway replica reliance** — replication works today under
  `certificate: null` (F3) and is drilled in PoC 6; what defers is *relying*
  on it while docs label it experimental.
- **Automated billing triangulation** — the read-only provider identity and
  the invoice-integral reconcile, when scale outgrows the manual AC7 drill.
- **Spend caps / off-switch** — a budget control and a suspend primitive, if
  ever needed; note the Flux-ownership constraint (a controller must not
  write `spec`), so any future off-switch is an annotation or a Flux-level
  action, not a spec field.
- **Rate classes** — per-class discovery CRs against class-partitioned proxy
  paths (F30), when a second hardware class needs different `rpm`/`tpm`.
- **External Redis** only if coordination outgrows the API server (§11).
- **CRD graduation** (`v1alpha1` → `v1beta1`) once PoCs settle the field set.
- Revisit **v0.2 (Liqo)** only if arbitrary K8s workloads need external GPUs.

---

## 16. Final architectural statement

> **For serving models, external GPU capacity is a Model CR in Git, an
> OpenAI-compatible endpoint behind the routing plane the platform already
> governs, and a machine that exists only while it is needed — or exactly as
> long as it is pinned.**

Git owns intent. Flux delivers it.
squall-controller owns: the 0↔1 flip (wake, sleep, pinned), drain-first
deletion via finalizer, and orphan reconciliation. Nothing else.
squall-proxy owns: the per-request decision and the honest wait. Nothing else.
alitellm-operator owns: LiteLLM, as it always did.
dstack owns: providers, offers, run execution, instance lifecycle via fleets
(an explicit `idle_duration: 0` always), tunnels, ingress.
The engine owns tokens.

Squall's custom footprint is therefore:

> **watch the CRs, coalesce demand through the API server, flip one integer
> — never with force — answer the wait truthfully, reconcile orphans, tear
> down only through the finalizer — draining first — and prove, continuously,
> that exactly one HTTP ingress path exists and idle costs zero.**

That is the v0.1 boundary.

---

## Appendix A — Changelog v0.17 → v0.18 (RC — D28 resolution; shadow proxy rejected)

Trigger: implementation RFC `rfc-shadow-proxy.md` (D28: `Ready` had no
writer; per-model shadow-proxy Deployment proposed). Adjudication: **shadow
rejected, both variants** — each claimed benefit has a cheaper owner that
already exists, and the real work (engine templates) remains under every
option. Recorded here so it does not return under another name.

1. **F35 added, source-verified:** dstack has first-class service probes and
   exposes per-probe state (`JobSubmission.probes`) plus `submitted_at` on
   the API the client already consumes. Readiness needs a client unfreeze
   and a field read — not a component.
2. **§6 `Ready` completed with two named evidences:** dstack probe state
   (primary) and first-party forward success via the proxy's activity
   endpoint (confirmation). Squall probes nothing, ever.
3. **§7 hold made active:** the held real request retries the actual
   forward — 503/502/conn-refused = still waking; success streams and
   reports `lastSuccessAt`. The serving path's oracle is the user's own
   request; a token can be served before `Ready` is even written.
4. **§10 telemetry lanes split:** request-level at the squall-proxy
   (`/metrics`, scraped — the RFC's benefit 4, assigned to the component
   already in the path); host-level agent scope reduced to DCGM/logs.
   Two-lane rule gains a clarifier (first-party ≠ probe; dstack's own
   probes don't count), not an exception.
5. **§5.2/`status.wakeStartedAt`:** the controller journals the age anchor
   at the moment of actuation — ledger D7 dissolves with a timestamp, not
   `progressDeadlineSeconds`.
6. **Fix order fixed by interlock:** D28 (templates + client unfreeze +
   forward-retry) → D39 (wire validation; `holdTimeout ≤
   provisioningTimeout` must be live first) → 8.3 (`provisioningTimeout`
   from `wakeStartedAt`). Implementing 8.3 before D28 would destroy every
   model at the deadline in a recreate loop — the RFC's interlock analysis
   was correct and is preserved.
7. **Rejected-alternatives ledger:** per-model shadow Deployment — pods
   representing remote machines mislead scheduler/quota/on-call; kubectl
   visibility belongs to `additionalPrinterColumns`; a provisioning timer
   belongs to a journaled timestamp; readiness belongs to F35.

---

## Appendix B — Changelog v0.16 → v0.17 (RC — coherence sweep)

1. **§5.1/§8 reconciled** — `weights:` cut (weights ride in `args`/`env` in
   each engine's vocabulary; §9's origin doctrine governs how they're
   written); `engine` acknowledged as the one legitimate **per-engine**
   element (it selects the health/warmup shape feeding `Ready`). F34's claim
   sharpened to what it earned: no per-**provider** anything.
2. **§7 table completed** — `Draining` (in-flight forwards to `drainTimeout`,
   new requests 404) and `Dead` (block + recreate demand, alarm if
   uncommanded) rows added; six phases, six rows, no coin-flips.
3. **AC3 made satisfiable** — router fallback chains are `router_settings`
   (router-level; F30's per-deployment bag cannot carry them), owned by the
   LiteLLM Helm values in Git, outside squall's v0.1 scope; AC3 now asserts
   the 503 wait contract squall actually owns.
4. **§10 de-contradicted** — "no spend **control**" (the integral is free
   PromQL visibility); per-provider/pool budget-alarm sentence removed (cut
   in v0.13); AC8 points at the AC19 alerts that ship.
5. **§10's invoice test** given the AC5 scoping (model-serving GPU capacity;
   platform baseline separate) that v0.16 applied everywhere else.
6. **§6 diagram updated to F33** (gpu spec, not VRAM GiB).
7. **§5.1 warm-window warning rewritten** to be readable by its implementer.

---

## Appendix C — Changelog v0.15 → v0.16 (RC — correctness sweep, third-party review)

Scalpel applied: zero new components, one bounded config value, the rest is
invariants and stale text. All review points accepted except one:
SSE-keepalive mentions are deliberate targeted-mitigation text, not residue.

1. **Sleep made safe under multi-replica reality** (§6): per-replica
   `inFlight`/`lastRequestAt` on an internal endpoint; the controller
   aggregates across the proxy Service's Endpoints and requires complete,
   fresh answers from every replica. Invariant adopted: **wake may tolerate
   uncertainty; sleep must not** — it also gates teardown's drain.
2. **Directional fail rule** (§5.2): `0→1` fails open, `1→0` fails safe —
   superseding the earlier "replica-count changes are non-destructive"
   reading; a wrong sleep kills a generation, a wrong wake costs money.
3. **`Ready` defined once** (§6): running → engine health → warmup → dstack
   readiness gate → controller (sole status writer) sets Ready. "Job
   running" is never Ready; no probes, no health subsystem.
4. **Gateway ingress corrected to the real caller** (§12.2.5, PoC 2): SG
   source = squall-proxy, not LiteLLM — stale v0.8-era text.
5. **PoC 3 rewritten around the actual failure boundaries** (patch→actuate,
   actuate→status, status→completion, concurrent CAS loser); Redis
   Streams-era ACK/redelivery wording removed.
6. **AC5/PoC 4 scoped honestly**: 48 h without model traffic →
   model-serving GPU capacity cost = 0; the platform baseline (gateway,
   server) is accounted separately — the old wording was literally
   unachievable while the system behaved correctly.
7. **`maxPendingPerModel`** (§7): one global proxy bound on held capacity,
   answering the wait contract beyond it; not a CRD field; tuned by PoC 3's
   measured peak.
8. **Gateway ownership disambiguated** (§11): Model reconciliation owns
   runs/fleets; the gateway is platform-scoped Squall infrastructure with an
   independent, documented lifecycle.
9. **Terminology normalized** to CRD camelCase in live sections; §10's
   conditions sentence fixed (the CR carries standard conditions).
10. **Status: RC, not FINAL** — the honest label while F21's Vast caveat
    (instance release on non-VM backends) awaits PoC 4; if Vast needs an
    explicit release action, that is a §6 amendment, not an architecture
    change.

---

## Appendix D — Changelog v0.14 → v0.15 (FINAL)

1. **Status: FINAL — approved for implementation.** No architectural change
   from v0.14; this revision closes decisions and dependencies.
2. **DigitalOcean reinstated in v0.1** by explicit decision, reversing the
   v0.13 deferral: Goal 7, §12.3, AC1, PoC 1/PoC 6 restored; the §15 entry
   removed. DO-targeting models carry a DO-sold card in `gpu.name` (F12).
3. **Standing dependencies closed:** the TGW outbound-SSH egress check
   **passed** (§12.1 marked VERIFIED); the Vast account is live; the first
   GPU instance has been launched through dstack. PoC 0 closes on one
   end-to-end GPU token through the private path if the launch test did not
   already cover it.
4. Implementation starts against this document. Suggested order now that
   nothing blocks: close PoC 0 → PoC 2 GPU wall-clocks (Vast
   RTX3090/RTX5090 class) in parallel with PoC 10 (discovery, config-only)
   → PoC 4 idle-invoice run → drills (PoC 5/6/9) → AC sweep.

---

## Appendix E — Changelog v0.13 → v0.14 (launch model closed)

1. **F34 added, source-verified:** one OCI image everywhere; Vast lands it as
   the instance (`dockerized=False`, `onstart`, `registry_auth`), AWS/DO land
   it via AMI + `dstack-shim` (`dockerized=True`). Vast-console templates are
   explicitly not used. A "template" is exactly §5.1's `image` + `args` +
   `env` — no per-provider artifact exists (§8).
2. **Driver rule promoted to the §8 review criterion:** images are
   userspace-only; drivers come from the AMI (AWS/DO) or the host (Vast).
   The one mistake that breaks loudly and late.
3. **Registry rule (§9, §12.4):** marketplace targets default to public
   images — `registry_auth` lands on the Vast host; private-registry creds
   there join the scoped-secrets set only if ever unavoidable.
4. **§9 records why cold-start curves differ in shape** per backend
   (pull-at-create vs boot-then-pull) so PoC 2 numbers are read correctly.
5. **PoC 8's single-backend scope reclassified** from pragmatic to correct
   (the matrix would test dstack, not squall); **§11's exit seam made
   concrete** (portable artifact = OCI image + args + env).

---

## Appendix F — Changelog v0.12 → v0.13 (YAGNI pass)

Applied test: is it additive later? Everything cut passes; the architecture
(CRD, discovery seam, blocking hold, two-layer scale-to-zero, finalizer) is
untouched.

1. **Cut `budget`/`onExceed` and the `Suspended` phase** (one round old):
   `maxPricePerHour` × bounded durations already bounds spend; the spend
   integral, resume rules, and the controller-writes-spec Flux conflict were
   a subsystem v0.1 never asked for. Visibility ships instead as **metric
   pairs** (§10): declared vs observed gauges, one generic alert rule.
2. **Cut `routerClass`** (one round old): one discovery CR, one profile, one
   large timeout; a second class is a config change later (§15). F30 stays.
3. **Cut `firstRequestPolicy`**: two deadlines wearing three names —
   `holdTimeout` alone (0 = immediate) expresses the behavior; fallback is
   LiteLLM router config the CR never influenced.
4. **Cut `suspend`** — with a stronger reason than "revert works": on a
   Flux-managed CR, `kubectl edit spec.suspend` and `kubectl delete` are
   both reverted by Flux; the platform's real emergency stop is
   `flux suspend kustomization` + delete, which already exists. A spec-field
   off-switch was fighting the GitOps model it lives in.
5. **GPU spec corrected to a dstack `GPUSpec` passthrough** (F33): named
   cards express the bandwidth requirement GiB cannot; `memory:` is a native
   range; `preferredGpuMemoryGiB` was unimplementable under `offer_order`
   (F10); per-backend `gpuAllowlist` keying had no mechanism behind it.
6. **DigitalOcean deferred to §15** (pending JC confirmation — it reverses a
   stated v0.1 uniformity goal): the abstraction is proven by F4/F30 and
   enabling DO is a config block; Vast (measured) + AWS (compliance) cover
   v0.1's real needs. AC1/PoC 6 trimmed accordingly.
7. **Billing triangulation reduced to a manual AC7 drill**; automation and
   its read-only identity move to §15. §11's server-before-gateway warning
   stays — that failure is real and documenting it is free.
8. Kept deliberately: `minReplicas` (pinned, AC17), `maxLifetime`
   (alert-only, now metric-exported), the every-hop timeout inventory, and
   the v0.12 blocking-hold mechanism in full.

---

## Appendix G — Changelog v0.11 → v0.12 (defect sweep + blocking hold)

1. **`hold` rebuilt on KubeAI's blocking mechanism** (§7, F32): the proxy
   blocks writing nothing, answers a real status on deadline. The v0.11
   SSE-keepalive design is withdrawn — its decisive defect was placing `200`
   on the wire before the outcome, silently opting `hold` out of `fallback`.
   One code path for all three policies; the block covers every non-Ready
   phase (fixes the burst defect); streaming-only restriction gone.
   Keepalives demoted to targeted mitigation. External router profile
   timeouts move to provisioning-budget magnitude; deadline ordering is a
   validation rule; PoC 2 inventories every hop.
2. **Job-layer idle knob added** (`scaleDownDelaySeconds`, §5.1/§6): three
   explicit timings — job idle (engine reload), fleet idle (machine),
   holdTimeout (the wait) — replacing an undefined "idle window".
3. **Idle signal closed in favour of the proxy** (§6): blocking makes
   in-flight count and last-request time inherent; gateway-stats branch
   dropped.
4. **Rate classes** (§5.1 `routerClass`, §7, F30): router params are
   per-discovery-source and verbatim; per-model ceilings ride on
   class-partitioned discovery paths, one discovery CR per class.
5. **`suspend: true` and `budget`** (§5.1/§5.2, PoC 9, AC6/AC19): an off
   switch that keeps the reviewed declaration, and a spend cap that acts
   (fail-closed against unreadable billing) — an alarm is not a control.
6. **Stale v0.10 text swept**: §11's registry sentence, AC1's
   `external_capacity`, AC6/PoC 9's registry machinery — all rewritten
   against the CRD/finalizer model; orphaned-section separators and AC12
   wording fixed.
7. **F30–F32 added** (discovery verbatim-params + type:kubeai reuse;
   discovery fail-closed D-09; KubeAI blocking + litellm timeout model).
   PoC 10 reframed as a config change.

---

## Appendix H — Changelog v0.10 → v0.11 (platform alignment — structural)

1. **Desired state moved to a CRD** (`squall.ackstorm.ai/v1alpha1 Model`,
   §5): the registry-as-source-of-truth model of v0.8–v0.10 inverted the
   platform's production pattern (F27). Intent now lives in Git next to the
   ten in-cluster models; LiteLLM is generated by alitellm-operator
   discovery of the proxy's `/v1/models` — squall never calls the LiteLLM
   API. Deletion safety reframed around finalizers (drain-first on delete
   and Flux prune); the multi-cycle absence machinery dissolves.
2. **The squall-proxy replaces LiteLLM hooks** (§2 withdrawal, §7): hooks
   cannot emit SSE keepalives, and correctness no longer depends on hook
   semantics. LiteLLM runs vanilla. Outward wait contract: 503 +
   `Retry-After` + state body; 404 reserved for "not in desired state"
   (503 teaches the router to retry/fall back; 404 teaches it the model is
   broken). Proxy is Go, stateless, ≥2 replicas, in the F26 sizing
   discipline.
3. **Coordination moved to the API server** (§5.2, §11): demand = coalesced
   annotation patches; shared state = CR status via informers;
   `resourceVersion` CAS on intent + dstack no-force CAS on actuation.
   **No Redis in v0.1** (miniredis-class embeds are test doubles).
4. **Ollama admitted as third engine by capability, not preference** (§8):
   the target model's hybrid architecture is Ollama-only today; the
   vLLM-first doctrine stands as default. CRD carries `args`/`env`/image —
   the three shapes production models actually use.
5. **Pinned mode** (`minReplicas: 1`, §5/§6): a fixed GPU that never sleeps,
   alive while its CR exists; `maxLifetime` alert-only is its
   forgotten-pin alarm; AC17.
6. **`hold` quantified and constrained** (F28, §7): streaming-only with SSE
   keepalives and `holdTimeout`; rejected by validation with short
   `idleDuration`; discovery generates a distinct external router profile
   (PoC 10, AC18).
7. **Sizing re-derived** (F29): target class is 24–48 GB (RTX3090/5090 on
   Vast, G-family on AWS, L40S on DO) — **the P-family quota question is
   closed as unnecessary**; the pending G→16 (eu-central-1) covers the AWS
   leg. Billing-granularity idle deferred to §15 as a per-backend property.
8. Facts F27–F29 added; PoC 10 and ACs 17–18 added; PoC 9 gains the
   Flux-prune/finalizer drill.

---

## Appendix I — Changelog v0.9 → v0.10 (defect sweep)

1. **F3 corrected** (source-verified): the ACM clause was wrong — only
   `lets-encrypt` blocks gateway replication, and `certificate: null`
   (our posture) supports replicas today. Gateway SPOF promoted to a stated,
   accepted availability property of v0.1 with a config-change mitigation
   (§12.1) and a PoC 6 drill; the docs' "experimental" label is retained as
   the reason v0.1 still runs one replica.
2. **§12.1 / F24 contradiction fixed**: `https: auto` survived from v0.7;
   services behind `certificate: null` set `https: false`.
3. **Header region fixed** to eu-west-2 (eu-west-3 has no single-GPU H100 —
   the old example was unsatisfiable, not merely stale).
4. **§5.2 closing paragraph updated** from v0.7 text: 1..N re-scoped, and the
   idle-signal carve-out now referenced instead of appearing contradicted.
5. **Gateway sizing** (F26): `t3.micro` default rejected; `instance_type` is
   a deliberate decision (§12.1) — burstable throttling on the shared data
   path is a platform-wide incident.
6. **Gateway lifecycle owned** (§11): documented create/decommission sequence
   (runs → fleets → gateway → control plane); billing triangulation and the
   AC7 orphan drill now cover the destroyed-owner-record case.

---

## Appendix J — Changelog v0.8 → v0.9 (PoC 0-CPU revision)

1. **Identity correction** (§5.2, F20): `external_capacity.id` is ours,
   carried as tags; the run id is a journaled mutable pointer — stable across
   flips, invalidated by terminal states (terminal ⇒ gateway 404 ⇒ next apply
   mints a new run). Asleep vs dead formalized as distinct states/actions.
2. **Fleet layer modeled** (§5.1, §6, F21): scale-to-zero is two-layered; the
   flip releases the job, the fleet releases the instance. Per-model owned
   fleet with mandatory explicit `idle_duration` (default is 3 days — up to
   €288 per sleep cycle on H100-class if left alone). The warm window is the
   cost/wake-latency dial and feeds §7. AC5/PoC 4 restated at instance level.
3. **Hook decision table fixed by measurement** (§7, F23): 503 asleep → wake;
   404 dead → recreate + alarm; 403 → never wake. Gateway never holds, never
   wakes. PoC 2 shrinks to wall-clock. Cold-start control-plane floor
   measured: ~97 s.
4. **Gateway tier-0 and the egress-22 dependency** (§12, F22): the gateway
   dials out and holds the project private key; outbound TCP/22 is the
   marketplace data plane — TGW egress verification promoted to PoC 0
   prerequisite and standing platform dependency.
5. **Operational facts hardened**: `https: false` required behind
   `certificate: null` gateways (F24, §8); admin token printed to stdout →
   mandatory log-pipeline exclusion + upstream patch backlog (F25, §10);
   `max_duration` terminal behavior confirmed single-submission, which —
   given F20's 404 consequence — **reinforces** the spec's alert-only
   `max_lifetime` rule: any native hard stop is a scheduled outage plus full
   recreate, not a restart. Any implementation-side hard-stop backstop must
   be reconciled with §5.2 explicitly.
6. Facts F20–F25 added; PoC statuses recorded (flip mechanics and the
   llama.cpp half of PoC 8 complete, on CPU, for cents).

---

## Appendix K — Changelog v0.7 → v0.8 (implementer-driven revision)

1. **Wake/sleep mechanism replaced** (§6): fixed-replica 0↔1 flip; `scaling:`
   forbidden on external services — enforced by symmetric model validation
   (F15), so native gateway wake is closed at the validation layer, not by
   network isolation. The reconciler now owns the full 0↔1 lifecycle
   including idle→0, with a two-lane-compliant idle signal (PoC-decided).
2. **Identity and concurrency grounded in source** (§5.2): `external_capacity.id`
   binds to `RunModel.id` (flips are in-place, F17); `deployment_num` is the
   idempotency token; `force=True` is forbidden — dstack's apply CAS (F18)
   backs AC13 server-side.
3. **Ownership statements corrected** (header, §0, §1.4, §3, §16): dstack owns
   provisioning, execution, tunnels, ingress — not replica lifecycle. The
   "no autoscaler" non-goal re-scoped to load-based 1..N.
4. **Examples fixed to hardware reality** (F12 generalized): no single-A100
   anywhere in scope; single-H100 = p5.4xlarge (eu-west-2/us-east-1/us-west-2);
   platform region is eu-west-1; spec examples now `H100:1` / eu-west-2.
5. **Cross-backend fronting** re-classified from open question to documented
   feature (official gateways doc), smoke-tested as new PoC 0 together with
   live flip mechanics and the two unchased update-path checks.
6. **Server proxy modeled as the second door** (F19, §12.2.6; PoC 5 extended);
   §12.2.5 rationale returns to defense-in-depth.
7. **Facts F15–F19 added; F1/F4/F12 hardened.** PoC ordering re-sequenced
   around quota reality: Vast/DO first, AWS routed-private last, pending the
   eu-west-2 P5 quota decision — a human-owned decision the implementer
   correctly declined to file unilaterally.

---

## Appendix L — Changelog v0.6 → v0.7 (closing)

1. **Status: Accepted.** This document is the v0.1 implementation baseline.
2. **`provisioning_timeout` added** (destructive, reconciler-enforced): bounds
   runs that never reach ready — the one case neither RPS idle scale-to-zero
   nor native `max_duration` covers, since the latter excludes
   provisioning/pull (F14, verified in source). Drilled in PoC 6; AC15.
3. **`max_lifetime` reinstated as ALERT-ONLY** per review decision: an age
   alarm for human review, never destructive, never implemented via native
   `max_duration`.
4. **Gateway source restriction made load-bearing** (§12.2.5): listener SG
   limited to LiteLLM; native gateway wake, if PoC 2 finds it, is classified
   as a hazard to suppress. Verification added to PoC 2.
5. **Dual runtime contained**: PoC 8 scoped to one backend and framed as a
   protocol-boundary proof, not a runtime × provider matrix (AC14).
   llama-server's OpenAI surface grounded as F13, with upstream's own
   compatibility caveat carried into the PoC.
6. **F13/F14 added** to the verified-facts table; serving templates keep
   `utilization_policy` off to avoid fighting the RPS scaler (§6).

---

## Appendix M — Changelog v0.5 → v0.6

1. **Wake ownership closed:** reconciler is the sole explicit `0→1` owner.
   dstack remains responsible for provisioning after actuation, `1→N`,
   `N→1`, and `1→0`. PoC 2 now characterizes native gateway behavior but is
   no longer an ownership gate.
2. **Crash semantics made explicit:** Redis Streams is treated as at-least-once;
   pending-entry recovery plus idempotent lookup by `external_capacity.id`
   are required. PoC 3 now kills the reconciler on both sides of actuation.
3. **Runtime contract expanded:** v0.1 supports vLLM and llama.cpp server under
   one OpenAI-compatible HTTP boundary. Runtime-specific behavior moves to
   `runtime.template`; both templates are validated in PoC 8.
4. **Network invariant corrected:** runtimes may bind to an internal
   service/container interface. Security depends on zero public HTTP/runtime
   port mappings and the single-public-SSH-door proof, not localhost bind.
5. **Health state demoted from authority to hint:** Redis warm state has TTL,
   is invalidated by dstack/failure signals, and cannot override authoritative
   dstack/readiness state. Target failure emits fresh demand before fallback.
6. **Fallback graph constrained:** external models may fall back only to
   non-external/warm-capable targets in v0.1; cycles are rejected to prevent a
   single request from waking multiple external GPUs.
7. **Generic `max_lifetime` removed:** fail-closed deletion is not weakened by
   an arbitrary serving TTL. Idle scale-to-zero and budget alarms bound outage
   cost; future host rotation must be modeled separately as replacement+drain.
8. **Provider credentials clarified:** only dstack holds write/provisioning
   credentials; direct provider inventory/billing access by the reconciler, if
   needed, is read-only and separately scoped.
9. **Telemetry wording corrected:** no Kubernetes sidecar primitive is assumed;
   the runtime environment carries a telemetry agent/process.

---

## Appendix N — Changelog v0.4 → v0.5

1. **F10 corrected and grounded in source** (`backends/vastai/profile_options.py`):
   `offer_order` is a `backend_options` option, default **`score`** (Vast's
   composite console ranking) — not cheapest-first. Added companion knobs
   `min_reliability` (default 0.9) and `min_score`, and the upstream warning
   that `price` ordering should be compensated with stricter filters. §6
   updated accordingly.
2. **F12 added** (DigitalOcean catalog, verified against DO docs): no A100;
   EU GPU = Amsterdam/H100 only. Propagated to §5.1 (`gpu:` satisfiability),
   §12.3, PoC 1, and AC1.
3. **Teardown is now drain-first** (§5.2, PoC 8, AC6): deregister → stop
   accepting → bounded `drain_timeout` (new §5.1 field) → delete. Explicit
   disable no longer implies mid-stream cuts.
4. **Fail-closed cost explicitly capped** (§5.2): prolonged registry outage
   keeps capacity billing; `max_lifetime` + budget alarms are the designed
   bounds, and weakening fail-closed is stated as a forbidden "fix".
5. **PoC 3 made actuator-agnostic and sequenced after Gate A** (§13, AC4):
   pass criterion is one *effective* wake action regardless of which
   component actuates, valid under both Gate A outcomes.
6. Cosmetic: §8 line-wrap artifact fixed.
