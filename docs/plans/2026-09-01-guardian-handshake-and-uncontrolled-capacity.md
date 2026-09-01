# Guardian Handshake and Uncontrolled Capacity — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to
> implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the proxy the single, durable idleness sensor; delete every engine-specific and
host-dependent probe; and when the proxy cannot answer, alert on a metric and destroy the
capacity on a deadline instead of guessing.

**Architecture:** Three guardians with disjoint jobs and no overlapping measurement.
Guardian 1 (proxy request accounting, §6) is the only thing that measures idleness — its
verdict is made durable by persisting the last-request instant to `status`, which is what a
proxy rollout used to destroy. Guardian 2 (`Reaper`) does orphan GC only: it needs no
utilisation signal, because "no Model claims this run" is already proof. When guardian 1
reports incomplete evidence, nothing tries to re-measure the replica from outside; squall
publishes `squall_model_uncontrolled_seconds` and, past `spec.uncontrolledTimeout`, performs
the ordinary `1->0` flip.

**Tech Stack:** Go 1.26.6 (containerized — every command goes through `./scripts/dev.sh`),
controller-runtime, Prometheus client_golang, kubebuilder CRD markers.

---

## Global Constraints

- **The spec is authoritative.** `docs/specs/squall-spec-v0_18-RC.md`. Where this plan and the
  spec disagree, say so rather than silently picking one.
- **`0→1` fails open, `1→0` fails safe.** Wake may tolerate uncertainty; sleep must not.
- **Squall NEVER sends `force`** (F18, §5.2, AC13). Enforced by construction: `dstack.ApplyRequest`
  has no `Force` field.
- **All commands go through the wrapper.** `./scripts/dev.sh go ...`, `./scripts/dev.sh make test-unit`,
  `./scripts/dev.sh make test-envtest`, `./scripts/dev.sh make qa-lint` (the lint target is
  `qa-lint`, NOT `lint`). Never call bare `go`/`make`. Never set `DOCKER_BUILDKIT=0`.
- **`make test-unit` must never need a control plane.** envtest cases call `t.Skip` under `-short`.
- **Git rules for concurrent agents:** never `git add -A`; never `git reset`, `--amend`, or rebase
  past a commit you did not create; never `git checkout -- <file>` / `git restore` on a dirty
  tree. **Only add commits.** Stay inside the file tree this plan names.
- **CRD regeneration is two files plus a server-side apply.** After any `api/` change run
  `./scripts/dev.sh make manifests`, which rewrites BOTH
  `config/crd/bases/squall.ackstorm.ai_models.yaml` and
  `deploy/helm/squall/crds/squall.ackstorm.ai_models.yaml`. Helm never upgrades CRDs (D88) —
  deployment uses `kubectl apply --server-side --force-conflicts`.
- **Ledger numbering: the next free number is D129.** D128 is the current maximum in
  `docs/references/deviations-and-findings.md`. Never renumber, never delete an entry.

---

## Context: what was measured, and why this plan exists

Three live measurements on 2026-08-31/09-01 drive every decision below.

1. **The proxy's accounting is the only exact sensor, and it lived in RAM.** A
   `kubectl rollout restart` of squall-proxy wiped the activity map of a Ready Model. Every
   replica then answered "no key for this Model", `aggregateActivity` returned
   `ActivityEvidence{}`, `sleepDue` could never form a verdict, and the `1->0` flip became
   permanently unreachable. Measured cost: **2h21m of a $1.894/h GPU serving zero requests.**

2. **dstack's generic metrics are host-dependent, so they cannot be load-bearing.** dstack does
   expose an engine-agnostic series — `GET /api/project/{p}/metrics/job/{run}` yields
   `gpu_util_percent_gpu0` at a 10.0s interval, verified moving 0 → 58 → 58 → 0 across a real
   generation. But on a cgroup-v1 host `dstack-runner` refuses to start its collector, verbatim:
   `Metrics collector is not available err=get cgroup mount point: only cgroup v1 mounts found`,
   and the endpoint returns `{"metrics":[]}` forever. Observed on 1 of 2 Vast hosts. A
   safety-critical signal that is sometimes silently absent is not a safety-critical signal.

3. **Every alternative external probe measures a symptom, not requests.** `nvidia-smi` over SSH
   *does* work where dstack's collector does not (confirmed on the cgroup-v1 host: `0 %, 890 MiB`),
   but it is an instantaneous gauge: the GPU reads 0% between tokens and 0% with a request
   queued. vLLM's `request_success_total` is a real odometer but exists only for vLLM.

The conclusion this plan implements: **do not add a fourth half-working sensor. Fix where the
good sensor is stored, and make its absence loud and time-bounded.**

---

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `internal/controller/squall/phase.go` | `ActivityEvidence`, `sleepDue`, `unhealthyDue`, `Decide` | Add `AnyData`; add durable anchor to `sleepDue`; add `uncontrolledDue` + `Action.Uncontrolled` |
| `internal/controller/squall/activity.go` | `aggregateActivity` — pure, no I/O | `!sawData` stops meaning "incomplete" |
| `internal/controller/squall/finalizer.go` | `drainEvidenceClean` teardown gate | Explicit decision on the new `!sawData` shape |
| `api/squall/v1alpha1/model_types.go` | CRD schema | `spec.uncontrolledTimeout`, `status.lastRequestAt`, `status.uncontrolledSince` |
| `internal/controller/squall/model_controller.go` | Reconcile, status writes, metric calls | Persist the two new status fields; wire the collector |
| `internal/metrics/uncontrolled.go` | **NEW** — the alert half | Observed/declared gauge pair |
| `internal/controller/squall/reaper.go` | Guardian 2 | **Delete** the idle path; keep orphan GC |
| `internal/controller/squall/utilisation.go` | Engine probe table | **Delete entirely** |
| `cmd/controller/main.go` | Wiring | Drop `SSHUtilisation` + `controllerReplicaKeyLoader`; register the collector |
| `docs/references/deviations-and-findings.md` | Ledger | D129–D132 |

**Blocks (review boundary, per CLAUDE.md — review per block, not per task):**
- **Block A** = Tasks 1–3 (evidence semantics + durable anchor). Highest risk: touches the
  `1->0` decision and the finalizer's destructive gate.
- **Block B** = Tasks 4–6 (uncontrolled timeout + metric + wiring).
- **Block C** = Tasks 7–8 (deletion + ledger). Low risk, mostly subtraction.

