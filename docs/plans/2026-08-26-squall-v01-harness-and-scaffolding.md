# Squall v0.1 — Harness and Scaffolding Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Stand up the Squall repository — containerized toolchain, kubebuilder scaffold, `Model` CRD, a fake dstack server, and the controller/proxy skeletons — so that every mechanic in spec §5–§7 becomes testable in `envtest` without spending a euro on GPUs.

**Architecture:** Two Go binaries (`squall-controller`, `squall-proxy`) built from one kubebuilder v4 multigroup module, forked from the proven `../alitellm-operator` harness (Makefile, devtools container, kind hydration, three-phase test pyramid). The load-bearing test asset is an **in-memory fake dstack server** that reproduces the five source-verified dstack behaviours the design rests on (F17, F18, F20, F21, F23), letting the 0↔1 flip, the CAS rules and the finalizer be driven deterministically in `envtest`.

**Tech Stack:** Go 1.26.4, controller-runtime v0.19.4, kubebuilder v4.4.0, controller-gen v0.16.5, kustomize v5.4.3, envtest (k8s 1.31.0), kind v0.25.0, Ginkgo v2 (e2e only), Prometheus client_golang.

**Spec of record:** `docs/specs/squall-spec-v0_18-RC.md`. Every `F<n>`, `§<n>`, `AC<n>` and `PoC <n>` reference below points into it. (v0.15 and v0.16 superseded 2026-08-26; both recoverable from git history.)

**Reference harness:** `/home/jcm/Projects/alitellm-operator` (a sibling checkout, not a dependency). When a task says "port from the sister project", read that file first — it is battle-tested and the conventions are house style.

---

## Conventions this plan assumes

Read once before Task 1. Every task below relies on these.

| Thing | Value | Why |
|---|---|---|
| Go module | `github.com/ackstorm/squall` | Mirrors `github.com/ackstorm/alitellm-operator` |
| Kubebuilder domain | `ackstorm.ai` | Same as sister; yields `squall.ackstorm.ai` |
| API group | `squall` | Spec §5.1: `squall.ackstorm.ai/v1alpha1` |
| Layout | `multigroup: true` | Puts types at `api/squall/v1alpha1/`, matching the sister |
| Binaries | `cmd/controller`, `cmd/proxy` | Spec §11: separate Deployments, separate failure domains |
| Fake dstack | `internal/dstack/mock` | Mirrors `internal/litellm/mock` in the sister |
| dstack client | `internal/dstack` | |
| Unit + envtest | plain `testing` + `TestMain` | House style — see `internal/controller/suite_test.go` |
| E2E only | Ginkgo v2 | House style — see `test/e2e/suite_test.go` |
| Every `.go` file starts with | `// SPDX-License-Identifier: Apache-2.0` | Enforced by the sister's `pre-push` gate; port it |

**The host has Go 1.24.4; this project needs 1.26.4.** Every `go`, `kubebuilder`, `controller-gen` and `make` invocation that touches the toolchain runs inside the devtools container via `./scripts/dev.sh`. Phase 1 builds that container. Until it exists, nothing else in this plan can run.

**Commit after every task.** Conventional commits, imperative subject under 72 chars (`ackstorm-git-guidelines`).

**Execution model (decided 2026-08-26):**

| Role | Who | Why |
|---|---|---|
| Planning, spec review, judging findings | **Opus** (main session) | The decisions that are expensive to get wrong |
| Implementation | **Sonnet** subagent, one per phase | These tasks are mechanical ports and TDD loops; pace matters more than depth |
| Review | **Sonnet** subagent, **once per phase** | Two passes: spec compliance, then code quality |

Phase loop: **implement → review → fix → tests green → close.** One implementer works the whole phase task-by-task, committing as it goes. Per-task review is too slow for a phase of mechanical tasks.

Tests are **not** batched: every task's own verification step is non-negotiable, and a phase never closes with a red test. It is the review round-trips that were eating the clock, not the tests.

---

## Gate: ALL FOUR SPEC DECISIONS ARE CLOSED

As of spec **v0.17-RC** (2026-08-26) every decision this plan was gated on is resolved in the spec itself. **No phase is blocked.** Build to the spec text; the "recommended defaults" this plan once carried are historical and must not be implemented in place of it.

| # | Was | Resolved in |
|---|---|---|
| **D1** | `spec.engine` / `spec.weights` vs §8's three-field claim | **v0.17 §5.1/§8** — `weights` **cut** (weights ride in `args`/`env` in each engine's own vocabulary; §9's origin doctrine governs how they are written). `engine` **kept**, acknowledged as the one legitimate per-*engine* element: it selects the health/warmup shape that feeds `Ready`. F34's claim narrowed to what it actually earned — **no per-*provider* anything**. |
| **D2** | `Draining` / `Dead` missing from §7's table | **v0.17 §7** — six phases, six rows. `Dead` → demand patch (recreate) + alarm if uncommanded, block, 503 `recreating` with full cold-start expectations. `Draining` → in-flight forwards until `drainTimeout`, **new requests never block**, 404 (leaving desired state). |
| **D3** | Idle aggregation across ≥2 proxy replicas | **v0.16 §6** — Endpoints enumeration requiring a complete, fresh answer from every replica. See below. |
| **D4** | Owner of LiteLLM's `fallbacks` chain | **v0.17 AC3** — fallback chains are `router_settings` (router-level; F30's per-deployment bag cannot carry them), owned by the LiteLLM Helm values in Git, **outside squall's v0.1 scope**. AC3 now asserts the 503 wait contract squall actually owns. |

### D3 as resolved in spec v0.16 §6 — implement exactly this

The spec chose a better mechanism than this plan's original per-replica-annotation default. **Discard the annotation/TTL design; it is superseded.**

Each proxy replica exposes per-model `inFlight` and `lastRequestAt` on an **internal HTTP endpoint**. The controller enumerates the proxy Service's **Endpoints** and requires a *complete, fresh answer from every replica*. Sleep fires only when all replicas report `inFlight == 0` and the newest `lastRequestAt` is older than `scaleDownDelaySeconds`. **Any replica unreachable, stale, or ambiguous → stay awake.**

This is better than annotations on three counts: no CR write amplification at request rate, no TTL semantics to get subtly wrong, and "unreachable means stay awake" falls out of the mechanism instead of being a rule bolted onto it.

It rests on an invariant worth memorising, because it decides every ambiguous case in Phases 7, 8 and 9:

> **Wake may tolerate uncertainty; sleep must not.** Paying for a GPU a little longer is always preferable to terminating an active generation.

