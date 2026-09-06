// SPDX-License-Identifier: MIT

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/clock"
)

// Backend resolves where a Ready Model's requests forward to. Squall's
// dstack.Client (FROZEN) carries no such URL on dstack.Run, and neither
// ModelSpec nor ModelStatus names one (see docs/references/
// deviations-and-findings.md D25) — real wiring is deferred to whoever
// resolves D1 (the dstack wire shape) and D6 (the proxy Service naming).
// This seam exists so everything else in this package is fully
// implemented and tested against it today.
type Backend interface {
	// URL resolves model's forward target. ok is false when unknown (e.g.
	// an informer race: Ready in the cache but no backend registered yet)
	// — the handler answers 502 rather than forwarding into a nil target.
	URL(model string) (target *url.URL, ok bool)
}

// waitBody is §7's wait-contract JSON body: "a JSON body naming the
// state" (asleep | waking | recreating).
type waitBody struct {
	Error string    `json:"error"`
	State WaitState `json:"state"`
}

// Request outcomes recorded by requestRecord (D87). These are log values, so
// they are stable strings: something will eventually grep for them.
const (
	outcomeCommitted    = "committed"
	outcomeWaitContract = "wait-contract"
	outcomeImmediate    = "immediate"
	outcomeGatewayAuth  = "gateway-auth-fault"
	// outcomeClientGone is a caller that hung up before the forward could
	// commit. It is deliberately NOT outcomeWaitContract: nothing was written
	// to a socket that is already closed, and nothing was learned about the
	// replica. See the Activity.Failure guard in ServeHTTP.
	outcomeClientGone = "client-gone"
	outcomeCeiling    = "request-ceiling"
)

// errRequestCeiling distinguishes squall's own bound from a client disconnect.
var errRequestCeiling = errors.New("squall-proxy: request ceiling reached")

func applyRequestCeiling(r *http.Request, d time.Duration) (*http.Request, context.CancelFunc) {
	if d <= 0 {
		return r, func() {}
	}
	ctx, cancel := context.WithTimeoutCause(r.Context(), d, errRequestCeiling)
	return r.WithContext(ctx), cancel
}

// requestRecord accumulates what one request did, so ServeHTTP's single
// deferred log line can describe the whole lifecycle from any of its several
// return paths (D87). An empty outcome means a path returned without
// recording one — logged at Warn precisely because that is a bug.
type requestRecord struct {
	model   string
	phase   squallv1alpha1.ModelPhase
	outcome string
	held    time.Duration
	// reason is why a non-committing attempt failed, verbatim from
	// attemptForward. Empty on the happy path (D99).
	reason string
}

// log emits the record at a level chosen by outcome, so that a busy proxy
// stays readable: an ordinary forward is Debug, while anything that made a
// caller wait or handed it an error is worth seeing by default.
func (rec *requestRecord) log(ctx context.Context, elapsed time.Duration) {
	level := slog.LevelWarn
	switch rec.outcome {
	case outcomeCommitted:
		level = slog.LevelDebug
		if rec.held > 0 {
			level = slog.LevelInfo
		}
	case outcomeWaitContract, outcomeImmediate:
		level = slog.LevelInfo
	case outcomeClientGone:
		// Routine: callers disconnect. Warn would drown a busy proxy in noise
		// for something that is not a fault.
		level = slog.LevelInfo
	}
	attrs := []any{
		"model", rec.model,
		"outcome", rec.outcome,
		"phase", string(rec.phase),
		"duration_ms", elapsed.Milliseconds(),
	}
	if rec.reason != "" {
		attrs = append(attrs, "reason", rec.reason)
	}
	if rec.held > 0 {
		attrs = append(attrs, "held_ms", rec.held.Milliseconds())
	}
	slog.Log(ctx, level, "request served", attrs...)
}

