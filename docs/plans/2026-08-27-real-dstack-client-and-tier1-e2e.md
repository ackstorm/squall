# Real dstack client + Tier-1 `e2e-local` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `internal/dstack` speak the protocol a real dstack server actually speaks, and prove it against a real dstack server driving real workloads through its Kubernetes backend on the e2e kind cluster.

**Architecture:** The client stops being a hand-rolled protocol and becomes a narrow adapter over dstack's real API: every operation is a `POST` under `/api/project/{project}/runs/...`, `Apply` becomes `get_plan` → `apply`, F18's CAS anchor becomes the whole previous `Run` object round-tripped verbatim, and errors are classified from the **body** (`detail[].code` / message) rather than the status line. Readiness is derived (`success_streak >= ready_after`) because dstack exposes no boolean. The fake is rewritten to the same shape so unit and envtest keep their speed, and a real dstack server in the kind cluster — Kubernetes backend, no GPU, no gateway — is what proves the fake is not lying.

**Tech Stack:** Go 1.26.6 via `./scripts/dev.sh`; `dstackai/dstack:0.21.2`; kind; Ginkgo v2 for e2e.

**REVISED 2026-08-27 after measuring a real dstack end to end.** The first draft was written from
source reading. Running it invalidated parts of it: task 2's readiness derivation was wrong,
`runs/list` was on the wrong router, and a defect in the already-shipped serving path surfaced
(D44). Everything below now cites measurements in `docs/references/dstack-real-api.md` §8-§9.

**Runs in parallel with** `docs/plans/2026-08-27-helm-chart-and-dstack-in-cluster.md`, which owns
the cluster. File ownership is disjoint and must stay disjoint:

| Owner | Tree |
|---|---|
| **This plan** | `internal/**`, `cmd/**`, `api/**`, `test/e2e/*_test.go` |
| The Helm brief | `deploy/helm/**`, `test/e2e/cluster/**`, `hack/cluster.sh`, the `Makefile`'s cluster/helm/e2e targets |

**Squall does NOT need a fleet API.** dstack requires a fleet per run (D45) and on the Kubernetes
backend it must have `nodes` target 0 — but a run matches an existing fleet automatically, as
measured. The chart creates the fleet; this client never touches one. What this plan owes is a
loud failure when none exists: `failed_to_start_due_to_no_capacity` with "No matching fleet
found" must not be swallowed as a generic error.

## Global Constraints

- **Every** `go` / `make` / `kubectl` invocation goes through `./scripts/dev.sh`. Never bare `go`. Never `DOCKER_BUILDKIT=0`.
- **Squall never sends `force: true`.** Real dstack makes `force` a **required** field with no default, so the guarantee changes shape: encode the **literal** `false` at the single encode site and expose **no** `Force` field on any exported struct. A caller must have nothing to set. (§5.2, AC13, F18.)
- **`0→1` fails open, `1→0` fails safe.** Wake may tolerate uncertainty; sleep must not.
- **Pin the dstack version.** `dstackai/dstack:0.21.2`, never `:latest` — this whole plan is written against measurements from that exact version (`docs/references/dstack-real-api.md` §8).
- Never `git add -A`; never `git reset`/`amend`/rebase past a commit you did not create; stay inside your named file tree.
- Tests: `make test-unit` must never need a control plane. e2e stays behind `//go:build e2e`.
- Gates run **once, at the block boundary** — not per task.

**The reference every task depends on:** `docs/references/dstack-real-api.md`. It carries the measured request/response shapes, the error-code table, and the file:symbol citation for each claim. Read it before task 1.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/dstack/client.go` | The `Client` interface, `Run`, `ApplyRequest`, error sentinels. Shrinks: wire types move out. |
| `internal/dstack/wire.go` | **New.** The real JSON shapes (`runSpec`, `configuration`, `applyRequest`, `runResponse`, `errorEnvelope`) and nothing else. One file to diff against upstream when dstack changes. |
| `internal/dstack/errors.go` | **New.** `classifyError(status int, body []byte) error` — the body-first mapping. Pure, table-tested. |
| `internal/dstack/probes.go` | **New.** `probesReady(jobs []jobWire, readyAfter int) bool` — the derivation dstack does not give us. Pure. |
| `internal/dstack/http.go` | **New.** `HTTPClient`, the four operations, the two-step apply. |
| `internal/dstack/mock/mock.go` | The fake's state machine — mostly survives; its outputs change. |
| `internal/dstack/mock/http.go` | The fake's HTTP surface — rewritten to the real paths, the real error envelope, the real status codes. |
| `internal/proxy/attempt.go` | The wake-vs-dead disambiguation D44 requires. |
| `test/e2e/dstack_test.go` | **New.** Tier-1 specs: wire shape, in-place flip, CAS, dead≠asleep, the 404 wake window. |

---

### Task 1: Classify dstack errors from the body, not the status line

This is first because it is the smallest change with the largest blast radius, and because
every later task's failure mode is unreadable without it.

**Files:**
- Create: `internal/dstack/errors.go`
- Create: `internal/dstack/errors_test.go`
- Modify: `internal/dstack/client.go` (delete `decodeOrMapError`'s status-code switch)

**Interfaces:**
- Produces: `classifyError(status int, body []byte) error`, returning `nil`, `ErrNotFound`, `ErrResourceChanged`, `ErrUnauthorized`, or a wrapped generic error.
- Consumes: nothing.

- [ ] **Step 1: Write the failing test**

Create `internal/dstack/errors_test.go`:

```go
// SPDX-License-Identifier: MIT

package dstack

import (
	"errors"
	"testing"
)

// TestClassifyError pins the MEASURED error contract of dstack 0.21.2
// (docs/references/dstack-real-api.md §8.1). dstack answers HTTP 400 for
// BOTH "not found" and "CAS conflict" — the status line cannot distinguish
// them, so the body must. Keying off 404/409, as this client did before,
// silently disables F20 and F18 against a real server.
func TestClassifyError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{
			name:   "200 is not an error",
			status: 200,
			body:   `{"id":"x"}`,
			want:   nil,
		},
		{
			name:   "400 + resource_not_exists is ErrNotFound",
			status: 400,
			body:   `{"detail":[{"msg":"Run not found","code":"resource_not_exists"}]}`,
			want:   ErrNotFound,
		},
		{
			name:   "400 + resource-has-been-changed is ErrResourceChanged",
			status: 400,
			body:   `{"detail":[{"msg":"Failed to apply plan. Resource has been changed. Try again or use force apply.","code":"error"}]}`,
			want:   ErrResourceChanged,
		},
		{
			name:   "403 + object detail is ErrUnauthorized",
			status: 403,
			body:   `{"detail":{"msg":"Invalid token","code":null}}`,
			want:   ErrUnauthorized,
		},
		{
			name:   "400 with an unrecognised code is a generic error",
			status: 400,
			body:   `{"detail":[{"msg":"something else entirely","code":"error"}]}`,
			want:   nil, // asserted separately: non-nil, but none of the sentinels
		},
		{
			name:   "500 with an unparseable body is still an error",
			status: 500,
			body:   `<html>gateway timeout</html>`,
			want:   nil, // asserted separately
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyError(tc.status, []byte(tc.body))

			switch {
			case tc.want != nil:
				if !errors.Is(got, tc.want) {
					t.Fatalf("classifyError(%d, %s) = %v, want %v", tc.status, tc.body, got, tc.want)
				}
			case tc.status == 200:
				if got != nil {
					t.Fatalf("classifyError(200, ...) = %v, want nil", got)
				}
			default:
				if got == nil {
					t.Fatalf("classifyError(%d, %s) = nil, want a non-nil error", tc.status, tc.body)
				}
				for _, sentinel := range []error{ErrNotFound, ErrResourceChanged, ErrUnauthorized} {
					if errors.Is(got, sentinel) {
						t.Fatalf("classifyError(%d, %s) = %v, must NOT match sentinel %v", tc.status, tc.body, got, sentinel)
					}
				}
			}
		})
	}
}

