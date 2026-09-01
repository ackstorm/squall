# Review Findings Before the 0.1.0 Tag — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development.
> Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the ten adjudicated review findings, plus two the review did not see, before
v0.1.0 is tagged.

**Architecture:** No new subsystems. Findings 1 and 2 are one bug in two directions and are
fixed together. Everything else is local.

**Tech Stack:** Go 1.26.6 (containerized — every command through `./scripts/dev.sh`),
controller-runtime, Helm.

---

## Global Constraints

- **The spec is authoritative.** `docs/specs/squall-spec-v0_18-RC.md`.
- **`0→1` fails open, `1→0` fails safe.** Wake may tolerate uncertainty; sleep must not.
- **Squall NEVER sends `force`** (F18, §5.2, AC13), enforced by construction.
- **All commands through the wrapper.** `./scripts/dev.sh make test-unit | test-envtest | qa-lint`.
  The lint target is `qa-lint`. Never bare `go`/`make`. Never `DOCKER_BUILDKIT=0`.
- **`go test ./...` on its own does NOT work** and is not a gate. It fails with
  `/usr/local/kubebuilder/bin/etcd` missing. That path is a red herring: envtest assets are
  pre-baked at `/opt/envtest/k8s/1.31.0-linux-amd64/` and `make test-envtest` injects
  `KUBEBUILDER_ASSETS` at invocation. **Use `make test-envtest`.** (Cost a previous executor a
  false "missing binary" report.)
- **CRD changes are two files** — `./scripts/dev.sh make manifests generate` rewrites
  `config/crd/bases/` and `deploy/helm/squall/crds/`. Helm never upgrades CRDs (D88): deploy
  with `kubectl apply --server-side --force-conflicts`.
- **Git rules for concurrent agents:** never `git add -A`; never `git reset`, `--amend` or
  rebase past a commit you did not create; never `git checkout --` / `restore` on a dirty
  tree. **Only add commits.**
- **The kind cluster `squall-test` is SHARED and LIVE.** `cluster-up` and `cluster-hydrate`
  are safe; **never run `cluster-down`.** Check `kubectl -n squall get models` for an awake
  Model before hydrating.
- **Ledger numbering starts at D134.** D133 is the F6 audit entry.

**Baseline:** branch `feat/v01-remaining-blockers` at `180dc67`, `make test-envtest` green
(exit 0, `-race`), verified 2026-09-01.

---

## Block A — the D129 pair. Highest risk; money and live generations.

Findings 1 and 2 are **the same bug in opposite directions**, and fixing them separately
invites one fix to reopen the other. Both come from treating "no proxy replica holds a key for
this Model" as a finished verdict.

- **#1 says the verdict is too weak.** A Model wakes, becomes Ready via dstack's own probe,
  and nobody ever forwards a request to it — the client that triggered the wake gave up first.
  `Begin` never fired, so no replica has a key, so `AnyData` is false. `status.lastRequestAt`
  is never seeded, because its writer is gated on `AnyData` (`model_controller.go:472`). So
  `sleepDue` returns false forever. And `UncontrolledSince` is cleared (`:477`) precisely
  because the evidence *is* `Complete`. **Nothing fires. The GPU bills indefinitely.** This is
  the D129 incident class, reintroduced by D129's own fix. (D97 covers the never-Ready half of
  this; the Ready-but-never-forwarded half is open.)
- **#2 says the verdict is too strong.** MEASURED 2026-09-01 on this cluster (k8s v1.31.0):
  a terminating pod is **removed from the `Endpoints` object entirely** — not moved to
  `notReadyAddresses`. First sample after deleting a pod that ignores SIGTERM:

  ```
  t+0s  pod=Running:2026-09-01T08:36:11Z  target_in=ABSENT  | addr=10.244.0.39  notready=
  ```

  So D104's protection ("a replica that refuses the query makes the evidence incomplete") does
  **not** apply: there is nobody to ask, because the pod is not in the list. The deployed
  `squall-proxy` has `terminationGracePeriodSeconds: 30` (the k8s default; nothing sets it)
  and no `preStop` hook, and `cmd/proxy/main.go:172` calls `srv.Shutdown`, which closes the
  listener immediately and lets in-flight streams run. **That is a 30-second window, not a
  race**, in which a proxy pod is serving a live generation and is invisible to
  `gatherActivity`. Post-D129 the surviving pods report no key, the verdict is
  `Complete + AllIdle`, and the `1→0` flip kills the generation.

  Note which generations it selects for: the durable anchor must be older than
  `scaleDownDelay` for the flip to fire, so it preferentially kills **long** generations —
  the expensive ones. Measured lengths on this project: 155s (Ollama), 160-330s (vLLM).

