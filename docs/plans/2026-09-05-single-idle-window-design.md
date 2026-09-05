# One idle window: collapsing `fleet.idleDuration` into `idleTimeout`

**Status:** design, approved in conversation 2026-09-05. Not yet planned or implemented.
**Amends:** `docs/specs/squall-spec-v0_18-RC.md` §5.1, §6, F21, F21b.
**Supersedes:** ledger D158 and D164 (both become moot).

## Goal

A `Model` declares **one** idle window, not two. After the last request, squall keeps paying
for `idleTimeout` with the model loaded and answering instantly; past it, nothing is paid and
the next wake is a full cold start. No warm pool, on any backend, by design.

## Why

Today the CR carries two windows that stack:

| Knob | Layer | Releases | Cost while it runs |
|---|---|---|---|
| `scaleDownDelaySeconds` | job (controller flips `replicas`→0) | the **job** | full instance price |
| `fleet.idleDuration` | instance (dstack fleet) | the **machine** | full instance price |

Both bill identically — the instance is rented throughout — but the first keeps the weights in
VRAM and answers in 0.8 s, while the second only keeps the machine and needs the engine
restarted and the weights reloaded. **The first window dominates the second at equal price**:
same total coverage, strictly better outcome. There is no traffic pattern where moving a
minute from the first to the second helps.

So `fleet.idleDuration` is not a cost-versus-latency dial, as §6 describes it. Its optimal
value is 0 everywhere, and the whole budget belongs in the window squall controls precisely
and gates on in-flight evidence.

Three secondary reasons the dstack-side clock cannot be the primary one:

- **It does not exist on the backends we run.** On `vastai`, `kubernetes`, `runpod` and
  `slurm`, dstack forces the window to zero and never reads the value (F21b).
- **It is blind to traffic.** It measures "instance with no job on it". It cannot distinguish
  a replica mid-generation from an idle one, so it cannot implement `1→0 fails safe`.
- **It is the wrong grain.** It hangs off the fleet, and squall's fleets are
  `squall-auto-<backend>`, shared by every Model on that backend.

### The one argument against, recorded

An empty machine is generic; a machine with vLLM loaded is dedicated to one model. If a
surviving idle instance could serve a *different* Model's wake, the second window would buy
something the first cannot. It does not: squall's fleets declare `nodes: 0..`, so every
instance is job-provisioned rather than pooled, and another Model would bring a different
image and different weights. Recorded because it is the argument that would reopen this.

## Measured evidence (2026-09-05, live on Vast.ai)

A Model was woken by a real request and flipped to `replicas: 0` **while still provisioning**,
having never reached Ready. Instance `49971114`, run `11036d3b`:

```
14:50:25  squall applies replicas: 0
14:50:45  dstack: Job RUNNING -> TERMINATING   reason: SCALED_DOWN
14:50:48  dstack: Job TERMINATING -> TERMINATED
14:50:48  dstack: Instance BUSY -> TERMINATING reason: JOB_FINISHED
14:50:48  dstack: Instance TERMINATING -> TERMINATED
14:50:49  Vast API v1: 0 instances
```

**24 seconds from flip to released**, faster than the 45 s measured from a Ready model. A
mid-provision teardown leaks nothing. Also measured on the same run: the Vast instance exists
**8 seconds** after the request (the 10-minute cold start is image and weights, not machine
acquisition), and the wait contract answered `503 {"state":"waking"}` at **60.007 s** against
`holdTimeout: 1m`.

## API changes

### Removed: `spec.fleet`

`ModelFleet.IdleDuration` was its only member, so the block and the type both go.

### Renamed and retyped: `scaleDownDelaySeconds` → `idleTimeout`

```go
// IdleTimeout is how long squall keeps paying after the last request, with
// the model loaded and answering instantly. Past it the run goes to zero
// replicas and dstack releases the machine; the next wake is a full cold
// start. There is no warm pool on any backend, by design.
//
// It is also hasDemand's TTL, so a zero makes an on-demand Model
// permanently unwakeable — hence the > 0 rejection in validation.
// +kubebuilder:default="5m"
// +optional
IdleTimeout metav1.Duration `json:"idleTimeout,omitempty"`
```

`5m` is the same value as today's `300`, kept deliberately so no existing Model changes what
it spends.

**The `omitempty` rescue is gone.** An `int32` with `omitempty` dropped an explicit `0` from
serialization, so `scaleDownDelaySeconds: 0` read back as the default. `metav1.Duration`
serializes `0s`, which is not empty, so an explicit zero now survives to the controller.

### Not added: `status.readyAt`

An earlier draft added a durable "this run has served at least once" marker. Its only consumer
was the sleep gate rejected below. With that gone the field has no reader, so it is not added:
a status field with no consumer is a CRD change bought for nothing.

## Behaviour changes

### What squall sends dstack

`idle_duration: 0`, always, in both places: the run profile on every apply, and `EnsureFleet`
when it creates a fleet.

