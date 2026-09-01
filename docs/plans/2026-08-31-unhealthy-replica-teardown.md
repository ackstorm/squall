# Unhealthy Replica Teardown Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement
> this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A replica that is taking traffic and delivering nothing for 15 minutes is torn down
to zero and left asleep until the next request, instead of being kept alive forever by the
in-flight requests it is failing.

**Architecture:** Squall already collects `lastSuccessAt` per proxy replica and aggregates it
as `ActivityEvidence.NewestLastSuccessAt` — and the sleep decision never reads it. This plan
(1) tightens what "success" means to *a 2xx delivered in full*, (2) adds `spec.unhealthyAfter`,
and (3) adds a second flip condition beside `sleepDue` that fires on **traffic without
success**. Both flips produce the same actuation — `Apply{Replicas: 0}`, phase `Asleep` — so
the proxy, the wait contract and the wake path are untouched. The two are told apart by a new
`Healthy` condition on the Model, not by a new phase.

**Tech Stack:** Go 1.26.6, controller-runtime, kubebuilder CRD markers. All toolchain commands
go through `./scripts/dev.sh`.

## Global Constraints

- **`docs/specs/squall-spec-v0_18-RC.md` is authoritative.** Where this plan and the spec
  disagree, say so rather than silently picking one.
- **Wake may tolerate uncertainty; sleep must not. `0→1` fails open, `1→0` fails safe.**
  This plan adds a *destructive* trigger and every default must be chosen on that basis.
- **Squall NEVER sends `force`** (F18, §5.2, AC13). `dstack.ApplyRequest` has no `Force` field.
- **`make test-unit` must never need a control plane.** Pure tests carry no skip; envtest cases
  skip under `-short`.
- **Never `git add -A`**, never `git reset`/`--amend`/rebase past a commit you did not create,
  never `git checkout --`/`restore` on a dirty tree. Only ADD commits.
- Toolchain: `./scripts/dev.sh make test-unit|test-envtest|qa-lint`. The lint target is
  **`qa-lint`**, not `lint`.
- **Before claiming a behaviour is covered, mutate the implementation and watch a test go red.**
  Run the mutation sweep as ONE pass at the end (Task 5), not after each behaviour.
- Gates run **once**, at the end. Do not re-run a gate whose inputs provably did not change.

## Decisions taken (do not reopen)

Settled with the owner on 2026-08-31:

| Question | Decision |
|---|---|
| When is a request a "success"? | **At completion** — the response was 2xx AND was streamed to the client in full. |
| Threshold | **15 minutes**, as `spec.unhealthyAfter`, CRD default `15m`. |
| What happens on unhealthy | **Scale to 0 and wait for a request.** No automatic recreate. If a client is still sending, the next request wakes a fresh run; if nothing is asking, no GPU is bought. |
| How much evidence before the verdict | **At least 3 failed requests since the last success** (`spec.health.failureThreshold`, CRD default 3), Kubernetes' `failureThreshold` by another name. Time alone is not enough — see below. |
| Active probe? | **No.** dstack already runs the liveness analogue and it is what failed here. |

## Why this is not a percentage

Rejected on the owner's own data (2026-08-28/29, 2,443 successful responses over 14h32m):
gaps between consecutive successes were p50 2s, p90 59s, p99 293s, max 1030s. A ratio rule
needs a window, a minimum sample and hysteresis — three knobs — and is pathological at low
volume (one request, one failure, 100% failure rate, GPU killed). Time-since-last-success
needs one knob and is already computed.

**Known limitation, stated up front:** this rule would NOT have caught the night of
2026-08-28. That model was *degraded* (correct 2xx every ~2s at the median, too slow for the
offered load), not *broken*. This plan targets the broken case. The degraded case is D94 and
is out of scope.

## Why time alone is not enough — the thin-evidence false positive

Time-since-last-success on its own kills healthy GPUs under sporadic traffic. A model serves
fine, traffic goes quiet for 20 minutes, then ONE request arrives and fails: the last success
is now 20 minutes old and traffic is current, so the rule fires and tears down a working
replica on the evidence of a single failed request.

So the verdict needs a second, independent condition: **at least
`spec.health.failureThreshold` requests must have failed since the last success.** This is
exactly Kubernetes' `failureThreshold` and it is a floor on evidence, not a tuning knob — it
cannot be disabled, and a value <= 0 is read as the default 3.

The two conditions are deliberately different in kind. Time says "this has been going on long
enough to not be a blip". Count says "we asked enough times to be sure". Either alone has a
cheap counter-example; together they do not.

## Why not an active health check

The obvious Kubernetes instinct is a liveness probe, and squall already has one: dstack runs
`spec.probe` (`/health`, every 10s, `readyAfter: 2`) and terminates the run when it fails.
That is the analogue, it is already wired, and **it is what failed here.** MEASURED
2026-08-29: `/health` answered `200 OK` at 08:49:33, long after the endpoint had stopped being
useful to any client. A process-liveness probe cannot see "answers its own health endpoint but
delivers nothing to callers".

An active *inference* probe would see it, and is still rejected:

- It contradicts §6/§10 — squall probes nothing itself; readiness has two named evidences and
  both are first-party traffic. The held request IS the oracle (§7).
- It contends for the GPU it is measuring, so under exactly the load where the answer matters
  it degrades the thing it is checking.
- It costs tokens on every model, forever, to answer a question that real traffic already
  answers for free whenever there is traffic — and when there is no traffic, the idle flip
  owns the decision and the health of the replica is moot.

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `internal/proxy/handler.go` | Per-request data path | `commit()` records success only after a full 2xx delivery, and records a failure otherwise |
| `internal/proxy/activity.go` | Per-replica activity ledger | `+ Failure()`, `+ failuresSinceSuccess` counter reset by `Success()` |
| `api/squall/v1alpha1/activity.go` | Proxy->controller wire | `+ ModelActivity.FailuresSinceSuccess` |
| `internal/controller/squall/activity.go` | Idle-evidence aggregation | `+ ActivityEvidence.FailuresSinceSuccess` (sum across replicas) |
| `api/squall/v1alpha1/model_types.go` | CRD schema | `+ ModelHealth`, `+ spec.health` |
| `api/squall/v1alpha1/conditions.go` | Condition/reason constants | `+ ConditionHealthy`, `+ ReasonNoSuccessfulResponses`, `+ ReasonHealthy` |
| `internal/controller/squall/phase.go` | Pure state machine | `+ unhealthyDue()`, `+ Action.Unhealthy`, wired into `Decide` |
| `internal/controller/squall/model_controller.go` | Impure reconcile | Sets the `Healthy` condition from `Action.Unhealthy` |
| `config/crd/bases/...models.yaml`, `deploy/helm/...` | Generated | `make gen-manifests helm-sync` |

