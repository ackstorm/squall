# Image URL to use all building/pushing image targets.
IMG ?= squall:latest

# Version stamped into both binaries via -ldflags (see build-operator), and
# into the images built from them. Keep in step with deploy/helm/squall/
# Chart.yaml's version/appVersion: a binary reporting "dev" in a tagged
# release is indistinguishable from a developer's local build, which is
# exactly the ambiguity you do not want when reading a bug report.
VERSION ?= 0.1.0

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set).
# Guarded with `command -v go` so host-only targets (clean, doctor, cluster-up, ...)
# do not surface a "make: go: No such file or directory" error: go lives inside
# the devtools container (see scripts/dev.sh), not on the host PATH.
ifeq (,$(shell command -v go >/dev/null 2>&1 && go env GOBIN))
GOBIN=$(shell command -v go >/dev/null 2>&1 && go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker. However, you
# might want to replace it to use other tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# --- execution-context routing (explicit opt-in; NO magic-by-prefix) -------
# container_target re-runs a PRIVATE target ($1, conventionally _name) inside
# the devtools container, unless we are already inside it. Each public target
# that needs the Go/helm toolchain calls this explicitly, so `make help` stays
# honest and a future host-only target is never auto-wrapped by accident.
#
# $(MAKEOVERRIDES) forwards the caller's command-line variable assignments
# (e.g. PKG=… FOCUS=… RUN=… TIMEOUT=…). It is REQUIRED on the dev.sh path:
# scripts/dev.sh only forwards an explicit -e allowlist into the container, so
# MAKEFLAGS (which normally carries command-line overrides to a sub-make) does
# NOT cross the docker boundary. Without this, arg-taking wrappers would see an
# empty $(PKG)/$(FOCUS).
SQUALL_IN_DEVTOOLS ?= 0
define container_target
	@if [ "$(SQUALL_IN_DEVTOOLS)" = "1" ]; then \
		$(MAKE) --no-print-directory $(1) $(foreach o,$(MAKEOVERRIDES),'$o'); \
	else \
		./scripts/dev.sh $(MAKE) --no-print-directory $(1) $(foreach o,$(MAKEOVERRIDES),'$o'); \
	fi
endef
# Each command-line override is single-quoted ($(foreach …,'$o')) so a value
# containing shell metacharacters — notably FOCUS='TestA|TestB' (regex
# alternation) — survives the dev.sh / sub-make hop as ONE argument instead
# of being split into a shell pipe.

# --- bootstrap guard -------------------------------------------------------
# There is no go.mod until the kubebuilder scaffold lands (plan Phase 2), and
# every `go` subcommand hard-fails outside a module ("directory prefix . does
# not contain main module"). Targets that compile or test Go code route through
# this macro, so the whole pipeline — local and CI — is green on the empty repo
# and starts doing real work the moment go.mod appears. Nothing needs deleting
# then: the guard turns into a no-op on its own.
define go_or_skip
	@if [ -f go.mod ]; then $(1); else echo "$@: no go.mod yet (kubebuilder scaffold = plan Phase 2) — skipping."; fi
endef

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build-operator

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Diagnostics

.PHONY: doctor
doctor: ## Fast local preflight: docker, devtools image, socket, cache paths, in-container tools, kubeconfig (if present). No network.
	@echo "== squall doctor (fast) =="
	@docker info >/dev/null 2>&1 && echo "OK   docker daemon reachable" || { echo "FAIL docker daemon unreachable"; exit 1; }
	@test -S /var/run/docker.sock && echo "OK   /var/run/docker.sock present" || echo "WARN /var/run/docker.sock not a socket on host"
	@docker image inspect squall-devtools:latest >/dev/null 2>&1 && echo "OK   squall-devtools:latest present" || echo "WARN squall-devtools:latest absent (built on first ./scripts/dev.sh use)"
	@for d in .gocache/gopath .gocache/build .gocache/envtest .gocache/kube; do test -d "$$d" && echo "OK   $$d" || echo "WARN $$d missing (created on first dev.sh run)"; done
	@test -f go.mod && echo "OK   go.mod present" || echo "INFO no go.mod yet (kubebuilder scaffold = plan Phase 2)"
	@./scripts/dev.sh bash -c '\
	  for t in go kubebuilder controller-gen setup-envtest kind helm kubectl govulncheck; do \
	    command -v $$t >/dev/null 2>&1 && echo "OK   (container) $$t" || echo "FAIL (container) $$t MISSING"; \
	  done; \
	  for t in golangci-lint; do \
	    if command -v $$t >/dev/null 2>&1 || test -x /workspace/bin/$$t; then \
	      echo "OK   (container) $$t (baked or installed-on-demand)"; \
	    else \
	      echo "INFO (container) $$t not yet installed (go-installed on first lint into ./bin)"; \
	    fi; \
	  done; \
	  test -d "$$ENVTEST_BIN_DIR/k8s" && echo "OK   (container) envtest assets in $$ENVTEST_BIN_DIR" || echo "WARN (container) no pre-baked envtest assets"'
	@test -f .gocache/kube/config && echo "OK   kubeconfig present (.gocache/kube/config)" || echo "INFO no kubeconfig yet (run make cluster-up)"

.PHONY: shell
shell: ## Interactive shell inside the devtools container.
	./scripts/dev.sh bash

.PHONY: clean-cache
clean-cache: ## Remove ./.gocache, unlocking Go's read-only modcache first. Host-only; re-created on next dev.sh use.
	@if [ ! -e .gocache ]; then echo "No ./.gocache here — nothing to clean."; exit 0; fi
	@echo "Unlocking read-only Go modcache dirs under ./.gocache ..."
	@chmod -R u+w .gocache 2>/dev/null || true
	rm -rf .gocache
	@echo "Removed ./.gocache (re-created on next ./scripts/dev.sh use)."

.PHONY: clean
clean: clean-cache ## Remove all build artifacts: bin/, dist/, coverage profiles, testbin/, and ./.gocache. Host-only.
	rm -rf bin dist testbin cover-unit.out cover-envtest.out
	@echo "Removed bin/ dist/ testbin/ cover*.out (tool + service binaries re-fetched/rebuilt on next make)."

##@ Development

.PHONY: gen-code
gen-code: ## Generate DeepCopy, DeepCopyInto and DeepCopyObject method implementations.
	$(call container_target,_gen-code)
# paths= is scoped to our own source roots rather than the bare "./..." that
# controller-gen's default examples use: .gocache/ (dev.sh's persisted GOPATH,
# gitignored but living inside this module's directory tree) accumulates
# third-party modules across every `go`/`golangci-lint` invocation, and at
# least one (github.com/ckaznocha/intrange, a golangci-lint plugin dep) ships
# its own go.work file that controller-gen's package loader trips over. "./..."
# walks into .gocache and hits it; explicit roots don't.
CONTROLLER_GEN_PATHS := paths=./api/... paths=./cmd/... paths=./internal/...

_gen-code:
	$(call go_or_skip,$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt $(CONTROLLER_GEN_PATHS))

.PHONY: gen-manifests
gen-manifests: ## Generate WebhookConfiguration, Role and CustomResourceDefinition objects.
	$(call container_target,_gen-manifests)
_gen-manifests:
	$(call go_or_skip,$(CONTROLLER_GEN) rbac:roleName=manager-role crd $(CONTROLLER_GEN_PATHS) output:crd:artifacts:config=config/crd/bases)

.PHONY: fmt
fmt: ## Run go fmt against code.
	$(call container_target,_fmt)
_fmt:
	$(call go_or_skip,go fmt ./...)

.PHONY: vet
vet: ## Run go vet against code.
	$(call container_target,_vet)
_vet:
	$(call go_or_skip,go vet ./...)

.PHONY: qa-fmt-check
qa-fmt-check: ## Fail if any Go file is not gofmt-clean (no mutation).
	$(call container_target,_qa-fmt-check)
_qa-fmt-check:
	@files=$$(git ls-files '*.go' | grep -v -E 'zz_generated|/vendor/' || true); \
	if [ -z "$$files" ]; then echo "No Go files yet — skipping."; exit 0; fi; \
	out=$$(gofmt -l $$files); \
	if [ -n "$$out" ]; then echo "Not gofmt-clean:"; echo "$$out"; exit 1; fi; \
	echo "OK gofmt-clean"

##@ QA

.PHONY: qa-lint
qa-lint: ## Run golangci-lint linter.
	$(call container_target,_qa-lint)
_qa-lint: golangci-lint
	$(call go_or_skip,$(GOLANGCI_LINT) run)

.PHONY: qa-lint-fix
qa-lint-fix: ## Run golangci-lint linter and perform fixes.
	$(call container_target,_qa-lint-fix)
_qa-lint-fix: golangci-lint
	$(call go_or_skip,$(GOLANGCI_LINT) run --fix)

##@ Security

.PHONY: qa-security
qa-security: ## Security umbrella (D36): gitleaks + trufflehog + internal-hostname/ackstorm-email sweep, full repo/history.
	$(call container_target,_qa-security)
_qa-security:
	@./scripts/security-scan.sh

##@ Test

.PHONY: test-unit
test-unit: ## Phase 1 — pure-logic tests, no envtest, no cluster.
	$(call container_target,_test-unit)
_test-unit: fmt vet
	$(call go_or_skip,go test ./... -short -count=1 -coverprofile cover-unit.out)

.PHONY: test-envtest
test-envtest: ## Phase 2 — controller envtest with -race. Slower, but catches data races. CI gate.
	$(call container_target,_test-envtest)
_test-envtest: gen-manifests gen-code fmt vet setup-envtest
	$(call go_or_skip,KUBEBUILDER_ASSETS="$$($(envtest_assets))" go test ./... -race -count=1 -timeout 15m -coverprofile cover-envtest.out)

.PHONY: test-envtest-fast
test-envtest-fast: ## Phase 2 without -race. Dev inner loop (~3x faster). NOT a CI gate.
	$(call container_target,_test-envtest-fast)
_test-envtest-fast: setup-envtest
	$(call go_or_skip,KUBEBUILDER_ASSETS="$$($(envtest_assets))" go test ./... -count=1 -timeout 10m)

.PHONY: test-full
test-full: test-unit test-envtest ## All non-cluster tests (test-unit + test-envtest).

##@ Build

.PHONY: build-operator
build-operator: ## Build both binaries into bin/: squall-controller and squall-proxy.
	$(call container_target,_build-operator)
_build-operator: gen-manifests gen-code fmt vet
	$(call go_or_skip,go build -trimpath -ldflags="-s -w -X main.Version=$(VERSION)" -o bin/squall-controller ./cmd/controller && \
	  go build -trimpath -ldflags="-s -w -X main.Version=$(VERSION)" -o bin/squall-proxy ./cmd/proxy)

# --- e2e images (Phase 11.1/11.2) -------------------------------------------
# Built from the root Dockerfile (ARG CMD selects the cmd/ package), tagged
# :e2e and kind-loaded by hack/cluster.sh — never pushed, never part of a
# release. Real release images (goreleaser, SBOM, cosign) are out of v0.1
# scope (see plan Phase 10's release-scope note).
.PHONY: build-image-controller
build-image-controller: ## Build squall-controller:e2e from the root Dockerfile.
	$(call container_target,_build-image-controller)
_build-image-controller:
	$(CONTAINER_TOOL) build -t squall-controller:e2e --build-arg CMD=controller --build-arg VERSION=$(VERSION) .

.PHONY: build-image-proxy
build-image-proxy: ## Build squall-proxy:e2e from the root Dockerfile.
	$(call container_target,_build-image-proxy)
_build-image-proxy:
	$(CONTAINER_TOOL) build -t squall-proxy:e2e --build-arg CMD=proxy --build-arg VERSION=$(VERSION) .

.PHONY: build-image-fake-dstack
build-image-fake-dstack: ## Build fake-dstack:e2e (Phase 4's mock, containerized) from the root Dockerfile.
	$(call container_target,_build-image-fake-dstack)
_build-image-fake-dstack:
	$(CONTAINER_TOOL) build -t fake-dstack:e2e --build-arg CMD=fake-dstack .

.PHONY: build-image-model-mock
build-image-model-mock: ## Build model-mock:e2e (minimal OpenAI-compatible engine double) from the root Dockerfile.
	$(call container_target,_build-image-model-mock)
_build-image-model-mock:
	$(CONTAINER_TOOL) build -t model-mock:e2e --build-arg CMD=model-mock .

##@ Dependencies

## Location to install dependencies to.
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool binaries.
# controller-gen and setup-envtest are BAKED into the devtools image
# under /usr/local/bin (Dockerfile.devtools sets GOBIN=/usr/local/bin), pinned
# there by ARG. The bare name therefore resolves on PATH in every context these
# targets actually run in — inside the container. golangci-lint is deliberately
# not baked (it churns faster than the rest of the toolchain); it is go-installed
# on demand into $(LOCALBIN) by go-install-tool.
KUBECTL ?= kubectl
CONTROLLER_GEN ?= controller-gen
ENVTEST ?= setup-envtest
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

## Tool versions.
GOLANGCI_LINT_VERSION ?= v1.62.2

# ENVTEST_K8S_VERSION is pinned by Dockerfile.devtools (ARG ENVTEST_K8S_VERSION)
# and exported into the container environment. An environment variable already
# counts as "defined" for `?=`, so in-container the image's value always wins
# over this default; the default only covers a stray non-container invocation.
ENVTEST_K8S_VERSION ?= 1.31.0

# envtest asset resolution. ENVTEST_ASSET_DIR is the PRIMARY store, probed first
# with -i (installed-only, NO network). In the devtools image it is /opt/envtest
# — Dockerfile.devtools bakes the k8s assets there (currently
# /opt/envtest/k8s/1.31.0-linux-amd64) and sets ENVTEST_BIN_DIR=/opt/envtest,
# which the container inherits as ENV; scripts/dev.sh deliberately does NOT
# override it, and make picks it up as a variable. On a miss (version skew, or a
# non-container path where ENVTEST_BIN_DIR is unset) it falls back to a download
# into the writable $(LOCALBIN). $(envtest_assets) is a shell command STRING
# (not a result), so it composes in both recipe-runtime `$$(...)` and make-time
# `$(shell ...)` call sites.
ENVTEST_ASSET_DIR ?= $(or $(ENVTEST_BIN_DIR),$(LOCALBIN))
envtest_assets = $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSET_DIR) -i -p path 2>/dev/null || $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path

