# dstack's real API, read from source

Verified against `github.com/dstackai/dstack` at commit `a70d98b` (2026-08-27), by reading the
source directly — not docs, not DeepWiki. Everything below cites a file and a symbol so the
next reader can re-check it in one command.

**Why this file exists.** Ledger D1 says "the wire shape is unvalidated". That undersells it:
`internal/dstack` speaks a protocol Squall invented, and **none of it matches**. This file is
the ground truth to build against.

## 1. The endpoints

All run operations are `POST`, all under a per-project prefix
(`server/routers/runs.py`):

| Squall today | Real dstack |
|---|---|
| `POST /apply` | `POST /api/project/{project_name}/runs/apply` |
| `GET /runs/{name}` | `POST /api/project/{project_name}/runs/get` |
| `GET /runs` | `POST /api/runs/list` **or** `POST /api/project/{project_name}/runs/list` |
| `DELETE /runs/{name}` | `POST /api/project/{project_name}/runs/delete` (and `/stop`) |
| — | `POST /api/project/{project_name}/runs/get_plan` |

There is no GET anywhere and no `/runs/{name}` path parameter. The run name travels in the
body (`GetRunRequest{run_name, id}`).

## 2. Apply is two-step, and the CAS is an object, not an integer

`server/schemas/runs.py`:

```python
class ApplyRunPlanRequest(CoreModel):
    plan: ApplyRunPlanInput
    force: Annotated[bool, Field(description="Use `force: true` to apply even if the expected resource does not match.")]
```

`core/models/runs.py`:

```python
class ApplyRunPlanInput(CoreModel):
    run_spec: RunSpec
    current_resource: Optional[Run] = None   # "The expected current resource.
                                             #  If the resource has changed, the apply fails unless force: true."
```

Consequences for Squall:

- **The flow is `get_plan` → `apply`.** You submit a full `RunSpec`, get a `RunPlan` back
  (which carries `current_resource` and an `action`), and send it to `apply`.
- **F18's CAS is real and works as §5.2 assumes**, but the anchor is the *entire previous
  `Run` object*, not our invented `base_deployment_num` integer. The router docstring states
  it outright: *"Errors if the expected current resource from the plan does not match the
  current resource."*
- **`force` has no default — it is a required field.** So "Squall never sends force" becomes
  "Squall always sends `force: false`". The by-construction guarantee (AC13) survives: hardcode
  the literal `false` at the encode site and expose no field for it. What must NOT happen is a
  `Force bool` on a request struct that a caller can flip.

## 3. `deployment_num` is real, at two levels

`Run.deployment_num` and `JobSubmission.deployment_num`, both with the comment
*"uses a default value for compatibility with pre-0.19.14 servers"*. `Run.is_deployment_in_progress()`
compares the two — that is F17's in-place flip, in dstack's own words.

## 4. F35's probes — confirmed, with one detail the spec does not have

`core/models/configurations.py`:

```python
class ProbeConfig(CoreModel):
    type: Literal["http"]      # "expect other probe types in the future, namely `exec`"
    url / method / headers / body / timeout / interval
    ready_after: Optional[int]  # ge=1, "number of consecutive successful probe executions
                                #  required for the replica to be considered ready"
    until_ready: Optional[bool]
```

`core/models/runs.py` — and this is the part §6 does not yet account for:

```python
class Probe(CoreModel):
    success_streak: int

class JobSubmission(CoreModel):
    ...
    submitted_at: datetime
    deployment_num: int = 0
    status: JobStatus
    probes: list[Probe] = []
```

**There is no boolean on the wire.** `Run.ProbesReady` must be *derived*:

```
ProbesReady == every probe p in the submission has p.success_streak >= its ProbeConfig.ready_after
```

`ready_after` comes from the run spec we submitted, not from the response — so the client
needs both halves to answer the question. `Probe` carries nothing else: no last-check time,
no failure reason.

## 5. The Kubernetes backend does support what Squall needs

`core/backends/kubernetes/compute.py`:

```python
class KubernetesCompute(
    ComputeWithAllOffersCached,
    ComputeWithPrivilegedSupport,
    ComputeWithInstanceVolumesSupport,
    ComputeWithVolumeSupport,
    ComputeWithGatewaySupport,
    ComputeWithMultinodeSupport,
    Compute,
):
```

