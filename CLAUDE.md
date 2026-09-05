# Squall

External GPU capacity for LLM model serving. A `Model` CR in Git is reconciled by Flux; a
controller provisions GPUs on demand through **dstack** (Vast.ai, AWS, DigitalOcean), wakes
them on the first request, and sleeps them when idle. Models are registered into **LiteLLM**
by an existing operator, not by us.

Two binaries, deliberately separate failure domains:

- **`squall-controller`** — reconciliation only: the 0↔1 replica flip, drain-first teardown
  via finalizer, orphan reconciliation, status.
- **`squall-proxy`** — the per-request data path: forward when Ready, block while waking,
  answer the wait contract truthfully when the deadline expires.

## Spec of record

`docs/specs/squall-spec-v0_18-RC.md` is **authoritative**.
Where the plan and the spec disagree, **the spec wins** — say so rather than silently
picking one. Every `F<n>` / `§<n>` / `AC<n>` / `PoC <n>` reference points into it.

Its §4 fact table is source-verified against dstack's own code, and several rows are marked
*measured (PoC 0-CPU)*. Treat those as measurements, not opinions.

Plan: `docs/plans/2026-08-26-squall-v01-harness-and-scaffolding.md`.

## How to run anything — read before your first command

**The host does not have the right Go.** The project targets **Go 1.26.6**; the toolchain
lives in a container. Every `go`, `kubebuilder`, `controller-gen`, `golangci-lint` and `make`
invocation goes through the wrapper:

```bash
./scripts/dev.sh go version          # -> go1.26.6 linux/amd64
./scripts/dev.sh make test-unit
./scripts/dev.sh make test-envtest
./scripts/dev.sh make qa-lint        # the lint target is qa-lint, NOT lint
./scripts/dev.sh bash                # interactive shell inside the toolchain
make help                            # host-side; self-documenting
```

Never call bare `go`/`make` for build or test. Never set `DOCKER_BUILDKIT=0` — it is a hard
parse error, not a degraded build. Details and the rest of the toolchain's load-bearing
properties: `docs/references/toolchain-and-traps.md`.

## Two invariants that decide dozens of implementation coin-flips

> **Wake may tolerate uncertainty; sleep must not.** Paying for a GPU a little longer is
> always preferable to terminating an active generation.

> **`0→1` fails open, `1→0` fails safe.** A wrong wake costs money; a wrong sleep kills a
> generation.

These supersede any older "replica-count changes are non-destructive" reading found in an
earlier spec revision in git history.

**Squall NEVER sends `force`** (F18, §5.2, AC13). This is enforced *by construction*:
`dstack.ApplyRequest` has no `Force` field, so there is nothing to set and nothing to
validate. The fake refuses force unconditionally so a caller that adds it fails a test
rather than a bill.

## The test phase split — enforced in three places, all needed

`make test-unit` must **never** need a control plane:

- `test/e2e/*.go` are behind `//go:build e2e`.
- `internal/controller/squall/suite_test.go`'s `TestMain` calls `flag.Parse()` and returns
  early under `-short`.
- Each envtest case itself calls `t.Skip` under `-short`.

Pure unit tests (e.g. `phase_test.go`) run under `test-unit` with no skip.

## Testing discipline — the failure mode this project keeps hitting

Three reviews in a row found **no code bug** but tests that pass for the wrong reason. Before
claiming a behaviour is covered, **mutate the implementation to break it and watch a test go
red.** A mutation that leaves the suite green is a finding, not a formality. See
`docs/references/testing-discipline.md`.

**The same failure mode applies to probing a live API — and it has bitten twice.** Vast.ai's
`/api/v0/` is deprecated and answers `410` with a JSON *error body*. A check written as
`json.load(x).get("instances", [])` reads that body, finds no key, and reports **zero
instances** — a green "nothing is running, nothing is billing" that is computed over an error.
Measured 2026-09-05: every `v0` instance check that day was vacuous.

Rules for any live-API probe, money-related or not:

- **Vast.ai: use `/api/v1/`.** `v0` is gone.
- **Assert the success shape before reading it.** `if not d.get("success") or "instances" not
  in d: raise` — never `.get(key, default)` on a response you have not proven is a success.
