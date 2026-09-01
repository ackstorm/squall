# Helm chart + dstack in-cluster — Implementation Brief

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make the Helm chart the real deployment artifact for Squall — controller, proxy, RBAC **and a real dstack server** — and make the kind e2e cluster install exactly that chart, so the test environment is the production shape rather than a bespoke overlay full of doubles.

**Architecture:** `deploy/helm/squall` grows from a CRD-only chart into the full deployment. `test/e2e/cluster/` stops being a parallel kustomize universe and becomes a thin values overlay on the chart. dstack runs **inside** the cluster, configured with the Kubernetes backend pointed at the very cluster it runs in, so a wake provisions a real pod instead of a fake reporting one.

**Tech Stack:** Helm 3 (in the devtools container), kind, `dstackai/dstack:0.21.2`.

**This brief runs IN PARALLEL with** `docs/plans/2026-08-27-real-dstack-client-and-tier1-e2e.md`. File ownership is disjoint and **must stay disjoint**:

| Owner | Tree |
|---|---|
| **This brief** | `deploy/helm/**`, `test/e2e/cluster/**`, `hack/cluster.sh`, the `Makefile`'s cluster/helm/e2e targets |
| The dstack-client plan | `internal/**`, `cmd/**`, `api/**`, `test/e2e/*_test.go` |

Never `git add -A`. Never `git reset`/`amend`/rebase past a commit you did not create. Only add commits.

---

## What exists today — verified, not assumed

**The chart is CRD-only.** Three files, no templates:

```
deploy/helm/squall/Chart.yaml
deploy/helm/squall/crds/squall.ackstorm.ai_models.yaml
deploy/helm/squall/values.yaml      # deliberately empty
```

`Chart.yaml` says so explicitly: *"This chart ships the Model CRD only — Phase 10.2 scoped the drift gate to the exact failure mode the plan names (a stale CRD shipped to users), not full manifest templating."*

**The real deployment is kustomize**, in two places:

- `config/` — `crd/`, `default/`, `manager/`, `proxy/`, `rbac/`, `network-policy/`, `prometheus/`, `samples/`. `config/proxy/` already has the proxy's Deployment, Service, Role, RoleBinding and ServiceAccount. `config/rbac/` has the controller's plus the leader-election, metrics and the three user-facing `squall_model_{admin,editor,viewer}` roles.
- `test/e2e/cluster/` — four numbered overlays: `00-namespaces`, `01-fake-dstack` (which also carries `model-mock`), `02-operator` (patches over `config/`), `03-fixtures`, `04-discovery`.

**Existing gates you must not break:**

- `make helm-sync` copies `config/crd/bases/*.yaml` into `deploy/helm/squall/crds/`.
- `make helm-sync-check` fails CI on drift. It exists because the sibling project shipped stale CRDs to users **twice**. Whatever you restructure, this gate must still catch a changed CRD field that was not re-synced.
- `hack/cluster.sh` stamps every e2e Deployment with `squall.ackstorm.ai/build-stamp` (a hash of the Go sources) and `make e2e-run` refuses to start when the running stamp differs from the tree. That guard exists because e2e twice reported verdicts on stale binaries. **Any Deployment you add must be stamped too**, or it silently reintroduces the bug.

---

## The measured facts about running dstack in Kubernetes

All of these were measured on 2026-08-27 against a real `dstackai/dstack:0.21.2` driving the kind cluster. Full detail with request/response in `docs/references/dstack-real-api.md` §9. **Do not re-derive these from documentation — the docs do not say most of them.**

### 1. dstack has NO in-cluster config support

Verified by reading `core/backends/kubernetes/utils.py`: the backend loads a kubeconfig from `kubeconfig.filename` or `kubeconfig.data` and nothing else. There is no `load_incluster_config` path anywhere in that package.

**So even running as a Pod, dstack needs a kubeconfig file.** Build one from a ServiceAccount token and ship it as a Secret, pointing at `https://kubernetes.default.svc`. Do not expect the ServiceAccount to be picked up automatically.

### 2. The namespace comes from the kubeconfig context

`Cluster.namespace` is taken from the kubeconfig context's `namespace` when `contexts:` is used. The `namespace` property on the backend config is deprecated and logs a warning.

Set the namespace **in the kubeconfig context** — e.g. `squall-runs` — so dstack's pods land in a namespace we own. Measured: with no namespace set, the run pods appeared in `default`, alongside `dstack-main-ssh-jump-pod`.

### 3. `proxy_jump.hostname` must be set explicitly

kind nodes have no `ExternalIP`, and dstack fails provisioning when it cannot autodetect one. Measured working config:

```yaml
projects:
- name: main
  backends:
  - type: kubernetes
    kubeconfig:
      filename: /root/.dstack/kubeconfig
    contexts:
    - name: kind-squall-test
      proxy_jump:
        hostname: 172.20.0.2    # the kind node's InternalIP — resolve at install time
        port: 32000             # pinned, inside kind's NodePort range
```

