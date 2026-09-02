#!/usr/bin/env bash
#
# hack/cluster.sh — Task 11.1: lifecycle for the canonical kind e2e cluster.
#
# Runs INSIDE the devtools container (invoked via `make cluster-*`, which
# routes through scripts/dev.sh) — never on the host. `docker`, `kind`,
# `kubectl`, `make` and the host Docker socket are all already available in
# that context (see Dockerfile.devtools / scripts/dev.sh).
#
# Squall's phases: namespaces -> model-mock (the engine double) -> a
# development Postgres (test/e2e/cluster/02-postgres, dev-only per
# deploy/helm/squall's values.yaml doctrine) -> the Helm chart (real dstack
# 0.21.2 on that Postgres + squall-operator + squall-proxy) -> fixtures.
# No LiteLLM, no toolhive — those are not squall's concern.
#
# The dstack server here is the REAL one, not a fake. The only thing this
# cluster fakes is the GPU-backed engine itself (cmd/model-mock), which is
# the whole point: we change the BACKEND to Kubernetes so no GPU is billed,
# and everything else is what production runs.
#
# Usage:
#   hack/cluster.sh up        # create the cluster (if absent) and hydrate it
#   hack/cluster.sh hydrate   # (re)build/load images, apply all four phases
#   hack/cluster.sh down      # delete the cluster
#   hack/cluster.sh status    # kubectl get across both namespaces
#   hack/cluster.sh check-fresh  # fail unless the running pods ARE this tree

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-squall-test}"
KIND_CONFIG="${KIND_CONFIG:-hack/kind-config.yaml}"
KUBECONFIG="${KUBECONFIG:-$(pwd)/.gocache/kube/config}"
export KUBECONFIG

CLUSTER_DIR="test/e2e/cluster"
CHART_DIR="deploy/helm/squall"
HELM_RELEASE="${HELM_RELEASE:-squall}"
HELM_VALUES="${HELM_VALUES:-${CLUSTER_DIR}/helm-values.yaml}"
# Optional second -f, for configuration that must NOT live in the repo's e2e
# values: real cloud backends, a live proxy target. It is not a nicety —
# dstack RECONCILES its backends against server/config.yml at every boot and
# DELETES any backend the file does not mention, so a `helm upgrade` that
# forgets this file silently removes vastai and every later provision returns
# zero offers with no error. Measured 2026-08-27.
EXTRA_HELM_VALUES="${EXTRA_HELM_VALUES:-}"
# Pinned, and it must match deploy/helm/squall/values.yaml's dstack.image.tag:
# every measured fact the chart's dstack wiring relies on was measured on
# exactly this version. See docs/references/dstack-real-api.md.
DSTACK_IMAGE="dstackai/dstack:0.21.2"
# Backs both the dev Postgres deployment (02-postgres) and the dstack Pod's
# wait-for-postgres init container — pulled and kind-loaded once, same as
# DSTACK_IMAGE, so the cluster never reaches the network during a test run.
POSTGRES_IMAGE="postgres:16-alpine"
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-180s}"

# The four Deployments built from this tree, as "namespace/name".
# The Deployments built from THIS tree, as "namespace/name". squall-system/dstack
# is deliberately absent: it runs an upstream image that no tree change can
# invalidate, and it is stateful — stamping it would restart the server, and
# its database, on every unrelated Go edit.
E2E_DEPLOYMENTS=(
    "squall/model-mock"
    "squall-system/squall-operator"
    "squall-system/squall-proxy"
)

# Which image each of those Deployments runs. Needed because the build stamp
# alone is a SELF-ASSERTION: stamp_deployments patches the annotation whether
# or not the image load actually landed in the node, so a green check-fresh can
# sit on top of a stale binary. Comparing image IDs is the check that cannot
# lie. Measured the hard way on 2026-08-27: a failed `docker cp` into the node
# left the old controller running while every other signal said fresh.
declare -A E2E_IMAGES=(
    ["squall/model-mock"]="model-mock:e2e"
    ["squall-system/squall-operator"]="squall-controller:e2e"
    ["squall-system/squall-proxy"]="squall-proxy:e2e"
)

BUILD_STAMP_ANNOTATION="squall.ackstorm.ai/build-stamp"

# build_stamp hashes every input that ends up inside the four images. The
# image tag is the mutable ':e2e', so `kubectl apply` sees a byte-identical
# Deployment after a rebuild and changes nothing — the pods keep running the
# PREVIOUS binary and e2e reports a verdict on code that is not in the tree.
# That has now cost real time twice. Stamping the pod template with this hash
# makes the rollout content-addressed: it restarts when, and only when, the
# code actually changed.
build_stamp() {
    {
        find cmd internal api -type f -name '*.go' -print0 | sort -z | xargs -0 sha1sum
        sha1sum go.mod go.sum
    } | sha1sum | cut -c1-12
}