- **Check the HTTP status.** `curl -w '%{http_code}'`, and in Python let a non-2xx raise.
- A probe that cannot fail loudly is not evidence. Treat "0" from an unchecked call exactly
  like a test that passes for the wrong reason.

## Rules for concurrent agents on one branch — learned the hard way

Two agents once ran concurrently and one destroyed work. These go in **every** implementer
prompt:

- **Never `git add -A`.** The tree contains other agents' in-flight files.
- **Never `git reset`, `git commit --amend`, or rebase past a commit you did not create.**
- **Never `git checkout -- <file>` / `git restore` while the tree is dirty.** Copy to a temp
  path and edit forward instead.
- **Only add commits.** Give each agent a named, disjoint file tree and tell it to stay in it.

## How this project is executed

| Role | Who |
|---|---|
| Planning, spec review, judging findings | The main session (Opus) |
| Implementation | A **Sonnet** subagent, one per **block of phases** |
| Review | A **Sonnet** subagent, once per **block** |

**Block loop: implement → review → fix → gates green → close.**

- **Review per block, not per task and not per phase.** Re-reviewing settled ground costs
  pace and buys nothing.
- **NEVER test after every task. Accumulate, then verify.** This is the owner's most-repeated
  instruction: *"no hagamos testing cada línea que escribimos, porque si no no acabamos ni la
  semana que viene."* A test cycle costs a fixed overhead whether the change is 2 lines or
  200. Group **several tasks** that share a seam, then run **one** scoped package test over
  the accumulated work. Full gates (`test-unit`, `test-envtest`, `qa-lint`) run **once**, at
  the block boundary — not per task, not per file.
- **Batch the mutation sweep too.** Mutation testing is how this project's real gaps were
  found and it stays — but run it as **one sweep at the end of the block**, walking the test
  table, not as a ritual after each behaviour. Write the tests as you go; prove them
  non-vacuous in a single pass.
- **Don't re-run a gate whose inputs provably didn't change.** `git diff` the range first.
  Re-running a suite over a byte-identical tree is pure waste, and heavy gates on a shared dev
  box are felt immediately.
- A block never **closes** with a red test. That is the bar — not "green after every edit".
- **Pipeline.** Start the next block's implementer while the previous block's review runs —
  reviews are read-only, so they do not conflict.
- **Choose block boundaries by RISK, not phase count.** Full depth on novel, high-risk,
  money-or-safety-critical ground; far less on what the spec already settled.

## Reference index

- `docs/references/dstack-real-api.md` — dstack's **actual** REST API, probe model, Kubernetes
  backend and ManualScaler, read from upstream source at commit `a70d98b`. Read this before
  touching `internal/dstack`: the client currently speaks a protocol Squall invented and none
  of it matches.
- `docs/references/toolchain-and-traps.md` — the containerized toolchain's load-bearing
  properties, and every trap that already cost real time.
- `docs/references/testing-discipline.md` — the vacuous-test failure mode, with the concrete
  mutations that exposed it.
- `docs/references/decisions-and-open-items.md` — decisions taken (do not reopen) and the
  items that need a human.
- `docs/references/deviations-and-findings.md` — **the running ledger.** Every deviation,
  invention and known hazard found during implementation. Do NOT stop to adjudicate these one
  at a time; append and keep moving. They are reviewed in one architecture pass at the end of
  v0.1. Every implementer and reviewer appends; never renumber, never delete an entry.
- `docs/reviews/2026-08-31-v01-branch-code-review.md` — the full v0.1 branch review, 10
  finder angles over 215 files. All 26 findings with file:line, severity, risk and a
  fix-size estimate, plus the raw JSON. These are the ledger's **D103-D128**: read the
  ledger for the adjudication, this file for the evidence behind it.
- `docs/references/rtx6kpro-notes.md` — measured serving numbers for our GPU class
  (Qwen3.8-27B perf headroom, GLM-5.3-Flash 4-card recipe), extracted from the rtx6kpro
  community wiki. Read before touching `config/samples/`.
- In-path memories: `internal/dstack/CLAUDE.md`, `internal/controller/squall/CLAUDE.md`.
