# TODO

Running list. Owner decisions are recorded inline so they are not re-litigated.
Findings keep their `D<n>` ledger ids — `docs/references/deviations-and-findings.md` stays the
detail; this file is the ordering.

Last reviewed: 2026-09-02.

---

## Now — in flight

- [x] **Live battery.** **DONE 2026-08-31/09-01**, on the cheap `ollama-tiny` fixture rather
      than the 27B: $0.13-0.21/h instead of $1.89/h tests the same control-plane paths. Cold
      wake 2m12s, idle reap verified at exactly the 15m limit, and the probe battery that
      produced D130 and D135. Superseded detail: Verify, against a real GPU, the two things fixed today:
      the idle flip (D91) and the unhealthy teardown (D95). Full battery in
      `docs/plans/2026-08-31-unhealthy-replica-teardown.md` Task 6.
      **Hazard to watch:** `provisioningTimeout` has no destructive trigger (D97), so a wake
      that hangs is not bounded by squall. dstack's own probe-failure path and
      `fleet.idleDuration: 10m` are the only backstops. Do not leave it unattended.

## Done today (2026-08-31)

- [x] **D91 — the 1→0 flip was unreachable.** A proxy replica that had never served a Model
      reported `NoData`, which was read as ambiguity and killed the sleep decision on every
      pass. Two replicas + one keep-alive client = a GPU squall could never release.
      Proven by A/B against the live cluster: pre-fix stayed `Waking` 120s, fixed slept in 4s.
- [x] **D92 — the uncommanded-death log said "recreating" when nothing was.**
- [x] **D95 — unhealthy teardown.** A failing replica looked *busier* the worse it got, because
      in-flight requests are what block the idle flip. Now: no 2xx delivered in full for
      `spec.health.unhealthyAfter` (15m) across `spec.health.failureThreshold` (3) failures,
      while traffic is arriving → scale to 0, `Healthy=False`, wait for the next request.

---

## v0.1.0 release blockers

Ordered by what unblocks what. Nothing here is optional for the release.