# verify_image_in_node fails unless the image the node will actually run is
# byte-identical to the one just built on the host. `kind load` reports success
# on paths that do not end with the node holding the new bytes, and every one
# of the four times this trap fired, the load — not the build — was what
# silently did nothing.
verify_image_in_node() {
    local img="$1" host_id node_id
    host_id="$(docker image inspect "${img}" --format '{{.Id}}' 2>/dev/null || true)"
    if [ -z "${host_id}" ]; then
        echo "hack/cluster.sh: ${img} was never built on the host" >&2
        return 1
    fi
    node_id="$(docker exec "${CLUSTER_NAME}-control-plane" \
        crictl inspecti "${img}" -o json 2>/dev/null \
        | sed -n 's/.*"id": "\(sha256:[0-9a-f]*\)".*/\1/p' | head -1 || true)"
    if [ "${host_id}" != "${node_id}" ]; then
        echo "hack/cluster.sh: ${img} in the node is '${node_id:-<absent>}'," >&2
        echo "                 but this host just built '${host_id}'." >&2
        echo "                 The image load did NOT take effect. Do not trust any" >&2
        echo "                 result from this cluster." >&2
        return 1
    fi
    return 0
}

stamp_deployments() {
    local stamp="$1" ns name
    for d in "${E2E_DEPLOYMENTS[@]}"; do
        ns="${d%%/*}"; name="${d##*/}"
        kubectl patch "deployment/${name}" -n "${ns}" \
            -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"${BUILD_STAMP_ANNOTATION}\":\"${stamp}\"}}}}}" >/dev/null
    done
}

