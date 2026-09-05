# internal/controller/squall

Reconciliation only. The 0↔1 replica flip, drain-first teardown via finalizer, orphan
reconciliation, status. The per-request data path lives in `squall-proxy`, not here.

## The invariants that decide the coin-flips

> **Wake may tolerate uncertainty; sleep must not.** Paying for a GPU a little longer is
> always preferable to terminating an active generation.

> **`0→1` fails open, `1→0` fails safe.** A wrong wake costs money; a wrong sleep kills a
> generation.

The wake path is the fail-**open** side: where uncertain, waking is the tolerable error. The
sleep path is the opposite and far stricter — **nothing written for the wake path may assume
sleep gets the same latitude.**

## `phase.go` — keep it pure

`Decide(observed, prior, spec, hasDemand, now) → (phase, Action)`. No clients, no context, no
I/O, no clock reads — `now` is a parameter. That purity is what makes it table-testable with
zero envtest and keeps it in `test-unit`.

**`Dead` is not `Asleep`** (F20). A terminal run is deregistered from the gateway and the next
apply mints a *new* run id, so `Dead` means recreate-and-alarm, not merely wake. Confusing the
two is the most consequential error available here — it is the difference between recovering a
reclaimed spot instance and serving 404s forever. Also: `Dead` + no demand must alarm *without*
a costly recreate.

## Other things worth knowing

- **`Ready` is reachable in production.** `observe()` derives `Observed.Ready` from dstack
  probe evidence (`run.ProbesReady`) or a recent successful forward (`freshSuccess`); do not
  describe `ModelPhaseReady` as unreachable. A wake that remains unready past
  `spec.provisioningTimeout` takes the destructive timeout path.

- **`Apply` must carry the `BaseDeploymentNum` actually observed** — not cached, defaulted or
  zero. A stale base either spuriously conflicts or defeats the CAS entirely.
- **The CAS loser must fail loudly and requeue.** Never swallow `ErrResourceChanged`, never
  retry with force (impossible anyway — `dstack.ApplyRequest` has no `Force` field).
- **`DemandAnnotation` (`squall.ackstorm.ai/demand-since`) is invented, and lives in
  `api/squall/v1alpha1`, not here.** Neither spec nor plan names a key. It was moved out of
  this package so `squall-proxy` (Phase 9, zero controller-runtime deps) can cheaply import
  the same constant it must WRITE rather than redeclare a literal — a divergent key means
  demand is never seen, and every test in this package would still pass since they all use
  the constant directly. Settle whether the key itself is right when Phase 9 is built.
- **Use `internal/clock`, never the mock package**, for any time source.
- **Level-triggered idempotence.** Every path must be safe to replay: re-reconciling an
  already-Ready model must not re-Apply, and watch replays must not double-wake.

## One idle window, not two

`spec.fleet` is gone. `spec.idleTimeout` (`metav1.Duration`, default `5m`) is the only knob:
past it, `Decide` flips `replicas: 0` AND squall always tells dstack `idle_duration: 0`, so
the machine goes with the job — no warm pool on any backend, every wake is a full cold start.
Both windows used to bill identically (the instance is rented throughout either way); the
first just kept the weights in VRAM. There is no traffic pattern where the second window helps
and several where it costs a needless reload, so the whole budget now lives in the window this
package controls and gates on in-flight evidence
(`docs/plans/2026-09-05-single-idle-window-design.md`).

**`metav1.Duration`'s `omitempty` does not drop a zero value — it is a struct, not a scalar.**
An `int32` field with `omitempty` used to drop an explicit `scaleDownDelaySeconds: 0` from
serialization, so it read back as the CRD default. `IdleTimeout metav1.Duration` serializes a
zero value as an explicit `"0s"`, which is not empty, so `omitempty` never fires and the CRD
default never fires either — a Model whose typed object never had the field set (e.g. a test
fixture unmarshalled client-side, then re-marshalled before `Create`) round-trips a written
`0s` and fails the `idleTimeout > 0` validation for real, structural-schema defaulting having
never had the chance to run. If a fixture or client-side object needs the default, set
`IdleTimeout` explicitly rather than relying on omission.

**Do not reintroduce a sleep gate keyed on "has this run ever been Ready."** It was proposed,
approved, then withdrawn on live evidence (ledger D165): a client still waiting keeps
`inFlight > 0`, which already makes `sleepDue` false, so the protection such a gate would add
already exists. The only case it would change is when nobody is waiting — and there, gating
sleep on Ready leaves a run that keeps crashing and recreating (F20) alive all the way to
`provisioningTimeout`, burning real money for a request nobody is still holding. A
`phase_test.go` case exists specifically to keep this from coming back; if you find yourself
adding a `prior.ReadyAt`-shaped check ahead of `sleepDue`, stop and re-read D165 first.

## Test conventions

- `phase_test.go` is pure and runs under `-short` with **no** skip.
- Envtest cases skip under `-short`; `suite_test.go`'s `TestMain` calls `flag.Parse()` and
  returns early. `make test-unit` must never need a control plane.
- No naked polling loops — every wait needs an upper bound **and** an explicit failure path.
- Before claiming coverage, mutate the code and watch a test go red. Two known-weak tests:
  the AC4 coalescing test fails by *timeout* rather than assertion, and the AC13 test goes red
  via an unintended Kubernetes 409 path. See `docs/references/testing-discipline.md`.
