// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

type stubBackend struct{ target *url.URL }

func (b stubBackend) URL(string) (*url.URL, bool) {
	if b.target == nil {
		return nil, false
	}
	return b.target, true
}

func TestHandler_RequestCeilingReleasesInFlight(t *testing.T) {
	c := NewCache()
	c.Set("m", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseWaking, Schedulable: true, HoldTimeout: time.Hour})
	h := newHandler(t, c, nil)
	h.MaxRequestDuration = 50 * time.Millisecond
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, chatRequest("m")); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request ceiling did not return")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", rec.Code)
	}
	if got := h.Activity.Report().Models["m"].InFlight; got != 0 {
		t.Fatalf("inFlight=%d", got)
	}
}

type noopPatcher struct{}

func (noopPatcher) PatchDemand(context.Context, string, time.Time) error { return nil }

// countingPatcher records how many demand patches actually reached the API.
type countingPatcher struct{ n *int32 }

func (c countingPatcher) PatchDemand(context.Context, string, time.Time) error {
	atomic.AddInt32(c.n, 1)
	return nil
}

func newHandler(t *testing.T, cache *ModelCache, backendTarget *url.URL) *Handler {
	t.Helper()
	return &Handler{
		Cache:              cache,
		Demand:             NewDemandCoalescer(noopPatcher{}, time.Minute, nil),
		Activity:           NewActivityTracker(nil),
		Backend:            stubBackend{target: backendTarget},
		RefreshInterval:    5 * time.Millisecond,
		MaxPendingPerModel: 0,
	}
}

func chatRequest(model string) *http.Request {
	body, _ := json.Marshal(map[string]string{"model": model})
	return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
}

func TestHandler_MissingModel_BadRequest(t *testing.T) {
	h := newHandler(t, NewCache(), nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandler_NoSuchModel_404Immediately(t *testing.T) {
	h := newHandler(t, NewCache(), nil)
	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatRequest("ghost"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("no-CR case blocked for %v, want immediate", elapsed)
	}
}

func TestHandler_Draining_404ImmediatelyNoBlock(t *testing.T) {
	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseDraining, HoldTimeout: time.Hour})
	h := newHandler(t, c, nil)

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatRequest("qwen"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (Draining: new requests never block)", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Draining blocked for %v, want immediate", elapsed)
	}
}

func TestHandler_Ready_ForwardsToBackend(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)

	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady})
	h := newHandler(t, c, target)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatRequest("qwen"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Fatalf("body = %q, want the upstream body forwarded verbatim", rec.Body.String())
	}
}

func TestHandler_Asleep_TimesOutWithWaitContract_ZeroBytesBeforeOutcome(t *testing.T) {
	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep, HoldTimeout: 60 * time.Millisecond, Schedulable: true})
	h := newHandler(t, c, nil)

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	start := time.Now()
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(mustJSON(map[string]string{"model": "qwen"})))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close on a read-only GET.
	elapsed := time.Since(start)

	// The client received nothing at all until the full round trip
	// completed (a real network hop, not a ResponseRecorder) — so a
	// response arriving before the hold's deadline elapsed would itself
	// prove bytes were written early.
	if elapsed < 50*time.Millisecond {
		t.Fatalf("response arrived after only %v, before the 60ms hold's deadline", elapsed)
	}

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("Retry-After header missing")
	}
	var body waitBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.State != WaitAsleep {
		t.Fatalf("state = %q, want %q", body.State, WaitAsleep)
	}
}

func TestHandler_UnblocksAndForwardsWhenModelBecomesReadyMidWait(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)

	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep, HoldTimeout: 2 * time.Second, Schedulable: true})
	h := newHandler(t, c, target)

	go func() {
		time.Sleep(20 * time.Millisecond)
		c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady})
	}()

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatRequest("qwen"))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the model turns Ready mid-wait", rec.Code)
	}
	if elapsed >= time.Second {
		t.Fatalf("took %v, want well under the 2s hold deadline", elapsed)
	}
}

