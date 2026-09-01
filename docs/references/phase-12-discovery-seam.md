# Phase 12 — discovery seam

Task 12.1/12.2/12.3 of the final block. Per F30, no new operator code is needed anywhere in
this repo: the discovery seam is entirely a CR (owned here) plus alitellm-operator's existing
`type: kubeai` provider (owned in the sister repo). This file records what was built, what was
proven, and what is honestly still a drill rather than an executed test.

## 12.1 — the `LiteLLMModelDiscovery` CR

`test/e2e/cluster/04-discovery/discovery.yaml` (+ `kustomization.yaml`) is the deliverable — a
version-controlled, directly `kubectl apply -k`-able CR, deliberately **not** wired into
`hack/cluster.sh`'s default `00-03` hydrate sequence (see below for why).

Key choices, all commented inline in the file itself:

- **`spec.type: kubeai`** — the provider F27/F30 says already understands an OpenAI-compatible
  `/v1/models` listing; no new provider code.
- **`spec.prefix: squall`** — distinct from the default `kubeai` prefix so generated child
  `Model` names never collide with an in-cluster `kubeai` discovery source's children (F27
  notes ten of those already exist in production).
- **`spec.baseUrl: http://proxy.squall-system.svc:8080/v1`** — squall-proxy's real,
  actually-deployed Service address in this repo's own kind cluster. **This deviates from the
  spec's own illustrative example**, `http://squall-proxy.squall.svc/v1` (service name
  `squall-proxy`, namespace `squall`, no port). Confirmed live via `kubectl get svc -n
  squall-system` that this repo's proxy Service is named plainly `proxy`, lives in
  `squall-system`, and listens on `8080` (`cmd/proxy/main.go`'s `SQUALL_PROXY_ADDR` default).
  Using the spec's literal string would not resolve against this cluster. This ties directly
  into the still-**OPEN** ledger item **D6** ("which Service's Endpoints to enumerate... final
  naming waits on the Helm skeleton") — the CR's `baseUrl` has exactly the same
  not-yet-settled-naming problem D6 already tracks, so no new ledger entry was opened for it;
  the discrepancy is called out inline in `discovery.yaml`'s own header comment instead.
- **`spec.params.timeout` / `stream_timeout: 3600`** — the external router profile F28/F32
  require: provisioning-budget magnitude, **not** the in-cluster default (`300`/`60`), and
  comfortably above this repo's example `holdTimeout` (20m) and `provisioningTimeout` (45m,
  see `config/samples/squall_v1alpha1_model.yaml`), satisfying §7's ordering rule (`holdTimeout
  <= provisioningTimeout <= external router timeout`). `params` is documented as a verbatim
  pass-through bag (F30): every field lands unmodified on every generated child's
  `litellm_params`.
- **`spec.refresh.interval: 5m`** — arbitrary but reasonable; nothing in F27-F32 mandates a
  specific value.

Validated (both checks passed, exactly as expected):

```
./scripts/dev.sh kubectl kustomize test/e2e/cluster/04-discovery
```
builds valid YAML.

```
./scripts/dev.sh kubectl apply -k test/e2e/cluster/04-discovery --dry-run=client
```
fails with:
```
no matches for kind "LiteLLMModelDiscovery" in version "litellm.ackstorm.ai/v1alpha1",
ensure CRDs are installed first
```

That failure is the point, not a bug — see 12.2 below.

## 12.2 — fail-closed proof

**Not executed. Documented as a drill instead**, per the task's explicit fallback instruction.

This repo's live kind cluster (`squall-test`) has **no `litellm.ackstorm.ai` CRDs and no
alitellm-operator installed** — confirmed both by `kubectl get crds | grep litellm` (empty) and
by the `--dry-run=client` failure above. Installing the sibling operator solely to exercise
this one CR is explicitly out of scope for this block (task instructions: "do NOT install the
sibling operator"), and there is no way to fake `LiteLLMModelDiscovery`'s reconciliation
without it — its "deregisters nothing on an unreachable source" behavior (F31) lives entirely
in `alitellm-operator`'s own controller, not in anything this repo owns or can stub.

**The drill, if a cluster with alitellm-operator were available:**

1. Install alitellm-operator and its `litellm.ackstorm.ai` CRDs into the cluster (out of scope
   here, but this is the only missing precondition).
2. `./scripts/dev.sh kubectl apply -k test/e2e/cluster/04-discovery` — applies the CR built in
   12.1 against the running `squall-proxy` from this repo's own e2e cluster.
3. Wait for alitellm-operator's discovery reconciliation to complete (its own refresh interval
   or CR status), then confirm at least one generated `Model` CR/LiteLLM config entry exists
   with the `squall` prefix, and that its `litellm_params` carries `timeout: 3600,
   stream_timeout: 3600` (the pass-through in action, F30).
4. Kill the proxy: `kubectl -n squall-system scale deployment proxy --replicas=0` (or delete
   the Pod directly).
5. Wait at least one `refresh.interval` (5m here) past the kill, so a real refresh cycle is
   guaranteed to have run against the now-unreachable `baseUrl`.
6. Assert F31's two halves:
   - `LiteLLMModelDiscovery`'s own `status` records the fetch failure (whatever field
     alitellm-operator uses for that — see `api/litellm/v1alpha1/modeldiscovery_types.go` in
     the sister repo) rather than silently reporting success.
   - The `squall`-prefixed children from step 3 **still exist, unchanged** — no
     enumeration/diff/deletion ran against them. This is the fail-closed assertion: an
     unreachable source must never be treated as "zero models observed, therefore deregister
     everything."
7. Scale the proxy back up, confirm a subsequent refresh cycle recovers cleanly (not required
   by F31 itself, but a reasonable sanity check that the fail-closed state is not sticky).

No part of this drill was run. Nothing was faked to produce a passing result.

## 12.3 — AC sweep

See `docs/references/ac-sweep-v0_1.md` for the full AC1-19 table. Summary: 7 fully proven by
test (AC3, AC4, AC6, AC13, AC16, AC17, AC19), 3 partly proven by test / partly deferred (AC5,
AC8, AC11), 1 documented as a drill rather than executed (AC18), and 8 deferred outright (AC1,
AC2, AC7, AC9, AC10, AC12, AC14, AC15) — mostly real-hardware PoCs and §8 engine-template work
this block was explicitly told not to touch, plus one genuine new gap (AC12, logged as **D37**).