.PHONY: setup-envtest
setup-envtest: ## Resolve the ENVTEST binaries (pre-baked assets first, download as fallback).
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(envtest_assets) || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of
# binary, if it doesn't exist.
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

##@ Packaging & Sync

.PHONY: helm-sync
helm-sync: ## Regenerate deploy/helm/squall/crds/ from config/crd/bases/ (Phase 10.2: CRD drift only, see plan).
	$(call container_target,_helm-sync)
_helm-sync: gen-manifests
	mkdir -p deploy/helm/squall/crds
	cp -f config/crd/bases/squall.ackstorm.ai_*.yaml deploy/helm/squall/crds/

.PHONY: helm-sync-check
helm-sync-check: ## CI gate: fail if `make helm-sync` produced uncommitted diff (drift between the Model CRD and the chart).
	$(call container_target,_helm-sync-check)
_helm-sync-check: helm-sync
	@if ! git diff --quiet deploy/helm/squall/; then \
	  echo "CHART DRIFT: deploy/helm/squall/ is out of sync with config/crd/bases/. Run \`make helm-sync\` and commit."; \
	  git diff deploy/helm/squall/; \
	  exit 1; \
	fi

##@ Release

# Where a tagged release publishes to. IMAGE_NAMESPACE is an override rather
# than a constant on purpose: .github/workflows/release.yml passes
# github.repository_owner, so a rehearsal on a scratch repository publishes to
# that owner's packages and exercises the real path instead of simulating it.
REGISTRY         ?= ghcr.io
IMAGE_NAMESPACE  ?= ackstorm
CONTROLLER_IMAGE ?= $(REGISTRY)/$(IMAGE_NAMESPACE)/squall-controller
PROXY_IMAGE      ?= $(REGISTRY)/$(IMAGE_NAMESPACE)/squall-proxy
CHART_REPO       ?= oci://$(REGISTRY)/$(IMAGE_NAMESPACE)/charts
DIST             ?= dist

