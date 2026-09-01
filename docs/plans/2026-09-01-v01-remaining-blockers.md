# v0.1.0 Remaining Blockers — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to
> implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the four v0.1.0 blockers that are actual work — `status.fleet`, D97's
provisioning deadline, the deployment rename, and the dead e2e suite — and make the ledger an
honest statement of current state.

**Architecture:** Nothing structural. `status.fleet` publishes what `preflight` already
computes and throws away. D97 adds the fourth and only *destructive* phase flip, on the one
window the other three deliberately do not cover. The rename and the e2e fix are mechanical.

**Tech Stack:** Go 1.26.6 (containerized — every command through `./scripts/dev.sh`),
controller-runtime, Helm, Ginkgo (e2e).

---

## Global Constraints

- **The spec is authoritative.** `docs/specs/squall-spec-v0_18-RC.md`. Where this plan and the
  spec disagree, say so rather than silently picking one.
- **`0→1` fails open, `1→0` fails safe.** Wake may tolerate uncertainty; sleep must not.
- **Squall NEVER sends `force`** (F18, §5.2, AC13), enforced by construction.
- **All commands go through the wrapper.** `./scripts/dev.sh go ...`,
  `./scripts/dev.sh make test-unit | test-envtest | qa-lint` (the lint target is `qa-lint`).
  Never bare `go`/`make`. Never `DOCKER_BUILDKIT=0`.
- **`make test-unit` must never need a control plane.**
- **CRD changes are two files.** `./scripts/dev.sh make manifests generate` rewrites both
  `config/crd/bases/` and `deploy/helm/squall/crds/`. Helm never upgrades CRDs (D88) —
  deploy with `kubectl apply --server-side --force-conflicts`.
- **Git rules for concurrent agents:** never `git add -A`; never `git reset`, `--amend` or
  rebase past a commit you did not create; never `git checkout --` / `restore` on a dirty
  tree. **Only add commits.**
- **Ledger numbering starts at D133.** D128 is the file maximum; the guardian-handshake plan
  (`2026-09-01-guardian-handshake-and-uncontrolled-capacity.md`) reserves D129–D132.

---

## HARD SEQUENCING CONSTRAINT — read before scheduling

**The constraint is on DEPLOYING the rename, not on committing it.** An earlier wording said
"must not land", which was read as "must not commit". To be unambiguous:

- **Committing Task 3 is safe and unblocked.** Editing Helm templates replaces no Pod.
- **`helm upgrade` with the rename is the hazardous act.** To Helm a Deployment rename is a
  delete plus a create, so every proxy Pod is replaced, which wipes the in-memory activity map
  of every Ready Model — the event measured on 2026-08-31 at **2h21m of a $1.894/h GPU serving
  zero requests** (D129).

**That hazard is now closed.** The guardian-handshake plan is merged and is an ancestor of this
branch, so `status.lastRequestAt` is persisted and any deploy of this branch ships the rename
and the fix together. There is no ordering left to respect. Proceed.

**No task in this plan blocks any other.** Tasks 0-4 touch disjoint files; Task 5 is
documentation and depends on nothing. A task reported BLOCKED stops **that task only** — carry
on with the rest and report at the end what was left undone and why. Do not stop the plan.

---

## Task 0: Bury the reaper's idle path — it was commented out, not deleted

**Do this first.** It is five minutes, it touches one file, and it removes a 95-line corpse that
anyone reading `reaper.go` during Tasks 1-4 has to wade through. **It gets no review of its own
and no gate of its own** — it rides the Task 1-2 block gate.

**Files:** `internal/controller/squall/reaper.go` only.

**What happened.** The guardian-handshake plan's Task 7 required deleting `reapIfIdle` and the
engine-probe path. Commit `793529f` ("refactor(reaper): remove engine-specific idle probing")
removed the `Reaper` struct fields but wrapped the method body in a `/* ... */` block instead of
deleting it. Verified 2026-09-01: `reaper.go` is 309 lines, of which **213-307 are commented-out
dead code** — a third of the file. It compiles and every gate passes precisely because it is
commented out, so nothing was ever going to catch it.

