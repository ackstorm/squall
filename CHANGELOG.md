# Changelog

All notable changes to Squall are recorded here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Squall is pre-1.0: the `Model` CRD's `v1alpha1` API may change between minor
versions, and a change that would be breaking after 1.0 is only a minor bump
here. Anything that can cost money or terminate a running generation is called
out explicitly, because that is the class of change worth reading twice.

## [Unreleased]

Nothing yet.

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

[Unreleased]: https://github.com/ackstorm/squall/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/ackstorm/squall/releases/tag/v0.1.0