// TestClassifyError_NotFoundIsNotStatus404 is the anti-regression for the
// exact bug this task fixes: a real dstack NEVER answers 404 for a missing
// run, so a 404-keyed mapping must not be what produces ErrNotFound.
func TestClassifyError_NotFoundIsNotStatus404(t *testing.T) {
	if got := classifyError(404, []byte(`{"detail":"Service main/x not found"}`)); errors.Is(got, ErrNotFound) {
		t.Fatal("a bare 404 produced ErrNotFound: the run API answers 400 + code, and 404 belongs to the service proxy (F23), not to run lookup")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `./scripts/dev.sh go test ./internal/dstack/ -run TestClassifyError -count=1`
Expected: FAIL — `undefined: classifyError`, `undefined: ErrUnauthorized`.

- [ ] **Step 3: Write the implementation**

Create `internal/dstack/errors.go`:

```go
// SPDX-License-Identifier: MIT

package dstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrUnauthorized is returned when the dstack server rejects our token.
// F23 keeps this distinct from every other failure: an auth fault is never
// a reason to wake anything, and never a transient to retry.
var ErrUnauthorized = errors.New("dstack: unauthorized")

// errorEnvelope is dstack's error body. `detail` is polymorphic: a LIST of
// {msg, code} for API errors, a bare OBJECT for auth errors, and a plain
// string from the service proxy. Measured on 0.21.2 — see
// docs/references/dstack-real-api.md §8.1.
type errorEnvelope struct {
	Detail json.RawMessage `json:"detail"`
}

type errorDetail struct {
	Msg  string `json:"msg"`
	Code string `json:"code"`
}

// resourceNotExistsCode is dstack's own code for a missing resource.
const resourceNotExistsCode = "resource_not_exists"

// resourceChangedMarker identifies the CAS conflict. dstack tags it with
// the generic code "error", so the message is the only discriminator it
// gives us. Matching a substring of an upstream string is fragile by
// nature; it is pinned by the Tier-1 e2e (task 6), which fails loudly if
// upstream ever rewords it.
const resourceChangedMarker = "resource has been changed"

// classifyError maps one dstack response to this package's sentinels.
// Status codes alone are NOT sufficient: dstack answers HTTP 400 for both
// "not found" and "CAS conflict".
func classifyError(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}

	details := parseDetails(body)
	for _, d := range details {
		if d.Code == resourceNotExistsCode {
			return fmt.Errorf("%w: %s", ErrNotFound, d.Msg)
		}
		if strings.Contains(strings.ToLower(d.Msg), resourceChangedMarker) {
			return fmt.Errorf("%w: %s", ErrResourceChanged, d.Msg)
		}
	}

	if status == 401 || status == 403 {
		return fmt.Errorf("%w: %s", ErrUnauthorized, summarise(details, body))
	}
	return fmt.Errorf("dstack: http %d: %s", status, summarise(details, body))
}

// parseDetails copes with all three `detail` shapes, returning an empty
// slice rather than an error when the body is not dstack's envelope at all
// (an intermediary's HTML error page, say).
func parseDetails(body []byte) []errorDetail {
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || len(env.Detail) == 0 {
		return nil
	}

	var list []errorDetail
	if err := json.Unmarshal(env.Detail, &list); err == nil {
		return list
	}
	var one errorDetail
	if err := json.Unmarshal(env.Detail, &one); err == nil {
		return []errorDetail{one}
	}
	var msg string
	if err := json.Unmarshal(env.Detail, &msg); err == nil {
		return []errorDetail{{Msg: msg}}
	}
	return nil
}

func summarise(details []errorDetail, body []byte) string {
	if len(details) > 0 && details[0].Msg != "" {
		return details[0].Msg
	}
	const max = 256
	if len(body) > max {
		return string(body[:max])
	}
	return string(body)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `./scripts/dev.sh go test ./internal/dstack/ -run TestClassifyError -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dstack/errors.go internal/dstack/errors_test.go
git commit -m "feat(dstack): classify errors from the body, not the status line"
```

---

### Task 2: Derive readiness, because dstack exposes no boolean

**Files:**
- Create: `internal/dstack/probes.go`
- Create: `internal/dstack/probes_test.go`

**Interfaces:**
- Produces: `type probeWire struct{ SuccessStreak int }`, `type jobSubmissionWire struct{...}`, `type jobWire struct{...}`, and `probesReady(jobs []jobWire, deploymentNum, readyAfter int) bool`.

**REVISED after measurement (D46).** The first draft signature had no `deploymentNum` and would
have been wrong. Measured after one in-place flip:

```
run deployment_num=1  jobs=2
  job[0] sub[0] status=terminated  dep=0  probes=[]                      <- stays forever
  job[1] sub[0] status=running     dep=1  probes=[{success_streak: 20}]
```

`jobs` **accumulates across deployments**. "Every job's latest submission must be probe-ready"
returns false forever, because `job[0]` has no probes and never will — a healthy, billing model
that never reaches `Ready`, which is D28 verbatim. The derivation must consider only submissions
whose `deployment_num` equals the RUN's, and whose status is not finished.
- Consumed by: task 4's `HTTPClient.Get`.

- [ ] **Step 1: Write the failing test**

Create `internal/dstack/probes_test.go`:

```go
// SPDX-License-Identifier: MIT

package dstack

import "testing"

func sub(status string, dep int, streaks ...int) jobSubmissionWire {
	probes := make([]probeWire, 0, len(streaks))
	for _, s := range streaks {
		probes = append(probes, probeWire{SuccessStreak: s})
	}
	return jobSubmissionWire{Status: status, DeploymentNum: dep, Probes: probes}
}

// TestProbesReady is §6 evidence (a), derived. dstack's Probe model is
// literally `{success_streak: int}` (measured, 0.21.2) — there is no ready
// flag on the wire, so the client computes readiness from the streak and
// the ready_after WE submitted.
//
// The governing invariant decides every ambiguous row: absence is never
// readiness. A model wrongly held un-Ready costs a held request; a model
// wrongly called Ready sends a user's tokens into a cold engine and gets
// the model evicted from LiteLLM's fallback chain.
func TestProbesReady(t *testing.T) {
	tests := []struct {
		name       string
		jobs       []jobWire
		dep        int
		readyAfter int
		want       bool
	}{
		{"no jobs at all is not ready", nil, 0, 2, false},
		{"a live submission with no probes is NOT ready",
			[]jobWire{{JobSubmissions: []jobSubmissionWire{sub("running", 0)}}}, 0, 2, false},
		{"streak below ready_after is not ready",
			[]jobWire{{JobSubmissions: []jobSubmissionWire{sub("running", 0, 1)}}}, 0, 2, false},
		{"streak equal to ready_after is ready",
			[]jobWire{{JobSubmissions: []jobSubmissionWire{sub("running", 0, 2)}}}, 0, 2, true},
		{"every probe must pass, not just one",
			[]jobWire{{JobSubmissions: []jobSubmissionWire{sub("running", 0, 5, 1)}}}, 0, 2, false},
		{"every live replica must pass, not just one",
			[]jobWire{
				{JobSubmissions: []jobSubmissionWire{sub("running", 0, 5)}},
				{JobSubmissions: []jobSubmissionWire{sub("running", 0, 0)}},
			}, 0, 2, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := probesReady(tc.jobs, tc.dep, tc.readyAfter); got != tc.want {
				t.Fatalf("probesReady(dep=%d, readyAfter=%d) = %v, want %v", tc.dep, tc.readyAfter, got, tc.want)
			}
		})
	}
}

// TestProbesReady_IgnoresPreviousDeployments is D46, measured on a real
// server: after an in-place flip (F17) the PREVIOUS deployment's replica
// stays in `jobs` forever — terminated, with `probes: []`. Counting it
// makes readiness unreachable, which is D28 all over again.
func TestProbesReady_IgnoresPreviousDeployments(t *testing.T) {
	jobs := []jobWire{
		{JobSubmissions: []jobSubmissionWire{sub("terminated", 0)}},        // the old replica
		{JobSubmissions: []jobSubmissionWire{sub("running", 1, 20)}},       // the live one
	}

	if !probesReady(jobs, 1, 2) {
		t.Fatal("probesReady = false: a terminated replica from deployment 0 was counted against deployment 1 (D46)")
	}
}

// TestProbesReady_IgnoresFinishedSubmissionsOfTheCurrentDeployment: a
// replica of the CURRENT deployment that has died is not evidence either
// way, but a run with no live submission at all is never ready.
func TestProbesReady_IgnoresFinishedSubmissionsOfTheCurrentDeployment(t *testing.T) {
	jobs := []jobWire{{JobSubmissions: []jobSubmissionWire{sub("failed", 1, 9)}}}

	if probesReady(jobs, 1, 2) {
		t.Fatal("probesReady = true from a FAILED submission's stale streak; only live submissions count")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `./scripts/dev.sh go test ./internal/dstack/ -run TestProbesReady -count=1`
Expected: FAIL — `undefined: probesReady`, `undefined: jobWire`.

- [ ] **Step 3: Write the implementation**

Create `internal/dstack/probes.go`:

```go
// SPDX-License-Identifier: MIT

package dstack

import "time"

// probeWire is dstack's entire per-probe state. Measured on 0.21.2: the
// model is `{success_streak: int}` and nothing else — no last-check time,
// no failure reason.
type probeWire struct {
	SuccessStreak int `json:"success_streak"`
}

// jobSubmissionWire is one attempt at running one replica.
type jobSubmissionWire struct {
	SubmittedAt   time.Time   `json:"submitted_at"`
	DeploymentNum int         `json:"deployment_num"`
	Status        string      `json:"status"`
	Probes        []probeWire `json:"probes"`
}

// jobWire is one replica of the service. dstack appends a new submission
// per attempt, and — measured, D46 — KEEPS the jobs of previous
// deployments in the list after an in-place flip.
type jobWire struct {
	JobSubmissions []jobSubmissionWire `json:"job_submissions"`
}

// finishedJobStatuses mirrors dstack's own terminal set.
var finishedJobStatuses = map[string]bool{
	"terminated": true,
	"failed":     true,
	"done":       true,
	"aborted":    true,
}

// probesReady is §6 evidence (a), derived: a run is probe-ready when it has
// at least one LIVE submission of the CURRENT deployment, and every such
// submission has at least one probe with every probe's success streak at
// or above readyAfter.
//
// The deploymentNum filter is not a refinement, it is load-bearing (D46):
// an in-place flip leaves the previous deployment's replica in `jobs`,
// terminated, with `probes: []`, permanently. Counting it makes readiness
// unreachable for the life of the run.
//
// Absence is never readiness. No live submission, no probes, or a short
// streak all mean "not ready" — "dstack job running" is never Ready (§6),
// and a missing probe is exactly how that would smuggle itself back in.
func probesReady(jobs []jobWire, deploymentNum, readyAfter int) bool {
	if readyAfter < 1 {
		return false
	}
	live := 0
	for _, j := range jobs {
		for _, s := range j.JobSubmissions {
			if s.DeploymentNum != deploymentNum || finishedJobStatuses[s.Status] {
				continue
			}
			live++
			if len(s.Probes) == 0 {
				return false
			}
			for _, p := range s.Probes {
				if p.SuccessStreak < readyAfter {
					return false
				}
			}
		}
	}
	return live > 0
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `./scripts/dev.sh go test ./internal/dstack/ -run TestProbesReady -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dstack/probes.go internal/dstack/probes_test.go
git commit -m "feat(dstack): derive probe readiness from success_streak"
```

---

### Task 3: The real wire types

**Files:**
- Create: `internal/dstack/wire.go`
- Modify: `internal/dstack/client.go` (delete `applyWire`/`runWire`; extend `Run`; change `ApplyRequest`)

**Interfaces:**
- Produces:
  - `runSpecWire`, `configurationWire`, `replicasWire`, `probeConfigWire`, `getPlanRequest`, `runPlanWire`, `applyRequest`, `applyPlanInput`, `runWire`, `getRunRequest`, `deleteRunsRequest`.
  - `Run` gains `ServiceURL string` and an **unexported** `raw json.RawMessage`.
  - `ApplyRequest` loses `BaseDeploymentNum`, gains `Current *Run`.
- Consumed by: task 4.

- [ ] **Step 1: Change `Run` and `ApplyRequest`**

In `internal/dstack/client.go`, replace `ApplyRequest` and `Run` with:

```go
// ApplyRequest is the input to Apply. There is deliberately no Force field.
// Real dstack makes `force` a REQUIRED field on the apply body, so the
// literal false is encoded at the single encode site in wire.go and no
// caller has anything to set (§5.2, AC13).
type ApplyRequest struct {
	Name     string
	Replicas int

	// Image is spec.image, the pinned digest (§8, §9). Port is where the
	// engine listens, derived from spec.engine by enginePort — the CRD does
	// NOT carry a port field and this plan does not add one.
	Image string
	Port  int

	// Current is the CAS anchor (F18). dstack's apply compares the ENTIRE
	// previous Run object, not a deployment number: "Errors if the expected
	// current resource from the plan does not match the current resource."
	// Pass the Run a previous Get returned; nil means "expect no run to
	// exist", which is what creates one. The losing side of a race gets
	// ErrResourceChanged and must re-read and re-plan, never force.
	Current *Run
}

// Run is the client's view of dstack run state.
type Run struct {
	Name          string
	RunID         string
	DeploymentNum int
	Replicas      int

	// ServiceURL is dstack's own forward target for this service —
	// measured shape: "/proxy/services/{project}/{run}/", a path relative
	// to the dstack server base URL when no gateway is provisioned. This is
	// what squall-proxy's Backend resolves to (closes ledger D25).
	ServiceURL string

	// ProbesReady is §6 evidence (a), DERIVED: dstack exposes only
	// `success_streak` per probe, so readiness is
	// `success_streak >= ready_after` across every probe of every replica's
	// latest job submission. Squall probes nothing, ever. "Replicas > 0" is
	// NEVER Ready by itself (§6).
	ProbesReady bool

	// Status is dstack's own run status. MEASURED, and load-bearing for
	// F20: a slept run settles at "pending" with zero live replicas, while
	// a dead one is "terminated"/"failed". Both look identical through the
	// service proxy, which answers 404 for either — so this is the only
	// thing that tells asleep from dead.
	Status string

	// SubmittedAt is when dstack accepted the run. Informational: the
	// provisioningTimeout anchor is status.wakeStartedAt, journaled by the
	// controller at actuation (§5.2), because an in-place flip (F17) reuses
	// the run and SubmittedAt dates from its first creation.
	SubmittedAt time.Time

	// raw is the verbatim JSON dstack returned for this run. It exists ONLY
	// to be handed back as apply's `current_resource`: the CAS compares the
	// whole object, so any field we drop on decode would corrupt the
	// comparison. Never read it for state — that is what the typed fields
	// above are for.
	raw json.RawMessage
}
```

- [ ] **Step 2: Write the wire types**

Create `internal/dstack/wire.go`:

```go
// SPDX-License-Identifier: MIT

// Wire types for dstack's real API, measured against 0.21.2. Keep this file
// a mirror of upstream and nothing else: no behaviour, no defaults beyond
// what the server itself applies. When dstack changes, this is the file to
// diff. See docs/references/dstack-real-api.md.
package dstack

import (
	"encoding/json"
	"time"
)

// defaultReadyAfter mirrors dstack's own ProbeConfig default and is the
// value squall submits, so probesReady can compare against it without
// re-reading the plan.
const defaultReadyAfter = 2

// probeIntervalSeconds is how often dstack runs the probe. It bounds how
// stale evidence (a) can be, so it belongs next to the deadline reasoning
// in §5.1, not in a caller.
const probeIntervalSeconds = 5

type replicasWire struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type probeConfigWire struct {
	Type       string `json:"type"`
	URL        string `json:"url"`
	Interval   int    `json:"interval"`
	ReadyAfter int    `json:"ready_after"`
}

// configurationWire is a dstack service configuration. Squall submits a
// FIXED replica count (min == max), never a range: a range would require a
// `scaling` block and hand the replica count to dstack's RPS autoscaler.
// With `scaling` absent dstack selects ManualScaler, which ignores request
// stats entirely — that is what makes §10's two-lane rule hold by
// construction rather than by policy.
type configurationWire struct {
	Type     string            `json:"type"`
	Name     string            `json:"name"`
	Image    string            `json:"image"`
	Port     int               `json:"port"`
	Replicas int               `json:"replicas"`
	Probes   []probeConfigWire `json:"probes,omitempty"`
}

type runSpecWire struct {
	RunName       string            `json:"run_name"`
	Configuration configurationWire `json:"configuration"`
}

type getPlanRequest struct {
	RunSpec runSpecWire `json:"run_spec"`
}

// runPlanWire is get_plan's response. Only the fields apply needs are
// decoded; the run_spec is kept RAW because apply must echo back exactly
// what the server normalised (replicas 1 -> {min:1,max:1}, interval "5s" ->
// 5), not our re-serialisation of it.
type runPlanWire struct {
	RunSpec         json.RawMessage `json:"run_spec"`
	CurrentResource json.RawMessage `json:"current_resource"`
	Action          string          `json:"action"`
}

// applyPlanInput carries the plan back. current_resource is raw for the
// same reason: the CAS compares the whole object.
type applyPlanInput struct {
	RunSpec         json.RawMessage `json:"run_spec"`
	CurrentResource json.RawMessage `json:"current_resource,omitempty"`
}

// applyRequest is the ONLY place `force` is encoded, and it is encoded as
// the literal false. dstack requires the field; squall requires it to be
// false. There is no Go-level way for a caller to reach it (AC13).
type applyRequest struct {
	Plan  applyPlanInput `json:"plan"`
	Force bool           `json:"force"`
}

func newApplyRequest(plan applyPlanInput) applyRequest {
	return applyRequest{Plan: plan, Force: false}
}

type getRunRequest struct {
	RunName string `json:"run_name"`
}

type deleteRunsRequest struct {
	RunsNames []string `json:"runs_names"`
}

type serviceWire struct {
	URL string `json:"url"`
}

type runWire struct {
	ID            string       `json:"id"`
	SubmittedAt   time.Time    `json:"submitted_at"`
	Status        string       `json:"status"`
	DeploymentNum int          `json:"deployment_num"`
	Jobs          []jobWire    `json:"jobs"`
	Service       *serviceWire `json:"service"`
	RunSpec       struct {
		RunName       string `json:"run_name"`
		Configuration struct {
			Replicas replicasWire `json:"replicas"`
		} `json:"configuration"`
	} `json:"run_spec"`
}
```

- [ ] **Step 3: Verify it compiles (the client will not yet)**

Run: `./scripts/dev.sh go build ./internal/dstack/ 2>&1 | head`
Expected: errors only in `client.go` about the now-deleted `applyWire`/`BaseDeploymentNum` — task 4 fixes those. Do not chase them here.

- [ ] **Step 4: Commit**

```bash
git add internal/dstack/wire.go internal/dstack/client.go
git commit -m "feat(dstack): model the real wire types"
```

---

### Task 4: `HTTPClient` against the real endpoints

**Files:**
- Create: `internal/dstack/http.go`
- Modify: `internal/dstack/client.go` (delete `HTTPClient`'s old method bodies and `decodeOrMapError`)
- Modify: `internal/dstack/client_test.go` (its httptest servers now speak the real paths)

**Interfaces:**
- Consumes: task 1's `classifyError`, task 2's `probesReady`, task 3's wire types.
- Produces: `NewHTTPClient(baseURL, project, token string, httpClient *http.Client) *HTTPClient` satisfying `Client`.

Note the **one new constructor parameter**: `project`. dstack scopes every path by it and there is no server-wide run namespace. Wire it from `cmd/controller` via `SQUALL_DSTACK_PROJECT`, default `main`.

Image and port travel on `ApplyRequest`, not on the client, because they are **per-Model**: one client serves every Model in the cluster. `spec.image` supplies the image. There is no port field on the CRD, so the port is derived from `spec.engine` by a table in the controller:

```go
// enginePort is where each engine template listens. These are the engines'
// own defaults, not a squall choice, and they belong next to the engine
// enum rather than in the dstack client — the client should not know what
// vLLM is.
func enginePort(e squallv1alpha1.ModelEngine) int {
	switch e {
	case squallv1alpha1.ModelEngineVLLM:
		return 8000
	case squallv1alpha1.ModelEngineLlamaCpp:
		return 8080
	case squallv1alpha1.ModelEngineOllama:
		return 11434
	default:
		return 8000
	}
}
```

Put it in `internal/controller/squall/`, next to the Apply call site, and table-test it against the enum so a new engine cannot be added without a port.

- [ ] **Step 1: Write the failing test**

Append to `internal/dstack/client_test.go`:

```go
// TestHTTPClient_Apply_IsTwoStepAndNeverForces walks the MEASURED apply
// contract: get_plan first, then apply echoing the server's own normalised
// run_spec back, with force always the literal false.
func TestHTTPClient_Apply_IsTwoStepAndNeverForces(t *testing.T) {
	var sawPlan, sawApply bool
	var applyBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/project/main/runs/get_plan":
			sawPlan = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"run_spec":{"run_name":"qwen","configuration":{"replicas":{"min":1,"max":1}}},"current_resource":null,"action":"create"}`))
		case "/api/project/main/runs/apply":
			sawApply = true
			if err := json.NewDecoder(r.Body).Decode(&applyBody); err != nil {
				t.Errorf("decode apply body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"11111111-1111-1111-1111-111111111111","submitted_at":"2026-08-27T09:00:00Z","status":"submitted","deployment_num":0,"jobs":[],"service":{"url":"/proxy/services/main/qwen/"},"run_spec":{"run_name":"qwen","configuration":{"replicas":{"min":1,"max":1}}}}`))
		default:
			t.Errorf("unexpected path %q — every run operation is POST under /api/project/{project}/runs/", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	run, err := c.Apply(context.Background(), ApplyRequest{Name: "qwen", Replicas: 1, Image: "ollama/qwen", Port: 11434})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !sawPlan || !sawApply {
		t.Fatalf("get_plan seen = %v, apply seen = %v; apply is a TWO-step flow", sawPlan, sawApply)
	}
	if force, ok := applyBody["force"]; !ok {
		t.Fatal("apply body has no `force` field: dstack requires it")
	} else if force != false {
		t.Fatalf("force = %v, want false — squall NEVER forces (AC13)", force)
	}
	if run.RunID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("RunID = %q, want dstack's own run id", run.RunID)
	}
	if run.ServiceURL != "/proxy/services/main/qwen/" {
		t.Errorf("ServiceURL = %q, want the service proxy path", run.ServiceURL)
	}
	if run.Replicas != 1 {
		t.Errorf("Replicas = %d, want 1 read from run_spec.configuration.replicas.min", run.Replicas)
	}
}

