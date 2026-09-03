# Idle-Capacity Guardians — Close the Holes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Guarantee that a GPU squall launched for a scale-to-zero Model (`minReplicas: 0`) cannot keep billing with no requests, by wiring the one dead knob (`fleet.idleDuration`), bounding the one unbounded request path, removing the one opt-out to "unbounded", and making a dead controller and an idle-but-awake Model visible.

**Architecture:** Squall's 1→0 flips live in `internal/controller/squall/phase.go` (`sleepDue`, `unhealthyDue`, `uncontrolledDue`, `provisioningDue`), the drain-first finalizer, and the orphan reaper. All are evidence-gated and correct. What is missing sits *around* them: the instance-layer timer that dstack enforces is never told squall's value; a request that never ends pins `inFlight > 0` forever; `uncontrolledTimeout: 0` disables the deadline; and every alert is computed by the same process whose death would remove every guardian. This plan touches each of those four seams with the smallest diff that turns each into a bound.

**Tech Stack:** Go 1.26.6 (containerised — every `go`/`make` goes through `./scripts/dev.sh`), controller-runtime, prometheus client_golang, Helm chart under `deploy/helm/squall`, dstack 0.21.2 REST API (`docs/references/dstack-real-api.md`).

**Source review this plan implements:** the 2026-09-03 guardian review (main session). Findings, in priority order: (1) `spec.fleet.idleDuration` validated but never sent to dstack; (2) controller death leaves no guardian and silences every alert; (3) a zombie stream pins the GPU; (4) `uncontrolledTimeout` opt-out and no cap; (5) no alert for Ready+idle+awake; (6) 190 lines of commented-out obsolete reaper tests.

## Global Constraints

- **Spec of record:** `docs/specs/squall-spec-v0_18-RC.md`. Where this plan and the spec disagree, the spec wins — say so, do not silently pick.
- **Spec §5.2, verbatim:** "`maxLifetime` is ALERT-ONLY, never destructive, never implemented via dstack's native `max_duration`". This plan therefore does **NOT** add `max_duration`. The controller-down hole is closed only as far as visibility (alert) and redundancy (replicas) allow. The remaining decision — a large `max_duration` as dead-man's switch — is the owner's, recorded in ledger, not made here.
- **Spec §6 / F21, verbatim:** "Flipping `replicas` to 0 terminates the **job** but the **instance** is released only by fleet `idle_duration` — defaults are `5m` for runs and **`3d` for fleets**; on reuse the fleet's value applies, on fresh provisioning the shorter of the two".
- **Invariants:** `0→1` fails open, `1→0` fails safe. Nothing in this plan may add a sleep path that fires on uncertainty. Every new bound is either a *ceiling on already-complete evidence* (Task 7) or a *value passed to dstack* (Tasks 1–2).
- **Squall NEVER sends `force`.** Do not touch `newApplyRequest` or `applyFleetPlanRequest`.
- **Toolchain:** never bare `go`/`make`. `./scripts/dev.sh make test-unit`, `./scripts/dev.sh make test-envtest`, `./scripts/dev.sh make qa-lint` (the lint target is `qa-lint`), `./scripts/dev.sh make gen-manifests`, `./scripts/dev.sh make helm-sync`. Never set `DOCKER_BUILDKIT=0`.
- **Test phase split:** pure tests run under `make test-unit` with no skip; envtest cases `t.Skip` under `-short`. Nothing new in this plan needs envtest — every new test is pure or `httptest`.
- **Gates run ONCE, at the end (Task 11)**, not per task. Per task: run only the scoped package test named in that task. Owner's most-repeated instruction.
- **Mutation check before claiming coverage:** every task names the mutation that must turn its test red. Do it, observe red, revert.
- **Concurrent-agent git rules:** never `git add -A`; never `git reset`, `--amend`, or rebase past a commit you did not create; never `git checkout -- <file>`/`git restore` on a dirty tree; only add commits. Stage files by explicit path.
- **Commit style:** conventional commits, imperative subject < 72 chars, body says why. Trailer: `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- **Ledger:** `docs/references/deviations-and-findings.md`. Append rows after `D151` (line 209, end of file) as `| **D152** | ... | ... | ... |` in the same 4-column table. Never renumber, never delete. Entries used by this plan: **D152** (idleDuration never sent), **D153** (request ceiling), **D154** (uncontrolledTimeout opt-out removed), **D155** (controller-down visibility; `max_duration` decision left to owner).
- **CHANGELOG:** `CHANGELOG.md` `## [Unreleased]` currently reads "Nothing yet." Replace that line with the sections each task adds. Anything that can cost money or terminate a generation is called out explicitly.
- **dstack duration wire format:** dstack's `parse_duration` accepts `"<int><unit>"` with unit in `s m h d w`; the server normalises durations to integer seconds (measured: probe `interval "5s" -> 5`). This plan always sends whole seconds as a string, e.g. `"600s"`.

---

## File map

| File | Task | Change |
|---|---|---|
| `internal/dstack/client.go` | 1, 2 | `ApplyRequest.IdleDuration`, `FleetSpec.IdleDuration` |
| `internal/dstack/wire.go` | 1, 2 | `configurationWire.IdleDuration`, `fleetConfigurationWire.IdleDuration`, `dstackDuration()` helper |
| `internal/dstack/http.go` | 1, 2 | thread both fields onto the wire |
| `internal/dstack/client_test.go` | 1, 2 | two new wire-capture tests |
| `internal/controller/squall/model_controller.go` | 1, 2, 4 | pass `spec.fleet.idleDuration` to Apply and preflight; `IdleMetrics` field + `recordMetrics` |
| `internal/controller/squall/preflight.go` | 2 | new `idle time.Duration` parameter |
| `internal/controller/squall/preflight_test.go` | 2 | update 4 call sites; one new assertion |
| `deploy/helm/squall/templates/dstack-config-secret.yaml` | 2 | render `idle_duration` per fleet, `required` |
| `deploy/helm/squall/values.yaml` | 2, 5, 7 | `fleets[].idleDuration`, `controller.replicas: 2`, `proxy.env.maxRequestDuration` |
| `internal/metrics/idle.go` (new) | 4 | `IdleCollector` |
| `internal/metrics/idle_test.go` (new) | 4 | |
| `cmd/controller/main.go` | 4 | register + wire `IdleCollector` |
| `internal/controller/squall/finalizer.go` | 4 | `IdleMetrics.Forget` |
| `config/prometheus/rules.yaml` | 5 | two new alerts |
| `config/prometheus/rules_test.go` | 5 | count 5→7, two new `wantExprs` |
| `deploy/helm/squall/files/prometheus-rule-spec.yaml` | 5 | regenerated by `make helm-sync` |
| `internal/proxy/handler.go` | 7 | `MaxRequestDuration`, `errRequestCeiling`, `outcomeCeiling` |
| `internal/proxy/handler_test.go` | 7 | `TestHandler_RequestCeiling_*` |
| `cmd/proxy/main.go` | 7 | `SQUALL_MAX_REQUEST_DURATION` |
| `deploy/helm/squall/templates/proxy-deployment.yaml` | 7 | env line |
| `internal/controller/squall/phase.go` | 8 | clamp in `uncontrolledTimeoutFor` |
| `internal/controller/squall/phase_test.go` | 8, 10 | rename two tests; add `TestDecide_BoundMatrix` |
| `internal/controller/squall/model_validation.go` | 8 | reject `uncontrolledTimeout` `<= 0` or `> 24h` |
| `internal/controller/squall/model_validation_test.go` | 8 | two cases |
| `api/squall/v1alpha1/model_types.go` | 8 | doc comment on `UncontrolledTimeout` |
| `deploy/helm/squall/crds/…`, `config/crd/bases/…` | 8 | regenerated |
| `internal/controller/squall/reaper_test.go` | 9 | delete lines 282–472 |
| `docs/references/deviations-and-findings.md` | 1, 5, 7, 8 | D152–D155 |
| `CHANGELOG.md` | 1, 5, 7, 8 | Unreleased |

---

## Block A — Send `idleDuration` to dstack (the bill)

### Task 1: Run-level `idle_duration` on every Apply

**Why first:** F21 says fresh provisioning honours the *shorter* of the run's and the fleet's `idle_duration`. Today the run carries none, so a Model's own `fleet.idleDuration` has no effect on the machine dstack rents for it. After this task every run squall applies carries it.

**Files:**
- Modify: `internal/dstack/client.go:69-110` (`ApplyRequest`)
- Modify: `internal/dstack/wire.go:78-94` (`configurationWire`) and add helper below it
- Modify: `internal/dstack/http.go:252-270` (run spec builder)
- Modify: `internal/controller/squall/model_controller.go:238-256` (the `DstackClient.Apply(...)` call)
- Test: `internal/dstack/client_test.go`

**Interfaces:**
- Produces: `dstack.ApplyRequest.IdleDuration time.Duration` (zero = omit from wire); `dstackDuration(d time.Duration) string` (unexported, `internal/dstack`), returns `""` for `d <= 0`, else `fmt.Sprintf("%ds", int64(d/time.Second))`.

- [ ] **Step 1: Write the failing wire test**

Append to `internal/dstack/client_test.go` (same package `dstack_test`, reuse the existing `getPlanPath` const and imports):