It also still carries the stale `see D100` reference that Task 7 Step 5 required renumbering.
D100 is the `forwardModel` finding; the sleep-unreachable incident is **D129**. Deleting the
block resolves that reference by removing it, which is the correct outcome — there is no
surviving code for it to describe.

- [ ] **Step 1: Confirm the boundaries before cutting**

Line numbers drift; verify against content, not just the numbers.

```bash
sed -n '212,214p' internal/controller/squall/reaper.go   # blank, then `/*`, then `// said so for IdleLimit.`
sed -n '305,309p' internal/controller/squall/reaper.go   # ... `var _ manager.Runnable = &Reaper{}`, `*/`, blank, then the LIVE `var _ manager.Runnable = &Reaper{}`
```

Note the duplication: the commented block ends with its own copy of
`var _ manager.Runnable = &Reaper{}`, and the live one sits immediately after the closing `*/`.
Cutting `213-307` inclusive keeps exactly one.

- [ ] **Step 2: Confirm nothing live depends on what you are cutting**

Every reference to the dead symbols must fall inside the range:

```bash
grep -n "idleLimit\|Utilisation" internal/controller/squall/reaper.go
```
Expected (verified 2026-09-01): lines `231 244 281 284 292` — all inside 213-307, all inside the
comment. `DefaultIdleCapacityLimit` and `UtilisationProbe` already have zero references anywhere
in `internal/` or `cmd/`; do not go looking for them to delete.

**Imports need no cleanup.** Commented-out code does not hold an import, so the current import
block is already exactly what the live code uses. Deleting a comment cannot orphan one.

- [ ] **Step 3: Cut it**

```bash
sed -i '213,307d' internal/controller/squall/reaper.go
```

- [ ] **Step 4: Verify**

```bash
test "$(wc -l < internal/controller/squall/reaper.go)" -eq 214 || echo "UNEXPECTED LENGTH"
grep -c "D100" internal/controller/squall/reaper.go   # must be 0
grep -c "^/\*$" internal/controller/squall/reaper.go # must be 0
./scripts/dev.sh go build ./... && ./scripts/dev.sh go test ./internal/controller/squall/ -count=1 -short
```

The orphan-GC tests must all still pass — this task removes no live behaviour whatsoever. If any
test goes red, **stop**: something live was inside the range and the boundaries were wrong.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/squall/reaper.go
git commit -m "refactor(reaper): delete the idle path instead of commenting it out"
```

---

## Task 1: `status.fleet` — publish what preflight already knows

**Files:**
- Modify: `api/squall/v1alpha1/model_types.go`
- Modify: `internal/controller/squall/preflight.go`
- Modify: `internal/controller/squall/model_controller.go`
- Test: `internal/controller/squall/preflight_test.go`

**Interfaces:**
- Produces: `ModelStatus.Fleet []FleetStatus`; `preflight` returns a third value
  `[]squallv1alpha1.FleetStatus`.
- Consumes: nothing from other tasks.

**Why this is nearly free:** `preflight` (`preflight.go:44`) already loops the Model's
backends and, per backend, learns whether it is configured (`BackendConfigured`), whether a
fleet admits it (`HasFleetFor`), and — when it does not — whether `EnsureFleet` could create
one. It then collapses all of that into one condition string and discards the detail. This
task stops discarding it.

**Decision already taken, do not reopen:** no Fleet CRD. See
`docs/references/decisions-and-open-items.md` §"Decision: no Fleet CRD". A declarative Fleet CR
was **rejected** by the owner, not deferred; a read-only mirror CR was noted as a possible
future convenience only. `status.fleet` on the Model is what 0.1.0 ships.

- [ ] **Step 1: Add the status type and field**

In `api/squall/v1alpha1/model_types.go`:

```go
// FleetStatus reports, per backend this Model may draw instances from, whether
// dstack actually has a pool that admits it.
//
// It exists because LIVE-7 was invisible: the vastai fleet every live GPU run
// depended on existed only inside dstack's SQLite database, created by hand
// during debugging. A Postgres migration started dstack on an empty database,
// the fleet vanished, and every vastai Model reported Schedulable=False /
// NoFleet with nothing in any repository describing what had been lost.
//
// A fleet is a POOL, not a filter -- closest analogue is a managed-Kubernetes
// node pool. It declares which backends instances may be drawn from, scales
// 0->N on demand, and persists as an object even while empty. An empty fleet
// costs nothing. spec.placement/spec.resources are the RUN's requirements, a
// separate thing that dstack INTERSECTS with the fleet's.
type FleetStatus struct {
	// Backend is the dstack backend name, e.g. "vastai".
	Backend string `json:"backend"`

	// Name is the fleet squall looked for or created (dstack.FleetName).
	// Empty when the backend is not configured, because squall never got as
	// far as asking about a fleet.
	// +optional
	Name string `json:"name,omitempty"`

	// State is what squall found, and it distinguishes the three outcomes
	// that all used to read as one unhappy condition string:
	//
	//   Admitting   -- a fleet already admits this backend.
	//   Created     -- none did, and squall created one (EnsureFleet).
	//   Unfleeted   -- none did, and squall could not create one. This
	//                  backend contributes nothing; runs get zero offers.
	//   Unconfigured-- the backend itself is not configured on the server,
	//                  so the fleet question never arose.
	//
	// +kubebuilder:validation:Enum=Admitting;Created;Unfleeted;Unconfigured
	State string `json:"state"`
}
```

and in `ModelStatus`:

```go
	// Fleet reports the dstack pools backing this Model, one entry per entry
	// in spec.placement.backends, in that order.
	//
	// It is a MIRROR and never a source of truth: squall does not own fleet
	// definitions (that decision is recorded and closed -- no Fleet CRD). What
	// removes the LIVE-7 failure mode is EnsureFleet making the fleet derived
	// state; this field is what makes the result visible without reading
	// controller logs.
	// +optional
	// +listType=map
	// +listMapKey=backend
	Fleet []FleetStatus `json:"fleet,omitempty"`