func TestHandler_MaxPendingPerModel_NPlusOnethAnsweredImmediately(t *testing.T) {
	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep, HoldTimeout: time.Second, Schedulable: true})
	h := newHandler(t, c, nil)
	h.MaxPendingPerModel = 2

	results := make(chan struct {
		code    int
		elapsed time.Duration
	}, 3)

	start := time.Now()
	for i := 0; i < 3; i++ {
		go func() {
			reqStart := time.Now()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, chatRequest("qwen"))
			results <- struct {
				code    int
				elapsed time.Duration
			}{rec.Code, time.Since(reqStart)}
		}()
		time.Sleep(5 * time.Millisecond) // stagger so the 3rd clearly arrives after the first 2 are held.
	}

	var immediate, held int
	deadline := time.After(3 * time.Second)
	for i := 0; i < 3; i++ {
		select {
		case r := <-results:
			if r.code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", r.code)
			}
			if r.elapsed < 500*time.Millisecond {
				immediate++
			} else {
				held++
			}
		case <-deadline:
			t.Fatal("results did not arrive within 3s")
		}
	}
	_ = start

	if immediate != 1 || held != 2 {
		t.Fatalf("immediate=%d held=%d, want exactly 1 immediate (the 3rd, over MaxPendingPerModel=2) and 2 held to their deadline", immediate, held)
	}
}

func TestHandler_DemandSignalledWhileHeld(t *testing.T) {
	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep, HoldTimeout: 40 * time.Millisecond, Schedulable: true})
	p := &fakePatcher{}
	h := newHandler(t, c, nil)
	h.Demand = NewDemandCoalescer(p, time.Millisecond, nil) // tiny cooldown: every refresh tick should get through.
	h.RefreshInterval = 10 * time.Millisecond

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatRequest("qwen"))

	if got := p.count(); got < 2 {
		t.Fatalf("demand patch calls = %d, want at least 2 (an initial signal plus at least one refresh, not a one-shot write)", got)
	}
}

func TestHandler_GatewayForbidden_TranslatedTo502WithAlarm(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("auth fault"))
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)

	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady})
	h := newHandler(t, c, target)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatRequest("qwen"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (F23: gateway 403 is an auth fault, never a wake)", rec.Code)
	}
}

// TestHandler_InFlightCountedWhileHeld_NotOnlyAfterForward pins D11/D34's
// ordering at the Handler level: Activity.Begin() must run before the
// blocking hold, not after it, so a request that is currently held (not yet
// forwarded) is already visible to the sleep evaluation's inFlight count.
// See docs/references/deviations-and-findings.md D34.
func TestHandler_InFlightCountedWhileHeld_NotOnlyAfterForward(t *testing.T) {
	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep, HoldTimeout: 2 * time.Second, Schedulable: true})
	h := newHandler(t, c, nil)

	done := make(chan struct{})
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, chatRequest("qwen"))
		close(done)
	}()

	// Give ServeHTTP time to reach and enter the blocking hold.
	time.Sleep(50 * time.Millisecond)

	report := h.Activity.Report()
	activity, ok := report.Models["qwen"]
	if !ok || activity.InFlight < 1 {
		t.Fatalf("activity for a request currently held (not yet forwarded) = %+v (present=%v), want InFlight >= 1", activity, ok)
	}

	// Unblock so the goroutine finishes before the test returns.
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("held request never completed after model turned Ready")
	}
}

func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestHandler_HoldRetriesForward_ZeroBytesUntilCommit is §7's oracle and
// F32's invariant together: the held request retries the ACTUAL forward,
// 503 keeps holding, and nothing reaches the client until a commit.
func TestHandler_HoldRetriesForward_ZeroBytesUntilCommit(t *testing.T) {
	var attempts int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"chat.completion"}`))
	}))
	t.Cleanup(upstream.Close)

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}

	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep, HoldTimeout: 10 * time.Second, Schedulable: true})
	h := &Handler{
		Cache:           c,
		Demand:          NewDemandCoalescer(noopPatcher{}, 0, nil),
		Activity:        NewActivityTracker(nil),
		Backend:         staticBackend{u: u},
		RefreshInterval: 20 * time.Millisecond,
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen","messages":[]}`))
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the retry eventually committed): body=%q", rec.Code, rec.Body.String())
	}
	// The commit lands on the third 20ms tick. Holding past it would deliver a
	// finished response a full holdTimeout late, with the upstream body open
	// and unread the whole time — the exact opposite of §7's "a token can be
	// served even before phase: Ready is written".
	if elapsed > 2*time.Second {
		t.Fatalf("ServeHTTP took %v with a 10s holdTimeout: the hold must END at the commit, not wait out the deadline", elapsed)
	}
	if got := atomic.LoadInt32(&attempts); got < 3 {
		t.Fatalf("upstream attempts = %d, want >= 3 (503s must be retried, not committed)", got)
	}
	if !strings.Contains(rec.Body.String(), "chat.completion") {
		t.Fatalf("body = %q, want the committed completion", rec.Body.String())
	}
	if got := h.Activity.Report().Models["qwen"].LastSuccessAt; got.IsZero() {
		t.Fatal("LastSuccessAt is zero after a committed forward, want it set (§6 evidence (b))")
	}
}