```go
// TestHTTPClient_Apply_SendsIdleDuration is the CR field that was validated
// as mandatory and then never sent (ledger D152). F21: on fresh
// provisioning dstack honours the shorter of the run's and the fleet's
// idle_duration, so the run MUST carry it or the machine outlives the job
// by dstack's own default.
func TestHTTPClient_Apply_SendsIdleDuration(t *testing.T) {
	var planBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case getPlanPath:
			if err := json.NewDecoder(r.Body).Decode(&planBody); err != nil {
				t.Errorf("decode get_plan body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"run_spec":{},"current_resource":null,"action":"create"}`))
		case "/api/project/main/runs/apply":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"abc","jobs":[],"run_spec":{"configuration":{"replicas":{"min":1,"max":1}}}}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	if _, err := c.Apply(context.Background(), dstack.ApplyRequest{
		Name: "qwen", Replicas: 1, Image: "img", Port: 8080,
		IdleDuration: 10 * time.Minute,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	cfg, _ := planBody["run_spec"].(map[string]any)["configuration"].(map[string]any)
	if got := cfg["idle_duration"]; got != "600s" {
		t.Fatalf("configuration.idle_duration = %v, want \"600s\": spec.fleet.idleDuration is not reaching dstack (D152)", got)
	}
}

func TestHTTPClient_Apply_OmitsIdleDurationWhenZero(t *testing.T) {
	var planBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case getPlanPath:
			_ = json.NewDecoder(r.Body).Decode(&planBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"run_spec":{},"current_resource":null,"action":"create"}`))
		case "/api/project/main/runs/apply":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"abc","jobs":[],"run_spec":{"configuration":{"replicas":{"min":1,"max":1}}}}`))
		}
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	if _, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 1, Image: "img", Port: 8080}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cfg, _ := planBody["run_spec"].(map[string]any)["configuration"].(map[string]any)
	if _, present := cfg["idle_duration"]; present {
		t.Fatalf("idle_duration must be omitted when unset, got %v — an empty string would be rejected by dstack's parse_duration", cfg["idle_duration"])
	}
}
```

If `time` is not already imported in `client_test.go`, add it.

- [ ] **Step 2: Run to verify it fails**

Run: `./scripts/dev.sh go test ./internal/dstack/ -run 'TestHTTPClient_Apply_.*IdleDuration' -v`
Expected: compile error `unknown field IdleDuration in struct literal of type dstack.ApplyRequest`.

- [ ] **Step 3: Add the field, the wire member, the helper, and thread them**

`internal/dstack/client.go` — inside `ApplyRequest`, after the `Placement Placement` field:

```go
	// IdleDuration is spec.fleet.idleDuration, sent as dstack's
	// `idle_duration` on the RUN configuration. F21: on fresh provisioning
	// dstack keeps the instance for the SHORTER of this and the fleet's
	// value; on reuse the fleet's value applies (see FleetSpec.IdleDuration).
	// Zero omits the field, which hands the decision to dstack's defaults
	// (5m for a run, 3d for a fleet) — never what a Model wants, which is
	// why the CRD makes the source field mandatory. Ledger D152.
	IdleDuration time.Duration
```

`internal/dstack/wire.go` — in `configurationWire`, after `MaxPrice`:

```go
	IdleDuration string `json:"idle_duration,omitempty"`
```

and add below the struct:

```go
// dstackDuration renders a Go duration in the "<int><unit>" form dstack's
// parse_duration accepts (units s/m/h/d/w). Always whole seconds: dstack
// normalises every duration to integer seconds server-side anyway
// (measured: probe interval "5s" -> 5). "" for d <= 0 so omitempty drops
// the field rather than sending "0s", which dstack would honour literally.
func dstackDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return fmt.Sprintf("%ds", int64(d/time.Second))
}
```

Add `"fmt"` and `"time"` to `wire.go`'s imports if absent.

`internal/dstack/http.go:252-270` — in the `runSpecWire{...}` literal, after `MaxPrice:  req.Placement.MaxPrice,`:

```go
			IdleDuration: dstackDuration(req.IdleDuration),
```

`internal/controller/squall/model_controller.go` — in the `r.DstackClient.Apply(ctx, dstack.ApplyRequest{...})` literal (around line 238), after `Placement: enginePlacement(model.Spec.Placement),`:

```go
			// D152: the CRD has required this since v0.1 and nothing sent it.
			IdleDuration: model.Spec.Fleet.IdleDuration.Duration,
```

- [ ] **Step 4: Run to verify it passes**

Run: `./scripts/dev.sh go test ./internal/dstack/ -run 'TestHTTPClient_Apply' -v`
Expected: PASS for both new tests and every pre-existing `TestHTTPClient_Apply_*`.

- [ ] **Step 5: Mutation check**

Comment out the `IdleDuration: dstackDuration(req.IdleDuration),` line in `http.go`. Re-run step 4. Expected: `TestHTTPClient_Apply_SendsIdleDuration` FAILS with `configuration.idle_duration = <nil>`. Restore the line.

- [ ] **Step 6: Ledger + CHANGELOG**

Append to `docs/references/deviations-and-findings.md` after the `D151` row:

```markdown
| **D152** | **`spec.fleet.idleDuration` was validated as mandatory and never sent to dstack.** `model_validation.go` rejects a Model without it and its CRD comment calls dstack's 3-day fleet default "the single most expensive footgun in the system" — but `configurationWire`, `fleetConfigurationWire` and the chart's fleet Secret all lacked `idle_duration`. Every Asleep since v0.1 left the instance to whatever the fleet YAML said; the chart's own fleets said nothing, i.e. 3 days. Found 2026-09-03 in the idle-guardian review. | The one squall-controlled knob that stops the BILL (F21: the flip stops the job, only `idle_duration` stops the machine) was decorative. | **FIXED 2026-09-03.** Run configuration now carries `idle_duration` (`ApplyRequest.IdleDuration`); auto-fleets carry it on creation (`FleetSpec.IdleDuration`, first Model's value wins — create-only, D83); chart fleets require `idleDuration`. Tests: `TestHTTPClient_Apply_SendsIdleDuration`, `TestHTTPClient_EnsureFleet_SendsIdleDuration`. Semantics caveat: F21's "VM-based backends only" (D9) still stands; and a shared auto-fleet cannot honour per-Model values on reuse — the run-level field bounds fresh provisioning, the fleet-level one bounds reuse at the creating Model's value. |
```

In `CHANGELOG.md`, replace `Nothing yet.` under `## [Unreleased]` with:

```markdown
### Fixed

- **Money:** `spec.fleet.idleDuration` is now actually sent to dstack — on the
  run configuration for every Apply and on every fleet squall creates.
  Until now the field was validated and then dropped, so after a Model went
  Asleep the rented instance lived for whatever the fleet said, which for
  the chart's own fleets was dstack's 3-day default (D152).
```

- [ ] **Step 7: Commit**

