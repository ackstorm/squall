# Design — an enforced floor under `spec.idleTimeout`

**Status:** proposed, awaiting owner review
**Date:** 2026-09-06
**Closes:** the "still owed" half of ledger **D171**
**Supersedes nothing. Reverses nothing.**

## Goal

Reject a `spec.idleTimeout` too short to work, instead of admitting it and letting the
Model silently never wake.

One constant, one rejection, in the file that already holds this project's other
cross-field rules. Everything else in this document exists to record what was
considered and deliberately *not* done, so it is not reopened.

## The problem

`spec.idleTimeout` is the idle budget **and** the TTL of the demand annotation
(`squall.ackstorm.ai/demand-since`) the proxy writes on the data path:

```go
// model_controller.go:1199
return now.Sub(demandSince) < m.Spec.IdleTimeout.Duration
```

A value that is legal but too short makes an on-demand Model **permanently
unwakeable**. There is no error, no event, and no condition — the operator sees a
correctly-admitted Model that simply does not respond. Today validation rejects only a
literal zero (`model_validation.go:55`), and the cliff sits well above zero.

Measured, D171: with `SQUALL_IDLE_REQUEUE_INTERVAL=2s`, `idleTimeout: 2s` stayed Asleep
indefinitely, while `8s` and `30s` reached Ready about two seconds after the request.

The hazard is live in this repository right now.
`test/e2e/cluster/03-fixtures/model.yaml` declares `minReplicas: 0` with
`idleTimeout: 2s`. The in-test `loopModelYAML` was moved to `8s` during the v0.1.7
release; this second fixture was missed. The rule below catches it.

## The rule

In `internal/controller/squall/phase.go`, beside the constants that already live there:

```go
// MinIdleTimeout is the floor under spec.idleTimeout. It is not a tuning
// preference: idleTimeout is ALSO the demand annotation's TTL, and the
// annotation is stamped at RFC3339 SECOND granularity, so a value below this
// can expire before the controller next evaluates it. The Model then never
// wakes, with no error, no event and no condition (D171).
const MinIdleTimeout = time.Minute
```

In `internal/controller/squall/model_validation.go`, replacing the current
`spec.IdleTimeout.Duration <= 0` branch:

```go
if spec.IdleTimeout.Duration < MinIdleTimeout {
    return nil, fmt.Errorf("idleTimeout (%s) must be at least %s: it is also the "+
        "demand annotation's TTL, stamped at RFC3339 second granularity, so a "+
        "shorter window can expire before the controller next evaluates it and "+
        "the Model then never wakes, with no error and no event",
        spec.IdleTimeout.Duration, MinIdleTimeout)
}
```

A zero is caught by the same comparison. The distinct zero-case message is dropped: its
reasoning ("a zero expires demand the instant the proxy writes it") is a special case of
the sentence above, and two messages for one rule invite them to drift apart.

Shape, message style and placement follow `MinHardStop = time.Hour` (`phase.go:310`) and
its rejection at `model_validation.go:42` exactly. This is deliberately not a new pattern.

## Where it takes effect, and why that is the safe place

`ValidateWithWarnings` has exactly one non-test caller: `model_controller.go:890`, on the
wake path. A rejection there:

- sets `Schedulable=False` with `ReasonInvalidSpec` and the message above,
- sets `action.Apply = false`, so no money is spent on a wake that cannot be honoured,
- preserves the prior phase rather than naming a run that was never applied.

**There is no admission webhook in squall.** This rule therefore cannot refuse a Model at
`kubectl apply` time, cannot fail a Flux `HelmRelease` reconcile, and cannot block a
GitOps sync. An already-deployed Model below the floor stops provisioning and states why
in `status.conditions`, readable with `kubectl describe model`.

It is still a behaviour change, and the spec should say so plainly rather than sell it as
purely diagnostic: **a Model running today with `idleTimeout: 10s` stops waking after
upgrade.** The position taken here is that such a Model could not wake reliably anyway,
and that a visible refusal is strictly better than silence. An operator who disagrees
raises `idleTimeout` — which is the same action the message asks for.

## Why one minute

The exact mechanism behind D171(b) is **not** settled, and this design does not pretend
otherwise. Two readings survive the code:

1. the stamp expires before the controller's next evaluation, or
2. the Model wakes and `sleepDue` immediately re-sleeps it, because `idleTimeout` is
   shorter than the wake takes.

They would imply different rules — a cadence-derived floor versus a cold-start-derived
one. Rather than derive a constant from an unproven mechanism, one minute is chosen to
clear **all three** known contributors at once:

| Contributor | Value | Source |
|---|---|---|
| RFC3339 second truncation | up to 1s | `patcher.go:58`, `at.UTC().Format(time.RFC3339)` |
| Default idle requeue interval | 15s | `model_controller.go:101` |
| Cold start, every real backend | 2m12s measured on Vast.ai; ~10m with weights | D165, D171 |

Below one minute, no configuration behaves sensibly on any of the three counts. The
constant is a floor under a cliff, not a recommendation: the CRD default stays `5m`, and
the README's `Choosing idleTimeout` section remains the guidance for picking a real value.

Settling the mechanism is worth doing on its own, but it does not change where this floor
lands, and it is recorded as an open item rather than a prerequisite.

## Impact on existing tests

This is the part that is larger than the rule. Six fixtures sit below the floor:

| File | Value | Note |
|---|---|---|
| `internal/controller/squall/model_controller_sleep_envtest_test.go:42` | `1s` | comment says the small value exists to avoid a fake clock |
| `internal/controller/squall/model_controller_sleep_envtest_test.go:163` | `1s` | |
| `internal/controller/squall/model_controller_schedulable_unit_test.go:259` | `1s` | |
| `internal/proxy/handler_test.go:771` | `2s` | proxy-side `ModelSnapshot`, does not call `Validate` |
| `internal/controller/squall/model_validation_test.go:145` | `0` | the existing zero-rejection case; keeps working |
| `test/e2e/cluster/03-fixtures/model.yaml:60` | `2s` | live D171 hazard, raise to at least `1m` |

The blast radius is exactly this list: `exampleModelSpec()`, which nearly every test in
the package builds on, already declares `IdleTimeout: 5m`, so only the tests that
explicitly override it are affected. Only fixtures that reach `ValidateWithWarnings` are
affected — the proxy-side one is not.
The envtest fixtures use short real durations specifically so "aged past idleTimeout"
needs no fake clock; they move to `internal/clock`'s fake. That is the correct way to
test time-dependent behaviour and removes real sleeps from the suite, but it touches
tests guarding the sleep path, **including `TestDecide_SleepsARunThatNeverReachedReady`
(`phase_test.go:884`), the D165 guard**. That guard's behaviour must be identical before
and after; if it changes, stop and re-read D165 rather than adjusting the test.

## Testing

Table cases in `model_validation_test.go`: below the floor, exactly at the floor, above
the floor, and the existing zero. Each asserts on the error's presence and that its text
names both `idleTimeout` and the floor.

The non-vacuity check that matters, run as part of the same pass: raise `MinIdleTimeout`
to two minutes and the "exactly at the floor" case must go **red**. A mutation that
leaves the suite green is a finding, not a formality.

## Explicitly not doing

Recorded so none of it is reopened without new evidence.

**A wake latch.** Proposed in this same conversation, reproduced in envtest, then found
already decided: approved and withdrawn on live measurement (D165, 2026-09-05), with a
guard test at `phase_test.go:884`. A client still waiting keeps `inFlight > 0`, so
`AllIdle` is false and such a wake already cannot be slept; in the only case a latch
changes anything nobody is waiting, and it would hold a second instance to
`provisioningTimeout`. A client that abandons cancelling its own wake is correct,
thrifty behaviour.

**Decoupling the demand TTL from `idleTimeout`.** `sleepDue` and `hasDemand` anchor on
the same request instant with the same window, so they expire together by construction.
Lengthening the demand TTL alone produces a sleep/wake oscillation: `sleepDue` flips to
0, the next pass finds `Run.Replicas == 0` with `wantAwake` still true and applies
`Replicas: 1`. On a rented GPU that is the expensive direction. Doing this properly needs
the controller to clear the annotation on the sleep flip — a separate design.

**Giving `freshSuccess` its own window.** `model_controller.go:1037` reads `idleTimeout`
as the staleness bound on readiness evidence (b), ungated by `minReplicas`. Ledger
D167(b) is open and warns the decay is load-bearing. Untouched here.

**A `scaleToZero` block in the CR.** It depends on the two items above, not on this one.
Noted also that KubeAI — the closest comparable project, same 0-replica problem — keeps a
flat `minReplicas: 0` and introduced no such block.

## Open items this leaves

- **D171's mechanism is still unproven.** Recorded above; does not change the floor.
- **D167(b)** stays open: two `phase.go` comments state things that are no longer true.
- **The e2e cluster fixture at 2s** is fixed by this work as a side effect; the reason it
  survived the v0.1.7 release is that only the in-test `loopModelYAML` was reviewed.