// TestHandler_HoldRetries_DeadlineAnswersWaitContract: every attempt
// retryable until the deadline -> the §7 wait contract, unchanged, and no
// upstream error body leaks to the client.
func TestHandler_HoldRetries_DeadlineAnswersWaitContract(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`UPSTREAM-503-BODY`))
	}))
	t.Cleanup(upstream.Close)

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}

	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep, HoldTimeout: 150 * time.Millisecond, Schedulable: true})
	h := &Handler{
		Cache:           c,
		Demand:          NewDemandCoalescer(noopPatcher{}, 0, nil),
		Activity:        NewActivityTracker(nil),
		Backend:         staticBackend{u: u},
		RefreshInterval: 20 * time.Millisecond,
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (the wait contract)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "UPSTREAM-503-BODY") {
		t.Fatalf("body = %q, want squall's wait contract, never the upstream body", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model not ready") {
		t.Fatalf("body = %q, want the wait-contract JSON", rec.Body.String())
	}
	if got := h.Activity.Report().Models["qwen"].LastSuccessAt; !got.IsZero() {
		t.Fatal("LastSuccessAt set after only-retryable attempts, want zero (success is commit-only)")
	}
}

// TestHandler_ReadyButGatewayNotYetRegistered_KeepsHoldingThenWaitContract is
// LIVE-6, reproduced at the handler level: the phase flips to Ready mid-hold
// (the controller's view: probe green, §6 evidence (a)) while dstack's
// service proxy keeps answering its own "service not found" 404 for the
// whole rest of the hold — the measured transition race where the probe
// goes green before the service-proxy route is registered. The request must
// NOT commit that 404; it must keep retrying, and when holdTimeout expires
// the caller must see §7's wait contract, never dstack's raw body.
func TestHandler_ReadyButGatewayNotYetRegistered_KeepsHoldingThenWaitContract(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Service main/qwen3-8-27b not found"}`))
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)

	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseWaking, HoldTimeout: 150 * time.Millisecond, Schedulable: true})
	h := &Handler{
		Cache:           c,
		Demand:          NewDemandCoalescer(noopPatcher{}, 0, nil),
		Activity:        NewActivityTracker(nil),
		Backend:         staticBackend{u: target},
		RefreshInterval: 15 * time.Millisecond,
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		// The controller says Ready (probe green); dstack's service proxy
		// has not registered the route yet — the upstream handler above
		// keeps answering 404 regardless of this flip.
		c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady, Schedulable: true})
	}()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatRequest("qwen"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (the wait contract) — a bare 404 here is LIVE-6 recurring", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "qwen3-8-27b not found") {
		t.Fatalf("body = %q, want squall's wait contract — dstack's raw 404 body must never reach the caller", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model not ready") {
		t.Fatalf("body = %q, want the wait-contract JSON", rec.Body.String())
	}
}

type staticBackend struct{ u *url.URL }

func (s staticBackend) URL(string) (*url.URL, bool) { return s.u, s.u != nil }

// noopDemand is a DemandCoalescer wired to a no-op patcher, for tests that
// need a Handler.Demand but never assert on what it writes.
func noopDemand(t *testing.T) *DemandCoalescer {
	t.Helper()
	return NewDemandCoalescer(noopPatcher{}, time.Minute, nil)
}