// Handler is squall-proxy's per-request data path (spec §7): resolve the
// Model from the request, run Decide, block via Await when prescribed,
// then forward, answer the wait contract, or answer immediately.
type Handler struct {
	Cache    *ModelCache
	Demand   *DemandCoalescer
	Activity *ActivityTracker
	Backend  Backend
	Metrics  *ProxyMetrics

	// DstackToken authenticates the FORWARDED request to dstack's own
	// service proxy (LIVE-4). dstack's default per-service auth (`auth:
	// true` — docs/references/dstack-real-api.md §8.1, and squall's
	// ApplyRequest never sets it otherwise) demands `Authorization: Bearer
	// <token>` on GET /proxy/services/... ; without it dstack answers 403,
	// which commit() below (F23) turns into a client-facing "gateway auth
	// fault" with no clue in the logs why. Set on the outbound request in
	// attemptForward ONLY — never read from or echoed onto the client's
	// request or response. Empty means every forward will 403; cmd/proxy
	// logs that loudly at startup so the failure is diagnosable rather than
	// a silent per-request 502 loop.
	DstackToken string

	// Clock computes the hold deadline (deadline = Clock.Now().Add(hold)).
	// Defaults to clock.RealClock{} when nil. See hold.go's doc comment: the
	// actual blocking wait is always real wall-clock regardless.
	Clock clock.Clock

	// RefreshInterval is now a CEILING, not the interval itself (LIVE-3,
	// corrected): the actual per-hold cadence is derived per-Model by
	// refreshIntervalFor from that Model's OWN spec.idleTimeout,
	// because one proxy-wide value cannot be right for every Model at once —
	// measured live, a 300s production Model and a 2s e2e fixture disagreed
	// about their TTL in the same cluster, and the 2s fixture's tuned value
	// was 600x too fast for the 300s Model, whose annotation churn is what
	// starved the controller's status write (LIVE-1). RefreshInterval still
	// bounds the result from above (and is what a Model with no TTL
	// configured falls back to entirely) — see refreshIntervalFor.
	RefreshInterval time.Duration

	// MaxPendingPerModel bounds held capacity per model (v0.16 addition, a
	// global proxy setting — not a CRD field, per §7). <= 0 means
	// unbounded. Beyond it, the wait contract is answered immediately
	// instead of blocking (task 9's N+1-concurrent-holds test).
	MaxPendingPerModel int
	// MaxRequestDuration bounds hold, forward, and stream for one request.
	MaxRequestDuration time.Duration

	pendingMu sync.Mutex
	pending   map[string]int
}

// refreshIntervalFor derives a held request's demand-refresh cadence from
// THAT Model's own idleTimeout — the annotation's self-expiry TTL
// — rather than one proxy-wide constant (LIVE-3, corrected). A single global
// interval cannot be right for every Model at once: measured live, a real
// 300s production Model inherited a refresh interval tuned for a 2s e2e
// fixture and churned the annotation ~600x too often, which is what starved
// the controller's status write for the whole hold (LIVE-1).
//
// fraction (1/10) leaves margin for up to nine consecutive missed refreshes
// (a slow tick, a transient PatchDemand failure, a GC pause) before the
// annotation could self-expire mid-hold — 1->0 must fail safe, so a hold
// must never let it lapse under an in-flight request. floor keeps a very
// short TTL (the e2e fixture's 2s) from hammering the API server every few
// hundred ms — not coincidentally, TTL/10 clamped to this floor reproduces
// the fixture's previously hand-tuned 500ms exactly, which is why that
// override is no longer needed. ceiling is the one thing left for an
// operator to tune (Handler.RefreshInterval / SQUALL_DEMAND_REFRESH_INTERVAL):
// it bounds how stale demand evidence is allowed to get even for a Model
// with a very long TTL, and is what a Model with no TTL configured
// (idleTimeout <= 0, e.g. never scales down) falls back to
// entirely.
func refreshIntervalFor(idleTimeout time.Duration, ceiling time.Duration) time.Duration {
	const (
		fraction = 10
		floor    = 500 * time.Millisecond
	)
	if idleTimeout <= 0 {
		return ceiling
	}
	interval := idleTimeout / fraction
	if interval < floor {
		interval = floor
	}
	if ceiling > 0 && interval > ceiling {
		interval = ceiling
	}
	return interval
}

func (h *Handler) clock() clock.Clock {
	if h.Clock == nil {
		return clock.RealClock{}
	}
	return h.Clock
}

