# RFC — the per-model shadow proxy, and how D28 got us here

**Status:** request for an independent architectural assessment. Nothing here is decided.
**Date:** 2026-08-27

---

## 1. The problem: D28

Squall v0.1 is feature-complete against its plan — 12 phases, all gates green, 6/6 e2e specs
passing on a real kind cluster. It can provision a GPU through dstack, wake it on demand,
journal the run, sleep it when idle, and tear it down drain-first.

**It cannot serve a single request.**

```go
// internal/proxy/decision.go — the ONLY forward
case squallv1alpha1.ModelPhaseReady:
    return Action{Forward: true}
```

```go
// internal/controller/squall/model_controller.go — observe()
observed := Observed{Run: run}
if run.Replicas > 0 {
    observed.Activity = r.gatherActivity(ctx, name, now)
}
return observed, nil          // Ready is NEVER assigned. Nothing, anywhere, sets it.
```

Verified by grep across all non-test code: **no writer for `Observed.Ready` exists.** So
`ModelPhaseReady` is unreachable, so the proxy never forwards. Every request blocks for
`holdTimeout` and then answers 503 — against a GPU that is up, healthy, and billing.

Observed live on the cluster:

```
POST /v1/chat/completions {"model":"e2e-fixture-model"}
  -> blocked 8s, zero bytes on the wire
  -> HTTP 503 {"error":"model not ready","state":"waking"}
  -> status.runId = run-e2e-fixture-model-8     (the wake DID fire)
  -> status.phase = Asleep                      (never passed through Ready)
```

## 2. Why the gap exists — it is by design, and the design is incomplete

The spec is explicit that a running container is not a ready model (§6):

> **`Ready` has one definition and one writer.** The template's entrypoint orders the
> evidence — start engine → engine health endpoint succeeds → warmup request succeeds → only
> then open the dstack readiness gate — and the controller, sole writer of `status`, sets
> `phase: Ready` from dstack's replica-ready state. "dstack job running" is never `Ready`;
> no probe by the proxy, no separate health subsystem.

The reasoning is sound. On Vast the model image *is* the instance (F34); the container is
"running" long before a 27B model finishes loading into VRAM. Forwarding at "running" yields
connection-refused or 500s, and LiteLLM then routes around the model as unhealthy.

So the intended chain is:

```
[container]  start engine -> health endpoint OK -> warmup OK -> open dstack readiness gate
[dstack]     replica: running ------------------------------> replica: READY
[squall]     controller reads dstack replica-ready state -----> phase: Ready
```

**Two pieces are missing, in different layers:**

- **(A) squall-side.** `dstack.Run` carries `{Name, RunID, DeploymentNum, Replicas}` — a
  *count of requested replicas* and no readiness field at all. The controller has nothing to
  read even if the engine were warm. The client is currently marked FROZEN.
- **(B) container-side.** The §8 engine templates (entrypoint ordering: health → warmup →
  open the gate) were explicitly out of v0.1 scope and do not exist.

## 3. Two adjacent gaps that interact badly