// TestServeHTTP_RewritesTheModelFieldToTheServedName: callers address a
// Model by its Kubernetes name. Every engine is made to answer to that name
// (vLLM --served-model-name, Ollama `ollama cp`), so this is normally a
// no-op — but when status.servedModel says otherwise, the upstream body must
// carry what the ENGINE knows or it answers 404 for its own model.
func TestServeHTTP_RewritesTheModelFieldToTheServedName(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = body.Model
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cache := NewCache()
	cache.Set("m", ModelSnapshot{
		Phase: squallv1alpha1.ModelPhaseReady, ServiceURL: "/",
		// D100: ForwardModel drives the rewrite. ServedModel deliberately
		// carries the JOINED diagnostic report here — if the proxy ever
		// reads that field again, the upstream sees a name that exists
		// nowhere and this test says so.
		ServedModel:  "qwen3:8b,Qwen/Qwen3-8B",
		ForwardModel: "qwen3:8b",
	})
	h := &Handler{
		Cache:    cache,
		Demand:   noopDemand(t),
		Activity: NewActivityTracker(nil),
		Backend:  StatusBackend{Cache: cache, DstackBaseURL: upstream.URL},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "qwen3:8b" {
		t.Fatalf("upstream saw model=%q, want the forward name qwen3:8b", seen)
	}
}

// TestServeHTTP_NoForwardModel_LeavesTheCallersModelAlone is D100's other
// half: when the controller found no safe single name (a mismatch, or
// several served ids with nothing to disambiguate them), the proxy must
// forward what the caller sent rather than invent a target.
func TestServeHTTP_NoForwardModel_LeavesTheCallersModelAlone(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = body.Model
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cache := NewCache()
	cache.Set("m", ModelSnapshot{
		Phase: squallv1alpha1.ModelPhaseReady, ServiceURL: "/",
		ServedModel:  "a,b", // reported, but no single safe answer
		ForwardModel: "",
	})
	h := &Handler{
		Cache:    cache,
		Demand:   noopDemand(t),
		Activity: NewActivityTracker(nil),
		Backend:  StatusBackend{Cache: cache, DstackBaseURL: upstream.URL},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "m" {
		t.Fatalf("upstream saw model=%q, want the caller's own %q left untouched", seen, "m")
	}
}

// TestServeHTTP_UnschedulableAnswersImmediately: holding a request for
// spec.holdTimeout is right while a model is COMING UP. When the controller
// has already established it cannot provision at all (no backend, no fleet),
// a 20-minute hold is 20 minutes of lying to the caller.
func TestServeHTTP_UnschedulableAnswersImmediately(t *testing.T) {
	cache := NewCache()
	cache.Set("m", ModelSnapshot{
		Phase:       squallv1alpha1.ModelPhaseAsleep,
		HoldTimeout: time.Hour,
		Schedulable: false,
	})
	h := &Handler{Cache: cache, Demand: noopDemand(t), Activity: NewActivityTracker(nil)}

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[]}`)))
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("held an unschedulable model; the caller should be told, not stalled")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestServeHTTP_UnschedulableStillSignalsDemand is the other half of
// TestServeHTTP_UnschedulableAnswersImmediately, and it exists because those
// two behaviours were in direct conflict.
//
// preflight() — the ONLY thing that ever rewrites the Schedulable condition —
// runs on the wake path (model_controller.go), and the wake path is reached
// only when demand exists. So refusing to signal demand for an unschedulable
// Model closes the loop on itself: no demand, no wake attempt, no preflight,
// and the stale False that started it is never revisited.
//
// MEASURED 2026-09-01: a helm upgrade dropped the vastai backend (D67), every
// Model went Schedulable=False, and restoring the backend did not bring them
// back — 7 minutes after the backend answered config_info with 200 again, the
// Model was still refusing to wake, and would have refused forever.
//
// Not holding the caller is right (that is the test above). Not telling the
// controller anything happened is what makes it permanent, and it inverts the
// project's `0->1 fails open` invariant on the one path that invariant is about.
func TestServeHTTP_UnschedulableStillSignalsDemand(t *testing.T) {
	var patched int32
	cache := NewCache()
	cache.Set("m", ModelSnapshot{
		Phase:       squallv1alpha1.ModelPhaseAsleep,
		HoldTimeout: time.Hour,
		Schedulable: false,
	})
	h := &Handler{
		Cache:    cache,
		Demand:   NewDemandCoalescer(countingPatcher{n: &patched}, time.Minute, nil),
		Activity: NewActivityTracker(nil),
	}

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[]}`)))
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("held an unschedulable model; caller should be told, not stalled")
	}

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := atomic.LoadInt32(&patched); got != 1 {
		t.Fatalf("demand patches = %d, want 1: without one, preflight never re-runs and "+
			"Schedulable=False can never be cleared by a caller", got)
	}
}