# cmd_check_fresh is the guard on `make e2e-run`: refuse to report a verdict
# on stale pods.
cmd_check_fresh() {
    local want ns name got stale=0
    want="$(build_stamp)"
    for d in "${E2E_DEPLOYMENTS[@]}"; do
        ns="${d%%/*}"; name="${d##*/}"
        got="$(kubectl get "deployment/${name}" -n "${ns}" \
            -o "jsonpath={.spec.template.metadata.annotations.${BUILD_STAMP_ANNOTATION//./\\.}}" 2>/dev/null || true)"
        if [ "${got}" != "${want}" ]; then
            echo "hack/cluster.sh: ${ns}/${name} runs build '${got:-<none>}', tree is '${want}'" >&2
            stale=1
        fi
        # The stamp is the Deployment agreeing with itself. This is the part
        # that cannot be faked: the bytes in the node vs the bytes just built.
        verify_image_in_node "${E2E_IMAGES[$d]}" || stale=1
    done
    if [ "${stale}" -ne 0 ]; then
        echo "hack/cluster.sh: the cluster is running STALE binaries — run 'make cluster-hydrate' (or 'make e2e-full')" >&2
        exit 1
    fi
    echo "hack/cluster.sh: cluster is running build ${want}"
}

cluster_exists() {
    kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"
}

cmd_up() {
    if cluster_exists; then
        echo "hack/cluster.sh: kind cluster '${CLUSTER_NAME}' already exists"
        kind export kubeconfig --name "${CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}"
    else
        echo "hack/cluster.sh: creating kind cluster '${CLUSTER_NAME}'"
        kind create cluster --name "${CLUSTER_NAME}" --config "${KIND_CONFIG}" --kubeconfig "${KUBECONFIG}"
    fi
    cmd_hydrate
}

cmd_down() {
    kind delete cluster --name "${CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}"
}

build_and_load_images() {
    # Reuses the Makefile's build-image-* targets (single source of truth for
    # the docker build invocation) rather than duplicating it here.
    make build-image-controller build-image-proxy build-image-model-mock
    kind load docker-image squall-controller:e2e --name "${CLUSTER_NAME}"
    kind load docker-image squall-proxy:e2e --name "${CLUSTER_NAME}"
    kind load docker-image model-mock:e2e --name "${CLUSTER_NAME}"

    # Fail HERE, not four steps later in a test whose verdict is meaningless.
    local failed=0
    for img in squall-controller:e2e squall-proxy:e2e model-mock:e2e; do
        verify_image_in_node "${img}" || failed=1
    done
    [ "${failed}" -eq 0 ] || exit 1

    # dstack is pulled once and loaded, so the cluster never reaches the
    # network during a test run.
    docker image inspect "${DSTACK_IMAGE}" >/dev/null 2>&1 || docker pull "${DSTACK_IMAGE}"
    kind load docker-image "${DSTACK_IMAGE}" --name "${CLUSTER_NAME}"

    docker image inspect "${POSTGRES_IMAGE}" >/dev/null 2>&1 || docker pull "${POSTGRES_IMAGE}"
    kind load docker-image "${POSTGRES_IMAGE}" --name "${CLUSTER_NAME}"
}

# wait_for_operator_leadership blocks until squall-operator actually HOLDS its
# leader lease, which `rollout status` above does not prove.
#
# rollout status proves the Pod is READY. controller-runtime then does nothing
# at all until it wins the lease, and that gap was MEASURED at 30s on
# 2026-09-01 (pod created 09:46:05, workers started 09:46:35). The e2e suite
# gives a wake 15 seconds, so it was asserting against a controller that had
# not started its workers yet -- the same assertions pass by hand once the
# cluster settles. The failure looked like a squall bug and was a harness race.
#
# Bounded with an explicit failure path: no naked polling loop.
wait_for_operator_leadership() {
    local i holder pod del
    for i in $(seq 1 90); do
        # The holder is "<pod-name>_<uuid>". Read it FIRST and validate that
        # pod, rather than matching against a pod LIST: during a rollout the
        # list contains the outgoing pod too, and it is still the lease holder
        # for a moment -- so matching the list returns while the OLD binary is
        # still the one reconciling, which is exactly the race this is here to
        # remove.
        holder="$(kubectl get leases -n squall-system \
            -o jsonpath='{.items[*].spec.holderIdentity}' 2>/dev/null || true)"
        pod="${holder%%_*}"
        case "${pod}" in
            squall-operator-*)
                del="$(kubectl get pod "${pod}" -n squall-system \
                    -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || echo missing)"
                if [ -z "${del}" ]; then
                    echo "hack/cluster.sh: squall-operator is leading after ${i}s (${pod})"
                    return 0
                fi
                ;;
        esac
        sleep 1
    done
    echo "hack/cluster.sh: squall-operator never acquired its leader lease within 90s" >&2
    return 1
}

cmd_hydrate() {
    if ! cluster_exists; then
        echo "hack/cluster.sh: no kind cluster '${CLUSTER_NAME}' — run 'up' first" >&2
        exit 1
    fi

    build_and_load_images

    kubectl apply -k "${CLUSTER_DIR}/00-namespaces"
    kubectl apply -k "${CLUSTER_DIR}/01-model-mock"

    # squall-system normally comes into being via `helm --create-namespace`
    # below, but 02-postgres must land in it BEFORE that install so dstack's
    # wait-for-postgres init container has something to wait for rather than
    # racing it. `kubectl create ... --dry-run=client | apply` is idempotent —
    # safe to run on every hydrate, and --create-namespace is a no-op once
    # the namespace already exists.
    kubectl create namespace squall-system --dry-run=client -o yaml | kubectl apply -f -
    kubectl apply -k "${CLUSTER_DIR}/02-postgres"
    # Bounded wait via kubectl's own --timeout (not a hand-rolled poll loop):
    # fails loudly if the dev Postgres itself never comes up, instead of
    # letting the helm install below time out 60s later inside dstack's own
    # wait-for-postgres init container with a much less obvious symptom.
    kubectl rollout status deployment/squall-postgres -n squall-system --timeout=90s

    # --install so hydrate is idempotent; CRDs are applied separately because
    # helm only installs from crds/ on FIRST install and never upgrades them,
    # which would silently pin an old CRD across a schema change.
    kubectl apply -f "${CHART_DIR}/crds"
    helm upgrade --install "${HELM_RELEASE}" "${CHART_DIR}" \
        --namespace squall-system --create-namespace \
        --values "${HELM_VALUES}" \
        ${EXTRA_HELM_VALUES:+--values "${EXTRA_HELM_VALUES}"} \
        --wait --timeout "${ROLLOUT_TIMEOUT}"

    # Stamp BEFORE waiting: the patch is what triggers the rollout when the
    # code changed, so the waits below must observe the post-patch generation.
    stamp_deployments "$(build_stamp)"

    kubectl rollout status deployment/model-mock -n squall --timeout="${ROLLOUT_TIMEOUT}"
    kubectl rollout status deployment/squall-operator -n squall-system --timeout="${ROLLOUT_TIMEOUT}"
    wait_for_operator_leadership
    kubectl rollout status deployment/squall-proxy -n squall-system --timeout="${ROLLOUT_TIMEOUT}"

    kubectl apply -k "${CLUSTER_DIR}/03-fixtures"

    cmd_status
}

cmd_status() {
    kubectl get namespaces -l e2e=true
    kubectl get all -n squall
    kubectl get all -n squall-system
    kubectl get all -n squall-runs
    kubectl get models -n squall -o wide
}

case "${1:-}" in
    up) cmd_up ;;
    hydrate) cmd_hydrate ;;
    down) cmd_down ;;
    status) cmd_status ;;
    check-fresh) cmd_check_fresh ;;
    *)
        echo "usage: hack/cluster.sh {up|hydrate|down|status|check-fresh}" >&2
        exit 1
        ;;
esac