`hack/cluster.sh` should resolve the node IP and pass it as a Helm value:

```bash
node_ip="$(kubectl get node "${CLUSTER_NAME}-control-plane" \
    -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
[ -n "${node_ip}" ] || { echo "cluster.sh: could not resolve the kind node IP" >&2; exit 1; }
```

dstack creates the jump pod itself (`dstack-main-ssh-jump-pod` was observed); you do not deploy it.

### 4. A fleet is REQUIRED, and on Kubernetes it must have `nodes` target 0

This is the one that will waste your afternoon if you skip it. A run with no matching fleet dies with:

```
failed_to_start_due_to_no_capacity
"No matching fleet found."
```

`KubernetesCompute` does not implement `ComputeWithCreateInstanceSupport`, so an ordinary fleet gets `NO_OFFERS` and terminates. `server/services/fleets.py` only admits such backends when `nodes.target == 0`:

```python
include_only_create_instance_supported_backends = nodes.target != 0
```

Measured working fleet:

```yaml
type: fleet
name: kind-fleet
nodes: 0..2
backends: [kubernetes]
resources:
  cpu: 1..
  memory: 512MB..
  disk: 10GB..
```

With `nodes: 1` it terminated. With `nodes: 0..2` the fleet went `active` and the service provisioned immediately. **The chart must create this fleet** — a Job or init container that applies it after the server is up. It is not optional setup; nothing runs without it.

### 5. RBAC dstack needs

ClusterRole: `get`,`create` on `namespaces`; `get`,`list` on `nodes`; `list` on `pods`.
Namespaced Role (in the runs namespace): `get`,`watch`,`create`,`delete` on `pods`, plus `services`, `secrets`, `persistentvolumeclaims`.

Grant exactly these. **D29 was an RBAC verb-set bug** — `endpoints` had `get` but not `list;watch`, which silently disabled §6's sleep in every real deployment, and no test caught it because envtest does not enforce RBAC. e2e is the only layer that can. If you narrow from cluster-admin (and you should), prove it by running the wake and watching a pod appear, not by reading the manifest.

### 6. Secrets: dstack stores the admin token and an SSH private key UNENCRYPTED by default

Measured in the server's sqlite DB: `projects.ssh_private_key` is a plaintext PEM, and `users.token` reads `enc:identity:noname:<token>` — the `identity` codec, meaning no encryption, just a prefix declaring it. dstack supports an `encryption:` block in `server/config.yml`; without it nothing is encrypted.

Also: **the server prints the admin token to stdout on every start** (F25, confirmed live).

For the e2e chart this is acceptable with a fixed, obviously-fake token, but it must be **obviously fake** (`e2e-local-token`) and never a value that could be mistaken for a real one. For any non-e2e values file, `encryption:` is mandatory and the DB volume must be treated as a secret. Note this in the chart's values documentation so the next person deploying it does not learn it the hard way.

### 7. Timing, measured

nginx on CPU: ~65s from apply to the service proxy answering 200. Size e2e waits accordingly — and remember §5 below: it answers **404** for that whole minute.

---

## Tasks

### Task 1: Grow the chart into the real deployment

- [ ] **Step 1: Move the deployment manifests into templates**

Port `config/manager/`, `config/proxy/` and `config/rbac/` into `deploy/helm/squall/templates/`, parameterised through `values.yaml`: image repository/tag/pullPolicy per binary, replica counts, resource requests, the namespace, and the env vars the two binaries already read (`SQUALL_NAMESPACE`, `SQUALL_DEMAND_COOLDOWN`, `SQUALL_DEMAND_REFRESH_INTERVAL`, `SQUALL_MAX_PENDING_PER_MODEL`, `SQUALL_PROXY_ADDR`, `SQUALL_BACKEND_URL_TEMPLATE`, `SQUALL_IDLE_REQUEUE_INTERVAL`).

Keep `crds/` exactly where it is. Helm installs `crds/` unconditionally before templates, which is the behaviour the drift gate depends on.

- [ ] **Step 2: Keep `helm-sync-check` meaningful**

Re-run `make helm-sync-check` and confirm it still fails when a CRD field changes without a re-sync. Prove it: add a field to a type, run the gate, watch it fail, revert.

- [ ] **Step 3: Commit**

```bash
git add deploy/helm/squall
git commit -m "feat(helm): template the controller, proxy and RBAC"
```

### Task 2: Add dstack to the chart

- [ ] **Step 1: The server**

A Deployment (`dstackai/dstack:0.21.2`, **pinned** — every measured fact above is from that exact version), a Service on 3000, a PVC for `/root/.dstack/server`, and a ConfigMap holding `config.yml` templated from values (`dstack.proxyJump.hostname`, `.port`, the context name, the runs namespace).

- [ ] **Step 2: The kubeconfig Secret**