// TestHTTPClient_Apply_RoundTripsCurrentResourceVerbatim is F18's CAS:
// dstack compares the WHOLE previous Run, so anything the client drops on
// decode would corrupt the comparison and turn a legitimate flip into a
// permanent conflict.
func TestHTTPClient_Apply_RoundTripsCurrentResourceVerbatim(t *testing.T) {
	const stored = `{"id":"abc","some_field_squall_does_not_model":42,"jobs":[]}`
	var sent json.RawMessage

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/project/main/runs/get":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(stored))
		case "/api/project/main/runs/get_plan":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"run_spec":{},"current_resource":null,"action":"update"}`))
		case "/api/project/main/runs/apply":
			var body struct {
				Plan struct {
					CurrentResource json.RawMessage `json:"current_resource"`
				} `json:"plan"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			sent = body.Plan.CurrentResource
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"abc","jobs":[],"run_spec":{"configuration":{"replicas":{"min":0,"max":0}}}}`))
		}
	}))
	t.Cleanup(srv.Close)

	c := NewHTTPClient(srv.URL, "main", "tok", srv.Client())
	prev, err := c.Get(context.Background(), "qwen")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := c.Apply(context.Background(), ApplyRequest{Name: "qwen", Replicas: 0, Image: "img", Port: 8080, Current: prev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(sent, &got); err != nil {
		t.Fatalf("current_resource was not sent as an object: %v (raw %q)", err, sent)
	}
	if got["some_field_squall_does_not_model"] != float64(42) {
		t.Fatalf("current_resource = %s; a field squall does not model was DROPPED, which corrupts dstack's whole-object CAS", sent)
	}
}
```

