# Changelog

All notable changes to Squall are recorded here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Squall is pre-1.0: the `Model` CRD's `v1alpha1` API may change between minor
versions, and a change that would be breaking after 1.0 is only a minor bump
here. Anything that can cost money or terminate a running generation is called
out explicitly, because that is the class of change worth reading twice.

## [0.1.7] — 2026-09-05

### BREAKING

- `spec.fleet` is removed and `spec.scaleDownDelaySeconds` is renamed to
  `spec.idleTimeout`, now written as a duration (`5m`, not `300`).

  **Upgrade order — apply the CRD FIRST, then rewrite your Models:**

  ```sh
  # 1. Helm NEVER upgrades CRDs from crds/ (it installs them once, on
  #    install), so the new schema has to be applied by hand — a chart
  #    upgrade alone reports success and changes nothing. Ledger D160.
  kubectl apply --server-side -f deploy/helm/squall/crds/squall.ackstorm.ai_models.yaml
  # 2. Only now can a Model without spec.fleet be accepted: the 0.1.6 CRD
  #    lists `fleet` in its required fields, so rewriting first fails.
  kubectl apply -f your-models.yaml
  ```

  **Then rewrite every Model.** Kubernetes prunes unknown fields, so an
  unmodified manifest applies without error and silently takes
  `idleTimeout`'s 5m default — a Model that had
  `scaleDownDelaySeconds: 600` goes from a 10-minute idle window to a
  5-minute one with no warning.
- Squall now always asks dstack for `idle_duration: 0`. There is no warm
  pool on any backend, so every wake is a full cold start and `holdTimeout`
  must cover one.
- `spec.idleTimeout` and `spec.provisioningTimeout` are now rejected at
  zero. `idleTimeout` doubles as the demand annotation's TTL, so a zero
  expires demand the instant squall-proxy writes it and the Model can never
  wake; `provisioningTimeout` is the only bound on a run that never reaches
  Ready, and a non-positive value silently disables it.

  **Picking a value: the floor is not zero.** Because `idleTimeout` is also
  the demand TTL, any value shorter than the controller's idle requeue
  interval (`SQUALL_IDLE_REQUEUE_INTERVAL`, 15s by default) leaves a Model
  unwakeable for the same reason a zero does — the demand stamp expires
  before the controller next evaluates the Model, and the request 503s along
  with every retry. Validation rejects only the unambiguous zero. Keep
  `idleTimeout` comfortably above the requeue interval; the `5m` default is
  20x it. README's "Choosing `idleTimeout`" covers the cost/latency trade and
  the measurements behind this (ledger D171).

### Changed

- The validation warning about a warm window that a backend cannot honour
  is replaced by one on `holdTimeout < provisioningTimeout / 2`. With no
  warm pool anywhere, there is no per-backend warm window left to warn
  about — the question is now only whether a hold is long enough to cover
  a full cold start, or will answer 503 to the request that triggered the
  wake. It is a heuristic, not a rule, and it names the values it used.

## [0.1.6] — 2026-09-04

### Fixed

- **The warm-window warning no longer counts `fleet.idleDuration` on backends that
  cannot hold a warm pool.** dstack only honours an idle window when its `dockerized`
  flag is true, which is false on **Vast.ai, Kubernetes and RunPod** (D158). The
  validation warning added them anyway, so a live Vast.ai Model was told its warm
  window was 15m when it was really 5m — and its demand anchor then expired **14
  minutes before** the GPU it was holding open finished provisioning. One
  non-dockerized backend in `placement.backends` is now enough to discount
  `idleDuration` entirely; the warning names which formula it used. Ledger D158.
- **`NoCapacity` no longer retries flat out.** A Model the backend cannot satisfy
  minted a fresh dstack run every 15-20 seconds indefinitely — measured on Vast.ai,
  where every attempt is one the provider may charge for. Recreates are now paced by
  a 1-minute backoff after a failure dstack itself diagnosed as no capacity,
  insufficient credit, or rate limiting. A wake still **fails open**: the pacing never
  abandons the retry, and `hardStop`-fired runs are never paced. Ledger D163.