**A Go zero is not enough, and getting this wrong reintroduces the 3-day footgun.**
`wire.go`'s `dstackDuration()` returns `""` for `d <= 0` and the field carries `omitempty`, so
passing `0` **omits** `idle_duration` from the JSON — and dstack then applies its own defaults:
`DEFAULT_RUN_TERMINATION_IDLE_TIME = 5m` and `DEFAULT_FLEET_TERMINATION_IDLE_TIME = 3d`
(`core/models/profiles.py`). The wire must carry a literal `"idle_duration": 0`.

Mechanism: change both wire fields from `string` with `omitempty` to `*int` with `omitempty`,
and always set a pointer to zero. A non-nil pointer survives `omitempty`. The read side
already models the field this way (`wire.go:291`).

Required test: marshal an apply body and a fleet-create body and assert each contains
`"idle_duration":0` **as a literal substring**. Asserting on a decoded struct would pass while
the field was absent.

### No fleet migration

Existing `squall-auto-*` fleets keep whatever value they were created with, and it becomes
unreachable. dstack applies `min(fleet, run)` when provisioning a new instance, which is 0;
and its "on reuse the fleet's value applies" rule never fires, because with 0 no instance
survives to be reused (`core/models/profiles.py:319`).

## Validation changes

| Kind | Before | After |
|---|---|---|
| reject | `fleet.idleDuration > 0` | **removed** — field gone |
| reject | — | **`idleTimeout > 0`** |
| reject | — | **`provisioningTimeout > 0`** |
| reject | `holdTimeout <= provisioningTimeout` | unchanged |
| warn | `holdTimeout` exceeds the warm window, computed per backend | **replaced** (below) |

The warm-window warning measured a window that no longer exists. Its replacement warns in the
direction of the mistake actually made in production: **a `holdTimeout` too small to cover a
cold start**, so every cold request is answered `503`. The comparison is against
`provisioningTimeout` — the operator's own statement of how long a wake may take — and needs
no knowledge of backends.

The threshold is **`holdTimeout < provisioningTimeout / 2`**. It is a heuristic and the
warning must say so in its text; what makes it defensible is the measured shape of a wake —
a 9m53s cold start against a 30m `provisioningTimeout` sits just above the line, and the
`holdTimeout: 5m` that would have answered `503` to every cold request sits well below it.
Half is chosen because it is the coarsest rule that separates those two cases, and a warning
that fires on borderline-but-workable settings would be trained away.

`provisioningTimeout > 0` is new and load-bearing: `provisioningDue` returns false for a
non-positive timeout, and it is the primary bound on a run that never reaches Ready.

## Deleted

- `ModelFleet` and `spec.fleet`.
- `nonDockerizedBackends` and `backendsHoldAWarmPool` in `model_validation.go`.
- The warm-window arithmetic and `TestValidate_WarmWindowIgnoresIdleDurationWhereItIsInert`,
  including the `slurm` row added on 2026-09-05.
- The `strings` import in `model_validation.go` if nothing else uses it.

Ledger D158 and D164 are closed as moot, pointing here. They are not deleted.

## Call sites to update with the rename

All read `spec.ScaleDownDelaySeconds` as `int32` seconds and become `metav1.Duration`:

| File | What it is |
|---|---|
| `phase.go:260` | `sleepDue` — signature takes `time.Duration` |
| `phase.go:274` | `unhealthyDue`'s recent-traffic window |
| `phase.go:370` | `uncontrolledTimeoutFor` — `4 × idleTimeout + 15m`, capped at 2h |
| `model_controller.go:1021` | `freshSuccess` window |
| `model_controller.go:1183` | `hasDemand` TTL |
| `model_controller.go:975` | `IdleMetrics.Observe` |
| `model_validation.go:61` | validation |
| `internal/proxy/cache.go:55,61,266` | `ModelSnapshot` field |
| `internal/proxy/handler.go:365` | `refreshIntervalFor` |

## Explicitly NOT changing

### Rejected: gating the sleep check on "has been Ready"

Proposed and approved in conversation, then **withdrawn on the evidence of the same
measurement**. It would have made `sleepDue` inapplicable to a run that never reached Ready,
so that a client abandoning mid-provision could not cancel its own wake.

The measurement showed both halves of that reasoning to be wrong.

**The protection already exists.** A client still waiting keeps `inFlight > 0`, which makes
`AllIdle` false, which makes `sleepDue` false. A wake somebody is waiting for cannot be slept
today. The gate would only change the case where *nobody* is waiting.

**And in that case the gate costs money.** Mid-experiment, the first container died on its own
(`CONTAINER_EXITED_WITH_ERROR`, 105 s in). Squall's F20 detected the uncommanded death and
minted a fresh run — a **second Vast instance** — for a request already answered `503` 68
seconds earlier:

```
14:49:09  Job -> TERMINATING  reason: CONTAINER_EXITED_WITH_ERROR
14:49:13  instance #1 TERMINATED
14:49:28  squall: "uncommanded dstack run death detected, recreating"
14:49:29  instance #2 created
14:50:25  squall applies replicas: 0     <-- the sleep check stops it
```