# `docker login` and `helm registry login` write into $HOME, and scripts/dev.sh
# gives the container a HOME of its own (/workspace/.gocache). A login done on
# the host is therefore invisible in here even though both sides drive the same
# daemon through the mounted socket — so the login happens inside, reading
# REGISTRY_USER / REGISTRY_TOKEN, which dev.sh forwards explicitly.
#
# $$REGISTRY_TOKEN, not $(REGISTRY_TOKEN): the value reaches the recipe through
# the environment and is never interpolated into the command line, so it cannot
# land in a build log or in `ps`. Empty means "a login already exists" (the
# local case) rather than an error.
define registry_login
	@if [ -n "$${REGISTRY_TOKEN:-}" ]; then \
	  printf '%s' "$$REGISTRY_TOKEN" | $(1) $(REGISTRY) -u "$$REGISTRY_USER" --password-stdin >/dev/null \
	    && echo "$@: logged in to $(REGISTRY) as $$REGISTRY_USER"; \
	else \
	  echo "$@: REGISTRY_TOKEN is empty — assuming an existing login to $(REGISTRY)"; \
	fi
endef

.PHONY: release-check
release-check: ## Fail unless VERSION, Chart.yaml version and Chart.yaml appVersion all agree.
	@chart="$$(sed -n 's/^version: *//p' deploy/helm/squall/Chart.yaml | tr -d '\"' | head -1)"; \
	 app="$$(sed -n 's/^appVersion: *//p' deploy/helm/squall/Chart.yaml | tr -d '\"' | head -1)"; \
	 if [ "$$chart" != "$(VERSION)" ] || [ "$$app" != "$(VERSION)" ]; then \
	   echo "RELEASE VERSION MISMATCH: VERSION=$(VERSION) chart.version=$$chart chart.appVersion=$$app"; \
	   echo "All three must agree — the chart defaults its image tag to appVersion, so a skew installs the wrong binary."; \
	   exit 1; \
	 fi; \
	 echo "release-check: VERSION, chart.version and chart.appVersion all say $(VERSION)."