- The `hardStop is disabled` warning no longer implies that enabling it gives you a
  dead-man's switch — on the Kubernetes backend it does not fire at all (D161).

- **Corrected a claim made for `spec.hardStop` in 0.1.5.** The release notes
  called it "the only bound that still holds when squall-controller is dead".
  A live end-to-end test has since seen a replica run **104 minutes against
  `max_duration: 3600`** without being terminated, with dstack's own runner
  API still reporting `state: running` and an empty `termination_reason` —
  on the Kubernetes backend, with the value confirmed present in the job spec
  and in the runner's submit body. **Do not rely on `hardStop` as a
  dead-man's switch until it has been observed to fire.** It stays wired and
  defaulted; only the claim changes. Ledger D161.
- Documented that `hardStop` bounds **one continuous job submission**, not a
  Model's cumulative awake time. Each sleep/wake cycle mints a new dstack job
  submission with a fresh clock, so a Model that flips regularly never
  approaches it. Correct for the runaway it targets, wrong as a spend cap.

### Documentation

- Corrected what `fleet.idleDuration` actually does per backend. dstack gates
  idle release on an internal `dockerized` flag: on **Vast.ai and Kubernetes**
  it is false, dstack forces the idle window to zero and never reads the
  value, so the field is inert there and the instance is released on the first
  pass after the job stops. The 3-day default this project calls "the single
  most expensive footgun" binds on VM backends only. The consequence worth
  knowing: **there is no warm pool on Vast** — every wake is a full cold
  provision, so `holdTimeout` has to cover one. Source-verified against the
  deployed 0.21.2, not measured. Ledger D158, spec F21b,
  `docs/references/dstack-real-api.md` §9.9. The CRD field description changed
  with it; the field stays required, because it does bind on VM backends and
  the chart cannot know which backend a fleet will draw from.

## [0.1.5] — 2026-09-03

The idle-capacity release. Every change here exists to answer one question:
can a GPU squall started keep running with nobody using it? Read the two
**Changed** entries before upgrading — one of them fails the chart render
until you edit your values, deliberately.

### Fixed

- **Money, the big one:** `spec.fleet.idleDuration` now actually reaches
  dstack, on the run configuration for every apply and on every fleet squall
  creates. Until now the field was required by the CRD and then dropped
  before the wire, so once a Model went `Asleep` the rented instance lived
  for whatever the fleet said — which for the chart's own fleets was dstack's
  default of **three days**. Flipping replicas to zero stops the job; only
  `idle_duration` stops the machine (F21).
- **Money:** the sleep flip now re-sends the durations the running dstack run
  was created with rather than the Model's current ones. dstack refuses an
  apply against an active run whose spec differs in anything but the replica
  count, and squall recovers from that refusal only on the wake path, so
  without this any run created before the fix above would have been refused
  on its way to sleep and stayed awake indefinitely.

### Added

- **`spec.hardStop`** (default `24h`), a dead-man's switch for on-demand
  Models. It is sent to dstack as the run's `max_duration` and enforced by
  the runner inside the container, so it is the only bound that still holds
  when squall-controller is dead — every other path to zero replicas runs
  inside that one process. Verified against dstack 0.21.2 to terminate the
  job without submitting a replacement. Pinned Models (`minReplicas: 1`) are
  unaffected: there a hard stop would be a scheduled outage, and `maxLifetime`
  remains the alert-only instrument for them. Must be at least `1h` and never
  shorter than the uncontrolled deadline; `0` disables it and warns.
  **A `hardStop` that fires is an incident** — it means nothing else acted for
  the whole window — and it alerts as `SquallModelHardStopFired`.
- **A ceiling on every proxied request** (`proxy.env.maxRequestDuration`,
  default `45m`). **This can terminate a generation**, which is why it is
  generous by default: set it above your longest `holdTimeout` plus a real
  generation. It exists because in-flight requests are what block
  scale-to-zero, so a request that never finished — a client that stops
  reading, an engine that hangs — kept a GPU awake with no other guardian
  able to see it.