Full gates (`test-unit`, `test-envtest`, `qa-lint`) run **once per block**, at the boundary.
Mutation sweep is **one pass at the end of each block**, not after each task.

---

## DECISION (owner, 2026-09-01) — the default deadline

`min(4 × scaleDownDelaySeconds + 15m, 2h)`. The formula scales the deadline with the idle
window the operator actually configured; the 2h cap bounds the worst case, so no Model can
bill unmeasured for longer than two hours whatever its idle window says.

| `scaleDownDelaySeconds` | `4×idle + 15m` | after the 2h cap |
|---|---|---|
| 120s (`ollama-tiny` fixture) | 23m | **23m** |
| 900s (15m, realistic production) | 75m | **75m** |
| 1575s (26m) | 120m | **120m** — the cap starts biting here |
| 3600s (1h) | 255m | **120m** |

**The cap applies to the DEFAULT ONLY, never to an explicit `spec.uncontrolledTimeout`.** It
has to: `0s` is the documented opt-out, and an absolute ceiling that overrode explicit values
would either break the opt-out or make it the one arbitrary exception. An operator who writes
`8h` has made a deliberate, visible choice, and `squall_model_uncontrolled_timeout_seconds`
publishes it so an alert can disagree.

---

## Task 1: `ActivityEvidence.AnyData` — separate "unreachable" from "nothing to report"

**Why this is first:** `aggregateActivity` currently collapses two different facts into one
`Complete: false`. "A replica did not answer" is genuine ignorance. "Every replica answered and
none had ever routed this Model" is a positive fact: because `ActivityTracker.Begin` inserts the
key BEFORE the upstream call, the absence of a key proves in-flight is zero there (D91). Only
the *timestamp* is missing, and Task 2 supplies it from `status`.

**Files:**
- Modify: `internal/controller/squall/phase.go` (the `ActivityEvidence` struct, ~line 53)
- Modify: `internal/controller/squall/activity.go` (the `!sawData` branch, ~line 131)
- Test: `internal/controller/squall/activity_gather_test.go`

**Interfaces:**
- Produces: `ActivityEvidence.AnyData bool`. Task 2 reads it in `sleepDue`; Task 3 reads it in
  `drainEvidenceClean`.
- Consumes: nothing from earlier tasks.

- [ ] **Step 1: Write the failing test**

In `internal/controller/squall/activity_gather_test.go`:

```go
// TestAggregate_NoReplicaHasTheKey_IsCompleteAndIdleWithoutData is the D91/D100-class
// distinction. Every replica answered, on time, unambiguously; none had ever routed
// this Model. That is NOT ignorance: ActivityTracker.Begin inserts the key BEFORE the
// upstream call, so a replica with no key is a replica with nothing in flight. What is
// missing is only WHEN the last request was, and status.lastRequestAt supplies that.
func TestAggregate_NoReplicaHasTheKey_IsCompleteAndIdleWithoutData(t *testing.T) {
	now := time.Now()
	ev := aggregateActivity(
		[]string{"10.0.0.1:8080", "10.0.0.2:8080"},
		[]ActivityQuery{
			{Addr: "10.0.0.1:8080", OK: true, NoData: true},
			{Addr: "10.0.0.2:8080", OK: true, NoData: true},
		}, now)

	if !ev.Complete {
		t.Fatal("every replica answered cleanly; this is a complete observation, not ignorance")
	}
	if !ev.AllIdle {
		t.Fatal("no replica holds a key for this Model, so nothing is in flight")
	}
	if ev.AnyData {
		t.Fatal("AnyData must be false: no replica contributed a timestamp")
	}
	if !ev.NewestLastRequestAt.IsZero() {
		t.Fatalf("no replica contributed a timestamp, got %v", ev.NewestLastRequestAt)
	}
}

// TestAggregate_UnreachableReplicaIsStillIncomplete guards the half that must NOT change.
// One silent replica is real ignorance and must keep blocking every destructive path.
func TestAggregate_UnreachableReplicaIsStillIncomplete(t *testing.T) {
	now := time.Now()
	ev := aggregateActivity(
		[]string{"10.0.0.1:8080", "10.0.0.2:8080"},
		[]ActivityQuery{
			{Addr: "10.0.0.1:8080", OK: true, NoData: true},
			{Addr: "10.0.0.2:8080", OK: false},
		}, now)

	if ev.Complete || ev.AllIdle || ev.AnyData {
		t.Fatalf("an unreachable replica must stay fully incomplete, got %+v", ev)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -run 'TestAggregate_' -count=1 -short
```
Expected: FAIL — `ev.AnyData` undefined (compile error).

- [ ] **Step 3: Add the field**

In `internal/controller/squall/phase.go`, inside `type ActivityEvidence struct`, immediately
after the `AllIdle` field:

```go
	// AnyData is meaningful only when Complete is true: at least one replica
	// held a key for this Model and therefore contributed a timestamp.
	//
	// It exists because "every replica answered, none had ever heard of this
	// Model" used to be indistinguishable from "a replica did not answer".
	// The two are opposites. The first is a COMPLETE observation whose only
	// missing piece is the instant of the last request — in-flight is provably
	// zero, because ActivityTracker.Begin inserts the key before the upstream
	// call. The second is ignorance and must keep blocking every 1->0 path.
	//
	// Collapsing them cost 2h21m of a $1.894/h GPU on 2026-08-31: a proxy
	// rollout emptied every activity map, evidence read as permanently
	// incomplete, and the sleep flip became unreachable.
	AnyData bool
```

- [ ] **Step 4: Change the `!sawData` branch**

In `internal/controller/squall/activity.go`, replace the block that currently reads:

```go
	if !sawData {
		// Every replica answered, none had ever routed to this Model. That
		// is the absence of evidence, not evidence of idleness.
		return ActivityEvidence{}
	}
```

with:

```go
	if !sawData {
		// Every replica answered, none had ever routed to this Model. This
		// is a COMPLETE observation of in-flight work — each replica proved
		// zero by having no key at all — with no timestamp attached. The
		// caller supplies the timestamp from status.lastRequestAt; see
		// AnyData's doc comment for why the previous reading (incomplete)
		// made the sleep flip permanently unreachable after a proxy rollout.
		return ActivityEvidence{Complete: true, AllIdle: true}
	}
```

Then, in the normal return at the end of the same function, add `AnyData: true`:

```go
	return ActivityEvidence{
		Complete:             true,
		AnyData:              true,
		AllIdle:              allIdle,
		NewestLastRequestAt:  newest,
		NewestLastSuccessAt:  newestSuccess,
		FailuresSinceSuccess: failures,
	}
```

