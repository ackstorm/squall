# Serving Path Implementation Plan — forward-retry and readiness

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make squall serve a real token — the held request retries the actual forward and streams on success, and `phase: Ready` finally has a writer.

**Architecture:** Two independent evidences promote a Model to `Ready` (spec §6): dstack's own probe state read off `JobSubmission.probes` (primary), and first-party forward success reported by the proxy as `lastSuccessAt` (confirmation). The proxy's hold stops being passive — while blocked it periodically retries the user's real request against the gateway, treating 502/503/connection-refused as "still waking" and anything else as a commit. Squall probes nothing, ever.

**Tech Stack:** Go 1.26.6, controller-runtime, client-go dynamic informers, plain `net/http` (no new dependencies), `testing` + `httptest`, Ginkgo for e2e only.

## Global Constraints

- **Every `go`/`make` command runs through `./scripts/dev.sh`.** The host Go is the wrong version. Never bare `go`/`make`. Never `DOCKER_BUILDKIT=0`.
- **The lint target is `qa-lint`**, not `lint`.
- Every `.go` file starts with `// SPDX-License-Identifier: Apache-2.0` as its first line.
- Unit + envtest use plain `testing` with table-driven cases. **Ginkgo is e2e-only.**
- `make test-unit` must **never** need a control plane. Pure tests run with no skip; envtest cases skip under `-short` via `suite_test.go`'s `TestMain`.
- **No naked polling loops.** Every wait needs an upper bound **and** an explicit failure path. Test goroutines tear down via `t.Cleanup` — no leaks, no writes to `t` after the test returns.
- Doc comments explain WHY and cite `F<n>` / `§<n>` / `AC<n>`.
- **Git:** stage explicit paths only, never `git add -A`/`git add .`. Never `git reset`, `--amend`, rebase, `git checkout -- <file>`, `git restore`, `git stash`. Only ADD commits.
- **Testing cadence:** do NOT run full gates after every task. Run scoped package tests as you go; run `make test-unit`, `make test-envtest`, `make qa-lint` and the e2e suite **once**, at the end of the block. Run the mutation sweep as **one pass** at the end.
- **Out of scope, do not start:** `provisioningTimeout`/`maxLifetime` behaviour (task 8.3, ledger D7) — implementing it before this block lands destroys every model at the deadline in a recreate loop; rendering `probes:` into a dstack service config (§8 engine-template work); the Ollama warmup caveat (container-side).
- **F32 is the invariant that must not break:** nothing may be written to the client before the outcome is known. A premature `200` silently removes the model from LiteLLM's fallback chain.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/proxy/attempt.go` *(new)* | One outbound forward attempt; retry-vs-commit classification; streaming commit |
| `internal/proxy/attempt_test.go` *(new)* | Attempt/classify/stream unit tests |
| `internal/proxy/handler.go` *(modify)* | Hold loop drives attempts; commit path replaces `forward` |
| `internal/proxy/activity.go` *(modify)* | `Success()` records `lastSuccessAt` |
| `api/squall/v1alpha1/activity.go` *(modify)* | `LastSuccessAt` on the wire contract |
| `internal/dstack/client.go` *(modify)* | `Run.ProbesReady`, `Run.SubmittedAt`; narrow the FROZEN rule |
| `internal/dstack/mock/mock.go` *(modify)* | Probe state as a real state with duration, driven by the clock |
| `internal/controller/squall/activity.go` *(modify)* | Aggregate `NewestLastSuccessAt` without changing `Complete`/`AllIdle` |
| `internal/controller/squall/model_controller.go` *(modify)* | `observe` sets `Ready`; journal `status.wakeStartedAt` |
| `api/squall/v1alpha1/model_types.go` *(modify)* | `status.wakeStartedAt`; printer columns |
| `test/e2e/e2e_test.go` *(modify)* | Delete the D38 controller-pause hack |

---

### Task 1: Activity wire carries `lastSuccessAt`

**Files:**
- Modify: `api/squall/v1alpha1/activity.go`
- Modify: `internal/proxy/activity.go`
- Test: `internal/proxy/activity_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: `squallv1alpha1.ModelActivity.LastSuccessAt time.Time`; `(*ActivityTracker).Success(model string)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/proxy/activity_test.go`:

```go
// TestActivityTracker_Success_RecordsLastSuccessAt pins §6's evidence (b):
// a committed forward is first-party proof the engine is serving.
func TestActivityTracker_Success_RecordsLastSuccessAt(t *testing.T) {
	fake := clock.NewFakeClock(time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC))
	tr := NewActivityTracker(fake)

	done := tr.Begin("qwen")
	if got := tr.Report().Models["qwen"].LastSuccessAt; !got.IsZero() {
		t.Fatalf("LastSuccessAt = %v before any success, want zero", got)
	}

	fake.Advance(5 * time.Second)
	tr.Success("qwen")
	done()

	want := fake.Now()
	if got := tr.Report().Models["qwen"].LastSuccessAt; !got.Equal(want) {
		t.Fatalf("LastSuccessAt = %v, want %v", got, want)
	}
}