- `squall_model_idle_seconds` and `squall_model_scale_down_delay_seconds`,
  plus three alerts: `SquallModelIdleButAwake` (live capacity idle past three
  times its own window, so the sleep guardian is not firing),
  `SquallControllerAbsent` (nothing has scraped the controller for 10m, which
  also means every other squall alert is silently evaluating to nothing) and
  `SquallModelHardStopFired`. All behind `prometheusRule.enabled`.

### Changed

- **`dstack.fleets[].idleDuration` is now REQUIRED and the chart render fails
  without it.** This is deliberate rather than defaulted: dstack's own default
  is three days, so a silently-omitted value is the most expensive mistake
  available here. Add it to every entry in `dstack.fleets` before upgrading.
- **`spec.uncontrolledTimeout: 0` no longer disables the deadline.** It is
  rejected on the wake path and read as "use the default" for a Model already
  awake, and explicit values are capped at `24h`. There is no longer any way
  to configure unbounded capacity that squall cannot observe.
- `controller.replicas` now defaults to `2`. Leader election was already on,
  so the standby actuates nothing until it holds the lease.

## [0.1.4] — 2026-09-02

### Added

- The chart now ships the Prometheus alert rules, behind
  `prometheusRule.enabled` (off by default; needs the Prometheus Operator
  CRDs). `spec.maxLifetime` is ALERT-ONLY by design — nothing in the
  controller stops a Model that never goes idle — so until now a chart-only
  install had no ceiling of any kind: the rules existed solely in
  `config/prometheus/rules.yaml`, which nothing installed.
- Three alerts for guardrails the controller deliberately does not enforce:
  `SquallModelProvisioningRetryLoop` (a wake that never lands is destroyed at
  `provisioningTimeout`, but demand re-arms the next one and each attempt
  rents a machine until the deadline), `SquallModelUncontrolledApproachingDeadline`
  (fires at half the deadline, so a proxy outage is visible before the
  uncontrolled flip acts on it) and `SquallModelCapacityWhileNotReady`
  (a run is active while the Model cannot serve — capacity that still bills).
- `make helm-sync` now syncs the rules into the chart, so the shipped alerts
  cannot drift from the ones `config/prometheus/rules.yaml` declares.

## [0.1.3] — 2026-09-02

### Added

- Added bounded lifecycle and provisioning metrics to the controller, request
  health metrics to the proxy, enabled bundled dstack metrics, and optional
  Prometheus Operator `ServiceMonitor` resources.

### Changed

- `dstack.adminToken` now uses Kubernetes' native `value`/`valueFrom` shape;
  existing scalar values must move to `dstack.adminToken.value`.
- Updated the bundled dstack server from 0.21.2 to 0.21.3.
- Reordered the Model's default printer columns to Backend, Engine, Run,
  Phase, Schedulable, Fleet, Age.
- Documented the operator/backend and Model placement policy boundary for
  Vast.ai, including canonical regions, Community Cloud and Secret-backed
  credentials.

### Fixed

- Terminal dstack provisioning failures now remain visible on the Model's
  `Provisioning` condition, including credit, capacity and rate-limit causes,
  until a replacement reaches Ready.

## [0.1.2] — 2026-09-02

### Changed

- The bundled Kubernetes backend now places dstack run pods and its SSH jump
  in namespace `squall` by default instead of `squall-runs`. Scale Models down
  before upgrading an existing installation because the old namespace is
  removed by Helm.

## [0.1.1] — 2026-09-02

### Added

- The chart exposes the controller metrics endpoint by default.
- Added measured deployment notes for Qwen3.8-27B and an unverified
  GLM-5.3-Flash four-GPU sample.

### Changed

- Removed unused Kubebuilder/Kustomize deployment scaffolding; Helm remains
  the deployment path.
- Reworked the README around the supported install and operating workflow.

### Fixed

- Fresh bundled-dstack installs now reuse `dstack.adminToken` for Squall's
  dstack clients, wire scale-to-zero to the chart-managed proxy Service, and
  discover the SSH jump NodePort host through the Downward API instead of
  requiring a node IP in values.
- Unschedulable requests still signal demand, allowing a changed Model to be
  reconsidered, while retaining the immediate response contract.
