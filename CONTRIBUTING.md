# Contributing to Squall

Thanks for considering a contribution. Please read the
[Code of Conduct](CODE_OF_CONDUCT.md) first.

Squall is pre-alpha and is being built against a written specification. Behaviour changes
are decided in `docs/specs/` before they are written in Go — if a change alters the state
machine, the wait contract or the provider boundary, say which spec section it implements
(or propose the spec change first).

## Setting up

Your host needs Docker and nothing else. The whole toolchain — Go 1.26.4, kubebuilder,
controller-gen, kustomize, envtest assets, kind, helm, kubectl — lives in a container:

```sh
make doctor                  # preflight; tells you what is missing
./scripts/dev.sh go version  # first run builds the image (a few minutes)
```

See [README.md](README.md#building) for how the wrapper works and why it is Linux-only.

## Before you open a pull request

```sh
make qa-lint       # golangci-lint
make qa-fmt-check  # gofmt, no mutation
make test-unit     # pure logic
make test-envtest  # controller against a real API server, race-enabled
```

CI runs exactly these targets through `./scripts/dev.sh`, and nothing else. That is
deliberate: no CI step reimplements a local command, so a green local run means a green
pipeline. If you need CI to do something new, add a `make` target for it — do not inline
shell into the workflow YAML.

## House rules

- **Every `.go` file starts with** `// SPDX-License-Identifier: Apache-2.0`.
- **Conventional commits**, imperative subject under 72 characters.
- **No naked polling loops.** `until <cmd>; do sleep N; done` hangs forever the moment the
  thing it polls disappears. Every wait needs an upper bound *and* an explicit failure
  path. Shell waits belong in the Makefile's `Waiters` section.
- **Never commit `.gocache/`.** `scripts/dev.sh` points `KUBECONFIG` inside it, so kind
  cluster admin credentials live there.
- Regenerated artifacts (`make gen-code`, `make gen-manifests`) are committed alongside
  the change that caused them.

## Reporting

- Bugs and features: GitHub issues.
- Security vulnerabilities: **not** an issue — see [SECURITY.md](SECURITY.md).