`ComputeWithGatewaySupport` is in the list and `create_gateway_replica` /
`terminate_gateway_replica` are implemented — so services and gateways both work on the
Kubernetes backend.

**A gateway is not even required.** The server mounts its own proxy (`server/app.py:259`):

```python
app.include_router(service_proxy.router, prefix="/proxy/services", tags=["proxy"])
app.include_router(model_proxy.router,   prefix="/proxy/models",   tags=["proxy"], deprecated=True)
```

`ServiceSpec.url` is documented as *"Full URL or path relative to dstack-server's base URL"* —
i.e. with no gateway it is the in-server proxy path. A CPU-only e2e can therefore skip domains,
TLS and gateway provisioning entirely.

## 6. ManualScaler is the default, and it does exactly what §6 needs

`server/services/services/autoscalers.py`:

```python
def get_service_scaler(count, scaling):
    if scaling is None:
        return ManualScaler(min_replicas=count.min, max_replicas=count.max)
```

```python
class ManualScaler(BaseServiceScaler):
    def get_desired_count(self, current_desired_count, stats, last_scaled_at) -> int:
        return min(max(current_desired_count, self.min_replicas), self.max_replicas)
```

`ReplicaGroup.replicas` is a `Range[int]`; `scaling` is *required* only when it is a range. So a
fixed `replicas: 1` gives `min == max == 1` and the desired count is clipped to exactly 1 —
and `replicas: 0` clips to 0. The 0↔1 flip is a plain re-apply with a different fixed count,
with no autoscaler in the loop. §10's two-lane rule is safe by construction here: with
`scaling` absent, `stats` is ignored, so request traffic can never move the replica count.

## 7. Statuses

`RunStatus`: `pending | submitted | provisioning | running | terminating | terminated | failed | done`.
`finished_statuses()` = `{terminated, failed, done}`. Note **`running` is a run status, not
readiness** — which is precisely §6's rule, confirmed upstream.

## 8. MEASURED against a running server — dstack **0.21.2**

Everything in §1-§7 is source-read. This section is different: a real `dstackai/dstack:latest`
server (reported version **0.21.2**) was started, its `/openapi.json` read, and the requests
below actually issued. These are measurements.

### 8.1 The error codes are NOT what Squall assumes — both sentinels are wrong

| Condition | Real dstack | `internal/dstack` today |
|---|---|---|
| Run does not exist | **HTTP 400**, `{"detail":[{"msg":"Run not found","code":"resource_not_exists"}]}` | expects **404** |
| CAS conflict | **HTTP 400**, `{"detail":[{"msg":"Failed to apply plan. Resource has been changed. Try again or use force apply.","code":"error"}]}` | expects **409** |
| Bad token | HTTP 403, `{"detail":{"msg":"Invalid token","code":null}}` | 403 ✓ (F23) |
| Service proxy, no such service | HTTP 404, `{"detail":"Service main/squall-probe not found"}` | 404 ✓ (F23) |

**dstack uses HTTP 400 for both**, and distinguishes them in the body: `code` is
`resource_not_exists` for the missing resource, and the CAS conflict is identifiable by its
message. Note the two `detail` shapes differ — a **list** of `{msg, code}` for API errors, a
bare **object** for the auth error.

Consequence, and it is severe: against a real server today, `ErrNotFound` and
`ErrResourceChanged` would **never** be produced. F20 (dead != asleep) and F18's CAS both
collapse into "generic error". The mapping must key off the body, not the status line.

### 8.2 The whole flow works, measured

- `POST /runs/get_plan` with a `run_spec` carrying **only** `configuration` -> **200**. A
  minimal service config is enough; `repo_*`, `ssh_key_pub`, `profile` may all be omitted and
  the server fills them in (`repo_id: "none"`, `repo_data: {repo_type: "virtual"}`).
- The plan echoes back normalised: `replicas: 1` becomes `{"min": 1, "max": 1}`,
  `interval: "5s"` becomes `interval: 5`. `action: "create"`, `current_resource: null`.
- `POST /runs/apply` with `{plan: {run_spec, current_resource}, force: false}` -> **200**,
  run created, `status: "submitted"`.
- **Re-applying the same now-stale plan -> 400 "Resource has been changed"**. F18's CAS is
  real and fires exactly where §5.2 says it does.