The same evidence gates teardown's drain step (§5.2), and the companion directional rule from v0.16 §5.2 is: **`0→1` fails open, `1→0` fails safe.** A wrong wake costs money; a wrong sleep kills a generation. This supersedes the older "replica-count changes are non-destructive" reading — a `1→0` on a serving model *is* destructive.

---

# Phase 1 — Containerized toolchain

**Outcome:** `./scripts/dev.sh go version` prints `go1.26.4`, on a host that has 1.24.4.

### Task 1.1: Devtools image

**Files:**
- Create: `Dockerfile.devtools`

**Step 1: Port the sister's image**

Read `/home/jcm/Projects/alitellm-operator/Dockerfile.devtools` in full, then write ours with the **same pins** (they are what make the sister's envtest patterns port cleanly):

```dockerfile
# syntax=docker/dockerfile:1.7
#
# Devtools image — Go + kubebuilder + controller-gen + kustomize +
# setup-envtest + kind + helm + kubectl + docker CLI. The host runs no
# Go 1.26 toolchain; every `go`, `kubebuilder`, `make` invocation goes
# through scripts/dev.sh, which mounts the workspace and the host Docker
# socket into this image.

FROM golang:1.26.4-bookworm

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG KUBEBUILDER_VERSION=v4.4.0
ARG CONTROLLER_GEN_VERSION=v0.16.5
ARG KUSTOMIZE_VERSION=v5.4.3
ARG SETUP_ENVTEST_VERSION=release-0.19
ARG ENVTEST_K8S_VERSION=1.31.0
ARG KIND_VERSION=v0.25.0
ARG HELM_VERSION=v3.16.3
ARG GOVULNCHECK_VERSION=v1.3.0

ENV DEBIAN_FRONTEND=noninteractive \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=mod \
    GOBIN=/usr/local/bin

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl make git jq gnupg lsb-release \
        bash-completion docker.io docker-compose \
    && rm -rf /var/lib/apt/lists/*
```

Then port, verbatim in structure, the sister's install layers for: kubebuilder, kind, helm, kubectl, the three `go install` tools (controller-gen, kustomize, setup-envtest), govulncheck, and the pre-baked envtest assets under `/opt/envtest`. Finish with:

```dockerfile
ENV ENVTEST_K8S_VERSION=${ENVTEST_K8S_VERSION} \
    ENVTEST_BIN_DIR=/opt/envtest

WORKDIR /workspace
CMD ["bash"]
```

**Step 2: Commit**

```bash
git add Dockerfile.devtools
git commit -m "build: add devtools container image"
```

---

### Task 1.2: dev.sh wrapper

**Files:**
- Create: `scripts/dev.sh` (mode 0755)

**Step 1: Port with squall naming**

Read `/home/jcm/Projects/alitellm-operator/scripts/dev.sh`. Port it, applying exactly these changes:

- `IMAGE="${SQUALL_DEVTOOLS_IMAGE:-squall-devtools:latest}"`
- `LITELLM_IN_DEVTOOLS` → `SQUALL_IN_DEVTOOLS` (every occurrence, including the `-e` flag)
- `LITELLM_DEVTOOLS_REBUILD` → `SQUALL_DEVTOOLS_REBUILD`
- **Delete** the three LiteLLM-spike env forwards (`LITELLM_SPIKE_URL`, `LITELLM_SPIKE_MASTER_KEY`, `LITELLM_VERSION`)

**Keep unchanged** — each of these exists because something broke without it, and the comments say what:
- the `.gocache/{gopath,build,envtest,kube}` pre-creation (docker would `mkdir` them as root)
- `--user "$(id -u):$(id -g)"` + `--group-add` for the docker GID
- the git-worktree `.git`-is-a-file detection and second bind mount
- **not** overriding `ENVTEST_BIN_DIR`, so the pre-baked assets are used
- the `-t 0 && -t 1` TTY guard, so CI callers work

**Step 2: Verify the whole toolchain**

```bash
chmod +x scripts/dev.sh
./scripts/dev.sh go version
```
Expected: first run builds the image (several minutes), then `go version go1.26.4 linux/amd64`.

```bash
./scripts/dev.sh bash -c 'kubebuilder version && controller-gen --version && kustomize version && kind version && helm version --short'
```
Expected: all five print versions, none "command not found".

**Step 3: Commit**

```bash
git add scripts/dev.sh
git commit -m "build: add dev.sh devtools wrapper"
```

---

### Task 1.3: Makefile skeleton

**Files:**
- Create: `Makefile`

**Step 1: Port the frame, not the contents**

Read `/home/jcm/Projects/alitellm-operator/Makefile` sections `##@ General`, `##@ Diagnostics`, `##@ Development`, `##@ Test`, `##@ Dependencies`. Port:

- the `IMG` / `GOBIN` / `CONTAINER_TOOL` preamble
- **the `container_target` macro** — this is the piece that makes everything else work. Rename `LITELLM_IN_DEVTOOLS` → `SQUALL_IN_DEVTOOLS`. Keep the `$(MAKEOVERRIDES)` forwarding and the single-quoting; the comment explains that `FOCUS='TestA|TestB'` breaks without it.
- `SHELL = /usr/bin/env bash -o pipefail` and `.SHELLFLAGS = -ec`
- the `help` target's awk (it is what makes `##@`/`##` self-documenting)
- `doctor`, adapted to squall

**Port every `##@` section header now, even where the targets are stubs.** CI files reference target names, and renaming a target later means editing workflow YAML in lockstep. The names are the interface; establish them once.

Add these targets now, each `##`-documented:

| Target | Does |
|---|---|
| `gen-code` | `controller-gen object:headerFile=hack/boilerplate.go.txt paths=./...` |
| `gen-manifests` | `controller-gen rbac:roleName=manager-role crd webhook paths=./... output:crd:artifacts:config=config/crd/bases` |
| `fmt` / `vet` / `qa-fmt-check` | `go fmt ./...` / `go vet ./...` / fail if not gofmt-clean |
| `qa-lint` | `golangci-lint run` |
| `test-unit` | Phase 1 — `go test ./... -short`, no envtest, no cluster, ~10s warm |
| `test-envtest` | Phase 2 — race-enabled, the CI gate |
| `test-envtest-fast` | Phase 2 without `-race` — dev inner loop, ~3× faster, **not** a CI gate |
| `test-full` | `test-unit` + `test-envtest` |
| `build-operator` | builds **both** binaries into `bin/` |

**Define `ENVTEST_ASSET_DIR` from `ENVTEST_BIN_DIR`.** `scripts/dev.sh` carries a comment saying it deliberately does not override `ENVTEST_BIN_DIR` so that `make` uses the image's pre-baked assets via `ENVTEST_ASSET_DIR`. That variable does not exist yet, so the comment is currently aspirational. Assets live at `/opt/envtest/k8s/1.31.0-linux-amd64`.

**Stub these sections with their real names**, each holding a single target that echoes "not yet implemented — see plan Phase N" and exits 0:
`##@ Security` (`qa-security`), `##@ Packaging & Sync` (`helm-sync`, `helm-sync-check`), `##@ Cluster (e2e infra)` (`cluster-up`, `cluster-down`, `cluster-hydrate`), `##@ Waiters`, `##@ E2E` (`e2e-run`, `e2e-full`).

Port the sister's `##@ Waiters` header comment verbatim — *"use these; never write ad-hoc until/while loops"*. That rule is in the global CLAUDE.md too (naked polling loops are banned; every wait needs an upper bound and a failure path).

**Step 2: Verify**

```bash
make help
```
Expected: categories print, every target has a description.

**Step 3: Commit**

```bash
git add Makefile
git commit -m "build: add Makefile with devtools routing and test phases"
```

---

### Task 1.4: Lint and boilerplate config

**Files:**
- Create: `hack/boilerplate.go.txt`, `.golangci.yml`, `.gitleaks.toml`

**Step 1:** Copy all three from the sister project unchanged except the module path in `.golangci.yml`.

`hack/boilerplate.go.txt` is one line:
```
// SPDX-License-Identifier: Apache-2.0
```

**Step 2:** Fix the stale comment in `.gitignore` — it labels the cache dir `(hack/dev.sh)`; the wrapper lives at `scripts/dev.sh`.

**Step 3: Commit**

```bash
git add hack/boilerplate.go.txt .golangci.yml .gitleaks.toml .gitignore
git commit -m "build: add lint, secret-scan and license boilerplate config"
```

---

### Task 1.5: CI pipeline

**Decision taken 2026-08-26: the repo is GitHub, public OSS** — same posture as the sister project. So the workflow files port nearly verbatim.

**Files:**
- Create: `.github/workflows/ci.yml`, `.github/workflows/devtools-image.yml`, `.github/actions/setup-devtools/action.yml`

**Step 1: Port `ci.yml`**

Read `/home/jcm/Projects/alitellm-operator/.github/workflows/ci.yml`. **The design principle to preserve above all: every CI step is `./scripts/dev.sh make <target>`.** CI never reimplements a local command, so CI and local cannot diverge. If a job needs something `make` cannot do, add the make target — do not inline it in YAML.

Port the jobs `lint`, `unit`, `envtest` (`needs: unit`), and keep `security` and `e2e` as jobs that currently invoke the stub targets. Keep: the `pull_request`-only trigger with `paths-ignore`, the `concurrency` group with `cancel-in-progress`, per-job `timeout-minutes`, coverage artifact upload, and the `if: "!github.event.pull_request.draft"` guard on e2e.

**Drop for now** (per the v0.1 release-scope decision): the chart/CRD drift step — it arrives in Phase 10 with the chart it guards.

**Step 2: Port the content-addressed devtools image**

`devtools-image.yml` + `setup-devtools/action.yml`, renaming `litellm-devtools` → `squall-devtools`. The mechanism: the image is tagged by `sha256(Dockerfile.devtools)` truncated to 12 chars and pushed to GHCR; `setup-devtools` pulls it and falls back to a local build when the hash is not published. This saves 2–3 minutes per job across every job in the PR — the cost we just paid locally.

Keep the `actions/cache` step for `.gocache/build` and `.gocache/gopath`, keyed on `hashFiles('**/go.sum', 'Dockerfile.devtools')`.

**Step 3: Verify what can be verified without a remote**

There is no git remote yet, so the workflows cannot run. Verify statically:

```bash
./scripts/dev.sh bash -c 'for f in .github/workflows/*.yml .github/actions/*/action.yml; do python3 -c "import sys,yaml;yaml.safe_load(open(sys.argv[1]))" "$f" && echo "OK $f"; done'
```

Then confirm by inspection that **every** `run:` line invoking make names a target that exists in the Makefile — mismatches here are the whole failure mode this task is guarding against:

```bash
grep -oE 'make [a-z0-9-]+' .github/workflows/*.yml | sort -u
make help
```

**Step 4: Commit**

```bash
git add .github
git commit -m "ci: add PR pipeline and content-addressed devtools image"
```

---

### Task 1.6: Open-source repository files

**Files:**
- Create: `LICENSE` (Apache-2.0), `NOTICE`, `README.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `CONTRIBUTING.md`, `.github/dependabot.yml`

**Step 1:** Port from the sister, adapting names and scope. The README states what Squall is in a paragraph, points at `docs/specs/`, and documents the one thing a newcomer must know: **the host needs only Docker; every toolchain command goes through `./scripts/dev.sh`.**

**Step 2: `docs/specs/` IS published — decided 2026-08-26.**

The spec ships with the repository. Scanned before the decision was recorded: no private IPv4, no email addresses, no AWS account IDs or ARNs, no internal hostnames, no credentials. Two consequences to hold onto:

- **The threat model becomes public** — accepted residual risks (host-key verification disabled on marketplace paths), the workload-eligibility table, and the network topology. This is deliberate and defensible: it is honest engineering writing, and it is the section a reader should judge us on.
- **The dstack findings become public** — including that the server prints its admin token to stdout on every start (F25). These are upstream-reportable defects in someone else's OSS project, not ours. §15 already lists the upstream PRs; file them before or with publication rather than after, so the public record shows the finding and the fix attempt together.

Every future spec revision inherits this decision, so the Phase 10 pre-push gate (secret scan, internal hostnames, ackstorm emails) must cover `docs/` — not just source.

**Step 3: Commit**

```bash
git add LICENSE NOTICE README.md SECURITY.md CODE_OF_CONDUCT.md CONTRIBUTING.md .github/dependabot.yml
git commit -m "docs: add open-source repository files"
```

---

# Phase 2 — Kubebuilder scaffold and two binaries

**Outcome:** `make build-operator` produces `bin/squall-controller` and `bin/squall-proxy`; `make test-unit` is green on an empty reconciler.

### Task 2.1: Init the project

**Step 1: Run kubebuilder**

```bash
./scripts/dev.sh kubebuilder init \
  --domain ackstorm.ai \
  --repo github.com/ackstorm/squall \
  --multigroup=true \
  --plugins go.kubebuilder.io/v4
```

**Step 2: Pin controller-runtime to the sister's version**

```bash
./scripts/dev.sh go get sigs.k8s.io/controller-runtime@v0.19.4
./scripts/dev.sh go get k8s.io/api@v0.31.0 k8s.io/apimachinery@v0.31.0 k8s.io/client-go@v0.31.0
./scripts/dev.sh go mod tidy
```

Matching the sister exactly is deliberate: its envtest helpers, finalizer patterns and conflict tests get copied in later phases and must compile unmodified.

**Step 3: Verify**

```bash
./scripts/dev.sh go build ./...
```
Expected: no output, exit 0.

**Step 4: Commit**

```bash
git add -A
git commit -m "feat: scaffold kubebuilder v4 multigroup project"
```

---

### Task 2.2: Create the Model API

**Step 1:**

```bash
./scripts/dev.sh kubebuilder create api \
  --group squall --version v1alpha1 --kind Model \
  --resource=true --controller=true
```

Expected new files: `api/squall/v1alpha1/model_types.go`, `internal/controller/squall/model_controller.go`.

**Step 2: Verify the group renders as the spec requires**

```bash
make gen-manifests
grep -E '^  (group|names|scope)' -A2 config/crd/bases/squall.ackstorm.ai_models.yaml | head
```
Expected: `group: squall.ackstorm.ai`, `kind: Model`, `scope: Namespaced`.

**Step 3: Commit**

```bash
git add -A
git commit -m "feat: add squall.ackstorm.ai/v1alpha1 Model API scaffold"
```

---

### Task 2.3: Split into two binaries

**Files:**
- Move: `cmd/main.go` → `cmd/controller/main.go`
- Create: `cmd/proxy/main.go`
- Modify: `Makefile` (`build-operator`)

**Step 1: Move the manager entrypoint**

```bash
mkdir -p cmd/controller cmd/proxy
git mv cmd/main.go cmd/controller/main.go
```

**Step 2: Write a proxy entrypoint that does nothing yet but serve health**

`cmd/proxy/main.go` — deliberately minimal; Phase 9 fills it in:

```go
// SPDX-License-Identifier: Apache-2.0

// Command squall-proxy is the Squall data path (spec §7): per request it
// forwards when the Model is Ready, blocks while it wakes, and answers the
// wait contract truthfully when its deadline expires. It is a separate
// binary from squall-controller by design (spec §11): separate failure
// domain, separate deploy cadence, stateless, >=2 replicas.
package main

import (
	"log/slog"
	"net/http"
	"os"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	addr := os.Getenv("SQUALL_PROXY_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	slog.Info("squall-proxy listening", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("listen failed", "err", err)
		os.Exit(1)
	}
}
```

**Step 3: Teach the Makefile about both**

```makefile
.PHONY: build-operator
build-operator: manifests generate fmt vet ## Build squall-controller and squall-proxy binaries.
	$(call container_target,_build-operator)

.PHONY: _build-operator
_build-operator:
	go build -o bin/squall-controller ./cmd/controller
	go build -o bin/squall-proxy ./cmd/proxy
```

**Step 4: Verify**

```bash
make build-operator && ls -1 bin/
```
Expected: both `squall-controller` and `squall-proxy` listed.

**Step 5: Commit**

```bash
git add -A
git commit -m "feat: split controller and proxy into separate binaries"
```

---

# Phase 3 — The Model CRD

> **Blocked on decision D1.** If unresolved, apply the recommended default: drop `weights`, keep `engine`.

**Outcome:** A CR matching spec §5.1's example applies cleanly; every §5.1 validation rule rejects what it should, proven in envtest.

### Task 3.1: Write the spec types

**Files:**
- Modify: `api/squall/v1alpha1/model_types.go`

**Step 1: Write the types**

Transcribe §5.1 exactly. `resources.gpu` is dstack's own `GPUSpec` **passed through** (F33) — do not invent a schema, and do not translate. `Memory` is a string range because that is dstack's native form (`"24GB..32GB"`).

```go
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelSpec is the desired state of one externally-served model.
// Field-for-field, spec v0.15 §5.1.
type ModelSpec struct {
	// Engine selects the readiness path, the warmup request shape and the
	// telemetry configuration. It is the ONLY per-engine artifact (spec §8);
	// it is never per-provider — one OCI image serves every backend (F34).
	// +kubebuilder:validation:Enum=vllm;llama-cpp;ollama
	Engine string `json:"engine"`

	// Image is a digest-pinned OCI reference. It MUST be userspace-only:
	// drivers come from dstack's AMI on AWS/DO and from the host on Vast
	// (F34), so an image bundling its own driver stack works on one path
	// and fails on the other.
	// +kubebuilder:validation:Pattern=`^[^@]+@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`

	// Args are ordered, engine-native arguments.
	// +optional
	Args []string `json:"args,omitempty"`

	// Env is an engine-native environment map.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	Resources  ResourceRequirements `json:"resources"`
	Placement  Placement            `json:"placement"`
	Fleet      FleetSpec            `json:"fleet"`

	// MinReplicas: 0 = on-demand 0<->1 flip; 1 = PINNED, never sleeps.
	// +kubebuilder:validation:Enum=0;1
	// +kubebuilder:default=0
	MinReplicas int32 `json:"minReplicas"`

	// HoldTimeout bounds the proxy's blocking wait (§7). 0 answers
	// immediately. MUST be <= ProvisioningTimeout.
	// +kubebuilder:default="20m"
	HoldTimeout metav1.Duration `json:"holdTimeout"`

	// ScaleDownDelaySeconds is the job-layer idle window: in-flight 0 and
	// last request older than this flips replicas to 0 (§6).
	// +kubebuilder:default=300
	ScaleDownDelaySeconds int32 `json:"scaleDownDelaySeconds"`

	// +kubebuilder:default="120s"
	DrainTimeout metav1.Duration `json:"drainTimeout"`

	// ProvisioningTimeout is the SINGLE age-based destructive trigger
	// (§5.2): a run that never reached ready is destroyed and alarmed.
	// +kubebuilder:default="45m"
	ProvisioningTimeout metav1.Duration `json:"provisioningTimeout"`

	// MaxLifetime is ALERT-ONLY and never destructive (§5.2). It is
	// exported as a declared/observed metric pair (§10) and is the safety
	// net for a pinned GPU everyone forgot.
	// +optional
	MaxLifetime *metav1.Duration `json:"maxLifetime,omitempty"`
}

// GPUSpec is dstack's own GPUSpec, passed through verbatim (F33).
// VRAM in GiB cannot express a bandwidth requirement; a card list can —
// an A10G and an RTX3090 are both 24 GB and differ ~50% in bandwidth,
// and decode is bandwidth-bound.
type GPUSpec struct {
	// +optional
	Vendor string `json:"vendor,omitempty"`
	// +optional
	Name []string `json:"name,omitempty"`
	// Memory is a dstack-native range, e.g. "24GB..32GB".
	// +optional
	Memory string `json:"memory,omitempty"`
	// +optional
	Count string `json:"count,omitempty"`
}

type ResourceRequirements struct {
	GPU GPUSpec `json:"gpu"`
}

type Placement struct {
	// Backends is the compliance allowlist (§12.3) — it ENFORCES the
	// workload-eligibility table, it does not merely document it.
	// +kubebuilder:validation:MinItems=1
	Backends []string `json:"backends"`
	// +optional
	Regions []string `json:"regions,omitempty"`
	// MaxPricePerHour is the cost control, enforced before provisioning.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	MaxPricePerHour string `json:"maxPricePerHour"`
}

type FleetSpec struct {
	// IdleDuration releases the MACHINE. Required, no default: dstack's
	// own default is 3 days (F21), which is the single most expensive
	// footgun in the system.
	IdleDuration metav1.Duration `json:"idleDuration"`
}
```

**Note the deliberate omissions:** no `scaling:`-style fields exist in this CRD at all (§5.1), no `budget`, no `routerClass`, no `suspend`, no `firstRequestPolicy` — all cut in v0.13 and each cut is recorded in Appendix C. Do not reintroduce them.

**Step 2: Write the status types**

```go
// ModelPhase is the shared truth between controller and proxy. Asleep and
// Dead are DIFFERENT states with different actions (F20, §5.2): asleep is
// registered + gateway 503 -> flip; dead is terminal + gateway 404 ->
// recreate + alarm.
// +kubebuilder:validation:Enum=Asleep;Waking;Ready;Draining;Recreating;Dead
type ModelPhase string

const (
	PhaseAsleep     ModelPhase = "Asleep"
	PhaseWaking     ModelPhase = "Waking"
	PhaseReady      ModelPhase = "Ready"
	PhaseDraining   ModelPhase = "Draining"
	PhaseRecreating ModelPhase = "Recreating"
	PhaseDead       ModelPhase = "Dead"
)

type ModelStatus struct {
	// +optional
	Phase ModelPhase `json:"phase,omitempty"`
	// RunID is a MUTABLE POINTER, not an identity: stable across flips
	// (F17), invalidated by terminal states (F20). Identity is the CR uid.
	// +optional
	RunID string `json:"runId,omitempty"`
	// DeploymentNum is the idempotency token within a run generation.
	// +optional
	DeploymentNum int64 `json:"deploymentNum,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

**Step 3: Add printer columns and generate**

```go
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Engine",type=string,JSONPath=`.spec.engine`
// +kubebuilder:printcolumn:name="Pinned",type=string,JSONPath=`.spec.minReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
```

```bash
make gen-code gen-manifests
```

**Step 4: Commit**

```bash
git add -A
git commit -m "feat: define Model CRD types per spec v0.15 section 5.1"
```

---

### Task 3.2: Write the failing validation tests

**Files:**
- Create: `internal/controller/squall/model_validation_test.go`

**Step 1: Write the tests first**

These encode §5.1's four validation rules. Every one must FAIL before Task 3.3 exists.

```go
// SPDX-License-Identifier: Apache-2.0

package squall

import "testing"

func TestValidate_RejectsHoldTimeoutAboveProvisioningTimeout(t *testing.T) {
	m := validModel()
	m.Spec.HoldTimeout = dur("60m")
	m.Spec.ProvisioningTimeout = dur("45m")

	if err := Validate(m); err == nil {
		t.Fatal("expected rejection: holdTimeout must be <= provisioningTimeout")
	}
}

func TestValidate_RejectsMissingFleetIdleDuration(t *testing.T) {
	m := validModel()
	m.Spec.Fleet.IdleDuration = dur("0s")

	if err := Validate(m); err == nil {
		t.Fatal("expected rejection: fleet.idleDuration is required (F21: dstack's default is 3 days)")
	}
}

func TestValidate_RejectsEmptyBackendAllowlist(t *testing.T) {
	m := validModel()
	m.Spec.Placement.Backends = nil

	if err := Validate(m); err == nil {
		t.Fatal("expected rejection: placement.backends enforces the eligibility table (§12.3)")
	}
}

// §5.1: a hold that habitually waits out a full cold start is a declared
// misconfiguration in all but intent. WARNING, never rejection.
func TestValidate_WarnsOnHoldTimeoutExceedingWarmWindow(t *testing.T) {
	m := validModel()
	m.Spec.HoldTimeout = dur("20m")
	m.Spec.ScaleDownDelaySeconds = 30
	m.Spec.Fleet.IdleDuration = dur("60s")

	warnings, err := ValidateWithWarnings(m)
	if err != nil {
		t.Fatalf("must not reject, only warn: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected a warm-window warning")
	}
}

func TestValidate_AcceptsTheSpecExample(t *testing.T) {
	if err := Validate(validModel()); err != nil {
		t.Fatalf("the §5.1 example must validate: %v", err)
	}
}
```

Write `validModel()` and `dur()` helpers returning the §5.1 example verbatim.

**Step 2: Run them, confirm they fail**

```bash
make test-unit
```
Expected: FAIL — `undefined: Validate`.

**Step 3: Commit the red tests**

```bash
git add internal/controller/squall/model_validation_test.go
git commit -m "test: add failing Model validation tests per spec section 5.1"
```

---

### Task 3.3: Make the validation tests pass

**Files:**
- Create: `internal/controller/squall/model_validation.go`

**Step 1:** Implement `Validate` and `ValidateWithWarnings` — the minimum that turns the five tests green. Structural rules (`MinItems`, `Enum`, the digest `Pattern`) are already enforced by the CRD from Task 3.1; this function covers only the cross-field rules the OpenAPI schema cannot express.

**Step 2: Run**

```bash
make test-unit
```
Expected: PASS, 5 tests.

**Step 3: Commit**

```bash
git add internal/controller/squall/model_validation.go
git commit -m "feat: enforce cross-field Model validation rules"
```

---

### Task 3.4: Sample manifest + envtest apply

**Files:**
- Create: `config/samples/squall_v1alpha1_model.yaml`
- Create: `internal/controller/squall/suite_test.go`

**Step 1:** Write the sample as the §5.1 example, verbatim.

**Step 2:** Port `TestMain`-based envtest bootstrap from the sister's `internal/controller/suite_test.go` — take only the `testEnv` / `cfg` / `k8sClient` setup and the scheme registration; leave every LiteLLM-specific global behind.

**Step 3:** Write one envtest that applies the sample and reads it back, asserting the defaults from Task 3.1 materialised (`minReplicas: 0`, `holdTimeout: 20m`, `provisioningTimeout: 45m`).

**Step 4: Run**

```bash
make test-envtest
```
Expected: PASS.

**Step 5: Commit**

```bash
git add -A
git commit -m "test: add envtest bootstrap and Model sample apply test"
```

---

# Phase 4 — The fake dstack server

**This is the highest-value asset in the repository.** Five source-verified dstack behaviours are what the whole design rests on; a fake that reproduces them makes §5.2, §6 and §7 testable deterministically, in seconds, for free. Without it, every one of those mechanics needs a GPU and a credit card.

**Outcome:** `internal/dstack/mock` reproduces F17, F18, F20, F21 and F23, each with a test that fails if the behaviour regresses.

### Task 4.1: Behaviour contract

**Files:**
- Create: `internal/dstack/mock/doc.go`

**Step 1:** Write the package doc as the contract. This is the reference an implementer reads instead of re-reading dstack's source:

```go
// SPDX-License-Identifier: Apache-2.0

// Package mock is an in-memory fake of the dstack server API, reproducing
// the five source-verified behaviours Squall's design rests on. It exists
// so that every mechanic in spec §5.2/§6/§7 is testable in envtest without
// provisioning a GPU.
//
// Reproduced, each with a test that fails if it regresses:
//
//	F17  `replicas` is an IN-PLACE updatable service field. Apply updates
//	     the same run id and increments deployment_num — no new run. Fixed
//	     `replicas: 0` is accepted and yields a registered, routable
//	     service with zero jobs. Asleep-but-addressable is first-class.
//
//	F18  apply_plan enforces optimistic concurrency: an apply computed
//	     against changed state fails with "Resource has been changed. Try
//	     again or use force apply". Squall NEVER sends force — the losing
//	     side of any race must fail loudly (§5.2, AC13).
//
//	F20  The run id survives flips but NOT terminal states. A terminal run
//	     is DEREGISTERED from the gateway (404, not 503) and the next apply
//	     mints a NEW run id. Dead is not asleep.
//
//	F21  Runs land on fleets. Flipping replicas to 0 terminates the JOB;
//	     the INSTANCE is released only by fleet idle_duration. The surviving
//	     instance is the warm pool.
//
//	F23  Gateway responses are immediate, never held: registered + 0
//	     replicas + auth -> 503; unregistered/terminal -> 404; bad token
//	     -> 403. The gateway never wakes a ManualScaler service.
//
// Deliberately NOT modelled: offer selection, pricing, real provisioning
// latency, SSH tunnels. Those are dstack's job and are proven on real
// hardware in PoC 0/2, not here.
package mock
```

**Step 2: Commit**

```bash
git add internal/dstack/mock/doc.go
git commit -m "docs: specify fake dstack behaviour contract"
```

---

### Task 4.2: In-place flip preserves run identity (F17)

**Files:**
- Create: `internal/dstack/mock/mock_test.go`, `internal/dstack/mock/mock.go`

**Step 1: Write the failing test**

```go
func TestApply_FlipIsInPlace_PreservesRunID(t *testing.T) {
	s := New()

	r1 := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	r2 := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 0, BaseDeploymentNum: r1.DeploymentNum})

	if r2.RunID != r1.RunID {
		t.Fatalf("F17: flip must be in-place; run id changed %s -> %s", r1.RunID, r2.RunID)
	}
	if r2.DeploymentNum != r1.DeploymentNum+1 {
		t.Fatalf("F17: deployment_num must increment: got %d, want %d", r2.DeploymentNum, r1.DeploymentNum+1)
	}
}