---

### Task 1: Success/failure accounting in the proxy

**Files:**
- Modify: `internal/proxy/activity.go` (`modelCounters`, `Success`, `Report`)
- Modify: `api/squall/v1alpha1/activity.go` (`ModelActivity`)
- Modify: `internal/proxy/handler.go` (`commit`; and the Ready-but-unservable wait-contract branch)
- Test: `internal/proxy/activity_test.go`, `internal/proxy/handler_test.go`

**Interfaces:**
- Produces: `ActivityTracker.Failure(model string)`,
  `ModelActivity.FailuresSinceSuccess int` (JSON `failuresSinceSuccess`). Both used by Task 3.

Today `commit()` calls `h.Activity.Success(model)` *before* streaming and for **any** status
except 403. A 500 counts as a success and a response that dies mid-stream counts as a success,
so `lastSuccessAt` is really `lastResponseAt`. Nothing counts failures at all. Task 3 cannot
key a destructive decision off either until both are fixed.

- [ ] **Step 1: Extend the wire type**

In `api/squall/v1alpha1/activity.go`, add to `ModelActivity`:

```go
	// FailuresSinceSuccess is how many requests this replica has failed for
	// this Model since it last delivered a 2xx in full. Reset to 0 by every
	// success, so it is a CONSECUTIVE-failure count, not a lifetime total.
	//
	// It is the evidence floor under the unhealthy verdict: time alone fires on
	// a single failed request after a quiet stretch and tears down a healthy
	// replica. An older proxy replica mid-rollout omits this field and it
	// decodes to 0, which reads as "no failure evidence here" and can only ever
	// PREVENT a teardown. That is the safe direction and is deliberate.
	FailuresSinceSuccess int `json:"failuresSinceSuccess"`
```

- [ ] **Step 2: Write the failing tracker test**

Add to `internal/proxy/activity_test.go`:

```go
// TestActivityTracker_FailuresSinceSuccess pins the consecutive-failure
// counter: it is the evidence floor for the unhealthy teardown, so "reset on
// success" is the load-bearing half, not the increment.
func TestActivityTracker_FailuresSinceSuccess(t *testing.T) {
	tr := NewActivityTracker(nil)

	if got := tr.Report().Models["m"].FailuresSinceSuccess; got != 0 {
		t.Fatalf("unseen model reports %d failures, want 0", got)
	}

	tr.Failure("m")
	tr.Failure("m")
	if got := tr.Report().Models["m"].FailuresSinceSuccess; got != 2 {
		t.Fatalf("after 2 failures = %d, want 2", got)
	}

	tr.Success("m")
	if got := tr.Report().Models["m"].FailuresSinceSuccess; got != 0 {
		t.Fatalf("a success must reset the counter, got %d, want 0", got)
	}

	tr.Failure("m")
	if got := tr.Report().Models["m"].FailuresSinceSuccess; got != 1 {
		t.Fatalf("after the reset = %d, want 1", got)
	}
}
```

- [ ] **Step 3: Write the failing handler test**

Add to `internal/proxy/handler_test.go`:

```go
// TestCommit_SuccessOnlyOnDeliveredTwoXX pins evidence (b) to what it claims
// to mean. Before 2026-08-31 a 500 recorded a success, which made "no
// successful response in 15 minutes" unable to fire against exactly the
// replica it exists to catch: one answering, and answering badly.
func TestCommit_SuccessOnlyOnDeliveredTwoXX(t *testing.T) {
	for _, tt := range []struct {
		name         string
		status       int
		wantSuccess  bool
		wantFailures int
	}{
		{"200 counts as success", http.StatusOK, true, 0},
		{"204 counts as success", http.StatusNoContent, true, 0},
		{"500 is a failure", http.StatusInternalServerError, false, 1},
		{"429 is a failure", http.StatusTooManyRequests, false, 1},
		{"400 is a failure", http.StatusBadRequest, false, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer backend.Close()

			c := NewCache()
			c.Set("m", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady, Schedulable: true})
			h := newHandler(t, c, backend.URL)

			h.ServeHTTP(httptest.NewRecorder(), chatRequest("m"))

			rep := h.Activity.Report().Models["m"]
			if got := !rep.LastSuccessAt.IsZero(); got != tt.wantSuccess {
				t.Fatalf("recorded success = %v, want %v (status %d)", got, tt.wantSuccess, tt.status)
			}
			if rep.FailuresSinceSuccess != tt.wantFailures {
				t.Fatalf("failures = %d, want %d (status %d)", rep.FailuresSinceSuccess, tt.wantFailures, tt.status)
			}
		})
	}
}
```

- [ ] **Step 4: Run both and watch them fail**

```sh
./scripts/dev.sh go test ./internal/proxy/ -count=1 -run 'TestActivityTracker_FailuresSinceSuccess|TestCommit_SuccessOnlyOnDeliveredTwoXX'
```

Expected: FAIL to compile — `Failure` and `FailuresSinceSuccess` do not exist.

- [ ] **Step 5: Implement the tracker**

In `internal/proxy/activity.go`, add the field to `modelCounters`:

```go
type modelCounters struct {
	inFlight             int
	lastRequestAt        time.Time
	lastSuccessAt        time.Time
	failuresSinceSuccess int
}
```

Reset it inside `Success`, next to `c.lastSuccessAt = now`:

```go
	c.lastSuccessAt = now
	// The reset is the point. A lifetime failure total would condemn a replica
	// forever for a bad minute; what the unhealthy verdict needs to know is
	// whether anything has worked SINCE.
	c.failuresSinceSuccess = 0
```

