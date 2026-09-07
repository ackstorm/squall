# Design — `spec.mode` and the `spec.onDemand` block

**Status:** **SUPERSEDED — not implemented, do not implement.** Rejected by the owner on
2026-09-07 as over-engineering. Kept for the two measurements in it, which are load-bearing
and were expensive to obtain — see "What survives" below.
**Date:** 2026-09-06
**Superseded by:** the flat `minReplicas` shape that already exists, plus mode-scoping
sentences in the `idleTimeout` / `uncontrolledTimeout` / `hardStop` doc comments
(`api/squall/v1alpha1/model_types.go`), which reach an operator through
`kubectl explain model.spec.<field>`.
**Breaking:** yes — that was the point, and the reason it was dropped.
**Depends on:** the `freshSuccess` fallback, which lands first and separately (see "Prerequisite").

## Why this was rejected

Three reasons, in descending weight.

**1. The proposed block would have been a false statement about the code.**
`spec.onDemand.idleTimeout` asserts the field applies only on demand. It does not:
`freshSuccess` (`model_controller.go`) reads `spec.IdleTimeout` **ungated** by
`minReplicas` as the staleness bound on readiness evidence (b), and `squall-proxy` uses it
as the demand-refresh cadence in both modes. Only the *sleep flip* is on-demand-only. This
design already conceded the point by requiring a `freshSuccess` fallback as a prerequisite
— that is, extra work whose sole purpose is to make the new shape honest. The flat field is
honest today.

**2. The thing being made structural is five lines.** Every `minReplicas` gate in the
non-test tree: `wantAwake` (`phase.go`), `sleepDue`, `unhealthyDue`, `uncontrolledDue`
(same file, adjacent), and the `hardStop` assignment in `model_controller.go`. One
subsystem, effectively one screen. Not a concept that needs a schema to express.

**3. KubeAI — the closest comparable project, with the same 0-replica problem — kept a
flat `minReplicas: 0`** and introduced no such block. The `2026-09-06-idle-timeout-floor`
design had already recorded this under "Explicitly not doing"; this design then did it
anyway without new evidence. That is the actual process error, and it is the reason this
banner exists rather than a quiet delete.

Against those: a breaking API change across two binaries, eight in-tree `Model` manifests,
a CEL tombstone, a two-half migration guard, and the `internal/proxy/cache.go` unstructured
read of `spec.idleTimeout` whose absence resolves to `0` **silently** — no compiler, no
failing test. The highest-risk edit in the whole change would have bought zero behaviour.

The narrow real complaint — that `minReplicas: 1` plus an explicit `idleTimeout: 10m`
tells the operator nothing — is answered where operators look, in the field docs and
therefore in `kubectl explain`. Note that `ValidateWithWarnings` warnings would NOT have
answered it: they go to `logger.Info` only (`model_controller.go`), never to a condition or
an event, so they are invisible to `kubectl describe`.

## What survives

Two measured results, both still true and neither recorded anywhere else:

- **Ledger D70's "CEL is impossible here" is node-local, not spec-wide.** A *spec-level*
  CEL rule installs fine on Kubernetes 1.31 with the untyped
  `placement.maxPricePerHour` node as a descendant. D70 remains correct about that node;
  it does not generalize. Anyone reading `price.go`'s "do not re-add a CEL marker without
  re-reading that entry" should know the finding's real scope.
- **A schema tombstone works, and needs both halves.** CRD pruning runs *before* CEL
  validation, so a removed field must stay declared in the Go struct or the rule never
  fires; and stored objects are never re-admitted, so admission alone cannot protect a
  migration — the reconcile path has to refuse them too. Verified against a live API
  server: the tombstone rejects with our own message.

Everything below this line is the original proposal, retained unchanged for that evidence.
It is not a plan of record.

---

## Goal

Make the on-demand/always-on distinction structural instead of documented. Today
`spec.minReplicas: 0|1` is a replica count that is not a count, and three fields are
silently inert when it is `1`. After this change the settings that only apply to a Model
that sleeps live inside a block that only exists when it sleeps.