func TestApply_ZeroReplicas_StaysRegisteredAndRoutable(t *testing.T) {
	s := New()
	r := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 0, BaseDeploymentNum: r.DeploymentNum})

	// F23: registered + 0 replicas + auth -> 503, NOT 404.
	if code := s.GatewayGet("qwen", validToken); code != 503 {
		t.Fatalf("F17/F23: asleep-but-addressable must answer 503, got %d", code)
	}
}
```

**Step 2:** Run — FAIL, `undefined: New`.

**Step 3:** Implement the minimum: a `Server` with a mutex-guarded `map[string]*service` holding `runID`, `deploymentNum`, `replicas`, `state`; `Apply` mutating in place; `GatewayGet` returning per F23.

**Step 4:** Run — PASS.

**Step 5: Commit**

```bash
git commit -am "feat: fake dstack in-place replica flip (F17)"
```

---

### Task 4.3: CAS rejection without force (F18)

**Step 1: Write the failing test**

```go
func TestApply_StaleDeploymentNum_IsRejected(t *testing.T) {
	s := New()
	r1 := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	// Two concurrent flips computed against the same base — AC13's drill.
	_, err := s.Apply(ApplyRequest{Name: "qwen", Replicas: 0, BaseDeploymentNum: r1.DeploymentNum})
	if err != nil {
		t.Fatalf("first flip must win: %v", err)
	}
	_, err = s.Apply(ApplyRequest{Name: "qwen", Replicas: 1, BaseDeploymentNum: r1.DeploymentNum})
	if !errors.Is(err, ErrResourceChanged) {
		t.Fatalf("F18: the loser must fail loudly, got %v", err)
	}
}

