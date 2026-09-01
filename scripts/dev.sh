#!/usr/bin/env bash
#
# scripts/dev.sh — run a command inside the devtools container.
#
# Host has no Go toolchain. All `go`, `kubebuilder`, `controller-gen`,
# `kustomize`, `setup-envtest`, `make`, etc. invocations must go through
# this wrapper.
#
# The wrapper:
#   * mounts the repo at /workspace (read-write, host UID:GID preserved)
#   * mounts /var/run/docker.sock so the container can drive the host
#     Docker daemon
#   * adds the docker group so the in-container user can write to the socket
#   * persists Go module and build caches under .gocache/ for fast reruns
#   * leaves ENVTEST_BIN_DIR pointing at the image's pre-baked envtest assets
#   * rebuilds the image whenever Dockerfile.devtools changes
#
# Usage:
#   ./scripts/dev.sh go build ./...
#   ./scripts/dev.sh kubebuilder init --domain ackstorm.ai --multigroup
#   ./scripts/dev.sh make gen-manifests
#   ./scripts/dev.sh bash         # interactive shell inside the container

set -euo pipefail

IMAGE="${SQUALL_DEVTOOLS_IMAGE:-squall-devtools:latest}"
WORKSPACE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# If we are already inside the devtools container, run the command directly.
# Auto-wrapping make targets (Makefile `container_target` macro) re-invoke
# ./scripts/dev.sh from within; this prevents nested containers.
if [[ "${SQUALL_IN_DEVTOOLS:-0}" == "1" ]]; then
    exec "${@:-bash}"
fi

# Host requirement: docker only. Fail fast with a clear message rather than
# letting a downstream `kind`/`helm` call die cryptically.
if ! docker info >/dev/null 2>&1; then
    echo "scripts/dev.sh: docker daemon not reachable — is Docker running and is your user in the docker group?" >&2
    exit 1
fi

# `docker info` alone is not enough: it also succeeds over DOCKER_HOST=tcp://…,
# Docker Desktop's socket and rootless sockets — none of which put a socket at
# /var/run/docker.sock. We bind-mount that exact path below, and docker would
# silently auto-create an EMPTY DIRECTORY there, so the failure would surface
# minutes later from inside kind as "Cannot connect to the Docker daemon".
[[ -S /var/run/docker.sock ]] || {
    echo "scripts/dev.sh: /var/run/docker.sock not found — this wrapper requires the default Docker socket (DOCKER_HOST / rootless / Desktop sockets are unsupported)" >&2
    exit 1
}

# Persisted caches (gitignored). Pre-create so docker doesn't mkdir them as root.
mkdir -p "${WORKSPACE}/.gocache/gopath" \
         "${WORKSPACE}/.gocache/build" \
         "${WORKSPACE}/.gocache/envtest" \
         "${WORKSPACE}/.gocache/kube"

# Take the GID from the socket itself, not from `getent group docker`: the group
# is named `dockerroot` on RHEL/CentOS, absent under rootless Docker, and may be
# anything wherever the socket was chgrp'd. A name lookup silently yields an
# empty group-add on those hosts and the container hits EACCES on the socket.
DOCKER_GID="$(stat -c '%g' /var/run/docker.sock 2>/dev/null || true)"
DOCKER_GROUP_ADD=()
if [[ -n "${DOCKER_GID}" ]]; then
    DOCKER_GROUP_ADD=(--group-add "${DOCKER_GID}")
fi

# TTY only if stdin is a terminal — keeps CI / non-interactive callers working.
TTY_ARGS=()
if [[ -t 0 && -t 1 ]]; then
    TTY_ARGS=(-it)
fi

# Rebuild when Dockerfile.devtools changes, not just when the image is missing.
# The toolchain image IS this repo's build system and will be edited; an
# existence check alone means edits appear to do nothing. The Dockerfile hash is
# stamped as a label and compared — a missing image yields an empty label, so
# this subsumes the existence check.
DF_SHA="$(sha256sum "${WORKSPACE}/Dockerfile.devtools" | cut -d' ' -f1)"
CUR_SHA="$(docker image inspect -f '{{index .Config.Labels "squall.dockerfile.sha"}}' "${IMAGE}" 2>/dev/null || true)"
if [[ "${SQUALL_DEVTOOLS_REBUILD:-0}" == "1" || "${CUR_SHA}" != "${DF_SHA}" ]]; then
    echo "scripts/dev.sh: building ${IMAGE}" >&2
    docker build --label "squall.dockerfile.sha=${DF_SHA}" \
        -t "${IMAGE}" -f "${WORKSPACE}/Dockerfile.devtools" "${WORKSPACE}"
fi

# Worktree gitdir mount: when WORKSPACE is a git worktree, its `.git` is a FILE
# (`gitdir: /path/to/main/.git/worktrees/agent-XXX`), not a directory. The
# referenced gitdir lives OUTSIDE the worktree, so without a second mount any
# `git` invocation inside the container fatals with "not a git repository".
# Ask git for the path rather than parsing the file — the host has git (it lacks
# Go, not git), and git handles paths with spaces and submodule worktrees.
# Mounted read-WRITE on purpose: git writes to $GIT_DIR for far more than
# commits (`git status` rewrites the index, which in a worktree lives under this
# mount), and in the non-worktree case `.git` is already read-write inside
# /workspace anyway.
WORKTREE_GIT_MOUNT=()
if [[ -f "${WORKSPACE}/.git" ]]; then
    MAIN_GIT_DIR="$(git -C "${WORKSPACE}" rev-parse --git-common-dir 2>/dev/null || true)"
    if [[ -d "${MAIN_GIT_DIR}" ]]; then
        WORKTREE_GIT_MOUNT=(-v "${MAIN_GIT_DIR}:${MAIN_GIT_DIR}")
    else
        echo "scripts/dev.sh: ${WORKSPACE}/.git is a worktree pointer but its gitdir could not be resolved — git will not work inside the container" >&2
    fi
fi

# Default command: drop into bash if no args.
if [[ $# -eq 0 ]]; then
    set -- bash
fi

# ENVTEST_BIN_DIR is intentionally NOT passed below: the container inherits the
# image's ENV ENVTEST_BIN_DIR=/opt/envtest so make uses the pre-baked envtest
# assets (Makefile ENVTEST_ASSET_DIR) instead of re-downloading them.
exec docker run --rm "${TTY_ARGS[@]}" \
    --user "$(id -u):$(id -g)" \
    "${DOCKER_GROUP_ADD[@]}" \
    --network=host \
    -v "${WORKSPACE}:/workspace" \
    "${WORKTREE_GIT_MOUNT[@]}" \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -e SQUALL_IN_DEVTOOLS=1 \
    -e DELETE_ON_FAILURE="${DELETE_ON_FAILURE:-0}" \
    -e EXTRA_HELM_VALUES="${EXTRA_HELM_VALUES:-}" \
    -e REGISTRY_USER="${REGISTRY_USER:-}" \
    -e REGISTRY_TOKEN="${REGISTRY_TOKEN:-}" \
    -e HOME=/workspace/.gocache \
    -e GOPATH=/workspace/.gocache/gopath \
    -e GOCACHE=/workspace/.gocache/build \
    -e GOMODCACHE=/workspace/.gocache/gopath/pkg/mod \
    -e KUBECONFIG=/workspace/.gocache/kube/config \
    -e HOST_PWD="${WORKSPACE}" \
    -w /workspace \
    "${IMAGE}" \
    "$@"