- [ ] **Step 5: Run the whole package — this change has blast radius**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -count=1 -short
```

Expected: the two new tests PASS. **Some existing tests may now fail**, and that is the point of
running the whole package: `sleepDue`, `unhealthyDue` and `drainEvidenceClean` all branch on
`Complete`. Do NOT weaken any failing test to make it green. Record each failure verbatim in
the task report; Tasks 2 and 3 resolve them deliberately. If a test fails for a reason Tasks 2
and 3 do not cover, stop and report `DONE_WITH_CONCERNS`.

Reasoning already done for you, to check your work against:
- `unhealthyDue` — safe. With `AnyData` false, `NewestLastRequestAt` is zero, so
  `now.Sub(zero) > scaleDownDelay` returns false ("no current traffic"). Unchanged behaviour.
- `freshSuccess` — safe. Requires `!NewestLastSuccessAt.IsZero()`, still zero.
- `sleepDue` — **changes**. Task 2 owns it.
- `drainEvidenceClean` — **changes**. Task 3 owns it.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/squall/phase.go internal/controller/squall/activity.go \
        internal/controller/squall/activity_gather_test.go
git commit -m "feat(activity): tell 'nobody answered' apart from 'nobody had anything to say'"
```

---

## Task 2: Persist the last-request instant to `status`, and anchor `sleepDue` on it

**Files:**
- Modify: `api/squall/v1alpha1/model_types.go` (`ModelStatus`)
- Modify: `internal/controller/squall/phase.go` (`sleepDue`, and its call site in `Decide`)
- Modify: `internal/controller/squall/model_controller.go` (status write)
- Test: `internal/controller/squall/phase_test.go`, `internal/controller/squall/model_controller_unit_test.go`

**Interfaces:**
- Consumes: `ActivityEvidence.AnyData` (Task 1).
- Produces: `ModelStatus.LastRequestAt *metav1.Time`; new `sleepDue` signature
  `sleepDue(activity *ActivityEvidence, durableLastRequestAt *metav1.Time, scaleDownDelaySeconds int32, now time.Time) bool`.

- [ ] **Step 1: Add the status field**

In `api/squall/v1alpha1/model_types.go`, inside `type ModelStatus struct`, after `WakeStartedAt`:

```go
	// LastRequestAt is the newest instant any squall-proxy replica reported
	// forwarding a request for this Model — the same number §6 already used,
	// but written down instead of held in proxy memory.
	//
	// It exists because that memory is destroyed by an ordinary rollout.
	// MEASURED 2026-08-31: after `kubectl rollout restart` of squall-proxy,
	// every replica reported "no key for this Model", the idle evidence read
	// as permanently incomplete, and the 1->0 flip became unreachable for
	// 2h21m at $1.894/h. The engine could not help — this is squall's own
	// bookkeeping, and it belongs in squall's own durable object.
	//
	// It only ever moves FORWARD. A stale or absent report must never be able
	// to rewind the clock and manufacture an idle window that did not happen.
	// +optional
	LastRequestAt *metav1.Time `json:"lastRequestAt,omitempty"`
```

- [ ] **Step 2: Regenerate the CRD and deepcopy**

```bash
./scripts/dev.sh make manifests generate
git diff --stat config/crd/bases/squall.ackstorm.ai_models.yaml \
                deploy/helm/squall/crds/squall.ackstorm.ai_models.yaml \
                api/squall/v1alpha1/zz_generated.deepcopy.go
```
Expected: all three files changed, `lastRequestAt` present in both CRD copies.

- [ ] **Step 3: Write the failing test**

In `internal/controller/squall/phase_test.go`:

```go
// TestSleepDue_UsesTheDurableAnchorWhenNoReplicaHasData is the D100-class fix at the
// decision layer. Every proxy replica was restarted, so none holds a key for this
// Model; the evidence is complete and idle but carries no timestamp. status.lastRequestAt
// is the timestamp, and without it the flip is unreachable forever.
func TestSleepDue_UsesTheDurableAnchorWhenNoReplicaHasData(t *testing.T) {
	now := time.Now()
	ev := &ActivityEvidence{Complete: true, AllIdle: true, AnyData: false}

	stale := metav1.NewTime(now.Add(-10 * time.Minute))
	if !sleepDue(ev, &stale, 120, now) {
		t.Fatal("10 minutes idle against a 2 minute delay must sleep; the durable anchor was ignored")
	}

	fresh := metav1.NewTime(now.Add(-30 * time.Second))
	if sleepDue(ev, &fresh, 120, now) {
		t.Fatal("30 seconds idle against a 2 minute delay must NOT sleep")
	}

	if sleepDue(ev, nil, 120, now) {
		t.Fatal("no live data and no durable anchor is not evidence of idleness; must not sleep")
	}
}

// TestSleepDue_LiveDataWinsOverTheDurableAnchor keeps the fallback a fallback. A replica
// that IS reporting is fresher than anything status can hold.
func TestSleepDue_LiveDataWinsOverTheDurableAnchor(t *testing.T) {
	now := time.Now()
	ev := &ActivityEvidence{
		Complete: true, AllIdle: true, AnyData: true,
		NewestLastRequestAt: now.Add(-10 * time.Second),
	}
	ancient := metav1.NewTime(now.Add(-3 * time.Hour))
	if sleepDue(ev, &ancient, 120, now) {
		t.Fatal("a request 10s ago must block sleep; the stale durable anchor was preferred")
	}
}

// TestSleepDue_IncompleteEvidenceNeverSleeps is the invariant. A replica that did not
// answer is ignorance, and no durable timestamp may paper over it.
func TestSleepDue_IncompleteEvidenceNeverSleeps(t *testing.T) {
	now := time.Now()
	ancient := metav1.NewTime(now.Add(-3 * time.Hour))
	if sleepDue(&ActivityEvidence{Complete: false}, &ancient, 120, now) {
		t.Fatal("1->0 fails safe: incomplete evidence must never sleep, however old the anchor")
	}
}
```