A `preStop` hook does **not** help. The pod leaves `Endpoints` at its `deletionTimestamp`
regardless, so delaying SIGTERM only lengthens the invisible window.

### Task A1: See the terminating replica again — move to EndpointSlice

**Files:** `internal/controller/squall/model_controller.go` (`gatherActivity`),
`config/rbac/role.yaml`, `deploy/helm/squall/templates/` (RBAC), `cmd/controller/main.go`.

`discovery.k8s.io/v1` **retains** terminating pods, with `conditions.serving: true` and
`conditions.terminating: true`. That is precisely the visibility the legacy `Endpoints` API
throws away, and it removes the need for any invented safety margin: the departing pod is
enumerated, so it either reports its in-flight work (`AllIdle` false) or refuses the query
(`Complete` false). Both already block the flip.

- [ ] **Step 1: Write the failing test**

Test that `gatherActivity` enumerates an endpoint whose conditions are
`{ready: false, serving: true, terminating: true}`. Use the package's existing envtest
harness for `gatherActivity` (read the neighbouring tests first; do not invent a new fixture).
The assertion is that the terminating address is in the expected set, so a replica that fails
to answer produces `Complete: false`.

- [ ] **Step 2: Run it, watch it fail**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -run 'GatherActivity' -count=1
```

- [ ] **Step 3: Switch the enumeration**

Replace the `corev1.Endpoints` Get with a List of `discoveryv1.EndpointSlice` selected by
`kubernetes.io/service-name=<proxy service>`. Include an address when
`Conditions.Serving` is nil-or-true — **serving, not ready**. Keep D104's rule intact and say
so in the comment: not-ready still counts, and now terminating does too.

A List error must keep producing `Complete: false` exactly as the Get error did (T4). Do not
let "zero slices found" read as "zero addresses expected".

- [ ] **Step 4: RBAC**

Add `discovery.k8s.io/endpointslices: [get,list,watch]` to the controller Role in **both**
`config/rbac/` and the Helm chart. D125 is the precedent: a kustomize RBAC change that did not
reach the chart silently lost a capability with every test still green. Verify the deployed
Role after hydrating.

- [ ] **Step 5: Run, commit**

```bash
./scripts/dev.sh make test-envtest
git commit -m "fix(activity): see a terminating proxy replica that is still serving"
```

### Task A2: Seed the durable anchor at wake

**Files:** `internal/controller/squall/model_controller.go`, `phase.go`, `phase_test.go`.

- [ ] **Step 1: Write the failing test**

```go
// TestSleepDue_WakeSeedsTheAnchor is finding #1. A Model woke, reached Ready on dstack's
// own probe, and no request was EVER forwarded to it -- the client that caused the wake
// gave up first. No replica holds a key, so AnyData is false and no live timestamp
// exists. Without a seeded anchor sleepDue returns false forever and the GPU bills
// indefinitely, while UncontrolledSince stays clear because the evidence IS complete.
func TestSleepDue_WakeSeedsTheAnchor(t *testing.T) {
	now := time.Now()
	ev := &ActivityEvidence{Complete: true, AllIdle: true, AnyData: false}
	woke := metav1.NewTime(now.Add(-10 * time.Minute))

	if !sleepDue(ev, &woke, 120, now) {
		t.Fatal("woke 10 minutes ago, never served a request: must sleep, not bill forever")
	}
}
```

- [ ] **Step 2: Seed it**

In the status block, alongside the existing `LastRequestAt` writer, seed from the wake when
nothing else has:

```go
	// A wake is caused by demand, so it is the earliest honest "something
	// happened" instant. Without this, a Model that woke and was never
	// forwarded to has NO anchor at all: sleepDue cannot form a verdict and
	// the uncontrolled deadline never starts, because the evidence is
	// perfectly complete -- it just says nothing. Finding #1, 2026-09-01.
	if model.Status.LastRequestAt == nil && model.Status.WakeStartedAt != nil {
		model.Status.LastRequestAt = model.Status.WakeStartedAt.DeepCopy()
	}