Add `Failure`, mirroring `Success`:

```go
// Failure records that a request for model reached a verdict about the replica
// and the replica did not deliver: a committed non-2xx, or a Ready Model the
// gateway would not serve. It is NOT called for a client that disconnected
// mid-stream (not the replica's fault) nor for the ordinary wait-contract of a
// Model that is still waking (nothing has been promised yet). Counting either
// would let normal cold starts accumulate towards a teardown.
func (t *ActivityTracker) Failure(model string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.models[model]
	if !ok {
		c = &modelCounters{}
		t.models[model] = c
	}
	c.failuresSinceSuccess++
}
```

Carry it in `Report`:

```go
		report.Models[name] = squallv1alpha1.ModelActivity{
			InFlight:             c.inFlight,
			LastRequestAt:        c.lastRequestAt,
			LastSuccessAt:        c.lastSuccessAt,
			FailuresSinceSuccess: c.failuresSinceSuccess,
		}
```

- [ ] **Step 6: Implement the handler half**

In `internal/proxy/handler.go`, replace the tail of `commit`:

```go
	rec.outcome = outcomeCommitted
	// Evidence (b), tightened 2026-08-31. Two things changed and both matter to
	// the teardown that now reads this:
	//
	//   - Only a 2xx counts. This used to fire for any status we agreed to
	//     stream, so a replica answering 500 to everything looked exactly as
	//     healthy as one serving tokens.
	//   - It fires AFTER delivery, not before. The owner's choice ("success al
	//     completar"): a replica that accepts a request, sends headers and then
	//     hangs is precisely the failure this evidence must not launder into
	//     proof of health.
	//
	// A client that disconnects mid-stream is NOT the replica's failure, so it
	// records neither a success nor a failure. The next request will.
	if err := streamCommit(w, resp); err != nil {
		slog.Warn("client disconnected mid-stream", "model", model, "err", err)
		return
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		h.Activity.Success(model)
		return
	}
	h.Activity.Failure(model)
```

And in the `action.Forward` branch, where a Ready Model could not be served, record the
failure — this is the shape the 2026-08-29 incident actually took, so leaving it out would
miss the case that motivated the feature:

```go
		if res != attemptCommit {
			// Ready in cache but the gateway is not serving: answer the wait
			// contract rather than a bare 502, so the client sees a truthful
			// state. It is also a FAILURE for the health verdict -- the Model
			// was advertised as ready and this request got nothing.
			h.Activity.Failure(model)
			rec.outcome = outcomeWaitContract
			h.answerWait(w, Action{DeadlineStatus: http.StatusServiceUnavailable, DeadlineState: WaitWaking})
			return
		}
```

- [ ] **Step 7: Run the whole proxy package**

```sh
./scripts/dev.sh go test ./internal/proxy/ -count=1
```

Expected: PASS. If an existing test asserted a success on a non-2xx, that test encoded the
bug — fix the test and say so in its comment.

- [ ] **Step 8: Commit**

```sh
git add internal/proxy/activity.go internal/proxy/activity_test.go internal/proxy/handler.go internal/proxy/handler_test.go api/squall/v1alpha1/activity.go
git commit -m "feat(proxy): count consecutive failures, and only call a delivered 2xx a success"
```

---

### Task 2: `spec.health` and the `Healthy` condition

**Files:**
- Modify: `api/squall/v1alpha1/model_types.go` (beside `ScaleDownDelaySeconds`, ~line 377)
- Modify: `api/squall/v1alpha1/conditions.go`
- Regenerate: `config/crd/bases/squall.ackstorm.ai_models.yaml`, chart copy

**Interfaces:**
- Produces: `ModelSpec.UnhealthyAfter metav1.Duration`, `ConditionHealthy`,
  `ReasonNoSuccessfulResponses`, `ReasonHealthy` — all consumed by Tasks 3 and 4.

- [ ] **Step 1: Add the spec field**

In `api/squall/v1alpha1/model_types.go`, after `ScaleDownDelaySeconds`:

```go
	// UnhealthyAfter is the "taking traffic and delivering nothing" verdict:
	// once the run is up and dstack's probes are ready, if requests keep
	// arriving but no replica has delivered a 2xx IN FULL for this long, the
	// run is flipped to zero replicas and left Asleep until the next request.
	//
	// This is a DESTRUCTIVE trigger and the second one in squall, so it is
	// deliberately slow. It exists because in-flight requests are what block
	// the idle flip, which gives a failing replica an inverted incentive: the
	// worse it performs, the more requests pile up inside it, and the more
	// "in use" it looks. MEASURED 2026-08-29: 57 requests hung for ~305s each,
	// all night, and the model was never eligible to sleep for one second of it.
	//
	// It does NOT catch a merely SLOW replica (D94). On the night that produced
	// this field the gaps between successful responses were p50 2s / p90 59s /
	// max 1030s — correct output, too slowly. That is a capacity problem and
	// this is a liveness check; do not retune this field to try to cover it.
	//
	// The clock is anchored at the LATER of the newest success and
	// status.wakeStartedAt, so a freshly woken run that has not served anything
	// yet is measured from its own wake and not from the epoch.
	//
	// Zero disables the check entirely. The CRD default is 15m, which must stay
	// comfortably above the slowest legitimate generation — a request that has
	// not finished is not a success, so a threshold below the worst-case
	// generation time would tear down a working GPU mid-answer.
	// +kubebuilder:default="15m"
	// +optional
	UnhealthyAfter metav1.Duration `json:"unhealthyAfter,omitempty"`
```

And immediately after it, the evidence floor:

```go
	// UnhealthyFailureThreshold is how many requests must have failed since the
	// last success before UnhealthyAfter is allowed to fire. Kubernetes'
	// failureThreshold by another name, and it exists because time alone has a
	// cheap counter-example: a Model that has served fine, gone quiet for
	// twenty minutes, and then failed ONE request has a twenty-minute-old last
	// success and current traffic -- and would be torn down on the evidence of
	// that single failure.
	//
	// It CANNOT be disabled. A value <= 0 is read as the default 3 rather than
	// as "fire with no evidence": 1->0 fails safe, and the one setting that
	// must never be reachable is a destructive trigger with no floor under it.
	// To turn the whole check off, set unhealthyAfter to 0.
	// +kubebuilder:default=3
	// +optional
	UnhealthyFailureThreshold int32 `json:"unhealthyFailureThreshold,omitempty"`
```