```

Add the four state constants beside the existing `Reason*` constants:

```go
const (
	FleetStateAdmitting    = "Admitting"
	FleetStateCreated      = "Created"
	FleetStateUnfleeted    = "Unfleeted"
	FleetStateUnconfigured = "Unconfigured"
)
```

- [ ] **Step 2: Regenerate**

```bash
./scripts/dev.sh make manifests generate
```

- [ ] **Step 3: Write the failing test**

In `internal/controller/squall/preflight_test.go`, using the file's existing
`preflightClient` fake (read it first; do not invent a second one):

```go
// TestPreflight_ReportsFleetStatePerBackend pins the mirror. The three outcomes that
// used to collapse into one condition string must come back distinguishable.
func TestPreflight_ReportsFleetStatePerBackend(t *testing.T) {
	c := &fakePreflightClient{
		configured: map[string]bool{"vastai": true, "aws": true, "gcp": false},
		hasFleet:   map[string]bool{"vastai": true, "aws": false},
		ensureErr:  map[string]error{}, // aws creation succeeds
	}
	_, _, fleets := preflight(context.Background(), c, []string{"vastai", "aws", "gcp"})

	want := []squallv1alpha1.FleetStatus{
		{Backend: "vastai", Name: "squall-auto-vastai", State: squallv1alpha1.FleetStateAdmitting},
		{Backend: "aws", Name: "squall-auto-aws", State: squallv1alpha1.FleetStateCreated},
		{Backend: "gcp", State: squallv1alpha1.FleetStateUnconfigured},
	}
	if !reflect.DeepEqual(fleets, want) {
		t.Fatalf("fleet mirror:\n got %+v\nwant %+v", fleets, want)
	}
}

// TestPreflight_UnfleetedWhenCreationFails is the state that costs money: this
// backend yields zero offers and the operator must be able to see which one.
func TestPreflight_UnfleetedWhenCreationFails(t *testing.T) {
	c := &fakePreflightClient{
		configured: map[string]bool{"vastai": true},
		hasFleet:   map[string]bool{"vastai": false},
		ensureErr:  map[string]error{"vastai": errors.New("dstack said no")},
	}
	_, _, fleets := preflight(context.Background(), c, []string{"vastai"})
	if len(fleets) != 1 || fleets[0].State != squallv1alpha1.FleetStateUnfleeted {
		t.Fatalf("a failed EnsureFleet must report Unfleeted, got %+v", fleets)
	}
}

