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

## Test conventions

- `phase_test.go` is pure and runs under `-short` with **no** skip.
- Envtest cases skip under `-short`; `suite_test.go`'s `TestMain` calls `flag.Parse()` and
  returns early. `make test-unit` must never need a control plane.
- No naked polling loops — every wait needs an upper bound **and** an explicit failure path.
- Before claiming coverage, mutate the code and watch a test go red. Two known-weak tests:
  the AC4 coalescing test fails by *timeout* rather than assertion, and the AC13 test goes red
  via an unintended Kubernetes 409 path. See `docs/references/testing-discipline.md`.
