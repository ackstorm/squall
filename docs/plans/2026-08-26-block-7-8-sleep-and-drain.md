# Block 7+8 — Sleep path and drain-first teardown

**Status:** ready to implement. Decisions below are TAKEN; do not re-litigate them.
**Spec of record:** `docs/specs/squall-spec-v0_18-RC.md` §6, §5.2,
§7, §4 rows F17/F20/F21. The spec wins over this document wherever they disagree.

This is the highest-risk work in the project. A wrong sleep terminates a live generation; a
wrong teardown loses in-flight requests.

## The governing invariants

> **Wake may tolerate uncertainty; sleep must not.**
> **`0→1` fails open, `1→0` fails safe.**

Phase 6 built the fail-open side. This block is the fail-safe side and is far stricter.
**None of the wake path's latitude carries over.**

---

## 1. Sleep decision rule (§6)

Sleep fires only when **all** replicas report `inFlight == 0` **and** the newest
`lastRequestAt` is older than `scaleDownDelaySeconds`. Any replica unreachable, stale, or
ambiguous → **stay awake**.

Extend `Observed`, not `Decide`'s signature — activity evidence is I/O the reconciler performs
*before* calling `Decide`, so `Decide` stays pure:

```go
type Observed struct {
    Run      *dstack.Run
    Ready    bool
    Activity *ActivityEvidence // nil = "not evaluated"; MUST NOT be read as idle
}

type ActivityEvidence struct {
    Complete            bool      // every listed replica answered fresh, this pass
    AllIdle             bool      // meaningful only if Complete
    NewestLastRequestAt time.Time // meaningful only if Complete && AllIdle
}
```

`Action` gains a `Replicas int` field. Today `model_controller.go` hardcodes `Replicas: 1`, so
a wake and a sleep Apply are indistinguishable at the call site — fix that first.

### The three words an implementer will quietly weaken

- **Fresh** — obtained by an HTTP round trip issued and returned *inside the current
  reconcile's* pass. No cache, no TTL to tune. A report carried over from a previous
  `Reconcile` is stale by definition. Full stop.