Ensure imports include `"context"`, `"encoding/json"`, `"net/http"`, `"net/http/httptest"`, `"testing"`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `./scripts/dev.sh go test ./internal/dstack/ -run TestHTTPClient_Apply -count=1`
Expected: FAIL to compile — `NewHTTPClient` takes a different signature.

- [ ] **Step 3: Write the implementation**

Create `internal/dstack/http.go` with the four operations. Every one is a `POST`; every one routes its response through `classifyError` before decoding:

```go
// SPDX-License-Identifier: MIT

package dstack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// post issues one POST and classifies the outcome. It is the ONLY place
// this package talks HTTP, so the error contract cannot drift between
// operations.
func (c *HTTPClient) post(ctx context.Context, path string, in any) ([]byte, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("dstack: encode %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("dstack: build %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dstack: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("dstack: read %s: %w", path, err)
	}
	if err := classifyError(resp.StatusCode, body); err != nil {
		return nil, err
	}
	return body, nil
}

func (c *HTTPClient) runsPath(op string) string {
	return "/api/project/" + c.project + "/runs/" + op
}

// Apply is dstack's two-step apply: submit the desired spec to get_plan,
// then send the plan the server normalised back to apply. Squall never
// forces — newApplyRequest encodes the literal false (AC13).
func (c *HTTPClient) Apply(ctx context.Context, req ApplyRequest) (*Run, error) {
	plan, err := c.post(ctx, c.runsPath("get_plan"), getPlanRequest{
		RunSpec: runSpec(req),
	})
	if err != nil {
		return nil, err
	}
	var planned runPlanWire
	if err := json.Unmarshal(plan, &planned); err != nil {
		return nil, fmt.Errorf("dstack: decode run plan: %w", err)
	}

	// The CAS anchor is what WE last observed, not what get_plan just read:
	// get_plan re-reads current state, so echoing its current_resource back
	// would compare the server against itself and defeat F18 entirely.
	var current json.RawMessage
	if req.Current != nil {
		current = req.Current.raw
	}

	body, err := c.post(ctx, c.runsPath("apply"), newApplyRequest(applyPlanInput{
		RunSpec:         planned.RunSpec,
		CurrentResource: current,
	}))
	if err != nil {
		return nil, err
	}
	return decodeRun(body)
}

// Get returns current run state, or ErrNotFound if dstack no longer knows
// the run (F20: dead is not asleep).
func (c *HTTPClient) Get(ctx context.Context, name string) (*Run, error) {
	body, err := c.post(ctx, c.runsPath("get"), getRunRequest{RunName: name})
	if err != nil {
		return nil, err
	}
	return decodeRun(body)
}

// Delete removes the run. Fleet instance release is dstack's own job via
// fleet.idleDuration (F21); Delete does not and must not model that.
func (c *HTTPClient) Delete(ctx context.Context, name string) error {
	_, err := c.post(ctx, c.runsPath("delete"), deleteRunsRequest{RunsNames: []string{name}})
	return err
}

// ListRuns backs the reconcile loop's orphan diff (§5.2).
func (c *HTTPClient) ListRuns(ctx context.Context) ([]Run, error) {
	// MEASURED: /api/project/{p}/runs/list answers 405. `list` lives on the
	// ROOT router (`/api/runs/list`), unlike every other run operation.
	body, err := c.post(ctx, "/api/runs/list", struct{}{})
	if err != nil {
		return nil, err
	}
	var wires []json.RawMessage
	if err := json.Unmarshal(body, &wires); err != nil {
		return nil, fmt.Errorf("dstack: decode run list: %w", err)
	}
	runs := make([]Run, 0, len(wires))
	for _, w := range wires {
		run, err := decodeRun(w)
		if err != nil {
			return nil, err
		}
		runs = append(runs, *run)
	}
	return runs, nil
}

func runSpec(req ApplyRequest) runSpecWire {
	return runSpecWire{
		RunName: req.Name,
		Configuration: configurationWire{
			Type:     "service",
			Name:     req.Name,
			Image:    req.Image,
			Port:     req.Port,
			Replicas: req.Replicas,
			Probes: []probeConfigWire{{
				Type:       "http",
				URL:        "/health",
				Interval:   probeIntervalSeconds,
				ReadyAfter: defaultReadyAfter,
			}},
		},
	}
}

// decodeRun keeps the verbatim body on the Run so a later Apply can hand it
// back as dstack's whole-object CAS anchor.
func decodeRun(body []byte) (*Run, error) {
	var w runWire
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("dstack: decode run: %w", err)
	}
	return &Run{
		Name:          w.RunSpec.RunName,
		RunID:         w.ID,
		DeploymentNum: w.DeploymentNum,
		Replicas:      w.RunSpec.Configuration.Replicas.Min,
		ServiceURL:    serviceURL(w.Service),
		ProbesReady:   probesReady(w.Jobs, w.DeploymentNum, defaultReadyAfter),
		SubmittedAt:   w.SubmittedAt,
		raw:           append(json.RawMessage(nil), body...),
	}, nil
}

func serviceURL(s *serviceWire) string {
	if s == nil {
		return ""
	}
	return s.URL
}
```

