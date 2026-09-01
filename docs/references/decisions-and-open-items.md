# Decisions taken, and items that need a human

## Decisions — do not reopen

- **Repo will be GitHub, public OSS**, Apache-2.0, same posture as `../alitellm-operator`.
- **`docs/specs/` is published with the repo.** Scanned clean: no private IPs, emails, AWS
  account IDs, ARNs, internal hostnames or credentials. Two consequences: the threat model
  becomes public (deliberate), and so do the dstack findings — including that the dstack
  server prints its admin token to stdout on every start (F25). **File the dstack upstream
  issues before or with publication**, so the public record shows the finding and the fix
  attempt together.
- **v0.1 release scope: drift gates + pre-push gate only.** No goreleaser, no SBOM, no
  cosign — nothing is published yet and each is additive. The **drift gate is not deferred**:
  the sibling project shipped stale CRDs to users twice because nothing enforced that a
  changed CRD field regenerated the published chart. Squall inherits that failure mode the
  moment `Model` ships in a chart.
- **All four plan gate decisions (D1–D4) are closed in spec v0.17.** `weights` is cut from
  the CRD; `engine` stays and is load-bearing; the proxy decision table has all six phases;
  the LiteLLM fallback chain is `router_settings`, owned by Helm values, outside v0.1 scope.
- **Spec v0.7 was deleted.** v0.17-RC supersedes it and is the only spec in the tree.

## e2e must include a real dstack server — decided, not yet built

Every test in the repo runs against the fake, and the fake speaks a wire protocol Squall
invented (`POST /apply`, `base_deployment_num`, …) that has never touched a real dstack
server. If e2e also used the fake, **no layer anywhere** would catch a wire-shape mismatch
and it would surface on first contact with a real backend.

The spec shows this is cheap: F20, F23 and F24 are marked *measured (PoC 0-CPU)*, so a real
dstack server has already been run CPU-only, with no GPU and no marketplace spend.

Two tiers, to be built in the Phase 11+12 block (`make e2e-run` / `e2e-full` are stubs today):

- **Tier 1 — `e2e-local`, no credentials, CI-able.** kind + a real dstack server in a
  container, CPU backend. Validates: the wire shape actually decodes; F17 in-place flip
  (`deployment_num`++, same run id); F18 CAS (stale base → "Resource has been changed", force
  never sent); F20 dead ≠ asleep (terminal → deregistered → 404, new run id on next apply);
  F23 gateway codes (503 / 404 / 403).
- **Tier 2 — `e2e-cloud`, credentials and money, run by a human.** Vast.ai, real GPU. PoC 2
  wall-clocks, F21 per-backend instance release (the Vast caveat the spec flags as
  unresolved). Never in CI.

This does not change the fake's job — it stays for fast unit and envtest. Tier 1 is what
proves the fake is not lying.

## Demand is self-expiring — decided 2026-08-26

`hasDemand` must read the **value** of `DemandAnnotation` (`squall.ackstorm.ai/demand-since`),
not its presence, and treat demand as live only inside a TTL:

```go
demandSince, ok := parse(m.Annotations[DemandAnnotation])
hasDemand := ok && now.Sub(demandSince) < demandTTL
```

**The bug this fixes.** Phase 6 shipped `hasDemand` as a presence check, and nothing anywhere
writes or clears the annotation. Once the proxy sets it, demand is true forever: the model
wakes, sleeps, and is immediately re-woken on the next reconcile. **Scale-to-zero could never
work** — the product thesis. No test caught it because the proxy that writes the annotation
does not exist until Phase 9.

**Why not have the controller clear it on `Ready`** (the other candidate): clearing races the
proxy. Proxy writes demand at `t`, controller clears at `t+ε`, and the request that caused the
wake waits forever. That is a **lost wake**, and lost wakes are the one direction the
invariants forbid — `0→1` fails open. Self-expiry has no write from the controller, so no
race and no lost wake; a stale annotation ages out on its own and a crashed proxy cannot pin a
GPU awake indefinitely. It also stays idempotent and level-triggered, like everything else in
the reconciler.

Consequence for Phase 9: the proxy must **refresh** the annotation while demand persists, not
write it once. §5.2's coalesced patch already has that shape. Default TTL:
`scaleDownDelaySeconds`.