// TestRefreshIntervalFor is LIVE-3, corrected: the refresh cadence must be
// derived per-Model from scaleDownDelaySeconds, not from one proxy-wide
// constant — a single flat interval cannot be right for every Model at once
// (measured live: a 300s production Model and a 2s e2e fixture disagreed
// about their TTL in the same cluster).
func TestRefreshIntervalFor(t *testing.T) {
	tests := []struct {
		name                  string
		scaleDownDelaySeconds int32
		ceiling               time.Duration
		want                  time.Duration
	}{
		{
			name:                  "e2e fixture's 2s TTL floors to 500ms, reproducing the former hand-tuned override",
			scaleDownDelaySeconds: 2,
			ceiling:               30 * time.Second,
			want:                  500 * time.Millisecond,
		},
		{
			name:                  "production-representative 300s TTL derives exactly to the 1/10 fraction",
			scaleDownDelaySeconds: 300,
			ceiling:               30 * time.Second,
			want:                  30 * time.Second,
		},
		{
			name:                  "no TTL configured (<=0) falls back to the ceiling entirely",
			scaleDownDelaySeconds: 0,
			ceiling:               45 * time.Second,
			want:                  45 * time.Second,
		},
		{
			name:                  "very large TTL is clamped to the ceiling, never left unbounded",
			scaleDownDelaySeconds: 36000, // 10h
			ceiling:               time.Minute,
			want:                  time.Minute,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := refreshIntervalFor(tc.scaleDownDelaySeconds, tc.ceiling); got != tc.want {
				t.Fatalf("refreshIntervalFor(%d, %v) = %v, want %v", tc.scaleDownDelaySeconds, tc.ceiling, got, tc.want)
			}
		})
	}
}

// TestHandler_ServeHTTP_UsesPerModelRefreshInterval_NotTheCeiling proves
// ServeHTTP actually WIRES refreshIntervalFor's derivation into the hold —
// TestRefreshIntervalFor alone only proves the pure function is right in
// isolation, which would stay green even if ServeHTTP quietly kept passing
// h.RefreshInterval straight through. This Model's ScaleDownDelaySeconds (2s)
// derives to a 500ms refresh (floor-clamped); h.RefreshInterval is set to a
// much larger 5s ceiling that would let only the initial tick fire before the
// hold's own deadline if the derivation were bypassed.
func TestHandler_ServeHTTP_UsesPerModelRefreshInterval_NotTheCeiling(t *testing.T) {
	c := NewCache()
	c.Set("qwen", ModelSnapshot{
		Phase:                 squallv1alpha1.ModelPhaseAsleep,
		HoldTimeout:           900 * time.Millisecond,
		ScaleDownDelaySeconds: 2,
		Schedulable:           true,
	})
	p := &fakePatcher{}
	h := newHandler(t, c, nil)
	h.Demand = NewDemandCoalescer(p, time.Millisecond, nil) // tiny cooldown: every refresh tick should get through.
	h.RefreshInterval = 5 * time.Second                     // the ceiling; must NOT be used directly here.

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatRequest("qwen"))

	if got := p.count(); got < 2 {
		t.Fatalf("demand patch calls = %d, want at least 2 (the model's own 2s TTL should derive a ~500ms refresh; "+
			"if ServeHTTP used the 5s ceiling directly, only the initial tick would fire in a 900ms hold)", got)
	}
}