Update `NewHTTPClient` and the `HTTPClient` struct in `client.go` to carry `project`, and delete `decodeOrMapError` and the old method bodies.

- [ ] **Step 4: Run the package tests**

Run: `./scripts/dev.sh go test ./internal/dstack/ -count=1`
Expected: PASS. Pre-existing tests that asserted 404/409 mapping must be **rewritten to the measured contract**, not deleted — if a test cannot be made to assert something true, say so in the ledger rather than dropping it.

- [ ] **Step 5: Commit**

```bash
git add internal/dstack/http.go internal/dstack/client.go internal/dstack/client_test.go
git commit -m "feat(dstack): speak dstack's real run API"
```

---

### Task 4b: Stop committing a 404 during a wake (D44)

**Files:**
- Modify: `internal/proxy/attempt.go`
- Modify: `internal/proxy/handler.go`
- Test: `internal/proxy/attempt_test.go`

**Interfaces:**
- Consumes: `ModelSnapshot.Phase` from the informer cache.
- Produces: `classifyAttempt(resp *http.Response, err error, phase squallv1alpha1.ModelPhase) attemptResult`.

**Why this exists.** MEASURED (`dstack-real-api.md` §9.5): from `replicas: 0` back to `1`, the
dstack service proxy answers **404** through `pending`, `submitted`, `provisioning` and even
`running` with `success_streak: 1`, flipping to 200 only when the streak reaches `ready_after`.
`classifyAttempt` today retries on 502/503/transport error and treats 404 as a commit, so against
a real server the FIRST attempt of a held request commits a 404 to the client and §7's
held-request-as-oracle never holds anything. It passes today only because our fake answers 503 —
a 503 we invented.