- **`provisioningTimeout` is unimplemented** (task 8.3, blocked on ledger D7: neither
  `dstack.Run` nor `ModelStatus` carries a "since when" anchor, so nothing knows a run's age).
  AC15 requires: *a run that never reaches ready is destroyed at `provisioningTimeout` with an
  alarm; a healthy run older than `maxLifetime` is alerted, NOT destroyed.*
- **The cross-field validation layer is dead code** (ledger D39). `Validate` /
  `ValidateWithWarnings` enforce §5.1's rules — including the deadline ordering
  `holdTimeout <= provisioningTimeout` — are fully unit-tested, and have **zero callers**. No
  webhook, no CEL rules on the CRD. A violating CR is accepted silently.

**The interaction is the dangerous part.** Because nothing sets `Ready`, *every* run
"never reaches ready". Today that means a stuck run bills forever, silently, with no alarm.
If 8.3 were implemented while D28 stands, we would get the opposite failure: **every model
destroyed at `provisioningTimeout`, forever, in a recreate loop.** They must be fixed in
order — readiness first, then the deadline.

## 4. The proposal under evaluation: a per-model shadow proxy

Instead of the controller reading a readiness flag off the dstack API, the controller creates
**one small in-cluster Deployment per awake Model** — a "shadow" that represents the remote
dstack container locally.

Claimed benefits, in the order they were raised:

1. **Readiness via a Kubernetes primitive.** The shadow's `readinessProbe` determines
   readiness; the controller reads `status.readyReplicas`. The kubelet does the probing —
   no health subsystem is written by us.
2. **`provisioningTimeout` for free.** `Deployment.spec.progressDeadlineSeconds` *is* the
   provisioning deadline; `Progressing=False/ProgressDeadlineExceeded` is the expiry signal.
   This dissolves ledger D7 (no age anchor needed — Kubernetes keeps it).
3. **Operational visibility.** `kubectl get pods` answers "which models do I have running on
   dstack right now" at a glance, which nothing answers today.
4. **Request-level telemetry without the push pipeline.** If the shadow sits *in the data
   path*, it meters every call locally: tokens, tokens/s, requests, latency/TTFT, cache hits
   — read from the OpenAI `usage` block, which §8 states all three engines return through the
   same path. It exposes `/metrics` on a stable in-cluster endpoint that Prometheus scrapes
   normally.

Benefit 4 deserves weight, because §10 currently mandates the opposite:

> **Push only.** Ephemeral instances behind NAT are not scraped. The runtime environment
> ships a telemetry agent/process (e.g. Alloy): runtime metrics + DCGM (GPU) + logs →
> remote_write / Loki into the LGTM stack, with **write-only, tenant-scoped** credentials.

That design requires every engine image to bundle an agent and, on Vast (where the image *is*
the instance, F34), **ships write-only telemetry credentials to a rented marketplace host**.
An in-cluster metering point removes that for request-level metrics entirely. It does NOT
remove it for GPU-level metrics (DCGM: utilisation, VRAM, temperature), which only the host
can produce. It may also reduce §8/PoC 8's per-engine telemetry-parity problem, since
`usage` is uniform where Prometheus surfaces are not.

### The hard constraint this collides with

§10's two-lane rule:

> **Monitoring MUST NOT generate demand.** Nothing synthetic may traverse the gateway data
> path. LiteLLM background health checks are disabled for external models; **blackbox probes
> at the gateway are forbidden**, and the squall-proxy never probes — it reads Model CR
> `status` via informers.

Its stated purpose is AC5: *48h without model traffic → GPU capacity cost = 0. If capacity
cost isn't zero, something synthetic is waking it, and that is a bug.*

A `readinessProbe` that reaches the engine through the gateway is, on the letter, exactly a
forbidden blackbox probe.

The counter-argument, which needs adjudication rather than assertion: the shadow exists
**only while the model is already awake at a real user's request**. It cannot wake anything
(F23: the gateway never wakes a `ManualScaler` service), and it cannot hold anything awake,
because sleep is decided by §6 activity evidence gathered at squall-proxy — which a
gateway-side probe never touches. So the letter forbids it; the intent may not.

### Two variants

| | Shadow probes the engine | Shadow mirrors dstack state |
|---|---|---|
| Readiness source | earned first-hand by a real probe | still from `dstack.Run` (needs a new field) |
| Two-lane rule | violates the letter | clean |
| `progressDeadlineSeconds` | free | free |
| `kubectl get pods` visibility | yes | yes |
| Token/request metrics in-cluster | yes, if in the data path | only if in the data path |

## 5. What the assessment must weigh

Costs and risks that have NOT been analysed and should be:

- **An extra hop in the hot path.** client → LiteLLM → squall-proxy → shadow → gateway → SSH
  → replica. Latency, and a new failure domain on the serving path.
- **A pod per awake model.** Lifecycle, finalizers, orphan reconciliation, resource cost —
  and the failure mode where a shadow outlives or predeceases its run.
- **Two sources of truth.** The shadow's readiness versus the Model's phase, with drift risk.
- **Architectural relocation.** Squall today has ONE `squall-proxy` holding §7's decision
  table and §6's activity endpoint. Per-model shadows would split or duplicate that. The
  spec's §11 "separate failure domains" reasoning needs re-testing against the new shape.
- **Does it actually remove work,** or relocate it? The engine still has to become ready; the
  shadow only changes who observes it.

## 6. The questions

1. Is the shadow proxy the right architecture, or a clever way to avoid adding one field to
   `dstack.Run`? Judge on merit — the frozen client is a project convention, not a law.
2. Which variant, if either? Specifically: does the two-lane rule's *intent* permit a probe
   that runs only during an already-demanded wake window? If yes, say what invariant replaces
   the current bright line, and how AC5 stays testable.
3. Is benefit 4 (in-cluster request metrics, no push agent, no credentials on rented hosts)
   strong enough on its own to justify an in-path shadow, independent of readiness?
4. Given D28 + 8.3 + D39 interact, what is the correct ORDER of fixes, and what is the
   smallest change that makes squall serve a token?
5. What did this analysis get wrong or miss?

Answer with a recommendation, not a survey. Where the spec must change, say which section and
what the new text should assert.