## REJECTED — the per-model shadow proxy (2026-08-27)

Two independent architectural reviews rejected `docs/references/rfc-shadow-proxy.md`, both
variants. Recorded with reasons so the idea does not return under a different name.

**The fact that closed it: dstack already has the entire mechanism §6 assumed we must build.**
`ProbeConfig` is first-class in the service config (HTTP probes with `interval`, `timeout`,
`ready_after`, `until_ready`), modelled on Kubernetes readiness probes; upstream's own words:
*"Without probes, a replica is treated as available the moment its container comes up; with
them, availability reflects an actual response from the app. This matters most while models
are loading."* Rolling deploys already gate on `running` **and** probes passing. And
`JobSubmission.probes` is exposed in the API model the client already deserializes, next to
`submitted_at` and `age` — verified in dstack source.

Nobody checked upstream before writing the RFC. That single fact inverted the conclusion.

**Why the shadow was wrong on its own terms, beyond being unnecessary:**

- Probing `shadow → gateway → SSH → replica` **conflates transport health with engine
  health**; dstack's probe runs against the replica and can tell them apart. Strictly weaker
  evidence than what it would replace.
- **Two sources of truth was the disqualifier**, not a cost to weigh. `status.phase` has
  exactly one writer by design and D5 depends on it. A shadow lets `readyReplicas` and `phase`
  disagree both ways: shadow-ready + run-dead → forward into a 404; run-ready + shadow-unready
  → 503 against a billing GPU.
- **It moves a bug class into the layer that cannot catch it.** Readiness is pure and
  table-tested under `-short` today. A shadow moves it behind Deployment status, observable
  only in envtest — **where RBAC is not enforced**. That is exactly D29's blind spot
  (`endpoints` `get` vs `list;watch` silently disabled §6 sleep in every real deployment), and
  a shadow needs new `apps/deployments` RBAC, the same verb-set class already got wrong once.
- **`progressDeadlineSeconds` is the wrong clock** — it measures a local pod rollout, not
  remote GPU provisioning, and cannot distinguish "probe failing for 45m" from "image pull
  failed".
- A Pod marked `Running` that represents a container in Romania is **a fiction that misleads
  the scheduler, quotas, and the on-call at 3AM**.

**The four claimed benefits, reassigned to owners that already exist:**

| Claimed benefit | Real owner |
|---|---|
| Readiness | dstack probes, read from `JobSubmission.probes` |
| `provisioningTimeout` | a journaled timestamp (see the open anchor question below) |
| `kubectl` visibility | `additionalPrinterColumns` on `Model` — ~10 lines of markers |
| Request/token metrics | `squall-proxy`, already in the data path with a `ModifyResponse` hook |

**The RFC's own §5 sentenced it:** *"the engine still has to become ready; the shadow only
changes who observes it."*

**What survives, and it is the best idea to come out of the exercise:** the **held request is
its own oracle**. During the hold, the proxy retries forwarding the *real user request*
against the gateway — 503/502/conn-refused → keep waking; success → stream to the client.
First-party demand, so the two-lane rule never applies, and it serves the first token even
before anything writes `Ready`. Phase then carries two evidences: dstack probes as the primary
source (controller) and `lastSuccessAt` reported by the proxy on the activity endpoint the
controller already reads.

**The two-lane rule is NOT relaxed.** No accepted design needs it relaxed. Add a clarifying
sentence, not an exception: *the retry of a real held request is demand, not monitoring;
first-party traffic is never a probe.* dstack's own probes are its internal machinery,
existing with or without squall, and do not count against the rule.

**The shadow proxy is the right design for `fake-dstack` and the wrong design for
`squall-controller`** — it is queued follow-up #1, correct in the fake, promoted by mistake
into the production architecture.

## Agreed follow-ups — queued 2026-08-27

Raised by the project owner; not yet implemented.

### 1. fake-dstack should PROVISION model-mock, not have it pre-deployed

Today `model-mock` is a permanent Deployment in the e2e cluster — it runs even while the
Model is `Asleep`, which is fake in the worst way: in reality the container does not exist
until a wake provisions it.

Instead, fake-dstack should act as a small provisioner:

```
Apply(replicas: 1) -> create the model-mock Deployment
                   -> pod starts        -> report "running"
                   -> readinessProbe OK -> report "ready"
Apply(replicas: 0) -> delete the Deployment
```