func TestApply_ForceIsNeverAccepted(t *testing.T) {
	// Squall must never send force (§5.2). The fake refuses it outright so
	// that a future caller adding force fails a test rather than a bill.
	s := New()
	if _, err := s.Apply(ApplyRequest{Name: "qwen", Replicas: 1, Force: true}); !errors.Is(err, ErrForceForbidden) {
		t.Fatalf("force must be refused by construction, got %v", err)
	}
}
```

**Step 2–4:** Red → implement `ErrResourceChanged` / `ErrForceForbidden` and the `BaseDeploymentNum` check → green.

**Step 5: Commit**

```bash
git commit -am "feat: fake dstack optimistic-concurrency rejection (F18)"
```

---

### Task 4.4: Terminal states mint a new run (F20)

**Step 1: Write the failing test**

```go
func TestTerminal_DeregistersAndMintsNewRunID(t *testing.T) {
	s := New()
	r1 := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	s.Terminate("qwen") // uncommanded death — host loss, spot reclaim

	// F20/F23: dead is 404, not 503. This is what tells the proxy to
	// recreate-and-alarm instead of merely waking.
	if code := s.GatewayGet("qwen", validToken); code != 404 {
		t.Fatalf("F20: terminal run must deregister (404), got %d", code)
	}

	r2 := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	if r2.RunID == r1.RunID {
		t.Fatal("F20: apply after a terminal state must mint a NEW run id")
	}
}
```

**Step 2–4:** Red → implement → green.

**Step 5: Commit**

```bash
git commit -am "feat: fake dstack terminal-state semantics (F20)"
```

---

### Task 4.5: Fleet releases the machine, not the flip (F21)

**Step 1: Write the failing test**

```go
func TestFleet_FlipReleasesJobNotInstance(t *testing.T) {
	s := New()
	s.SetClock(fakeClock) // no wall-clock sleeps in tests
	r := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1, FleetIdleDuration: 10 * time.Minute})
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 0, BaseDeploymentNum: r.DeploymentNum})

	if got := s.InstanceCount("qwen"); got != 1 {
		t.Fatalf("F21: flipping to 0 terminates the JOB; the instance is the warm pool, got %d", got)
	}

	fakeClock.Advance(11 * time.Minute)
	s.Tick()

	if got := s.InstanceCount("qwen"); got != 0 {
		t.Fatalf("F21: fleet idle_duration must release the machine, got %d", got)
	}
}
```

**Step 2–4:** Red → implement an injectable clock and a `Tick()` that expires idle instances → green.

The injectable clock matters beyond this test: Phase 7's sleep flip and Phase 8's drain both have timing, and neither may be tested with `time.Sleep`.

**Step 5: Commit**

```bash
git commit -am "feat: fake dstack fleet idle release with injectable clock (F21)"
```

---

# Phase 5 — dstack client

**Outcome:** `internal/dstack` speaks to the fake in tests and to a real server in production, and cannot send `force`.

### Task 5.1: Client interface

**Files:**
- Create: `internal/dstack/client.go`

**Step 1:** Define the narrow interface the controller needs — nothing more (YAGNI; the client is not a dstack SDK):

```go
type Client interface {
	// Apply flips the replica count in place. It NEVER sends force: the
	// losing side of a race must fail loudly (F18, §5.2, AC13).
	Apply(ctx context.Context, req ApplyRequest) (*Run, error)
	// Get returns current run state, or ErrNotFound if deregistered (F20).
	Get(ctx context.Context, name string) (*Run, error)
	// Delete removes the run. Fleet release is dstack's, via idle_duration.
	Delete(ctx context.Context, name string) error
	// ListRuns backs the reconcile loop's orphan diff (§5.2).
	ListRuns(ctx context.Context) ([]Run, error)
}
```

**Step 2:** Write a test asserting the HTTP client sends no `force` field under any input, then implement.

**Step 3: Commit**

```bash
git commit -am "feat: add dstack client with force forbidden by construction"
```

---

# Phase 6 — The wake path

**Outcome:** AC4 and AC13 pass in envtest — a 50-request burst yields exactly one wake, and a concurrent-flip race has a loud loser.

### Task 6.1: Phase state machine as a pure function

**Files:**
- Create: `internal/controller/squall/phase.go`, `phase_test.go`

Keep it pure: `(observed dstack state, spec, now) → (desired phase, action)`. Pure means table-driven tests with no envtest, running in `test-unit` at ~10s. Every §5.2 transition gets a table row, including `Asleep`→flip and `Dead`→recreate+alarm.

Write the table test first, red, then implement.

### Task 6.2: Wake reconcile against the fake

**Files:**
- Modify: `internal/controller/squall/model_controller.go`
- Create: `internal/controller/squall/model_controller_test.go`

Test: create a CR, patch a demand annotation, assert exactly one `Apply` reached the fake, `status.phase == Waking`, `status.runId` journaled.

### Task 6.3: Coalescing — AC4

Test: fire 50 concurrent demand patches at a cold model; assert `fake.ApplyCount("qwen") == 1`. This is AC4 and PoC 3's headline, and it runs in envtest in under a second.

### Task 6.4: CAS loser fails loudly — AC13

Test: two deliberately concurrent flips; assert one returns `ErrResourceChanged` and the reconciler requeues rather than retrying with force.

Commit after each task.

---

# Phase 7 — The sleep path

**Unblocked:** spec v0.16 §6 fixes the mechanism. Implement it exactly (see the D3 section above); do not reintroduce the annotation/TTL design.

### Task 7.1: Idle aggregation across replicas

Proxy side: each replica serves per-model `inFlight` and `lastRequestAt` on an internal endpoint. Controller side: enumerate the proxy Service's Endpoints, query every replica, and require a complete, fresh answer from all of them.

```
sleep iff  every replica in Endpoints answered, freshly
      AND  all report inFlight == 0
      AND  now - max(lastRequestAt) > scaleDownDelaySeconds
      AND  spec.minReplicas == 0