// TestActivityTracker_Success_UnknownModel_DoesNotPanic: Success may race a
// map entry that Begin created and a later Report drained.
func TestActivityTracker_Success_UnknownModel_DoesNotPanic(t *testing.T) {
	tr := NewActivityTracker(clock.NewFakeClock(time.Now()))
	tr.Success("never-seen")
	if _, ok := tr.Report().Models["never-seen"]; !ok {
		t.Fatal("Success on an unknown model should create its entry, not drop the evidence")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `./scripts/dev.sh go test ./internal/proxy/ -run TestActivityTracker_Success -count=1`
Expected: FAIL — `tr.Success undefined` and `LastSuccessAt` unknown field.

- [ ] **Step 3: Add the wire field**

In `api/squall/v1alpha1/activity.go`, inside `ModelActivity`, after `LastRequestAt`:

```go
	// LastSuccessAt is when this replica last COMMITTED a forward for the
	// Model — i.e. the gateway answered something other than 502/503 and
	// the response was streamed to a real client. It is §6's evidence (b):
	// first-party proof the engine is serving, distinct from LastRequestAt
	// (which records acceptance, not success).
	//
	// The zero value means "no successful forward observed by this
	// replica" and is NOT ambiguous. A reader MUST treat a report that
	// omits this field entirely as zero, never as ambiguous — an older
	// proxy replica mid-rollout must not wedge §6's sleep aggregation.
	LastSuccessAt time.Time `json:"lastSuccessAt"`
```

- [ ] **Step 4: Add `Success` and surface the field**

In `internal/proxy/activity.go`, add `lastSuccessAt` to `modelCounters`:

```go
type modelCounters struct {
	inFlight      int
	lastRequestAt time.Time
	lastSuccessAt time.Time
}
```

Add the method after `Begin`:

```go
// Success records a COMMITTED forward for model — the gateway answered and
// the response is being streamed to a real client. This is §6's evidence
// (b): the held request is the serving path's own oracle, so a first-party
// success is proof of readiness without squall probing anything (§7).
// Called on commit, never on a retryable attempt.
func (t *ActivityTracker) Success(model string) {
	now := t.clock.Now()

	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.models[model]
	if !ok {
		c = &modelCounters{}
		t.models[model] = c
	}
	c.lastSuccessAt = now
}
```

In `Report`, add the field to the constructed `ModelActivity`:

```go
		report.Models[name] = squallv1alpha1.ModelActivity{
			InFlight:      c.inFlight,
			LastRequestAt: c.lastRequestAt,
			LastSuccessAt: c.lastSuccessAt,
		}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `./scripts/dev.sh go test ./internal/proxy/ ./api/... -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add api/squall/v1alpha1/activity.go internal/proxy/activity.go internal/proxy/activity_test.go
git commit -m "feat(proxy): record lastSuccessAt as first-party readiness evidence"
```

---

### Task 2: One forward attempt, classified — never touching the client

**Files:**
- Create: `internal/proxy/attempt.go`
- Test: `internal/proxy/attempt_test.go`

**Interfaces:**
- Consumes: `Backend.URL(model string) (*url.URL, bool)` from `handler.go`.
- Produces: `type attemptResult int` with `attemptRetry`/`attemptCommit`; `classifyAttempt(resp *http.Response, err error) attemptResult`; `(*Handler).attemptForward(ctx context.Context, r *http.Request, model string, body []byte) (*http.Response, attemptResult)`; `streamCommit(w http.ResponseWriter, resp *http.Response) error`; `const maxReplayableBody = 4 << 20`.

- [ ] **Step 1: Write the failing test**

Create `internal/proxy/attempt_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClassifyAttempt is §7's retry rule: "503, 502, or connection-refused
// mean 'still waking', keep holding". Everything else commits — an engine
// answering 400 is READY, it just disliked the request.
func TestClassifyAttempt(t *testing.T) {
	tests := []struct {
		name   string
		status int
		err    error
		want   attemptResult
	}{
		{"transport error is still waking", 0, errors.New("connection refused"), attemptRetry},
		{"502 is still waking", http.StatusBadGateway, nil, attemptRetry},
		{"503 is still waking", http.StatusServiceUnavailable, nil, attemptRetry},
		{"200 commits", http.StatusOK, nil, attemptCommit},
		{"400 commits: the engine answered", http.StatusBadRequest, nil, attemptCommit},
		{"403 commits: F23 auth fault, never a wake", http.StatusForbidden, nil, attemptCommit},
		{"404 commits: F20 dead, not a wake to wait out", http.StatusNotFound, nil, attemptCommit},
		{"500 commits: the engine answered and failed", http.StatusInternalServerError, nil, attemptCommit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var resp *http.Response
			if tc.err == nil {
				resp = &http.Response{StatusCode: tc.status}
			}
			if got := classifyAttempt(resp, tc.err); got != tc.want {
				t.Fatalf("classifyAttempt(%d, %v) = %v, want %v", tc.status, tc.err, got, tc.want)
			}
		})
	}
}

// TestStreamCommit_StreamsProgressively pins that a completion is NOT
// buffered: an SSE body must reach the client as it arrives, not at EOF.
func TestStreamCommit_StreamsProgressively(t *testing.T) {
	chunks := make(chan string, 2)
	body := io.NopCloser(readerFunc(func(p []byte) (int, error) {
		s, ok := <-chunks
		if !ok {
			return 0, io.EOF
		}
		return copy(p, s), nil
	}))
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}

	rec := httptest.NewRecorder()
	seen := make(chan string, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = streamCommit(&flushRecorder{ResponseRecorder: rec, onFlush: func() {
			seen <- rec.Body.String()
		}}, resp)
	}()

	chunks <- "data: one\n"
	select {
	case got := <-seen:
		if !strings.Contains(got, "one") {
			t.Fatalf("first flush = %q, want it to contain the first chunk", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no flush within 2s: the body is being buffered, not streamed")
	}
	close(chunks)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streamCommit did not return within 2s after EOF")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want it copied from upstream", ct)
	}
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

type flushRecorder struct {
	*httptest.ResponseRecorder
	onFlush func()
}

func (f *flushRecorder) Flush() {
	f.ResponseRecorder.Flush()
	if f.onFlush != nil {
		f.onFlush()
	}
}
```

Add `"time"` to the import block.

- [ ] **Step 2: Run test to verify it fails**

Run: `./scripts/dev.sh go test ./internal/proxy/ -run 'TestClassifyAttempt|TestStreamCommit' -count=1`
Expected: FAIL — `classifyAttempt`, `attemptRetry`, `attemptCommit`, `streamCommit` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/proxy/attempt.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// maxReplayableBody bounds how much request body is buffered so a held
// request can be retried (§7). A body larger than this is forwarded ONCE,
// streamed straight from r.Body, with no retry — bounded memory beats a
// retry we cannot afford. Chat-completion bodies are prompts; 4 MiB is
// generous for that shape and is not a CRD knob.
const maxReplayableBody = 4 << 20

// attemptResult classifies one outbound try.
type attemptResult int

const (
	// attemptRetry means the gateway says the engine is not serving YET —
	// §7: "503, 502, or connection-refused mean 'still waking', keep
	// holding". Nothing has been written to the client.
	attemptRetry attemptResult = iota
	// attemptCommit means the engine ANSWERED. Stream it, stop retrying.
	// This includes 4xx and 5xx that are not 502/503: an engine returning
	// 400 is ready, it just disliked the request.
	attemptCommit
)

// classifyAttempt implements §7's rule. A transport error (connection
// refused/reset, dial timeout) is indistinguishable from "the engine has
// not bound its port yet", which is exactly the waking state.
func classifyAttempt(resp *http.Response, err error) attemptResult {
	if err != nil {
		return attemptRetry
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return attemptRetry
	default:
		return attemptCommit
	}
}

// attemptForward performs ONE outbound try and writes NOTHING to the
// client — that is what makes the retry loop safe under F32 ("nothing on
// the wire before the outcome"). On attemptRetry the response body is
// drained and closed here; on attemptCommit the caller owns the body and
// MUST close it.
//
// body is the buffered request payload for replay; when nil the request's
// own body is streamed (the over-cap, single-shot path).
func (h *Handler) attemptForward(ctx context.Context, r *http.Request, model string, body []byte) (*http.Response, attemptResult) {
	target, ok := h.Backend.URL(model)
	if !ok {
		return nil, attemptRetry
	}

	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	} else if r.Body != nil {
		payload = r.Body
	}

	out, err := http.NewRequestWithContext(ctx, r.Method, target.String()+r.URL.Path, payload)
	if err != nil {
		return nil, attemptRetry
	}
	out.Header = r.Header.Clone()
	out.Header.Del("Host")
	if body != nil {
		out.ContentLength = int64(len(body))
	}

	resp, err := http.DefaultClient.Do(out)
	result := classifyAttempt(resp, err)
	if result == attemptRetry && resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return nil, attemptRetry
	}
	if err != nil {
		return nil, attemptRetry
	}
	return resp, attemptCommit
}

// streamCommit copies a committed upstream response to the client,
// flushing as it goes so a streamed completion (SSE) reaches the caller
// progressively instead of being buffered for the length of a generation.
func streamCommit(w http.ResponseWriter, resp *http.Response) error {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// readReplayableBody buffers r's body up to maxReplayableBody. ok is false
// when the body is larger, meaning the caller must forward once without
// retrying. r.Body is always left readable from the start.
func readReplayableBody(r *http.Request) (body []byte, ok bool) {
	if r.Body == nil {
		return nil, true
	}
	limited := io.LimitReader(r.Body, maxReplayableBody+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, false
	}
	if len(buf) > maxReplayableBody {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(buf))
	return buf, true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `./scripts/dev.sh go test ./internal/proxy/ -run 'TestClassifyAttempt|TestStreamCommit' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/attempt.go internal/proxy/attempt_test.go
git commit -m "feat(proxy): add attempt/commit split for retryable forwards"
```

---

### Task 3: The held request retries the real forward

**Files:**
- Modify: `internal/proxy/handler.go`
- Test: `internal/proxy/handler_test.go`

**Interfaces:**
- Consumes: Task 2's `attemptForward`, `classifyAttempt`, `streamCommit`, `readReplayableBody`; Task 1's `(*ActivityTracker).Success`.
- Produces: `ServeHTTP` that commits via `streamCommit` and reports `Success`. `forward` is deleted.

- [ ] **Step 1: Write the failing test**

Append to `internal/proxy/handler_test.go`:

```go
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
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep, HoldTimeout: 10 * time.Second})
	h := &Handler{
		Cache:           c,
		Demand:          NewDemandCoalescer(nil, 0, nil),
		Activity:        NewActivityTracker(nil),
		Backend:         staticBackend{u: u},
		RefreshInterval: 20 * time.Millisecond,
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the retry eventually committed): body=%q", rec.Code, rec.Body.String())
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
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep, HoldTimeout: 150 * time.Millisecond})
	h := &Handler{
		Cache:           c,
		Demand:          NewDemandCoalescer(nil, 0, nil),
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

type staticBackend struct{ u *url.URL }

func (s staticBackend) URL(string) (*url.URL, bool) { return s.u, s.u != nil }
```

Ensure the import block has `"net/url"`, `"strings"`, `"sync/atomic"`, `"time"`, `"net/http"`, `"net/http/httptest"`, and `squallv1alpha1`.

- [ ] **Step 2: Run test to verify it fails**

Run: `./scripts/dev.sh go test ./internal/proxy/ -run TestHandler_HoldRetries -count=1`
Expected: FAIL — the hold returns without ever attempting a forward, so status is 503 with no upstream attempts in the first test.

- [ ] **Step 3: Rewrite `ServeHTTP`'s block and commit path**

In `internal/proxy/handler.go`, replace the body of `ServeHTTP` from the `if action.Block {` line through the closing `}` of the trailing `switch` with:

```go
	body, replayable := readReplayableBody(r)

	if action.Block {
		if !h.acquirePending(model) {
			// maxPendingPerModel exceeded: answer the wait contract
			// immediately rather than queuing a bound-defeating Nth+1 hold.
			h.answerWait(w, action)
			return
		}
		defer h.releasePending(model)

		// §7: the held real request IS the serving path's readiness oracle.
		// Each tick refreshes demand AND retries the actual forward; 502/503/
		// connection-refused mean "still waking". First-party traffic is never
		// a probe, so the two-lane rule (§10) is untouched, and a token can be
		// served before phase: Ready is ever written.
		var committed *http.Response
		tick := func() {
			h.Demand.Signal(r.Context(), model)
			if committed != nil || !replayable {
				return
			}
			if resp, res := h.attemptForward(r.Context(), r, model, body); res == attemptCommit {
				committed = resp
			}
		}

		deadline := h.clock().Now().Add(snap.HoldTimeout)
		newSnap, newHasCR, _ := Await(r.Context(), h.Cache, model, deadline, h.RefreshInterval, tick)

		if committed != nil {
			h.commit(w, committed, model)
			return
		}

		snap = newSnap
		action = Decide(newSnap.Phase, newHasCR, 0)
		if action.Block {
			h.answerWait(w, action)
			return
		}
	}

	switch {
	case action.Forward:
		resp, res := h.attemptForward(r.Context(), r, model, body)
		if res != attemptCommit {
			// Ready in cache but the gateway is not serving: answer the wait
			// contract rather than a bare 502, so the client sees a truthful
			// state (§7).
			h.answerWait(w, Action{DeadlineStatus: http.StatusServiceUnavailable, DeadlineState: WaitWaking})
			return
		}
		h.commit(w, resp, model)
	case action.ImmediateStatus != 0:
		w.WriteHeader(action.ImmediateStatus)
	default:
		http.Error(w, "internal: Decide produced no action", http.StatusInternalServerError)
	}
}