- [ ] **Step 2: Add the condition and reasons**

In `api/squall/v1alpha1/conditions.go`, beside the existing condition types:

```go
	// ConditionHealthy is False when the unhealthy flip fired: the run was up,
	// dstack's probes were ready, requests kept arriving, and no replica
	// delivered a 2xx for spec.unhealthyAfter. It is the ONLY way to tell that
	// flip apart from a plain idle sleep after the fact — both end at phase
	// Asleep on purpose, so that the proxy, the wait contract and the wake path
	// need no knowledge of this feature at all.
	ConditionHealthy = "Healthy"
```

and beside the existing reasons:

```go
	// ReasonNoSuccessfulResponses accompanies ConditionHealthy=False.
	ReasonNoSuccessfulResponses = "NoSuccessfulResponses"
	// ReasonHealthy accompanies ConditionHealthy=True, set when a fresh run is
	// minted: a new run has never been judged, and inheriting the previous
	// run's verdict would be a lie about a different machine.
	ReasonHealthy = "Healthy"
```

- [ ] **Step 3: Regenerate and sync the chart**

```sh
./scripts/dev.sh make gen-code gen-manifests
./scripts/dev.sh make helm-sync
git diff --stat config/crd deploy/helm
```

Expected: `unhealthyAfter` appears in the CRD with `default: 15m`, and the chart's copy matches.

- [ ] **Step 4: Commit**

```sh
git add api/squall/v1alpha1/model_types.go api/squall/v1alpha1/conditions.go \
        api/squall/v1alpha1/zz_generated.deepcopy.go config/crd deploy/helm
git commit -m "feat(api): spec.unhealthyAfter and the Healthy condition"
```

---

### Task 3: `unhealthyDue` and the second flip in `Decide`

**Files:**
- Modify: `internal/controller/squall/phase.go`
- Test: `internal/controller/squall/phase_test.go`

**Interfaces:**
- Consumes: `ActivityEvidence{Complete, NewestLastRequestAt, NewestLastSuccessAt,
  FailuresSinceSuccess}`, `Observed.Ready`, `prior.WakeStartedAt`, `spec.health.unhealthyAfter`,
  `spec.Health.FailureThreshold`, `spec.ScaleDownDelaySeconds`
- Produces: `ActivityEvidence.FailuresSinceSuccess int`, `Action.Unhealthy bool`.

**Step 0 first: carry the failure count through the aggregation.** In
`internal/controller/squall/activity.go` add to `ActivityQuery`:

```go
	// FailuresSinceSuccess is this replica's consecutive-failure count for the
	// Model. Absent from an older proxy's report and therefore 0, which can
	// only PREVENT a teardown -- the safe direction.
	FailuresSinceSuccess int
```

and to `ActivityEvidence`:

```go
	// FailuresSinceSuccess is the SUM across replicas of requests failed since
	// each last delivered a success. Summed, not maxed: the question the
	// unhealthy verdict asks is "have we tried enough times to be sure", and
	// three failures spread over three replicas is the same amount of evidence
	// as three on one. A NoData replica contributes nothing, as with every
	// other field.
	FailuresSinceSuccess int
```

Populate it in `aggregateActivity` beside `newest`/`newestSuccess` (skipping `NoData`
replicas, which `continue` before this point), and decode it in `queryActivity`
(`model_controller.go`) from `activity.FailuresSinceSuccess`.

- [ ] **Step 1: Add the `Unhealthy` field to `Action`**

In `internal/controller/squall/phase.go`, in the `Action` struct beside `Alarm`:

```go
	// Unhealthy is true when this pass's flip to Replicas: 0 was the
	// "traffic but no successful responses" verdict rather than a plain idle
	// sleep. Both produce the identical actuation, so the reconciler needs
	// this to report WHICH happened on the Healthy condition. Mirrors Alarm:
	// a diagnosis carried out of the pure layer, never an instruction.
	Unhealthy bool
```

- [ ] **Step 2: Write the failing tests**

Add to `internal/controller/squall/phase_test.go`:

```go
// TestUnhealthyDue is the pure table for the 2026-08-31 liveness verdict.
func TestUnhealthyDue(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	woke := metav1.NewTime(now.Add(-2 * time.Hour))
	const scaleDown int32 = 300
	const after = 15 * time.Minute

	// 5 failures: comfortably over the threshold, so these cases exercise the
	// TIME half. The threshold half gets its own cases at the bottom.
	complete := func(lastReq, lastOK time.Time) *ActivityEvidence {
		return &ActivityEvidence{Complete: true, NewestLastRequestAt: lastReq,
			NewestLastSuccessAt: lastOK, FailuresSinceSuccess: 5}
	}
	withFailures := func(lastReq, lastOK time.Time, n int) *ActivityEvidence {
		e := complete(lastReq, lastOK)
		e.FailuresSinceSuccess = n
		return e
	}

	tests := []struct {
		name     string
		activity *ActivityEvidence
		ready    bool
		woke     *metav1.Time
		after    time.Duration
		want     bool
	}{
		{
			// THE CASE THIS EXISTS FOR: requests still arriving, last delivered
			// 2xx is 20 minutes old.
			name:     "traffic arriving, no success for 20m -> unhealthy",
			activity: complete(now.Add(-10*time.Second), now.Add(-20*time.Minute)),
			ready:    true, woke: &woke, after: after, want: true,
		},
		{
			name:     "successes are recent -> healthy",
			activity: complete(now.Add(-10*time.Second), now.Add(-2*time.Second)),
			ready:    true, woke: &woke, after: after, want: false,
		},
		{
			name:     "no success for 20m but no traffic either -> idle, not unhealthy",
			activity: complete(now.Add(-20*time.Minute), now.Add(-20*time.Minute)),
			ready:    true, woke: &woke, after: after, want: false,
		},
		{
			// Anchored at wakeStartedAt: a run that woke 1 minute ago and has
			// never served anything is not unhealthy, it is new.
			name:     "never succeeded but only just woke -> not unhealthy",
			activity: complete(now.Add(-1*time.Second), time.Time{}),
			ready:    true, woke: ptrTime(now.Add(-time.Minute)), after: after, want: false,
		},
		{
			name:     "never succeeded and woke 20m ago -> unhealthy",
			activity: complete(now.Add(-1*time.Second), time.Time{}),
			ready:    true, woke: ptrTime(now.Add(-20*time.Minute)), after: after, want: true,
		},
		{
			// Provisioning owns the pre-Ready window, not this check.
			name:     "not ready yet -> never unhealthy",
			activity: complete(now.Add(-1*time.Second), time.Time{}),
			ready:    false, woke: ptrTime(now.Add(-20*time.Minute)), after: after, want: false,
		},
		{
			name:     "incomplete evidence -> no verdict",
			activity: &ActivityEvidence{Complete: false},
			ready:    true, woke: &woke, after: after, want: false,
		},
		{
			name:     "nil evidence -> no verdict",
			activity: nil, ready: true, woke: &woke, after: after, want: false,
		},
		{
			name:     "disabled by zero unhealthyAfter",
			activity: complete(now.Add(-10*time.Second), now.Add(-20*time.Minute)),
			ready:    true, woke: &woke, after: 0, want: false,
		},
		{
			// No anchor at all: never succeeded AND never journalled a wake.
			// 1->0 fails safe, so no anchor means no verdict.
			name:     "no success and no wakeStartedAt -> no verdict",
			activity: complete(now.Add(-1*time.Second), time.Time{}),
			ready:    true, woke: nil, after: after, want: false,
		},
		{
			// THE THIN-EVIDENCE CASE the threshold exists for: served fine,
			// went quiet for 20 minutes, then ONE request failed. Time says
			// yes; the evidence floor says no, and it wins.
			name:     "20m since success but only 1 failure -> not enough evidence",
			activity: withFailures(now.Add(-1*time.Second), now.Add(-20*time.Minute), 1),
			ready:    true, woke: &woke, after: after, want: false,
		},
		{
			name:     "one below the threshold -> still not enough",
			activity: withFailures(now.Add(-1*time.Second), now.Add(-20*time.Minute), 2),
			ready:    true, woke: &woke, after: after, want: false,
		},
		{
			name:     "exactly at the threshold -> unhealthy",
			activity: withFailures(now.Add(-1*time.Second), now.Add(-20*time.Minute), 3),
			ready:    true, woke: &woke, after: after, want: true,
		},
		{
			// Many failures but the time window has NOT elapsed: a burst of
			// errors inside a good minute is not a broken replica.
			name:     "threshold met but success is recent -> healthy",
			activity: withFailures(now.Add(-1*time.Second), now.Add(-2*time.Second), 50),
			ready:    true, woke: &woke, after: after, want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unhealthyDue(tt.activity, tt.ready, tt.woke, tt.after, 3, scaleDown, now)
			if got != tt.want {
				t.Errorf("unhealthyDue = %v, want %v", got, tt.want)
			}
		})
	}
}

func ptrTime(t time.Time) *metav1.Time { m := metav1.NewTime(t); return &m }

// TestDecide_UnhealthyFlipsToZeroAndReportsWhy proves the verdict reaches the
// actuation, not just the predicate.
func TestDecide_UnhealthyFlipsToZeroAndReportsWhy(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	woke := metav1.NewTime(now.Add(-time.Hour))
	observed := Observed{
		Run:   &dstack.Run{RunID: "r1", Replicas: 1},
		Ready: true,
		Activity: &ActivityEvidence{
			Complete:            true,
			NewestLastRequestAt:  now.Add(-time.Second),
			NewestLastSuccessAt:  now.Add(-30 * time.Minute),
			FailuresSinceSuccess: 40,
		},
	}
	prior := squallv1alpha1.ModelStatus{RunID: "r1", Phase: squallv1alpha1.ModelPhaseReady, WakeStartedAt: &woke}
	spec := squallv1alpha1.ModelSpec{
		MinReplicas:           0,
		ScaleDownDelaySeconds: 300,
		UnhealthyAfter:            metav1.Duration{Duration: 15 * time.Minute},
		UnhealthyFailureThreshold: 3,
	}

	// hasDemand is TRUE: requests are arriving. The whole point is that this
	// fires anyway — demand is not a veto on a replica that cannot serve it.
	phase, action := Decide(observed, prior, spec, true, now)

	if phase != squallv1alpha1.ModelPhaseAsleep {
		t.Errorf("phase = %q, want Asleep", phase)
	}
	if !action.Apply || action.Replicas != 0 {
		t.Errorf("action = %+v, want Apply with Replicas 0", action)
	}
	if !action.Unhealthy {
		t.Error("action.Unhealthy = false, want true so the reconciler can say WHY")
	}
	if action.Current != observed.Run {
		t.Error("Current must carry the observed run for the CAS")
	}
}
```

- [ ] **Step 3: Run and watch them fail**

```sh
./scripts/dev.sh go test ./internal/controller/squall/ -short -run 'TestUnhealthyDue|TestDecide_Unhealthy' -count=1
```

Expected: FAIL to compile — `unhealthyDue` and `Action.Unhealthy` do not exist.

- [ ] **Step 4: Implement `unhealthyDue`**

Add to `internal/controller/squall/phase.go`, beside `sleepDue`:

```go
// unhealthyDue is the second fail-safe flip (2026-08-31): the run is up, its
// probes are ready, requests keep arriving, and nothing has delivered a 2xx in
// full for unhealthyAfter. That is a replica taking traffic and returning
// nothing, and it must not be kept alive by the very requests it is failing.
//
// Ready is REQUIRED and is what keeps this out of provisioning's way: before
// the engine answers its own health probe, spec.provisioningTimeout owns the
// timeout, not this. (Note that provisioningTimeout's destructive trigger is
// still unimplemented — Task 8.3, OQ3 — so today nothing bounds a model stuck
// Waking. That gap is real and is NOT this function's job.)
//
// Recent traffic is REQUIRED so the two flips stay distinguishable: with no
// traffic, sleepDue already owns the decision and reporting "unhealthy" would
// slander a model nobody is using.
//
// The failure THRESHOLD is the second, independent condition and the two are
// different in kind on purpose. Time says "this has gone on long enough to not
// be a blip"; the count says "we asked enough times to be sure". Either alone
// has a cheap counter-example -- notably a Model that served fine, went quiet
// for twenty minutes, and then failed a single request. It cannot be disabled;
// see spec.Health.FailureThreshold.
//
// The anchor is the LATER of the newest success and the wake instant. Without
// the wake term, a run that has never succeeded would measure from the zero
// time and be declared unhealthy the instant it came up. Without any anchor at
// all — never succeeded, no wake journalled — there is no verdict: 1->0 fails
// safe, and a missing timestamp is not evidence of failure.
// DefaultUnhealthyFailureThreshold is the evidence floor used when a Model
// carries no explicit spec.unhealthyFailureThreshold -- an object stored
// before the field existed, or an explicit 0. Three, matching Kubernetes'
// own probe default. It is a floor, never a target: its only job is to keep
// one or two failed requests from being read as a verdict.
const DefaultUnhealthyFailureThreshold int32 = 3

func unhealthyDue(
	activity *ActivityEvidence,
	ready bool,
	wakeStartedAt *metav1.Time,
	unhealthyAfter time.Duration,
	threshold int32,
	scaleDownDelaySeconds int32,
	now time.Time,
) bool {
	if !ready || unhealthyAfter <= 0 {
		return false
	}
	if activity == nil || !activity.Complete {
		return false
	}
	if now.Sub(activity.NewestLastRequestAt) > time.Duration(scaleDownDelaySeconds)*time.Second {
		return false // no current traffic: sleepDue's business, not this one's.
	}
	if threshold <= 0 {
		threshold = DefaultUnhealthyFailureThreshold
	}
	if activity.FailuresSinceSuccess < int(threshold) {
		return false // not enough evidence yet; one bad request is not a verdict.
	}
	anchor := activity.NewestLastSuccessAt
	if wakeStartedAt != nil && wakeStartedAt.Time.After(anchor) {
		anchor = wakeStartedAt.Time
	}
	if anchor.IsZero() {
		return false
	}
	return now.Sub(anchor) > unhealthyAfter
}
```

- [ ] **Step 5: Wire it into `Decide`**

In `Decide`, immediately after the existing `sleepDue` block in the
`observed.Run.Replicas > 0` branch:

```go
	// Second fail-safe flip, checked after the idle one so that a model which
	// is BOTH idle and unsuccessful is reported as the cheaper, more ordinary
	// of the two. Same actuation, different diagnosis.
	if spec.MinReplicas == 0 && unhealthyDue(observed.Activity, observed.Ready, prior.WakeStartedAt,
		spec.Health.UnhealthyAfter.Duration, spec.Health.FailureThreshold, spec.ScaleDownDelaySeconds, now) {
		return squallv1alpha1.ModelPhaseAsleep, Action{
			Apply:     true,
			Replicas:  0,
			Current:   observed.Run,
			Unhealthy: true,
			At:        now,
		}
	}
```

- [ ] **Step 6: Run and watch them pass**

```sh
./scripts/dev.sh go test ./internal/controller/squall/ -short -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```sh
git add internal/controller/squall/phase.go internal/controller/squall/phase_test.go
git commit -m "feat(controller): tear down a replica that takes traffic and delivers nothing"
```

---

### Task 4: The reconciler reports why, and an envtest proves the flip

**Files:**
- Modify: `internal/controller/squall/model_controller.go`
- Test: `internal/controller/squall/model_controller_sleep_envtest_test.go`

**Interfaces:**
- Consumes: `Action.Unhealthy`, `ConditionHealthy`, `ReasonNoSuccessfulResponses`, `ReasonHealthy`

- [ ] **Step 1: Set the condition at the flip**

In `Reconcile`, immediately after the `if action.Alarm { ... }` block:

```go
	if action.Unhealthy {
		// LOUD. This spent money and then stopped spending it; an operator who
		// finds a Model asleep must be able to see it was pushed, not that it
		// went quiet.
		logger.Error(nil, "replica took traffic and delivered nothing; scaling to zero and waiting for a request",
			"model", model.Name, "unhealthyAfter", model.Spec.Health.UnhealthyAfter.Duration.String())
		meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
			Type: squallv1alpha1.ConditionHealthy, Status: metav1.ConditionFalse,
			Reason:  squallv1alpha1.ReasonNoSuccessfulResponses,
			Message: fmt.Sprintf("no replica delivered a 2xx for %s while requests were arriving", model.Spec.Health.UnhealthyAfter.Duration),
		})
	}
```

- [ ] **Step 2: Clear it when a fresh run is minted**

In the existing `if action.Current == nil { ... }` block (which already clears
`ServedModelVerified`), add:

```go
		// A new run has never been judged. Inheriting the previous run's health
		// verdict would be a statement about a different machine.
		meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
			Type: squallv1alpha1.ConditionHealthy, Status: metav1.ConditionTrue,
			Reason: squallv1alpha1.ReasonHealthy,
		})