- **`replicas: 0` is accepted** -> `{"min": 0, "max": 0}`. The 0<->1 sleep flip is valid on a
  real server, which nothing had confirmed before.

### 8.3 `service.url` is the forward target — this closes D25

A real run carries:

```json
"service": {"url": "/proxy/services/main/squall-probe/", "model": null, "options": {}}
```

A **path relative to the dstack server's base URL**, no gateway involved. That is what
`squall-proxy`'s `Backend` should resolve to, instead of the `SQUALL_BACKEND_URL_TEMPLATE`
placeholder.

### 8.4 F25 confirmed live

The server printed `"The admin token is <token>"` to stdout on start, exactly as F25 says.

## 9. MEASURED end to end — dstack 0.21.2 driving a real Kubernetes backend on kind

A real `dstackai/dstack:0.21.2` server was run on kind's docker network with a Kubernetes
backend pointed at the e2e cluster, and a real service was provisioned, flipped, slept and
woken. Everything below was observed, not inferred.

### 9.1 It works — dstack created real pods in kind

```
default   dstack-main-probe-0-0-2cydzhwn   1/1  Running   # the service replica
default   dstack-main-ssh-jump-pod         1/1  Running   # created BY dstack, unprompted
```

No GPU operator needed for a CPU run. The SSH jump pod is provisioned automatically; only
`proxy_jump.hostname` had to be set, because kind nodes have no `ExternalIP`.

### 9.2 A run REQUIRES a fleet, and the Kubernetes fleet must have `nodes` target 0

The first service apply failed:

```
failed_to_start_due_to_no_capacity
"No matching fleet found."
```

`KubernetesCompute` does **not** implement `ComputeWithCreateInstanceSupport`, so a normal
fleet gets `NO_OFFERS` and terminates. `server/services/fleets.py` has the rule:

```python
include_only_create_instance_supported_backends = True
if nodes is not None:
    include_only_create_instance_supported_backends = nodes.target != 0
```

So the fleet must declare `nodes: 0..N`. With `nodes: 0..2` the fleet went `active` and the
service provisioned immediately.

**Squall's client has no fleet concept at all.** This is a missing subsystem, not a missing
field.

### 9.3 F17's in-place flip, measured

Re-applying with a changed `probes` block updated in place. Afterwards:

```
run status=running  deployment_num=1  jobs=2
  job[0] sub[0] status=terminated  dep=0  probes=[]
  job[1] sub[0] status=running     dep=1  probes=[{'success_streak': 20}]
```

**`jobs` accumulates across deployments.** The old replica stays in the list, terminated, with
`probes: []`, forever.

This breaks the obvious readiness derivation. "Every job's latest submission must be
probe-ready" returns **false forever**, because `job[0]` has no probes and never will.
`probesReady` MUST first filter to submissions whose `deployment_num == run.deployment_num`
and whose status is not finished. Getting this wrong reproduces D28 exactly: a healthy,
billing model that never reaches `Ready`.

### 9.4 The sleep flip works, and asleep is `pending` — not `terminated`

`replicas: 0` -> `deployment_num` 2, every submission terminated, **the replica pod is gone
from the cluster** (only the ssh-jump-pod remains), and the run settles at
`status: "pending"`.

So F20's "dead is not asleep" maps to run status: `pending` is asleep, `terminated`/`failed`
is dead. Both have zero live replicas.

### 9.5 THE serving-path measurement: the proxy answers 404 all the way through a wake

Polling the service proxy and the run together, from `replicas: 0` back to `1`:

| t | proxy | run status | probes |
|---|---|---|---|
| 1-3 | **404** | pending | [] |
| 4-5 | **404** | submitted | [] |
| 6-11 | **404** | provisioning | [] |
| 12-13 | **404** | running | `success_streak: 1` |
| 14 | **200** | running | `success_streak: 2` |

Two facts, both load-bearing:

1. **The whole wake answers 404 — never 503.** `internal/proxy/attempt.go`'s
   `classifyAttempt` retries only on 502/503/transport error and treats **404 as a commit**
   ("the engine answered"). Against a real dstack the first attempt of a held request commits
   a 404 to the user, and §7's held-request-as-oracle never holds anything. It works today
   only because `internal/dstack/mock` answers 503. See ledger D44.