- [ ] **Step 4: Run it and watch it fail**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -run 'TestSleepDue_' -count=1 -short
```
Expected: FAIL — `sleepDue` takes 3 arguments, not 4.

- [ ] **Step 5: Change `sleepDue`**

In `internal/controller/squall/phase.go`, replace the whole function:

```go
// sleepDue answers §6's idle question. The anchor is the newest live report when any
// replica has one, and status.lastRequestAt when none does.
//
// The two-source anchor is the whole D100-class fix. Live reports are strictly better
// evidence and are always preferred. But they live in proxy memory, and a rollout
// empties every one of them at once; the durable copy is what keeps the flip reachable
// across that. Incomplete evidence still never sleeps: this widens the anchor, never
// the confidence.
func sleepDue(activity *ActivityEvidence, durableLastRequestAt *metav1.Time, scaleDownDelaySeconds int32, now time.Time) bool {
	if activity == nil || !activity.Complete || !activity.AllIdle {
		return false
	}
	anchor := activity.NewestLastRequestAt
	if !activity.AnyData {
		// Complete and idle, but nobody carried a timestamp. status holds it.
		if durableLastRequestAt == nil || durableLastRequestAt.IsZero() {
			// Never observed a request at all. Not idleness — the absence of
			// any history whatsoever. Task 4's uncontrolled deadline owns this.
			return false
		}
		anchor = durableLastRequestAt.Time
	}
	return now.Sub(anchor) > time.Duration(scaleDownDelaySeconds)*time.Second
}
```

- [ ] **Step 6: Update the call site in `Decide`**

In the same file, the sleep check inside `Decide` currently reads
`sleepDue(observed.Activity, spec.ScaleDownDelaySeconds, now)`. It needs the prior status.
`Decide` already receives `prior` — pass `prior.LastRequestAt`:

```go
	if spec.MinReplicas == 0 && sleepDue(observed.Activity, prior.LastRequestAt, spec.ScaleDownDelaySeconds, now) {
```

No plumbing is needed: `Decide`'s `prior` parameter is `squallv1alpha1.ModelStatus` itself
(`phase.go:153`), so `prior.LastRequestAt` is the field Task 2 Step 1 just added. Verified.

- [ ] **Step 7: Persist it, monotonically**

In `internal/controller/squall/model_controller.go`, in the status-reconciliation block that
already sets `model.Status.DeploymentNum` from `observed.Run` (search for
`LIVE-2: reconcile status.runID/deploymentNum/serviceURL`), add:

```go
	// Persist §6's anchor so a proxy rollout cannot destroy it. FORWARD ONLY:
	// a replica that restarted reports nothing, and nothing must never be able
	// to rewind this clock into manufacturing an idle window.
	if observed.Activity != nil && observed.Activity.AnyData && !observed.Activity.NewestLastRequestAt.IsZero() {
		if model.Status.LastRequestAt == nil || observed.Activity.NewestLastRequestAt.After(model.Status.LastRequestAt.Time) {
			t := metav1.NewTime(observed.Activity.NewestLastRequestAt)
			model.Status.LastRequestAt = &t
		}
	}
```

- [ ] **Step 8: Run the package**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -count=1 -short
```
Expected: PASS, including every test Task 1 Step 5 left red for `sleepDue`.

- [ ] **Step 9: Commit**

```bash
git add api/squall/v1alpha1/model_types.go api/squall/v1alpha1/zz_generated.deepcopy.go \
        config/crd/bases/squall.ackstorm.ai_models.yaml \
        deploy/helm/squall/crds/squall.ackstorm.ai_models.yaml \
        internal/controller/squall/phase.go internal/controller/squall/phase_test.go \
        internal/controller/squall/model_controller.go
git commit -m "feat(sleep): anchor the idle window on durable status, not proxy memory"
```

---

## Task 3: Decide, explicitly and with a test, what the finalizer's drain gate does now

**Why this is its own task:** `drainEvidenceClean` is `activity != nil && activity.Complete &&
activity.AllIdle`. Task 1 changed the `!sawData` shape from `{}` to `{Complete: true, AllIdle:
true}`, so this gate now **passes** where it previously blocked. That is a behaviour change on a
destructive path and must not happen by accident.

**The ruling to implement:** the gate should pass. D91's argument is exactly this — a proxy
replica with no key for a Model has nothing in flight for it, because the key is inserted before
the upstream call. Blocking teardown on the absence of a key is what wedged the finalizer during
rollouts. But it must be a *named* decision with a test that fails if someone reverts it.

**Files:**
- Modify: `internal/controller/squall/finalizer.go` (doc comment only — no logic change)
- Test: `internal/controller/squall/finalizer_test.go` (create if absent)

- [ ] **Step 1: Write the test that pins the ruling**

```go
// TestDrainEvidenceClean_NoKeyAnywhereIsCleanDrain pins a deliberate ruling. After a
// proxy rollout no replica holds a key for this Model. That is not ignorance: the key
// is inserted BEFORE the upstream call, so no key means nothing in flight, and teardown
// is safe. Treating it as ignorance wedged the finalizer for the whole of a rollout.
func TestDrainEvidenceClean_NoKeyAnywhereIsCleanDrain(t *testing.T) {
	if !drainEvidenceClean(&ActivityEvidence{Complete: true, AllIdle: true, AnyData: false}) {
		t.Fatal("no replica holds a key: nothing is in flight and the drain is clean")
	}
}

// TestDrainEvidenceClean_SilentReplicaBlocksTeardown is the half that must never move.
func TestDrainEvidenceClean_SilentReplicaBlocksTeardown(t *testing.T) {
	if drainEvidenceClean(&ActivityEvidence{Complete: false}) {
		t.Fatal("a replica that did not answer may still be streaming; teardown must block")
	}
	if drainEvidenceClean(nil) {
		t.Fatal("no evidence at all must block teardown")
	}
	if drainEvidenceClean(&ActivityEvidence{Complete: true, AnyData: true, AllIdle: false}) {
		t.Fatal("a replica reporting in-flight work must block teardown")
	}
}
```

- [ ] **Step 2: Run**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -run 'TestDrainEvidenceClean' -count=1 -short
```
Expected: PASS with no production change — Task 1 already produced this behaviour. The tests
exist to make the ruling explicit and to go red if it is silently reverted.

- [ ] **Step 3: Record the ruling where the code is**

Extend the doc comment above `drainEvidenceClean` in `internal/controller/squall/finalizer.go`:

```go
// drainEvidenceClean gates the destructive half of teardown: it must be true before
// squall stops a run that may still be serving.
//
// Complete && AllIdle covers TWO shapes on purpose. Replicas that reported in-flight
// zero, and replicas that reported no key for this Model at all. The second is not
// ignorance — ActivityTracker.Begin inserts the key BEFORE the upstream call, so a
// replica with no key has nothing in flight for this Model. Reading it as ignorance
// wedged this gate for the duration of every proxy rollout. A replica that does not
// ANSWER is still ignorance and still blocks. See TestDrainEvidenceClean_*.
```

- [ ] **Step 4: Commit**

```bash
git add internal/controller/squall/finalizer.go internal/controller/squall/finalizer_test.go
git commit -m "test(finalizer): pin the drain-gate ruling for a proxy with no key"
```

- [ ] **Step 5: BLOCK A GATE — run everything once, then the mutation sweep**

```bash
./scripts/dev.sh make test-unit
./scripts/dev.sh make test-envtest
./scripts/dev.sh make qa-lint
```

Then the mutation sweep for Block A. For each mutation: apply it, run only the named test, confirm
RED, revert. A mutation that leaves the suite green is a finding, not a formality.

| # | File | Mutation | Test that MUST go red |
|---|---|---|---|
| M1 | `activity.go` | `!sawData` branch back to `return ActivityEvidence{}` | `TestSleepDue_UsesTheDurableAnchorWhenNoReplicaHasData` (via `Decide`) and `TestAggregate_NoReplicaHasTheKey_...` |
| M2 | `activity.go` | drop `AnyData: true` from the normal return | `TestSleepDue_LiveDataWinsOverTheDurableAnchor` |
| M3 | `phase.go` | in `sleepDue`, drop the `durableLastRequestAt == nil` guard | `TestSleepDue_UsesTheDurableAnchorWhenNoReplicaHasData` (the nil case) |
| M4 | `phase.go` | in `sleepDue`, use the durable anchor unconditionally (ignore `AnyData`) | `TestSleepDue_LiveDataWinsOverTheDurableAnchor` |
| M5 | `model_controller.go` | drop the `.After(...)` monotonic guard | needs a test — write `TestReconcile_LastRequestAtNeverGoesBackwards` if none goes red |
| M6 | `finalizer.go` | `drainEvidenceClean` requires `AnyData` too | `TestDrainEvidenceClean_NoKeyAnywhereIsCleanDrain` |

Report every mutation whose test did NOT go red.

---

## Task 4: `spec.uncontrolledTimeout` and `status.uncontrolledSince`

**Files:**
- Modify: `api/squall/v1alpha1/model_types.go`
- Modify: `internal/controller/squall/phase.go` (`uncontrolledDue`, `Action.Uncontrolled`, `Decide`)
- Test: `internal/controller/squall/phase_test.go`

**Interfaces:**
- Consumes: `ActivityEvidence.Complete` (Task 1).
- Produces: `uncontrolledTimeoutFor(spec) time.Duration`; `Action.Uncontrolled bool`;
  `ModelStatus.UncontrolledSince *metav1.Time`. Task 5 reads all three.

- [ ] **Step 1: Add the spec field**

```go
	// UncontrolledTimeout bounds how long capacity may stay up while squall's
	// own idle accounting is unable to reach a verdict.
	//
	// squall has exactly one sensor for "is anyone using this": squall-proxy's
	// request accounting. Every alternative was measured and rejected --
	// dstack's job metrics are silently absent on cgroup-v1 hosts (1 of 2 Vast
	// hosts sampled, verbatim: "only cgroup v1 mounts found"), nvidia-smi and
	// gpu_util_percent are instantaneous gauges that read 0% between tokens and
	// 0% with a request queued, and vLLM's own counters exist only for vLLM.
	// So when the sensor cannot answer, squall does not guess with a worse one.
	// It publishes squall_model_uncontrolled_seconds and, after this long,
	// performs the ordinary 1->0 flip anyway.
	//
	// Nil takes the default: min(4x ScaleDownDelaySeconds + 15m, 2h). The
	// formula scales with the idle window actually configured; the cap bounds
	// the worst case, because two hours of paying for capacity nobody can
	// measure is already more than enough to notice.
	//
	// An explicit value is honoured as written and is NOT capped -- an
	// operator who writes 8h has chosen it. "0s" is the opt-out: alert
	// forever, never act. Both are published as
	// squall_model_uncontrolled_timeout_seconds so an alert can disagree.
	// +optional
	UncontrolledTimeout *metav1.Duration `json:"uncontrolledTimeout,omitempty"`
```

And the status field, next to `LastRequestAt`:

```go
	// UncontrolledSince is when squall last LOST the ability to judge idleness
	// for this Model -- the first pass where the run was up and the activity
	// evidence came back incomplete. Cleared the moment evidence is complete
	// again, and on every sleep.
	//
	// It is in status rather than controller memory for the same reason
	// LastRequestAt is: a controller restart must not silently reset the
	// deadline and start paying again from zero.
	// +optional
	UncontrolledSince *metav1.Time `json:"uncontrolledSince,omitempty"`
```

- [ ] **Step 2: Regenerate**

```bash
./scripts/dev.sh make manifests generate
```

- [ ] **Step 3: Write the failing test**

```go
func TestUncontrolledTimeoutFor_Default(t *testing.T) {
	// min(4 x scaleDownDelay + 15m, 2h).
	for _, tc := range []struct {
		name  string
		delay int32
		want  time.Duration
	}{
		{"fixture, well under the cap", 120, 23 * time.Minute},
		{"realistic production", 900, 75 * time.Minute},
		{"exactly at the cap", 1575, 2 * time.Hour},
		{"formula would give 4h15m; the cap bites", 3600, 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := uncontrolledTimeoutFor(squallv1alpha1.ModelSpec{ScaleDownDelaySeconds: tc.delay})
			if got != tc.want {
				t.Fatalf("scaleDownDelay=%ds: got %v want %v", tc.delay, got, tc.want)
			}
		})
	}
}