// ServeHTTP implements the six-row table end to end for one request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	metricModel := "_unknown"
	rec := requestRecord{outcome: outcomeImmediate}
	mw := &metricResponseWriter{ResponseWriter: w}
	w = mw
	defer func() {
		if h.Metrics != nil {
			h.Metrics.Observe(metricModel, requestMetricOutcome(rec), mw.status, rec.held > 0, time.Since(started))
		}
	}()

	model, errStatus := modelFromRequest(r)
	if errStatus != 0 {
		msg := `{"error":"missing model"}`
		if errStatus == http.StatusRequestEntityTooLarge {
			// D120: a >4 MiB body is declined honestly, not blamed on a
			// "missing model" the proxy simply refused to go looking for.
			msg = `{"error":"request body exceeds the proxy's 4 MiB limit"}`
		}
		http.Error(w, msg, errStatus)
		return
	}

	// D87: one line per request, so what the proxy DID is recoverable after
	// the fact. Before this, both replicas logged nothing but their startup
	// banner across a 48-concurrent ramp and a 6.6-minute held cold start,
	// and D86 had to be diagnosed by re-deriving the code path from source.
	// Wall-clock on purpose: this measures what the caller actually waited,
	// which is not the same question as the hold deadline's clock seam.
	//
	// NEVER add the request or response body here, nor any header: bodies are
	// user prompts and model output, and headers carry the dstack bearer
	// token (see Handler.DstackToken).
	rec.model = model
	defer func() { rec.log(r.Context(), time.Since(started)) }()

	snap, hasCR := h.Cache.Get(model)
	metricModel = modelMetricLabel(model, hasCR)

	// D11: increment in-flight at accept time, before any upstream call —
	// minimises a known TOCTOU window in §6's sleep decision.
	//
	// Gated on hasCR (D126): the tracker is keyed by a CALLER-SUPPLIED
	// string, and tracking every junk name an unauthenticated client
	// invents grew the /internal/activity report without bound (verified:
	// 5000 names -> a 698 KB body the controller decodes per replica per
	// reconcile). A request with no CR is answered immediately below and
	// never reaches an upstream, so skipping it cannot hide in-flight work
	// from the sleep decision — aggregateActivity's soundness argument
	// ("Begin before any upstream call") is untouched.
	if hasCR {
		var cancel context.CancelFunc
		r, cancel = applyRequestCeiling(r, h.MaxRequestDuration)
		defer cancel()
		done := h.Activity.Begin(model)
		defer done()
		if h.Metrics != nil {
			metricsDone := h.Metrics.Begin(metricModel)
			defer metricsDone()
		}
	}
	action := Decide(snap.Phase, hasCR, 0)
	rec.phase = snap.Phase

	// A Model the controller has ruled unschedulable will not become Ready
	// by waiting. Hold only while something is actually coming up (§7).
	if action.Block && hasCR && !snap.Schedulable {
		// Signal demand anyway, before answering. preflight() is the only
		// writer of the Schedulable condition and it runs on the WAKE path,
		// which demand is what triggers — so staying silent here closes the
		// loop on itself: no demand, no wake attempt, no preflight, and the
		// False that sent us down this branch is never re-evaluated. One
		// transient backend outage would make a Model unwakeable for good
		// (MEASURED 2026-09-01: backend restored, still refusing 7m later).
		//
		// Refusing to hold is right; refusing to try is `0->1` failing
		// CLOSED, on the one path the invariant exists for.
		if action.DemandPatch {
			h.Demand.Signal(r.Context(), model)
		}
		rec.outcome = outcomeWaitContract
		h.answerWait(w, Action{
			DeadlineStatus: http.StatusServiceUnavailable,
			DeadlineState:  WaitAsleep,
		})
		return
	}

	body, replayable := readReplayableBody(r)

	// Hoisted out of the hold block: D86's post-hold retry spends the SAME
	// budget and cadence the hold itself used. holdStart being zero is what
	// distinguishes a request that never waited — such a request has no
	// budget to spend and must not retry.
	var (
		deadline  time.Time
		refresh   time.Duration
		holdStart time.Time
	)

	if action.Block {
		if !h.acquirePending(model) {
			// maxPendingPerModel exceeded: answer the wait contract
			// immediately rather than queuing a bound-defeating Nth+1 hold.
			rec.outcome = outcomeWaitContract
			h.answerWait(w, action)
			return
		}
		defer h.releasePending(model)
		if h.Metrics != nil {
			releaseMetricsHold := h.Metrics.Hold(metricModel)
			defer releaseMetricsHold()
		}

		// §7: the held real request IS the serving path's readiness oracle.
		// Each tick refreshes demand AND retries the actual forward; 502/503/
		// connection-refused mean "still waking". First-party traffic is never
		// a probe, so the two-lane rule (§10) is untouched, and a token can be
		// served before phase: Ready is ever written.
		// stopHold ends the hold the instant the oracle answers. Without it
		// Await keeps blocking until status.phase changes or the deadline
		// elapses, so a request that committed at 40ms is still delivered a
		// full holdTimeout later — with the upstream body held open, unread,
		// for the whole wait. attemptForward and commit deliberately use
		// r.Context(), never holdCtx, so cancelling the hold never cancels
		// the response being streamed.
		holdCtx, stopHold := context.WithCancel(r.Context())
		defer stopHold()

		var committed *http.Response
		tick := func() {
			h.Demand.Signal(r.Context(), model)
			if committed != nil || !replayable {
				return
			}
			// D44: re-read the phase fresh on every attempt, never the
			// snapshot captured before the hold started — the hold exists
			// precisely because the phase is expected to change underneath,
			// and a 404's meaning (still waking vs. actually dead) depends
			// on which phase is current right now.
			freshSnap, _ := h.Cache.Get(model)
			if resp, res, _ := h.attemptForward(r.Context(), r, model, body, freshSnap.Phase); res == attemptCommit {
				committed = resp
				stopHold()
			}
		}

		deadline = h.clock().Now().Add(snap.HoldTimeout)
		refresh = refreshIntervalFor(snap.IdleTimeout, h.RefreshInterval)
		holdStart = time.Now()
		slog.Info("holding request for a waking model",
			"model", model, "phase", string(snap.Phase), "hold_timeout", snap.HoldTimeout.String())

		newSnap, newHasCR, _ := Await(holdCtx, h.Cache, model, deadline, refresh, tick)
		rec.held = time.Since(holdStart)

		if committed != nil {
			h.commit(w, committed, &rec)
			return
		}
		if errors.Is(context.Cause(r.Context()), errRequestCeiling) {
			rec.outcome = outcomeCeiling
			rec.reason = errRequestCeiling.Error()
			h.answerWait(w, Action{DeadlineStatus: http.StatusServiceUnavailable, DeadlineState: WaitWaking})
			return
		}

		snap = newSnap
		action = Decide(newSnap.Phase, newHasCR, 0)
		rec.phase = newSnap.Phase
		if action.Block {
			rec.outcome = outcomeWaitContract
			h.answerWait(w, action)
			return
		}
	}

	switch {
	case action.Forward:
		resp, res, ferr := h.attemptForward(r.Context(), r, model, body, snap.Phase)
		if res != attemptCommit && !holdStart.IsZero() && replayable {
			// D86 (LIVE-8): the phase flipped to Ready but dstack's service
			// proxy has not finished wiring the route yet. Measured live, that
			// gap is seconds wide and the next attempt succeeds — yet this
			// path used to surrender immediately, so a caller paid the entire
			// 399s cold start and STILL got an error with ~13 of 20 minutes of
			// holdTimeout unspent. "Wake may tolerate uncertainty": keep
			// trying while budget remains.
			resp, res, ferr = h.retryForward(r.Context(), r, model, body, deadline, refresh, &rec)
			rec.held = time.Since(holdStart)
		}
		if res != attemptCommit {
			if rerr := r.Context().Err(); rerr != nil && !errors.Is(context.Cause(r.Context()), errRequestCeiling) {
				// THE CALLER HUNG UP. This says nothing whatsoever about the
				// replica, and counting it as a failure is how `1->0 fails
				// safe` gets violated: MEASURED LIVE 2026-08-31, a pkill of a
				// 64-way client turned one healthy GPU into 64 recorded
				// "replica failures" in under a second, with the engine still
				// serving normally three minutes later (D99). The same door
				// admits an OOM, a redeploy, or a reset connection -- and the
				// same client's own logs show 27,998 socket resets in 21 hours
				// on 2026-08-28. Left uncounted, that is a teardown of a
				// working replica driven entirely by client-side weather.
				//
				// Nothing is written: the socket is already closed.
				rec.outcome = outcomeClientGone
				rec.reason = rerr.Error()
				return
			}
			if errors.Is(context.Cause(r.Context()), errRequestCeiling) {
				rec.outcome = outcomeCeiling
				rec.reason = errRequestCeiling.Error()
				h.answerWait(w, Action{DeadlineStatus: http.StatusServiceUnavailable, DeadlineState: WaitWaking})
				return
			}
			// Ready in cache but the gateway is not serving: answer the wait
			// contract rather than a bare 502, so the client sees a truthful
			// state (§7). It is also a FAILURE for the health verdict -- the
			// Model was advertised Ready and this request got nothing, which is
			// the exact shape the 2026-08-29 incident took.
			//
			// UNLESS the fault is the proxy's own configuration (D118): "no
			// backend url" means status.serviceURL or SQUALL_DSTACK_URL is
			// empty, which says nothing about the replica — charging it to
			// the replica's failure count is D99's bug shape again, three of
			// which tear down a healthy GPU.
			if !errors.Is(ferr, errNoBackendURL) {
				h.Activity.Failure(model)
			}
			rec.outcome = outcomeWaitContract
			if ferr != nil {
				rec.reason = ferr.Error()
			}
			h.answerWait(w, Action{DeadlineStatus: http.StatusServiceUnavailable, DeadlineState: WaitWaking})
			return
		}
		h.commit(w, resp, &rec)
	case action.ImmediateStatus != 0:
		rec.outcome = outcomeImmediate
		w.WriteHeader(action.ImmediateStatus)
	default:
		http.Error(w, "internal: Decide produced no action", http.StatusInternalServerError)
	}
}

type metricResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *metricResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *metricResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *metricResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// retryForward re-attempts a forward at the hold's own cadence until it
// commits, the model stops being able to serve, or the hold deadline passes
// (D86). It is reached ONLY by a request that actually held: one that never
// waited has no budget to spend, and turning every transient 502 on the hot
// path into a multi-second retry is precisely the behaviour not wanted.
//
// Demand is re-signalled on every attempt for the same reason the hold itself
// refreshes it — the annotation self-expires, and `1->0` must fail safe. A
// caller still waiting here is in-flight, so the model must not be allowed to
// sleep underneath it.
func (h *Handler) retryForward(ctx context.Context, r *http.Request, model string, body []byte, deadline time.Time, refresh time.Duration, rec *requestRecord) (*http.Response, attemptResult, error) {
	if refresh <= 0 {
		refresh = time.Second
	}
	// last is why the most recent attempt failed, so a hold that burns its
	// whole budget still reports a cause rather than a bare deadline (D99).
	var last error
	for {
		// Real wall-clock, matching Await's own reasoning: the deadline is
		// about a real client actually waiting.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, attemptRetry, errors.Join(errors.New("hold deadline reached"), last)
		}
		wait := refresh
		if wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, attemptRetry, ctx.Err()
		case <-timer.C:
		}

		h.Demand.Signal(ctx, model)

		// Re-read the phase every attempt (D44's reasoning): a 404's meaning
		// depends on the phase that is current right now, not the one that
		// was current when the hold started.
		snap, hasCR := h.Cache.Get(model)
		rec.phase = snap.Phase
		if a := Decide(snap.Phase, hasCR, 0); a.ImmediateStatus != 0 {
			// The model stopped being able to serve at all (deleted, dead).
			// Waiting out the remaining budget would tell the caller nothing
			// new.
			return nil, attemptRetry, fmt.Errorf("model stopped serving in phase %s", snap.Phase)
		}
		resp, res, err := h.attemptForward(ctx, r, model, body, snap.Phase)
		if res == attemptCommit {
			return resp, attemptCommit, nil
		}
		last = err
	}
}

