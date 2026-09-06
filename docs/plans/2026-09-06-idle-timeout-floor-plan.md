# `MinIdleTimeout` Floor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reject a `spec.idleTimeout` below one minute, so an on-demand Model can never be admitted in a state where it silently never wakes.

**Architecture:** One constant in `phase.go` beside the existing `MinHardStop`, one rejection in `ValidateWithWarnings` replacing the current `<= 0` check. No new files, no new patterns, no API change. Validation already runs on the wake path (`model_controller.go:890`) and reports through `Schedulable=False`; there is no admission webhook, so nothing changes about how Models are accepted by the API server.

**Tech Stack:** Go 1.26.6 via the containerized toolchain, controller-runtime, envtest, Ginkgo (e2e only).

**Design of record:** `docs/plans/2026-09-06-idle-timeout-floor-design.md`. Where this plan and that design disagree, the design wins — say so rather than silently picking one.

## Global Constraints

- **Never call bare `go` or `make` for build or test.** Everything goes through `./scripts/dev.sh` (e.g. `./scripts/dev.sh make test-unit`). The lint target is `qa-lint`, not `lint`.
- **`make test-unit` must never need a control plane.** Envtest cases skip under `-short`.
- **Never `git add -A`.** Add named paths only. Only add commits; never `git reset`, `--amend`, or rebase past a commit you did not create.
- **Before claiming a behaviour is covered, mutate the implementation and watch a test go red.** A mutation that leaves the suite green is a finding, not a formality.
- **The constant is exactly `time.Minute`.** Not 30s, not 90s. The design justifies this value against three separate contributors; changing it is a design decision, not an implementation one.
- **`TestDecide_SleepsARunThatNeverReachedReady` (`phase_test.go:884`) must behave identically before and after.** It guards a design decision reversed on live evidence (ledger D165). If it changes, stop and re-read D165 rather than adjusting the test.
- All output in English: code, comments, docs, commit messages.

---

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `internal/controller/squall/phase.go` | Pure decision logic and the package's tuning constants | Add `MinIdleTimeout` |
| `internal/controller/squall/model_validation.go` | Cross-field spec rules | Replace the `<= 0` idleTimeout branch |
| `internal/controller/squall/model_validation_test.go` | Validation table tests | Add floor cases |
| `internal/controller/squall/model_controller_sleep_envtest_test.go` | Sleep-flip envtest fixtures | Drop two sub-floor overrides |
| `internal/controller/squall/model_controller_schedulable_unit_test.go` | Schedulable-veto unit test | Drop one sub-floor override |
| `test/e2e/cluster/03-fixtures/model.yaml` | e2e cluster fixture Model | Raise `idleTimeout` |
| `test/e2e/e2e_test.go` | e2e loop Model and its sleep assertion | Raise `idleTimeout`, widen one `Eventually` |
| `api/squall/v1alpha1/model_types.go` | CRD field docs | Correct the now-false floor sentence |
| `README.md`, `CHANGELOG.md`, `docs/references/deviations-and-findings.md` | Operator-facing docs and the ledger | Record the floor |

**Not changed:** `internal/proxy/handler_test.go:771` carries `IdleTimeout: 2 * time.Second` on a proxy-side `ModelSnapshot`. The proxy never calls `ValidateWithWarnings`, so it is unaffected. Leave it alone.

---

### Task 1: The floor, and every fixture that sits under it

**Files:**
- Modify: `internal/controller/squall/phase.go` (add constant next to `MinHardStop`, currently line 310)
- Modify: `internal/controller/squall/model_validation.go:55-57`
- Test: `internal/controller/squall/model_validation_test.go`
- Modify: `internal/controller/squall/model_controller_sleep_envtest_test.go:42`, `:163`
- Modify: `internal/controller/squall/model_controller_schedulable_unit_test.go:259`
- Modify: `test/e2e/cluster/03-fixtures/model.yaml:60`
- Modify: `test/e2e/e2e_test.go` (loop model fixture, and the sleep `Eventually`)