```

**Three tests, and the last two are the point of the whole task:**

1. All replicas idle past the window → the flip to 0 happens.
2. Replica A holds one in-flight request, replica B reports idle → **no flip.** Release A → flip happens.
3. Replica B is **unreachable** while A reports idle → **no flip.** Not "assume idle", not "use the last known value" — stay awake. This is the failure mode that silently kills generations, and it is the one an implementer optimises away by accident.

### Task 7.2: Endpoints churn does not cause a wrong sleep

Test: a proxy replica is added to Endpoints but has not yet been queried → incomplete evidence → stay awake. A replica is removed mid-evaluation → re-evaluate, do not sleep on the stale set. Rolling a proxy Deployment must never sleep a serving model.

### Task 7.3: Pinned models never sleep — AC17

Test: `minReplicas: 1` survives a full idle window; toggling pinned ↔ on-demand takes effect without a recreate (the run id must not change — F17).

---

# Phase 8 — Finalizer and drain-first teardown

**Outcome:** AC6 and PoC 9 pass.

### Task 8.1: Finalizer ordering

Implement and test the §5.2 sequence, in order: deregister from discovery → proxy answers 404 → bounded in-flight drain (`drainTimeout`) → delete runs → release fleet → remove finalizer. Test each hop with the injectable clock; assert an in-flight request is never cut inside `drainTimeout`.

Port the finalizer test shapes from the sister's `internal/controller/litellmconnection_finalizer_test.go`.

### Task 8.2: Fail-closed on an unreadable API server

Test: simulate API-server unavailability and partial watch data while live capacity exists; assert **zero** destructive actions (§5.2, AC6, PoC 9). An unreadable desired state MUST NEVER be interpreted as empty.

### Task 8.3: provisioningTimeout is the only destructive trigger

Test: a run that never reaches ready is destroyed at `provisioningTimeout`, alarmed, no drain attempted (AC15). Separately: a healthy run older than `maxLifetime` raises an alert and is **NOT** destroyed.

---

# Phase 9 — squall-proxy

**Unblocked** — D2 and D3 are both closed in the spec. Implement §7's table as written: six phases, six rows.

Two v0.16 additions land here:

- **`maxPendingPerModel`** — one global proxy setting (generous default) bounding held capacity per model. Beyond it, answer the wait contract immediately instead of blocking. **Not a CRD field in v0.1**; PoC 3's recorded peak tunes the default. Test: N+1 concurrent holds → the N+1th is answered, not queued.
- **The internal activity endpoint** from Task 7.1 — per-model `inFlight` and `lastRequestAt`. It is the proxy's half of the sleep contract, so it belongs to the same acceptance bar as the hold itself.

### Task 9.1: The decision table as a pure function

`(phase, hasCR, gatewayCode) → action`. Table-driven test transcribing §7's table verbatim — one test case per row, no exceptions. Two rows carry behaviour that is easy to get subtly wrong:

- **`Draining`** — in-flight forwards until `drainTimeout`, but **new requests never block**; they get 404 (the model is leaving desired state). Blocking a new request against a draining model would hold a connection for capacity that is being torn down.
- **`Dead`** — demand patch (recreate) **plus an alarm when the death was uncommanded**, block, then 503 `recreating` on deadline. The wait is a full cold start, not a flip, so the deadline expectations differ from `Asleep`.

### Task 9.2: Blocking hold

KubeAI's mechanism (F32): block awaiting Ready, write **nothing** to the client, answer a real status code on deadline. Test with `httptest` and the injectable clock: assert zero bytes on the wire before the outcome, then 503 + `Retry-After` + a JSON body naming the state.

The v0.11 SSE-keepalive design is withdrawn. Do not emit keepalives. Putting `200` on the wire before the outcome silently opts the model out of LiteLLM's fallback chain.

### Task 9.3: Demand patches

Coalesced and rate-limited per cooldown. Test: N concurrent requests produce one patch per cooldown window, not N.

### Task 9.4: `/v1/models`

The discovery surface (F27, F30). Test: shape matches what `type: kubeai` discovery expects — a `{"data":[{"id":...}]}` listing — and it is served from the informer cache, never by probing capacity (§10's two-lane rule).

---

# Phase 10 — Metrics, drift gates and pre-push

**Release scope decision, 2026-08-26:** v0.1 ships **drift gates and the pre-push gate only**. No goreleaser, no archives, no SBOM, no cosign signing — there is nothing published yet, and each is additive later. What is *not* deferred is the drift gate: the sister shipped stale CRDs to users twice (v0.7.0, two separate PRs) because nothing enforced that a changed CRD field regenerated the published chart. Squall acquires that exact failure mode the moment the `Model` CRD ships in a chart.

### Task 10.1: Declared/observed gauge pairs — AC19

Export `squall_model_age_seconds` vs `squall_model_max_lifetime_seconds`, and `squall_model_price_per_hour` (observed, from dstack) vs `squall_model_max_price_per_hour` (spec). One generic `observed > declared` alert rule covers every model.

Port the scrape-test pattern from the sister's `internal/metrics` + its metrics scrape test.

**Note for the spec, not for the code:** §10 currently both claims "no spend integral exists" and lists a "time integral of active replica price"; AC8 requires budget alarms that v0.13 cut. Once resolved, the integral is a PromQL `sum_over_time` over the price gauge — no Go code, so it does not belong in this phase either way.

### Task 10.2: Chart + CRD drift gate

Port `helm-sync` / `helm-sync-check` and `deploy-kustomize-sync` / `deploy-kustomize-sync-check` from the sister, filling in the Phase 1 stubs. Add the drift step to `ci.yml` that Task 1.5 deliberately left out. Pass criterion: changing a field in `api/squall/v1alpha1/model_types.go` without running `make helm-sync` **fails CI**.

### Task 10.3: pre-push gate

Port `scripts/pre-push-check.sh` and `hack/install-hooks.sh`. The sister runs 18 gates; port the ones that apply to a repo of this shape: gitleaks, trufflehog, large tracked files, sensitive file patterns, LICENSE/README presence, origin remote, internal hostnames and private IPv4, ackstorm emails in tracked files, `.gitignore` sanity, commit authors, DO-NOT-COMMIT markers, working-tree status, govulncheck against an acknowledged-list, `go mod tidy` drift, SPDX headers, full lint sweep, unit tests.

Two of these matter more than the rest for a repo going public: **the secret scanners** and **internal hostnames / ackstorm emails in tracked files**. Wire the hook so it cannot be forgotten.

---

# Phase 11 — e2e on kind

### Task 11.1: Cluster hydration

Port `hack/cluster.sh`, `hack/kind-config.yaml` and the phased `test/e2e/cluster/0{0..4}-*` layout. Squall's phases: namespaces → **fake dstack Deployment** → operator → fixtures. No LiteLLM, no toolhive.

### Task 11.2: Ship the fake dstack as a container

The Phase 4 package gets a thin `cmd/fake-dstack` and a Dockerfile, kind-loaded like the sister's `litellm-mock:e2e`. The whole CR→wake→serve→sleep loop then runs in CI.

### Task 11.3: Full-loop e2e

Ginkgo: apply a Model CR → assert `Waking` → fake reports ready → assert `Ready` → drive a request through the proxy → idle → assert the flip to 0 → assert the instance survives → advance the fleet clock → assert release.

---

# Phase 12 — Discovery seam (PoC 10)

**This phase needs no new operator code** (F30, verified): `type: kubeai` against any OpenAI-compatible listing already yields the `baseUrl → params.api_base` overlay and `hosted_vllm/<id>` naming.

### Task 12.1: Discovery CR

Create a `LiteLLMModelDiscovery` with `type: kubeai`, `baseUrl: http://squall-proxy.squall.svc/v1`, and the **external router profile** — `timeout`/`stream_timeout` at provisioning-budget magnitude, ≥ `holdTimeout` (F28, F32). Not `300`/`60`.