2. **404 cannot distinguish dead from waking.** F23 reads gateway 404 as "deregistered/dead",
   but it is equally the waking state. The Model CR's phase must disambiguate; the gateway
   code alone cannot.

Also confirmed: **the proxy flips to 200 exactly when `success_streak` reaches `ready_after`**.
dstack's own service proxy is gated on the probes, so evidence (a) and the ability to serve
arrive together. Wall-clock for nginx on CPU: ~65s from apply to first 200.

### 9.6 Secrets at rest, measured (the "master key" question)

From the server's sqlite DB:

- `projects.ssh_private_key` holds the project-wide key as a **plaintext PEM**
  (`-----BEGIN PRIVATE KEY-----`, 1704 bytes). F7 said it is stored in the DB; it is stored
  **unencrypted**.
- `users.token` reads `enc:identity:noname:<token>` — the `identity` codec, i.e. the admin
  token sits in the DB in plaintext with a prefix declaring that no encryption is applied.
  dstack supports an `encryption:` block in `server/config.yml`; **absent it, the default is
  no encryption at all.**
- The backend `auth` blob is encrypted with the same codec, so the same applies.

Consequences for §12: the dstack server's DB is a credential store holding a provisioning-
capable token and an SSH private key in plaintext by default. Configuring `encryption:` is not
optional hardening, and the DB volume needs the same handling as a secret.

## What is still unverified

Superseded twice. §9 measured a full lifecycle against a real Kubernetes backend on kind
(provision, in-place flip, sleep, wake), and on 2026-08-27 the recreate path was measured too
(§9.7 below). What remains genuinely unmeasured:

- **Any non-Kubernetes backend.** Vast.ai, AWS and DigitalOcean are still read-from-source
  only. F21's fleet idle-release in particular is documented "VM-based backends only" and is
  unconfirmed on Vast's container instances (D9). That is Tier-2 / PoC 4, and it needs money.
- **A real GPU and a real engine.** Everything measured so far served nginx on CPU. Model
  load time, and therefore the relationship between `holdTimeout` and a cold start, is
  entirely unmeasured.
- **Squall submitting the run.** Every run above was applied by hand or by the CLI. Squall's
  own `internal/dstack` client has not yet driven a real server end to end — that is the
  Tier-1 `e2e-local` job, and D51 (the >=100GB default disk) has to be settled first.
- **`failed_to_start_due_to_no_capacity` handling.** Measured that it happens (D45, D51);
  squall has no code path that distinguishes it from a transient, so it would retry forever.

### 9.7 Recreating over a terminal run's name — MEASURED 2026-08-27

Driven against 0.21.2 with a Kubernetes backend, on a run stopped with `abort: true`:

| Call | Result |
|---|---|
| `runs/get` on the terminated run | **HTTP 200**, body is the run with `status: "terminated"`. NOT an error, NOT `resource_not_exists`. |
| `runs/get_plan` on the same name | `action: "create"`, and `current_resource` is **set** to the terminated run |
| `runs/apply` with `current_resource` **omitted** | **HTTP 200**. Fresh run: new `id`, `status: "submitted"`, `deployment_num` reset to **0**, `jobs` clean with no carryover |

Three consequences:

1. F20's "dead is not asleep" cannot be read off an error. `get` succeeds either way; only
   `status` separates a sleeping run (`pending`) from a dead one (`terminated`/`failed`).
2. Squall omitting `current_resource` on the recreate path is **accepted**, though it is not
   what dstack's own CLI would send — the plan offers the terminated run back. Omitting it is
   the stronger choice: there is nothing to CAS against, and a fresh mint is exactly F20's
   intent.
3. The `deployment_num` reset to 0 plus a clean `jobs` list is what makes `probesReady`
   correct across a recreate — the D46 filter has nothing stale to trip over on a new run.

### 9.8 Fleet selection for a run, and why dstack never auto-creates a fleet — MEASURED 2026-08-28

LIVE-7 (ledger D83): a run whose backend has no admitting fleet gets zero offers and no
error — exactly D58/D67's "looks fine, serves nothing" shape. Read against a real
`dstackai/dstack:0.21.2` container to settle two questions squall's fix depends on: does
dstack ever auto-create a fleet for such a run, and does `backends` actually filter which
fleet gets picked.

