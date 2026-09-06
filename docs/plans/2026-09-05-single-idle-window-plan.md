# Single Idle Window Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `Model` declares one idle window (`idleTimeout`) instead of two, and squall always tells dstack `idle_duration: 0`, so machine and model live and die together on every backend.

**Architecture:** Three layers change in dependency order. First the dstack wire learns to send an explicit zero (today it physically cannot). Then the controller stops reading the Model's fleet block and always sends zero. Then the CRD drops `spec.fleet` and renames `scaleDownDelaySeconds` to `idleTimeout` as a duration, and validation is re-pointed at the mistake operators actually make.

**Tech Stack:** Go 1.26.6, controller-runtime, kubebuilder markers, Helm. Everything runs inside the devtools container via `./scripts/dev.sh`.

**Design of record:** `docs/plans/2026-09-05-single-idle-window-design.md`. Where this plan and that design disagree, the design wins — say so rather than silently picking.

## Global Constraints

- **Never call bare `go` / `make`.** Every build, test, lint and codegen goes through `./scripts/dev.sh` (e.g. `./scripts/dev.sh make test-unit`). The host does not have Go 1.26.6.
- **The lint target is `qa-lint`, not `lint`.** CRD/codegen targets are `gen-manifests` and `gen-code`.
- **Never `git add -A`.** Add explicit paths only. Only add commits — never `git reset`, `--amend`, rebase, or `git checkout -- <file>`.
- **`helm-sync-check` is `git diff --quiet deploy/helm/squall/`.** Commit chart changes before running any gate that includes it.
- **Squall NEVER sends `force`.** Nothing in this change goes near it; if you find yourself adding a `Force` field, stop.
- **`1→0 fails safe, 0→1 fails open.** A wrong wake costs money; a wrong sleep kills a generation. Every ambiguity in this change resolves in that direction.
- Every new `.go` file carries the `// SPDX-License-Identifier: MIT` header (the pre-push gate enforces it).
- Ledger entries are append-only: never renumber, never delete.

## Execution model — read this before running anything

This project's `CLAUDE.md` overrides the default per-task test ritual:

- **Do not run the full gates after every task.** Accumulate, then verify.
- The tasks below are grouped into **three blocks**. Run `./scripts/dev.sh make test-unit` and `./scripts/dev.sh make qa-lint` **once per block**, at the block boundary — not per task, not per file.
- Scoped package tests inside a block are fine and encouraged (`./scripts/dev.sh go test ./internal/dstack/ -count=1`).
- **One review per block**, not per task.
- **Batch the mutation sweep**: write the mutation-proofs as you go, but run them as one sweep at the end of each block.
- Do not re-run a gate whose inputs provably did not change. `git diff` the range first.

| Block | Tasks | What it is | Gate at the end |
|---|---|---|---|
| 1 | 1, 2 | The money path: wire + controller send an explicit zero | `test-unit`, `qa-lint`, mutation sweep |
| 2 | 3, 4, 5 | The CRD: delete `spec.fleet`, rename the knob, re-point validation | `test-unit`, `test-envtest`, `qa-lint`, mutation sweep |
| 3 | 6 | Docs, samples, runbooks, spec of record, CHANGELOG | `test-unit`, `test-envtest`, `helm-sync-check` |

## File structure

| File | Responsibility | Block |
|---|---|---|
| `internal/dstack/wire.go` | JSON shapes. Gains `dstackSeconds`; both `idle_duration` fields become `*int`. | 1 |
| `internal/dstack/http.go` | Builds the bodies. Two call sites switch to `dstackSeconds`. | 1 |
| `internal/dstack/client_test.go` | Wire assertions on the literal marshalled bytes. | 1 |
| `internal/controller/squall/model_controller.go` | `applyDurationsFor` stops reading `spec.Fleet`; three renamed call sites. | 1, 4 |
| `internal/controller/squall/preflight.go` | `EnsureFleet` gets a literal zero. | 1 |
| `internal/controller/squall/phase.go` | `sleepDue`, `unhealthyDue`, `uncontrolledTimeoutFor` take `time.Duration`. | 4 |
| `api/squall/v1alpha1/model_types.go` | `ModelFleet` deleted; `ScaleDownDelaySeconds` → `IdleTimeout`. | 3, 4 |
| `internal/controller/squall/model_validation.go` | Warm-window machinery deleted; two new rejections; new warning. | 3, 5 |
| `internal/proxy/cache.go`, `internal/proxy/handler.go` | Snapshot field and refresh cadence renamed. | 4 |
| `config/crd/bases/…yaml`, `deploy/helm/squall/crds/…yaml` | Regenerated. Never hand-edited. | 3, 4 |
| `config/samples/*.yaml`, `docs/runbooks/*.yaml`, `test/e2e/cluster/03-fixtures/model.yaml` | Rewritten to the new shape. | 6 |

### Out of scope — do not touch

`deploy/helm/squall/values.yaml` and `deploy/helm/squall/templates/dstack-config-secret.yaml` contain `dstack.fleets[].idleDuration`. **That is a different field**: dstack's own flat fleet schema for operator-declared fleets, not the Model CR's. It keeps its name, its type and its required-ness. Renaming it breaks the chart's fleet Job.