```

- [ ] **Step 3: Write the envtest**

Add to `internal/controller/squall/model_controller_sleep_envtest_test.go`, matching the
file's existing harness (fake dstack client, fake activity server, `t.Skip` under `-short`):

```go
// TestUnhealthyModelIsScaledToZeroWhileTrafficArrives is the 2026-08-29
// incident as a test: the proxy reports requests arriving RIGHT NOW and a last
// success 30 minutes old. The idle flip cannot fire (traffic is current); the
// unhealthy flip must.
func TestUnhealthyModelIsScaledToZeroWhileTrafficArrives(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest: needs a control plane")
	}
	// Build the Model with UnhealthyAfter: 15m, ScaleDownDelaySeconds: 300,
	// UnhealthyFailureThreshold: 3, a live run at Replicas: 1 with ProbesReady
	// true, and one proxy replica reporting
	// {InFlight: 3, LastRequestAt: now, LastSuccessAt: now-30m,
	//  FailuresSinceSuccess: 40}.
	// Assert, within the suite's usual bounded wait:
	//   - the fake dstack client received exactly one Apply with Replicas 0
	//   - status.phase == Asleep
	//   - ConditionHealthy is False with reason NoSuccessfulResponses
}
```

**Note to the implementer:** this stub is the ONE place in this plan without literal code,
because the envtest harness (fake client type names, activity server helper, wait helper)
must be read from the existing file and matched exactly rather than guessed. Read
`model_controller_sleep_envtest_test.go` in full first and mirror its existing sleep test.

- [ ] **Step 4: Run**

```sh
./scripts/dev.sh make test-envtest
```

Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/controller/squall/model_controller.go \
        internal/controller/squall/model_controller_sleep_envtest_test.go
git commit -m "feat(controller): report the unhealthy teardown on the Healthy condition"
```

---

### Task 5: Mutation sweep, gates, deploy, ledger

**Files:**
- Modify: `docs/references/deviations-and-findings.md`

- [ ] **Step 1: Mutation sweep — one pass over the whole block**

For each mutation: apply, run the named test, confirm RED, restore **inside the same command**
so the tree is never left broken.

| # | Mutation | Must turn red |
|---|---|---|
| 1 | `commit()` records success before `streamCommit` | `TestCommit_SuccessOnlyOnDeliveredTwoXX` |
| 2 | `commit()` drops the 2xx check | same |
| 3 | `unhealthyDue` drops the `ready` guard | `TestUnhealthyDue/not_ready_yet` |
| 4 | `unhealthyDue` drops the recent-traffic guard | `TestUnhealthyDue/no_success_for_20m_but_no_traffic` |
| 5 | `unhealthyDue` drops the `wakeStartedAt` anchor | `TestUnhealthyDue/never_succeeded_but_only_just_woke` |
| 6 | `unhealthyDue` drops the failure-threshold check | `TestUnhealthyDue/20m_since_success_but_only_1_failure` |
| 7 | `unhealthyDue` reads `threshold <= 0` as "no floor" instead of the default | `TestUnhealthyDue` (add a case with threshold 0 and 1 failure) |
| 8 | `Success()` stops resetting `failuresSinceSuccess` | `TestActivityTracker_FailuresSinceSuccess` |
| 9 | `Decide` sets `Unhealthy: false` at the flip | `TestDecide_UnhealthyFlipsToZeroAndReportsWhy` |

A mutation that leaves the suite green is a **finding**, not a formality. Record it.

- [ ] **Step 2: Full gates, once**

```sh
./scripts/dev.sh make test-unit
./scripts/dev.sh make test-envtest
./scripts/dev.sh make qa-lint
```

- [ ] **Step 3: Deploy, and beware the two traps**

D88 — Helm never upgrades CRDs, and `unhealthyAfter` is a **new field**, so it will be
silently pruned unless the CRD is applied by hand:

```sh
kubectl apply --server-side --force-conflicts -f config/crd/bases/squall.ackstorm.ai_models.yaml
```

D89 — a rebuilt image does not reach a running Pod. Build, load, restart, and **verify the
digests match** before believing anything:

```sh
make build-image-controller build-image-proxy
for img in squall-controller:e2e squall-proxy:e2e; do
  docker save $img | docker exec -i squall-test-control-plane ctr -n k8s.io images import -
done
kubectl -n squall-system rollout restart deploy/squall-controller-manager deploy/proxy
kubectl -n squall-system get pods -o custom-columns='POD:.metadata.name,IMAGEID:.status.containerStatuses[*].imageID'
```

- [ ] **Step 4: Ledger**

Append D95 to `docs/references/deviations-and-findings.md`: the inverted incentive (in-flight
requests keeping a failing replica alive), what was built, the decisions taken, and the
**known hazard** — unhealthy → Asleep → a client that is still sending immediately re-wakes a
new run, so a Model that is broken by configuration rather than by machine will provision a
fresh GPU every `unhealthyAfter`. Bounded at ~$0.32/cycle at $1.29/h. No backoff was added:
it fights `0→1 fails open` and the owner chose "wait for a request". Flag for a decision if it
is ever observed.

- [ ] **Step 5: Commit**

```sh
git add docs/references/deviations-and-findings.md
git commit -m "docs: record the unhealthy-teardown design and its flap hazard as D95"
```

---

### Task 6: A live qwen3.8 Model and the test battery

**Files:**
- Create: `docs/runbooks/qwen3-8-27b.yaml` (the Model manifest, checked in)

This task spends real money. Do not start it without the owner saying go.

- [ ] **Step 1: Write the Model**

Identical to the run that produced the incident, plus the new field. `unhealthyAfter: 15m`
is the CRD default but is stated explicitly here because this Model is the thing being tested.

```yaml
apiVersion: squall.ackstorm.ai/v1alpha1
kind: Model
metadata:
  name: qwen3-8-27b
  namespace: squall
spec:
  engine: vllm
  image: vllm/vllm-openai@sha256:2286e8533ca8b6bc777594bae30524f1426ba46ca21797524e06df6a94b06635
  model: Qwen/Qwen3.8-27B-FP8
  args: ["--max-model-len", "131072", "--kv-cache-dtype", "fp8", "--reasoning-parser", "qwen3"]
  env: {VLLM_LOGGING_LEVEL: INFO}
  features: [TextGeneration]
  minReplicas: 0
  holdTimeout: 20m
  drainTimeout: 60s
  maxLifetime: 168h
  provisioningTimeout: 30m
  scaleDownDelaySeconds: 300
  health:
    unhealthyAfter: 15m
    failureThreshold: 3
  fleet: {idleDuration: 10m}
  probe: {path: /health, interval: 10s, readyAfter: 2}
  resources:
    cpu: {count: "4.."}
    memory: 16GB..
    shmSize: 8GB
    gpu: {name: [RTXPRO6000WS, RTXPRO6000S], memory: 90GB..}
  placement:
    backends: [vastai]
    maxPricePerHour: "2.20"
    regions: [es-spain, pt-portugal, fr-france, it-italy, de-germany, nl-netherlands,
              be-belgium, at-austria, ch-switzerland, pl-poland, cz-czechia, se-sweden,
              fi-finland, no-norway, ie-ireland, ro-romania, bg-bulgaria, hu-hungary,
              ee-estonia, lt-lithuania]
```