A ServiceAccount + the RBAC from §5, and a kubeconfig Secret built from that ServiceAccount's token, `server: https://kubernetes.default.svc`, and a context whose `namespace:` is the runs namespace. Mount it where `config.yml`'s `kubeconfig.filename` points.

- [ ] **Step 3: The fleet**

A post-install Job that waits for the server and applies the `nodes: 0..2` fleet from §4. It must be idempotent — Helm upgrades re-run it — and it must **fail loudly** if the fleet does not reach `active`, with a bounded wait and a non-zero exit. No naked polling loops: bounded retries with an explicit failure path.

- [ ] **Step 4: Prove it provisions**

Install the chart into kind, then apply a service run through dstack and confirm a pod appears:

```bash
kubectl get pods -n <runs-namespace>
# expect a dstack-main-<run>-0-0-xxxx pod AND dstack-main-ssh-jump-pod
```

Do **not** move on until you have seen a real pod. Every fact in this brief was earned that way.

- [ ] **Step 5: Commit**

```bash
git add deploy/helm/squall
git commit -m "feat(helm): run a real dstack server with the Kubernetes backend"
```

### Task 3: The e2e cluster installs the chart

- [ ] **Step 1: Replace the overlays with a values file**

`hack/cluster.sh` should `helm upgrade --install` the chart with `test/e2e/cluster/values-e2e.yaml` (e2e image tags, the resolved node IP, the fake token) instead of applying `01-fake-dstack` and `02-operator`.

- [ ] **Step 2: Delete `fake-dstack` from the cluster**

`test/e2e/cluster/01-fake-dstack/` goes away. `cmd/fake-dstack` and `internal/dstack/mock` **stay in the repo** — they remain the in-process double for unit and envtest, where a real control plane is not allowed. What ends is the fake being the thing e2e runs against.

`model-mock` also moves: it is what dstack **provisions** as the model image, not a pre-deployed Deployment. Ledger follow-up #1 in `decisions-and-open-items.md` already called for this.

- [ ] **Step 3: Keep the build stamp covering everything**

Add the dstack Deployment to `E2E_DEPLOYMENTS` in `hack/cluster.sh`, or make the stamping walk the chart's Deployments. `make e2e-run` must still refuse to run against stale pods.

- [ ] **Step 4: `make e2e-full` green**

Run it. The suite still uses the old assertions at this point; if a spec fails because the environment is now real rather than faked, **that is a finding** — log it in `docs/references/deviations-and-findings.md` and report it. Do not weaken an assertion to make it pass. (An `Eventually` timeout was widened once for a phantom cause; the real cause was stale pods. See D40.)

- [ ] **Step 5: Commit**

```bash
git add hack/cluster.sh Makefile test/e2e/cluster
git commit -m "build(e2e): install the Helm chart instead of bespoke overlays"
```

---

## What to report back

1. Whether the chart provisions a real pod through dstack, with the `kubectl get pods` output.
2. The narrowed RBAC you settled on, and how you proved it sufficient.
3. Anything measured that contradicts §1-§7 above — those are measurements from one version on one cluster, and a contradiction is worth more than a green test.
4. Every deviation appended to `docs/references/deviations-and-findings.md`. Do not renumber or delete entries; append only.

---

## Running concurrently with the other plan — read this before your first commit

Two agents once ran concurrently on this repo and one destroyed the other's work. These are not
suggestions.

- **Never `git add -A`.** Name every path you commit. The tree contains another agent's in-flight
  files.
- **Never `git reset`, `git commit --amend`, or rebase past a commit you did not create.** Only
  add commits.
- **Never `git checkout -- <file>` or `git restore` while the tree is dirty.** Copy to a temp path
  and edit forward.
- **Stay inside your named tree.** If you believe you must touch a file the other plan owns, stop
  and report it instead — that is a coordination decision, not an implementation one.

### The one shared file: the ledger

`docs/references/deviations-and-findings.md` is appended by both agents and **will** conflict.
Rules: append only, never renumber, never delete an entry. On a conflict, keep **both** sides —
the entries are independent observations, not competing versions. If two agents pick the same
`D<n>`, the second one to land renumbers **its own** entry upward and says so in the commit
message.

### Seams to leave alone until coordinated

- `SQUALL_BACKEND_URL_TEMPLATE` — the proxy's forward target. Measured, dstack exposes
  `service.url` = `/proxy/services/{project}/{run}/`, so this template is destined to be replaced
  (ledger D25). **Neither plan does that rewiring.** Whoever needs it first must raise it, not
  quietly change it, or the other agent's environment stops forwarding.
- `cmd/fake-dstack` and `internal/dstack/mock` **stay in the repo.** They are the in-process
  double for unit and envtest, where no control plane is allowed. What ends is the fake being what
  e2e runs against. Do not delete them.
- `test/e2e/*_test.go` (the Go specs) belong to the dstack-client plan; `test/e2e/cluster/**` (the
  environment) belongs to the Helm brief. Same directory tree, different owners — check which
  before editing.
