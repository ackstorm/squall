# Code review — `feat/v01-harness-and-scaffolding`

**Date:** 2026-08-31
**Scope:** `git diff main...HEAD` — 183 commits, 215 files, ~38k insertions
(~9.2k lines of production Go). `main` is nearly empty, so the branch is the
whole project.
**Method:** recall-oriented max-effort review. 10 independent finder angles
(5 correctness, 3 cleanup, altitude, CLAUDE.md conventions) + one gap sweep,
each verified. Several findings were reproduced independently by 2–4 angles;
two were confirmed by agents running real tests against the code.

Severity scale: **Critical** = broken or destructive in a default deployment.
**High** = destructive or product-breaking on a realistic path.
`code size` is an estimate of the fix diff, not of the affected code.

---

## 1. Ranked findings (top 15)

| # | Title | Description | Severity | Risk | Code size |
|---|---|---|---|---|---|
| 1 | Chart default `proxy.env.namespace: ""` kills every wake | One env var drives two things. `RunInformerCache` reads `""` as all-namespaces (fine); `DynamicPatcher.Namespace=""` builds a cluster-scoped URL for a namespaced CRD → 404, swallowed at `demand.go:83`. Stock install: cold requests hold, then 503, controller never sees demand. `test/e2e/cluster/helm-values.yaml:40` overrides it and its comment names the bug (D54). | Critical | Product broken by default, silently. No log, no event, no condition. | ~5 lines (set default + startup fatal if empty) |
| 2 | `gatherActivity` drops `NotReadyAddresses` | `model_controller.go:674` iterates `subset.Addresses` only. A proxy replica whose `/healthz` flaps or that is terminating vanishes from `expected`, so its in-flight streams are invisible. | Critical | Kills live generations. Violates `1→0 fails safe`. Hits sleep **and** drain gate. | ~5 lines |
| 3 | `scaleDownDelaySeconds` undefaulted → Model unwakeable | `hasDemand` TTL is that field. No `+kubebuilder:default`, no rule in `ValidateWithWarnings`. Omit it → `ttl=0` → `now.Sub(demandSince) < 0` false forever. | Critical | Schema-valid Model never wakes. Also defeats `sleepDue`, `freshSuccess`, `unhealthyDue`. | ~2 lines (marker + regen) |
| 4 | Dead SSH tunnel never evicted | `sshbackend.go:179` reuses on endpoint equality alone. No keepalive, no `client.Wait()`. Verified: after `ssh.Client.Close()`, 3 forwards failed, `Inner` never reached. | Critical | Permanent blackhole + each failure charged to replica → `unhealthyDue` tears down a healthy GPU, recreates, repeats. | ~25 lines (Wait() watcher) |
| 5 | Reaper lists Models from the informer cache | `reaper.go:117` uses `mgr.GetClient()`. Comment 3 lines up says the read must come from the API server so a stale view can't orphan a run. `GetAPIReader()` unused. | High | Short list + nil error → `Stop` on a live paid run mid-generation. | 1 line |
| 6 | Reaper stops unowned runs by name, ignores `SQUALL_MODEL_UID` | `ListRuns` posts to the un-scoped root route (no project filter). `classifyRunOwnership` exists; the destructive caller never asks. | High | Destroys third-party dstack jobs on a shared server. | ~10 lines |
| 7 | `reconcileDelete` uses `Status().Update` | Whole-object CAS, the race LIVE-1 forced `Reconcile` to abandon. Held requests patch the demand annotation every ~500ms. | High | Self-sustaining 409 loop: proxy stops signalling only when it sees `Draining`, which is the write being starved. GPU bills on. | ~3 lines (→ `Patch`) |
| 8 | `/healthz` green before informer sync | `cmd/proxy/main.go:191` returns 200 unconditionally; it's also the readinessProbe. Informer is fire-and-forget. | High | Pod Ready with empty cache → every request 404s; `/v1/models` publishes an empty list to LiteLLM discovery on every rollout. | ~8 lines |
| 9 | `status.forwardModel` not cleared on fresh run | The `action.Current == nil` block clears `ServedModel` + condition, forgets `ForwardModel` — the field the proxy actually rewrites bodies to. | High | Rewrites to a dead run's id → engine 400 → committed → `Activity.Failure` ×3 → teardown of a healthy replica. | 1 line |
| 10 | `tunnelFor` closes a live `ssh.Client` | One connection per replica, no pool. Endpoint change → `Close()` while N generations stream over it. | High | In-flight generations die mid-token. A routing decision terminating active work. | ~15 lines (lazy retire) |
| 11 | No HTTP timeout on activity/dstack clients | Both fall back to `http.DefaultClient` (`Timeout: 0`). `served.go:54` already bounds its own at 10s with the exact rationale. | High | One black-holed proxy IP wedges the whole controller at `MaxConcurrentReconciles: 1`. Every GPU bills. | ~4 lines |
| 12 | `resolveEnv` blocks the sleep flip | Guard sits on the single shared `Apply` site; its doc reasons only about provisioning. The `Replicas>0` split already exists 8 lines above. | High | Rotated Secret → `1→0` never actuates → GPU bills forever. Project's named worst case. | ~3 lines |
| 13 | e2e suite aborts at `BeforeSuite` | Requires a `fake-dstack` Deployment commit `3d405d5` deleted. Nothing creates it. | High | The only end-to-end proof of wake/idle/sleep runs zero specs. A broken wake path looks identical to a working one. | ~2 lines |
| 14 | `dstackRunName` join is not injective | `ns + "-" + name`. `ml-prod/qwen3` and `ml/prod-qwen3` → same run. | High | The exact F1 collision, domain narrowed not removed. Either Model's finalizer kills the other's GPU; `runNamesFor` makes each own the other. | ~10 lines |
| 15 | `Price.Validate` accepts `Inf`/`NaN`/negative | `strconv.ParseFloat` only. CRD is `x-kubernetes-preserve-unknown-fields`, no pattern. Value goes verbatim to dstack's `max_price`. | High | Unbounded spend passes the check documented as "the one thing squall must never do by accident". | ~4 lines |