// commit streams a committed upstream response to the client and records
// §6's evidence (b). A raw upstream 403 is translated per F23 (auth fault,
// never a wake) BEFORE anything reaches the wire.
func (h *Handler) commit(w http.ResponseWriter, resp *http.Response, rec *requestRecord) {
	defer func() { _ = resp.Body.Close() }()
	model := rec.model

	if resp.StatusCode == http.StatusForbidden {
		gw := Decide(squallv1alpha1.ModelPhaseReady, true, GatewayCode(resp.StatusCode))
		if gw.Alarm {
			slog.Error("gateway auth fault forwarding to model backend", "model", model, "status", resp.StatusCode)
		}
		rec.outcome = outcomeGatewayAuth
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(gw.ImmediateStatus)
		_, _ = w.Write([]byte(`{"error":"gateway auth fault"}`))
		return
	}

	rec.outcome = outcomeCommitted
	// Evidence (b), tightened 2026-08-31. Two things changed and both matter to
	// the unhealthy teardown that now reads this:
	//
	//   - Only a 2xx counts. This used to fire for ANY status we agreed to
	//     stream, so a replica answering 500 to everything looked exactly as
	//     healthy as one serving tokens -- which made "no successful response
	//     in 15 minutes" unable to fire against precisely the replica it exists
	//     to catch.
	//   - It fires AFTER delivery, not before. A replica that accepts a
	//     request, sends headers and then hangs is the failure this evidence
	//     must not launder into proof of health.
	//
	// A client that disconnects mid-stream is NOT the replica's failure, so it
	// records neither a success nor a failure. The next request will.
	if err := streamCommit(w, resp); err != nil {
		if resp.Request != nil && errors.Is(context.Cause(resp.Request.Context()), errRequestCeiling) {
			rec.outcome = outcomeCeiling
			rec.reason = errRequestCeiling.Error()
			return
		}
		slog.Warn("client disconnected mid-stream", "model", model, "err", err)
		return
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		h.Activity.Success(model)
		return
	}
	h.Activity.Failure(model)
}