**A normal run ALWAYS runs fleet-candidate selection, unconditionally.**
`server/services/runs/plan.py`'s `get_job_plans` (~line 120): for a plan that is not
`for_offers_only` and does not target `--instances` directly, it calls
`_select_candidate_fleet_models(...)` regardless of whether the run names a `--fleet` or
not — there is no "no fleets configured, skip fleet logic" branch.

**Candidate selection is NOT backend-filtered at the query level.**
`get_run_candidate_fleet_models_filters` (`server/services/runs/plan.py`, ~line 246) builds
the SQL `fleet_filters` from exactly three things: project ownership (or an imported-fleet
subquery), `FleetModel.deleted == False`, and — only if the run is already pinned to one, or
names one via `run_spec.merged_profile.fleets` (a `--fleet <name>` reference) — a name match.
**`backends` never appears in this function.** In a project with N fleets across different
backends, ALL N are candidates; nothing here narrows to the run's requested backend.

**`backends` filtering happens later, per candidate, via requirements combination — and a
mismatch is caught, not fatal.** `find_optimal_fleet_with_offers`
(`server/services/runs/plan.py`, ~line 362) loops every candidate fleet and, for the offer
side, calls `_get_backend_offers_in_fleet` → `get_run_profile_and_requirements_in_fleet` →
`combine_fleet_and_run_profiles` (`server/services/requirements/combine.py:33`):
`backends=_intersect_lists_optional(fleet_profile.backends, run_profile.backends)`. This is
an **allow-set intersection**: `None` on either side means "unrestricted", so a fleet with no
declared backends is compatible with any run; two lists intersect to the backends common to
both; a run requesting `vastai` against a fleet scoped to `aws` intersects to empty, which
`_intersect_lists_optional` turns into `CombineError` → `combine_fleet_and_run_profiles`
returns `None` → `get_run_profile_and_requirements_in_fleet` raises
`ValueError("Cannot combine fleet profile")` → caught at `_get_backend_offers_in_fleet`'s own
`except ValueError` (~line 782), which simply contributes **no backend offers from that
fleet** rather than failing the whole plan. So yes, `backends` filters — but as an
elimination inside the offer computation, not as a database predicate, and a mismatched
fleet is silently skipped rather than erroring.

**A permissive elastic fleet (`nodes: "0..N"` or unbounded `"0.."`) does NOT block cloud
provisioning.** `can_create_new_cloud_instance_in_fleet` (`server/services/fleets.py`,
~line 1003) refuses a new cloud instance only when `fleet_spec.configuration.nodes.max is
not None` **and** `len(active_instances) >= nodes.max` — its own comment calls `nodes.max`
"a soft limit that can be exceeded when provisioning concurrently". A fleet with `nodes.max
== None` (squall's auto-created fleet always sends `"0.."`, `internal/dstack/http.go`'s
`createFleet`) or with room under its max never trips this gate. This closes off the
"maybe our own generous elastic floor is what's blocking the run" hypothesis raised before
this was read from source.

**When zero candidate fleets survive filtering, the result is a normal, empty plan — not an
auto-created fleet.** Back in `find_optimal_fleet_with_offers`: `if not
candidates_with_backend_offers: return None, [], []` (~line 134). No exception, no fleet
minted, no signal distinguishing this from "the market genuinely has nothing available"
(D58) or "the backend token itself is broken" (D67). This is the run-apply path; it never
imports or calls `_create_fleet`.

**Fleet auto-creation exists, but only behind the FLEET api's own two-step apply, called
explicitly.** `server/services/fleets.py`'s `apply_plan` (~line 549): if the fleet spec names
no existing fleet (`configuration.name is None`) or names one that does not yet exist
(`get_project_fleet_model_by_name(...)` returns `None`), it calls `_create_fleet(...)`
(~line 1023) directly — this is the *fleet* API's own `get_plan`→`apply` two-step
(`server/routers/fleets.py:128` `get_plan`, `:149` `apply_plan`), mirroring the *run* API's
apply exactly, and it is what `internal/dstack`'s `EnsureFleet` drives. **Nothing on the run
side ever reaches this code.** grepping every run-processing file (`services/runs/*.py`,
`background/pipeline_tasks/*.py`) for a call into `services.fleets.create_fleet` or
`.apply_plan` finds none; the only importers of the fleets service module outside
`routers/fleets.py` itself are `runs/plan.py` (read-only candidate/offer helpers) and
`backends/handlers.py` (backend-removal bookkeeping).

