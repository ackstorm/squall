# Toolchain and traps

Everything here was discovered the expensive way. Read before changing build, CI or test
plumbing.

## The containerized toolchain — four load-bearing properties

1. **Linux-only, by explicit decision.** `scripts/dev.sh` requires the default
   `/var/run/docker.sock`, uses `--network=host`, and uses GNU `stat -c`. Docker Desktop,
   rootless Docker and `DOCKER_HOST=tcp://…` are unsupported and rejected with a clear
   message. Confirmed by the project owner — do not "fix" it for macOS without asking.
2. **BuildKit is mandatory.** `Dockerfile.devtools` uses `RUN --mount=type=cache`. With
   `DOCKER_BUILDKIT=0` that is a hard parse error, not a degraded build. Never disable it,
   in CI or locally.
3. **The image rebuilds itself when `Dockerfile.devtools` changes.** `dev.sh` stamps a
   `squall.dockerfile.sha` label and compares it against `sha256sum Dockerfile.devtools`.
   Editing the Dockerfile means the next `dev.sh` call rebuilds — expect minutes. Deliberate:
   it prevents the "works on my machine" failure where an image silently predates the
   Dockerfile.
4. **`GOTOOLCHAIN=local`**, so `go.mod`'s directive can never exceed the image's Go. To bump
   Go: **image first, rebuild, then `go.mod`.** The other order breaks every build instantly.

The first `dev.sh` call on a fresh checkout builds the `squall-devtools` image and
repopulates `.gocache/` (module cache, build cache, envtest assets). Slow once, fast after.
Confirm the baseline before writing anything:

```bash
./scripts/dev.sh go version          # go1.26.6 linux/amd64
./scripts/dev.sh make test-unit      # green
./scripts/dev.sh make test-envtest   # green
./scripts/dev.sh make qa-lint        # green
```

## Traps

**The lint target is `qa-lint`, not `lint`.** `make help` is authoritative; do not invent
target names.

**`kubebuilder init` refuses a non-empty directory.** Phase 1 had already populated the tree.
Solved by scaffolding into a container-local scratch dir and copying only non-conflicting
output. Side effect that bit us: kubebuilder derived the project name from the *scratch
directory's basename*, leaking `squall-init-system` into `config/` and `test/e2e/`. It was
corrected — but if you scaffold anything else this way, check the derived names.

**`controller-gen` with bare `paths=./...` is broken here.** `.gocache/gopath/pkg/mod` lives
*inside* the repo tree by design, and controller-gen walks into a golangci-lint plugin
dependency whose `go.work` breaks its package loader. `gen-code` / `gen-manifests` are scoped
to `./api/... ./cmd/... ./internal/...` via `CONTROLLER_GEN_PATHS`. Do not "simplify" that
back.

**`-X` ldflags fail silently against the wrong symbol path.** A `-X` pointing at a
non-existent symbol is a no-op with no error. `ach` uses `github.com/…/cmd/proxy.Version`
because its `Version` lives in a subpackage; ours lives in `package main`, so the target is
`main.Version`. If you add version stamping anywhere, verify with a real value end to end.

**`setup-envtest` was pinned to `release-0.19`, which is a git branch, not a tag.**
`go install @release-0.19` resolves to the branch tip *at build time* — two developers
building weeks apart get different binaries with nothing changed in git, and the layer cache
hides it. Now pinned to a commit SHA. There are no semver tags for that line. **The sibling
project `../alitellm-operator` still has this hole** — worth telling whoever owns it.

**`kustomize version` prints `(devel)`.** Inherent to `go install`; the binary is correct.
Never assert on that string.

**A bare `go test ./internal/controller/...` needs `KUBEBUILDER_ASSETS`**, which `make` sets
internally. Use the make targets for anything touching envtest.

**Naked polling loops are banned.** Every wait needs an upper bound **and** an explicit
failure path. When the polled target disappears (container removed, process exited) the
predicate can never become true and the agent hangs forever.

## Reference projects

Two sibling checkouts. **Neither is a dependency** — they are sources to port *from*. Read
the relevant file before writing ours; their comments explain past breakages.

- **`../alitellm-operator`** — kubebuilder v4 multigroup operator. Origin of our Makefile,
  `dev.sh`, `Dockerfile.devtools`, CI, and the house test conventions. Also the source of the
  `LiteLLMModelDiscovery` seam Squall plugs into (Phase 12).