- [ ] **Step 2: The battery**

Run in order. Each names what it proves and how it fails.

| # | Test | Method | Pass |
|---|---|---|---|
| 1 | Cold wake from zero | Apply the Model, send one request | 2xx within `provisioningTimeout`; `status.phase` Waking→Ready |
| 2 | Served model is the right one | `GET /v1/models` via the proxy | `ConditionServedModelVerified=True`, `status.servedModel == qwen3-8-27b` |
| 3 | Direct SSH path is taken | `status.replica` populated; proxy logs `ssh tunnel to replica established` | Direct, not fallback |
| 4 | **Idle sleep — the D91 regression** | Stop all traffic; watch with **two** proxy replicas, only one of which has served this model | `Asleep` within `scaleDownDelaySeconds` + a reconcile; dstack shows `replicas {min:0,max:0}` |
| 5 | Wake from sleep | One request after the sleep | 2xx; a NEW `status.runId` |
| 6 | **Unhealthy teardown** | With the model Ready, break the engine (`kubectl`-free: point the proxy at a backend returning 500, or stop vLLM inside the replica) while a client keeps sending | Within `unhealthyAfter` AND after >=3 failures: `Asleep`, `ConditionHealthy=False/NoSuccessfulResponses` |
| 7 | Load, correctly configured | Ramp 4→32 concurrent, `reasoning_effort: "medium"`, `max_tokens: 1500`, client timeout **600s** | Zero non-2xx; record tok/s |
| 8 | Load at the edge | 64 concurrent, same settings | Record the 2xx rate and per-stream tok/s. This is the D94 measurement, not a pass/fail |
| 9 | Cost | Vast console vs `status` | Instance count returns to 0 after the battery; no orphan |

- [ ] **Step 3: Record the numbers in the ledger and README**

Update the README's measured table if the SSH-path numbers moved.

---

## Self-review

**Spec coverage.** Every decision the owner gave is implemented: success at completion
(Task 1), 15m (Task 2 default), sleep-and-wait rather than recreate (Task 3 returns
`ModelPhaseAsleep`, no `Apply{Replicas:1}`). The qwen Model and battery are Task 6.

**Placeholders.** One deliberate stub: Task 4 Step 3's envtest body, flagged in the task
itself with the reason (the harness must be read, not guessed) and the instruction to mirror
the existing sleep test. Every other step carries literal code.

**Type consistency.** `UnhealthyAfter metav1.Duration` (Task 2) is read as
`spec.Health.UnhealthyAfter.Duration` (Task 3) and printed as
`model.Spec.Health.UnhealthyAfter.Duration.String()` (Task 4). `UnhealthyFailureThreshold int32`
(Task 2) is passed as `unhealthyDue`'s `threshold int32` (Task 3). `Action.Unhealthy bool`
(Task 3 Step 1) is read as `action.Unhealthy` (Task 4 Step 1). `unhealthyDue`'s signature is
identical in its test call, its definition and its call site in `Decide`, all in Task 3.
`ConditionHealthy` / `ReasonNoSuccessfulResponses` / `ReasonHealthy` are defined in Task 2
and used only in Task 4. `FailuresSinceSuccess` is one name at all four layers: the proxy's
`modelCounters.failuresSinceSuccess`, the wire's `ModelActivity.FailuresSinceSuccess`, the
query's `ActivityQuery.FailuresSinceSuccess` and the evidence's
`ActivityEvidence.FailuresSinceSuccess` (Tasks 1 and 3).

**Two conditions, not one.** Every `want: true` case in `TestUnhealthyDue` satisfies BOTH the
time window and the evidence floor, and there is a dedicated case for each one failing while
the other holds. Mutations 6 and 7 exist to prove neither half is decorative.

**Known gaps, stated not hidden.**
- This does not catch a *degraded* replica (D94). Said in the plan header.
- `spec.provisioningTimeout` still has no destructive trigger (Task 8.3, OQ3), so nothing
  bounds a Model stuck in `Waking`. Observed live on 2026-08-31. Out of scope; recorded.
- The unhealthy→Asleep→re-wake flap is unbounded by design. Recorded as a D95 hazard.


---

## Deviations taken during execution (2026-08-31)

Recorded here rather than silently: the plan is the record, so where the build diverged from
it, the divergence is the interesting part.

1. **`spec.health` block instead of two flat fields.** The plan put `unhealthyAfter` and
   `unhealthyFailureThreshold` directly on `ModelSpec`. The owner asked mid-execution where
   the failure count should be configured and floated `probe.completion_errors`. Both fields
   moved into a `ModelHealth` struct at `spec.health`, because (a) they are ANDed halves of
   one policy and are only correct read together, and (b) `spec.probe` is a straight
   passthrough to dstack's own HTTP probe, so putting a count of REAL completions inside it
   would imply squall probes something itself. It does not.
   The block carries `+kubebuilder:default={}` so nested defaults materialise for a Model that
   omits it entirely.

2. **A ninth mutation, and it survived.** The sweep's mutation 9 — `Decide` reporting
   `Unhealthy: false` at the flip — came back GREEN on the first pass. That was a real finding,
   not a formality: `TestDecide_UnhealthyFlipsToZeroAndReportsWhy` had been specified in the
   plan and never written. Added, plus `TestDecide_IdleSleepIsNotReportedAsUnhealthy` for the
   other direction, and mutation 9 then turned red. All nine are load-bearing.

3. **Task 6 not executed.** The live qwen3.8 Model and its battery spend real money and are
   held for the owner's go-ahead.

4. **Out of scope, recorded not built:** a KubeAI-style `nodeSelector` on the Kubernetes
   backend so Karpenter provisions GPU nodes on demand. Written up in
   `docs/references/decisions-and-open-items.md` with the three questions it needs answered
   first. No code.