### Task 12.2: Fail-closed proof

Kill the proxy; assert discovery deregisters **nothing** (F31, D-09). A proxy outage must not remove external models from LiteLLM.

### Task 12.3: AC sweep

Walk AC1–AC19. Record which are proven by test, which by drill, which are deferred. Per v0.17, AC3 asserts only the 503 wait contract squall owns; the router fallback chain lives in the LiteLLM Helm values, outside v0.1 scope.

---

## What this plan does not cover

Deliberately out of scope, and each is a separate plan when its time comes:

- **PoC 0/2 on real hardware** — wall-clock cold starts per backend, the end-to-end GPU token. Hardware truth, not code; the fake cannot answer it.
- **The engine templates themselves** (§8) — the vLLM/llama.cpp/Ollama images, args and warmup requests. Blocked on D1's resolution of what `engine` selects.
- **Gateway lifecycle** (§11) — the documented create/decommission sequence is a runbook, not Go.
- **goreleaser / SBOM / cosign signing** — deferred by decision (2026-08-26): nothing is published yet and each is additive. Drift gates and pre-push are NOT deferred; they are Phase 10.

---

## Suggested execution order

Phases 1 → 2 → 3 → 4 → 5 → 6 unblocked today. Phase 4 (the fake) is the one to resist rushing: everything after it is only as trustworthy as it is.

All four gate decisions are closed in spec v0.17-RC — no phase is blocked on a pending answer.