// TestPreflight_DstackErrorPublishesNoMirror keeps the fail-open contract. An error
// talking to dstack already means "no opinion" for the condition; it must not
// publish a half-built mirror that reads as fact.
func TestPreflight_DstackErrorPublishesNoMirror(t *testing.T) {
	c := &fakePreflightClient{configuredErr: errors.New("connection refused")}
	reason, _, fleets := preflight(context.Background(), c, []string{"vastai"})
	if reason != "" || fleets != nil {
		t.Fatalf("a dstack error must yield no reason and no mirror, got %q / %+v", reason, fleets)
	}
}
```

- [ ] **Step 4: Run and watch it fail**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -run 'TestPreflight_' -count=1 -short
```
Expected: FAIL — `preflight` returns 2 values, not 3.

- [ ] **Step 5: Widen `preflight`**

Change the signature to
`func preflight(ctx context.Context, c preflightClient, backends []string) (reason, message string, fleets []squallv1alpha1.FleetStatus)`
and record one entry per backend as the existing loop already decides each case. Keep every
early `return "", ""` as `return "", "", nil` — **a dstack error must publish no mirror at all**,
because a partial mirror would read as fact. Do not restructure the loop; only accumulate.

Update the single call site in `model_controller.go`'s `checkSchedulable` (or wherever
`preflight` is called) to take the third value.

- [ ] **Step 6: Publish it**

At the same call site, after the condition is set:

```go
	// Publish the mirror only when preflight actually reached a verdict. nil
	// means "could not ask", and the LAST known good mirror is better than
	// blanking the field on a transient dstack hiccup.
	if fleets != nil {
		model.Status.Fleet = fleets
	}
```

- [ ] **Step 7: Add the printer column**

Beside the existing `SCHEDULABLE` column on the Model type:

```go
// +kubebuilder:printcolumn:name="Fleet",type=string,JSONPath=`.status.fleet[*].state`
```

- [ ] **Step 8: Run and commit**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -count=1 -short
git add api/squall/v1alpha1/ config/crd/bases/ deploy/helm/squall/crds/ \
        internal/controller/squall/preflight.go internal/controller/squall/preflight_test.go \
        internal/controller/squall/model_controller.go
git commit -m "feat(status): mirror the dstack fleets backing each Model"
```

---

## Task 2: D97 — `provisioningTimeout` gets its destructive trigger

**Files:**
- Modify: `internal/controller/squall/phase.go`
- Modify: `internal/controller/squall/model_controller.go`
- Test: `internal/controller/squall/phase_test.go`

**The blocker is already gone.** D7 records OQ3 as **RESOLVED in spec v0.18 §5.2**:
`status.wakeStartedAt`, journaled by the controller on every `0→1` actuation. That field
exists today and is populated. Nothing is waiting on a decision; only the trigger was never
written. Verified 2026-09-01: `grep ProvisioningTimeout internal/controller/` returns no
non-test hit.

**What the spec mandates** (`model_types.go`, the field's own doc comment):

> ProvisioningTimeout is the single age-based DESTRUCTIVE trigger in the whole controller
> contract (§5.2): a run that never reaches Ready within the window is destroyed, alarmed, and
> marked Dead. No other timeout in this spec destroys anything — see MaxLifetime.

So this is implementing a settled contract, not designing one.

**Where it sits among the flips.** Four now, and they partition the Model's life cleanly. Get
this ordering right; each was deliberately scoped to not cover the others' window:

| Flip | Window it owns | Requires | Result |
|---|---|---|---|
| `provisioningTimeout` | **before** Ready | `!Ready` | **Dead** (destructive) |
| `unhealthyDue` | after Ready, traffic arriving, nothing delivered | `Ready` | Asleep |
| `sleepDue` | after Ready, no traffic | complete evidence | Asleep |
| `uncontrolledDue` | after Ready, evidence unobtainable | — | Asleep |

`unhealthyDue` deliberately requires `observed.Ready` precisely so it does **not** reach into
provisioning (its own doc comment says so). Widening it would silently duplicate a declared
spec field under a different number. Do not.

- [ ] **Step 1: Write the failing test**

```go
// TestProvisioningDue is D97. Nothing bounded a Model stuck in Waking; a wake that
// hangs -- a bad offer, a host with no network, an image that never pulls -- billed
// until a human noticed. Observed live 2026-08-31.
func TestProvisioningDue(t *testing.T) {
	now := time.Now()
	old := metav1.NewTime(now.Add(-25 * time.Minute))
	recent := metav1.NewTime(now.Add(-5 * time.Minute))

	if !provisioningDue(&old, false, 20*time.Minute, now) {
		t.Fatal("25 minutes waking against a 20 minute timeout must fire")
	}
	if provisioningDue(&recent, false, 20*time.Minute, now) {
		t.Fatal("5 minutes waking must not fire")
	}
	if provisioningDue(&old, true, 20*time.Minute, now) {
		t.Fatal("READY: provisioning finished, this deadline is no longer its business")
	}
	if provisioningDue(nil, false, 20*time.Minute, now) {
		t.Fatal("no wake anchor: nothing to measure from, must not fire")
	}
	if provisioningDue(&old, false, 0, now) {
		t.Fatal("timeout 0 disables the only destructive trigger; it must never fire")
	}
}