func TestUncontrolledTimeoutFor_ExplicitZeroIsOptOut(t *testing.T) {
	zero := metav1.Duration{Duration: 0}
	if got := uncontrolledTimeoutFor(squallv1alpha1.ModelSpec{
		ScaleDownDelaySeconds: 120, UncontrolledTimeout: &zero,
	}); got != 0 {
		t.Fatalf("explicit 0s is the opt-out, got %v", got)
	}
}

// TestUncontrolledTimeoutFor_ExplicitValueIsNotCapped pins the other half of the cap
// ruling. The 2h ceiling bounds the DEFAULT; it must not silently rewrite a number an
// operator typed, or the "0s" opt-out becomes an unexplainable special case.
func TestUncontrolledTimeoutFor_ExplicitValueIsNotCapped(t *testing.T) {
	eight := metav1.Duration{Duration: 8 * time.Hour}
	if got := uncontrolledTimeoutFor(squallv1alpha1.ModelSpec{
		ScaleDownDelaySeconds: 120, UncontrolledTimeout: &eight,
	}); got != 8*time.Hour {
		t.Fatalf("an explicit override must be honoured as written, got %v", got)
	}
}

// TestUncontrolledDue is the deadline itself.
func TestUncontrolledDue(t *testing.T) {
	now := time.Now()
	old := metav1.NewTime(now.Add(-30 * time.Minute))
	recent := metav1.NewTime(now.Add(-5 * time.Minute))

	if !uncontrolledDue(&old, 23*time.Minute, now) {
		t.Fatal("30 minutes uncontrolled against a 23 minute deadline must fire")
	}
	if uncontrolledDue(&recent, 23*time.Minute, now) {
		t.Fatal("5 minutes uncontrolled must not fire")
	}
	if uncontrolledDue(nil, 23*time.Minute, now) {
		t.Fatal("never lost control: must not fire")
	}
	if uncontrolledDue(&old, 0, now) {
		t.Fatal("timeout 0 is the opt-out and must NEVER fire, however long it has been")
	}
}
```

- [ ] **Step 4: Run and watch it fail**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -run 'TestUncontrolled' -count=1 -short
```
Expected: FAIL — undefined functions.