**The fix is not "retry on 404 too."** F23 reads gateway 404 as dead/deregistered, and the
measurement shows 404 is *also* the waking state. The HTTP code cannot disambiguate. The CR's
phase can: while the controller says the model is waking, a 404 is the wake; once it says
`Dead`/`Recreating`, a 404 is the truth.

- [ ] **Step 1: Write the failing test**

```go
// TestClassifyAttempt_404DependsOnPhase is D44, measured against dstack
// 0.21.2: the service proxy answers 404 for the ENTIRE wake window, not
// 503. A 404 must therefore keep the hold while the CR says the model is
// coming up, and commit only once the CR says it is not.
func TestClassifyAttempt_404DependsOnPhase(t *testing.T) {
	tests := []struct {
		name  string
		phase squallv1alpha1.ModelPhase
		want  attemptResult
	}{
		{"waking: 404 is the wake window, keep holding", squallv1alpha1.ModelPhaseWaking, attemptRetry},
		{"asleep: the wake was just requested, keep holding", squallv1alpha1.ModelPhaseAsleep, attemptRetry},
		{"ready: a 404 now is real, commit it", squallv1alpha1.ModelPhaseReady, attemptCommit},
		{"dead: 404 is the truth, commit it", squallv1alpha1.ModelPhaseDead, attemptCommit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAttempt(&http.Response{StatusCode: http.StatusNotFound}, nil, tc.phase)
			if got != tc.want {
				t.Fatalf("classifyAttempt(404, phase=%v) = %v, want %v", tc.phase, got, tc.want)
			}
		})
	}
}

// TestClassifyAttempt_PhaseDoesNotMakeOtherCodesAmbiguous: only 404 is
// phase-dependent. A 200 commits and a 503 retries whatever the CR says —
// otherwise a stale informer cache could swallow a real answer.
func TestClassifyAttempt_PhaseDoesNotMakeOtherCodesAmbiguous(t *testing.T) {
	for _, phase := range []squallv1alpha1.ModelPhase{
		squallv1alpha1.ModelPhaseAsleep, squallv1alpha1.ModelPhaseWaking,
		squallv1alpha1.ModelPhaseReady, squallv1alpha1.ModelPhaseDead,
	} {
		if got := classifyAttempt(&http.Response{StatusCode: 200}, nil, phase); got != attemptCommit {
			t.Fatalf("200 with phase %v = %v, want commit", phase, got)
		}
		if got := classifyAttempt(&http.Response{StatusCode: 503}, nil, phase); got != attemptRetry {
			t.Fatalf("503 with phase %v = %v, want retry", phase, got)
		}
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `./scripts/dev.sh go test ./internal/proxy/ -run TestClassifyAttempt -count=1`
Expected: FAIL — `classifyAttempt` takes two arguments today, and 404 commits unconditionally.

- [ ] **Step 3: Implement**

```go
// classifyAttempt implements §7's retry rule. A transport error (connection
// refused/reset, dial timeout) is indistinguishable from "the engine has
// not bound its port yet", which is exactly the waking state.
//
// 404 is phase-dependent, and that is not a nicety (D44). MEASURED against
// dstack 0.21.2: the service proxy answers 404 for the whole wake —
// pending, submitted, provisioning, and even running until the probe's
// success streak reaches ready_after — and 404 is ALSO what a dead or
// deregistered service answers (F23). The status line cannot tell them
// apart, so the CR's phase does: while the controller still says the model
// is coming up, keep holding; once it says otherwise, the 404 is the truth
// and the client deserves it.
func classifyAttempt(resp *http.Response, err error, phase squallv1alpha1.ModelPhase) attemptResult {
	if err != nil {
		return attemptRetry
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return attemptRetry
	case http.StatusNotFound:
		if phase == squallv1alpha1.ModelPhaseAsleep || phase == squallv1alpha1.ModelPhaseWaking {
			return attemptRetry
		}
		return attemptCommit
	default:
		return attemptCommit
	}
}
```

Thread the phase through `attemptForward` from the snapshot the handler already holds. Re-read it
from the cache on each attempt rather than capturing it once — the hold exists precisely because
the phase is expected to change underneath.

- [ ] **Step 4: Run the package**

Run: `./scripts/dev.sh go test ./internal/proxy/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/attempt.go internal/proxy/handler.go internal/proxy/attempt_test.go
git commit -m "fix(proxy): a 404 during a wake is the wake, not an answer"
```

---

### Task 5: The fake speaks the real shape

**Files:**
- Modify: `internal/dstack/mock/http.go`
- Modify: `internal/dstack/mock/mock.go`
- Modify: `internal/dstack/mock/mock_test.go`

**Interfaces:**
- Consumes: nothing from tasks 1-4 directly (the fake is a server, not a client), but its **output** must decode cleanly through task 4's `decodeRun`.
- Produces: the same `Server` API (`New`, `SetClock`, `SetProbeDelay`, `Apply`, `Get`, `ListRuns`, `Tick`, `InstanceCount`) with a rewritten HTTP surface.

The state machine mostly survives — what changes is what it emits.

- [ ] **Step 1: Write the failing test**

Append to `internal/dstack/mock/mock_test.go`:

```go
// TestFakeSpeaksTheRealWire is the whole point of the fake: a client that
// works against it must work against dstack. It drives the REAL client
// against the fake's HTTP surface, so any divergence is a compile-or-fail
// here rather than a surprise on first contact with a real server.
func TestFakeSpeaksTheRealWire(t *testing.T) {
	srv := httptest.NewServer(NewHTTPServer(New(), "tok"))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())

	if _, err := c.Get(context.Background(), "qwen"); !errors.Is(err, dstack.ErrNotFound) {
		t.Fatalf("Get on an unknown run = %v, want ErrNotFound", err)
	}

	run, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 1, Image: "img", Port: 8080})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if run.RunID == "" || run.Replicas != 1 {
		t.Fatalf("Apply returned %+v, want a run id and Replicas 1", run)
	}

	// F18: re-applying against a stale anchor must lose loudly.
	stale := run
	if _, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 0, Image: "img", Port: 8080, Current: &stale}); err != nil {
		t.Fatalf("Apply with the fresh anchor: %v", err)
	}
	if _, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 1, Image: "img", Port: 8080, Current: &stale}); !errors.Is(err, dstack.ErrResourceChanged) {
		t.Fatalf("Apply with a STALE anchor = %v, want ErrResourceChanged", err)
	}
}
```

- [ ] **Step 2: Run it to watch it fail**

Run: `./scripts/dev.sh go test ./internal/dstack/mock/ -run TestFakeSpeaksTheRealWire -count=1`
Expected: FAIL — the fake serves `/apply`, not `/api/project/main/runs/apply`.

- [ ] **Step 3: Rewrite the fake's HTTP surface**

In `internal/dstack/mock/http.go`:

- Route on the real paths: `POST /api/project/{project}/runs/{get_plan,apply,get,delete,list}`. Accept any project name — the fake is not a multi-tenant server, and rejecting one would only invent a rule dstack does not have.
- `get_plan` echoes the submitted `run_spec` back under `{"run_spec": <as sent>, "current_resource": null, "action": "create"}`, normalising `replicas: N` to `{"min":N,"max":N}` exactly as measured.
- Emit the real error envelope with the real codes:

```go
// writeError emits dstack's MEASURED error shape. The status codes are
// upstream's, not ours: dstack answers 400 for BOTH "not found" and the CAS
// conflict, and distinguishes them only in the body. A fake that answered
// 404/409 here would let a status-code-keyed client pass its tests and then
// fail against a real server — which is exactly the bug this rewrite fixes.
func writeError(w http.ResponseWriter, status int, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"detail": []map[string]any{{"msg": msg, "code": code}},
	})
}
```

with `ErrNotFound` → `writeError(w, 400, "Run not found", "resource_not_exists")` and `ErrResourceChanged` → `writeError(w, 400, "Failed to apply plan. Resource has been changed. Try again or use force apply.", "error")`.

- Emit runs in the real shape: `id`, `submitted_at`, `status`, `deployment_num`, `jobs[].job_submissions[].probes[].success_streak`, `service.url`, `run_spec.configuration.replicas.{min,max}`.
- The existing probe-delay clock drives `success_streak`: **0 before the delay elapses, `defaultReadyAfter` after**. Keep the delay — a fake that flips a boolean instantly is the lie that produced D28.

In `internal/dstack/mock/mock.go`, keep `Force` refused unconditionally: real dstack takes `force` as a required boolean, so the fake must reject `true` rather than ignore it.

- [ ] **Step 4: Run the mock and dstack suites**

Run: `./scripts/dev.sh go test ./internal/dstack/... -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dstack/mock/
git commit -m "feat(dstack-mock): serve dstack's real wire shape"
```

---

### Task 6: (REMOVED — owned by the Helm brief)

The cluster is no longer this plan's job. `docs/plans/2026-08-27-helm-chart-and-dstack-in-cluster.md`
installs a real dstack server via the Helm chart, with the measured Kubernetes-backend config,
the `nodes: 0..2` fleet, the kubeconfig Secret and the narrowed RBAC.

**What this plan depends on it delivering**, and must not start task 7 without:

- a dstack Service reachable in-cluster on port 3000, project `main`;
- a fleet in `active` state — without one every run dies `failed_to_start_due_to_no_capacity`;
- `model-mock` provisioned **by dstack** as the run image, not pre-deployed;
- the dstack Deployment covered by `hack/cluster.sh`'s build stamp;
- `make e2e-local` (or whatever the brief names it) bringing all of that up.

If the brief lands late, tasks 1-5 are still fully implementable and testable against
`internal/dstack/mock` — they are pure client work. Only task 7 blocks.

---

### Task 7: The Tier-1 specs

**Files:**
- Create: `test/e2e/dstack_test.go`

**Interfaces:**
- Consumes: the `dstack` Service from task 6, the real client from task 4.

These are the assertions that only a real server can make. Each one names the fact it proves.

- [ ] **Step 1: Write the specs**

Create `test/e2e/dstack_test.go` (behind `//go:build e2e`), skipping unless `SQUALL_E2E_REAL_DSTACK=1`.
Each spec asserts something that was MEASURED by hand — the numbers below are observations, not guesses:

```go
var _ = Describe("Real dstack (Tier-1)", Ordered, func() {
	It("decodes the wire shape squall's client expects (D1)", func() {
		// Apply through the real client, read back through the real client.
		// Assert RunID, DeploymentNum, Replicas, ServiceURL and Status are
		// all populated. ServiceURL must look like
		// "/proxy/services/main/<run>/" — measured.
	})

	It("flips in place: deployment_num increments, run id does not change (F17)", func() {
		// Apply 1 -> record id + deployment_num. Apply 0 with the fresh
		// anchor -> same id, deployment_num + 1. Measured: 0 -> 1 -> 2 across
		// two flips, run id stable.
	})

	It("keeps previous deployments in jobs, and stays Ready anyway (D46)", func() {
		// After a flip, assert len(jobs) > 1 AND ProbesReady is true. This is
		// the spec that fails if probesReady stops filtering by
		// deployment_num — the bug that would resurrect D28.
	})

	It("refuses a stale CAS anchor with ErrResourceChanged, and never forces (F18, AC13)", func() {
		// Apply with a deliberately stale Current. Assert errors.Is(err,
		// dstack.ErrResourceChanged) — NOT a generic error. Measured: the
		// server answers HTTP 400, so this is what catches a regression back
		// to 409-keyed mapping.
	})

	It("distinguishes asleep from dead by run status, not by the gateway code (F20, D44)", func() {
		// Flip to 0. Assert run Status == "pending" and the service proxy
		// answers 404. Then assert a DELETED run's proxy also answers 404 —
		// same code, different meaning. Measured both.
	})

	It("answers 404 for the whole wake and 200 only at ready_after (D44, F35)", func() {
		// Flip 0 -> 1 and poll the service proxy and the run together.
		// Assert: at least one poll saw 404 while the run was already
		// "running" with a success_streak BELOW ready_after, and the first
		// 200 coincided with the streak reaching ready_after. Measured:
		// 404 through pending/submitted/provisioning/running(streak=1),
		// 200 at streak=2, ~65s wall clock for nginx on CPU.
		//
		// The "saw 404 while running" assertion is what makes this
		// non-vacuous: without it the spec passes on any backend that just
		// becomes ready quickly.
	})

	It("serves a real request through squall-proxy while the model wakes (§7, D44)", func() {
		// The integration of everything: flip to 0, issue ONE chat
		// completion through squall-proxy, and assert it returns 200 with a
		// well-formed body — never a 404. Before D44's fix this spec fails
		// immediately with 404, which is the whole point of it existing.
	})
})
```

Fill each body against the helpers already in `test/e2e/`. **Every `Eventually` carries an explicit timeout and a failure message naming the fact.**

- [ ] **Step 2: Run them**