- [x] **D100 — FIXED: passing the served-model check breaks serving.** `status.servedModel` is a
      comma-joined LIST (`model_controller.go`, both the match and mismatch branches), and
      `attemptForward` (`internal/proxy/attempt.go`) rewrites the request's `model` field to that whole
      string. Measured live 2026-08-31 on `ollama-tiny`: every request after verification succeeded got
      `400 "model is required"` from Ollama, while the same request sent straight to the replica returned
      200. vLLM hides it (one served name); Ollama always reports the alias plus the source weights, so it
      fails every time. Fix: publish the ONE matched served name for the proxy to forward under and keep
      the joined list diagnostic only. Must cover the mismatch branch too.
      **DONE 2026-08-31.** Split into two fields rather than redefining one: `status.servedModel` keeps the
      honest joined report (D65's printer column is what catches a 0.6B standing in for a 27B), and the new
      `status.forwardModel` carries the single id the proxy may rewrite to — empty on a mismatch, and empty
      when several ids exist with no expectation to disambiguate them. Collapsing servedModel instead would
      have cost the D65 diagnostic; a reviewer test caught that.

- [x] **F1 — CRITICAL: a dstack run is keyed only by `model.Name`.** The CR is namespaced;
      the run is not. `squall/qwen` and `team-b/qwen` are the same dstack run, so one namespace
      can adopt or delete the other's GPU. `model_controller.go` `Apply(Name: model.Name)` and
      `observe(ctx, model.Name, ...)`; `dstack.Client.Get(ctx, name)` takes no namespace.
      **DECIDED (owner, 2026-08-31): run name becomes `<namespace>-<name>`.** Fixes it by
      construction rather than by discipline. Accepted cost: existing runs are renamed, so the
      current `qwen3-8-27b` run becomes an orphan the reaper collects once.
      Second half, cheaper and separable: record the CR UID alongside `status.runId` and refuse
      to act on a run whose recorded UID does not match, so a recreated CR cannot adopt the old
      run.
      **DONE 2026-08-31.** The "accepted cost" line above was wrong while a live run existed:
      renaming does not merely orphan the old run, it *reaps a live one and buys a duplicate*
      (`reaper.go` keys `owned` by bare `m.Name`, so the pre-rename run matches nothing and is
      stopped mid-generation, while `observe()` sees ErrNotFound and mints a second paid
      instance — D98). An adoption shim was written to survive that, then **deleted the same
      day at the owner's call** once the window closed: verified 0 Vast instances and every
      `qwen3-8-27b` dstack run terminal, so there was no live pre-F1 run left to protect.
      Shipped: `runNameFor` / `dstackRunName` / `runNamesFor` (`runname.go`), `status.runName`
      as the recorded identity, and `observe()` split into `runName` + `activityKey` because
      those stopped being the same string. Known and accepted: the asleep `e2e-fixture-model`
      run keeps its bare name and will be reaped once — a kind-backend mock at zero replicas,
      so it costs nothing.

- [x] **F6 — the ledger is not a reliable statement of current state.** 27 listed OPEN entries
      were audited against the current code on 2026-09-01; stale fixed entries were closed,
      documentation/process debts were marked `OPEN (docs)`, and unresolved hazards remain
      explicitly OPEN. See D133 and the inline VERIFIED evidence in the ledger.
      **Do this BEFORE the release mechanics** — the ledger is what a reader will use to judge
      whether 0.1.0 is honest about itself.

- [x] **F2 — DONE: validation is dead code.** `Validate` / `ValidateWithWarnings` have **zero**
      non-test callers, so `Reconcile` accepts configurations the rules exist to reject
      (`holdTimeout > provisioningTimeout` among them).
      **Judgement (mine, open to override):** wire it through `checkSchedulable` as
      `Schedulable=False`, matching the existing `ReasonInvalidPrice` veto — NOT as an error
      return from `Reconcile`, which would wedge an already-running Model on a spec edit.

- [x] **F3 — DONE (1bc8d49): unbounded request body.** `modelFromRequest` (`internal/proxy/handler.go`)
      calls `io.ReadAll(r.Body)` with no limit, and runs *before* the correctly-bounded
      `readReplayableBody`, so the 4 MiB cap never protects anything. One large request can
      exhaust proxy memory. ~5 lines: `io.LimitReader`.

- [x] **F4 — DONE: the publication gates block a push, partly on their own documentation.**
      - `make qa-security` FAILS on three doc hits: two private-IP examples in
        `docs/plans/`, and — genuinely — `deviations-and-findings.md:58`, where D36 documents
        the ackstorm-email check by quoting `someone@<company>.com` as the string that proved
        the check works. The check catches its own documentation. Redact the examples rather
        than allowlisting a file that is edited constantly.
      - `scripts/pre-push-check.sh` lists check 6 (origin remote) under "Soft checks (warnings
        only)" in its header and then calls `fail()` at line ~189. The remote is HTTPS; the
        script expects SSH. Pick one and make the header and the code agree.
      - gitleaks and trufflehog pass clean. No secrets.

- [x] **F5 — PARTLY DONE: release state is incoherent.** The chart now says `version: 0.1.0` /
      `appVersion: "0.1.0"`, `Makefile` says `VERSION ?= 0.1.0`, `CHANGELOG.md` is present,
      and the `origin` remote is configured. Only the release tag and push remain owner-run.
      Depends on F4: the push cannot land until the gates do.

- [x] **`status.fleet` on the Model.** **DONE 2026-09-01.** Published from what `preflight`
      already computed and discarded: one `FleetStatus` per backend with state
      Admitting/Created/Unfleeted/Unconfigured, plus a `Fleet` printer column. A dstack error
      publishes no mirror at all rather than a half-built one that would read as fact.
      See `docs/references/decisions-and-open-items.md` ("no Fleet CRD").

- [x] **Rename the deployments** to `squall-operator` / `squall-proxy`. **DONE 2026-09-01**,
      including the `SQUALL_PROXY_SERVICE_NAME` wiring and `hack/cluster.sh`. Deployed and
      verified live: a partial rename would have silently disabled §6, because `gatherActivity`
      returns nil when the proxy Service cannot be resolved.