// commit streams a committed upstream response to the client and records
// §6's evidence (b). A raw upstream 403 is translated per F23 (auth fault,
// never a wake) BEFORE anything reaches the wire.
func (h *Handler) commit(w http.ResponseWriter, resp *http.Response, model string) {
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusForbidden {
		gw := Decide(squallv1alpha1.ModelPhaseReady, true, GatewayCode(resp.StatusCode))
		if gw.Alarm {
			slog.Error("gateway auth fault forwarding to model backend", "model", model, "status", resp.StatusCode)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(gw.ImmediateStatus)
		_, _ = w.Write([]byte(`{"error":"gateway auth fault"}`))
		return
	}

	h.Activity.Success(model)
	if err := streamCommit(w, resp); err != nil {
		slog.Warn("client disconnected mid-stream", "model", model, "err", err)
	}
}
```

Then **delete the old `forward` method entirely** and drop the now-unused imports (`bytes`, `io`, `net/http/httputil`, `strconv` — check with the linter rather than guessing).

- [ ] **Step 4: Run tests to verify they pass**

Run: `./scripts/dev.sh go test ./internal/proxy/ -count=1`
Expected: PASS, including the pre-existing zero-bytes and decision-table tests.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/handler.go internal/proxy/handler_test.go
git commit -m "feat(proxy): make the held request retry the real forward"
```

---

### Task 4: `dstack.Run` carries probe state and `submittedAt`

**Files:**
- Modify: `internal/dstack/client.go`
- Modify: `internal/dstack/CLAUDE.md`
- Modify: `internal/dstack/mock/mock.go`
- Test: `internal/dstack/mock/mock_test.go`, `internal/dstack/client_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `dstack.Run.ProbesReady bool`, `dstack.Run.SubmittedAt time.Time`; `(*mock.Server).SetProbeDelay(d time.Duration)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/dstack/mock/mock_test.go`:

```go
// TestApply_ProbesReady_IsAStateWithDuration pins F35: "running" and
// "probe-passing" are different states. The fake must make a test ADVANCE
// A CLOCK to reach ready, never flip a flag — a fake that asserts
// readiness instantly would hide exactly the bug D28 was.
func TestApply_ProbesReady_IsAStateWithDuration(t *testing.T) {
	s := New()
	fake := clock.NewFakeClock(time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC))
	s.SetClock(fake)
	s.SetProbeDelay(30 * time.Second)

	run := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1, FleetIdleDuration: time.Hour})
	if run.ProbesReady {
		t.Fatal("ProbesReady true immediately after apply: running is not ready (F35, §6)")
	}
	if run.SubmittedAt.IsZero() {
		t.Fatal("SubmittedAt is zero, want the run's submission instant")
	}

	fake.Advance(29 * time.Second)
	got, err := s.Get("qwen")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ProbesReady {
		t.Fatal("ProbesReady true before the probe delay elapsed")
	}

	fake.Advance(2 * time.Second)
	got, err = s.Get("qwen")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.ProbesReady {
		t.Fatal("ProbesReady false after the probe delay elapsed, want true")
	}
}