```bash
git add internal/dstack/client.go internal/dstack/wire.go internal/dstack/http.go internal/dstack/client_test.go internal/controller/squall/model_controller.go docs/references/deviations-and-findings.md CHANGELOG.md
git commit -m "fix(dstack): send spec.fleet.idleDuration as the run's idle_duration

The CRD required the field since v0.1 and nothing put it on the wire, so
the instance behind every Asleep Model lived for dstack's own default.
F21: on fresh provisioning dstack honours the shorter of run and fleet
values, so the run must carry it. Ledger D152.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Fleet-level `idle_duration` — auto-fleets and chart fleets

**Why:** F21: "on reuse the fleet's value applies". A warm instance reused for a second wake is governed by the *fleet's* `idle_duration`, not the run's. Squall creates auto-fleets (`squall-auto-<backend>`) and the chart declares operator fleets; both must carry it.

**Design decision (state in code comments, do not reopen):** auto-fleets are shared per backend and create-only (D83). The first Model that triggers creation supplies the value. This is a known ceiling: two Models with different `idleDuration` on the same backend share the first one's fleet value on reuse. The run-level field (Task 1) still bounds fresh provisioning per Model. Recorded in D152.

**Files:**
- Modify: `internal/dstack/client.go:181-186` (`FleetSpec`)
- Modify: `internal/dstack/wire.go:174-180` (`fleetConfigurationWire`)
- Modify: `internal/dstack/http.go:220-231` (`createFleet`)
- Modify: `internal/controller/squall/preflight.go:14-19, 43, 70-73`
- Modify: `internal/controller/squall/model_controller.go:860`
- Modify: `internal/controller/squall/preflight_test.go:94,114,125,133`
- Modify: `deploy/helm/squall/templates/dstack-config-secret.yaml:42-52`
- Modify: `deploy/helm/squall/values.yaml:497-520`
- Test: `internal/dstack/client_test.go`

**Interfaces:**
- Consumes: `dstackDuration` from Task 1.
- Produces: `dstack.FleetSpec.IdleDuration time.Duration`; `preflight(ctx, c, backends []string, idle time.Duration)`.

- [ ] **Step 1: Write the failing wire test**

Append to `internal/dstack/client_test.go` (reuse the existing `fleetGetPath` const):

```go
// TestHTTPClient_EnsureFleet_SendsIdleDuration — F21: on REUSE of a warm
// instance the FLEET's idle_duration governs, so an auto-fleet created
// without one inherits dstack's 3-day default for every later wake (D152).
func TestHTTPClient_EnsureFleet_SendsIdleDuration(t *testing.T) {
	var planBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case fleetGetPath:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":[{"msg":"Resource not found","code":"resource_not_exists"}]}`))
		case "/api/project/main/fleets/get_plan":
			if err := json.NewDecoder(r.Body).Decode(&planBody); err != nil {
				t.Errorf("decode get_plan body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"spec":{"configuration":{"type":"fleet","name":"squall-auto-vastai","nodes":"0..","backends":["vastai"]}}}`))
		case "/api/project/main/fleets/apply":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"squall-auto-vastai","status":"active"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	if err := c.EnsureFleet(context.Background(), dstack.FleetSpec{
		Name: "squall-auto-vastai", Backends: []string{"vastai"}, IdleDuration: 2 * time.Minute,
	}); err != nil {
		t.Fatalf("EnsureFleet: %v", err)
	}

	cfg, _ := planBody["spec"].(map[string]any)["configuration"].(map[string]any)
	if got := cfg["idle_duration"]; got != "120s" {
		t.Fatalf("fleet configuration.idle_duration = %v, want \"120s\" (D152)", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `./scripts/dev.sh go test ./internal/dstack/ -run TestHTTPClient_EnsureFleet_SendsIdleDuration -v`
Expected: compile error `unknown field IdleDuration in struct literal of type dstack.FleetSpec`.

- [ ] **Step 3: Add the field and thread it**

`internal/dstack/client.go` — in `FleetSpec`, after `Backends []string`:

```go
	// IdleDuration becomes the fleet's `idle_duration` (F21: on REUSE of a
	// warm instance the fleet's value governs, not the run's). Auto-fleets
	// are shared per backend and create-only (D83), so the FIRST Model that
	// causes creation sets it for everyone on that backend — a known
	// ceiling, recorded in D152. Zero omits the field (dstack default: 3d).
	IdleDuration time.Duration
```

`internal/dstack/wire.go` — in `fleetConfigurationWire`, after `Backends`:

```go
	IdleDuration string `json:"idle_duration,omitempty"`
```

`internal/dstack/http.go:223-229` — in the `fleetConfigurationWire{...}` literal, after `Backends:  spec.Backends,`:

```go
				IdleDuration: dstackDuration(spec.IdleDuration),
```

`internal/controller/squall/preflight.go`:
- Change the signature at line 43 to:
  ```go
  func preflight(ctx context.Context, c preflightClient, backends []string, idle time.Duration) (reason, message string, fleets []squallv1alpha1.FleetStatus) {
  ```
- Add `"time"` to its imports.
- At line 70-73 change the `EnsureFleet` call to:
  ```go
			if err := c.EnsureFleet(ctx, dstack.FleetSpec{
				Name:         dstack.FleetName(b),
				Backends:     []string{b},
				IdleDuration: idle,
			}); err != nil {
  ```

`internal/controller/squall/model_controller.go:860` — change the call to:

```go
	if reason, msg, fleets := preflight(ctx, r.DstackClient, enginePlacement(model.Spec.Placement).Backends, model.Spec.Fleet.IdleDuration.Duration); reason != "" {
```

`internal/controller/squall/preflight_test.go` — the four call sites at lines 94, 114, 125, 133 gain a trailing `, 0` argument (e.g. `preflight(context.Background(), tc.fake, tc.backends, 0)`). The existing `fakePreflight` (line 15) records only backend names in `ensured []string`; add a sibling field and record the whole spec:

```go
	// ensuredSpecs records the full FleetSpec of every EnsureFleet call, so
	// a test can assert what was threaded through (D152's IdleDuration).
	ensuredSpecs []dstack.FleetSpec
```

and in `EnsureFleet` (line 35), before the `return`:

```go
	f.ensuredSpecs = append(f.ensuredSpecs, spec)
```

Then append this test:

```go
// TestPreflight_ThreadsIdleDurationOntoTheAutoFleet — F21: on reuse the
// FLEET's idle_duration governs, so the auto-fleet squall creates must carry
// the Model's value or every later wake inherits dstack's 3-day default.
func TestPreflight_ThreadsIdleDurationOntoTheAutoFleet(t *testing.T) {
	fake := &fakePreflight{
		configured: map[string]bool{"vastai": true},
		fleets:     map[string]bool{"vastai": false},
	}
	preflight(context.Background(), fake, []string{"vastai"}, 7*time.Minute)
	if len(fake.ensuredSpecs) != 1 || fake.ensuredSpecs[0].IdleDuration != 7*time.Minute {
		t.Fatalf("EnsureFleet specs = %+v, want exactly one with IdleDuration 7m (D152)", fake.ensuredSpecs)
	}
}
```

Add `"time"` to the test file's imports if absent.

- [ ] **Step 4: Chart — render `idle_duration` per fleet, required**

`deploy/helm/squall/templates/dstack-config-secret.yaml:42-52` becomes:

```yaml
{{- range $i, $f := .Values.dstack.fleets }}
  fleet-{{ $i }}.dstack.yml: |
    type: fleet
    name: {{ $f.name }}
    nodes: {{ $f.nodes | quote }}
    # F21: the fleet's idle_duration is what releases the MACHINE after
    # squall flips the job to zero. dstack's default is 3 DAYS. Required,
    # no default — the same posture as the Model CRD's fleet.idleDuration.
    idle_duration: {{ required (printf "dstack.fleets[%d].idleDuration is required: it is what releases the instance after a Model sleeps, and dstack's own default is 3 days (F21, D152)" $i) $f.idleDuration | quote }}
    backends: {{ toYaml $f.backends | nindent 6 }}
    resources:
      cpu: {{ $f.resources.cpu | quote }}
      memory: {{ $f.resources.memory | quote }}
      disk: {{ $f.resources.disk | quote }}
{{- end }}
```

`deploy/helm/squall/values.yaml` — in the `fleets:` list, add to `kind-fleet` after `nodes: "0..2"`:

```yaml
      # How long dstack keeps an instance after the last job on it ends
      # (F21). REQUIRED: dstack's default is 3 days. Ignored on the
      # Kubernetes backend (no VM to release) but still required so the
      # value is never forgotten when this fleet is copied for a VM backend.
      idleDuration: 10m
```

and to the commented `vast-live` example after `#   nodes: "0.."`:

```yaml
    #   idleDuration: 10m
```

Check `test/e2e/cluster/helm-values.yaml`: it has no `fleets:` override (verified 2026-09-03), so it inherits the default. If a later edit added one, add `idleDuration` there too.

- [ ] **Step 5: Run scoped tests**

Run: `./scripts/dev.sh go test ./internal/dstack/ ./internal/controller/squall/ -short -run 'EnsureFleet|Preflight|preflight' -v`
Expected: PASS.

Run: `./scripts/dev.sh helm template squall deploy/helm/squall | grep -c 'idle_duration: "10m"'`
Expected: `1`.

Run: `./scripts/dev.sh helm template squall deploy/helm/squall --set 'dstack.fleets[0].name=x,dstack.fleets[0].nodes=0..,dstack.fleets[0].backends[0]=vastai,dstack.fleets[0].resources.cpu=1..,dstack.fleets[0].resources.memory=1GB..,dstack.fleets[0].resources.disk=10GB..' 2>&1 | grep -c 'idleDuration is required'`
Expected: `1` (render fails with the message).

- [ ] **Step 6: Mutation check**

Remove `IdleDuration: dstackDuration(spec.IdleDuration),` from `createFleet`. Run step 5's Go test. Expected: `TestHTTPClient_EnsureFleet_SendsIdleDuration` FAILS. Restore.

- [ ] **Step 7: Commit**

```bash
git add internal/dstack/client.go internal/dstack/wire.go internal/dstack/http.go internal/dstack/client_test.go internal/controller/squall/preflight.go internal/controller/squall/preflight_test.go internal/controller/squall/model_controller.go deploy/helm/squall/templates/dstack-config-secret.yaml deploy/helm/squall/values.yaml
git commit -m "fix(fleet): carry idle_duration on auto-fleets and chart fleets

F21: on reuse of a warm instance the fleet's idle_duration governs, so a
fleet created without one keeps every later instance for dstack's 3-day
default. Auto-fleets take the creating Model's value (create-only, D83);
chart fleets must declare idleDuration. Ledger D152.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## Block B — See the guardians fail (controller death, idle-but-awake)

### Task 4: `squall_model_idle_seconds` / `squall_model_scale_down_delay_seconds` gauge pair

**Why:** three past incidents (D91, D134, D139) were "Ready, idle, never slept". Nothing alerts on that class. The controller already persists `status.lastRequestAt`; exporting its age beside the declared window makes the class visible with the same "observed > declared" shape the other alerts use.

**Files:**
- Create: `internal/metrics/idle.go`
- Create: `internal/metrics/idle_test.go`
- Modify: `cmd/controller/main.go:186-190, 234-237`
- Modify: `internal/controller/squall/model_controller.go` — `ModelReconciler` struct (add field next to `UncontrolledMetrics`), `recordMetrics` at line 900
- Modify: `internal/controller/squall/finalizer.go:128-136` (Forget block)

**Interfaces:**
- Produces: `metrics.IdleCollector` with `NewIdleCollector(clk clock.Clock) *IdleCollector`, `Observe(namespace, name string, lastRequestAt *time.Time, runActive bool, scaleDownDelay time.Duration)`, `Forget(namespace, name string)`. Emits both gauges only when `runActive && lastRequestAt != nil`.
- Existing test helpers in package `metrics`: `gather(t, c)` and `findFamily(mfs, name)` (see `uncontrolled_test.go`).

- [ ] **Step 1: Write the failing test**

`internal/metrics/idle_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"testing"
	"time"

	"github.com/ackstorm/squall/internal/clock"
)

func TestIdleCollector_EmitsOnlyForActiveRunsWithAnAnchor(t *testing.T) {
	now := time.Now()
	fc := clock.NewFakeClock(now)
	c := NewIdleCollector(fc)

	// No live run: nothing to be idle-and-billing, so no series at all.
	last := now.Add(-10 * time.Minute)
	c.Observe("default", "qwen", &last, false, 5*time.Minute)
	if fam := findFamily(gather(t, c), "squall_model_idle_seconds"); fam != nil {
		t.Fatal("asleep Model must not emit idle seconds")
	}

	// Live run, no anchor yet (never woke, never served): still nothing —
	// the controller's own wake-seeding (D134) fills the anchor within one pass.
	c.Observe("default", "qwen", nil, true, 5*time.Minute)
	if fam := findFamily(gather(t, c), "squall_model_idle_seconds"); fam != nil {
		t.Fatal("no anchor must not emit a zero that reads as 'just used'")
	}

	// Live run with an anchor: observed age and declared window.
	c.Observe("default", "qwen", &last, true, 5*time.Minute)
	fc.Advance(2 * time.Minute)
	mfs := gather(t, c)
	if got := findFamily(mfs, "squall_model_idle_seconds").GetMetric()[0].GetGauge().GetValue(); got != 12*60 {
		t.Fatalf("idle seconds = %v, want 720", got)
	}
	if got := findFamily(mfs, "squall_model_scale_down_delay_seconds").GetMetric()[0].GetGauge().GetValue(); got != 5*60 {
		t.Fatalf("declared = %v, want 300", got)
	}

	c.Forget("default", "qwen")
	if fam := findFamily(gather(t, c), "squall_model_idle_seconds"); fam != nil {
		t.Fatal("forgotten series must disappear")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `./scripts/dev.sh go test ./internal/metrics/ -run TestIdleCollector -v`
Expected: compile error `undefined: NewIdleCollector`.

- [ ] **Step 3: Implement the collector**

`internal/metrics/idle.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ackstorm/squall/internal/clock"
)

// IdleCollector exports how long a Model with LIVE capacity has gone
// without a request (observed) beside its own idle window (declared). The
// controller's sleep flip should fire at roughly declared; an observed
// value far past it is the "Ready, idle, never slept" class that has
// already happened three times (D91, D134, D139) and was invisible each
// time. Observed is derived from status.lastRequestAt, the same durable
// anchor sleepDue reads — there is no second idleness sensor (D132).
type IdleCollector struct {
	clock        clock.Clock
	mu           sync.Mutex
	entries      map[modelKey]idleEntry
	observedDesc *prometheus.Desc
	declaredDesc *prometheus.Desc
}

type idleEntry struct {
	lastRequestAt time.Time
	window        time.Duration
}

func NewIdleCollector(clk clock.Clock) *IdleCollector {
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &IdleCollector{
		clock: clk, entries: make(map[modelKey]idleEntry),
		observedDesc: prometheus.NewDesc("squall_model_idle_seconds", "Seconds since the last request for a Model that currently has live capacity.", []string{"namespace", "name"}, nil),
		declaredDesc: prometheus.NewDesc("squall_model_scale_down_delay_seconds", "spec.scaleDownDelaySeconds: the idle window after which the controller should have flipped to zero.", []string{"namespace", "name"}, nil),
	}
}

// Observe records the Model's anchor. A Model with no live run, or with no
// anchor yet, is FORGOTTEN rather than emitted as zero: a zero would read
// as "used just now" and mask exactly the condition this exists to show.
func (c *IdleCollector) Observe(namespace, name string, lastRequestAt *time.Time, runActive bool, window time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := modelKey{namespace, name}
	if !runActive || lastRequestAt == nil || lastRequestAt.IsZero() {
		delete(c.entries, key)
		return
	}
	c.entries[key] = idleEntry{lastRequestAt: *lastRequestAt, window: window}
}

func (c *IdleCollector) Forget(namespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, modelKey{namespace, name})
}

func (c *IdleCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.observedDesc
	ch <- c.declaredDesc
}

func (c *IdleCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	for key, e := range c.entries {
		ch <- prometheus.MustNewConstMetric(c.observedDesc, prometheus.GaugeValue, now.Sub(e.lastRequestAt).Seconds(), key.Namespace, key.Name)
		ch <- prometheus.MustNewConstMetric(c.declaredDesc, prometheus.GaugeValue, e.window.Seconds(), key.Namespace, key.Name)
	}
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `./scripts/dev.sh go test ./internal/metrics/ -run TestIdleCollector -v`
Expected: PASS.

- [ ] **Step 5: Wire it into the controller**

`internal/controller/squall/model_controller.go` — in the `ModelReconciler` struct, directly after the `UncontrolledMetrics` field, add:

```go
	// IdleMetrics exports status.lastRequestAt's age beside
	// scaleDownDelaySeconds for Models with live capacity. nil disables.
	IdleMetrics *metrics.IdleCollector
```

Change `recordMetrics`'s signature (line 900) to take the live-capacity fact from the caller rather than re-deriving it from status:

```go
func (r *ModelReconciler) recordMetrics(model *squallv1alpha1.Model, observedPerHour float64, uncontrolledSince *metav1.Time, runActive bool, now time.Time) {
```

and change its single call site (line 459) to:

```go
	r.recordMetrics(&model, observedPerHour, metricUncontrolledSince, observed.Run != nil && observed.Run.Replicas > 0, now)
```

(`grep -rn 'recordMetrics(' internal/` shows no test calls it directly.) Then, inside `recordMetrics` after the `UncontrolledMetrics` block:

```go
	if r.IdleMetrics != nil {
		var last *time.Time
		if model.Status.LastRequestAt != nil {
			t := model.Status.LastRequestAt.Time
			last = &t
		}
		// runActive is observed.Run.Replicas > 0 from THIS pass — the same
		// fact gatherActivity gates on, so the gauge exists exactly when a
		// sleep decision is being made.
		r.IdleMetrics.Observe(model.Namespace, model.Name, last, runActive,
			time.Duration(model.Spec.ScaleDownDelaySeconds)*time.Second)
	}
```

`internal/controller/squall/finalizer.go` — after the `UncontrolledMetrics.Forget` block:

```go
	if r.IdleMetrics != nil {
		r.IdleMetrics.Forget(model.Namespace, model.Name)
	}
```

`cmd/controller/main.go:186-190` — add `idleMetrics := metrics.NewIdleCollector(internalclock.RealClock{})` and include `idleMetrics` in the `MustRegister(...)` call. At lines 234-237 add `IdleMetrics: idleMetrics,` to the `ModelReconciler` literal.

- [ ] **Step 6: Build check**

Run: `./scripts/dev.sh go build ./... && ./scripts/dev.sh go vet ./internal/controller/squall/ ./cmd/controller/`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add internal/metrics/idle.go internal/metrics/idle_test.go cmd/controller/main.go internal/controller/squall/model_controller.go internal/controller/squall/finalizer.go
git commit -m "feat(metrics): export idle age beside scaleDownDelay for live capacity

Three incidents (D91, D134, D139) were 'Ready, idle, never slept' and
nothing showed it. status.lastRequestAt is the anchor sleepDue already
reads; exporting its age next to the declared window makes the class
alertable with the same observed > declared shape.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Alerts for idle-but-awake and for a dead controller; second controller replica

**Why:** every guardian and every existing alert lives in one leader-elected pod. If it dies, its series vanish and every `>` rule evaluates to *nothing*. controller-runtime itself exports `controller_runtime_active_workers{controller="squall-model"}` from the same registry (the controller is `Named("squall-model")` in `SetupWithManager`), so `absent()` over it is a heartbeat that exists even with zero Models.

**Files:**
- Modify: `config/prometheus/rules.yaml` (append two rules to the single group)
- Modify: `config/prometheus/rules_test.go:52-54, 66-78`
- Regenerate: `deploy/helm/squall/files/prometheus-rule-spec.yaml` via `make helm-sync`
- Modify: `deploy/helm/squall/values.yaml:44` (`controller.replicas`)

- [ ] **Step 1: Update the rules test first**

In `config/prometheus/rules_test.go` change the count assertion:

```go
	if len(rules) != 7 {
		t.Fatalf("rules = %d, want exactly 7 (age, price, provisioning loop, uncontrolled, idle capacity, idle-but-awake, controller absent)", len(rules))
	}
```

and add two entries to `wantExprs`:

```go
		// live capacity has sat idle far past its own window: the sleep
		// guardian is not firing — D91/D134/D139's class, finally visible
		"squall_model_idle_seconds > 3 * squall_model_scale_down_delay_seconds": false,
		// the controller stopped reporting at all: EVERY 1->0 path lives in
		// it, and every other rule here silently evaluates to nothing
		"absent(controller_runtime_active_workers{controller=\"squall-model\"}) > 0": false,
```

- [ ] **Step 2: Run to verify it fails**

Run: `./scripts/dev.sh go test ./config/prometheus/ -v`
Expected: FAIL `rules = 5, want exactly 7`.

- [ ] **Step 3: Add the rules**

Append to the `rules:` list in `config/prometheus/rules.yaml` (same indentation as the existing `SquallModelCapacityWhileNotReady` entry):

```yaml
        - alert: SquallModelIdleButAwake
          expr: squall_model_idle_seconds > 3 * squall_model_scale_down_delay_seconds
          for: 5m
          labels:
            severity: critical
          annotations:
            summary: Model {{ $labels.name }} has live capacity and no requests for 3x its idle window
            description: >-
              status.lastRequestAt is more than three scaleDownDelaySeconds
              old while a run is up. The idle-sleep guardian should have
              flipped this to zero; it has not. Either its evidence is
              incomplete (see SquallModelUncontrolledApproachingDeadline),
              a request is pinned in flight (squall_proxy_requests_in_flight),
              or the flip is broken. Every minute is a GPU bill (D91, D134, D139).
        - alert: SquallControllerAbsent
          expr: absent(controller_runtime_active_workers{controller="squall-model"}) > 0
          for: 10m
          labels:
            severity: critical
          annotations:
            summary: squall-controller is not reporting
            description: >-
              No scrape has seen the Model controller for 10m. EVERY 1->0
              path — idle sleep, unhealthy teardown, uncontrolled deadline,
              provisioning timeout, finalizer drain, orphan reaper — runs in
              that process, and every other squall alert compares series it
              emits, so they are all silent right now. Any Model that was
              awake is billing with nothing able to stop it (spec §5.2 forbids
              a dstack max_duration). Restore the controller first, then
              check dstack ps against the provider invoice (AC7).
```

- [ ] **Step 4: Regenerate the chart copy and run the test**

Run: `./scripts/dev.sh make helm-sync`
Expected: `deploy/helm/squall/files/prometheus-rule-spec.yaml` now contains both new alerts (`grep -c 'alert: Squall' deploy/helm/squall/files/prometheus-rule-spec.yaml` → `7`). Note `make helm-sync` also regenerates the CRD copies; if it produces an unrelated CRD diff, that diff is pre-existing drift — include it in this commit and say so in the body.

Run: `./scripts/dev.sh go test ./config/prometheus/ -v`
Expected: PASS.

- [ ] **Step 5: Second controller replica**

`deploy/helm/squall/values.yaml:42-45` — change `replicas: 1` to:

```yaml
  # Two, not one: every 1->0 guardian runs in this process and the spec
  # forbids a dstack-side dead-man's switch (§5.2, no max_duration). Leader
  # election is already on, and the Reaper declares NeedLeaderElection, so
  # the standby actuates nothing until it wins the lease (D155).
  replicas: 2
```

Verify nothing in the controller runs outside leader election: `grep -n 'NeedLeaderElection' internal/controller/squall/*.go cmd/controller/*.go` must show `Reaper.NeedLeaderElection() bool { return true }` and no `return false`. The Model controller itself is leader-gated by controller-runtime by default.

- [ ] **Step 6: Ledger + CHANGELOG**

Append to the ledger:

```markdown
| **D155** | **A dead controller removes every guardian AND every alert.** Every 1->0 path (`sleepDue`, `unhealthyDue`, `uncontrolledDue`, `provisioningDue`, the finalizer, the reaper) runs in one leader-elected pod; spec §5.2 forbids a dstack `max_duration`, so nothing on the run itself bounds it; and all five alert rules compare series that pod emits, so its death made them evaluate to nothing rather than fire. Found 2026-09-03. | The owner's requirement — "a GPU we launched for a scale-to-zero Model must be gone in the configured or a prudent time, whatever happens" — cannot be met by squall alone while §5.2 stands. | **PARTIALLY CLOSED 2026-09-03:** `SquallControllerAbsent` (absent() over controller-runtime's own worker gauge, 10m, critical) and `controller.replicas: 2`. **OPEN — OWNER DECISION:** whether to permit a large `max_duration` (the v0.7 24h hard_stop that v0.8 reversed) as a last-resort dead-man's switch, accepting F20's "scheduled outage plus full recreate" cost. Not made here; spec wins. |
```

CHANGELOG, under `## [Unreleased]`, add:

```markdown
### Added

- `squall_model_idle_seconds` / `squall_model_scale_down_delay_seconds`
  gauge pair, and two alerts: `SquallModelIdleButAwake` (live capacity idle
  past 3x its window — the guardian that should have slept it is not
  firing) and `SquallControllerAbsent` (no scrape has seen the controller
  for 10m — every 1->0 path lives there and every other alert is silent
  without it). Both behind `prometheusRule.enabled`.

### Changed

- `controller.replicas` defaults to 2. Leader election was already on; the
  standby actuates nothing until it holds the lease.
```

- [ ] **Step 7: Commit**

```bash
git add config/prometheus/rules.yaml config/prometheus/rules_test.go deploy/helm/squall/files/prometheus-rule-spec.yaml deploy/helm/squall/values.yaml docs/references/deviations-and-findings.md CHANGELOG.md
git commit -m "feat(alerts): fire on idle-but-awake capacity and on an absent controller

Every 1->0 guardian and every existing alert is computed by one pod. Its
death silenced all of them. absent() over controller-runtime's own worker
gauge is a heartbeat that exists with zero Models; the idle-age pair makes
the D91/D134/D139 class visible. Second replica for redundancy. D155.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## Block C — Bound the request path (the zombie stream)

### Task 7: Per-request ceiling in squall-proxy

**Why:** `ActivityTracker.Begin`'s `done()` runs when the handler returns. The proxy `http.Server` has no timeouts; the tunnel `http.Client` has none by design (streaming). A client that opens a completion and never reads, or an engine that never finishes, keeps `inFlight > 0` forever. `sleepDue` then sees *complete, busy* evidence and correctly refuses to sleep; `unhealthyDue` needs failures and a hang is not one; `uncontrolledDue` needs *incomplete* evidence. One stuck request = GPU forever. D95's 57 hung requests ended at 305s only because dstack's proxy cut them; the SSH fast path has no such cut.

**Design:** one `context.WithTimeoutCause` around the whole request, applied only when a CR exists (the same gate `Begin` uses). Cancelling the request context aborts the hold (`Await` selects on `ctx.Done()`), aborts an in-flight `attemptForward` (built with `NewRequestWithContext`), and aborts `streamCommit` (the transport returns the context error from `Body.Read`). The handler returns, `done()` runs, evidence becomes idle. The ceiling is **not** charged as a replica failure — same posture as D99: a bound squall imposed says nothing certain about the replica, and `1→0 fails safe`.

**Ceiling value:** must exceed `holdTimeout + longest legitimate generation`. Representative production hold is 20m; measured generations 155–330s; D144 measured a 213s cold-wake hold. Default **45m**. `<= 0` disables (documented as "never in production").

**Files:**
- Modify: `internal/proxy/handler.go:47-55` (outcome consts), `:108-152` (`Handler` struct), `:253-256` (inside `if hasCR {`), `:384-400` (the `r.Context().Err() != nil` branch), `:549-552` (`commit`'s stream-error branch)
- Modify: `cmd/proxy/main.go:153-163`
- Modify: `deploy/helm/squall/templates/proxy-deployment.yaml:46-47` (add after `SQUALL_MAX_PENDING_PER_MODEL`)
- Modify: `deploy/helm/squall/values.yaml:161` (after `maxPendingPerModel`)
- Test: `internal/proxy/handler_test.go`

**Interfaces:**
- Produces: `Handler.MaxRequestDuration time.Duration`; `errRequestCeiling` (unexported sentinel); `outcomeCeiling = "request-ceiling"`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/proxy/handler_test.go`:

```go
// TestHandler_RequestCeiling_ReleasesInFlight is the zombie-stream hole: a
// request that never ends keeps inFlight > 0, which keeps sleepDue false
// with COMPLETE evidence — no other guardian can see it. The ceiling is the
// only bound. Both subtests run ServeHTTP in a goroutine with their own
// wall-clock cap so a broken ceiling fails instead of hanging the suite.
func TestHandler_RequestCeiling_ReleasesInFlight(t *testing.T) {
	serve := func(t *testing.T, h *Handler, req *http.Request) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		finished := make(chan struct{})
		go func() { h.ServeHTTP(rec, req); close(finished) }()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("ServeHTTP did not return within 5s: the request ceiling is not cutting the request")
		}
		return rec
	}

	t.Run("upstream streams forever -> cut at the ceiling, not a replica failure", func(t *testing.T) {
		released := make(chan struct{})
		defer close(released)
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				_, _ = w.Write([]byte("data: first\n\n"))
				f.Flush()
			}
			<-released // never finishes: the engine hung mid-generation
		}))
		defer backend.Close()

		u, err := url.Parse(backend.URL)
		if err != nil {
			t.Fatalf("parse backend url: %v", err)
		}
		c := NewCache()
		c.Set("m", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady, Schedulable: true})
		h := newHandler(t, c, u)
		h.MaxRequestDuration = 200 * time.Millisecond

		serve(t, h, chatRequest("m"))

		got := h.Activity.Report().Models["m"]
		if got.InFlight != 0 {
			t.Fatalf("inFlight = %d after the ceiling, want 0: the GPU would be pinned forever", got.InFlight)
		}
		if got.FailuresSinceSuccess != 0 {
			t.Fatalf("failuresSinceSuccess = %d, want 0: a bound squall imposed is not evidence against the replica (D99 posture)", got.FailuresSinceSuccess)
		}
	})

	t.Run("hold outlives the ceiling -> wait contract, inFlight released", func(t *testing.T) {
		c := NewCache()
		c.Set("m", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseWaking, Schedulable: true, HoldTimeout: time.Hour})
		h := newHandler(t, c, nil)
		h.MaxRequestDuration = 200 * time.Millisecond

		rec := serve(t, h, chatRequest("m"))

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 wait contract when the ceiling ends a hold", rec.Code)
		}
		if got := h.Activity.Report().Models["m"].InFlight; got != 0 {
			t.Fatalf("inFlight = %d, want 0", got)
		}
	})

	t.Run("zero disables the ceiling", func(t *testing.T) {
		c := NewCache()
		c.Set("m", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseWaking, Schedulable: true, HoldTimeout: 100 * time.Millisecond})
		h := newHandler(t, c, nil)
		h.MaxRequestDuration = 0
		start := time.Now()
		serve(t, h, chatRequest("m"))
		if time.Since(start) < 80*time.Millisecond {
			t.Fatal("with the ceiling disabled the hold must run to its own timeout")
		}
	})
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `./scripts/dev.sh go test ./internal/proxy/ -run TestHandler_RequestCeiling -v`
Expected: compile error `h.MaxRequestDuration undefined`.

- [ ] **Step 3: Implement**

`internal/proxy/handler.go`:

(a) Outcome constants, after `outcomeClientGone = "client-gone"` (line 55):

```go
	// outcomeCeiling: squall's own per-request ceiling ended the request.
	// Distinct from client-gone so a hung engine is diagnosable in the log
	// and NOT charged to the replica (a bound we imposed is not evidence).
	outcomeCeiling = "request-ceiling"
```

(b) Sentinel, near the other package-level `errors.New` (e.g. beside `errNoBackendURL`):

```go
// errRequestCeiling is the context cause when Handler.MaxRequestDuration
// ends a request. Checked via context.Cause so it is never confused with a
// caller hanging up (D99).
var errRequestCeiling = errors.New("squall-proxy: request ceiling reached")
```

(c) `Handler` struct, after `MaxPendingPerModel int`:

```go
	// MaxRequestDuration bounds ONE request end to end — hold, forward and
	// stream — for a Model that has a CR. It exists because in-flight
	// requests are what block the idle flip: a client that never reads or
	// an engine that never finishes keeps inFlight > 0 forever, and with
	// COMPLETE, busy evidence no controller guardian can fire (D153). It
	// must exceed the longest holdTimeout plus a legitimate generation;
	// cmd/proxy defaults it to 45m. <= 0 disables — never in production.
	MaxRequestDuration time.Duration
```

(d) Inside `if hasCR {` at line 253, BEFORE `done := h.Activity.Begin(model)`:

```go
		if h.MaxRequestDuration > 0 {
			ctx, cancel := context.WithTimeoutCause(r.Context(), h.MaxRequestDuration, errRequestCeiling)
			defer cancel()
			r = r.WithContext(ctx)
		}
```

(e) In the `res != attemptCommit` branch (line ~384), change the client-gone check to distinguish the cause. Replace:

```go
			if rerr := r.Context().Err(); rerr != nil {
```

with:

```go
			if rerr := r.Context().Err(); rerr != nil && !errors.Is(context.Cause(r.Context()), errRequestCeiling) {
```

and immediately after that whole `if ... { ... return }` block add:

```go
			if errors.Is(context.Cause(r.Context()), errRequestCeiling) {
				// Squall's own ceiling, not the caller and not the replica.
				// Nothing is charged to the health verdict; the caller gets
				// the wait contract so the state it sees is truthful.
				rec.outcome = outcomeCeiling
				rec.reason = errRequestCeiling.Error()
				slog.Warn("request ceiling reached before commit", "model", model, "ceiling", h.MaxRequestDuration.String())
				h.answerWait(w, Action{DeadlineStatus: http.StatusServiceUnavailable, DeadlineState: WaitWaking})
				return
			}
```

Also cover the hold-only path: right after `newSnap, newHasCR, _ := Await(holdCtx, ...)` and before `if committed != nil`, add:

```go
		if errors.Is(context.Cause(r.Context()), errRequestCeiling) && committed == nil {
			rec.outcome = outcomeCeiling
			rec.reason = errRequestCeiling.Error()
			slog.Warn("request ceiling reached while holding", "model", model, "ceiling", h.MaxRequestDuration.String())
			h.answerWait(w, Action{DeadlineStatus: http.StatusServiceUnavailable, DeadlineState: WaitWaking})
			return
		}
```

(f) In `commit` (line ~549), replace:

```go
	if err := streamCommit(w, resp); err != nil {
		slog.Warn("client disconnected mid-stream", "model", model, "err", err)
		return
	}
```

with:

```go
	if err := streamCommit(w, resp); err != nil {
		if errors.Is(context.Cause(resp.Request.Context()), errRequestCeiling) {
			// Headers are already on the wire; all we can do is stop
			// paying for it and say why. Not a replica failure (D153).
			rec.outcome = outcomeCeiling
			rec.reason = errRequestCeiling.Error()
			slog.Warn("request ceiling reached mid-stream; cutting the generation", "model", model, "ceiling", h.MaxRequestDuration.String())
			return
		}
		slog.Warn("client disconnected mid-stream", "model", model, "err", err)
		return
	}
```

(`resp.Request` is set by `http.Client.Do` to the outbound request, which `attemptForward` builds with `NewRequestWithContext(ctx, ...)` from `r.Context()` — so its context carries the cause.) Ensure `"context"` and `"errors"` are imported in `handler.go` (both already are).

`cmd/proxy/main.go:153-163` — add to the `proxy.Handler{...}` literal:

```go
		// D153: bounds one request end to end so a hung stream cannot pin
		// inFlight > 0 and keep a GPU awake forever. Must exceed the longest
		// Model holdTimeout plus a real generation (20m + 5m is the
		// representative production shape).
		MaxRequestDuration: envDuration("SQUALL_MAX_REQUEST_DURATION", 45*time.Minute),
```

`deploy/helm/squall/templates/proxy-deployment.yaml` — after the `SQUALL_MAX_PENDING_PER_MODEL` pair:

```yaml
          - name: SQUALL_MAX_REQUEST_DURATION
            value: {{ .Values.proxy.env.maxRequestDuration | quote }}
```

`deploy/helm/squall/values.yaml` — after `maxPendingPerModel: ""`:

```yaml
    # End-to-end ceiling on ONE request (hold + forward + stream), as a Go
    # duration. A request that never ends keeps the Model's in-flight count
    # above zero, and in-flight is what blocks scale-to-zero — so without
    # this one stuck stream pins a GPU forever (D153). Must exceed the
    # longest Model holdTimeout plus a real generation. Empty = 45m. "0"
    # disables; never do that where a bill can grow.
    maxRequestDuration: ""
```

- [ ] **Step 4: Run to verify it passes**

Run: `./scripts/dev.sh go test ./internal/proxy/ -run 'TestHandler_RequestCeiling|TestForwardFailure_ClientDisconnect|TestHandler_Hold|TestHandler_Ready' -v`
Expected: PASS, including the pre-existing D99 test (a real client disconnect must still be `client-gone`, not `request-ceiling`).

- [ ] **Step 5: Mutation check**

Remove the `r = r.WithContext(ctx)` line (keep the rest). Re-run step 4. Expected: both ceiling subtests FAIL via the 5s cap ("ServeHTTP did not return within 5s"). Restore.

- [ ] **Step 6: Ledger + CHANGELOG**

Ledger:

```markdown
| **D153** | **One request that never ends pinned a GPU forever.** `ActivityTracker.Begin`'s `done()` runs on handler return; the proxy `http.Server` has no timeouts and the tunnel client none by design (streaming). A client that stops reading or an engine that never finishes keeps `inFlight > 0`; `sleepDue` then sees COMPLETE busy evidence and correctly stays awake, `unhealthyDue` sees no failure, `uncontrolledDue` sees no gap. D95's 57 hung requests ended at 305s only because dstack's proxy cut them — the SSH fast path has no such cut. Found 2026-09-03. | The only guardian that can see this is a bound on the request itself. | **FIXED 2026-09-03.** `Handler.MaxRequestDuration` (`SQUALL_MAX_REQUEST_DURATION`, default 45m) wraps the whole request in `context.WithTimeoutCause`; hold, forward and stream all honour it; the ceiling is logged as `request-ceiling` and NOT charged as a replica failure (D99 posture). Test: `TestHandler_RequestCeiling_ReleasesInFlight` (three subtests); mutation (drop `WithContext`) red. Ceiling must exceed `holdTimeout` + a real generation — documented in values.yaml. |
```

CHANGELOG `### Added`:

```markdown
- **Can terminate a generation:** squall-proxy now bounds every request end
  to end (`proxy.env.maxRequestDuration`, default 45m). A request that never
  finished kept the Model's in-flight count above zero, and in-flight is what
  blocks scale-to-zero — one stuck stream was a GPU forever (D153). Set it
  above your longest `holdTimeout` plus a real generation.
```

- [ ] **Step 7: Commit**

```bash
git add internal/proxy/handler.go internal/proxy/handler_test.go cmd/proxy/main.go deploy/helm/squall/templates/proxy-deployment.yaml deploy/helm/squall/values.yaml docs/references/deviations-and-findings.md CHANGELOG.md
git commit -m "feat(proxy): bound every request end to end so a hung stream cannot pin a GPU

In-flight requests are what block the idle flip, and nothing bounded one.
A client that never reads or an engine that never finishes kept inFlight
above zero with complete evidence, which no controller guardian can act
on. Default 45m, not charged as a replica failure. Ledger D153.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## Block D — No opt-out to unbounded

### Task 8: `uncontrolledTimeout`: reject `<= 0`, cap explicit values at 24h, clamp at runtime

**Why:** the field's own comment says "zero opts out" and "an explicit value is not capped". Both contradict the requirement. Validation only runs on the wake path (`checkSchedulable`), so an already-awake Model edited to `0` would keep a bypass; the pure `uncontrolledTimeoutFor` must clamp too.

**Files:**
- Modify: `internal/controller/squall/phase.go:284-296` (`uncontrolledTimeoutFor`)
- Modify: `internal/controller/squall/phase_test.go` (`TestUncontrolledTimeoutFor_ExplicitValueIsNotCapped`, `TestUncontrolledTimeoutFor_ExplicitZeroIsOptOut`)
- Modify: `internal/controller/squall/model_validation.go:29-50`
- Modify: `internal/controller/squall/model_validation_test.go`
- Modify: `api/squall/v1alpha1/model_types.go:387-392` (doc comment)
- Regenerate: `config/crd/bases/squall.ackstorm.ai_models.yaml`, `deploy/helm/squall/crds/squall.ackstorm.ai_models.yaml`

**Interfaces:**
- Produces: `MaxExplicitUncontrolledTimeout = 24 * time.Hour` (exported const in `phase.go`, used by `model_validation.go`).

- [ ] **Step 1: Rewrite the two phase tests to the new contract**

In `phase_test.go`, replace `TestUncontrolledTimeoutFor_ExplicitZeroIsOptOut` and `TestUncontrolledTimeoutFor_ExplicitValueIsNotCapped` (find them by name) with:

```go
// An explicit zero used to mean "opt out"; there is no opting out of a bound
// on a GPU bill. Zero (and negative) fall back to the default derivation.
func TestUncontrolledTimeoutFor_ExplicitZeroFallsBackToDefault(t *testing.T) {
	zero := metav1.Duration{Duration: 0}
	spec := squallv1alpha1.ModelSpec{ScaleDownDelaySeconds: 120, UncontrolledTimeout: &zero}
	if got := uncontrolledTimeoutFor(spec); got != 23*time.Minute {
		t.Fatalf("explicit 0: got %v, want the 23m default (4x120s + 15m)", got)
	}
}

// An explicit value is honoured up to a ceiling; beyond it the ceiling wins.
func TestUncontrolledTimeoutFor_ExplicitValueIsCappedAt24h(t *testing.T) {
	six := metav1.Duration{Duration: 6 * time.Hour}
	if got := uncontrolledTimeoutFor(squallv1alpha1.ModelSpec{UncontrolledTimeout: &six}); got != 6*time.Hour {
		t.Fatalf("explicit 6h: got %v, want 6h (above the 2h DEFAULT cap, below the 24h explicit cap)", got)
	}
	week := metav1.Duration{Duration: 7 * 24 * time.Hour}
	if got := uncontrolledTimeoutFor(squallv1alpha1.ModelSpec{UncontrolledTimeout: &week}); got != MaxExplicitUncontrolledTimeout {
		t.Fatalf("explicit 7d: got %v, want the %v cap", got, MaxExplicitUncontrolledTimeout)
	}
}
```

- [ ] **Step 2: Add validation tests**

Append to `internal/controller/squall/model_validation_test.go`. The file's existing valid-spec helper is `exampleModelSpec()` (line 19); use it directly:

```go
func TestValidate_UncontrolledTimeoutBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		d       time.Duration
		wantErr bool
	}{
		{"zero is rejected: no opt-out", 0, true},
		{"negative is rejected", -time.Minute, true},
		{"24h is the inclusive maximum", 24 * time.Hour, false},
		{"above 24h is rejected", 25 * time.Hour, true},
		{"ordinary value passes", 90 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := exampleModelSpec()
			spec.UncontrolledTimeout = &metav1.Duration{Duration: tc.d}
			_, err := ValidateWithWarnings(spec)
			if (err != nil) != tc.wantErr {
				t.Fatalf("uncontrolledTimeout %v: err = %v, wantErr %v", tc.d, err, tc.wantErr)
			}
		})
	}
	spec := exampleModelSpec()
	spec.UncontrolledTimeout = nil
	if _, err := ValidateWithWarnings(spec); err != nil {
		t.Fatalf("nil (defaulted) must pass: %v", err)
	}
}
```

- [ ] **Step 3: Run to verify they fail**

Run: `./scripts/dev.sh go test ./internal/controller/squall/ -short -run 'TestUncontrolledTimeoutFor|TestValidate_UncontrolledTimeoutBounds' -v`
Expected: compile error `undefined: MaxExplicitUncontrolledTimeout`.

- [ ] **Step 4: Implement**

`phase.go` — replace the constants block and `uncontrolledTimeoutFor`:

```go
const DefaultUncontrolledGrace = 15 * time.Minute

// MaxUncontrolledTimeout caps the DERIVED default (D129).
const MaxUncontrolledTimeout = 2 * time.Hour

// MaxExplicitUncontrolledTimeout caps an operator-supplied value. There is no
// opt-out: "uncontrolled" means a GPU is billing and squall cannot see
// whether anyone is using it, and no configuration may make that unbounded
// (D154). Validation rejects values outside (0, 24h]; this clamp is the
// defence for a Model that was already awake when its spec was edited,
// since validation only runs on the wake path.
const MaxExplicitUncontrolledTimeout = 24 * time.Hour

func uncontrolledTimeoutFor(spec squallv1alpha1.ModelSpec) time.Duration {
	if spec.UncontrolledTimeout != nil && spec.UncontrolledTimeout.Duration > 0 {
		d := spec.UncontrolledTimeout.Duration
		if d > MaxExplicitUncontrolledTimeout {
			d = MaxExplicitUncontrolledTimeout
		}
		return d
	}
	d := 4*time.Duration(spec.ScaleDownDelaySeconds)*time.Second + DefaultUncontrolledGrace
	if d > MaxUncontrolledTimeout {
		d = MaxUncontrolledTimeout
	}
	return d
}
```

`model_validation.go` — in `ValidateWithWarnings`, after the `fleet.idleDuration` check:

```go
	if u := spec.UncontrolledTimeout; u != nil {
		if u.Duration <= 0 {
			return nil, fmt.Errorf("uncontrolledTimeout must be > 0 (omit it for the default): zero used to opt out, and there is no opting out of a bound on a GPU that squall cannot see (D154)")
		}
		if u.Duration > MaxExplicitUncontrolledTimeout {
			return nil, fmt.Errorf("uncontrolledTimeout (%s) exceeds the %s maximum (D154)", u.Duration, MaxExplicitUncontrolledTimeout)
		}
	}
```

`api/squall/v1alpha1/model_types.go:387-392` — replace the `UncontrolledTimeout` doc comment with:

```go
	// UncontrolledTimeout bounds how long capacity may stay up while idle
	// evidence is unavailable. Nil defaults to min(4x the idle window + 15m,
	// 2h). An explicit value must be in (0, 24h]: zero is REJECTED (there is
	// no opt-out from a bound on a GPU squall cannot see, D154) and larger
	// values are rejected on the wake path and clamped to 24h at runtime.
	// +optional
	UncontrolledTimeout *metav1.Duration `json:"uncontrolledTimeout,omitempty"`
```

- [ ] **Step 5: Regenerate CRDs and run scoped tests**

Run: `./scripts/dev.sh make gen-manifests && ./scripts/dev.sh make helm-sync`
Expected: both CRD copies pick up the new comment text only.

Run: `./scripts/dev.sh go test ./internal/controller/squall/ -short -run 'TestUncontrolledTimeoutFor|TestValidate|TestUncontrolledDue|TestUpdateActivityStatus' -v`
Expected: PASS. If `TestUpdateActivityStatus_*` or any envtest case set `UncontrolledTimeout: 0` expecting opt-out, update them to expect the default — search: `grep -n 'UncontrolledTimeout' internal/controller/squall/*_test.go`.

- [ ] **Step 6: Mutation check**

In `uncontrolledTimeoutFor`, change `> 0` to `>= 0` in the first condition. Expected: `TestUncontrolledTimeoutFor_ExplicitZeroFallsBackToDefault` FAILS (`got 0s`). Restore.

- [ ] **Step 7: Ledger + CHANGELOG**

Ledger:

```markdown
| **D154** | **`uncontrolledTimeout: 0` was a documented opt-out to unbounded, and explicit values had no cap.** Combined with the ways evidence goes missing that the deadline exists for (proxy unreachable, RBAC, `ProxyService` unset — D148's class), one field turned "uncontrolled" into "forever". Found 2026-09-03. | Nothing an operator writes may make an unobservable GPU unbounded. | **FIXED 2026-09-03.** Validation rejects values outside (0, 24h]; `uncontrolledTimeoutFor` clamps at runtime for Models already awake when edited. Zero now falls back to the derived default. Tests: `TestValidate_UncontrolledTimeoutBounds`, `TestUncontrolledTimeoutFor_ExplicitZeroFallsBackToDefault`, `TestUncontrolledTimeoutFor_ExplicitValueIsCappedAt24h`. |
```

CHANGELOG `### Changed`:

```markdown
- `spec.uncontrolledTimeout: 0` no longer disables the uncontrolled-capacity
  deadline; it is rejected on the wake path and treated as "use the default"
  for a Model already awake. Explicit values are capped at 24h (D154).
```

- [ ] **Step 8: Commit**

```bash
git add internal/controller/squall/phase.go internal/controller/squall/phase_test.go internal/controller/squall/model_validation.go internal/controller/squall/model_validation_test.go api/squall/v1alpha1/model_types.go config/crd/bases/squall.ackstorm.ai_models.yaml deploy/helm/squall/crds/squall.ackstorm.ai_models.yaml docs/references/deviations-and-findings.md CHANGELOG.md
git commit -m "fix(controller): no opt-out from the uncontrolled-capacity deadline

Zero meant unbounded and explicit values had no cap. Reject outside
(0, 24h] on the wake path and clamp at runtime for Models already awake
when their spec is edited. Ledger D154.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## Block E — Hygiene and the bound matrix

### Task 9: Delete the obsolete commented-out reaper tests

**Files:**
- Modify: `internal/controller/squall/reaper_test.go:282-472`

- [ ] **Step 1: Confirm the block boundaries**

Run: `grep -nE '^/\*|^\*/' internal/controller/squall/reaper_test.go`
Expected: exactly `282:/* stubUtilisation is an obsolete engine probe test block.` and `472:*/`, and `wc -l` reports 472. If the numbers differ, adjust the range to those two lines inclusive.

- [ ] **Step 2: Delete lines 282–472 inclusive**

Use `sed -i '282,472d' internal/controller/squall/reaper_test.go`. Then remove any import that became unused (likely none — the block was already commented out, so its imports were already either used elsewhere or absent).

- [ ] **Step 3: Verify**

Run: `./scripts/dev.sh go test ./internal/controller/squall/ -short -run TestReaper -v`
Expected: the nine live `TestReaper_*` tests PASS; `go vet` clean.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/squall/reaper_test.go
git commit -m "test(reaper): drop 190 lines of commented-out Utilisation tests

The Reaper's utilisation probe was removed (D132: one idleness sensor,
the proxy); its tests stayed behind as a comment block and read as
coverage that does not exist.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 10: `TestDecide_BoundMatrix` — one table that names every bound

**Why:** the review's most useful artefact is a single place that says, for each way evidence can go wrong, which guardian fires and when. `Decide` is pure, so this runs under `test-unit` and doubles as documentation. It also pins Task 8's clamp through the public entry point.

**Files:**
- Modify: `internal/controller/squall/phase_test.go` (append)

- [ ] **Step 1: Write the table test**

```go
// TestDecide_BoundMatrix is the idle-capacity guardian map (2026-09-03
// review). One row per way the evidence can look for an AWAKE, on-demand
// Model, and the guardian that must answer it. If you add a 1->0 path, add
// a row. If a row stops flipping, a GPU bills until a human notices.
func TestDecide_BoundMatrix(t *testing.T) {
	now := time.Now()
	run := &dstack.Run{Name: "ns-m", RunID: "r1", DeploymentNum: 3, Replicas: 1}
	ago := func(d time.Duration) *metav1.Time { t := metav1.NewTime(now.Add(-d)); return &t }
	spec := squallv1alpha1.ModelSpec{
		MinReplicas:           0,
		ScaleDownDelaySeconds: 120,
		ProvisioningTimeout:   metav1.Duration{Duration: 20 * time.Minute},
		Health:                squallv1alpha1.ModelHealth{UnhealthyAfter: metav1.Duration{Duration: 10 * time.Minute}, FailureThreshold: 3},
	}
	idle := &ActivityEvidence{Complete: true, AllIdle: true, AnyData: true, NewestLastRequestAt: now.Add(-10 * time.Minute)}
	busy := &ActivityEvidence{Complete: true, AllIdle: false, AnyData: true, NewestLastRequestAt: now.Add(-1 * time.Second)}
	failing := &ActivityEvidence{Complete: true, AllIdle: true, AnyData: true,
		NewestLastRequestAt: now.Add(-30 * time.Second), NewestLastSuccessAt: now.Add(-30 * time.Minute), FailuresSinceSuccess: 5}
	incomplete := &ActivityEvidence{}

	zero := metav1.Duration{Duration: 0}
	specZeroUncontrolled := spec
	specZeroUncontrolled.UncontrolledTimeout = &zero

	pinned := spec
	pinned.MinReplicas = 1

	for _, tc := range []struct {
		name         string
		spec         squallv1alpha1.ModelSpec
		observed     Observed
		prior        squallv1alpha1.ModelStatus
		demand       bool
		wantPhase    squallv1alpha1.ModelPhase
		wantApply    bool
		wantReplicas int
		wantFlag     func(Action) bool
	}{
		{"idle past window -> sleep", spec,
			Observed{Run: run, Ready: true, Activity: idle}, squallv1alpha1.ModelStatus{WakeStartedAt: ago(time.Hour)}, false,
			squallv1alpha1.ModelPhaseAsleep, true, 0, func(a Action) bool { return !a.Unhealthy && !a.Uncontrolled }},
		{"idle past window, demand annotation still fresh -> sleep anyway (aggregation is the sole signal)", spec,
			Observed{Run: run, Ready: true, Activity: idle}, squallv1alpha1.ModelStatus{WakeStartedAt: ago(time.Hour)}, true,
			squallv1alpha1.ModelPhaseAsleep, true, 0, nil},
		{"busy -> stays (this is the row Task 7's request ceiling exists for)", spec,
			Observed{Run: run, Ready: true, Activity: busy}, squallv1alpha1.ModelStatus{WakeStartedAt: ago(time.Hour)}, false,
			squallv1alpha1.ModelPhaseReady, false, 1, nil},
		{"failing with traffic -> unhealthy sleep", spec,
			Observed{Run: run, Ready: true, Activity: failing}, squallv1alpha1.ModelStatus{WakeStartedAt: ago(time.Hour)}, true,
			squallv1alpha1.ModelPhaseAsleep, true, 0, func(a Action) bool { return a.Unhealthy }},
		{"evidence missing, no demand, past uncontrolled deadline -> uncontrolled sleep", spec,
			Observed{Run: run, Ready: true, Activity: incomplete}, squallv1alpha1.ModelStatus{WakeStartedAt: ago(2 * time.Hour), UncontrolledSince: ago(30 * time.Minute)}, false,
			squallv1alpha1.ModelPhaseAsleep, true, 0, func(a Action) bool { return a.Uncontrolled }},
		{"evidence missing, demand fresh -> stays", spec,
			Observed{Run: run, Ready: true, Activity: incomplete}, squallv1alpha1.ModelStatus{WakeStartedAt: ago(2 * time.Hour), UncontrolledSince: ago(30 * time.Minute)}, true,
			squallv1alpha1.ModelPhaseReady, false, 1, nil},
		{"evidence missing, uncontrolledTimeout: 0 -> default still bounds it (D154)", specZeroUncontrolled,
			Observed{Run: run, Ready: true, Activity: incomplete}, squallv1alpha1.ModelStatus{WakeStartedAt: ago(2 * time.Hour), UncontrolledSince: ago(30 * time.Minute)}, false,
			squallv1alpha1.ModelPhaseAsleep, true, 0, func(a Action) bool { return a.Uncontrolled }},
		{"never Ready past provisioningTimeout -> Dead, destroyed", spec,
			Observed{Run: run, Ready: false, Activity: incomplete}, squallv1alpha1.ModelStatus{WakeStartedAt: ago(25 * time.Minute)}, true,
			squallv1alpha1.ModelPhaseDead, true, 0, func(a Action) bool { return a.ProvisioningTimedOut && a.Alarm }},
		{"pinned, idle forever -> never sleeps (AC17; maxLifetime alert only)", pinned,
			Observed{Run: run, Ready: true, Activity: idle}, squallv1alpha1.ModelStatus{WakeStartedAt: ago(48 * time.Hour)}, false,
			squallv1alpha1.ModelPhaseReady, false, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phase, action := Decide(tc.observed, tc.prior, tc.spec, tc.demand, now)
			if phase != tc.wantPhase || action.Apply != tc.wantApply || (action.Apply && action.Replicas != tc.wantReplicas) {
				t.Fatalf("got phase=%s apply=%v replicas=%d; want phase=%s apply=%v replicas=%d",
					phase, action.Apply, action.Replicas, tc.wantPhase, tc.wantApply, tc.wantReplicas)
			}
			if tc.wantFlag != nil && !tc.wantFlag(action) {
				t.Fatalf("action flags wrong: %+v", action)
			}
		})
	}
}
```

Notes for the implementer: `Decide`'s `provisioningDue` check comes before the sleep checks, so the "never Ready" row needs `Ready: false`; the `failing` row needs `Ready: true` and traffic newer than the window (30s < 120s). If the `Health` field names differ from `UnhealthyAfter`/`FailureThreshold`, read `api/squall/v1alpha1/model_types.go` `ModelHealth` and adapt — the call in `phase.go:250-251` shows the exact names.

- [ ] **Step 2: Run**

Run: `./scripts/dev.sh go test ./internal/controller/squall/ -short -run TestDecide_BoundMatrix -v`
Expected: all nine rows PASS. If the "uncontrolledTimeout: 0" row fails, Task 8 was not applied.

- [ ] **Step 3: Mutation check (batch — this is the block's sweep)**

One at a time, apply, run this test, observe red, revert:
1. `phase.go` `sleepDue`: replace `return now.Sub(anchor) > ...` with `return true` → the "busy" row must still pass (AllIdle guards) but hmm — that mutation is caught by `TestSleepDue_LiveDataWinsOverTheDurableAnchor`; here instead delete the `!activity.AllIdle` guard → "busy -> stays" row goes red.
2. `phase.go` `uncontrolledDue`: drop `!hasDemand &&` → "evidence missing, demand fresh -> stays" goes red.
3. `phase.go` `Decide`: remove `spec.MinReplicas == 0 &&` before `sleepDue` → "pinned" row goes red.
4. `reaper.go`: comment out the `markedUID == ""` refusal → `TestReaper_LeavesUnmarkedRunsAlone` goes red (`go test ./internal/controller/squall/ -short -run TestReaper_LeavesUnmarkedRunsAlone`).
5. `finalizer.go`: skip `Stop` before `Delete` → `TestReconcile_Delete_StopsBeforeDeletingAnActiveRun` (envtest; run with `./scripts/dev.sh make test-envtest-fast` only for this one, or defer to Task 11's full gate and note it).

Record which mutations went red in the commit body.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/squall/phase_test.go
git commit -m "test(phase): bound matrix for every 1->0 guardian

One table, one row per evidence shape an awake on-demand Model can
present, naming the guardian that must answer it. Pins D154's clamp
through Decide. Mutations 1-4 from the plan went red.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 11: Gates, once

- [ ] **Step 1: Confirm the tree changed since the last full gate**

Run: `git log --oneline main..HEAD` — expect the seven commits from Tasks 1, 2, 4, 5, 7, 8, 9, 10 (Task 3 and 6 were folded). If HEAD equals the last green gate, skip.

- [ ] **Step 2: Run the three gates plus chart drift**

```bash
./scripts/dev.sh make test-unit
./scripts/dev.sh make test-envtest
./scripts/dev.sh make qa-lint
./scripts/dev.sh make helm-sync-check
```

Expected: all green. `helm-sync-check` green proves the CRD and rule copies in the chart match their sources.

- [ ] **Step 3: If anything is red**

Fix forward in a new commit on the task that owns the file. Never amend. A block does not close red.

- [ ] **Step 4: Final report (no commit)**

State in the hand-off message: the seven commit SHAs; the mutation results per task; that `D155` remains OPEN as an owner decision (`max_duration`); and that the cluster-level kill matrix (controller/proxy/dstack down against real capacity) is deliberately out of this plan and belongs to a follow-up e2e plan.

---

## Explicitly out of scope (do not do these here)

- **dstack `max_duration` as dead-man's switch.** Spec §5.2 forbids it. Recorded in D155 as an owner decision.
- **Cluster kill-matrix e2e** (scale controller to 0 with live capacity, kill proxy, kill dstack, revoke EndpointSlice RBAC, and assert time-to-zero). Valuable, but needs a fake-dstack or real-dstack fixture redesign; separate plan.
- **Provider-side orphan triangulation** after a dstack DB wipe (D83 class). Manual drill AC7 by spec.
- **Per-Model fleet idle on shared auto-fleets.** Documented ceiling in D152; the fix would be per-Model fleets, a design change the owner rejected once already (decisions-and-open-items, "Fleet CR").
- **D101** (fleet idle timer not resetting on reuse). External dstack behaviour; errs toward releasing early.

## Self-review (done by the plan author, 2026-09-03)

- **Coverage vs review findings:** (1) Tasks 1–2; (2) Task 5 (visibility + redundancy; actuation left to owner, stated); (3) Task 7; (4) Task 8; (5) Tasks 4–5; (6) Task 9. Bound matrix: Task 10.
- **Placeholders:** none. Every code step has the code; every helper name (`exampleModelSpec`, `fakePreflight.ensuredSpecs`, `gather`/`findFamily`, `fleetGetPath`, `getPlanPath`) was verified against the tree on 2026-09-03.
- **Type consistency:** `dstackDuration` (Task 1) used in Task 2; `MaxExplicitUncontrolledTimeout` (Task 8) used in Task 8 validation and Task 10; `IdleCollector.Observe(ns, name, *time.Time, bool, time.Duration)` identical in Task 4 file, wiring, and test; `preflight(..., idle time.Duration)` identical across preflight.go, model_controller.go, preflight_test.go; `errRequestCeiling`/`outcomeCeiling`/`MaxRequestDuration` identical across handler.go, handler_test.go, main.go.
- **Spec conflicts surfaced, not hidden:** §5.2 `max_duration` (Global Constraints, Task 5, D155).