- [x] **Go e2e suite does not run.** **DONE 2026-09-01** (`180dc67`). The diagnosis in this
      line was wrong: `fake-dstack` and `model-mock` are current names with live Makefile
      targets. The actual fault was the loop-model fixture pointing at `backends: [vastai]`
      with a non-existent Ollama digest — see D137, which records that it failed safe *only*
      because that dstack had no vastai backend configured. **Green 2026-09-01: `Ran 6 of 6 Specs — SUCCESS!`** The last failure was not the fixture but D141, a race against the controller's leader election.

---

## After 0.1.0

- [ ] **Vast placement guarantees for long-running batch jobs.** Allow a Model
      to require both (a) a minimum host reliability percentage and (b) an
      offer whose advertised continuous rental window covers the workload plus
      safety margin — for example, require at least 7 days of availability for
      a 3-day batch. These are separate constraints: a historically reliable
      host may still have a short remaining rental window. Reject the offer
      before provisioning and surface `NoCapacity` when none qualifies. Do not
      confuse this lower-bound guarantee with dstack's `max_duration`, which is
      an upper bound that terminates Squall's own run. First verify which Vast
      offer field dstack 0.21.x exposes for remaining/maximum rentable duration
      and whether it can be passed through `VastAIProfileOptions`; if dstack
      drops it, the change belongs upstream rather than in a Squall-side guess.

- [ ] **D94 — no admission control on the Ready path.** `maxPendingPerModel` bounds *holds*
      only; once Ready, the proxy forwards unlimited concurrency into whatever capacity exists.
      MEASURED 2026-08-28: ~1000 tok/s aggregate split across ~57 streams is ~17 tok/s each, so
      any request over ~4,800 tokens cannot finish inside a 300s client timeout. 79% of 11,378
      requests failed that way. The health check does NOT cover this — it is a capacity
      problem, not a liveness one. Needs a decision: per-model concurrency ceiling, or
      documented as the caller's job.

- [x] **D97 — `provisioningTimeout` has no destructive trigger.** **DONE 2026-09-01.** OQ3 was
      not actually blocking: it was resolved in spec v0.18 §5.2 and `status.wakeStartedAt` has
      existed and been populated all along — only the trigger was never written. A wake that
      never reaches Ready is now destroyed, alarmed, and marked **Dead** (not Asleep: the run
      never became usable, so the next attempt must mint a fresh one).

- [ ] **D101 probe — does an intervening wake reset the fleet idle timer?** Measured 2026-08-31: with
      `fleet.idleDuration: 2m`, an instance died 117s after the FIRST Asleep despite being woken and reused
      (same Vast id) at 64s. If the timer does not reset, a request arriving at 1:59 gets a GPU that dies a
      second later. Probe: wake deliberately at ~1:50 into the idle window, see whether the machine
      survives. One run so far, so this is a hypothesis. Do not tune `idleDuration` until it runs. Feeds D96.

- [ ] **D96 — the unhealthy teardown can flap.** A Model broken by *configuration* rather than
      by machine re-wakes on the next request and buys a fresh GPU every `unhealthyAfter`
      (~$0.32/cycle at $1.29/h). No backoff was added: it fights `0→1 fails open`, and the
      owner chose "wait for a request" precisely so nothing is bought when nothing is asking.
      Escalate only if observed.

- [ ] **Per-run ephemeral SSH keys.** Today the replica key is per-installation.

- [ ] **`EnsureSSHKey` fails open silently.** Needs a log line; a missing direct path is
      currently indistinguishable from a working one without reading `status.replica`.

- [ ] **KubeAI-style `nodeSelector` on the Kubernetes backend** so Karpenter provisions GPU
      nodes on demand. Idea only, no code. Three open questions written up in
      `docs/references/decisions-and-open-items.md`.

- [ ] **squall-proxy has no authentication.** This is why the live endpoint is LAN-only. Must
      land with any public exposure, not after.

- [ ] Loki exporter; gateway completion for non-Vast topologies.