**Interfaces:**
- Produces: `const MinIdleTimeout = time.Minute` in package `squall`, referenced by Task 2's documentation.
- Consumes: `ValidateWithWarnings(spec squallv1alpha1.ModelSpec) ([]string, error)`, unchanged signature.

**Why one task and not three:** splitting the rule from the fixtures would leave the suite red between commits — the fixtures below the floor fail the moment the rule lands. They are one unit of work.

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/squall/model_validation_test.go`:

```go
func TestValidate_RejectsIdleTimeoutBelowFloor(t *testing.T) {
	spec := exampleModelSpec()
	spec.IdleTimeout = metav1.Duration{Duration: 30 * time.Second}
	_, err := ValidateWithWarnings(spec)
	if err == nil {
		t.Fatal("ValidateWithWarnings() = nil for idleTimeout 30s; a value below " +
			"MinIdleTimeout can expire before the controller next evaluates the demand " +
			"annotation, leaving the Model permanently unwakeable with no error and no event")
	}
	if !strings.Contains(err.Error(), "idleTimeout") || !strings.Contains(err.Error(), MinIdleTimeout.String()) {
		t.Fatalf("error %q must name both the field and the floor, or an operator "+
			"cannot act on it", err)
	}
}

// TestValidate_AcceptsIdleTimeoutExactlyAtFloor is the non-vacuity anchor for
// the whole rule: it is the case that must turn red when MinIdleTimeout is
// mutated upward. A rule tested only from below passes for a floor of any size.
func TestValidate_AcceptsIdleTimeoutExactlyAtFloor(t *testing.T) {
	spec := exampleModelSpec()
	spec.IdleTimeout = metav1.Duration{Duration: MinIdleTimeout}
	if _, err := ValidateWithWarnings(spec); err != nil {
		t.Fatalf("ValidateWithWarnings() = %v; exactly MinIdleTimeout must be accepted — "+
			"the floor is inclusive", err)
	}
}

func TestValidate_AcceptsIdleTimeoutAboveFloor(t *testing.T) {
	spec := exampleModelSpec()
	spec.IdleTimeout = metav1.Duration{Duration: 5 * time.Minute}
	if _, err := ValidateWithWarnings(spec); err != nil {
		t.Fatalf("ValidateWithWarnings() = %v; the CRD default of 5m must validate", err)
	}
}
```

`model_validation_test.go` already imports `strings`, `time`, `metav1` and the API package — no import changes are needed.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -run 'TestValidate_.*IdleTimeout' -short -v
```

Expected: `TestValidate_RejectsIdleTimeoutBelowFloor` FAILS (30s is currently accepted) and the file does not compile until `MinIdleTimeout` exists — resolve by doing Step 3, then re-run.

- [ ] **Step 3: Add the constant**

In `internal/controller/squall/phase.go`, immediately after `const MinHardStop = time.Hour`:

```go
// MinIdleTimeout is the floor under spec.idleTimeout. It is not a tuning
// preference. idleTimeout is ALSO the demand annotation's TTL (hasDemand,
// model_controller.go), and the proxy stamps that annotation at RFC3339
// SECOND granularity, so a shorter window can expire before the controller
// next evaluates it — the Model then never wakes, with no error, no event
// and no condition (ledger D171; measured: idleTimeout 2s stayed Asleep
// indefinitely, 8s and 30s reached Ready in about two seconds).
//
// One minute clears all three known contributors at once: up to 1s lost to
// second-truncation, the 15s default SQUALL_IDLE_REQUEUE_INTERVAL, and cold
// starts measured in minutes on every real backend. It is a floor under a
// cliff, not a recommendation — the CRD default is 5m.
const MinIdleTimeout = time.Minute
```

- [ ] **Step 4: Replace the validation branch**

In `internal/controller/squall/model_validation.go`, replace these three lines:

```go
	if spec.IdleTimeout.Duration <= 0 {
		return nil, fmt.Errorf("idleTimeout must be > 0: it is also the demand annotation's TTL, so a zero expires demand the instant the proxy writes it and the Model can never wake")
	}
```

with:

```go
	if spec.IdleTimeout.Duration < MinIdleTimeout {
		return nil, fmt.Errorf("idleTimeout (%s) must be at least %s: it is also the "+
			"demand annotation's TTL, stamped at RFC3339 second granularity, so a shorter "+
			"window can expire before the controller next evaluates it and the Model then "+
			"never wakes, with no error and no event",
			spec.IdleTimeout.Duration, MinIdleTimeout)
	}
```

A zero is caught by the same comparison. Do not keep a separate zero branch: two messages for one rule drift apart.

- [ ] **Step 5: Run the validation tests to verify they pass**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -run 'TestValidate' -short -v
```

Expected: PASS, including the pre-existing `TestValidate_RejectsNonPositiveIdleTimeout` — it asserts only that an error is returned, not its text, so it still holds.

- [ ] **Step 6: Drop the three sub-floor Go fixtures**

These three set `IdleTimeout` to `time.Second` only so that "aged past idleTimeout" needed no fake clock. Their activity evidence already carries `LastRequestAt: time.Now().UTC().Add(-time.Hour)`, which is an hour past `exampleModelSpec()`'s own `IdleTimeout: 5m`. Deleting the override is therefore sufficient — no fake clock, no behaviour change to what the tests assert.

In `internal/controller/squall/model_controller_sleep_envtest_test.go`, delete line 42 entirely:

```go
	spec.IdleTimeout = metav1.Duration{Duration: time.Second} // small, so "aged past" needs no FakeClock/real sleep >1s
```

and delete line 163 entirely:

```go
	spec.IdleTimeout = metav1.Duration{Duration: time.Second}
```

In `internal/controller/squall/model_controller_schedulable_unit_test.go`, delete line 259:

```go
	spec.IdleTimeout = metav1.Duration{Duration: time.Second}
```

- [ ] **Step 7: Raise the two e2e fixtures**

In `test/e2e/cluster/03-fixtures/model.yaml`, line 60, change `idleTimeout: 2s` to:

```yaml
  idleTimeout: 5m
```

This Model is only ever asserted to *start* Asleep (`e2e_test.go:246`); it is never woken, so a longer window costs the suite nothing. It was the live instance of the D171 hazard still in the tree.

In `test/e2e/e2e_test.go`, in `loopModelYAML`, replace the `idleTimeout: 8s` line and the comment block above it (currently lines 75-86) with:

```go
  # 1m, the MinIdleTimeout floor (internal/controller/squall/phase.go).
  # idleTimeout is ALSO the demand annotation's TTL (hasDemand,
  # model_controller.go). The proxy stamps demand-since at RFC3339 SECOND
  # granularity and stops refreshing the moment the request commits — against
  # model-mock a request commits in ~20ms, so exactly one un-refreshed stamp
  # has to survive until the controller next reconciles. This fixture used to
  # carry 2s, which expired first and left the Model permanently Asleep
  # (D171); 8s fixed that empirically, and validation now enforces a floor
  # rather than relying on a fixture getting it right.
  idleTimeout: 1m
```

Then widen the sleep assertion, currently at `e2e_test.go:284-286`, so it can outlast the new window:

```go
		// idleTimeout: 1m in loopModelYAML — give the reconciler
		// (SQUALL_IDLE_REQUEUE_INTERVAL, see 02-operator/controller-patch.yaml)
		// comfortably longer than that to notice.
		Eventually(func(g Gomega) string {
			return getModelStatus(g, loopModelName).Phase
		}, 2*time.Minute, time.Second).Should(Equal("Asleep"))
```

This adds roughly a minute to the e2e suite's wall clock. That is the accepted cost of the floor; do not lower `MinIdleTimeout` to avoid it.

- [ ] **Step 8: Run the full non-cluster gates**

```bash
./scripts/dev.sh make test-unit
./scripts/dev.sh make test-envtest
./scripts/dev.sh make qa-lint
```

Expected: all green. If `TestDecide_SleepsARunThatNeverReachedReady` fails, **stop** — that is D165's guard and this change must not touch it.

- [ ] **Step 9: Prove the tests are not vacuous**

Temporarily change the constant in `phase.go` to `const MinIdleTimeout = 2 * time.Minute`, then:

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -run 'TestValidate_AcceptsIdleTimeoutExactlyAtFloor' -short -v
```