- **Complete** — the set of addresses that answered fresh this pass is *exactly* the set the
  proxy Service's Endpoints returned in that same pass. An error listing Endpoints is
  incompleteness (an unreadable API server must never degrade to "zero addresses observed,
  vacuously complete"). A timeout, refusal, non-200 or unparseable body from any individual
  address is incompleteness. Both → `Complete: false`.
- **Ambiguous** — a valid, on-time response that still cannot serve as evidence: no key for
  this Model in the payload (never default-substitute `InFlight: 0`), a negative in-flight
  count, a `lastRequestAt` in the future, or a shape that doesn't match the contract.
  Ambiguous folds into `Complete: false`. **There is no fourth outcome and no "assume idle"
  branch anywhere in this code.**

`hasDemand` does not gate the sleep branch — the aggregation is the sole signal.

---

## 2. Demand is self-expiring — DECIDED

`hasDemand` reads the **value** of `DemandAnnotation`, not its presence:

```go
demandSince, ok := parse(m.Annotations[DemandAnnotation])
hasDemand := ok && now.Sub(demandSince) < demandTTL   // default: scaleDownDelaySeconds
```

Phase 6 shipped a presence check and nothing clears the annotation, so demand was true
forever and a slept model would be re-woken on the very next reconcile — **scale-to-zero could
never work.** Rationale for self-expiry over controller-clears (a lost-wake race) is in
`docs/references/decisions-and-open-items.md`. Phase 9's proxy must **refresh** while demand
persists, not write once.

---

## 3. Drain-first teardown (§5.2)

Sequence: *deregister from discovery → stop accepting → bounded in-flight drain
(`drainTimeout`) → delete runs → release fleet → remove finalizer.*

Steps 1+2 collapse into **one atomic action**: write `status.phase = Draining` the instant a
`DeletionTimestamp` is observed with the finalizer still present. Squall never calls the
LiteLLM API (AC18) and discovery is pull-based and fail-closed, so there is no "deregister"
RPC — the status write *is* the deregistration. *(Invented resolution; the constraint it must
satisfy: no new API call, no new CRD field, observable by both the proxy's routing table and
its `/v1/models` listing from the one `status.phase` the controller already owns.)*

Drain reuses §1's evidence primitive **unchanged in its fail-safe semantics**, but with a
different exit condition. This asymmetry is load-bearing:

| Evidence | Sleep (§6) | Drain (§5.2) |
|---|---|---|
| incomplete / ambiguous | wait — **no deadline, ever** | wait, bounded by `drainTimeout` |
| complete, not idle | wait | wait, same bound |
| complete, idle | flip to 0 | proceed to `Delete` |
| deadline reached, evidence unclean | *(no deadline exists)* | **proceed to `Delete` anyway** |

**Sleep can wait forever; teardown cannot.** A finalizer that never releases is its own
operational incident — a stuck object blocking namespace deletion. AC6 guarantees no
mid-stream cut *within* `drainTimeout`, not "never". Past the deadline, `Delete` runs against
incomplete evidence. Make this a conscious, tested branch (T13), not an accident of the drain
loop looking like the sleep loop.

**Deadline anchor:** `now.Sub(model.DeletionTimestamp.Time) > spec.DrainTimeout`. Set once,
immutably, by the API server; survives a controller restart. No new status field, no race
between "deletion started" and "controller noticed".

**Ordering and idempotence:** add finalizer on non-deleting CRs (does not exist today) → on
deletion, write `Draining` (idempotent) → re-gather evidence every reconcile (fresh, never
accumulated) → `dstack.Delete` once evidence is clean **or** the deadline passed → remove
finalizer. `Delete` returning `ErrNotFound` is **success** — the client returns it precisely so
replay is unambiguous. A transport error is **not** success; fail toward "still exists,
retry", symmetric with sleep's "unreachable ≠ absent".

**Crash mid-teardown:** replay re-enters at the `Draining` step; `dstack.Get` now returns
`ErrNotFound`, so there is no run left to protect and drain is moot — proceed to finalizer
removal. No double-delete, no re-run of the drain wait. Correct behaviour falls out with zero
extra code.

---

## 4. F21 — the fleet is dstack's, not ours

Flipping to 0 terminates the **job**; the **instance** is released only by fleet
`idle_duration`. The gap between `scaleDownDelaySeconds` and `fleet.idleDuration` is the
**warm pool** — a wake inside that window skips provisioning and image pull.

The controller **must not**: call anything to release the instance (no such method exists on
the frozen `dstack.Client`, deliberately); track instance-up state to sleep faster; or add a
second idle timer shadowing the fleet's. The sleep path's *only* actuation is
`Apply{Replicas: 0}`.

Observable state after a sleep flip: `Run != nil, Replicas == 0` — this is `Asleep`, distinct
from `Dead` (F20).

**Carried forward unresolved:** §6 flags that idle release is documented "only for VM-based
backends" — confirm on Vast's container instances and on DO. That is PoC 4, not closable here;
the fake models one instance per service with no backend distinction. Report it as still open.

---

## 5. Tasks

Pure-unit tasks must run in `test-unit` with **no control plane**.

**7.0 — Activity wire contract.** The per-model `{inFlight, lastRequestAt}` shape the proxy
serves and the controller decodes. Must live where a controller-runtime-free `squall-proxy`
can import it cheaply — same rationale that moved `DemandAnnotation` into
`api/squall/v1alpha1`. Decoder must distinguish "no data for this model" from "0 in-flight".
*Invented; the spec names no wire shape.* §6 says one endpoint per replica returning all
models, not one call per (replica, model).

**7.1 — Idle aggregation.** Pure: the aggregation function (expected addresses + per-address
results → `ActivityEvidence`). Envtest: the Endpoints enumeration, with `httptest` servers
standing in for proxy replicas. Most mutation-tested cases belong in the pure half.

**7.2 — Endpoints churn.** Envtest. Add/remove replicas mid-evaluation; no premature sleep, no
sleep on a stale snapshot.

**7.3 — Pinned models never sleep (AC17).** Pin gate is unit-testable; the "toggle without
recreate" half needs envtest to prove `RunID`/`DeploymentNum` are unchanged.

**8.0 — Finalizer add path.** Does not exist today. Envtest.

**8.1 — Finalizer ordering.** Port the *test shapes* (not the LiteLLM content) from
`../alitellm-operator/internal/controller/litellmconnection_finalizer_test.go`. Ours must
observably pass through `Draining` and make exactly one replay-idempotent `dstack.Delete`.

**8.2 — Fail-closed on an unreadable API server.** Use
`sigs.k8s.io/controller-runtime/pkg/client/interceptor` to inject `List`/`Get` errors rather
than tearing down the shared `testEnv`. Assert zero destructive calls across N reconciles.

**8.3 — `provisioningTimeout` is the only destructive trigger.** Blocked on OQ3 below; scope it
only after the anchor is decided.

---

## 6. Tests, each with the mutation that must catch it

A test that stays green under its stated mutation is a finding, not coverage.

| # | Test | Mutation that must turn it red |
|---|---|---|
| T1 | All idle past `scaleDownDelaySeconds` → flip | Drop the time comparison; fire on `AllIdle` alone |
| T2 | A in-flight, B idle → no flip; release A → flip | Change aggregation from AND to OR |
| T3 | **B unreachable, A idle → no flip** (canonical) | Treat a query error as `InFlight: 0` instead of `Complete: false` |
| T4 | Endpoints List errors → no flip | On List error, fall through to "zero addresses = vacuously complete" |
| T5 | New replica listed but not yet queried → no flip | Compute `Complete` from `len(reports)` instead of cross-checking the listed set |
| T6 | Replica removed mid-evaluation → re-evaluate fresh | Cache Endpoints or reports across reconciles |
| T7 | `minReplicas: 1` survives a clean idle window | Delete the `spec.MinReplicas == 0` gate |
| T8 | Pin toggle doesn't change `RunID`/`DeploymentNum` | Route the toggle through the mint-fresh-run branch |
| T9 | Sleep flip leaves `InstanceCount == 1`; only `Tick()` past `fleetIdleDuration` clears it | Add any fleet-scoped call to the sleep path |
| T10 | Finalizer added on first reconcile | Skip `AddFinalizer` — delete would complete un-drained |
| T11 | `Draining` observable **before** `dstack.Delete` | Skip the phase write, call `Delete` directly |
| T12 | In-flight request never cut inside `drainTimeout` | Ignore drain evidence, `Delete` on entering `Draining` |
| T13 | `drainTimeout` elapses, evidence unclean → `Delete` proceeds | Make the drain wait indefinitely (copy sleep's semantics) |
| T14 | Injected `List`/`Get` errors → zero destructive calls | Fall through to "nothing observed, safe to proceed" |
| T15 | Replay after `Delete` succeeded → `ErrNotFound` treated as success | Treat `ErrNotFound` as a hard error (permanently stuck finalizer) |
| T16 | `provisioningTimeout` destroys with **no drain** (AC15) | Route it through the drain-evidence gate |
| T17 | Ready run past `maxLifetime` is **alarmed, not destroyed** | Wire `maxLifetime` into the destructive branch |
| T18 | `provisioningTimeout` resets on a genuine Dead→Recreate | Reuse the anchor across the recreate without resetting it |

`FakeClock`/`Tick()` is the right tool for T1's boundary, T9, T12/T13, and T18.
Note on T18 if a `metav1.Condition` is used as the anchor: `apimeta.SetStatusCondition` only
bumps `LastTransitionTime` on a `Status` flip, so it will **not** reset across a same-`Status`
recreate unless forced. That is the exact footgun T18 exists to catch.

---

## 7. Known hazard inherited from Phase 6

An Apply that sets `Apply: true` while leaving `BaseDeploymentNum` at zero against a
known-live run **permanently wedges** the model: every reconcile fails the CAS before reaching
`Status().Update`, so the phase never advances and `ApplyCount` is mathematically capped at 1
(it can never storm). Nothing backs off, alarms, or distinguishes "CAS conflict from a
legitimate concurrent writer" from "our own request is malformed forever".

Phase 6's AC4 sampler detects this only within a 3s window in one test. Since this block adds
a **second** Apply call site (the sleep flip, `Replicas: 0`), consider whether `Decide` or
`Reconcile` should treat a zero `BaseDeploymentNum` against a live `observed.Run` as a
programmer error rather than letting it degrade into an unbounded conflict loop.

---

## 8. Open questions — do NOT invent spec text over these

- **OQ2 — "release fleet" has no method** on the frozen `dstack.Client`, and the wire shape is
  unvalidated. **Recommended:** pass-through for v0.1 — `Delete` the run, remove the
  finalizer, let `fleet.idleDuration` reclaim the machine (bounded, since the CRD requires the
  field). Satisfies AC6's literal text but not §5.2's prose exactly. Needs a human/PoC-0 call.
  Do **not** invent a `DeleteFleet` call against an unvalidated surface.
- **OQ3 — `provisioningTimeout`/`maxLifetime` have no "since-when" anchor.** Neither
  `dstack.Run` nor `ModelStatus` carries one. **Recommended:** a controller-owned
  `status.runStartedAt`, written when a genuinely new run is minted and **explicitly reset**
  on every subsequent mint. Invented relative to §5.1; needs sign-off. Blocks task 8.3.
- **OQ4 — residual TOCTOU** between evidence-gathering and `Apply{Replicas: 0}`: a request
  accepted but not yet counted is invisible. **Recommended:** the proxy increments `InFlight`
  at accept time, before any upstream call. Minimises but does not eliminate the window.
  Record as an accepted residual risk; do not comment it away as "closed".
- **OQ5 — nothing names which Service's Endpoints to enumerate.** There is one proxy
  Deployment per cluster, not one per Model. **Recommended:** a `ModelReconciler` field set at
  startup, not a CRD field. Naming waits on the Helm skeleton.