- A request held during wake now advances the idle anchor when it completes,
  preventing premature scale-down.
- `status.fleet` no longer invents an auto-fleet name when an existing fleet
  is the one admitting the backend.
- Corrected the quickstart namespace, public-artifact guidance, warm-window
  values, and Model resource schema example.

## [0.1.0] — 2026-09-01

First tagged release. Everything below is the initial implementation rather
than a change against a predecessor, so only the decisions a reader needs in
order to run this safely are listed.

### Added

- `Model` CRD (`squall.ackstorm.ai/v1alpha1`) — declarative GPU capacity that
  sleeps at zero replicas and wakes on the first request.
- `squall-controller` — the reconciliation half: the 0↔1 replica flip,
  drain-first teardown via finalizer, orphan reaping, status and conditions.
- `squall-proxy` — the per-request data path: forward when Ready, hold while
  waking, and answer the wait contract truthfully when the deadline expires.
- Direct replica access over an SSH tunnel, with fallback to dstack's service
  proxy when no tunnel is available.
- Unhealthy-replica teardown: a replica taking traffic and delivering no 2xx
  for `spec.health.unhealthyAfter`, across at least
  `spec.health.failureThreshold` consecutive failures, is scaled to zero and
  reported on the `Healthy` condition.
- Served-model verification (`ServedModelVerified`): the replica is asked what
  it actually serves, because a green probe proves an HTTP server answered,
  not which model answered it.
- `spec.provisioningTimeout` is enforced: a wake that never reaches Ready is
  destroyed, alarmed and marked `Dead` — not `Asleep`, because the run never
  became usable and the next attempt must mint a fresh one.
- `spec.uncontrolledTimeout` and the `squall_model_uncontrolled_seconds`
  metric: capacity that is up while squall cannot see whether it is idle is
  alarmed, and torn down at the deadline. Defaults to
  `min(4 x scaleDownDelaySeconds + 15m, 2h)`; an explicit value is not capped,
  and `0` opts out.
- `status.fleet` — one entry per declared backend, each reporting
  `Admitting` / `Created` / `Unfleeted` / `Unconfigured`, with a `Fleet`
  printer column. A dstack error publishes no mirror at all rather than a
  half-built one that would read as fact.

### Operational notes

- **A dstack run is named `<namespace>-<name>`.** A dstack run name is global
  to the dstack server while a `Model` is namespaced, so keying the run on the
  bare name let two namespaces drive one run. The name is recorded in
  `status.runName` rather than recomputed, so a future change to the naming
  cannot orphan runs that already exist.
- **Squall never sends dstack's `force` flag.** Enforced by construction:
  `dstack.ApplyRequest` has no such field.
- **`0→1` fails open, `1→0` fails safe.** A wrong wake costs money; a wrong
  sleep kills a generation. Every ambiguity is resolved in that direction.
- `fleet.idleDuration` is required and has no default: dstack's own default is
  three days, which is not a safe fallback for rented GPUs.

### Known limitations

- **`squall-proxy` performs no authentication.** Any client that can reach it
  can wake capacity and spend money. Deploy it on a trusted network only, and
  do not expose it publicly without putting an authenticating gateway in
  front. This is the constraint the rest of this list assumes.
- No admission control on the Ready path: once a Model is Ready the proxy
  forwards unlimited concurrency into whatever capacity exists. Per-stream
  throughput, not squall, is what a caller feels under load.
- The unhealthy teardown can flap: a Model broken by configuration rather than
  by machine re-wakes on the next request.
- `status.forwardModel` is only written when served-model verification runs,
  so a Model verified before upgrading keeps it empty until its run generation
  is replaced. Empty means "do not rewrite", which is the safe direction.

[Unreleased]: https://github.com/ackstorm/squall/compare/v0.1.6...HEAD
[0.1.6]: https://github.com/ackstorm/squall/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/ackstorm/squall/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/ackstorm/squall/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/ackstorm/squall/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/ackstorm/squall/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/ackstorm/squall/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/ackstorm/squall/releases/tag/v0.1.0