- [ ] **Step 5: Implement both functions in `phase.go`**

```go
// DefaultUncontrolledGrace is the constant term of the default deadline. It is added
// to 4x the configured idle window so the deadline is always comfortably clear of a
// healthy sleep, however short that window is configured.
const DefaultUncontrolledGrace = 15 * time.Minute

// MaxUncontrolledTimeout caps the DEFAULT deadline. Two hours of billing capacity that
// nobody can measure is already far past the point of noticing -- the incident this
// whole mechanism exists for ran 2h21m before a human looked. It deliberately does NOT
// cap an explicit spec.uncontrolledTimeout: see uncontrolledTimeoutFor.
const MaxUncontrolledTimeout = 2 * time.Hour

// uncontrolledTimeoutFor resolves spec.uncontrolledTimeout.
//
// An explicit value is returned exactly as written, including the "0s" opt-out. Only
// the computed default is capped -- a ceiling that overrode explicit values would have
// to make "0s" an arbitrary exception to stay usable, and a limit with one unexplainable
// hole is worse than no limit.
func uncontrolledTimeoutFor(spec squallv1alpha1.ModelSpec) time.Duration {
	if spec.UncontrolledTimeout != nil {
		return spec.UncontrolledTimeout.Duration
	}
	d := 4*time.Duration(spec.ScaleDownDelaySeconds)*time.Second + DefaultUncontrolledGrace
	if d > MaxUncontrolledTimeout {
		d = MaxUncontrolledTimeout
	}
	return d
}

// uncontrolledDue is the third and last flip: squall has been unable to judge idleness
// for longer than the deadline, so it stops paying rather than keep guessing.
//
// This is NOT a measurement of idleness -- squall has none here, that is the whole
// premise. It is a bound on how long an unmeasurable bill may run. Zero disables it.
func uncontrolledDue(since *metav1.Time, timeout time.Duration, now time.Time) bool {
	if timeout <= 0 || since == nil || since.IsZero() {
		return false
	}
	return now.Sub(since.Time) > timeout
}
```

- [ ] **Step 6: Add `Uncontrolled` to `Action` and the third flip to `Decide`**

In `Action`, beside `Unhealthy`:

```go
	// Uncontrolled marks a 1->0 flip taken WITHOUT evidence of idleness,
	// purely because squall has been unable to obtain any for longer than
	// spec.uncontrolledTimeout. The reconciler reports it distinctly: this
	// is the one flip that is not a judgement about the workload.
	Uncontrolled bool
```

In `Decide`, immediately AFTER the `unhealthyDue` block (so a Model that is idle or
unhealthy is reported as the more specific of the three):

```go
	// Third fail-safe flip, checked LAST. sleepDue and unhealthyDue are both
	// judgements about the workload; this one is an admission that squall has
	// none to make. It fires only when the evidence has been incomplete for
	// longer than the deadline -- which is exactly the state that cost 2h21m
	// of a $1.894/h GPU on 2026-08-31.
	if spec.MinReplicas == 0 && uncontrolledDue(prior.UncontrolledSince, uncontrolledTimeoutFor(spec), now) {
		return squallv1alpha1.ModelPhaseAsleep, Action{
			Apply:        true,
			Replicas:     0,
			Current:      observed.Run,
			Uncontrolled: true,
			At:           now,
		}
	}
```

`prior` is `squallv1alpha1.ModelStatus`, so `prior.UncontrolledSince` is already available
once Step 1 adds the field. No plumbing.

- [ ] **Step 7: Run**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -count=1 -short
```
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add api/squall/v1alpha1/ config/crd/bases/ deploy/helm/squall/crds/ \
        internal/controller/squall/phase.go internal/controller/squall/phase_test.go
git commit -m "feat(phase): bound how long unmeasurable capacity may keep billing"
```

---

## Task 5: Maintain `status.uncontrolledSince`, and log the flip distinctly

**Files:**
- Modify: `internal/controller/squall/model_controller.go`
- Test: `internal/controller/squall/model_controller_unit_test.go`

- [ ] **Step 1: Write the failing test**

```go
// TestReconcile_UncontrolledSince_SetOnIncompleteClearedOnComplete is the handshake:
// guardian 2 stands down for exactly as long as guardian 1 is answering.
func TestReconcile_UncontrolledSince_SetOnIncompleteClearedOnComplete(t *testing.T) {
	// Pass 1: run up, evidence incomplete -> stamp set.
	// Pass 2: same run, evidence complete -> stamp cleared.
	// Pass 3: incomplete again -> stamp set to the NEW time, not the old one.
	//
	// Build the three passes with the package's existing reconcile fixture
	// (see TestReconcile_* neighbours for the harness), asserting on
	// model.Status.UncontrolledSince after each.
	t.Skip("replace this skip with the three passes using the file's existing fixture")
}
```

The implementer must write the three passes against the harness already used by the
neighbouring `TestReconcile_*` tests in this file. Do not invent a new fixture; do not leave
the `t.Skip` in place — a skipped test is a vacuous test.

- [ ] **Step 2: Implement the maintenance**

In `model_controller.go`, in the same status block as Task 2 Step 7:

```go
	// The guardian handshake. While the proxy can answer, guardian 2 has nothing
	// to say: the stamp stays clear and the deadline never starts. The moment it
	// cannot, the clock starts -- and it is written down, so a controller restart
	// cannot silently rewind it.
	switch {
	case observed.Run == nil || observed.Run.Replicas == 0:
		model.Status.UncontrolledSince = nil // nothing is billing; nothing to bound.
	case observed.Activity != nil && observed.Activity.Complete:
		model.Status.UncontrolledSince = nil // guardian 1 is in control.
	case model.Status.UncontrolledSince == nil:
		t := metav1.NewTime(now)
		model.Status.UncontrolledSince = &t
	}
```

- [ ] **Step 3: Report the flip distinctly**

At the `action.Apply && action.Replicas == 0` site that already distinguishes the unhealthy
flip, add the third case. Log at INFO with the money in it, and set the Model Condition:

```go
	if action.Uncontrolled {
		logger.Info("SLEEPING UNMEASURABLE CAPACITY: squall has been unable to judge idleness "+
			"for longer than spec.uncontrolledTimeout, so it is no longer paying to find out",
			"model", model.Name,
			"uncontrolledSince", model.Status.UncontrolledSince,
			"timeout", uncontrolledTimeoutFor(model.Spec),
			"pricePerHour", observedPerHour)
	}
```

- [ ] **Step 4: Run**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -count=1 -short
```

- [ ] **Step 5: Commit**

```bash
git add internal/controller/squall/model_controller.go \
        internal/controller/squall/model_controller_unit_test.go
git commit -m "feat(controller): track when squall loses the ability to judge idleness"
```

---

## Task 6: The alert — `squall_model_uncontrolled_seconds`

Mirror `internal/metrics/model_age.go` exactly: same `modelKey`, same observed/declared pair,
same `Observe`/`Forget`/`Describe`/`Collect` shape, same `clock.Clock` injection. Read that file
first and follow it; do not invent a different collector shape.

**Files:**
- Create: `internal/metrics/uncontrolled.go`
- Create: `internal/metrics/uncontrolled_test.go`
- Modify: `internal/controller/squall/model_controller.go` (call `Observe`/`Forget`)
- Modify: `cmd/controller/main.go` (construct + `MustRegister` + inject)

- [ ] **Step 1: Write the collector**

Two gauges, both labelled `{namespace, name}`:

```go
	// squall_model_uncontrolled_seconds -- how long squall has been unable to
	// judge whether this Model's capacity is in use. Zero while in control.
	// This is the "instancias sin control" alarm: pair it with
	// squall_model_price_per_hour to express the exposure in money.
	//
	//   squall_model_uncontrolled_seconds > 0
	//     * on(namespace,name) group_left squall_model_price_per_hour
	//
	// squall_model_uncontrolled_timeout_seconds -- the declared deadline after
	// which squall performs the 1->0 flip anyway. Zero means the operator opted
	// out and has chosen to pay indefinitely; alert harder on that one.
```

Emit the declared gauge for every observed Model (including the opted-out zero, so the
opt-out is visible in the metric rather than being an absence).

- [ ] **Step 2: Test it**

Copy the structure of `internal/metrics/model_age_test.go`. Cover: zero while in control; grows
while uncontrolled; declared gauge present and zero when opted out; `Forget` removes both series.

- [ ] **Step 3: Wire it**

In `cmd/controller/main.go`, beside the existing collectors:

```go
	// clock here is github.com/ackstorm/squall/internal/clock, NOT k8s.io/utils/clock
	// -- mirror NewModelAgeCollector, which nil-guards to clock.RealClock{}.
	uncontrolledMetrics := metrics.NewUncontrolledCollector(clock.RealClock{})
	ctrlmetrics.Registry.MustRegister(ageMetrics, priceMetrics, uncontrolledMetrics)
```

and pass it into the reconciler as `UncontrolledMetrics`, called from `recordMetrics` beside
`AgeMetrics.Observe` / `PriceMetrics.Observe`.

- [ ] **Step 4: Run and commit**

```bash
./scripts/dev.sh go test ./internal/metrics/ ./internal/controller/squall/ -count=1 -short
git add internal/metrics/uncontrolled.go internal/metrics/uncontrolled_test.go \
        internal/controller/squall/model_controller.go cmd/controller/main.go