// TestDecide_ProvisioningTimeoutMarksDeadNotAsleep is the distinction that matters most
// in this package (F20). Asleep is a run at zero replicas, addressable and reusable. Dead
// is deregistered: the next apply mints a NEW run id. A wake that never landed produced
// no usable run, so it must be Dead -- reporting Asleep would leave squall waiting on a
// replica flip against a run that will never serve.
func TestDecide_ProvisioningTimeoutMarksDeadNotAsleep(t *testing.T) {
	now := time.Now()
	woke := metav1.NewTime(now.Add(-30 * time.Minute))
	phase, action := Decide(
		Observed{Run: &dstack.Run{Name: "squall-qwen", Replicas: 1, Status: "provisioning"}, Ready: false},
		squallv1alpha1.ModelStatus{WakeStartedAt: &woke, RunID: "r1"},
		squallv1alpha1.ModelSpec{
			MinReplicas:           0,
			ProvisioningTimeout:   metav1.Duration{Duration: 20 * time.Minute},
			ScaleDownDelaySeconds: 120,
		},
		true, now)

	if phase != squallv1alpha1.ModelPhaseDead {
		t.Fatalf("a wake that never landed is Dead, not %s", phase)
	}
	if !action.Apply || action.Replicas != 0 || !action.Alarm {
		t.Fatalf("must destroy and alarm, got %+v", action)
	}
}
```

- [ ] **Step 2: Run and watch it fail**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -run 'TestProvisioningDue|TestDecide_ProvisioningTimeout' -count=1 -short
```
Expected: FAIL — `provisioningDue` undefined.

- [ ] **Step 3: Implement `provisioningDue`**

```go
// provisioningDue is §5.2's single age-based DESTRUCTIVE trigger: a wake that never
// reached Ready inside spec.provisioningTimeout.
//
// It is the operator's statement of how long an instance may take to become useful.
// If a model is known to need fifteen minutes, that is what the field says, and an
// offer that cannot manage it is not an offer worth paying for -- a host with a
// broken network, an image that never pulls, a GPU that never initialises.
//
// It requires !ready by construction, which is what keeps it out of the other three
// flips' way: once the engine answers, provisioning is over and this deadline has no
// further opinion. A zero timeout disables the ONLY destructive trigger in the
// contract, so it is honoured as written and never defaulted here -- the CRD's own
// default owns that.
func provisioningDue(wakeStartedAt *metav1.Time, ready bool, timeout time.Duration, now time.Time) bool {
	if ready || timeout <= 0 || wakeStartedAt == nil || wakeStartedAt.IsZero() {
		return false
	}
	return now.Sub(wakeStartedAt.Time) > timeout
}
```

- [ ] **Step 4: Wire it into `Decide`**

It goes in the `observed.Run.Replicas > 0` branch, **FIRST — ahead of `sleepDue`,
`unhealthyDue` and `uncontrolledDue`**. Rationale to put in the comment: those three all
describe a replica that reached Ready at some point; this one describes a replica that never
did, and the more specific diagnosis must win.