```

Keep the monotonic guard on the real writer: a seeded anchor must be overwritten by the first
genuine `lastRequestAt`, and never move backwards.

- [ ] **Step 3: Run, commit**

```bash
./scripts/dev.sh go test ./internal/controller/squall/ -count=1 -short
git commit -m "fix(sleep): a wake that never served must still be able to sleep"
```

### Task A3: Let the proxy actually finish a generation

**Files:** `deploy/helm/squall/templates/proxy-deployment.yaml`, `values.yaml`.

Independent of #1 and #2, and arguably worse than both: `terminationGracePeriodSeconds` is
**unset**, so it is k8s's default 30s. `srv.Shutdown` waits for in-flight handlers, but kubelet
SIGKILLs at 30s regardless. Every generation longer than 30 seconds is killed on **every**
proxy rollout — measured generation lengths on this project are 155s and 160-330s. D113 built
a graceful drain that the manifest does not let finish.

- [ ] **Step 1: Set it, from a value, with the reasoning in the comment**

Default it to something of the order of `holdTimeout`, not to a number nobody chose. State in
the comment that 30s was never a decision and that measured generations exceed it by 5-10x.

- [ ] **Step 2: Verify on the deployed object**

```bash
kubectl -n squall-system get deploy squall-proxy \
  -o jsonpath='{.spec.template.spec.terminationGracePeriodSeconds}{"\n"}'
```

- [ ] **Step 3: BLOCK A GATE + mutation sweep**

```bash
./scripts/dev.sh make test-unit && ./scripts/dev.sh make test-envtest && ./scripts/dev.sh make qa-lint
```

| # | Mutation | Test that MUST go red |
|---|---|---|
| MA1 | `gatherActivity` filters on `Ready` instead of `Serving` | the terminating-endpoint test |
| MA2 | EndpointSlice List error returns an empty expected set | the T4 incomplete-evidence test |
| MA3 | drop the wake seeding | `TestSleepDue_WakeSeedsTheAnchor` |
| MA4 | seeding overwrites a real `lastRequestAt` | the monotonicity test |

---

## Block B — the remaining money/safety findings

### Task B1: #3 — fresh demand resets the uncontrolled clock

**DECIDED by the owner, 2026-09-01. Do not reopen.**

`uncontrolledDue` ignores `hasDemand`, and the parameter is already in scope at the call site
(`phase.go:242`). The demand annotation and the activity query are **two different
mechanisms** — the proxy patches the annotation through the dynamic client
(`patcher.go:58`) while the controller queries `/activity` over HTTP — so one can break while
the other works. When it does:

```
t+0    evidence incomplete -> UncontrolledSince starts; traffic flowing, GPU serving fine
t+23m  uncontrolledDue fires -> Apply Replicas 0 -> KILLS A SERVING GPU
t+23m  next request -> hasDemand true -> wake -> paid cold start (2-6m measured)
t+49m  again. And again. It never converges, because nothing in the loop repairs /activity.
```

**The ruling:** fresh demand **resets** the clock. It does not veto permanently, and it does
not silence the alarm. The case that matters is still covered: if the proxy dies outright it
stops patching, the annotation **self-expires** on its `ScaleDownDelaySeconds` TTL,
`hasDemand` goes false, and the deadline fires.

- [ ] **Step 1: Test both directions** — fresh demand does not tear down; expired demand does.
- [ ] **Step 2: Pass `hasDemand` into the decision and reset `UncontrolledSince` on fresh demand.**
- [ ] **Step 3: The metric must NOT reset.** `squall_model_uncontrolled_seconds` keeps
      climbing so "squall cannot measure this" stays visible even when it declines to act.
      Write a test for this specifically; it is the whole point of the ruling.

### Task B2: #4 — `EnsureSSHKey` fails open to `""` on a live run

`applyEnvFor` (`model_controller.go:513`) uses `EnsureSSHKey` on the `Replicas > 0` branch.
`EnsureSSHKey` **fails open by returning `""`**, and on a flip over a live run `""` is fatal:
dstack answers `400 Cannot override active run`, because any spec difference beyond replicas
is an override. That is the D115 addendum reproduced on the wake direction — and `0→1` failing
**closed** violates the invariant.

The review proposed "prefer `Current` when non-nil". **Too wide**: a wake must keep resolving
env freshly, or a rotated secret is frozen forever. Only the key falls back:

```go
	key := EnsureSSHKey(ctx, r.Client, r.operatorNamespace())
	if key == "" && action.Current != nil {
		// Never send an empty key over a LIVE run: dstack reads any spec
		// difference beyond replicas as an override and answers 400 (D115
		// addendum, measured). EnsureSSHKey fails open to "", so a transient
		// RBAC or API error would otherwise make every wake fail, persistently.
		key = action.Current.SSHKeyPub
	}
	return withModelUID(resolvedEnv, model), key, nil
