# Testing discipline — the failure mode this project keeps hitting

Three reviews in a row (Phases 4, 5, 6) found **no code bug**. Every fact was implemented
correctly. What they found instead is worse in the long run: **tests that pass for the wrong
reason** — each proven by constructing a mutation that breaks a documented fact while the
whole suite stays green.

**The rule:** before claiming a behaviour is covered, mutate the implementation to break it
and watch a test go red. A mutation that leaves the suite green is a finding, not a
formality. Revert mutations *by hand* — never `git checkout --`, the tree may hold another
agent's uncommitted work.

Keep the tests listed below. They look redundant. They are the only thing standing between a
subtly wrong implementation and a suite that stays green anyway.

## Phase 4 — the fake dstack server

1. **The gateway's anti-leak ordering was unverified.** Every "bad token" test applied the
   service first, so only "bad token + existing service → 403" was covered. Swapping the
   existence check ahead of the token check passed the entire suite while reintroducing an
   existence leak. Fixed by: `GatewayGet("no-such-service", "wrong-token")` → **403**.
2. **The gateway happy path had zero coverage.** `return http.StatusOK` was never executed;
   every test deliberately hit 503/404/403. Hard-coding 503 there would have passed
   everything. Fixed by: applied + `replicas: 1` + valid token → **200**.
3. **F18's check ordering was unverified.** The only force test ran against a *fresh* server,
   which never reaches the CAS comparison. Moving the `Force` check after the CAS switch
   passed all tests. Fixed by: existing run + stale `BaseDeploymentNum` + `Force: true` →
   `ErrForceForbidden`, not `ErrResourceChanged`.
4. **Structural: the clock lived in a test-double package.** `Clock`/`RealClock`/`FakeClock`
   were in `internal/dstack/mock`. Production controller code importing a mock package is a
   layering violation that drags the whole fake server into the controller's dependency
   graph. Hoisted verbatim into **`internal/clock`**; the mock imports it. Phases 7 and 8
   import `internal/clock`, never the mock.

The clock design itself is sound: `Tick()`-polls-elapsed-time rather than real timers is the
right shape for a level-triggered idempotent reconciler — no timer leaks, no goroutines
racing the detector. Keep the pattern.

## Phase 5 — the dstack client

**Nothing defended the client's authentication.** Commenting out all three `c.authorize(...)`
calls in `Get`, `Delete` and `ListRuns` left the **entire suite green** — the client could
have silently stopped authenticating and no test would have noticed.

Root cause was not a missing assertion: the fake enforced a bearer token on
`GET /gateway/{name}` but on none of the `/runs*` routes, so there was nothing to fail
against. Fixed structurally — the fake now enforces the token on the run-management routes
too, with the check placed **before any existence lookup** so a bad token cannot leak whether
a run exists. The same mutation now turns 9 tests red.

Also closed: `ListRuns` terminal-exclusion had no client-level test (only the mock package's
own), and the `httpClient.Do` transport-error branch was untested in all four methods. That
last one matters beyond tidiness — a network failure must never be mistaken for
`ErrNotFound`, because the sleep path must treat an unreachable answer as "stay awake", never
as "assume idle".

## Phase 6 — the wake path

Two test-*quality* defects, self-reported by the implementer:

- **The AC4 coalescing test fails by timeout, not by assertion.** Mutating `phase.go` to
  always `Apply: true` produced a reconcile storm (re-Apply → status churn → watch triggers)
  that hit the 10s bound instead of a clean `ApplyCount != 1`. A test whose failure mode is
  "it hung" teaches the next person nothing, and invites a later "fix" that just raises the
  timeout.
- **The AC13 test's assertions do not discriminate the failure they were designed to catch.**
  Mutating the reconciler to swallow the `Apply` error still went red — but via an unintended
  path: both goroutines reached `Status().Update` and the second hit a Kubernetes 409 on the
  status subresource, tripping a `default: t.Fatalf` branch rather than the intended
  "swallowed error → false success" assertion.

## What still has no test, and matters most

The sleep path's hardest case, which **nothing in the AC/PoC set covers**: replica B is
unreachable while replica A reports idle → **no flip**. Not "assume idle", not "use the last
known value". The controller must enumerate the proxy Service's EndpointSlices and require a
*complete, fresh answer from every replica* before sleeping. Any replica unreachable, stale
or ambiguous → stay awake. An implementer will optimise that away by accident.