**Why this is the right shape:** it solves the readiness gap and the realism gap together.
The replica-ready signal the controller needs (see D28) gets an honest source — the
Deployment's own `status.readyReplicas` — instead of a state machine that merely asserts it.
The fake stops pretending and starts earning the signal, which is what real dstack does, just
with pods instead of GPUs. It also removes the controller-pause workaround (D38) and makes the
wake -> serve -> sleep loop true end to end.

**Cost:** fake-dstack needs RBAC to create/delete Deployments in its own namespace.

### 2. Rename the deployments to match the binaries

- `squall-controller-manager` -> `squall-controller`
- `proxy` -> `squall-proxy`

The kubebuilder scaffold named the first; the second is too generic for a cluster that may
host other proxies. Touches `config/`, the e2e overlays and any manifest referencing them.

### 3. MkDocs site documenting how to configure each backend in the Helm chart

Raised 2026-08-27. `deploy/helm/squall/values.yaml` currently carries the only documentation
for this, as inline comments with a worked example per backend. That is the wrong home for it:
it is the first thing an operator needs and the last place they look, and comments in a values
file cannot show a full worked flow.

What the page has to cover, per backend (vastai, aws, digitalocean, kubernetes):

- The exact `dstack.backends` block, since it is a **verbatim passthrough of dstack's own
  schema** — the chart adds nothing, so the reference is dstack's `BackendConfig` for that
  provider and it must be pinned to a dstack version.
- How credentials are supplied: `dstack.credentialsSecretName` + `${VAR}` placeholders,
  resolved by the `render-config` init container. Say plainly WHY the indirection exists —
  dstack does not interpolate env vars in `server/config.yml` — or the next reader will
  "simplify" it away and put credentials back in a values file.