// TestApply_ProbesReady_ResetsOnSleep: a flip to 0 unreadies the service;
// the next wake must earn readiness again, not inherit it.
func TestApply_ProbesReady_ResetsOnSleep(t *testing.T) {
	s := New()
	fake := clock.NewFakeClock(time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC))
	s.SetClock(fake)
	s.SetProbeDelay(10 * time.Second)

	run := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1, FleetIdleDuration: time.Hour})
	fake.Advance(11 * time.Second)
	if got, _ := s.Get("qwen"); !got.ProbesReady {
		t.Fatal("setup: expected ProbesReady after the delay")
	}

	_ = s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 0, BaseDeploymentNum: run.DeploymentNum + 1})
	got, err := s.Get("qwen")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ProbesReady {
		t.Fatal("ProbesReady survived a flip to 0 replicas, want false")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `./scripts/dev.sh go test ./internal/dstack/... -run ProbesReady -count=1`
Expected: FAIL — `SetProbeDelay` undefined, `ProbesReady`/`SubmittedAt` unknown fields.

- [ ] **Step 3: Add the fields and narrow the freeze**

In `internal/dstack/client.go`, extend `Run`:

```go
type Run struct {
	Name          string
	RunID         string
	DeploymentNum int
	Replicas      int

	// ProbesReady reports whether dstack's own service probes are passing
	// for this run (F35: ProbeConfig is first-class in dstack and per-probe
	// state is exposed on JobSubmission.probes). It is §6's evidence (a) and
	// the ONLY readiness signal squall reads — squall probes nothing, ever;
	// dstack's probes are dstack's own machinery and exist with or without
	// us. "Replicas > 0" is NEVER Ready by itself (§6).
	//
	// UNVALIDATED against a real dstack server, like the rest of this wire
	// shape — see the package doc and ledger D1.
	ProbesReady bool

	// SubmittedAt is when dstack accepted the run. Informational for now;
	// the provisioningTimeout anchor is status.wakeStartedAt, journaled by
	// the controller at actuation (§5.2), because an in-place flip (F17)
	// reuses the run and SubmittedAt may date from its first creation.
	//
	// UNVALIDATED — see ledger D1.
	SubmittedAt time.Time
}
```

Add `"time"` to the imports.

In `internal/dstack/client.go`'s package doc and in `internal/dstack/CLAUDE.md`, replace the blanket "FROZEN" wording with:

```
Not an SDK: one HTTP round trip per call, no retries, backoff, circuit
breakers or metrics. Fields on Run may be added when a spec section names
the state they carry, and only once that field is confirmed against a real
dstack server (ledger D1 / the Tier-1 e2e-local suite).
```

- [ ] **Step 4: Model probe state in the fake**

In `internal/dstack/mock/mock.go`, add to `Server`:

```go
	// probeDelay is how long after replicas go 0->N the fake's service
	// probes start passing (F35). Zero means "ready as soon as running",
	// which is deliberately NOT the default any readiness test should use:
	// a test that wants to exercise the not-ready state must set a delay
	// and advance the clock, so "running != ready" is a real state with
	// duration rather than a flag a test flips.
	probeDelay time.Duration
```

Add to `service`:

```go
	// replicasUpAt is when replicas last went from 0 to >0; the zero Time
	// means "not currently up". Probe readiness is measured from here.
	replicasUpAt time.Time
```

Add the setter next to `SetClock`:

```go
// SetProbeDelay sets how long after a wake the fake's probes begin
// passing. Intended for tests exercising the running-vs-ready distinction.
func (s *Server) SetProbeDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probeDelay = d
}
```

In `Apply`, where replicas are assigned, maintain `replicasUpAt`: set it to `s.clock.Now()` when the service transitions from `replicas == 0` to `req.Replicas > 0`, and to the zero `time.Time` when `req.Replicas == 0`. Set `svc.submittedAt` when a run id is minted.

Add a helper and use it in every place that builds a `Run` (`Apply`, `Get`, `ListRuns`) so the three cannot drift:

```go
// probesReady reports whether the fake's probes are passing: replicas up,
// and probeDelay elapsed since they came up (F35).
func (s *Server) probesReady(svc *service) bool {
	if svc.replicas == 0 || svc.replicasUpAt.IsZero() {
		return false
	}
	return !s.clock.Now().Before(svc.replicasUpAt.Add(s.probeDelay))
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `./scripts/dev.sh go test ./internal/dstack/... -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/dstack/client.go internal/dstack/CLAUDE.md internal/dstack/mock/ internal/dstack/client_test.go
git commit -m "feat(dstack): carry probe readiness and submittedAt on Run"
```

---

### Task 5: The controller writes `Ready`

**Files:**
- Modify: `internal/controller/squall/activity.go`
- Modify: `internal/controller/squall/model_controller.go`
- Modify: `api/squall/v1alpha1/model_types.go`
- Test: `internal/controller/squall/activity_test.go`, `internal/controller/squall/model_controller_unit_test.go`

**Interfaces:**
- Consumes: Task 4's `dstack.Run.ProbesReady`; Task 1's `ModelActivity.LastSuccessAt`.
- Produces: `ActivityEvidence.NewestLastSuccessAt time.Time`; `ModelStatus.WakeStartedAt *metav1.Time`.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/squall/activity_test.go`:

```go
// TestAggregateActivity_LastSuccessAt_DoesNotAffectCompleteness is the
// rollout-safety rule: adding a field must not turn an older replica's
// report into "ambiguous" and wedge §6's sleep for the whole cluster.
func TestAggregateActivity_LastSuccessAt_DoesNotAffectCompleteness(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	queries := []ActivityQuery{
		{Address: "10.0.0.1", Report: &squallv1alpha1.ActivityReport{Models: map[string]squallv1alpha1.ModelActivity{
			"qwen": {InFlight: 0, LastRequestAt: now.Add(-time.Hour)}, // no LastSuccessAt: an older replica
		}}},
		{Address: "10.0.0.2", Report: &squallv1alpha1.ActivityReport{Models: map[string]squallv1alpha1.ModelActivity{
			"qwen": {InFlight: 0, LastRequestAt: now.Add(-time.Hour), LastSuccessAt: now.Add(-2 * time.Minute)},
		}}},
	}

	got := aggregateActivity([]string{"10.0.0.1", "10.0.0.2"}, queries, "qwen")
	if !got.Complete {
		t.Fatal("Complete = false because one replica omitted LastSuccessAt; a mid-rollout replica must not wedge sleep")
	}
	if !got.AllIdle {
		t.Fatal("AllIdle = false, want true: both replicas report 0 in-flight")
	}
	if want := now.Add(-2 * time.Minute); !got.NewestLastSuccessAt.Equal(want) {
		t.Fatalf("NewestLastSuccessAt = %v, want %v (the newest across replicas)", got.NewestLastSuccessAt, want)
	}
}
```

Append to `internal/controller/squall/model_controller_unit_test.go`:

```go
// TestDecide_ReadyFromProbes is §6 evidence (a).
func TestDecide_ReadyFromProbes(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	spec := exampleModelSpec()

	notReady := Observed{Run: &dstack.Run{Name: "qwen", Replicas: 1, ProbesReady: false}, Ready: false}
	if phase, _ := Decide(notReady, squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseWaking}, spec, true, now); phase == squallv1alpha1.ModelPhaseReady {
		t.Fatal("phase = Ready with probes failing: 'dstack job running' is never Ready (§6)")
	}

	ready := Observed{Run: &dstack.Run{Name: "qwen", Replicas: 1, ProbesReady: true}, Ready: true}
	if phase, _ := Decide(ready, squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseWaking}, spec, true, now); phase != squallv1alpha1.ModelPhaseReady {
		t.Fatalf("phase = %v with probes passing, want Ready", phase)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `./scripts/dev.sh go test ./internal/controller/squall/ -run 'LastSuccessAt|ReadyFromProbes' -short -count=1`
Expected: FAIL — `NewestLastSuccessAt` and `ProbesReady` unknown.

- [ ] **Step 3: Aggregate the new field without touching completeness**

In `internal/controller/squall/activity.go`, add to `ActivityEvidence`:

```go
	// NewestLastSuccessAt is the newest committed-forward instant across
	// replicas — §6's evidence (b). It is NOT part of the Complete/AllIdle
	// determination: a replica that omits the field (an older build
	// mid-rollout) decodes it as the zero value, which is legitimate "no
	// success observed here", never ambiguity. Making it required would
	// wedge sleep cluster-wide during any rollout.
	NewestLastSuccessAt time.Time
```

In `aggregateActivity`, inside the loop that already folds each report, track the max `LastSuccessAt` alongside the existing `LastRequestAt` fold. **Do not add any new ambiguity branch.**

- [ ] **Step 4: Set `Ready` in `observe`, and journal `wakeStartedAt`**

In `internal/controller/squall/model_controller.go`, change `observe`:

```go
	observed := Observed{Run: run}
	if run.Replicas > 0 {
		observed.Activity = r.gatherActivity(ctx, name, now)
		// §6: Ready has two named evidences, whichever arrives first —
		// (a) dstack's own probe state (F35), (b) a first-party forward
		// success reported by the proxy. Squall probes nothing itself.
		observed.Ready = run.ProbesReady || freshSuccess(observed.Activity, now, spec.ScaleDownDelay())
	}
	return observed, nil
```

Add the helper in the same file:

```go
// freshSuccess reports whether a first-party forward succeeded recently
// enough to count as readiness evidence (§6 evidence (b)). Incomplete
// evidence is never a success: an unreachable replica must not be read as
// proof that anything is serving.
func freshSuccess(ev *ActivityEvidence, now time.Time, window time.Duration) bool {
	if ev == nil || !ev.Complete || ev.NewestLastSuccessAt.IsZero() {
		return false
	}
	return now.Sub(ev.NewestLastSuccessAt) <= window
}
```

`observe` needs the spec; pass `spec squallv1alpha1.ModelSpec` as a parameter and update its single caller in `Reconcile`. If `ScaleDownDelay()` does not exist, use `time.Duration(spec.ScaleDownDelaySeconds) * time.Second` inline.

In `Reconcile`, where the wake Apply is actuated, journal the anchor before the status write:

```go
	// §5.2: journal the provisioningTimeout anchor in the same act as the
	// 0->1 actuation — only the actuation site knows the moment. It is set
	// ONCE per wake and must not be rewritten on later reconciles of the
	// same wake, or the deadline would never expire. Task 8.3 consumes it;
	// this block implements no timeout behaviour.
	if action.Apply && action.Replicas > 0 && model.Status.WakeStartedAt == nil {
		model.Status.WakeStartedAt = &metav1.Time{Time: now}
	}
```

- [ ] **Step 5: Add the status field and printer columns**

In `api/squall/v1alpha1/model_types.go`, add to `ModelStatus`:

```go
	// WakeStartedAt is when the controller actuated the most recent 0->1
	// flip — the anchor provisioningTimeout measures from (§5.2). It is
	// deliberately NOT dstack's submitted_at: an in-place flip (F17) reuses
	// the run, so submitted_at may date from the run's first creation and
	// would fire the deadline instantly on a re-wake. Cleared when the
	// model sleeps.
	// +optional
	WakeStartedAt *metav1.Time `json:"wakeStartedAt,omitempty"`
```

Add printer columns above the `Model` type, next to the existing kubebuilder markers:

```go
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Run",type=string,JSONPath=`.status.runId`
// +kubebuilder:printcolumn:name="Engine",type=string,JSONPath=`.spec.engine`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
```

- [ ] **Step 6: Regenerate and sync the chart**

Run: `./scripts/dev.sh make gen-code gen-manifests helm-sync`
Expected: `zz_generated.deepcopy.go`, `config/crd/bases/`, and `deploy/helm/squall/crds/` all updated. The drift gate compares against **committed** state, so it will report drift until step 7 commits — that is correct behaviour, not a failure.

- [ ] **Step 7: Run tests and commit**

Run: `./scripts/dev.sh go test ./internal/controller/squall/ ./api/... -short -count=1`
Expected: PASS.

```bash
git add internal/controller/squall/ api/squall/v1alpha1/ config/crd/bases/ deploy/helm/squall/crds/
git commit -m "feat(controller): write phase Ready from probes or first-party success"
```

---

### Task 6: Prove it end to end, and delete the D38 hack

**Files:**
- Modify: `test/e2e/e2e_test.go`
- Modify: `docs/references/deviations-and-findings.md`

**Interfaces:**
- Consumes: everything above.
- Produces: an e2e forwarding spec with no controller-pause workaround.

- [ ] **Step 1: Delete the workaround**

In `test/e2e/e2e_test.go`, find the forwarding spec that scales `squall-controller-manager` to zero and patches `status.phase: Ready` via `kubectl patch --subresource=status`. Remove the scale-down, the `kubectl wait --for=delete`, the status patch, and the scale-back-up. The Model must reach `Ready` through the live controller or the spec fails.

- [ ] **Step 2: Run the e2e suite to verify it fails or passes for the right reason**

Run: `./scripts/dev.sh make e2e-full`
Expected: the forwarding spec reaches `Ready` and returns a real completion. If it does not, the failure is real — do NOT restore the workaround; report it.

Note: `fake-dstack` must report probe readiness for this to pass. If it does not yet model `ProbesReady` over the HTTP surface, add it there (`internal/dstack/mock/http.go`'s run wire shape) as part of this task — the mock's Go and HTTP surfaces must not drift.

- [ ] **Step 3: Close D38 in the ledger**

In `docs/references/deviations-and-findings.md`, change D38's status from `ACCEPTED` to `CLOSED`, with: *"Closed by the serving-path block: the forwarding spec now reaches Ready through the live controller (§6 evidence (a)/(b)) and the controller-pause workaround is deleted."*

- [ ] **Step 4: Commit**

```bash
git add test/e2e/e2e_test.go docs/references/deviations-and-findings.md
git commit -m "test(e2e): reach Ready honestly and drop the controller-pause hack"
```

---

## End-of-block verification

Run **once**, after all six tasks:

```bash
./scripts/dev.sh make test-unit
./scripts/dev.sh make test-envtest
./scripts/dev.sh make qa-lint
./scripts/dev.sh make helm-sync-check
./scripts/dev.sh make e2e-run
```

All must exit 0, and `git status --short` must be clean.

## Mutation sweep — one pass, at the end

Apply each mutation, confirm the named test goes red **for the designed reason**, revert by hand (never `git checkout --`), and record the result. A mutation that leaves the suite green is a finding to report, not to hide.

| # | Mutation | Test that must go red |
|---|---|---|
| S1 | Write status/headers on the first attempt instead of on commit | `TestHandler_HoldRetriesForward_ZeroBytesUntilCommit` |
| S2 | `classifyAttempt`: 503 → `attemptCommit` | `TestClassifyAttempt`, `TestHandler_HoldRetriesForward_ZeroBytesUntilCommit` |
| S3 | `classifyAttempt`: transport error → `attemptCommit` | `TestClassifyAttempt` |
| S4 | Keep attempting after `committed != nil` | `TestHandler_HoldRetriesForward_ZeroBytesUntilCommit` (attempt count) |
| S5 | `streamCommit`: `io.ReadAll` the body before writing | `TestStreamCommit_StreamsProgressively` |
| S6 | `classifyAttempt`: 403 → `attemptRetry` | `TestClassifyAttempt` |
| S7 | `classifyAttempt`: 404 → `attemptRetry` | `TestClassifyAttempt` |
| S8 | Call `Activity.Success` on attempt rather than on commit | `TestHandler_HoldRetries_DeadlineAnswersWaitContract` |
| S9 | Make `LastSuccessAt` required for `Complete` | `TestAggregateActivity_LastSuccessAt_DoesNotAffectCompleteness` |
| S10 | Return the upstream 503 body instead of the wait contract | `TestHandler_HoldRetries_DeadlineAnswersWaitContract` |
| R1 | `observe`: ignore `run.ProbesReady` | `TestDecide_ReadyFromProbes` + e2e |
| R2 | `freshSuccess`: always return false | envtest readiness case |
| R3 | `observe`: `observed.Ready = run.Replicas > 0` | `TestApply_ProbesReady_IsAStateWithDuration` + `TestDecide_ReadyFromProbes` |
| R4 | Rewrite `WakeStartedAt` on every reconcile | envtest wake case |
| R5 | `freshSuccess`: drop the `ev.Complete` check | new unit case — incomplete evidence must never prove readiness |

## Risks

- **F32 is the load-bearing invariant.** S1 and S5 are the tests that defend it. A premature byte silently removes the model from LiteLLM's fallback chain — the failure is invisible in tests that only check status codes.
- **Retry amplification.** Each held request costs one upstream round trip per tick. With `RefreshInterval` defaulting to `cooldown/2` (≈2.5s) and `MaxPendingPerModel` holds outstanding, confirm the product is sane against a gateway that is already 503-ing before merging.
- **The wire shape is still unvalidated** (D1). Two more fields now depend on it. Tier-1 `e2e-local` against a real dstack server matters more after this block than before.
- **Do not let this block grow into 8.3.** `wakeStartedAt` is written here and consumed later; implementing the timeout now destroys every model at the deadline while `Ready` is still rare.

## Self-review notes

- **Spec coverage:** §7's held-request retry → Tasks 2–3. §6's two evidences → Tasks 1, 4, 5. F35 → Task 4. §5.2's `wakeStartedAt` → Task 5. §10's two-lane rule → untouched by construction (first-party traffic only; no synthetic probe is added anywhere).
- **Type consistency:** `attemptResult`/`attemptRetry`/`attemptCommit` (Task 2) are used in Task 3; `ActivityTracker.Success` (Task 1) is called in Task 3's `commit`; `Run.ProbesReady` (Task 4) is read in Task 5's `observe`; `ActivityEvidence.NewestLastSuccessAt` (Task 5, step 3) is read by `freshSuccess` (Task 5, step 4).
- **Known gap, deliberate:** `spec.ScaleDownDelay()` may not exist as a method — Task 5 step 4 names the inline fallback rather than assuming.