Expected: **FAIL**. The "exactly at the floor" case is the anchor — if it stays green under a raised floor, the test proves nothing and must be fixed before continuing.

Then revert the constant to `time.Minute` and re-run to confirm green:

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -run 'TestValidate' -short -v
```

- [ ] **Step 10: Commit**

```bash
git add internal/controller/squall/phase.go \
        internal/controller/squall/model_validation.go \
        internal/controller/squall/model_validation_test.go \
        internal/controller/squall/model_controller_sleep_envtest_test.go \
        internal/controller/squall/model_controller_schedulable_unit_test.go \
        test/e2e/cluster/03-fixtures/model.yaml \
        test/e2e/e2e_test.go
git commit -m "fix: enforce a one-minute floor under spec.idleTimeout

idleTimeout is also the demand annotation's TTL, so a legal but too-short
value leaves an on-demand Model permanently unwakeable with no error, no
event and no condition. Validation rejected only a literal zero; the cliff
sits well above zero (D171: 2s stayed Asleep, 8s and 30s reached Ready).

MinIdleTimeout follows MinHardStop's existing shape and reports through
Schedulable=False on the wake path. There is no admission webhook, so this
cannot refuse a kubectl apply or fail a Flux sync.

test/e2e/cluster/03-fixtures/model.yaml carried the live D171 shape at
idleTimeout: 2s — only loopModelYAML was raised during the v0.1.7 release."
```

---

### Task 2: Correct every document that says the floor is zero

**Files:**
- Modify: `api/squall/v1alpha1/model_types.go` (the `IdleTimeout` doc comment)
- Modify: `config/crd/bases/squall.ackstorm.ai_models.yaml`, `deploy/helm/squall/crds/squall.ackstorm.ai_models.yaml` (regenerated, not hand-edited)
- Modify: `README.md` (the `Choosing idleTimeout` section)
- Modify: `CHANGELOG.md`
- Modify: `docs/references/deviations-and-findings.md` (D171's "Still owed" line)
- Modify: `test/e2e/cluster/helm-values.yaml:46` (stale comment)

**Interfaces:**
- Consumes: `MinIdleTimeout` from Task 1. This task is documentation only and must not change behaviour.

- [ ] **Step 1: Correct the CRD field comment**

In `api/squall/v1alpha1/model_types.go`, the `IdleTimeout` doc comment contains this sentence, which is now false:

```go
	// The floor is NOT zero, though zero is all validation can safely
	// reject.
```

Replace that sentence with:

```go
	// The floor is one minute and validation enforces it
	// (MinIdleTimeout, internal/controller/squall/phase.go): anything
	// shorter is refused with Schedulable=False rather than admitted and
	// left silently unwakeable.
```

Leave the rest of the comment — the measurements, the requeue-interval explanation and the refresh caveat — exactly as written. They are still true and still the reason the floor exists.

- [ ] **Step 2: Regenerate the CRDs**

```bash
./scripts/dev.sh make helm-sync
```

Never hand-edit the generated CRD YAML. Expect both `config/crd/bases/` and `deploy/helm/squall/crds/` to change.

- [ ] **Step 3: Update the README**

In `README.md`, line 319 is the heading `#### There is a floor, and it is not zero`. Replace that heading and the paragraphs under it, down to the next heading, with:

```markdown
#### There is a floor, and it is one minute

`idleTimeout` is also the TTL on the demand annotation the proxy writes, so a value too
short to survive until the controller's next reconcile leaves the Model permanently
unwakeable. Squall refuses anything below **one minute**: the Model reports
`Schedulable=False` with the reason in `status.conditions` instead of failing silently.

One minute is a floor under a cliff, not a recommendation. It clears three separate
contributors at once — up to a second lost to the annotation's RFC3339 second
granularity, the 15s default `SQUALL_IDLE_REQUEUE_INTERVAL`, and cold starts measured in
minutes on every backend. Pick a real value from the table above; the default is `5m`.
```