```go
	// §5.2's single destructive trigger, checked before the three Asleep flips:
	// they all describe a replica that served at some point, this one describes a
	// wake that never landed. Dead, not Asleep (F20): the run never became usable,
	// so the next attempt must mint a fresh one rather than flip replicas on a
	// husk. Alarm is REQUIRED -- this is squall destroying paid capacity on a
	// deadline, and it must never be silent.
	if provisioningDue(prior.WakeStartedAt, observed.Ready, spec.ProvisioningTimeout.Duration, now) {
		return squallv1alpha1.ModelPhaseDead, Action{
			Apply:    true,
			Replicas: 0,
			Current:  observed.Run,
			Alarm:    true,
			At:       now,
		}
	}
```

**Check the `observed.Run == nil` branch too.** A Model whose run vanished mid-provisioning
already lands there and is handled; do not add a second deadline path for it.

- [ ] **Step 5: Report it, with the money in the line**

At the actuation site in `model_controller.go`, beside the existing unhealthy report:

```go
	if action.Alarm && phase == squallv1alpha1.ModelPhaseDead && provisioningTimedOut {
		logger.Info("PROVISIONING DEADLINE EXCEEDED: this wake never reached Ready, so squall is "+
			"destroying it rather than paying for an instance that may never serve",
			"model", model.Name,
			"wakeStartedAt", model.Status.WakeStartedAt,
			"provisioningTimeout", model.Spec.ProvisioningTimeout.Duration,
			"pricePerHour", observedPerHour)
	}
```

Set `Healthy=False` with a distinct reason (add `ReasonProvisioningTimeout` beside the other
`Reason*` constants) so `kubectl describe model` says which of the four flips fired. Add an
Event too if the package already emits them; do not add an Event recorder if it does not.

- [ ] **Step 6: Check the flap interaction**

D96 records that a Model broken by *configuration* re-wakes on the next request and buys a
fresh GPU each cycle. This flip has the same shape and the same mitigation, already present:
`Dead` + no demand does **not** recreate (the live log line is `uncommanded dstack run death
detected; no demand, NOT recreating until a request arrives`). Confirm that path still holds
after this change and write one test asserting it; if it does not, stop and report — a
destructive trigger that re-buys immediately is worse than no trigger.

- [ ] **Step 7: Run and commit**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -count=1 -short
git add internal/controller/squall/phase.go internal/controller/squall/phase_test.go \
        internal/controller/squall/model_controller.go api/squall/v1alpha1/model_types.go