## The shape

```yaml
spec:
  mode: OnDemand              # required, enum OnDemand|AlwaysOn, no default
  onDemand:                   # required iff mode: OnDemand, forbidden otherwise
    idleTimeout: 10m          # default 5m, floor 1m (MinIdleTimeout)
    hardStop: 24h             # default 24h
    uncontrolledTimeout: 55m  # optional; derived when absent
  holdTimeout: 10m            # stays at spec level
  provisioningTimeout: 20m    # stays
  drainTimeout: 120s          # stays
  maxLifetime: 4h             # stays
---
spec:
  mode: AlwaysOn              # no companion block — see "No alwaysOn block, yet"
  holdTimeout: 10m
  provisioningTimeout: 20m
  drainTimeout: 120s
  maxLifetime: 4h
```

This is Kubernetes' own discriminated-union idiom, not an invention: `apps/v1`
Deployment pairs `strategy.type: RollingUpdate` with `strategy.rollingUpdate`, and
`autoscaling/v2` HPA pairs `metrics[].type: Resource` with `metrics[].resource`. The
enum value is `OnDemand`; the field that pairs with it is `onDemand`, lowerCamelCase, as
in both precedents.

`mode` carries **no default**, preserving the one good property of `minReplicas`: the
operator must state intent. Omitting it is rejected, never guessed. Guessing would have
to guess in some direction, and both directions are wrong — defaulting to `AlwaysOn`
bills a GPU nobody asked for, defaulting to `OnDemand` sleeps a Model somebody is
relying on.

## Which fields move, and why exactly these

Membership was decided by reading the gates, not by what groups nicely. Only three
fields are genuinely unreachable when a Model is pinned:

| Field | Gate that makes it on-demand-only |
|---|---|
| `idleTimeout` | `phase.go:260` `sleepDue`, `:273` `unhealthyDue`, `:283` via `uncontrolledTimeoutFor` |
| `hardStop` | `model_controller.go:829` — `if model.Spec.MinReplicas == 0 { hard = ... }` |
| `uncontrolledTimeout` | `phase.go:283` — same gate |

Set `hardStop: 4h` with `minReplicas: 1` today and it does nothing, with no indication.
After this change it cannot be written at all.

Everything else stays at spec level because it applies to both modes:

- **`holdTimeout`** — a pinned Model still cold-starts at first boot, and again after a
  crash. The proxy holds those requests exactly as it does for an on-demand wake.
- **`provisioningTimeout`** — `phase.go:246`, ungated.
- **`drainTimeout`** — the finalizer, which runs for both.
- **`maxLifetime`** — its own doc comment says it "stays meaningful even under
  MinReplicas: 1, because that is precisely the safety net for a pinned GPU everyone
  forgot about."

## No `alwaysOn` block, yet — and that asymmetry is deliberate

`AlwaysOn` has no settings of its own today, so it gets no block. Deployment does the
same: `type: Recreate` has no `recreate:` block.

The obvious future occupant is a pinned replica count greater than one. That is a
**feature, not a field**: `Action.Replicas` takes exactly two values across all thirteen
sites in `phase.go` (five `0`, eight `1`), and squall uses dstack's `ManualScaler` with a
fixed count precisely because F15 makes ranges and fixed counts mutually exclusive.
Supporting N would touch the apply path, the proxy's routing, and how §6 aggregates
activity across run replicas.

Adding `alwaysOn:` later is purely additive — one more CEL pairing rule, no migration —
so nothing is lost by reserving it. When it arrives, **the field is `replicas`, not
`minReplicas`**: inside `alwaysOn` there is no minimum, it is a fixed count, and reusing
the name this release tombstones would mean two different things one level apart.

## Validation: three CEL rules, and a measured reason to trust them

```
self.mode != 'OnDemand' || has(self.onDemand)     # OnDemand requires the block
self.mode != 'AlwaysOn' || !has(self.onDemand)    # AlwaysOn forbids it
!has(self.minReplicas)                            # the tombstone
```