## 2. Below the cut (real, lower rank)

| Title | Description | Severity | Risk | Code size |
|---|---|---|---|---|
| `Activity.Failure` on proxy config faults | `no backend url for model` (empty `serviceURL`) counted against the replica | Medium-High | False teardown of a healthy GPU | ~3 lines |
| `broadcast()` is global, not per-model | Any Model's status write wakes every held request's `tick` → full `attemptForward` | Medium-High | Forward storm against a waking replica — the load that collapsed dstack's proxy live | ~15 lines |
| >4 MiB body → `400 {"error":"missing model"}` | Should be 413. Makes 4 code paths + `attempt.go:19`'s stated contract dead | Medium | Misleading error; multimodal payloads rejected | ~10 lines |
| `URL()` / `Client()` resolve tunnel separately | Cache update between the two calls pairs dstack's URL with the SSH transport | Medium | Engine 404 committed to caller as the model's answer | ~10 lines |
| `peekAndRestore` `NopCloser` | Discards the transport's real `Closer` | Medium | Conn + readLoop goroutine leak on undrained 404s | ~5 lines |
| `drainTimeout` undefaulted | `pastDeadline` true on pass 1 → T12 drain never runs. All 4 fixtures set it, so untested | Medium | Zero drain on delete | ~2 lines |
| `SQUALL_DSTACK_PROJECT` unrendered | `dstack.projectName` is a chart value; controller hardcodes `main` | Medium | Non-`main` project → every dstack call 404s | ~3 lines |
| RBAC marker says `secrets: get`, code calls `Create` | Only the hand-written Helm Role has it | Medium | Kustomize path silently loses the SSH fast path | ~2 lines |
| `ActivityTracker` unbounded | Keyed by caller-supplied `"model"`, populated before the CR check, no eviction | Medium | 5000 junk names → 698 KB report decoded per replica per reconcile | ~10 lines |
| Query string dropped on forward | `target.String()+r.URL.Path`; hop-by-hop headers forwarded both ways | Medium | Silent param loss | ~5 lines |
| Price alert can never fire | `hasObserved` hard-coded `false` at the only caller | Medium | Operator believes cost overruns are alerted | ~5 lines |

---

## 3. Raw review output (JSON, as returned)