Run: `./scripts/dev.sh make e2e-local`
Expected: all specs pass. A red spec here is a **finding about reality**, not a test bug — log it in the ledger before changing any assertion.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/dstack_test.go
git commit -m "test(e2e): prove the dstack wire shape against a real server"
```

---

## End-of-block verification

Run **once**, at the boundary — not per task:

```bash
./scripts/dev.sh make test-unit
./scripts/dev.sh make test-envtest
./scripts/dev.sh make qa-lint
make e2e-full      # the fake path still works
make e2e-local     # the real path works
```

Then close out:

- [ ] Update `docs/references/dstack-real-api.md` §"What is still unverified" with what the Tier-1 run actually measured (probe streak timings, flip latency, whether `deployment_num` increments as predicted).
- [ ] Close **D1** and **D1b** in the ledger, or record precisely what remains open.
- [ ] Close **D25** if `ServiceURL` now feeds `squall-proxy`'s `Backend`; if that wiring is deferred, say so explicitly rather than leaving D25 ambiguous.
- [ ] Un-mark `internal/dstack` as FROZEN in `internal/dstack/CLAUDE.md` and replace the freeze note with the real rule: fields may be added when a spec section names the state **and** the field is confirmed against a real server.

## Mutation sweep — one pass, at the end

| # | Mutate | Expected red |
|---|---|---|
| M1 | `classifyError`: return `ErrNotFound` on status 404 instead of on the code | `TestClassifyError_NotFoundIsNotStatus404` |
| M2 | `classifyError`: drop the `resourceChangedMarker` branch | `TestClassifyError` CAS row; Tier-1 CAS spec |
| M3 | `probesReady`: return `true` when `len(latest.Probes) == 0` | "a job with no probes is NOT ready" |
| M4 | `probesReady`: drop the `deploymentNum` filter | `TestProbesReady_IgnoresPreviousDeployments`; Tier-1 D46 spec |
| M5 | `probesReady`: `>` instead of `>=` | "streak equal to ready_after is ready" |
| M6 | `probesReady`: return true if **any** live submission is ready | "every live replica must pass" |
| M6b | `probesReady`: count finished submissions too | "IgnoresFinishedSubmissionsOfTheCurrentDeployment" |
| M6c | `classifyAttempt`: commit on 404 regardless of phase | `TestClassifyAttempt_404DependsOnPhase`; Tier-1 wake spec |
| M6d | `classifyAttempt`: retry on 404 regardless of phase | the `Dead` row of the same test |
| M7 | `newApplyRequest`: encode `Force: true` | `TestHTTPClient_Apply_IsTwoStepAndNeverForces`; Tier-1 AC13 spec |
| M8 | `Apply`: send `planned.CurrentResource` instead of `req.Current.raw` | `TestHTTPClient_Apply_RoundTripsCurrentResourceVerbatim`; Tier-1 CAS spec |
| M9 | `decodeRun`: drop `raw` | M8's test |
| M10 | `decodeRun`: read `Replicas` from `Max` instead of `Min` | Tier-1 flip spec (0 vs 1 diverge) |
| M11 | fake: answer 409 for the CAS conflict | `TestFakeSpeaksTheRealWire` |
| M12 | fake: set `success_streak` to `defaultReadyAfter` immediately | the fake's probe-delay test |
| M13 | `ListRuns`: use the project router path for `list` | any Tier-1 spec that lists (measured: 405) |

A mutation that leaves the suite green is a **finding**, not a formality. Log it.

## Risks

1. **~~The Kubernetes backend may not provision inside kind~~ — RESOLVED by measurement.** It does.
   Real pods were created (`dstack-main-probe-0-0-xxxx` plus an auto-created
   `dstack-main-ssh-jump-pod`), a service served traffic, and a sleep flip removed the replica. No
   GPU operator needed for CPU. What it costs: a fleet with `nodes` target 0, and an explicit
   `proxy_jump.hostname` because kind nodes have no `ExternalIP`.
2. **The CAS marker is a substring match on an upstream English string.** It will break silently if
   upstream rewords it. The Tier-1 CAS spec is the only thing that would notice. Pin the dstack
   image; do not track `:latest`.
3. **`defaultReadyAfter` must match what squall actually submits.** `probesReady` compares against
   a constant, not against the plan's echo. If a Model ever carries its own probe config, that
   constant becomes a lie — read it back from the run spec at that point.
4. **`enginePort` is a table of upstream defaults that nothing verifies.** A wrong port makes every
   probe fail forever and the model never reach Ready — fail-safe in direction, opaque in
   diagnosis. Tier-1's probe spec catches it for whichever engine that spec runs.
5. **This block cannot be pipelined with anything touching `internal/dstack`.** Tasks 1-5 rewrite
   one package; give the whole package to one implementer. It CAN be pipelined with the Helm brief,
   whose file tree is disjoint.
6. **D44's fix threads CR phase into the data path.** A stale informer cache now influences retry
   behaviour, not just the decision table. The phase is re-read per attempt for that reason; a
   captured-once phase would hold a dead model to the deadline.

## Self-review notes

- **Spec coverage:** F17 (task 7), F18/AC13 (tasks 3, 4, 5, 7), F20 (tasks 1, 7), F23 (task 7), F35/§6 (tasks 2, 7), §5.2 CAS (task 4), §10 two-lane (task 3's fixed-replicas comment), D1/D1b/D25 (end-of-block).
- **Not covered, deliberately:** `provisioningTimeout` (task 8.3) and D39 stay after this block. This plan does not touch them; the interlock D28 → D39 → 8.3 is unchanged, and D28 is now closed.
- **Type consistency:** `probesReady(jobs []jobWire, readyAfter int) bool` is defined in task 2 and used in task 4's `decodeRun`; `jobWire`/`jobSubmissionWire`/`probeWire` are defined once, in task 2, and referenced by task 3's `runWire`; `newApplyRequest` is defined in task 3 and used in task 4.
- **Verified while writing this plan:** `ModelSpec` carries `Image` but **no port** (`api/squall/v1alpha1/model_types.go`), which is why the port is derived from `spec.engine` rather than read from the CR. No CRD change is needed and none is proposed — adding a port field would be inventing spec.

---

## Running concurrently with the other plan — read this before your first commit

Two agents once ran concurrently on this repo and one destroyed the other's work. These are not
suggestions.

- **Never `git add -A`.** Name every path you commit. The tree contains another agent's in-flight
  files.
- **Never `git reset`, `git commit --amend`, or rebase past a commit you did not create.** Only
  add commits.
- **Never `git checkout -- <file>` or `git restore` while the tree is dirty.** Copy to a temp path
  and edit forward.
- **Stay inside your named tree.** If you believe you must touch a file the other plan owns, stop
  and report it instead — that is a coordination decision, not an implementation one.

### The one shared file: the ledger

`docs/references/deviations-and-findings.md` is appended by both agents and **will** conflict.
Rules: append only, never renumber, never delete an entry. On a conflict, keep **both** sides —
the entries are independent observations, not competing versions. If two agents pick the same
`D<n>`, the second one to land renumbers **its own** entry upward and says so in the commit
message.

### Seams to leave alone until coordinated

- `SQUALL_BACKEND_URL_TEMPLATE` — the proxy's forward target. Measured, dstack exposes
  `service.url` = `/proxy/services/{project}/{run}/`, so this template is destined to be replaced
  (ledger D25). **Neither plan does that rewiring.** Whoever needs it first must raise it, not
  quietly change it, or the other agent's environment stops forwarding.
- `cmd/fake-dstack` and `internal/dstack/mock` **stay in the repo.** They are the in-process
  double for unit and envtest, where no control plane is allowed. What ends is the fake being what
  e2e runs against. Do not delete them.
- `test/e2e/*_test.go` (the Go specs) belong to the dstack-client plan; `test/e2e/cluster/**` (the
  environment) belongs to the Helm brief. Same directory tree, different owners — check which
  before editing.