`onDemand` is deliberately **not** given `+kubebuilder:default={}`. Defaulting runs
before validation, so a default would materialise the block on `AlwaysOn` Models and make
rule 2 unsatisfiable. An operator writes `onDemand: {}` at minimum and the inner fields
default from there.

**CEL at spec level works here — verified, because the codebase says otherwise.**
`api/squall/v1alpha1/price.go:40` records D70: an `XValidation` rule on
`spec.placement.maxPricePerHour` makes the apiserver refuse to install the CRD at all,
"even `rule: \"true\"` fails the same way", because that node is `type: ""` +
`PreserveUnknownFields` and has no concrete type for the CEL type environment. That
comment ends "Do not re-add a CEL marker here without re-reading that entry first."

Measured 2026-09-06 against envtest (Kubernetes 1.31): **D70's result is node-local.** A
rule on `spec` installs cleanly even though the poisoned node is one of its descendants,
and the tombstone rule rejects at admission with its own message:

```
Model.squall.ackstorm.ai "qwen3-8-27b" is invalid: spec: Invalid value: "object":
spec.minReplicas has been replaced by spec.onDemand
```

Both halves were exercised — install, and reject — before this design was written.

## The migration guard needs both halves

**Half one: `minReplicas` stays declared in the schema**, as an optional deprecated
`*int32` the controller never reads. This is not tidiness. A v1 CRD **prunes** undeclared
fields, and pruning happens *before* CEL validation — so a field removed from the Go
struct is gone before any rule can see it, and the tombstone would silently never fire. A
Model still carrying `minReplicas: 0` would be accepted, read as having no `mode`, and
the operator would be told nothing.

**Half two: `ValidateWithWarnings` rejects an unmigrated spec** — an empty `mode`, or a
non-nil `minReplicas`. Half one only fires on writes. Stored objects are never
re-admitted, and a `Get` returns the stored bytes, so an unmigrated Model reaches the
controller intact and must be refused there.

That call site (`model_controller.go:890`) already sets `Schedulable=False` with
`ReasonInvalidSpec` and `action.Apply = false`. So the worst case of a botched migration
is **a dead Model with a readable reason, never a billing one** — which is the direction
this project's invariants require: a wrong wake costs money, and a Model wrongly read as
`AlwaysOn` is a wrong wake that never ends.

## Prerequisite, landing first and separately

`freshSuccess` (`model_controller.go:1037`) reads `spec.IdleTimeout` on the **pinned**
path, ungated by `MinReplicas`. Moving the field would zero its window and kill readiness
evidence (b) for every `AlwaysOn` Model. The fix is a fallback, not a new tunable:

```go
window := spec.IdleTimeout.Duration   // spec.OnDemand.IdleTimeout after the move
if window <= 0 {
    window = DefaultReadinessWindow   // 5m, matching the CRD default
}
```

5m is chosen so a default `OnDemand` Model behaves bit-identically to today. D167(b) is
satisfied: the window still decays, which is what makes `Observed.Ready` mean "serving
now" rather than "has ever served".

This is non-breaking and lands as its own commit before the API change, so a regression
in either can be attributed.

## Call sites

Non-test, verified by grep:

| File:line | What |
|---|---|
| `phase.go:188` | `wantAwake := spec.MinReplicas == 1 \|\| hasDemand` |
| `phase.go:260`, `:273`, `:283` | the three `spec.MinReplicas == 0` sleep gates |
| `phase.go:377-384` | `uncontrolledTimeoutFor` |
| `model_controller.go:479` | idle requeue gate |
| `model_controller.go:829-830` | `hardStop` → dstack `max_duration` |
| `model_controller.go:991` | `IdleMetrics.Observe` |
| `model_controller.go:1037` | `freshSuccess` — the prerequisite |
| `model_controller.go:1199` | `hasDemand` TTL |
| `model_validation.go:33,41,55,81` | the existing rules |
| **`internal/proxy/cache.go:240`** | **`unstructured.NestedString(u.Object, "spec", "idleTimeout")`** |