---

## Task 1: The wire can express `idle_duration: 0`

Today it cannot, and this is the single most dangerous detail in the change. `dstackDuration()` returns `""` for any non-positive duration and both `idle_duration` fields carry `,omitempty`, so passing a Go zero **omits the key** — and dstack then applies its own defaults: `DEFAULT_RUN_TERMINATION_IDLE_TIME = 5m`, `DEFAULT_FLEET_TERMINATION_IDLE_TIME = 3d` (`core/models/profiles.py`). That 3-day fleet default is the exact footgun the CRD's required field exists to prevent. Ledger D166.

**Files:**
- Modify: `internal/dstack/wire.go:98` (`configurationWire.IdleDuration`), `internal/dstack/wire.go:198` (`fleetConfigurationWire.IdleDuration`), and add `dstackSeconds` beside `dstackDuration` at `internal/dstack/wire.go:102`
- Modify: `internal/dstack/http.go:229`, `internal/dstack/http.go:269`
- Test: `internal/dstack/client_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func dstackSeconds(d time.Duration) *int` (package-private, `internal/dstack`). Returns a **non-nil** pointer for a zero duration. `ApplyRequest.IdleDuration` and `FleetSpec.IdleDuration` keep their `time.Duration` type and their names — only the JSON encoding changes.

- [ ] **Step 1: Write the two failing tests**

Add to `internal/dstack/client_test.go`. The run configuration travels in the `runs/get_plan` body; the fleet configuration travels in the `fleets/get_plan` body. Both assert on the **literal marshalled bytes**, because asserting on a decoded struct passes happily while the key is absent.

```go
// TestHTTPClient_Apply_SendsExplicitIdleDurationZero is D166's guard. A Go
// zero used to serialize as an ABSENT key, and dstack reads an absent
// idle_duration as "apply my default" — 5m for a run, 3d for a fleet. The
// assertion is on raw bytes on purpose: unmarshalling into a struct with a
// *int field cannot tell "sent 0" from "sent nothing".
func TestHTTPClient_Apply_SendsExplicitIdleDurationZero(t *testing.T) {
	var planBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case getPlanPath:
			planBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"run_spec":{},"current_resource":null,"action":"create"}`))
		case "/api/project/main/runs/apply":
			_, _ = w.Write([]byte(`{"id":"abc","jobs":[],"run_spec":{"configuration":{"replicas":{"min":1,"max":1}}}}`))
		}
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	if _, err := c.Apply(context.Background(), dstack.ApplyRequest{
		Name: "qwen", Replicas: 1, Image: "img", Port: 8080, IdleDuration: 0,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !bytes.Contains(planBody, []byte(`"idle_duration":0`)) {
		t.Fatalf("run plan body does not carry an explicit idle_duration: 0.\n"+
			"An absent key makes dstack apply its own 5m run default (D166).\nbody = %s", planBody)
	}
}

// TestHTTPClient_EnsureFleet_SendsExplicitIdleDurationZero is the same
// guard on the fleet path, where the default squall would inherit is
// THREE DAYS of paid idle instance.
func TestHTTPClient_EnsureFleet_SendsExplicitIdleDurationZero(t *testing.T) {
	var planBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/project/main/fleets/get":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":[{"code":"resource_not_exists"}]}`))
		case "/api/project/main/fleets/get_plan":
			planBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"spec":{"configuration":{"type":"fleet","name":"squall-auto-vastai","nodes":"0.."}}}`))
		case "/api/project/main/fleets/apply":
			_, _ = w.Write([]byte(`{"id":"f1","name":"squall-auto-vastai"}`))
		}
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	if err := c.EnsureFleet(context.Background(), dstack.FleetSpec{
		Name: "squall-auto-vastai", Backends: []string{"vastai"}, IdleDuration: 0,
	}); err != nil {
		t.Fatalf("EnsureFleet: %v", err)
	}

	if !bytes.Contains(planBody, []byte(`"idle_duration":0`)) {
		t.Fatalf("fleet plan body does not carry an explicit idle_duration: 0.\n"+
			"An absent key makes dstack apply its own THREE DAY fleet default (D166).\nbody = %s", planBody)
	}
}
```

Add `"bytes"` and `"io"` to the test file's imports if they are not already there.

- [ ] **Step 2: Run them and watch both fail**

```bash
./scripts/dev.sh go test ./internal/dstack/ -run 'IdleDurationZero' -count=1
```

Expected: both FAIL, each printing a body with **no** `idle_duration` key at all. If a test fails for any other reason — a 404 from the fake server, a decode error — fix the test before touching production code. A test that fails for the wrong reason proves nothing.

- [ ] **Step 3: Add `dstackSeconds` to `wire.go`**

Place it directly below `dstackDuration` (which stays: `max_duration` still uses it).

```go
// dstackSeconds renders a duration as dstack's whole-second integer form.
//
// It returns a NON-NIL pointer for a zero duration on purpose. Zero is a
// meaningful value — "release on the first idle pass" — and it has to reach
// the wire, while a nil pointer means "unset" and lets dstack apply its own
// defaults: DEFAULT_RUN_TERMINATION_IDLE_TIME = 5m and
// DEFAULT_FLEET_TERMINATION_IDLE_TIME = 3d (core/models/profiles.py). The
// string form with `omitempty` could not express that difference, which is
// how a Go zero came to mean "three days of paid idle instance" (D166).
func dstackSeconds(d time.Duration) *int {
	if d < 0 {
		d = 0
	}
	s := int(d / time.Second)
	return &s
}
```

- [ ] **Step 4: Change both wire field types**

`configurationWire` (around `wire.go:98`):

```go
	MaxPrice     string            `json:"max_price,omitempty"`
	IdleDuration *int              `json:"idle_duration,omitempty"`
	MaxDuration  string            `json:"max_duration,omitempty"`