git commit -m "feat(metrics): publish uncontrolled capacity, in seconds and in money"
```

- [ ] **Step 5: BLOCK B GATE**

```bash
./scripts/dev.sh make test-unit
./scripts/dev.sh make test-envtest
./scripts/dev.sh make qa-lint
```

Mutation sweep for Block B:

| # | File | Mutation | Test that MUST go red |
|---|---|---|---|
| M7 | `phase.go` | `uncontrolledDue` drops the `timeout <= 0` guard | `TestUncontrolledDue` (opt-out case) |
| M8 | `phase.go` | `uncontrolledTimeoutFor` ignores the explicit override | `TestUncontrolledTimeoutFor_ExplicitZeroIsOptOut` |
| M8b | `phase.go` | drop the `d > MaxUncontrolledTimeout` cap | `TestUncontrolledTimeoutFor_Default` (the 3600s case) |
| M8c | `phase.go` | apply the cap to the explicit override too | `TestUncontrolledTimeoutFor_ExplicitValueIsNotCapped` |
| M9 | `model_controller.go` | the `Complete` case no longer clears the stamp | `TestReconcile_UncontrolledSince_...` |
| M10 | `model_controller.go` | the stamp is overwritten on every incomplete pass | `TestReconcile_UncontrolledSince_...` (pass 3) |
| M11 | `uncontrolled.go` | `Collect` skips the declared gauge when timeout is 0 | the opt-out visibility test |

---

## Task 7: Delete the engine probes and the reaper's idle path

This is subtraction. The reaper keeps orphan GC — which needs no utilisation signal at all,
because "no Model claims this run, and it carries squall's own UID stamp" is already proof.

**Files:**
- Delete: `internal/controller/squall/utilisation.go`
- Modify: `internal/controller/squall/reaper.go`
- Modify: `internal/controller/squall/reaper_test.go`
- Modify: `cmd/controller/main.go`

- [ ] **Step 1: Start from a clean base**

`internal/controller/squall/utilisation.go`, `reaper.go` and `reaper_test.go` currently carry
**uncommitted** work-in-progress (a monotonic-odometer rework). It is superseded by this plan.
Do NOT `git checkout` or `git restore` — the tree may hold other agents' files. Delete forward:

```bash
git rm internal/controller/squall/utilisation.go
```

- [ ] **Step 2: Strip the reaper**

Remove from `internal/controller/squall/reaper.go`:
- the `Utilisation`, `IdleLimit`, `idleSince` and `lastWork` fields on `Reaper`
- the `idleLimit()` helper and `DefaultIdleCapacityLimit`
- the whole `reapIfIdle` method
- in `Sweep`, the owned-run branch collapses back to `continue` — an owned run is simply not
  an orphan, and nothing else in this file has an opinion about it

Replace the type comment's idle paragraphs with:

```go
// The reaper does NOT judge whether owned capacity is in use. It used to try,
// through engine-specific counters read over an SSH tunnel, and the attempt was
// deleted rather than fixed: every external signal available was either
// engine-specific (vLLM's request counters), host-dependent (dstack's job
// metrics, silently empty on cgroup-v1 hosts) or an instantaneous gauge that
// reads zero between tokens (nvidia-smi, gpu_util_percent). squall has exactly
// one exact sensor for "is anyone using this" -- squall-proxy's own accounting --
// and when that cannot answer, the answer is spec.uncontrolledTimeout and the
// squall_model_uncontrolled_seconds alarm, not a worse measurement.
```

- [ ] **Step 3: Remove the tests that no longer have a subject**

Delete from `reaper_test.go`: `stubUtilisation`, `TestReaper_ReapsOwnedButIdleCapacity`,
`TestReaper_NeverReapsOnUnreadableUtilisation`, `TestReaper_BusyCapacityResetsTheClock`,
`TestReaper_NilUtilisationDisablesIdleReaping`, `TestReaper_WorkBetweenSweepsResetsTheClock`,
and `idleRun()` if nothing else uses it. **Keep every orphan-GC test untouched.**

- [ ] **Step 4: Unwire main.go**

Remove the `Utilisation: &squallcontroller.SSHUtilisation{...}` block from the `Reaper`
construction and delete `controllerReplicaKeyLoader` entirely. Remove the imports its removal
orphans (`golang.org/x/crypto/ssh`, and `context`/`fmt` only if nothing else uses them).

- [ ] **Step 5: Fix the wrong ledger references while you are here**

`reaper.go` and `utilisation.go` cite **D100** for the sleep-unreachable incident. D100 is
already taken — it is the `forwardModel` bug. Most of those references leave with the deleted
code; any that survive in `reaper.go` must be renumbered to **D129** (allocated in Task 8).

- [ ] **Step 6: Build, test, commit**

```bash
./scripts/dev.sh go build ./... && ./scripts/dev.sh go test ./internal/controller/squall/ -count=1 -short
git add internal/controller/squall/reaper.go internal/controller/squall/reaper_test.go cmd/controller/main.go
git commit -m "refactor(reaper): stop guessing at utilisation, delete the engine probes"
```

---

## Task 8: Ledger

Append to `docs/references/deviations-and-findings.md`, matching the existing table format
exactly. **Never renumber, never delete an existing entry.**

- [ ] **Step 1: Write D129–D132**

- **D129** — The sleep flip was unreachable after any proxy rollout. Measured 2h21m at $1.894/h
  serving zero requests. Root cause: §6's anchor lived only in proxy RAM, and `aggregateActivity`
  read "no replica has a key" as incomplete evidence rather than as proof of zero in-flight.
  Fixed by `ActivityEvidence.AnyData` + `status.lastRequestAt`. **Note the collision:** earlier
  in-code comments in `reaper.go`/`utilisation.go` cited this as "D100", which was already taken
  by the `forwardModel` finding. Renumbered here; if any comment still says D100 for the sleep
  incident, it is wrong.
- **D130** — dstack's generic job metrics are silently absent on cgroup-v1 hosts. Verbatim from
  `dstack-runner` 0.21.2: `Metrics collector is not available err=get cgroup mount point: only
  cgroup v1 mounts found`. The server-side endpoint then returns `{"metrics":[]}` indefinitely.
  Observed on 1 of 2 Vast hosts sampled 2026-08-31. Where it DOES work it is good —
  `gpu_util_percent_gpu0` at a 10.0s interval, verified 0 → 58 → 58 → 0 across a real
  generation. `nvidia-smi` worked on the same cgroup-v1 host, so the failure is dstack's
  collector, not the GPU. **Decision: not used.** A safety-critical signal that is sometimes
  silently absent cannot be load-bearing.
- **D131** — Reconcile burst, observed and NOT reproduced. 620 reconciles in 2.5 minutes
  (~5/s), all emitting the same `model spec warning`, 19:26–19:27:50 on 2026-08-31, during a
  wake→sleep transition of `squall/ollama-tiny`. It terminated a live replica mid-probe. A
  controlled re-run of the same wake under a monitor showed **0 reconciles/10s** throughout.
  Cause unknown. Recorded so the next occurrence is recognised rather than re-discovered.
- **D132** — Design decision, not a defect: squall deliberately has ONE idleness sensor.
  Rationale and the rejected alternatives, as above. The second guardian does orphan GC (which
  needs no sensor) and bounds the unmeasurable case by time, not by a worse measurement.

- [ ] **Step 2: Commit**

```bash
git add docs/references/deviations-and-findings.md
git commit -m "docs(ledger): D129-D132, the one-sensor decision and what it rejected"
```

- [ ] **Step 3: BLOCK C GATE — full gates once**

```bash
./scripts/dev.sh make test-unit
./scripts/dev.sh make test-envtest
./scripts/dev.sh make qa-lint
```

---

## Live verification (owner-run, after all blocks are green)

Not a task — the owner drives this, on the cheap `ollama-tiny` fixture ($0.13–0.21/h).

1. Apply the CRD server-side (Helm never upgrades CRDs, D88):
   `kubectl apply --server-side --force-conflicts -f deploy/helm/squall/crds/squall.ackstorm.ai_models.yaml`
2. Rebuild, load, `rollout restart`, and **verify the image digests actually changed** (D89).
3. Wake `ollama-tiny` with one request. Confirm `status.lastRequestAt` appears.
4. **The D129 regression test:** `kubectl rollout restart deploy/proxy -n squall-system` while
   the Model is Ready, then confirm it still sleeps on schedule. Before this plan it never would.
5. **The alarm:** scale the proxy to 0 replicas, confirm `squall_model_uncontrolled_seconds`
   climbs on `/metrics`, and confirm the flip fires at `4×idle + 15m` (23m on this fixture).