```json
[
  {
    "file": "./deploy/helm/squall/values.yaml",
    "line": 88,
    "summary": "The shipped chart default `proxy.env.namespace: \"\"` makes DynamicPatcher patch namespace \"\", which builds a cluster-scoped URL for a namespaced CRD; the 404 is swallowed by DemandCoalescer, so no on-demand Model can ever wake in a stock install.",
    "failure_scenario": "SQUALL_NAMESPACE is overloaded: RunInformerCache reads \"\" as all-namespaces (works), while DynamicPatcher.Namespace=\"\" makes client-go's makeURLSegments omit the `namespaces/<ns>` segment, so PatchDemand issues `PATCH /apis/squall.ackstorm.ai/v1alpha1/models/<name>` -> 404. internal/proxy/demand.go:83 discards the error with no log line. Stock `helm install`: informer syncs, /healthz green, /v1/models correct, and every cold request blocks for the full holdTimeout then answers 503 while the controller never sees demand and never Applies. test/e2e/cluster/helm-values.yaml:40 overrides it to `namespace: squall` and its comment names this exact failure (D54) — the workaround went into the e2e fixture, never into the chart default or a startup check."
  },
  {
    "file": "./internal/controller/squall/model_controller.go",
    "line": 674,
    "summary": "gatherActivity enumerates only `subset.Addresses` and silently drops `subset.NotReadyAddresses`, so a proxy replica holding live generations is excluded from the expected set and the 1->0 sleep flip kills its in-flight requests.",
    "failure_scenario": "The proxy Deployment has `readinessProbe: GET /healthz` (proxy-deployment.yaml:69) and proxy-service.yaml does NOT set publishNotReadyAddresses. Proxy replica A holds 40 streaming completions (InFlight=40) and its probe misses under load, or a rolling update sets its deletionTimestamp: the endpoints controller moves it to NotReadyAddresses. gatherActivity's `expected` is now {B} only; B reports InFlight:0, aggregateActivity returns Complete+AllIdle, sleepDue fires, and Reconcile Applies Replicas:0 — terminating 40 generations, the exact thing `1->0 fails safe` forbids. The same blind spot is on the teardown path: finalizer.go:79 calls the same gatherActivity, so drainEvidenceClean also declares a not-ready proxy's in-flight work absent and goes straight to Stop+Delete."
  },
  {
    "file": "./internal/controller/squall/model_controller.go",
    "line": 750,
    "summary": "hasDemand's TTL is spec.scaleDownDelaySeconds, which is +optional with no kubebuilder default and no rule in ValidateWithWarnings, so a schema-valid on-demand Model that omits it is permanently unwakeable.",
    "failure_scenario": "`ttl := time.Duration(m.Spec.ScaleDownDelaySeconds) * time.Second; return now.Sub(demandSince) < ttl`. The whole CRD has only three +kubebuilder:default markers (health={}, unhealthyAfter=15m, failureThreshold=3); scaleDownDelaySeconds has none (config/crd/bases/...:415 emits no `default:`) and model_validation.go requires only placement.backends and fleet.idleDuration. Apply `{minReplicas: 0, holdTimeout: 20m}` with no scaleDownDelaySeconds: the proxy writes demand-since successfully, the controller computes ttl=0, `now.Sub(demandSince) < 0` is false, wantAwake is false, Decide returns Asleep with no Apply — forever, with no error, event or condition. The same zero also makes sleepDue's `> 0` true instantly, disables freshSuccess (evidence b) and defeats unhealthyDue's recent-traffic gate."
  },
  {
    "file": "./internal/proxy/sshbackend.go",
    "line": 179,
    "summary": "A cached SSH tunnel is reused on endpoint equality alone with no liveness check, no keepalive and no client.Wait() watcher, so a tunnel that dies after being established is never evicted, never falls back to Inner, and each of its transport errors is charged to the replica's health.",
    "failure_scenario": "VERIFIED by an agent running an in-package test against a real in-process SSH server: dial a tunnel, forward one 200, close the ssh.Client (sshd restart / NAT idle drop / host reboot, status.replica unchanged). Every later URL(model) still returns the placeholder http://replica with ok=true and Client() returns the dead transport; three consecutive forwards failed with `use of closed network connection` and Inner was never reached (inner.hits stayed 0). b.conns is only deleted on an endpoint change or shutdown, and b.failed is only written by the dial path, so the documented promise (\"any failure to dial defers to Inner\") covers only the dial site. Worse than an outage: every failure hits handler.go:358 h.Activity.Failure, FailuresSinceSuccess climbs past the threshold of 3 while dstack's probe stays green and traffic keeps arriving, so unhealthyDue tears down a perfectly healthy GPU — which is then recreated and the loop repeats."
  },
  {
    "file": "./internal/controller/squall/reaper.go",
    "line": 117,
    "summary": "Sweep lists Models through mgr.GetClient() (informer-cached) despite the comment three lines above declaring the read must come from the API server precisely so a stale view cannot orphan a live run — the destructive Stop is gated on a read that is not what the code claims.",
    "failure_scenario": "cmd/controller/main.go:283 sets `Client: mgr.GetClient()`, and the manager's CacheOptions only DisableFor Secrets (main.go:190-191), so ModelList is served from the Model informer cache; mgr.GetAPIReader() is never used anywhere. A degraded reflector (dropped watch not yet relisted, partial resync after an apiserver rollout, a namespace-scoped cache option added later) returns a SHORT list with a nil error rather than an error, so the abort path this code depends on is unreachable through the cache. Every Model missing from that list has its run excluded from `owned`, and any run older than the 5-minute grace gets DstackClient.Stop — a live, paid, actively-serving run terminated mid-generation, which the type comment calls \"the single most important line in the file\"."
  },
  {
    "file": "./internal/controller/squall/reaper.go",
    "line": 162,
    "summary": "The reaper stops any active run whose NAME is not in the Model-derived set, never consulting the SQUALL_MODEL_UID marker ownership.go stamps for exactly this purpose — and ListRuns posts to the un-scoped root route, so runs from other dstack projects enter the candidate set.",
    "failure_scenario": "internal/dstack/http.go:120 sends `struct{}{}` to `/api/runs/list` with no project filter (the project-scoped variant answers 405 for list, so the root route is used and nothing narrows it). On a dstack server shared with anything else — another project, a colleague's `dstack apply`, a second squall install — a run named e.g. `finetune-job` older than 5 minutes is listed, matches no Model name, is not terminal, and gets `DstackClient.Stop(ctx, \"finetune-job\")`: someone else's live GPU job destroyed on the strength of a string miss. Run.Env is already decoded (client.go:241, http.go:345) and classifyRunOwnership already implements the correct three-state answer (owned / unmarked -> leave alone / other incarnation), but grep shows ModelUIDEnvKey has no reader outside ownership.go — the one destructive consumer never asks. Deleting a ModelUIDEnvKey check from reaper.go changes nothing, because it was never there."
  },
  {
    "file": "./internal/controller/squall/finalizer.go",
    "line": 49,
    "summary": "reconcileDelete writes phase Draining with Status().Update (whole-object optimistic lock), re-introducing the exact race LIVE-1 forced Reconcile to abandon for Status().Patch — and it gates every destructive teardown step below it.",
    "failure_scenario": "Commit b52bd8a documents, from live measurement, that Status().Update lost the optimistic-lock race against squall-proxy's DemandAnnotation metadata patches on EVERY attempt for a held request's whole duration, and changed only model_controller.go:435. `kubectl delete model` while a request is held: tick and retryForward call Demand.Signal unconditionally (handler.go:281, 411), merge-patching the annotation every refreshIntervalFor (500ms floor) from up to N replicas, so the Draining Update 409s, reconcileDelete returns before the Get/Stop/Delete/finalizer-removal block, and controller-runtime backs off. It is self-sustaining: the proxy only stops holding and signalling once it sees phase Draining (decision.go answers Draining with an immediate 404), and Draining is precisely the write being starved. The GPU bills for as long as traffic arrives. finalizer.go:114's full-object r.Update races the same annotation patch."
  },
  {
    "file": "./cmd/proxy/main.go",
    "line": 191,
    "summary": "/healthz — which is also the readinessProbe — returns 200 unconditionally while RunInformerCache runs in a fire-and-forget goroutine, so the Pod is marked Ready with an empty ModelCache.",
    "failure_scenario": "main.go:82-86 launches `go RunInformerCache(...)` with no sync barrier, then builds the mux and ListenAndServe's; proxy-deployment.yaml:69 points readinessProbe at GET /healthz, which is a bare `w.WriteHeader(200)`. Between the pod passing readiness and WaitForCacheSync completing, kube-proxy routes real traffic: Cache.Get returns hasCR=false for every model, Decide's `if !hasCR` returns ImmediateStatus 404, so every request is answered 404 with no hold and no demand patch. Simultaneously /v1/models — squall's LiteLLM `type: kubeai` discovery surface — serves an empty list from the same empty cache, so a rolling update publishes \"this proxy has zero models\" to discovery. RunInformerCache already returns only after HasSynced; gating /healthz on that flag is the missing barrier."
  },
  {
    "file": "./internal/controller/squall/model_controller.go",
    "line": 286,
    "summary": "The `action.Current == nil` mint-a-fresh-run block clears status.servedModel and removes ConditionServedModelVerified, but never clears status.forwardModel — the field D100 split out of servedModel and the one the proxy actually rewrites request bodies to.",
    "failure_scenario": "ForwardModel is written only at model_controller.go:387 and :396 (the verification arms) and nowhere else; the clear block at :287-288 forgets ServedModel and the condition but not ForwardModel, contradicting its own comment (\"A prior run generation's served-model answer says nothing about this one, so it must be forgotten here\"). A vLLM Model verified as `Qwen/Qwen3-8B` has forwardModel set; the run is reclaimed (terminal -> observe folds to Observed{} -> Recreating -> Apply with Current==nil) and the operator has edited spec.model. servedModel is cleared, forwardModel is not, so attempt.go:190 keeps rewriting every outbound `\"model\"` to the dead run's id. The engine answers 400/404, classifyAttempt commits it (phase Ready), and commit() records h.Activity.Failure for each one — feeding unhealthyDue's threshold-of-3 teardown of a replica that is serving fine. If the re-verification call errors (line 368) nothing is written at all, so the stale name persists indefinitely. No test asserts ForwardModel is cleared."
  },
  {
    "file": "./internal/proxy/sshbackend.go",
    "line": 183,
    "summary": "tunnelFor closes a live *ssh.Client synchronously the moment the cached endpoint stops matching, while other goroutines are still streaming responses multiplexed over that same connection.",
    "failure_scenario": "The design keeps ONE ssh.Client per replica and does not pool (documented at :37), so every in-flight generation for that model shares it. Requests A..N are mid-stream on t.http. The informer observes a new status.replica (dstack rescheduled the job, or reported the next deployment's endpoint first). The next request's tunnelFor takes b.mu, sees `t.endpoint != want`, and calls `t.client.Close()` — tearing down the connection A..N are streaming over. Every one of those generations dies mid-token; each returns from streamCommit with an error, and handler.go:466-468 records neither success nor failure, so the tokens are simply lost and each client sees a truncated body. SSHBackend.Close() at :312 does the same on shutdown with no drain. A request-routing decision is terminating active generations, which is what the project's top invariant forbids; the old connection should be retired lazily (stop handing it out, close when its last stream ends)."
  },
  {
    "file": "./internal/controller/squall/model_controller.go",
    "line": 735,
    "summary": "activityHTTPClient() falls back to http.DefaultClient (Timeout: 0) and cmd/controller never sets HTTPClient; dstack.NewHTTPClient(..., nil) does the same — so one black-holed proxy endpoint blocks a reconcile forever.",
    "failure_scenario": "gatherActivity enumerates the proxy Service's Endpoints and calls queryActivity SERIALLY, inside observe(), inside Reconcile; controller-runtime applies no per-Reconcile timeout, so the only cancellation is manager shutdown. A proxy pod IP that accepts the TCP connection and never answers — half-open socket after a node partition, a blackholed conntrack entry, a pod mid-termination still in Endpoints — parks `r.activityHTTPClient().Do(req)` indefinitely. With the default MaxConcurrentReconciles: 1 that wedges the entire Model controller: no wakes, no sleeps, no finalizer teardowns, no status writes, for any Model, while every provisioned GPU keeps billing. dstack calls have the identical exposure via client.go:355-357. internal/controller/squall/served.go:54 bounds its own client at 10s with the comment \"this runs inside a reconcile, and a replica that hangs must not hold the work queue\" — the hazard was recognised there and not applied here."
  },
  {
    "file": "./internal/controller/squall/model_controller.go",
    "line": 229,
    "summary": "resolveEnv (and EnsureSSHKey) sit on the single shared Apply call site, so a wake-path credential guard also blocks the 1->0 sleep flip; its doc reasons purely about provisioning.",
    "failure_scenario": "`if action.Apply { resolvedEnv, err := resolveEnv(...); if err != nil { return ctrl.Result{}, ... } }` runs before the only DstackClient.Apply in the tree, and the Replicas>0 split already exists 8 lines above (line 221, checkSchedulable). A Model with spec.secretEnv is awake; someone deletes or rotates the referenced Secret, removes the key, or adds the same name to spec.env (secretenv.go treats that as a hard error). Decide computes Asleep + Apply{Replicas: 0}; line 229 errors out, no status is written, the flip is never actuated, and controller-runtime requeues with backoff forever. The GPU bills indefinitely because a credential the sleep flip does not need could not be read — the project's named worst case, produced by a guard written for the opposite direction. EnsureSSHKey at line 253 has the same shape on the same call."
  },
  {
    "file": "./test/e2e/e2e_suite_test.go",
    "line": 35,
    "summary": "BeforeSuite still requires a `fake-dstack` Deployment that commit 3d405d5 deleted, so the entire Ginkgo e2e suite aborts before its first spec — the only end-to-end proof of the wake/idle/sleep loop runs zero behaviour.",
    "failure_scenario": "3d405d5 (\"run the e2e against real dstack, not a fake\") deleted test/e2e/cluster/01-fake-dstack/ and renamed it 01-model-mock; a full grep of *.yaml/*.sh/Makefile shows nothing creates a `fake-dstack` Deployment any more (only a `build-image-fake-dstack` image target). BeforeSuite loops over `{workloadNamespace, \"fake-dstack\"}` and runs `kubectl -n squall get deployment fake-dstack`, whose failure trips `Expect(err).NotTo(HaveOccurred())` at suite setup. `make e2e-run` therefore reports a suite-level failure having exercised nothing, and a green-looking CI change to the wake/sleep path would be indistinguishable from a broken one. Stale references remain at e2e_test.go:24, :108, :277 and :302."
  },
  {
    "file": "./internal/controller/squall/runname.go",
    "line": 18,
    "summary": "dstackRunName joins namespace and name with a bare '-', which is not injective over Kubernetes names — `a-b/c` and `a/b-c` both produce `a-b-c`, reintroducing the exact collision F1 was written to eliminate.",
    "failure_scenario": "`return m.Namespace + \"-\" + m.Name`, and both components legally contain hyphens (RFC 1123 labels). In a multi-team cluster `ml-prod/qwen3` and `ml/prod-qwen3` both resolve to run `ml-prod-qwen3`: each controller pass fights the other over replica count, either Model's finalizer Stops+Deletes the other's live run, and runNamesFor makes each Model `owned` by the other in the reaper's set so neither is protected by the ownership check either. The F1 commit message states the failure being fixed verbatim (\"squall/qwen and team-b/qwen drove ONE run: each controller pass fighting the other over replica count, and either Model's deletion tearing down the other's GPU\") — the fix narrowed the collision domain instead of removing it. Nothing validates uniqueness, and status.runName pins whichever Model reconciled first. A separator that cannot appear in a name, or a hash suffix, is the injective form. Related: reaper.go:124-129's comment still claims runNamesFor includes \"the pre-F1 bare one\", which it no longer does (D98 removed it) — a destructive guard the code does not implement."
  },
  {
    "file": "./api/squall/v1alpha1/price.go",
    "line": 118,
    "summary": "Price.Validate uses strconv.ParseFloat, which returns a nil error for \"Inf\", \"NaN\", negatives and 1e100, so the one check documented as squall's money safety valve passes an unbounded cost ceiling straight through to dstack.",
    "failure_scenario": "`if _, err := strconv.ParseFloat(string(p), 64); err != nil` is the entire check, and the CRD offers no backstop — maxPricePerHour is `x-kubernetes-preserve-unknown-fields: true` with no pattern (config/crd/bases/...:273-278). `maxPricePerHour: \"Inf\"` therefore satisfies checkSchedulable's veto — the block whose own comment says \"Unbounded spend is the one thing squall must never do by accident; do not 'fix' this into a warning\" — and enginePlacement (engine.go:71) sends `p.MaxPricePerHour.String()` VERBATIM as dstack's max_price, where pydantic float coercion also accepts Inf, so there is no ceiling on a marketplace where an H100 rents for $3.41/h. \"-1\" passes too and yields zero offers, which D58 documents as indistinguishable from an empty market. priceAsFloat64 (model_controller.go:557) then exports +Inf/NaN onto the squall_model_max_price_per_hour gauge. The parsed value needs math.IsInf / math.IsNaN / <= 0 rejection."
  }
]
```