// TestHandler_ReadyButGatewayLate_RetriesWithinBudgetAndCommits is D86
// (LIVE-8). Measured on a live Vast.ai run: status.phase flips to Ready a
// moment BEFORE dstack's service proxy finishes wiring the route. The handler
// used to attempt exactly one forward at that instant and, on its failure,
// answer the wait contract — so a caller that had already waited 399 seconds
// received an error with ~13 of its 20 minutes of holdTimeout unspent.
//
// The gateway here comes up on a WALL-CLOCK instant rather than after N
// attempts on purpose: an attempt-counting stub would also be satisfied by
// the single post-hold attempt the old code made, and the test would pass
// against the bug it exists to catch.
func TestHandler_ReadyButGatewayLate_RetriesWithinBudgetAndCommits(t *testing.T) {
	const gatewayUpAfter = 200 * time.Millisecond
	start := time.Now()
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		if time.Since(start) < gatewayUpAfter {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Service main/qwen not found"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"served":true}`))
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)

	c := NewCache()
	// Budget far exceeds the gateway's delay: the whole point is that the
	// deadline is NOT what runs out here.
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseWaking, HoldTimeout: 5 * time.Second, Schedulable: true})
	h := &Handler{
		Cache:           c,
		Demand:          NewDemandCoalescer(noopPatcher{}, 0, nil),
		Activity:        NewActivityTracker(nil),
		Backend:         staticBackend{u: target},
		RefreshInterval: 15 * time.Millisecond,
	}

	go func() {
		// Ready well before the gateway actually serves — this is the race.
		time.Sleep(20 * time.Millisecond)
		c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady, Schedulable: true})
	}()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatRequest("qwen"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the model was Ready with seconds of hold budget left, so the proxy must keep trying rather than surrender at the finish line (D86)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"served":true`) {
		t.Fatalf("body = %q, want the upstream response once the gateway came up", rec.Body.String())
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("took %v: the retry must run at the hold cadence, not wait out the whole deadline", elapsed)
	}
	if n := attempts.Load(); n < 2 {
		t.Fatalf("upstream saw %d attempts, want >= 2 (one that failed, one that succeeded)", n)
	}
}

// TestHandler_ReadyOnArrival_GatewayFails_DoesNotRetry pins D86's SCOPE. A
// request that never held has no hold budget to spend, so a transient
// upstream failure on the hot path must still answer the wait contract
// immediately. Without this, D86's fix would turn every 502 into a
// multi-second stall for every caller.
func TestHandler_ReadyOnArrival_GatewayFails_DoesNotRetry(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Service main/qwen not found"}`))
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)

	c := NewCache()
	// Ready on arrival: Decide never blocks, so the hold path is never entered
	// and holdStart stays zero. A large HoldTimeout is set deliberately — if
	// the retry were keyed off the timeout rather than off having actually
	// held, this test would hang for 10s instead of returning at once.
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady, HoldTimeout: 10 * time.Second, Schedulable: true})
	h := &Handler{
		Cache:           c,
		Demand:          NewDemandCoalescer(noopPatcher{}, 0, nil),
		Activity:        NewActivityTracker(nil),
		Backend:         staticBackend{u: target},
		RefreshInterval: 15 * time.Millisecond,
	}

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatRequest("qwen"))
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if elapsed > time.Second {
		t.Fatalf("took %v, want an immediate answer: a request that never held has no budget to spend (D86 scope)", elapsed)
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("upstream saw %d attempts, want exactly 1: the hot path must not retry", n)
	}
}

// TestCommit_SuccessOnlyOnDeliveredTwoXX pins evidence (b) to what it claims
// to mean. Before 2026-08-31 a 500 recorded a success, which made the
// controller's "no successful response in 15 minutes" teardown unable to fire
// against exactly the replica it exists to catch: one answering, and answering
// badly.
func TestCommit_SuccessOnlyOnDeliveredTwoXX(t *testing.T) {
	for _, tt := range []struct {
		name         string
		status       int
		wantSuccess  bool
		wantFailures int
	}{
		{"200 counts as success", http.StatusOK, true, 0},
		{"204 counts as success", http.StatusNoContent, true, 0},
		{"500 is a failure", http.StatusInternalServerError, false, 1},
		{"429 is a failure", http.StatusTooManyRequests, false, 1},
		{"400 is a failure", http.StatusBadRequest, false, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer backend.Close()
			u, err := url.Parse(backend.URL)
			if err != nil {
				t.Fatalf("parse backend url: %v", err)
			}

			c := NewCache()
			c.Set("m", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady, Schedulable: true})
			h := newHandler(t, c, u)

			h.ServeHTTP(httptest.NewRecorder(), chatRequest("m"))

			rep := h.Activity.Report().Models["m"]
			if got := !rep.LastSuccessAt.IsZero(); got != tt.wantSuccess {
				t.Fatalf("recorded success = %v, want %v (status %d)", got, tt.wantSuccess, tt.status)
			}
			if rep.FailuresSinceSuccess != tt.wantFailures {
				t.Fatalf("failures = %d, want %d (status %d)", rep.FailuresSinceSuccess, tt.wantFailures, tt.status)
			}
		})
	}
}

