# Squall

> **Status: pre-alpha, but it serves.** Verified end to end on 2026-08-28 against real
> Vast.ai GPUs: a `Model` in Git provisions a GPU on demand, wakes on the first request,
> serves OpenAI-compatible traffic, and releases the instance when idle. Measured at 256
> concurrent generations with zero failures.

Squall serves LLMs on GPU capacity that does not exist in the primary Kubernetes region.
Capacity is provisioned on demand from external providers through
[dstack](https://github.com/dstackai/dstack), wakes from zero on the first request, and
disappears again when idle. A model is declared the way this platform already declares
everything else — a `squall.ackstorm.ai/v1alpha1` `Model` custom resource in Git — and is
reached through [LiteLLM](https://github.com/BerriAI/litellm), which stays vanilla.
Kubernetes is involved in intent and coordination only; no Kubernetes runs in the serving
compute.

Two binaries:

- **`squall-controller`** — reconciliation only: the 0↔1 replica flip (wake on demand,
  sleep on idle), drain-first teardown via finalizer, orphan reconciliation, and status as
  the shared truth.
- **`squall-proxy`** — the data path: per request, forward when the model is Ready, or
  answer the wait honestly when it is not. Separate binary, separate failure domain.

## The data path, and why it is not dstack's

A request reaches a replica one of two ways. Both work; they are not equivalent.

**Direct over SSH (preferred).** `squall-proxy` opens its own SSH connection to the replica
and forwards HTTP inside it. dstack is not in the request path at all.

**Through dstack's service proxy (fallback).** Always available, works on every backend
topology, and used automatically whenever the direct path is unavailable.

The difference is not cosmetic. Measured 2026-08-28 on one RTXPRO6000WS, same prompt and
concurrency, minutes apart:

| concurrency | via dstack | direct over SSH |
|---|---|---|
| 32 | 746 tok/s | **1010 tok/s** |
| 128 | 97/128 ok, 31× HTTP 500, 407 tok/s | **128/128 ok, 1857 tok/s** |
| 256 | not attempted | **256/256 ok, 2106 tok/s** |

dstack's proxy exposes its repository and auth as FastAPI `yield` dependencies torn down
only after the response completes, so **every streamed generation pins two database
connections for its entire lifetime**. Its connection pool — not the GPU — is the ceiling.
The 31 failures above coincided exactly with `pg_stat_activity` reaching
`poolSize + maxOverflow + 1`.

### How the direct path authenticates

Squall never touches dstack's project private key. dstack's Vast.ai backend builds the
container's `authorized_keys` from **both** `run_spec.ssh_key_pub` and its own project key
(`core/backends/vastai/compute.py`), and squall is what builds the run spec. So:

1. `squall-controller` mints an ed25519 keypair into the Secret `squall-replica-ssh-key`.
2. It sends the public half on every Apply.
3. `squall-proxy` reads the private half and dials the replica.
4. Host keys are pinned trust-on-first-use; a changed key is refused.

**`authorized_keys` is written at container creation.** A replica provisioned before the
key existed will never accept it — the tunnel is refused and traffic falls back. It starts
working on the next wake, with no intervention. The same applies to key rotation: it costs
a re-provision.

Everything about the direct path fails soft. No published endpoint, a topology needing more
than one SSH hop (Kubernetes' jump pod, `dockerized` backends), a missing key, a refused
dial — each falls through to dstack's proxy. Enabling it cannot fail a request that worked
before.

### Verifying which path a request took

```bash
kubectl -n squall get model <name> -o jsonpath='{.status.replica}'   # empty = fallback
kubectl -n squall-system logs -l app.kubernetes.io/component=proxy | grep 'ssh tunnel'
```

`ssh tunnel to replica established` means direct. `ssh tunnel to replica unavailable, using
dstack proxy` names the reason and is not an error.

## Serving limits worth knowing before you load-test

**dstack's upstream timeout applies only to the fallback path.** Its default is 60s, copied
from nginx, and it bounds the wait for the upstream's *response headers* — which for a
non-streaming completion means the whole generation plus anything queued ahead of it.
Measured: `max_tokens: 3000` returned 200 at 59.9s, `4000` returned
`504 {"detail":"Timed out requesting upstream"}` at 60.02s. The chart sets
`dstack.serviceClientTimeoutSeconds: 600`. It **cannot** be set per request — dstack reads
it once at import into a module-level constant shared by every service.

Streaming was never affected: the timeout is applied per read, not per request.

**Reasoning models can spend your entire output budget thinking.** Measured on
Qwen3.8-27B with `max_tokens: 1500`:

| `reasoning_effort` | completion | reasoning | **answer** |
|---|---|---|---|
| *(omitted)* | 1500 | 1500 | **0** |
| `low` | 924 | 327 | 597 |
| `medium` | 931 | 340 | 591 |
| `xhigh` | 1500 | 1500 | **0** |

Omitting the field is identical to `xhigh`, and at `xhigh` the model emitted **zero** answer
tokens — not truncated output, no output. Send `reasoning_effort: "medium"` explicitly. It
is a per-request field and needs no server change. Valid values are `xhigh | medium | low`;
`high` is not one of them.

## Two deployment traps that will cost you an hour

Both cost exactly that on 2026-08-28. Neither produces an error message.

**Helm does not upgrade CRDs.** Files under a chart's `crds/` directory are applied on
install and **never** on upgrade. A new status field therefore does not exist in the
cluster's CRD, and the API server silently prunes it — the controller writes it, nothing
rejects the write, and the field is simply absent. `make helm-sync` keeps the chart's copy
correct, which helps a fresh install and nothing else. After changing the CRD:

```bash
kubectl apply --server-side --force-conflicts -f config/crd/bases/squall.ackstorm.ai_models.yaml
```

**A rebuilt image does not reach a running Pod.** With a mutable tag (`:e2e`) and
`imagePullPolicy: Never`, `kind load` replaces the image on the node but changes nothing for
a Pod already running, and `helm upgrade` only recreates Pods when something in the Pod spec
actually changed. Symptom: new code that appears to do nothing at all. Always:

```bash
kubectl -n squall-system rollout restart deploy/squall-controller-manager deploy/proxy
```

Check what is actually running before diagnosing anything else — compare
`.status.startTime` against when the image was built.

## Design

The design of record lives in [`docs/specs/`](docs/specs/). Read it before changing
behaviour — it is the authority on the state machine, the wait contract, and the
provider boundary, and every implementation task references its section numbers. The
implementation plan is in [`docs/plans/`](docs/plans/).

## Building

**The only thing your host needs is Docker.** No Go toolchain, no kubebuilder, no
controller-gen, no kubectl. Squall targets Go 1.26.4, and pinning that on every developer
machine is not a thing anyone should have to do — so the entire toolchain lives in a
container image built from [`Dockerfile.devtools`](Dockerfile.devtools).

Every toolchain command goes through the wrapper:

```sh
./scripts/dev.sh go version        # -> go1.26.4
./scripts/dev.sh make test-unit
./scripts/dev.sh bash              # interactive shell in the devtools container
```

The wrapper mounts the repo at `/workspace`, preserves your UID/GID, persists the Go
module and build caches under `.gocache/`, and rebuilds the image whenever
`Dockerfile.devtools` changes. The first run takes a few minutes; every run after that is
immediate.

`make` targets that need the toolchain re-enter the container by themselves, so
`make test-unit` and `./scripts/dev.sh make test-unit` are equivalent. Host-only targets
(`make doctor`, `make clean`) deliberately do not.

Start here:

```sh
make help      # every target, self-documenting
make doctor    # preflight: docker, image, caches, in-container tools. No network.
```

### Linux only, by design

`scripts/dev.sh` requires the default Docker socket at `/var/run/docker.sock` and runs the
container with `--network=host`. `DOCKER_HOST=tcp://…`, rootless Docker and Docker Desktop
sockets are unsupported, and the wrapper fails fast with a clear message rather than
letting a downstream `kind` call die cryptically several minutes later.

## Tests

Three phases, in ascending cost:

| Target | What it runs |
|---|---|
| `make test-unit` | Pure logic. No envtest, no cluster. |
| `make test-envtest` | Controller against a real API server, race-enabled. The CI gate. |
| `make test-envtest-fast` | Same without `-race`, for the inner loop. Not a CI gate. |
| `make test-full` | `test-unit` + `test-envtest`. |

The envtest Kubernetes assets are pre-baked into the devtools image, so envtest never hits
the network.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and the
[Code of Conduct](CODE_OF_CONDUCT.md). Security reports:
[SECURITY.md](SECURITY.md).

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