```

- [ ] Test: `EnsureSSHKey` failing + `Current` non-nil ⇒ the run's own key is sent, env still fresh.

### Task B3: #5 — the audit log always reports `uncontrolledSince=nil`

`model_controller.go:467` logs `model.Status.UncontrolledSince`, but `:311` clears it earlier
in the same pass. Capture before the clear. One line, and it is the only forensic record of
why the flip fired.

- [ ] **BLOCK B GATE:** `test-unit`, `test-envtest`, `qa-lint`, plus mutations for B1's
      "metric does not reset" and B2's empty-key fallback.

---

## Block C — correctness debt and hygiene

- [ ] **#10 — the dstack client has no deadline.** CONFIRMED: `client.go:367` falls back to
      `http.DefaultClient` (`Timeout: 0`) and `cmd/controller/main.go:242` passes `nil`. The
      Reaper's `Start(ctx)` is the manager context, which has no deadline either, and Sweep is
      a single goroutine on a ticker — **one hung dstack call leaves orphan GC dark until the
      process restarts**. Same shape as D114, which was real. Give `NewHTTPClient` a real
      client with a timeout at the call site, and a per-sweep context deadline.
- [ ] **#9 — no tests on the new dstack wire plumbing.** `ssh_key_pub` encode/decode and
      `replicaPricePerHour` have none. This is a **measured money path** — the D115 addendum
      cost two hours of billing — and the project's own rule is mutate-to-red before claiming
      coverage. Add table tests and prove them non-vacuous.
- [ ] **#7 — the finalizer leaks the new gauge.** It calls `AgeMetrics.Forget` and
      `PriceMetrics.Forget` (`finalizer.go:131,134`) but not the uncontrolled collector, so a
      deleted Model leaves a climbing series forever. One line, beside its two siblings.
- [ ] **#6 — `activity.go:73` still documents the removed behaviour.** The paragraph describes
      no-data-means-incomplete, which D129 deliberately reversed. It reads as an instruction to
      restore it. **This is the most dangerous of the doc findings**: acting on it reopens the
      2h21m incident.
- [ ] **#8 — three stale comments.** D26's "no production data source yet" (it has one now,
      measured `1.89445`), the matching ledger row, and the `ProxyService` zero-value doc,
      which now reads backwards.

---

## Task D: Ledger

Append, starting at **D134**. Never renumber, never delete.

- **D134** — Finding #1: a Model that woke, reached Ready and was never forwarded to has no
  anchor at all, so neither `sleepDue` nor the uncontrolled deadline can fire. D129's fix
  reintroduced D129's incident class through a different door. Fixed by seeding from
  `wakeStartedAt`.
- **D135** — MEASURED 2026-09-01, k8s v1.31.0: a terminating pod is removed from the legacy
  `Endpoints` object entirely, **not** moved to `notReadyAddresses`. Record the verbatim
  sample. Consequence: D104's protection did not cover a proxy pod that is draining a live
  generation, and the window is `terminationGracePeriodSeconds` (30s, the unset default), not
  a sub-second race. Fixed by moving to EndpointSlice.
- **D136** — `terminationGracePeriodSeconds` was never set on squall-proxy, so D113's graceful
  drain was capped at k8s's 30s default while measured generations run 155-330s. Every proxy
  rollout SIGKILLed any generation longer than 30 seconds.
- **D137** — the e2e loop-model fixture targeted `backends: [vastai]` with a non-existent
  Ollama digest. It failed safe **only** because the e2e dstack has no vastai backend
  configured; with one, the suite would have rented a real GPU on every run. Fixed in
  `180dc67`. Recorded because the failure mode is "a test suite one config change away from
  spending money", not "a broken fixture".
- **D138** — `go test ./...` is not a gate and never was: it fails on a missing
  `/usr/local/kubebuilder/bin/etcd`, which is a red herring. Assets are pre-baked at
  `/opt/envtest/` and `make test-envtest` injects `KUBEBUILDER_ASSETS`. Cost one executor a
  false "missing binary" report. Add it to `docs/references/toolchain-and-traps.md`.