// TestModelFromRequest_BoundsTheBody is F3: modelFromRequest runs BEFORE
// routing has picked a model, so an unbounded io.ReadAll there let any
// caller pin the proxy's heap with a single request — the bounded
// readReplayableBody downstream never got a say. Mutation check: drop the
// LimitReader and the oversized case starts returning ("qwen3-8-27b", true).
func TestModelFromRequest_BoundsTheBody(t *testing.T) {
	t.Run("normal body is peeked and restored", func(t *testing.T) {
		raw := `{"model":"qwen3-8-27b","messages":[]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw))

		model, errStatus := modelFromRequest(r)
		if errStatus != 0 || model != "qwen3-8-27b" {
			t.Fatalf("modelFromRequest = %q, %d; want %q, 0", model, errStatus, "qwen3-8-27b")
		}
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read restored body: %v", err)
		}
		if string(got) != raw {
			t.Errorf("restored body = %q, want %q", got, raw)
		}
		if r.ContentLength != int64(len(raw)) {
			t.Errorf("ContentLength = %d, want %d", r.ContentLength, len(raw))
		}
	})

	t.Run("oversized body is refused, not buffered whole", func(t *testing.T) {
		// Valid JSON with the model field FIRST: a truncating implementation
		// would happily parse it. Only a size check rejects this.
		pad := strings.Repeat("a", maxReplayableBody+1024)
		raw := `{"model":"qwen3-8-27b","pad":"` + pad + `"}`

		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw))
		model, errStatus := modelFromRequest(r)
		// D120: over-cap is 413, an honest answer about SIZE — not the 400
		// "missing model" it used to be blamed on.
		if errStatus != http.StatusRequestEntityTooLarge {
			t.Fatalf("modelFromRequest = %q, %d; want 413 on a %d-byte body", model, errStatus, len(raw))
		}
	})
}

// TestForwardFailure_ClientDisconnectIsNotAReplicaFailure is D99, MEASURED
// LIVE 2026-08-31. A benchmark client was pkill'd with 64 requests in flight
// against a Ready model; every one of them cancelled its request context, and
// squall recorded 64 "replica failures" for a GPU that was serving normally
// three minutes later. failuresSinceSuccess feeds unhealthyDue with a
// threshold of 3, so client-side weather -- a pkill, an OOM, a redeploy, or
// the 27,998 socket resets that same client logged in 21 hours on 2026-08-28
// -- could drive a teardown of a working replica. `1->0 fails safe` forbids
// that: a caller that hung up is evidence about the CALLER, never about the
// replica.
//
// The 503 subtest is the other half of the same rule and must stay: a genuine
// upstream failure is still a failure. Without it, "never count anything"
// would pass.
func TestForwardFailure_ClientDisconnectIsNotAReplicaFailure(t *testing.T) {
	t.Run("caller hangs up mid-forward -> not counted", func(t *testing.T) {
		reached := make(chan struct{})
		released := make(chan struct{})
		backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			close(reached)
			<-released // hold the request open, exactly like a long generation
		}))
		defer backend.Close()
		defer close(released)

		u, err := url.Parse(backend.URL)
		if err != nil {
			t.Fatalf("parse backend url: %v", err)
		}
		c := NewCache()
		c.Set("m", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady, Schedulable: true})
		h := newHandler(t, c, u)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-reached // only cancel once the forward is genuinely in flight
			cancel()
		}()

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, chatRequest("m").WithContext(ctx))

		if got := h.Activity.Report().Models["m"].FailuresSinceSuccess; got != 0 {
			t.Fatalf("failuresSinceSuccess = %d, want 0: the caller hung up, "+
				"which says nothing about the replica (D99)", got)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("wrote status %d to an already-closed socket, want nothing written", rec.Code)
		}
	})

	t.Run("upstream 503 with the caller still there -> counted", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer backend.Close()

		u, err := url.Parse(backend.URL)
		if err != nil {
			t.Fatalf("parse backend url: %v", err)
		}
		c := NewCache()
		c.Set("m", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady, Schedulable: true})
		h := newHandler(t, c, u)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, chatRequest("m"))

		if got := h.Activity.Report().Models["m"].FailuresSinceSuccess; got != 1 {
			t.Fatalf("failuresSinceSuccess = %d, want 1: the Model was advertised "+
				"Ready and this caller got nothing", got)
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (the wait contract)", rec.Code)
		}
	})
}