What stopped the second instance is precisely the check the gate would have disabled. With the
gate, nothing would have bounded it short of `provisioningTimeout` — 30 minutes in that CR,
$0.22 at $0.45/h and about $1 on a 27B — and a repeatedly-crashing container would have
death-recreate cycled for the whole window, a fresh instance each time.

A client that abandons cancelling its own wake is correct, thrifty behaviour, not a bug.

### Unchanged: the orphan reaper

It sweeps every dstack run against every Model, reaps only runs carrying squall's own
`SQUALL_MODEL_UID` stamp whose Model no longer exists, and uses its own constants
(`DefaultReapInterval`, `DefaultReapGrace`). It reads nothing from the CR and this change does
not touch it.

### Unchanged: the uncontrolled guardian

Rename only. `4 × idleTimeout + 15m`, capped at 2h — same formula, same numbers.

### Unchanged: `idleTimeout`'s second job

It remains `hasDemand`'s TTL as well as the idle window. Two questions sharing one number:
"how long do I keep paying after the last request" and "how long do I remember that somebody
asked". They expire together by construction, which is why the flip and the demand expiry
landed 4 seconds apart in the measurement. Left as is, deliberately, and recorded in the
ledger rather than fixed inside a change whose point was to remove a knob.

## Compatibility

`v1alpha1`, no version bump: pre-alpha, and the CRD is already applied by hand on every
upgrade because Helm never upgrades CRDs.

**An old manifest applies without error and changes behaviour silently.** Kubernetes prunes
unknown fields, so `fleet` and `scaleDownDelaySeconds` vanish and `idleTimeout` takes its 5m
default. A CR carrying `scaleDownDelaySeconds: 600` goes from 10m to 5m with no warning. This
is the accepted cost of removing the fields outright rather than deprecating them. The
CHANGELOG must carry a **BREAKING** entry instructing operators to rewrite their Models before
applying the new CRD.

## Tests

Grouped into one review and one gate run at the block boundary, per the project's testing
discipline. Written as they go; proven non-vacuous in a single mutation sweep at the end.

**Wire (`internal/dstack`)**
- Apply body contains the literal `"idle_duration":0`.
- Fleet-create body contains the literal `"idle_duration":0`.
- *Mutation:* revert the wire field to `string` + `omitempty`. Both must go red. This is the
  3-day-footgun test and it is the most important one in the change.

**Validation (`model_validation_test.go`)**
- `idleTimeout: 0` rejected; `idleTimeout: 0s` explicitly written also rejected.
- `provisioningTimeout: 0` rejected.
- `holdTimeout` well under `provisioningTimeout` warns; comfortably sized does not.
- *Mutation:* drop each rejection; the matching case must go red.

**Phase (`phase_test.go`)**
- `sleepDue` fires on `now - lastRequestAt > idleTimeout` with complete idle evidence,
  including for a run that never reached Ready — the behaviour the rejected gate would have
  removed. This test exists to stop it being reintroduced, and its comment says so.
- `uncontrolledTimeoutFor` still yields `4 × idleTimeout + 15m`, capped at 2h.
- *Mutation:* add the `readyAt != nil` gate to `sleepDue`; the never-Ready case must go red.

**Not re-run:** the envtest and e2e suites unless the CRD change touches them, which it does —
`config/samples/` and `docs/runbooks/` carry `fleet:` blocks and must be rewritten as part of
this change, and `model_sample_envtest_test.go` reads the samples.

## Documentation to update in the same change

- `docs/specs/squall-spec-v0_18-RC.md` §5.1, §6 (the two-layer table), F21, F21b.
- `README.md`: the two-layer explanation, the `Send it a request` section, every sample CR.
- `config/samples/*.yaml` and `docs/runbooks/*.yaml`: drop `fleet`, rename the field.
- `api/squall/v1alpha1/model_types.go` field docs.
- `CHANGELOG.md`: a BREAKING entry.
- `internal/controller/squall/CLAUDE.md`, `internal/dstack/CLAUDE.md`.

## Findings for the ledger, raised by this work

- The wire's `omitempty` + `dstackDuration("")` trap above.
- `phase.go:244`'s comment claims a provisioning timeout makes the next attempt mint a fresh
  run. It does not: the run survives at `replicas: 0`, so the next wake flips that same run.
- `phase.go:256`'s comment claims `Observed.Ready` is always false. It has not been since
  `model_controller.go:1021` began computing it from `ProbesReady || freshSuccess`.
- Recreate-after-death spends a fresh instance for a client that has already been answered,
  bounded only by the demand TTL and the sleep check. Correct per "wake fails open", worth
  knowing.
- The first container's `CONTAINER_EXITED_WITH_ERROR` at 105 s is unexplained. Not chased —
  it was not the question — but a `vllm` job dying that early on a known-good image and digest
  deserves a look before the next live run.
