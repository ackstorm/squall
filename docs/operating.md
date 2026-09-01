# Operating Squall

Everything below was measured, most of it the hard way. The README covers what squall is and
how to install it; this file covers what bites you afterwards.

## The direct SSH path

`squall-proxy` prefers its own SSH tunnel to the replica over dstack's service proxy. The
throughput difference and the reason for it are in the README. This is how it works.

### How it authenticates

Squall never touches dstack's project private key. dstack's Vast.ai backend builds the
container's `authorized_keys` from **both** `run_spec.ssh_key_pub` and its own project key
(`core/backends/vastai/compute.py`), and squall is what builds the run spec. So:

1. `squall-controller` mints an ed25519 keypair into the Secret `squall-replica-ssh-key`.
2. It sends the public half on every Apply.
3. `squall-proxy` reads the private half and dials the replica.
4. Host keys are pinned trust-on-first-use; a changed key is refused.

**`authorized_keys` is written at container creation.** A replica provisioned before the key
existed will never accept it — the tunnel is refused and traffic falls back to dstack's
proxy. It starts working on the next wake, with no intervention. The same applies to key
rotation: it costs a re-provision.

Everything about the direct path fails soft. No published endpoint, a topology needing more
than one SSH hop (Kubernetes' jump pod, `dockerized` backends), a missing key, a refused dial
— each falls through to dstack's proxy. Enabling it cannot fail a request that worked before.

### Verifying which path a request took

```bash
kubectl -n squall get model <name> -o jsonpath='{.status.replica}'   # empty = fallback
kubectl -n squall-system logs -l app.kubernetes.io/component=proxy | grep 'ssh tunnel'
```

`ssh tunnel to replica established` means direct. `ssh tunnel to replica unavailable, using
dstack proxy` names the reason and is not an error.

## Serving limits worth knowing before you load-test

### dstack's upstream timeout applies only to the fallback path

Its default is 60s, copied from nginx, and it bounds the wait for the upstream's *response
headers* — which for a non-streaming completion means the whole generation plus anything
queued ahead of it. Measured: `max_tokens: 3000` returned 200 at 59.9s, `4000` returned
`504 {"detail":"Timed out requesting upstream"}` at 60.02s.

The chart sets `dstack.serviceClientTimeoutSeconds: 600`. It **cannot** be set per request —
dstack reads it once at import into a module-level constant shared by every service.

Streaming was never affected: the timeout is applied per read, not per request.

### Reasoning models can spend your entire output budget thinking

Measured on Qwen3.8-27B with `max_tokens: 1500`:

| `reasoning_effort` | completion | reasoning | **answer** |
|---|---|---|---|
| *(omitted)* | 1500 | 1500 | **0** |
| `low` | 924 | 327 | 597 |
| `medium` | 931 | 340 | 591 |
| `xhigh` | 1500 | 1500 | **0** |

Omitting the field is identical to `xhigh`, and at `xhigh` the model emitted **zero** answer
tokens — not truncated output, no output. Send `reasoning_effort: "medium"` explicitly. It is
a per-request field and needs no server change. Valid values are `xhigh | medium | low`;
`high` is not one of them.

### There is no admission control once a model is Ready

`maxPendingPerModel` bounds requests being *held* during a cold start. Once the model is
Ready, the proxy forwards unlimited concurrency, so aggregate throughput divides across
however many streams arrive. Measured over 14h: ~900-1,200 tok/s split across ~57 concurrent
streams is ~16-21 tok/s each, so any request wanting more than ~4,800 tokens could not finish
inside a 300s client timeout. The GPU was not the queue — vLLM reported `Waiting: 0 reqs`
throughout.

Capacity planning is the caller's job in 0.1.0. Size concurrency against the tokens/second
one replica actually delivers, not against the number of requests it will accept.

## Two deployment traps that will cost you an hour

Both cost exactly that on 2026-08-28. Neither produces an error message.

### Helm does not upgrade CRDs

Files under a chart's `crds/` directory are applied on install and **never** on upgrade. A
new status field therefore does not exist in the cluster's CRD, and the API server silently
prunes it — the controller writes it, nothing rejects the write, and the field is simply
absent. `make helm-sync` keeps the chart's copy correct, which helps a fresh install and
nothing else. After changing the CRD:

```bash
kubectl apply --server-side --force-conflicts \
  -f config/crd/bases/squall.ackstorm.ai_models.yaml
```

The CRD also ships as an asset on every GitHub Release, so an operator upgrading from a
published chart does not need this repository.

### A rebuilt image does not reach a running Pod

With a mutable tag (`:e2e`) and `imagePullPolicy: Never`, `kind load` replaces the image on
the node but changes nothing for a Pod already running, and `helm upgrade` only recreates
Pods when something in the Pod spec actually changed. Symptom: new code that appears to do
nothing at all. Always:

```bash
kubectl -n squall-system rollout restart deploy/squall-operator deploy/squall-proxy
```

Check what is actually running before diagnosing anything else — compare `.status.startTime`
against when the image was built.

### Ready is not the same as leading

`kubectl rollout status` proves the operator Pod is *Ready*. It does not prove the operator
is *reconciling*: controller-runtime does nothing until it wins its leader lease, and the gap
was measured at 30s. Anything that asserts on reconciliation immediately after a rollout is
racing. `hack/cluster.sh`'s `wait_for_operator_leadership` waits for the lease holder
instead, and reads `holderIdentity` rather than matching a Pod list — during a rollout the
list still contains the outgoing Pod, which is still the holder for a moment.