```

`fleetConfigurationWire` (around `wire.go:198`):

```go
	Backends     []string      `json:"backends,omitempty"`
	IdleDuration *int          `json:"idle_duration,omitempty"`
```

`omitempty` is kept and is correct: on a pointer it drops only `nil`, and `dstackSeconds` never returns nil. Leave `MaxDuration` exactly as it is — it is not part of this change.

- [ ] **Step 5: Point both construction sites at the new helper**

`internal/dstack/http.go:229` (inside `createFleet`):

```go
				IdleDuration: dstackSeconds(spec.IdleDuration),
```

`internal/dstack/http.go:269` (inside the apply-body builder):

```go
			IdleDuration: dstackSeconds(req.IdleDuration),
			MaxDuration:  dstackDuration(req.MaxDuration),
```

- [ ] **Step 6: Run the package and watch it pass**

```bash
./scripts/dev.sh go test ./internal/dstack/ -count=1
```

Expected: PASS, whole package. If an existing test breaks because it asserted on `"idle_duration":"600s"`, update it to `"idle_duration":600` — the integer form is what dstack's `Duration.parse` takes and what its own read side already models (`wire.go:291`).

- [ ] **Step 7: Commit**

```bash
git add internal/dstack/wire.go internal/dstack/http.go internal/dstack/client_test.go
git commit -m "fix(dstack): make idle_duration: 0 expressible on the wire"
```

---

## Task 2: Always send zero

The Model's fleet block stops reaching dstack. `spec.fleet` still exists after this task — the CRD change is Task 3 — so the tree stays compiling and the two tasks stay separately reviewable.

**Files:**
- Modify: `internal/controller/squall/model_controller.go` — `applyDurationsFor` (around line 810) and its call site (around line 901)
- Modify: `internal/controller/squall/preflight.go:44` (signature) and `internal/controller/squall/preflight.go:71-75` (the `EnsureFleet` call)
- Test: `internal/controller/squall/phase_test.go` or the controller's unit test file, wherever `applyDurationsFor` is already exercised

**Interfaces:**
- Consumes: `dstackSeconds` indirectly, through `dstack.ApplyRequest.IdleDuration` and `dstack.FleetSpec.IdleDuration` (unchanged Go types).
- Produces: `applyDurationsFor(model *squallv1alpha1.Model, action Action) (idle, hard time.Duration)` keeps its signature and now always returns `idle == 0`. `preflight(ctx, c, backends []string)` **loses its `idle time.Duration` parameter**.

- [ ] **Step 1: Write the failing test**

```go
// TestApplyDurationsFor_AlwaysSendsZeroIdle is the single-idle-window
// invariant at the controller boundary: squall keeps no warm pool on any
// backend, so the fleet idle window it asks dstack for is always zero and
// the whole budget lives in idleTimeout. A non-zero here would buy a
// second, strictly worse idle window at the same hourly price.
func TestApplyDurationsFor_AlwaysSendsZeroIdle(t *testing.T) {
	model := &squallv1alpha1.Model{
		Spec: squallv1alpha1.ModelSpec{
			MinReplicas: 0,
			HardStop:    metav1.Duration{Duration: 2 * time.Hour},
		},
	}
	idle, hard := applyDurationsFor(model, Action{Replicas: 1})
	if idle != 0 {
		t.Errorf("idle = %s, want 0: squall never asks dstack to hold a machine idle", idle)
	}
	if hard != 2*time.Hour {
		t.Errorf("hard = %s, want 2h: hardStop must be unaffected by the idle change", hard)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -run 'AlwaysSendsZeroIdle' -short -count=1
```

Expected: FAIL — `idle` comes back as whatever `spec.fleet.idleDuration` holds (the fixture's zero value may accidentally pass; if it does, set `model.Spec.Fleet.IdleDuration = metav1.Duration{Duration: 10 * time.Minute}` in the fixture first and re-run, so you see a real red).

**`-short` is required.** `TestMain` starts envtest without it and the run dies on a missing etcd binary.

- [ ] **Step 3: Stop reading the fleet block**

In `applyDurationsFor`:

```go
func applyDurationsFor(model *squallv1alpha1.Model, action Action) (idle, hard time.Duration) {
	if action.Replicas == 0 && action.Current != nil {
		return action.Current.IdleDuration, action.Current.MaxDuration
	}
	// Always zero. dstack's fleet idle window is a second idle window that
	// bills exactly like the first one and buys strictly less: it keeps the
	// machine but drops the weights, so a wake inside it still reloads. The
	// whole budget belongs in idleTimeout, which the controller gates on
	// in-flight evidence. See the single-idle-window design.
	idle = 0
	if model.Spec.MinReplicas == 0 {
		hard = model.Spec.HardStop.Duration
	}
	return idle, hard
}
```

Leave the `action.Replicas == 0 && action.Current != nil` branch alone: that path echoes the live run's own values back for a CAS-safe in-place flip, and rewriting them would change what dstack compares.

- [ ] **Step 4: Drop `preflight`'s idle parameter**

`preflight.go:44` becomes:

```go
func preflight(ctx context.Context, c preflightClient, backends []string) (reason, message string, fleets []squallv1alpha1.FleetStatus) {
```

and its `EnsureFleet` call (around line 71):

```go
			if err := c.EnsureFleet(ctx, dstack.FleetSpec{
				Name:     dstack.FleetName(b),
				Backends: []string{b},
				// Zero, always: an auto-created fleet must never keep a
				// warm instance, and an ABSENT idle_duration would inherit
				// dstack's three-day default (D166).
				IdleDuration: 0,
			}); err != nil {
```

Update the single caller at `model_controller.go:901`:

```go
	if reason, msg, fleets := preflight(ctx, r.DstackClient, enginePlacement(model.Spec.Placement).Backends); reason != "" {
```

- [ ] **Step 5: Run the package**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -short -count=1
```

Expected: PASS. Fix any `preflight(...)` call in `preflight_test.go` that still passes the old fourth argument.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/squall/model_controller.go internal/controller/squall/preflight.go internal/controller/squall/preflight_test.go internal/controller/squall/phase_test.go
git commit -m "feat(controller): always ask dstack for a zero fleet idle window"
```

- [ ] **Step 7: BLOCK 1 GATE — run once, now, not after each task**

```bash
./scripts/dev.sh make test-unit
./scripts/dev.sh make qa-lint
```

Then the block's mutation sweep, one pass:

| Mutation | Must go red |
|---|---|
| Revert `configurationWire.IdleDuration` to `string` + `dstackDuration` | `TestHTTPClient_Apply_SendsExplicitIdleDurationZero` |
| Revert `fleetConfigurationWire.IdleDuration` to `string` + `dstackDuration` | `TestHTTPClient_EnsureFleet_SendsExplicitIdleDurationZero` |
| Make `dstackSeconds` return `nil` for a zero duration | both of the above |
| Restore `idle = model.Spec.Fleet.IdleDuration.Duration` | `TestApplyDurationsFor_AlwaysSendsZeroIdle` |

Restore the file after each mutation (`git checkout` is banned while the tree is dirty — copy the file to a temp path first, mutate, test, copy back).

A mutation that leaves the suite green is a finding: write it in the ledger and fix the test before moving to Block 2.

---

## Task 3: Delete `spec.fleet` and the warm-window machinery

**Files:**
- Modify: `api/squall/v1alpha1/model_types.go` — delete the `ModelFleet` type (around lines 239-258) and the `Fleet ModelFleet` field (around line 427)
- Modify: `internal/controller/squall/model_validation.go` — delete the `fleet.idleDuration` rejection (around line 34), the warm-window warning (around lines 60-71), `nonDockerizedBackends` and `backendsHoldAWarmPool` (end of file)
- Modify: `internal/controller/squall/model_validation_test.go` — delete `TestValidate_WarmWindowIgnoresIdleDurationWhereItIsInert` entirely
- Regenerate: `config/crd/bases/squall.ackstorm.ai_models.yaml`, `deploy/helm/squall/crds/squall.ackstorm.ai_models.yaml`

**Interfaces:**
- Consumes: nothing.
- Produces: `squallv1alpha1.ModelSpec` no longer has a `Fleet` field. Any code or fixture referencing `spec.Fleet` or `ModelFleet` will fail to compile — that is the point, and the compiler is the checklist.

- [ ] **Step 1: Delete the API type and field**

Remove the whole `ModelFleet` struct and its doc comment, and this from `ModelSpec`:

```go
	// Fleet configures the dstack fleet backing this model, in particular
	// the machine-release idle window.
	Fleet ModelFleet `json:"fleet"`
```

- [ ] **Step 2: Delete the validation machinery**

From `model_validation.go`, remove the rejection:

```go
	if spec.Fleet.IdleDuration.Duration <= 0 {
		return nil, fmt.Errorf("fleet.idleDuration is required and must be > 0: dstack's own default is 3 days (F21), so leaving it unset is not a safe fallback")
	}
```

the warm-window warning block, replacing it with the bare rule that survives (the new warning arrives in Task 5):

```go
	var warnings []string
	if spec.HardStop.Duration == 0 && spec.MinReplicas == 0 {
		warnings = append(warnings, "hardStop is disabled: nothing would stop this Model's capacity if the controller died — and note hardStop does not currently fire on the Kubernetes backend either (D161), so enabling it is not by itself a dead-man's switch")
	}
```

and both helpers at the end of the file — `nonDockerizedBackends` and `backendsHoldAWarmPool` — in full. Remove the `"strings"` import if nothing else in the file uses it; `qa-lint` will tell you.

- [ ] **Step 3: Delete the orphaned test**

Remove `TestValidate_WarmWindowIgnoresIdleDurationWhereItIsInert` from `model_validation_test.go`, including the `slurm` row added on 2026-09-05. It tested arithmetic over a window that no longer exists. Do not try to salvage it.

- [ ] **Step 4: Fix every fixture the compiler now rejects**

```bash
./scripts/dev.sh go build ./... 2>&1 | head -40
```

Expected: errors in test fixtures that set `Fleet:`. Delete those lines. `exampleModelSpec()` in `model_validation_test.go` is the main one; `model_sample_envtest_test.go` reads YAML rather than Go structs and is handled in Task 6.

- [ ] **Step 5: Regenerate the CRD**

```bash
./scripts/dev.sh make gen-manifests
./scripts/dev.sh make gen-code
git diff --stat config/crd/bases/ deploy/helm/squall/crds/
```

Expected: the `fleet` property and its `required` entry disappear from both files. **Never hand-edit a generated CRD.** If the Helm copy did not change, find the target that syncs it (`make help`) rather than copying by hand.

- [ ] **Step 6: Commit**

```bash
git add api/squall/v1alpha1/model_types.go internal/controller/squall/model_validation.go internal/controller/squall/model_validation_test.go config/crd/bases/squall.ackstorm.ai_models.yaml deploy/helm/squall/crds/squall.ackstorm.ai_models.yaml
git commit -m "feat!: remove spec.fleet — squall keeps no warm pool on any backend"
```

---

## Task 4: Rename `scaleDownDelaySeconds` to `idleTimeout`

An `int32` of seconds becomes a `metav1.Duration`, matching every other timing field in the CR.

**Files:**
- Modify: `api/squall/v1alpha1/model_types.go:393-406`
- Modify: `internal/controller/squall/phase.go:260, 274, 370` and the `sleepDue` / `unhealthyDue` signatures
- Modify: `internal/controller/squall/model_controller.go:975, 1021, 1183`
- Modify: `internal/controller/squall/model_validation.go:61`
- Modify: `internal/proxy/cache.go:55, 61, 241, 266`, `internal/proxy/handler.go:190, 365`
- Regenerate: both CRD files

**Interfaces:**
- Consumes: `ModelSpec` without `Fleet` (Task 3).
- Produces:
  - `squallv1alpha1.ModelSpec.IdleTimeout metav1.Duration` with json tag `idleTimeout`, default `"5m"`.
  - `sleepDue(activity *ActivityEvidence, durableLastRequestAt *metav1.Time, idleTimeout time.Duration, now time.Time) bool`
  - `unhealthyDue(activity *ActivityEvidence, ready bool, wakeStartedAt *metav1.Time, unhealthyAfter time.Duration, failureThreshold int32, idleTimeout time.Duration, now time.Time) bool`
  - `refreshIntervalFor(idleTimeout time.Duration, ceiling time.Duration) time.Duration`
  - `proxy.ModelSnapshot.IdleTimeout time.Duration`

- [ ] **Step 1: Change the API field**

```go
	// IdleTimeout is how long squall keeps paying after the last request,
	// with the model loaded and answering instantly. Once in-flight
	// requests are 0 and the newest request is older than this, the
	// controller flips replicas: 0 and dstack releases the machine (§6).
	// There is no warm pool on any backend, by design, so the next wake
	// after this window is a full cold start and holdTimeout has to cover
	// one.
	//
	// It is ALSO hasDemand's TTL, so a zero makes an on-demand Model
	// permanently unwakeable — the annotation the proxy writes would expire
	// the instant it lands. Unlike the int32 it replaces, a written `0s`
	// survives serialization, so validation rejects it explicitly.
	// +kubebuilder:default="5m"
	// +optional
	IdleTimeout metav1.Duration `json:"idleTimeout,omitempty"`
```

`5m` is deliberately the same window as the `300` it replaces: no existing Model changes what it spends because of a rename.

- [ ] **Step 2: Follow the compiler**

```bash
./scripts/dev.sh go build ./... 2>&1 | head -40
```

Work through each error. The mechanical shape everywhere is
`time.Duration(spec.ScaleDownDelaySeconds) * time.Second` → `spec.IdleTimeout.Duration`.

The three that need more than a substitution:

`phase.go:370` — `uncontrolledTimeoutFor`:

```go
	d := 4*spec.IdleTimeout.Duration + DefaultUncontrolledGrace
```

`phase.go:395` — `sleepDue`'s parameter type and its final comparison:

```go
func sleepDue(activity *ActivityEvidence, durableLastRequestAt *metav1.Time, idleTimeout time.Duration, now time.Time) bool {
	...
	return now.Sub(anchor) > idleTimeout
}
```

`internal/proxy/cache.go:241` reads the CR as unstructured; the field is now a duration **string**, not an int:

```go
	idleTimeout, _, _ := unstructured.NestedString(u.Object, "spec", "idleTimeout")
	idle, err := time.ParseDuration(idleTimeout)
	if err != nil {
		// Unparseable or absent resolves to zero, which refreshIntervalFor
		// reads as "no TTL configured" and answers with the proxy-wide
		// ceiling. Guessing a duration here would be worse: a held request
		// refreshing too slowly ages its own demand anchor out (LIVE-3).
		idle = 0
	}
```

and the `cache.Set` call below it (around `cache.go:266`) carries the renamed field:

```go
		cache.Set(u.GetName(), ModelSnapshot{
			Namespace:   u.GetNamespace(),
			Replica:     replicaFromStatus(u),
			Phase:       squallv1alpha1.ModelPhase(phase),
			HoldTimeout: hold,
			IdleTimeout: idle,
			Created:     u.GetCreationTimestamp().Time,
			// ... every other field unchanged
		})
```

and `refreshIntervalFor` takes the duration directly:

```go
func refreshIntervalFor(idleTimeout time.Duration, ceiling time.Duration) time.Duration {
	const (
		fraction = 10
		floor    = 500 * time.Millisecond
	)
	if idleTimeout <= 0 {
		return ceiling
	}
	interval := idleTimeout / fraction
	if interval < floor {
		interval = floor
	}
	if ceiling > 0 && interval > ceiling {
		interval = ceiling
	}
	return interval
}
```

- [ ] **Step 3: Update the tests the compiler points at**

Every test setting `ScaleDownDelaySeconds: 300` becomes `IdleTimeout: metav1.Duration{Duration: 5 * time.Minute}`. Keep the same durations — a rename must not change any test's meaning. `internal/proxy` tests that build a `ModelSnapshot` need `IdleTimeout: 300 * time.Second` instead of `ScaleDownDelaySeconds: 300`.

- [ ] **Step 4: Add the test that keeps the rejected gate out**

In `phase_test.go`:

```go
// TestDecide_SleepsARunThatNeverReachedReady exists to stop a rejected
// design from being reintroduced. A gate making sleepDue inapplicable to a
// never-Ready run was approved and then withdrawn on live evidence (D165):
// a client still waiting already keeps inFlight > 0, so AllIdle is false
// and such a wake cannot be slept; and in the only case the gate would
// change — nobody waiting — it would leave an abandoned wake's instance
// running until provisioningTimeout. A client that abandons cancelling its
// own wake is correct, thrifty behaviour.
func TestDecide_SleepsARunThatNeverReachedReady(t *testing.T) {
	now := time.Now()
	spec := exampleModelSpec()
	spec.MinReplicas = 0
	spec.IdleTimeout = metav1.Duration{Duration: 2 * time.Minute}

	observed := Observed{
		Run:   &dstack.Run{RunID: "r1", Replicas: 1},
		Ready: false, // never served
		Activity: &ActivityEvidence{
			Complete: true, AnyData: true, AllIdle: true,
			NewestLastRequestAt: now.Add(-5 * time.Minute),
		},
	}
	prior := squallv1alpha1.ModelStatus{
		RunID:         "r1",
		WakeStartedAt: &metav1.Time{Time: now.Add(-6 * time.Minute)},
	}

	phase, action := Decide(observed, prior, spec, false, now)
	if phase != squallv1alpha1.ModelPhaseAsleep || !action.Apply || action.Replicas != 0 {
		t.Fatalf("Decide() = %s %+v; a never-Ready run with complete idle evidence and an "+
			"expired anchor MUST still sleep — see D165", phase, action)
	}
}
```

`exampleModelSpec()` lives in `model_validation_test.go:19` and is reachable because every test
file in this directory is `package squall`. Set its `ProvisioningTimeout` long enough that
`provisioningDue` does not fire first and mask the assertion — 30 minutes against a 6-minute
wake is fine.

- [ ] **Step 5: Regenerate and run**

```bash
./scripts/dev.sh make gen-manifests
./scripts/dev.sh make gen-code
./scripts/dev.sh go test ./internal/... ./api/... -short -count=1
```

Expected: PASS, and the CRD now shows `idleTimeout` with `default: 5m` and no `scaleDownDelaySeconds`.

- [ ] **Step 6: Commit**

```bash
git add api/ internal/ config/crd/bases/squall.ackstorm.ai_models.yaml deploy/helm/squall/crds/squall.ackstorm.ai_models.yaml
git commit -m "feat!: rename scaleDownDelaySeconds to idleTimeout as a duration"
```

---

## Task 5: Re-point validation at the mistake operators actually make

**Files:**
- Modify: `internal/controller/squall/model_validation.go`
- Test: `internal/controller/squall/model_validation_test.go`

**Interfaces:**
- Consumes: `ModelSpec.IdleTimeout` (Task 4), `ModelSpec` without `Fleet` (Task 3).
- Produces: no new exported symbols. `ValidateWithWarnings` keeps its signature.

- [ ] **Step 1: Write the failing tests**

```go
func TestValidate_RejectsNonPositiveIdleTimeout(t *testing.T) {
	spec := exampleModelSpec()
	spec.IdleTimeout = metav1.Duration{Duration: 0}
	if _, err := ValidateWithWarnings(spec); err == nil {
		t.Fatal("ValidateWithWarnings() = nil; a zero idleTimeout expires the demand " +
			"annotation the instant it lands, making an on-demand Model permanently unwakeable")
	}
}

func TestValidate_RejectsNonPositiveProvisioningTimeout(t *testing.T) {
	spec := exampleModelSpec()
	spec.ProvisioningTimeout = metav1.Duration{Duration: 0}
	spec.HoldTimeout = metav1.Duration{Duration: 0}
	if _, err := ValidateWithWarnings(spec); err == nil {
		t.Fatal("ValidateWithWarnings() = nil; provisioningDue is a no-op for a " +
			"non-positive timeout, and it is the primary bound on a run that never " +
			"reaches Ready")
	}
}

func TestValidate_WarnsWhenHoldCannotCoverAColdStart(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hold        time.Duration
		provisning  time.Duration
		wantWarning bool
	}{
		{"hold far below half the provisioning window", 5 * time.Minute, 30 * time.Minute, true},
		{"hold at half exactly is not warned", 15 * time.Minute, 30 * time.Minute, false},
		{"hold above half", 20 * time.Minute, 30 * time.Minute, false},
		{"hold disabled is not this warning's business", 0, 30 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := exampleModelSpec()
			spec.HoldTimeout = metav1.Duration{Duration: tc.hold}
			spec.ProvisioningTimeout = metav1.Duration{Duration: tc.provisning}

			warnings, err := ValidateWithWarnings(spec)
			if err != nil {
				t.Fatalf("ValidateWithWarnings() error = %v, want nil", err)
			}
			var got bool
			for _, w := range warnings {
				if strings.Contains(w, "cold start") {
					got = true
				}
			}
			if got != tc.wantWarning {
				t.Errorf("cold-start warning = %v, want %v (hold %s, provisioning %s); warnings = %v",
					got, tc.wantWarning, tc.hold, tc.provisning, warnings)
			}
		})
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -run 'TestValidate_' -short -count=1
```

- [ ] **Step 3: Implement**

Add the two rejections beside the existing ones, before the warnings block:

```go
	if spec.IdleTimeout.Duration <= 0 {
		return nil, fmt.Errorf("idleTimeout must be > 0: it is also the demand annotation's TTL, so a zero expires demand the instant the proxy writes it and the Model can never wake")
	}
	if spec.ProvisioningTimeout.Duration <= 0 {
		return nil, fmt.Errorf("provisioningTimeout must be > 0: it is the only bound on a run that never reaches Ready, and provisioningDue does nothing for a non-positive value")
	}
```

and the replacement warning in the warnings block:

```go
	// Replaces the per-backend warm-window arithmetic (D158, D164), which
	// measured a window that no longer exists. This warns in the direction
	// of the mistake actually made in production: a hold too short to
	// outlast a cold start answers 503 to EVERY cold request. The
	// comparison is against provisioningTimeout — the operator's own
	// statement of how long a wake may take — so it needs no knowledge of
	// backends. Half is a heuristic and the text says so: a measured 9m53s
	// cold start against a 30m provisioningTimeout sits just above the
	// line, and the 5m hold that would 503 everything sits well below it.
	if spec.HoldTimeout.Duration > 0 && spec.HoldTimeout.Duration*2 < spec.ProvisioningTimeout.Duration {
		warnings = append(warnings, fmt.Sprintf(
			"holdTimeout (%s) is less than half of provisioningTimeout (%s): there is no warm pool on any backend, so every wake is a full cold start and a hold this short will answer 503 to cold requests rather than serve them. This is a heuristic, not a rule — raise holdTimeout, or lower provisioningTimeout if your wakes really are that fast",
			spec.HoldTimeout.Duration, spec.ProvisioningTimeout.Duration))
	}
```

- [ ] **Step 4: Run and watch them pass**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -run 'TestValidate_' -short -count=1
```

- [ ] **Step 5: Commit**

```bash
git add internal/controller/squall/model_validation.go internal/controller/squall/model_validation_test.go
git commit -m "feat(validation): warn when holdTimeout cannot cover a cold start"
```

- [ ] **Step 6: BLOCK 2 GATE — run once, now**

```bash
./scripts/dev.sh make test-unit
./scripts/dev.sh make test-envtest
./scripts/dev.sh make qa-lint
```

`test-envtest` is in this block's gate and not Block 1's because this is where the CRD schema changed.

Block 2 mutation sweep, one pass:

| Mutation | Must go red |
|---|---|
| Drop the `idleTimeout > 0` rejection | `TestValidate_RejectsNonPositiveIdleTimeout` |
| Drop the `provisioningTimeout > 0` rejection | `TestValidate_RejectsNonPositiveProvisioningTimeout` |
| Change `*2 <` to `<` in the cold-start warning | the `hold at half exactly` case |
| Add `if prior.ReadyAt == nil { return false }` to `sleepDue` | `TestDecide_SleepsARunThatNeverReachedReady` |
| Change `4*spec.IdleTimeout.Duration` to `2*` | the `uncontrolledTimeoutFor` case |

---

## Task 6: Documentation, samples and the spec of record

The code is done; this task is what stops the next person from reading a lie.

**Files:**
- Modify: `config/samples/squall_v1alpha1_model.yaml`, `config/samples/squall_v1alpha1_model_qwen3_8b.yaml`, `config/samples/squall_v1alpha1_model_glm53_flash.yaml`
- Modify: `docs/runbooks/ollama-tiny.yaml`, `docs/runbooks/qwen3-8-27b.yaml`, `docs/runbooks/battery-k8s.yaml`
- Modify: `test/e2e/cluster/03-fixtures/model.yaml`, and `test/e2e/e2e_test.go` if it asserts on either field name
- Modify: `README.md`, `CHANGELOG.md`
- Modify: `docs/specs/squall-spec-v0_18-RC.md` §5.1, §6, F21, F21b
- Modify: `internal/controller/squall/CLAUDE.md`, `internal/dstack/CLAUDE.md`

**Interfaces:**
- Consumes: the final field names from Tasks 3-5.
- Produces: nothing consumed by code.

- [ ] **Step 1: Rewrite every sample and runbook**

In each file, delete the whole `fleet:` block and its comments, and replace the knob:

```yaml
  # before
  scaleDownDelaySeconds: 300
  fleet:
    idleDuration: 10m

  # after
  idleTimeout: 5m
```

Keep the same effective window each file had (`300` → `5m`, `600` → `10m`). In `squall_v1alpha1_model_qwen3_8b.yaml`, the long comment explaining that `fleet.idleDuration` is inert on Vast goes away with the field — replace it with one line saying there is no warm pool anywhere and that `holdTimeout: 20m` covers the measured 9m53s cold start.

- [ ] **Step 2: Update the README**

Three places: the two-layer paragraph in **Architecture** (idle now releases both layers at once), the whole `fleet.idleDuration` discussion under **Send it a request** (there is no warm pool anywhere; `holdTimeout` must cover a cold start on every backend), and every sample CR in **Worked examples**. The `dockerized` table in **Which backends** goes: it explained a distinction that no longer changes squall's behaviour.

- [ ] **Step 3: Amend the spec of record**

`docs/specs/squall-spec-v0_18-RC.md`:
- §6's three-row knob table loses its `fleet.idleDuration` row; the `scaleDownDelaySeconds` row becomes `idleTimeout` and its "Releases" cell becomes **the job and the machine**.
- The paragraph beginning *"Flipping to 0 does not release the instance"* is now false. Replace it with the dominance argument and a pointer to `docs/plans/2026-09-05-single-idle-window-design.md`.
- §5.1's warm-window validation rule becomes the cold-start rule.
- F21 and F21b keep their measured content — they are facts about dstack, still true — and each gains one line saying squall now sends an explicit zero so neither fact reaches a Model author as a knob.

- [ ] **Step 4: Write the CHANGELOG entry**

```markdown
### BREAKING

- `spec.fleet` is removed and `spec.scaleDownDelaySeconds` is renamed to
  `spec.idleTimeout`, now written as a duration (`5m`, not `300`).
  **Rewrite your Models before applying the new CRD.** Kubernetes prunes
  unknown fields, so an unmodified manifest applies without error and
  silently takes `idleTimeout`'s 5m default — a Model that had
  `scaleDownDelaySeconds: 600` goes from a 10-minute idle window to a
  5-minute one with no warning.
- Squall now always asks dstack for `idle_duration: 0`. There is no warm
  pool on any backend, so every wake is a full cold start and `holdTimeout`
  must cover one.
```

- [ ] **Step 5: Update the in-path memories**

`internal/dstack/CLAUDE.md` gains the D166 trap: a Go zero used to omit the key, and dstack reads an absent `idle_duration` as its own default — 3 days for a fleet. `internal/controller/squall/CLAUDE.md` gains the one-window model and the reason the rejected sleep gate must not come back.

- [ ] **Step 6: Commit, then the Block 3 gate**

```bash
git add config/samples/ docs/runbooks/ test/e2e/ README.md CHANGELOG.md docs/specs/ internal/controller/squall/CLAUDE.md internal/dstack/CLAUDE.md
git commit -m "docs: one idle window, everywhere it was documented as two"
```

```bash
./scripts/dev.sh make test-unit
./scripts/dev.sh make test-envtest
./scripts/dev.sh make helm-sync-check
```

`helm-sync-check` is `git diff --quiet deploy/helm/squall/`, so it fails on any uncommitted chart change — commit first, then run it.

---

## Deliberately nothing to do

- **No fleet migration.** Existing `squall-auto-*` fleets keep whatever `idle_duration` they
  were created with, and it becomes unreachable: dstack applies `min(fleet, run)` when
  provisioning a new instance, which is 0, and its "on reuse the fleet's value applies" rule
  never fires because with 0 no instance survives to be reused. Do not write a migration.
- **The orphan reaper is untouched.** It reads nothing from the CR — its cadence and grace are
  package constants. If a change here makes you edit `reaper.go`, you have gone wrong.

## Not in this plan

- **The rejected sleep gate.** Recorded in the design and guarded by a test. Do not add it.
- **`status.readyAt`.** Dropped in design self-review: its only consumer was that gate.
- **Separating the demand TTL from `idleTimeout`.** Left sharing one number, deliberately. Ledger.
- **D167's two stale `phase.go` comments.** Comment-only, kept out of this diff so it stays about behaviour.
- **Why the first container exited with `CONTAINER_EXITED_WITH_ERROR` at 105 s** during the D165 measurement. Unexplained, worth a look before the next live run.
- **The chart's `dstack.fleets[].idleDuration`.** A different field. See "Out of scope".