The proxy site is the dangerous one and is called out separately below.

## The one failure mode that would be silent

`squall-proxy` is a separate binary that reads the CR **unstructured**, by JSON path:

```go
idleTimeout, _, _ := unstructured.NestedString(u.Object, "spec", "idleTimeout")
idle, err := time.ParseDuration(idleTimeout)
if err != nil {
    idle = 0   // "Unparseable or absent resolves to zero"
}
```

There is no compiler to catch this rename. If the path is not updated to
`"spec", "onDemand", "idleTimeout"`, every Model's proxy-side `IdleTimeout` silently
becomes `0`, `refreshIntervalFor` falls back to the proxy-wide ceiling, and a held
request refreshes its demand anchor too slowly and ages itself out — LIVE-3, the bug that
comment was written for. No error, no log line, and the unit tests pass because they
construct `ModelSnapshot` directly.

The e2e is the only gate that would catch it, so the plan must assert the proxy's
observed `IdleTimeout` explicitly rather than relying on the suite going green.

## Testing

**CEL rules are CRD-level, so they need envtest, not unit tests.** Six states, each
asserted for accept/reject and for the message an operator would read:

| State | Expected |
|---|---|
| `mode: OnDemand` + `onDemand: {}` | accepted, inner defaults materialise |
| `mode: OnDemand`, no block | rejected, rule 1 |
| `mode: AlwaysOn`, no block | accepted |
| `mode: AlwaysOn` + `onDemand: {}` | rejected, rule 2 |
| any `mode` + `minReplicas: 0` | rejected, tombstone |
| no `mode` | rejected, required field |

Guard two gets unit tests in `model_validation_test.go`: an empty `mode` and a non-nil
`minReplicas` each produce an error naming the replacement.

**The mutation that matters:** delete one CEL rule and its state must move from rejected
to accepted. A rule that never fired is worse than no rule, because it reads as
protection. This project has found two vacuous tests in the last day — one in the e2e,
one in a floor test written in this same session — so the sweep is not optional.

## Operator migration

Every Model must be rewritten. There are **eight** in-tree, all `minReplicas: 0`:

- `config/samples/squall_v1alpha1_model.yaml:223`, `_glm53_flash.yaml:104`, `_qwen3_8b.yaml:89`
- `docs/runbooks/ollama-tiny.yaml:25`, `battery-k8s.yaml:57`, `qwen3-8-27b.yaml:17`
- `test/e2e/cluster/03-fixtures/model.yaml:58`, and `loopModelYAML` in `test/e2e/e2e_test.go:83`

plus the unstructured fixture inside
`internal/controller/squall/model_sample_envtest_test.go:138`, which is deliberately
written as raw YAML so the typed client cannot paper over a schema change — it will need
the same rewrite, and it is the one most likely to be missed.

```diff
-  minReplicas: 0
-  idleTimeout: 10m
-  hardStop: 24h
+  mode: OnDemand
+  onDemand:
+    idleTimeout: 10m
+    hardStop: 24h
```

CRD first, as always — Helm never upgrades CRDs from `crds/` (D160), and Flux's
`spec.upgrade.crds` defaults to `Skip`. Applying the new CRD before rewriting the Models
is what makes the tombstone reject them loudly instead of the controller misreading them.

## Out of scope

- **Pinned replicas > 1.** Reserved for `alwaysOn.replicas`; a feature, not a field.
- **Decoupling the demand TTL from `idleTimeout`.** `sleepDue` and `hasDemand` anchor on
  the same instant with the same window and expire together by construction; lengthening
  one alone produces a sleep/wake oscillation. Unchanged by this design — inside the
  block, `idleTimeout` still serves both.
- **D167(b)**, the two stale `phase.go` comments. Should be fixed, but not inside a
  breaking API change.
- **D173**, the unverified provisioning-backoff defect. Independent.

## Open items

- **D173** remains unverified and untouched by this work.
- **The `alwaysOn` block's first field** is unspecified by design. It arrives with the
  feature that needs it, not before.
