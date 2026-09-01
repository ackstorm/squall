# internal/dstack

A narrow client over the dstack server.
Not an SDK: one HTTP round trip per call, no retries, backoff, circuit
breakers or metrics. Fields on Run may be added when a spec section names
the state they carry, and only once that field is confirmed against a real
dstack server (ledger D1 / the Tier-1 e2e-local suite).

## Rules that are load-bearing

- **`force` is unreachable by construction, not by a guard.** This rule survived contact
  with the real API but changed shape: dstack *requires* the `force` field, so `applyRequest`
  now has one and it is encoded as the literal `false`. What makes AC13 hold is that the only
  constructor is `newApplyRequest`, which hard-codes it — no caller can reach the field, and
  there is no parameter to thread. Do not add one. The losing side of a race must fail loudly.
- **`client.go` must never import `internal/dstack/mock`.** Production code importing a test
  double is the layering violation that forced the `internal/clock` hoist in Phase 4. Only
  `client_test.go` may import the mock. Verify with `go list -deps ./internal/dstack`.
- **Errors are classified from the response BODY, never the status code.** Measured on
  0.21.2: dstack answers **HTTP 400 for both** "run not found" and the CAS conflict. The
  discriminators are `detail[].code == "resource_not_exists"` for `ErrNotFound`, and the
  message substring `"resource has been changed"` for `ErrResourceChanged` (dstack tags the
  CAS conflict with the generic code `"error"`, so the message is all it gives us). A third
  sentinel, `ErrUnauthorized`, comes from 401/403. The earlier 404/409 mapping was invented
  and was wrong in both halves. `detail` is polymorphic — a list, a bare object, or a plain
  string — so decode it through `parseDetails`, never directly.
- **`Delete` returns `ErrNotFound` on an absent run** rather than swallowing to `nil`.
  Deliberate: the caller applies `IgnoreNotFound`-style handling at the layer with enough
  context, and the drain-first finalizer gets an unambiguous "already gone" signal on replay.
- **A transport error must never be mistaken for `ErrNotFound`.** The sleep path treats an
  unreachable answer as "stay awake", never as "assume idle".

## The wire shape is MEASURED — treat this file as the map

Every route and field name here was checked against a live dstack **0.21.2**; the reference
is `docs/references/dstack-real-api.md`, and its §9 was measured end to end against a real
Kubernetes backend on kind. The shape squall had invented before that (`POST /apply`,
`GET|DELETE /runs/{name}`, 404/409 errors) matched nothing and is gone.

What actually matters when you touch this package:

- **Every call is a POST**, under `/api/project/{project}/runs/{apply,get,get_plan,delete,stop}`
  — except `list`, which is on the **root** router at `/api/runs/list`. The project-scoped
  path answers 405.
- **Apply is two steps**: `get_plan` then `apply`. Echo back the server's normalised
  `run_spec` verbatim (that is why `runPlanWire` keeps it as `json.RawMessage`) — not your
  own re-serialisation, which the CAS would reject.
- **The CAS anchor is the whole previous `Run` object**, not an integer. That is why
  `Action.Current` is a `*dstack.Run`.
- **`jobs` accumulates across deployments** (D46). A terminated replica from a previous
  `deployment_num` stays in the list forever with `probes: []`. Any readiness derivation MUST
  filter on the run's current `deployment_num` and skip finished submissions, or readiness is
  unreachable for the life of the run. `probes.go` is the only place that logic belongs.
- **A dead run does NOT read back as `ErrNotFound`** (D52). `get` returns HTTP 200 with
  `status: "terminated"`. `Run.IsTerminal()` is the discriminator, and the controller's
  `observe()` folds a terminal run to a nil `Observed.Run` so "dead is not asleep" keeps
  working. Applying over that name with `current_resource` omitted mints a fresh run: new id,
  `deployment_num` back to 0, clean `jobs`.

## The fake (`mock/`)

Reproduces five source-verified behaviours: F17 (in-place replica flip), F18 (apply CAS, and
force refused unconditionally), F20 (run id survives flips but not terminal states — dead ≠
asleep), F21 (runs land on fleets; the instance is released by fleet `idle_duration`, not by
the flip), F23 (gateway answers immediately: 503 / 404 / 403, and never wakes a service).

- Two call surfaces, one state machine: every exported method is directly callable, and
  `Handler()` mounts the same `Server` behind `net/http`. There is no second implementation
  to drift.
- **Auth is enforced on `/runs*` as well as `/gateway/{name}`**, and the token check runs
  **before any existence lookup** so a bad token cannot leak whether a run exists. The token
  is exported as `mock.ValidToken` for out-of-package tests.
- Timing goes through `internal/clock`. `Tick()` polls elapsed time rather than using real
  timers — the right shape for a level-triggered reconciler.

Deliberately not modelled: offer selection, pricing, provisioning latency, SSH tunnels.

## Before you claim something is covered

Mutate the implementation to break it and watch a test go red. Phase 5's review found the
entire suite stayed green when all three `c.authorize(...)` calls were removed. See
`docs/references/testing-discipline.md`.