git commit -m "feat(phase): bound a wake that never lands (D97)"
```

- [ ] **Step 8: BLOCK GATE for Tasks 0-2 + mutation sweep**

```bash
./scripts/dev.sh make test-unit && ./scripts/dev.sh make test-envtest && ./scripts/dev.sh make qa-lint
```

| # | File | Mutation | Test that MUST go red |
|---|---|---|---|
| M1 | `preflight.go` | return the mirror even on a dstack error | `TestPreflight_DstackErrorPublishesNoMirror` |
| M2 | `preflight.go` | report `Created` when `EnsureFleet` errors | `TestPreflight_UnfleetedWhenCreationFails` |
| M3 | `phase.go` | `provisioningDue` drops the `ready` guard | `TestProvisioningDue` (ready case) |
| M4 | `phase.go` | `provisioningDue` drops the `timeout <= 0` guard | `TestProvisioningDue` (zero case) |
| M5 | `phase.go` | return `Asleep` instead of `Dead` | `TestDecide_ProvisioningTimeoutMarksDeadNotAsleep` |
| M6 | `phase.go` | move the provisioning check AFTER `sleepDue` | needs a test — write one if none goes red |

---

## Task 3: Rename the deployments — `squall-operator` / `squall-proxy`

**BLOCKED until the guardian-handshake plan is deployed.** See the sequencing constraint above.

**Files:** `deploy/helm/squall/templates/*.yaml`, `deploy/helm/squall/values.yaml`,
`Makefile`, `test/e2e/*`, `docs/runbooks/*`, `docs/references/toolchain-and-traps.md`.

Current names, verified 2026-09-01: Deployment `squall-controller-manager`, Deployment
`proxy`, Service `proxy`.

- [ ] **Step 1: Find every reference before changing anything**

```bash
grep -rn "squall-controller-manager\|deploy/proxy\|svc/proxy\|name: proxy" \
  --include="*.yaml" --include="*.go" --include="*.sh" --include="*.md" --include="Makefile" . \
  | grep -v "^./docs/reviews/" > /tmp/rename-refs.txt
wc -l /tmp/rename-refs.txt
```

Work from that list. A missed reference here is not cosmetic: the controller finds the proxy
through `SQUALL_PROXY_SERVICE_NAME` / `SQUALL_PROXY_SERVICE_NAMESPACE`, and `gatherActivity`
returns nil when `ProxyService` is unset — which silently disables §6's idle evidence
entirely. **Rename the Service and its env var in the same commit.**

- [ ] **Step 2: Rename, in one commit**

`squall-controller-manager` → `squall-operator`; Deployment and Service `proxy` →
`squall-proxy`. Update label selectors together with the names. Note that a Deployment rename
is a delete-plus-create for Helm, so `matchLabels` may be changed freely in the same move —
they are immutable only for an in-place upgrade.

- [ ] **Step 3: Verify nothing resolves the old names**

```bash
grep -rn "squall-controller-manager" --include="*.yaml" --include="*.go" --include="*.sh" . | grep -v docs/reviews/ | grep -v docs/references/deviations
./scripts/dev.sh make qa-lint && ./scripts/dev.sh helm template deploy/helm/squall | grep -E "^  name: (squall-operator|squall-proxy)"
```

- [ ] **Step 4: Commit**

```bash
git commit -m "refactor(deploy): name the deployments after the binaries they run"
```

---

## Task 4: Make the Go e2e suite run again

**Files:** `test/e2e/e2e_suite_test.go` (3 stale references verified 2026-09-01), and whatever
else the build turns up.

**The cluster is SHARED and LIVE.** `hack/cluster.sh` defaults to `CLUSTER_NAME=squall-test`,
which is the same kind cluster currently holding the real Models. `cluster-up` is idempotent
(it exports the kubeconfig for an existing cluster and only creates a missing one), and
`cluster-hydrate` rebuilds from the working tree and patches the Deployments — so it restarts
the proxy and controller with THIS branch's code, guardian fix included. `cluster-down`
deletes the cluster; do not run it. Check `kubectl -n squall get models` for a non-Asleep
Model before hydrating, and if one is awake, ask before restarting anything.

- [ ] **Step 1: See the actual failure**

```bash
./scripts/dev.sh go vet -tags e2e ./test/e2e/ 2>&1 | head -40
grep -n "fake-dstack\|model-mock" test/e2e/e2e_suite_test.go
```

- [ ] **Step 2: Find the current names**

The fake dstack server is `internal/dstack/mock`; the fixture Model in the live cluster is
`e2e-fixture-model`. Read `internal/dstack/mock/mock.go` and the Helm e2e values to get the
deployed names rather than guessing. If Task 3 has already landed, the proxy is `squall-proxy`.

- [ ] **Step 3: Fix the naming, run the suite**

```bash
./scripts/dev.sh go vet -tags e2e ./test/e2e/
./scripts/dev.sh make test-e2e   # or the target `make help` lists for e2e
```

If the suite compiles but fails on cluster assumptions, report exactly which and stop —
`test-unit` must stay green regardless, and the e2e build tag keeps it out of that target.

- [ ] **Step 4: Confirm the phase split still holds**

```bash
./scripts/dev.sh make test-unit    # must pass with NO control plane
```

- [ ] **Step 5: Commit**

```bash
git add test/e2e/
git commit -m "fix(e2e): the suite referenced names that no longer exist"
```

---

## Task 5: F6 — make the ledger an honest statement of current state

The ledger is what a reader uses to judge whether 0.1.0 is honest about itself. It currently
is not: entries fixed days ago still read OPEN.

**Method — do NOT trust the entry's own text.** For each entry whose status contains `OPEN`,
check the claim against the code and record the evidence. An entry that cannot be checked
mechanically stays OPEN with a one-line note saying why.

- [ ] **Step 1: Apply the six already verified by the main session (2026-09-01)**

Each was checked against the tree, not against its own prose. Update the status column and
append a one-line `VERIFIED 2026-09-01: <evidence>`; **do not rewrite the claim or the
analysis, and do not renumber anything.**

| Entry | Claim | Evidence it is fixed |
|---|---|---|
| **D26** | `squall_model_price_per_hour` has no production data source | `dstack.Run.PricePerHour` + `replicaPricePerHour` (`probes.go`) read `job_provisioning_data.price`. Measured live: `pricePerHour=1.89445`. Commit `899f3f8` |
| **D42** | `modelFromRequest` does an unbounded `io.ReadAll` | bounded read present in `internal/proxy/handler.go`. Commit `1bc8d49`; TODO F3 marked DONE |
| **D55** | squall never sends `resources`/`placement` to dstack | both present on the apply path; the live `ollama-tiny` run carried `regions`, `maxPricePerHour` and a gpu memory floor, and dstack honoured them |
| **D74** | `ReasonNoOffers` declared but never set | now set in non-test code under `internal/` |
| **D7** | `provisioningTimeout`/`maxLifetime` have no since-when anchor | `status.wakeStartedAt` exists and is populated. **Split the entry:** the ANCHOR half is resolved; the TRIGGER half is D97 and stays open until Task 2 lands |
| **D88** | Helm never upgrades CRDs; new status fields silently pruned | the `--server-side` procedure is documented in `docs/` and the deploy path. Keep it OPEN as **process**, not as a defect, and say so |

- [ ] **Step 2: Two the main session could NOT verify — check these yourself**

- **D86** (`squall-proxy has no per-request auth`). A grep for `Authorization` in
  `internal/proxy/handler.go` hits, but that is almost certainly **D79's forwarding of the
  caller's header to the upstream**, which is a different thing. The TODO still lists proxy
  authentication as an After-0.1.0 item. **Expected outcome: stays OPEN.** Verify rather than
  assume, and record which of the two the grep was hitting.
- **D28** (`Observed.Ready`/`ModelPhaseReady` unreachable in deployment today, by design).
  This is now **stale**: `phase=Ready` was observed live on `squall/ollama-tiny` on
  2026-08-31. `observe()` sets `Ready` from `run.ProbesReady || freshSuccess(...)`, so the
  path exists. Note that `internal/controller/squall/CLAUDE.md` repeats the stale claim and
  must be corrected in the same commit.

- [ ] **Step 3: Sweep the rest**

Remaining OPEN entries to check, each against the code:
`D1 D3 D4 D5 D6 D8 D9 D19 D20 D47 D51 D59 D60 D69 D70 D94 D96 D97 D101`.
Several are genuinely open (D69 is explicitly "not fixable in squall"; D94, D96, D101 are
After-0.1.0 decisions; D97 closes in Task 2). Several are documentation debts, not defects —
mark those `OPEN (docs)` so a reader can tell the difference between "squall is broken" and
"squall is undocumented".

- [ ] **Step 4: Add D133 recording the audit**

`D133 — the ledger's status column was not trustworthy` — how many entries were checked, how
many moved, and the method (verify against code, never against the entry's own prose). This is
the entry that lets the next reader know the audit happened and when.

- [ ] **Step 5: Update `docs/TODO.md`**

Tick F1 (its body already says DONE and the code confirms it). Move F5's completed half to
done: chart `0.1.0`/`0.1.0`, `VERSION ?= 0.1.0`, `CHANGELOG.md` present, remote configured —
only the tag and the push remain. Set `Last reviewed` to the current date.

- [ ] **Step 6: Commit**

```bash
git add docs/references/deviations-and-findings.md docs/TODO.md \
        internal/controller/squall/CLAUDE.md
git commit -m "docs(ledger): audit the status column against the code (F6)"
```

---

## Release mechanics — owner-run, after every task above

Not a task; the owner decides when to publish.

1. `./scripts/pre-push-check.sh` — F4 fixed its self-inflicted failures; confirm it is clean.
2. **192 commits ahead of `main`, nothing ever pushed, 0 tags.** Push the branch, open the PR,
   then tag `v0.1.0`.
3. `CHANGELOG.md` exists but predates this work: D129-D133 and the four tasks above need an
   entry before the tag.