- [ ] **Step 4: Update the CHANGELOG**

`CHANGELOG.md` currently opens with `# Changelog`, a format note, then `## [0.1.7] — 2026-09-05`. Insert a new section directly above the `[0.1.7]` heading:

```markdown
## [Unreleased]

### Changed

- **`spec.idleTimeout` now has an enforced floor of one minute.** It is also the demand
  annotation's TTL, so a shorter value could leave an on-demand Model permanently
  unwakeable with no error, no event and no condition. A Model below the floor now
  reports `Schedulable=False` with the reason in `status.conditions` and is not
  provisioned. There is no admission webhook, so this cannot refuse a `kubectl apply` or
  fail a GitOps sync — but a Model running today with, say, `idleTimeout: 10s` will stop
  waking after this upgrade. Raise it to at least `1m`.
```

- [ ] **Step 5: Close D171's owed item in the ledger**

In `docs/references/deviations-and-findings.md`, D171's resolution cell ends with:

> **Still owed:** decide whether validation should reject or warn on an `idleTimeout` too short to survive a reconcile interval, rather than only on zero.

Replace that sentence with:

> **Owed item CLOSED 2026-09-06:** rejects, not warns — `MinIdleTimeout = 1m`, enforced in `ValidateWithWarnings` and surfaced as `Schedulable=False`. A warning in a controller log for a Model that silently never serves is not protection. Design: `docs/plans/2026-09-06-idle-timeout-floor-design.md`. The same pass found `test/e2e/cluster/03-fixtures/model.yaml` still carrying the 2s shape — only `loopModelYAML` was raised during the v0.1.7 release. The mechanism behind (b) remains unproven: the stamp may expire before the next evaluation, or the Model may wake and be re-slept immediately by `sleepDue`. The floor clears both readings.

Do not renumber, delete or reword any other ledger entry.

- [ ] **Step 6: Fix the stale e2e comment**

In `test/e2e/cluster/helm-values.yaml`, line 46 reads that the fixture's own `idleTimeout` is 2s. Change `2s` to `5m` so it matches `03-fixtures/model.yaml` after Task 1.

- [ ] **Step 7: Verify the docs pass the gates**

```bash
./scripts/dev.sh make qa-lint
./scripts/dev.sh make helm-sync-check
```

Expected: green. `helm-sync-check` is implemented as `git diff --quiet deploy/helm/squall/`, so it reports "CHART DRIFT" for any *uncommitted* change in that directory — including the legitimately regenerated CRD from Step 2. Commit first, then re-run; a drift report on uncommitted regenerated output is a false positive, not a failure.

- [ ] **Step 8: Commit**

```bash
git add api/squall/v1alpha1/model_types.go \
        config/crd/bases/squall.ackstorm.ai_models.yaml \
        deploy/helm/squall/crds/squall.ackstorm.ai_models.yaml \
        README.md CHANGELOG.md \
        docs/references/deviations-and-findings.md \
        test/e2e/cluster/helm-values.yaml
git commit -m "docs: the idleTimeout floor is one minute and enforced

Corrects every place that said validation could only reject zero, closes
D171's owed item with the decision (reject, not warn), and fixes the stale
e2e values comment left by the fixture change."
```

---

## Verification

The change is complete when all of the following hold:

- `./scripts/dev.sh make test-unit` — green
- `./scripts/dev.sh make test-envtest` — green, `TestDecide_SleepsARunThatNeverReachedReady` behaving exactly as before
- `./scripts/dev.sh make qa-lint` — green
- Raising `MinIdleTimeout` to `2 * time.Minute` turns `TestValidate_AcceptsIdleTimeoutExactlyAtFloor` red
- `grep -rn "idleTimeout: 2s" test/` returns nothing
- No document claims validation can only reject a zero

The e2e suite (`make cluster-up` plus the Ginkgo run) is the one gate that gets measurably slower — about a minute, from the loop Model's longer idle window. Run it once at the end, not per task.