// answerWait is §7's wait contract: 503/404 + Retry-After + a JSON body
// naming the state.
func (h *Handler) answerWait(w http.ResponseWriter, action Action) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(action.DeadlineStatus)
	_ = json.NewEncoder(w).Encode(waitBody{Error: "model not ready", State: action.DeadlineState})
}

func (h *Handler) acquirePending(model string) bool {
	if h.MaxPendingPerModel <= 0 {
		return true
	}
	h.pendingMu.Lock()
	defer h.pendingMu.Unlock()
	if h.pending == nil {
		h.pending = make(map[string]int)
	}
	if h.pending[model] >= h.MaxPendingPerModel {
		return false
	}
	h.pending[model]++
	return true
}

func (h *Handler) releasePending(model string) {
	if h.MaxPendingPerModel <= 0 {
		return
	}
	h.pendingMu.Lock()
	defer h.pendingMu.Unlock()
	h.pending[model]--
}

// modelFromRequest peeks the OpenAI-compatible request body's top-level
// "model" field and restores the body for forwarding — squall-proxy sits
// behind LiteLLM's `type: kubeai` discovery (F27, F30), so every request
// this handler sees already carries "model": "<id>" matching what
// ModelsHandler listed.
//
// errStatus is 0 on success, otherwise the HTTP status the caller must
// answer: 400 for a missing/unreadable model field, 413 for a body over
// the buffering ceiling (D120 — routing needs the parsed body, so a body
// too large to buffer is one squall declines to route, and the caller
// deserves to be told THAT rather than "missing model").
func modelFromRequest(r *http.Request) (model string, errStatus int) {
	if r.Body == nil {
		return "", http.StatusBadRequest
	}
	// BOUNDED. This runs before routing has even chosen a model, so an
	// unbounded ReadAll let any unauthenticated caller pin the proxy's heap
	// with one request. Same ceiling as readReplayableBody, which buffers
	// this same body again downstream.
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxReplayableBody+1))
	if err != nil {
		return "", http.StatusBadRequest
	}
	if len(raw) > maxReplayableBody {
		return "", http.StatusRequestEntityTooLarge
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))

	var peek struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return "", http.StatusBadRequest
	}
	peek.Model = strings.TrimSpace(peek.Model)
	if peek.Model == "" {
		return "", http.StatusBadRequest
	}
	return peek.Model, 0
}