.PHONY: release-images
release-images: ## Build and push the squall-controller and squall-proxy images to the release registry.
	$(call container_target,_release-images)
_release-images: release-check
	$(call registry_login,$(CONTAINER_TOOL) login)
	$(CONTAINER_TOOL) build -t $(CONTROLLER_IMAGE):$(VERSION) -t $(CONTROLLER_IMAGE):latest \
	  --build-arg CMD=controller --build-arg VERSION=$(VERSION) .
	$(CONTAINER_TOOL) build -t $(PROXY_IMAGE):$(VERSION) -t $(PROXY_IMAGE):latest \
	  --build-arg CMD=proxy --build-arg VERSION=$(VERSION) .
	$(CONTAINER_TOOL) push $(CONTROLLER_IMAGE):$(VERSION)
	$(CONTAINER_TOOL) push $(CONTROLLER_IMAGE):latest
	$(CONTAINER_TOOL) push $(PROXY_IMAGE):$(VERSION)
	$(CONTAINER_TOOL) push $(PROXY_IMAGE):latest

.PHONY: release-chart
release-chart: ## Package the Helm chart into dist/ and push it to the release registry as an OCI artifact.
	$(call container_target,_release-chart)
# _helm-sync first: the chart carries a COPY of config/crd/bases, and a release
# that packages a stale copy ships a CRD the controller does not agree with.
_release-chart: release-check _helm-sync
	$(call registry_login,helm registry login)
	mkdir -p $(DIST)
	helm package deploy/helm/squall --version $(VERSION) --app-version $(VERSION) --destination $(DIST)
	helm push $(DIST)/squall-$(VERSION).tgz $(CHART_REPO)