- The interaction with the CR: a Model provisions only on a backend present in BOTH
  `dstack.backends` and its own `spec.placement.backends` (§12.3's compliance allowlist).
  This is the part nobody guesses.
- Per-backend gotchas already measured or read in source:
  - kubernetes: the fleet's `nodes` target MUST be `0..N` (D45), and `proxy_jump.hostname` is
    required because kind nodes have no ExternalIP.
  - vastai: `community_cloud` — LIVE-7 (2026-08-28): the chart's example previously showed
    this pinned to `false`, contradicting dstack's own default (`true`,
    `VASTAI_COMMUNITY_CLOUD_DEFAULT`) with no measurement behind the override. Corrected: the
    chart now leaves it commented out (dstack's own default applies) and documents that
    `false` only removes the individual-operator pool for reliability/trust reasons — §12.3
    already treats Server Cloud and Community Cloud identically ("internal, non-regulated
    workloads only"), so this is NOT a compliance knob, and D60's already-thin filtered EU
    catalogue makes restricting further to Server Cloud only something to measure before
    setting, not a safe default. `regions` are Vast's own location strings with **exact**
    matching and no "europe" grouping (`server/services/offers.py`); dstack additionally
    hardcodes `verified: true`.
  - aws: `creds.type: default` for IRSA/instance profiles instead of a static access key.
  - all: `spec.resources.disk` must be set or dstack's 100GB default applies and is billed
    (D55).

Not started. No mkdocs scaffold exists in the repo yet.

### 4. Tests the Vast.ai session proved we do not have — queued 2026-08-27

The first real provision on Vast (RTX5090, kr-southkorea, $0.41/h, Asleep -> Ready -> torn
down) found four things in one sitting. Every one was invisible to the existing suite. These
are the tests that would have caught them:

1. ~~**Delete a Model while its run is ACTIVE**~~ — DONE 2026-08-27:
   `TestReconcile_Delete_StopsBeforeDeletingAnActiveRun`, plus the `Reaper` and its six tests.
2. ~~**Make the fake refuse `Delete` on an active run**~~ — DONE 2026-08-27:
   `ErrDeleteActiveRun`, `TestDelete_RefusesAnActiveRun`,
   `TestHTTP_DeleteRun_RefusesAnActiveRun`.
2b. **Still missing: a cluster-level e2e for the reaper.** Every reaper test is a unit test
   against a fake client. Nothing yet proves the sweep runs under a real manager, wins leader
   election, and reaches a real dstack — which is exactly the layer D56 lived in.
3. **A "no offers" case that asserts squall fails LOUDLY.** D58's silence — zero offers, no
   error, no log — is indistinguishable from four unrelated causes. Squall should surface
   `failed_to_start_due_to_no_capacity` as a Model condition with the reason, not retry.
4. **A chart-upgrade test** that changes `dstack.backends` and asserts the dstack Pod actually
   rolled (D57). The `checksum/config` annotation fixes it; nothing guards it.
5. **A startup assertion that a usable fleet exists** (D58). Cheaper than any test: fail at
   boot rather than one silent wake at a time.

Also worth building, from the same session: a `Model` admission check that validates region
strings against the backend catalogue (D59), since a typo there is silently identical to "no
capacity".

### 5. Vast.ai offer tuning we CAN expose but have not — 2026-08-27

`backend_options` is a per-run profile field (`core/models/profiles.py:392`), so three of
dstack's Vast knobs are reachable from a `Model` CR today and are simply not wired:

- `offer_order` — `score` (default) or `price`
- `min_reliability` — default 0.9
- `min_score`

The other five filters in `vastai/compute.py:58` (`verified`, `cuda_max_good >= 12.8`,
`compute_cap`, `direct_port_count`, `inet_down`) are hardcoded in dstack and unreachable
without patching it upstream. Worth knowing before promising a knob we cannot deliver: see
D60 for why these matter to the cost model.

### 6. Engine-support facts for the target model — established 2026-08-27

Nobody had the earlier deployment notes, so this was re-established from source. Recorded
because it will otherwise be re-derived every time, and because it dates fast.

- **vLLM v0.28.0** (released 2026-08-26) is pinned in `config/samples/`
  (`sha256:2286e853...`, linux/amd64). It is **not a floor** — see the next bullet. It is
  simply the newest, and it carries v0.27.0's fused CUDA post-conv MTP decode kernel for
  "Qwen3.5 GDN" (Gated DeltaNet, this model's attention) plus its own "Qwen3.5 fixes for
  text-only checkpoints".
- **Multimodal support: RESOLVED 2026-08-27, it is supported.** An earlier reading of this
  same page called it an unresolved risk on the grounds that every release note says
  *text-only* while Qwen3.8-27B is multimodal. **That reading was wrong, and the way it was
  wrong is the lesson: release notes are not the registry.** Checked against source —
  the checkpoint's `config.json` declares `"architectures":
  ["Qwen3_5ForConditionalGeneration"]` with `language_model_only: false`, and that exact
  name is registered in vLLM's `_MULTIMODAL_MODELS` map
  (`vllm/model_executor/models/registry.py:592` at v0.28.0, `:581` at v0.27.0, and present
  as far back as v0.26.0). The vision path is registered. Do not re-raise this from the
  release notes alone.
- **The repo is NOT gated.** HF API reports `gated: false, private: false`, and an
  unauthenticated range-GET of `config.json` returns 206. **No HF token is required to
  serve this model.** The sample keeps `secretEnv` anyway — an authenticated pull gets the
  higher rate limit, and it exercises D63's path end to end — but the §12.4 worry about a
  token reaching a machine we do not control does not arise for THIS model. It will for the
  next gated one.
- **An official FP8 checkpoint exists**: `Qwen/Qwen3.8-27B-FP8` (verified). Prefer it over
  `--quantization fp8` on the BF16 weights — calibrated, reproducible, half the download.
- **Context**: native 262,144. A 128k window needs no YaRN and no
  `VLLM_ALLOW_LONG_MAX_MODEL_LEN`; those are only required past 262k.
- **F29 no longer applies to this deployment.** Its "~16 GiB, Q4_K_M" was measured under
  llama.cpp/Ollama and GGUF is not a vLLM path. **MEASURED from the HF tree API 2026-08-27:
  28.8 GiB total, 28.7 GiB of it safetensors, across 81 per-layer shards** — plus KV. The
  conclusion F29 drew still holds — no 80 GB card needed — but the number does not.
- **Still unverified, and now the only engine unknown:** `--kv-cache-dtype fp8` against this
  architecture's mixed `linear_attention`/`full_attention` layer stack (GDN every 4th layer
  is full attention). The model card does not mention the flag. It is our choice; drop it
  first if the server rejects the cache config.

### 7. Verify the replica is serving the model the CR asked for — from D65

A probe proves an HTTP server answers. It does not prove WHICH model answered, and on
2026-08-27 a run with no `commands` served vLLM's built-in default (`Qwen/Qwen3-0.6B`)
with `success_streak` climbing and `/health` returning 200 the whole time.

`GET /v1/models` on the replica returns `data[].id`, which is exactly `--served-model-name`.
Squall never reads it. Reading it once when a run first probes green, and refusing to write
`phase: Ready` when it does not match the Model's name, closes the gap for every engine that
speaks the OpenAI API. Cheap, and it is the only check that would have caught D65.

Related: D67's other half — assert at startup that every backend a Model names in
`spec.placement.backends` is actually configured on the dstack server, since an unconfigured
backend is indistinguishable from an empty market (D58, D67).

### 8. vLLM emits raw chain-of-thought into `message.content`

Observed on the first real completion: the reply began with the model's reasoning and a
literal `</think>` before the actual answer. vLLM needs `--reasoning-parser` (the Qwen3
family uses the `deepseek_r1`-style parser) to split it into a separate field. Not a squall
bug — a missing flag in the sample CR — but anything registering this model in LiteLLM will
ship the reasoning to end users until it is set.

## Open items for a human

- **Create the GitHub repo and push.** No git remote exists; nothing has ever been pushed.
  Outward-facing and irreversible — confirm the name (`ackstorm/squall`?) before doing it.
- **File the two dstack upstream issues** (host-key verification; admin token on stdout)
  before publication.
- **Run Tier 2 e2e / PoC 0 and PoC 2** — needs vast.ai credentials and a dstack server. The
  agent working this repo has no `kubectl` and no cloud CLI.
- **One spec question worth raising:** under the warm-window validation rule as implemented,
  *the spec's own §5.1 example CR triggers the warning* (`holdTimeout: 20m` vs a `5m + 10m`
  warm window). Either the rule's threshold or the example is off by intent. It is a warning,
  not a rejection, so nothing is blocked — but the spec's canonical example probably should
  not be a declared misconfiguration.
- **`DemandAnnotation` is an invented key.** The controller uses
  `squall.ackstorm.ai/demand-since`; neither spec nor plan names one, and the real writer is
  squall-proxy in Phase 9. If the proxy lands a different key, demand is never seen — and
  every current test still passes. Settle it when Phase 9 is built.

## Decision: the serving data path stays on dstack's service proxy for 0.1.0; the gateway is 0.2.0

**Decided 2026-08-28 by the owner, after a live load test. Do not reopen without new measurements.**

A concurrency ramp against a real Vast.ai GPU (RTX PRO 6000, cz-czechia, $1.96/h) found squall's
serving ceiling is **~19 concurrent requests**, and it is **not the GPU**: throughput scaled linearly
from 30.7 to 438 tok/s between 1 and 16 concurrent while latency stayed flat at ~43s — the signature
of an unsaturated accelerator. Above that, requests failed with a plain-text `Internal Server Error`
(not squall's JSON) and dstack's own log gave the cause:

    sqlalchemy.exc.TimeoutError: QueuePool limit of size 20 overflow 20 reached, timeout 30.00

**Root cause is connection LIFETIME, not query cost.** `server/services/proxy/deps.py` exposes both
the repo and the auth provider as FastAPI `yield` (generator) dependencies, each wrapping its own
`get_session_ctx()`. FastAPI tears a `yield` dependency down only *after the response completes*, and
squall's responses are long streaming generations (~43s measured). So every in-flight request pins
**two** DB connections for its whole duration, idle for nearly all of it. Postgres's own caching is
irrelevant to this: the connections are held, not busy. The predictive model is

    max_concurrent ~= (DSTACK_DB_POOL_SIZE + DSTACK_DB_MAX_OVERFLOW) / sessions_per_request

with `sessions_per_request = 2` while `configuration.auth` is true (dstack's default; squall never
overrides it). `(20+20)/2 = 20` against 18-19 measured. **Beware:** the owner's production GitOps
config sets `DSTACK_DB_POOL_SIZE=10, DSTACK_DB_MAX_OVERFLOW=0`, which by the same formula implies
**5 concurrent generations**. That cap was chosen to protect Postgres `max_connections`, before this
coupling was understood.

**Options examined** (`.superpowers/.../datapath-investigation.md`, `gateway-investigation.md`):
- *Direct squall-proxy -> replica*: **RULED OUT, measured impossible.** Vast's `job_provisioning_data`
  exposes only `hostname`/`ssh_port`; Vast maps exactly one direct port and dstack spends it on SSH.
  The app port is published nowhere. squall would also need the project's SSH private key, which no
  API exposes (it sits unencrypted in dstack's DB — D47), plus a reimplementation of dstack's tunnel
  lifecycle. This is rewriting dstack, not removing it from the path.
- *`configuration.auth: false`*: halves the sessions per request, but any in-cluster caller could then
  reach the GPU directly. Rejected on §12.3 grounds.
- *Postgres*: taken for 0.1.0 — see below.
- *dstack gateway*: **taken for 0.2.0.**

**Why the gateway is the real fix, and why it is not 0.1.0.** The gateway's routing repo is a pure
in-memory dict pushed by the server, with no database in the request path at all — it removes the
connection pinning structurally rather than raising a limit. On the **Kubernetes backend we already
configure**, `KubernetesCompute.create_gateway_replica` builds it *in-cluster*: one `ubuntu:22.04`
Pod (no GPU) plus a Service, so it is NOT the billed EC2 the owner remembers from the AWS backend.
It is deferred because it needs real work and two unresolved conditions:
1. **A LoadBalancer-capable cluster.** `create_gateway_replica` hardcodes `type="LoadBalancer"` with
   no ClusterIP branch, and blocks 120s for an external address before tearing the instance down.
   Our kind dev cluster has no MetalLB/cloud LB controller, so gateway creation would fail today.
2. **A domain is mandatory** — not to create the gateway, but at service registration, where dstack
   raises `"Domain is required for gateway"` unconditionally. It may be internal. TLS is genuinely
   optional (`certificate: null`).
Squall-side work: gateway provisioning, a `Gateway` field on `ApplyRequest`, and an absolute-URL
branch in `StatusBackend.URL` (today `status.serviceURL` is a path joined to dstack's base URL).
**Open security decision for 0.2.0, the owner's to make:** `public_ip` defaults to `true` and the
Service type is forced, so exposure must be constrained at the LB layer (internal NLB annotation on
AWS, MetalLB on a private range in dev). That, not TLS, is the §12.3 question.
Auth is *not* fully DB-free even with a gateway: `GatewayProxyAuthProvider.is_project_member` caches
60s per (project, token), so it degrades to ~1 DB hit per minute per Model — and if dstack-server is
down longer than 60s, authenticated requests start failing while routing itself still works.

**0.1.0 therefore ships Postgres**, which fixes a *correctness* problem independent of throughput:
`advisory_lock_ctx` is documented **"No-op for SQLite"**, so dstack's cross-process resource locking
does nothing on SQLite. The concurrency ceiling then has to be raised deliberately by sizing the pool
against target concurrency (not shrinking it to fit `max_connections`), and **re-measured** — a number
nobody should assume.

## Decision: no Fleet CRD. `status.fleet` on the Model is what 0.1.0 ships

**Decided 2026-08-28 by the owner. A declarative Fleet CR is rejected, not deferred.**

Context: LIVE-7 (see the ledger) — the vastai fleet squall's live GPU runs depended on existed only
inside dstack's SQLite database, created by hand during debugging. Migrating to Postgres started
dstack on an empty database and the fleet vanished, so every vastai Model reported
`Schedulable=False / NoFleet`. Nothing in any repository described it.

**What a fleet is**, since this keeps being misread: not a filter, a *pool*. Closest analogue is a
managed-Kubernetes node pool — it declares which backends instances may be drawn from and how many
(`nodes: "0..2"`), scales 0→N on demand, and persists as an object even while empty. A Model's
`spec.placement`/`spec.resources` are the *run's* requirements, a separate thing; dstack **intersects**
the two (`server/services/requirements/combine.py`, `_combine_resources`). An empty fleet costs
nothing — instances appear when a run lands and are released by the fleet's `idle_duration` (F21).

**Three shapes were considered:**

1. *Declarative Fleet CR (spec + status).* Its real value would be **recovery**: it would make
   dstack's database disposable for this object, since the controller could rebuild fleets from CRs
   after a wipe or a restored backup — precisely the property LIVE-7 proved we lacked. **Rejected by
   the owner**, who does not consider a fleet something squall should declare: it is dstack's object,
   and owning its definition misrepresents what a fleet is. Do not re-propose it as a 0.2.0 item
   without new evidence; this was a considered "no", not a "not yet".
2. *Read-only mirror CR (status only).* The only CR shape the owner would entertain, and only for
   visibility. Noted as a possible future convenience, with the standing caveat that a mirror is
   **not** recovery — it would have faithfully reported that the fleet was gone and changed nothing.
3. *`status.fleet` on the Model.* **Taken for 0.1.0.** It gives the same visibility for the cost of
   one status field, with no new CRD, no API-versioning commitment, and no second source of truth.

**What actually removes the failure mode** is not any CR: it is `EnsureFleet` (LIVE-7's fix), which
makes the fleet *derived state*. The Model already declares which backends it needs, so the
controller creates the admitting fleet if it is missing — level-triggered, idempotent, create-only,
and failing open (a creation error downgrades that backend to "unfleeted" and is reported on the
`Schedulable` condition; it never blocks a wake).

**If a mirror CR is ever built, two traps recorded here so they are not rediscovered:**
- *Two sources of truth.* The fleet would exist in dstack's DB and in the CR, and they can diverge —
  someone deletes or edits it in dstack. Decide which wins, and make sure the loop cannot oscillate.
- *The finalizer is the dangerous part.* The obvious design (deleting the CR deletes the dstack
  fleet) would be squall's first deletion path capable of terminating instances that are serving a
  live generation. That is a `1→0` fails-safe violation. Any such CR must adopt and create, and
  **never delete** — exactly as `EnsureFleet` does.

## PROVEN 2026-08-28 — the 0.2.0 data path: squall-proxy tunnels straight to the replica

Prototyped and MEASURED against a live Vast.ai GPU (RTXPRO6000WS, no-norway), not designed
on paper. This settles the "gateway" question, including its cost.

### The mechanism

dstack's own server reaches a replica by opening an SSH tunnel to it and forwarding the
engine port — `server/services/jobs/job_replica_tunnel.py`, which forwards
`localhost:{job_spec.service_port}` over `container_ssh_tunnel`. Everything squall needs to
do the same is already in the `runs/get` response it already fetches:

| field | live value |
|---|---|
| `job_provisioning_data.hostname` | `79.161.156.12` |
| `job_provisioning_data.ssh_port` | `40097` |
| `job_provisioning_data.username` | `root` |
| `job_provisioning_data.ssh_proxy` | `None` (Vast is ONE hop) |
| `job_spec.service_port` | `8000` |

The engine port is NOT publicly reachable (`:8000` refused; `:40097` answered
`SSH-2.0-OpenSSH_9.6p1`), so SSH is the only way in — and that is also why the rented GPU is
not an open endpoint today.

### The key: squall supplies its own, per run

`core/backends/vastai/compute.py:132` builds the container's `authorized_keys` from
**`[run.run_spec.ssh_key_pub, project_ssh_public_key]`** — BOTH. Squall builds the run spec,
so it can put its OWN public key there and hold the private half itself. Confirmed present on
the live run (`run_spec.ssh_key_pub`, currently filled by dstack with the admin user's key
because squall sends none).

This is strictly better than reusing dstack's `project.ssh_private_key`
(`server/services/ssh.py:49`), which is one long-lived key for the whole project stored
unencrypted in its DB (D47). Generate a keypair per wake, send the public half in the Apply,
keep the private half in memory: no long-lived credential, and nothing secret is reusable once
the run ends. Satisfies §12.4 nearly for free.

### Measured — same prompt, same max_tokens (200), same concurrency, minutes apart

| concurrency | via dstack server | direct over SSH |
|---|---|---|
| 32 | 32/32 ok, 746.1 tok/s, wall 8.6s | 32/32 ok, **1009.8 tok/s**, wall 6.3s |
| 128 | **97/128 ok, 31x HTTP 500**, 407.5 tok/s, wall 47.6s | **128/128 ok, 0 failed**, **1856.6 tok/s**, wall 13.8s |
| 256 | not attempted | **256/256 ok, 0 failed, 2106.2 tok/s** |

4.6x the throughput at 128 concurrent, and it stops failing. The dstack path's 31 failures are
the connection budget: `pg_stat_activity` hit 81 = poolSize 40 + maxOverflow 40 + 1 during
that exact run.

### The head-of-line-blocking worry was unfounded

SSH multiplexes every stream over one TCP connection, so the design's main risk was trading a
database ceiling for a transport ceiling. Measured directly, 128 concurrent:

- 1 SSH connection : 1856.6 tok/s
- 8 SSH connections: 1876.2 tok/s

**1% apart.** A single connection is enough; a pool is not needed at these levels. Throughput
kept climbing with concurrency (1009 -> 1307 -> 1857 -> 2106 tok/s at 32/64/128/256) with KV
cache at 13.7%, so the remaining limit is the engine, not the transport.

### Cost: nothing

No LoadBalancer, no VM, no public endpoint, no inbound port. squall-proxy dials OUT. The
confusion worth naming: **dstack's** "gateway" IS a rented VM with a public IP, and that is
what makes the word expensive — it is not what this is.

In Go the whole transport is:

```go
transport := &http.Transport{
    DialContext: func(context.Context, string, string) (net.Conn, error) {
        return sshClient.Dial("tcp", "localhost:8000")
    },
}
```

No `ssh` binary, no subprocess, no Unix socket. `golang.org/x/crypto` v0.24.0 is already in
the module graph.

### Open before this ships

- **Host key pinning.** The prototype used `ssh.InsecureIgnoreHostKey()`. An unpinned
  marketplace host is exactly the MITM surface §12.3 warns about; production MUST pin.
- **Non-Vast topologies.** Kubernetes routes through a jump pod and `dockerized: true`
  backends need two hops — `get_container_ssh_credentials` returns an ordered list for this
  reason. Vast (one hop) works today; the general case is more work.
- **Lifecycle**: reconnect, teardown on sleep, and replica selection once `replicas > 1` —
  jobs dstack currently does for us.
- **Key rotation needs a re-provision**, since `authorized_keys` is written at container
  creation.
- **squall-proxy becomes the only auth point** — it has none today, which is why the live
  endpoint is LAN-only. This must land WITH the tunnel, not after it.

Prototype: `.gocache/sshprobe` (gitignored, throwaway). The project key used to reach the
already-running replica was extracted from Postgres for the test and shredded immediately
after; production uses the per-run key above and never needs it.

## Backlog idea (owner, 2026-08-31): a `nodeSelector` on the Kubernetes backend, KubeAI-style

Today squall's Kubernetes backend schedules onto whatever the target cluster already has. The
idea is to let a `Model` carry a **`nodeSelector`** (and, by extension, tolerations) that
squall passes through to the dstack Kubernetes backend, the way
[KubeAI](https://github.com/substratusai/kubeai) does — so that a cluster running **Karpenter**
(or Cluster Autoscaler) sees an unschedulable GPU Pod and provisions a GPU node on demand.

Why it is attractive: it makes "external GPU capacity" work on a plain EKS/GKE cluster with no
marketplace involved, reusing the exact same `Model` CR and the same 0↔1 flip. The wake path
becomes "Pod pending → Karpenter provisions → Pod runs" instead of "dstack rents a Vast
machine", and squall does not have to care which.

What needs deciding before it is planned:

- **Where the field lives.** `spec.placement` already carries `backends`, `regions` and
  `maxPricePerHour`, and a `nodeSelector` is placement — but it is meaningful for exactly one
  backend, and every other backend must ignore it rather than fail.
- **Whether `provisioningTimeout` is still the right bound.** Karpenter's provision-plus-boot
  is minutes, and a Pod that stays Pending because no instance type matches must surface as
  `Schedulable=False` with a real reason (D58/D67's territory), not as a silent wait.
- **Cost ceiling.** `maxPricePerHour` is enforced by dstack against marketplace offers. There
  is no equivalent for a Karpenter NodePool, so the money guardrail this project takes
  seriously would be delegated to the cluster's own NodePool limits. That is a deliberate
  weakening and needs to be stated, not discovered.

Not scheduled. Recorded so it is not re-derived.