- **`../ach`** — a larger ACKstorm operator that, unlike the other, **builds two binaries**.
  Its Makefile build section and its explicit per-directory `COPY` allow-list in `Dockerfile`
  are the models to follow.

House conventions, both projects:

- Unit + envtest use plain `testing` with `TestMain`. **Ginkgo is used only for e2e.**
- Every `.go` file starts with `// SPDX-License-Identifier: MIT`.
- In-memory stateful mocks as a Go package (`internal/<domain>/mock`).

## The e2e cluster will happily test code that is not in your tree

The four e2e images are tagged `:e2e` — a mutable tag. Rebuild them, `kind load` them,
`kubectl apply` the overlays, and **nothing restarts**: the Deployment spec is byte-identical,
so apply is a no-op and the pods keep running the previous binary. `make e2e-full` then
reports a verdict on code from hours ago. This has cost real time twice (ledger D40).

The guard is content-addressed and automatic:

- `hack/cluster.sh` stamps every e2e Deployment's pod template with
  `squall.ackstorm.ai/build-stamp` — a sha1 over `cmd/`, `internal/`, `api/` `*.go` plus
  `go.mod`/`go.sum`. Changing that annotation is what triggers the rollout, so a restart
  happens exactly when the code changed and never when it did not.
- `hack/cluster.sh check-fresh` compares the running stamp against the tree.
- `make e2e-run` runs `check-fresh` first and **refuses to start** on stale pods, naming the
  fix (`make cluster-hydrate`).

So: `make e2e-full` is always safe, and `make e2e-run` can no longer lie. If you ever see
`runs build '<none>'`, the pods predate the stamping — hydrate once and it clears.

## Deploying a change to the kind cluster — the two silent failures

Both of these cost an hour on 2026-08-28, stacked on top of each other, and NEITHER produces
an error message. When new code appears to do nothing at all, check these before reading the
code (ledger D88, D89).

### Helm does not upgrade CRDs

Files under a chart's `crds/` directory are applied on **install** and never on upgrade.
That is documented Helm behaviour, not a bug in this chart.

Consequence: a field added to `ModelStatus` does not exist in the cluster's CRD, and the API
server **silently prunes it**. The controller writes it, the write succeeds, and the field is
absent afterwards. No event, no warning, no rejected write.

`make helm-sync` regenerates `deploy/helm/squall/crds/` and is what makes a FRESH install
correct. It does nothing for a cluster that already has the CRD.

```bash
kubectl apply --server-side --force-conflicts \
  -f config/crd/bases/squall.ackstorm.ai_models.yaml
```

Verify it landed, rather than assuming:

```bash
kubectl get crd models.squall.ackstorm.ai -o json | \
  jq '.spec.versions[0].schema.openAPIV3Schema.properties.status.properties | keys'
```

### A rebuilt image does not reach a running Pod

`kind load docker-image squall-proxy:e2e` replaces the image **on the node**. A Pod that is
already running keeps the image it started with, and because the tag is mutable (`:e2e`) with
`imagePullPolicy: Never`, nothing about it looks stale. `helm upgrade` only recreates Pods
when something in the Pod spec actually changes — an upgrade that touches only another
Deployment's env leaves these Pods untouched.

```bash
kubectl -n squall-system rollout restart deploy/squall-operator deploy/squall-proxy
```

Before diagnosing any behaviour, confirm what is actually running:

```bash
kubectl -n squall-system get pods -o custom-columns=\
'NAME:.metadata.name,START:.status.startTime,IMAGE:.spec.containers[0].image'
```

A `startTime` earlier than the image build is the whole answer. This is the third time in
this project that a fix landed somewhere the running workload does not read — the other two
were `EXTRA_HELM_VALUES` and `demandRefreshInterval`. The rule that keeps falling out of it:
**verify the running object, not the file you edited.**

### `go test ./...` is not the envtest gate

Running `go test ./...` directly can report a missing `/usr/local/kubebuilder/bin/etcd` even
though the repository's envtest assets are present. The assets are pre-baked under
`/opt/envtest/`; use `./scripts/dev.sh make test-envtest`, which injects
`KUBEBUILDER_ASSETS` after preparing the assets. Treat the direct command's missing-binary
message as a toolchain trap, not as evidence that envtest is unavailable.