.PHONY: release-assets
release-assets: ## Collect the GitHub Release payload into dist/: CRDs, release notes, checksums. Run AFTER release-chart.
	$(call container_target,_release-assets)
_release-assets:
	mkdir -p $(DIST)
	cp -f deploy/helm/squall/crds/squall.ackstorm.ai_*.yaml $(DIST)/
	@awk -v v='$(VERSION)' 'index($$0, "## [" v "]") == 1 { p = 1; next } \
	                        p && index($$0, "## [") == 1 { exit } \
	                        p && $$0 ~ /^\[[^]]+\]: / { exit } p' CHANGELOG.md > $(DIST)/release-notes.md
	@if [ ! -s $(DIST)/release-notes.md ]; then \
	  echo "release-assets: CHANGELOG.md has no '## [$(VERSION)]' section — a release with no notes is not a release."; \
	  exit 1; \
	fi
	@cd $(DIST) && sha256sum squall-$(VERSION).tgz *.yaml > checksums.txt
	@echo "release-assets: $(DIST)/ holds"; ls -1 $(DIST)

##@ Cluster (e2e infra)

.PHONY: cluster-up
cluster-up: ## Bring up the canonical kind cluster + hydration (Task 11.1).
	$(call container_target,_cluster-up)
_cluster-up:
	bash hack/cluster.sh up