**Live-probed, not just read from source:** against a running server, `fleets/get` on an
unknown fleet name answers **HTTP 400** with
`{"detail":[{"msg":"Resource not found","code":"resource_not_exists"}]}` — the identical
shape `runs/get` uses for a missing run, and the one `internal/dstack/errors.go`'s existing
`classifyError`/`ErrNotFound` already handles with zero new classification code.

**Conclusion (Branch B, decided over Branch A):** dstack never auto-creates a fleet for a
run that names a backend nothing admits. A hand-declared fleet that quietly disappears (a
database reset, a manual delete, a project rename) is a silent, permanent dead end for that
backend until something OUTSIDE the run path — squall's own `EnsureFleet`, or a human —
creates one. This is what makes LIVE-7 possible and what the fix (§ preflight remediation,
`internal/controller/squall/preflight.go`) closes.

### 9.9 `idle_duration` is gated on `dockerized`, and Vast.ai fails the gate — SOURCE-VERIFIED 2026-09-03

Read from the deployed **0.21.2** (`squall-system/dstack-*`, package under
`/root/.local/share/uv/tools/dstack/lib/python3.11/site-packages/dstack/_internal`).

F21 carried the phrase *"only applied for VM-based backends"* as a caveat to verify. It is
not a caveat. It is a hard branch, and the discriminator is `JobProvisioningData.dockerized`:

```python
# server/background/pipeline_tasks/jobs_submitted.py:1793  (_create_instance_model_for_job)
if not job_provisioning_data.dockerized:
    termination_policy = TerminationPolicy.DESTROY_AFTER_IDLE
    termination_idle_time = 0          # the profile's idle_duration is never read
else:
    termination_policy, termination_idle_time = get_termination(
        profile, DEFAULT_RUN_TERMINATION_IDLE_TIME
    )
```

`_promote_placeholder_instance` (line 1834) repeats it verbatim, so both provisioning paths
agree. The terminator then acts on the stamped value:

```python
# server/background/pipeline_tasks/instances/check.py:92
idle_duration = get_instance_idle_duration(instance_model)
if idle_duration <= timedelta(seconds=instance_model.termination_idle_time):
    return None
```

With `termination_idle_time = 0` any idle time terminates — the first background pass after
the job stops.

| Backend | `dockerized` | `idle_duration` honoured |
|---|---|---|
| AWS | `True` (`aws/compute.py:418`, *"because `dstack-shim` is used"*) | yes |
| Vast.ai | `False` (`vastai/compute.py:173`) | **no** |
| Kubernetes | `False` (`kubernetes/compute.py:339`) | **no** |
| RunPod | `False` (`runpod/compute.py:240`, `:337`) | **no** |

Second, independent confirmation for Vast: `VastAICompute` mixes in only
`ComputeWithFilteredOffersCached, Compute` — no `ComputeWithCreateInstanceSupport`, which
AWS carries at `aws/compute.py:122`. §9.2's rule means a Vast fleet must declare
`nodes` target 0, so Vast never pre-provisions a standalone instance: every Vast instance is
job-provisioned and therefore routed through the branch above. There is no path by which a
fleet's `idle_duration` reaches a Vast instance.

`idle_duration: -1` (`DONT_DESTROY`) is unreachable for the same reason: `get_termination`
is what maps a negative value to that policy, and the `not dockerized` branch returns before
calling it.

**What this means for squall.** On the two backends squall ships against there is **no warm
pool**. Spec §7 treats `hold` as viable only inside the warm window; on Vast that window is
zero-width, so `holdTimeout` must cover a full cold provision — instance, image pull and
weights — or the proxy always falls through. Cost-wise the direction is safe (a container
backend cannot leave an idle billing instance), which is why this is an expectations finding
rather than a money bug. The chart still requires `idleDuration` because it cannot know which
backend a given fleet will draw from, and the value does bind on VM backends.

**Standard: source-verified, not measured.** One notch below F21's own `measured` rows. A
live Vast run timing job-stop to instance release would close it.