.PHONY: cluster-down
cluster-down: ## Tear down the canonical kind cluster.
	$(call container_target,_cluster-down)
_cluster-down:
	bash hack/cluster.sh down

.PHONY: cluster-hydrate
cluster-hydrate: ## Re-apply hydration on an already-up cluster.
	$(call container_target,_cluster-hydrate)
_cluster-hydrate:
	bash hack/cluster.sh hydrate

.PHONY: cluster-status
cluster-status: ## Print kubectl get across the e2e fixtures.
	$(call container_target,_cluster-status)
_cluster-status:
	bash hack/cluster.sh status

##@ Waiters (use these; never write ad-hoc until/while loops)

WAIT_TIMEOUT ?= 300s

# E2E_KUBECTL — kubectl bound to the KIND cluster, never the host's default context.
#
# Every waiter added here talks to the ephemeral e2e kind cluster, whose
# kubeconfig lives at /workspace/.gocache/kube/config INSIDE the devtools
# container (set by scripts/dev.sh). A bare `kubectl` resolves the HOST
# kubeconfig instead — which on a developer machine is routinely a real
# production cluster. That is a live footgun, not a hypothetical: waiters sit
# next to targets that MUTATE.
#
# Routing through scripts/dev.sh makes the wrong cluster structurally
# unreachable: that kubeconfig only ever knows the kind cluster, so these
# targets fail closed instead of silently addressing prod.
E2E_KUBECTL ?= ./scripts/dev.sh kubectl

# Waiter targets land with the e2e cluster in plan Phase 11. Every one of them
# MUST carry an explicit --timeout (WAIT_TIMEOUT above) and a non-zero exit on
# expiry: no naked `until ...; do sleep N; done` anywhere in this repo.

##@ E2E

.PHONY: e2e-run
e2e-run: ## Task 11.3 — run the full e2e suite (behind the `e2e` build tag) against a running cluster.
	$(call container_target,_e2e-run)
_e2e-run:
	bash hack/cluster.sh check-fresh
	KUBECONFIG=$${KUBECONFIG:-$(PWD)/.gocache/kube/config} go test -tags=e2e -v -count=1 -timeout 15m ./test/e2e/...

.PHONY: e2e-full
e2e-full: ## cluster-up → e2e-run, cluster KEPT for re-runs. Teardown is explicit: `make cluster-down`.
	$(MAKE) cluster-up
	$(MAKE) e2e-run
