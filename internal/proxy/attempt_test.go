// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// TestClassifyAttempt is §7's retry rule: "503, 502, or connection-refused
// mean 'still waking', keep holding". Everything else commits — an engine
// answering 400 is READY, it just disliked the request. 404 is excluded
// here: it is phase-dependent (D44) — see TestClassifyAttempt_404DependsOnPhase.
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
		{"500 commits: the engine answered and failed", http.StatusInternalServerError, nil, attemptCommit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var resp *http.Response
			if tc.err == nil {
				resp = &http.Response{StatusCode: tc.status}
			}
			if got := classifyAttempt(resp, tc.err, squallv1alpha1.ModelPhaseReady, nil); got != tc.want {
				t.Fatalf("classifyAttempt(%d, %v) = %v, want %v", tc.status, tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyAttempt_404DependsOnPhase is D44, measured against dstack
// 0.21.2: the service proxy answers 404 for the ENTIRE wake window, not
// 503. A 404 must therefore keep the hold while the CR says the model is
// coming up, and commit only once the CR says it is not.
func TestClassifyAttempt_404DependsOnPhase(t *testing.T) {
	tests := []struct {
		name  string
		phase squallv1alpha1.ModelPhase
		want  attemptResult
	}{
		{"waking: 404 is the wake window, keep holding", squallv1alpha1.ModelPhaseWaking, attemptRetry},
		{"asleep: the wake was just requested, keep holding", squallv1alpha1.ModelPhaseAsleep, attemptRetry},
		{"recreating: also a wake in progress (F20), keep holding", squallv1alpha1.ModelPhaseRecreating, attemptRetry},
		{"ready: a 404 now is real, commit it", squallv1alpha1.ModelPhaseReady, attemptCommit},
		{"dead: decision.go holds on Dead to recreate, keep holding", squallv1alpha1.ModelPhaseDead, attemptRetry},
		{"draining: never holds, so a 404 here is final", squallv1alpha1.ModelPhaseDraining, attemptCommit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAttempt(&http.Response{StatusCode: http.StatusNotFound}, nil, tc.phase, nil)
			if got != tc.want {
				t.Fatalf("classifyAttempt(404, phase=%v) = %v, want %v", tc.phase, got, tc.want)
			}
		})
	}
}

// TestClassifyAttempt_PhaseDoesNotMakeOtherCodesAmbiguous: only 404 is
// phase-dependent. A 200 commits and a 503 retries whatever the CR says —
// otherwise a stale informer cache could swallow a real answer.
func TestClassifyAttempt_PhaseDoesNotMakeOtherCodesAmbiguous(t *testing.T) {
	for _, phase := range []squallv1alpha1.ModelPhase{
		squallv1alpha1.ModelPhaseAsleep, squallv1alpha1.ModelPhaseWaking,
		squallv1alpha1.ModelPhaseReady, squallv1alpha1.ModelPhaseDead,
	} {
		if got := classifyAttempt(&http.Response{StatusCode: 200}, nil, phase, nil); got != attemptCommit {
			t.Fatalf("200 with phase %v = %v, want commit", phase, got)
		}
		if got := classifyAttempt(&http.Response{StatusCode: 503}, nil, phase, nil); got != attemptRetry {
			t.Fatalf("503 with phase %v = %v, want retry", phase, got)
		}
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

// TestAttemptForward_SendsDstackBearerToken is LIVE-4: dstack's service
// proxy demands `Authorization: Bearer <token>` on every forwarded request
// (auth is on by default for services, docs/references/dstack-real-api.md
// §8.1). The header must reach the upstream call, must come from
// Handler.DstackToken rather than whatever the client sent, and must never
// be echoed back onto the response.
func TestAttemptForward_SendsDstackBearerToken(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)

	h := &Handler{Backend: stubBackend{target: target}, Cache: NewCache(), DstackToken: "dstack-secret-token"}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	// The client's own Authorization (e.g. its LiteLLM/squall-proxy
	// credential) authenticates it to squall-proxy, not squall-proxy to
	// dstack — it must be overwritten, never forwarded as-is.
	req.Header.Set("Authorization", "Bearer client-supplied-token")

	resp, res, _ := h.attemptForward(context.Background(), req, "qwen", nil, squallv1alpha1.ModelPhaseReady)
	if res != attemptCommit {
		t.Fatalf("attemptForward result = %v, want commit", res)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	if gotAuth != "Bearer dstack-secret-token" {
		t.Fatalf("upstream saw Authorization = %q, want %q", gotAuth, "Bearer dstack-secret-token")
	}
	if got := resp.Header.Get("Authorization"); got != "" {
		t.Fatalf("response carries an Authorization header (%q); it must never be echoed to the client", got)
	}
}

// TestAttemptForward_NoTokenConfigured_SendsNoAuthorizationHeader guards
// against a degenerate "Bearer " header when DstackToken is unset — LIVE-4's
// diagnosability requirement lives in cmd/proxy's startup log, not here, but
// the forward itself must stay silent about auth rather than send garbage.
func TestAttemptForward_NoTokenConfigured_SendsNoAuthorizationHeader(t *testing.T) {
	sawHeader := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawHeader = r.Header["Authorization"]
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)

	h := &Handler{Backend: stubBackend{target: target}, Cache: NewCache()}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp, res, _ := h.attemptForward(context.Background(), req, "qwen", nil, squallv1alpha1.ModelPhaseReady)
	if res != attemptCommit {
		t.Fatalf("attemptForward result = %v, want commit", res)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	if sawHeader {
		t.Fatal("Authorization header sent upstream with no DstackToken configured")
	}
}

// TestIsDstackServiceNotFound pins the matcher narrowly and explicitly
// (LIVE-6): it must fire on the exact measured shape and nothing else,
// including dstack's OWN differently-shaped errors and an engine's default
// 404.
func TestIsDstackServiceNotFound(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want bool
	}{
		{
			"measured dstack body, LIVE-6 incident",
			[]byte(`{"detail":"Service main/qwen3-8-27b not found"}`),
			true,
		},
		{
			"measured dstack body, different project/run (dstack-real-api.md §8.1)",
			[]byte(`{"detail":"Service main/squall-probe not found"}`),
			true,
		},
		{"engine's default FastAPI/vLLM 404", []byte(`{"detail":"Not Found"}`), false},
		{
			// dstack's run-management 404-analogue (400 for "run not found")
			// is a LIST, not a string — must not collide (§8.1).
			"dstack run-not-found shape (list detail)",
			[]byte(`{"detail":[{"msg":"Run not found","code":"resource_not_exists"}]}`),
			false,
		},
		{
			// dstack's bad-token 403 detail is an OBJECT, not a string.
			"dstack bad-token shape (object detail)",
			[]byte(`{"detail":{"msg":"Invalid token","code":null}}`),
			false,
		},
		{"empty body", nil, false},
		{"not JSON at all", []byte("Not Found"), false},
		{"has the words but not the shape", []byte(`{"detail":"not found: Service main/x"}`), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDstackServiceNotFound(tc.body); got != tc.want {
				t.Fatalf("isDstackServiceNotFound(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestAttemptForward_DstackNotFoundKeepsHoldEvenWhenReady is LIVE-6 itself:
// dstack's own "service not found" 404 must retry regardless of phase,
// including Ready — the exact transition race measured on the live run
// (probe green, service-proxy route not yet registered).
func TestAttemptForward_DstackNotFoundKeepsHoldEvenWhenReady(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Service main/qwen3-8-27b not found"}`))
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)

	h := &Handler{Backend: stubBackend{target: target}, Cache: NewCache()}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp, res, _ := h.attemptForward(context.Background(), req, "qwen", nil, squallv1alpha1.ModelPhaseReady)
	if res != attemptRetry {
		t.Fatalf("attemptForward with dstack's own 404 body, phase Ready = %v, want retry", res)
	}
	if resp != nil {
		t.Fatal("a retry result must not hand the caller a response to close")
	}
}

// TestAttemptForward_EngineNotFoundStillCommits proves the matcher is narrow
// enough to leave a genuine engine 404 alone: with phase Ready, an engine's
// own "route not found" answer is real and must commit, exactly as before
// LIVE-6.
func TestAttemptForward_EngineNotFoundStillCommits(t *testing.T) {
	const engineBody = `{"detail":"Not Found"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(engineBody))
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)

	h := &Handler{Backend: stubBackend{target: target}, Cache: NewCache()}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp, res, _ := h.attemptForward(context.Background(), req, "qwen", nil, squallv1alpha1.ModelPhaseReady)
	if res != attemptCommit {
		t.Fatalf("attemptForward with an engine 404, phase Ready = %v, want commit", res)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading committed body: %v", err)
	}
	if string(got) != engineBody {
		t.Fatalf("committed engine 404 body = %q, want %q (peeking for classification must not alter it)", got, engineBody)
	}
}

// TestAttemptForward_CommittedNotFoundBodyArrivesIntact is LIVE-6's
// truncation guard: peeking a 404 body to classify it (isDstackServiceNotFound)
// must not cost the client a single byte of a body that turns out to be
// committed, even when that body is larger than notFoundPeekLimit — a naive
// io.ReadAll(resp.Body) followed by a fresh reader over just the peeked
// bytes would silently truncate here.
func TestAttemptForward_CommittedNotFoundBodyArrivesIntact(t *testing.T) {
	big := strings.Repeat("x", notFoundPeekLimit*3)
	engineBody := `{"detail":"Not Found","trace":"` + big + `"}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(engineBody))
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)

	h := &Handler{Backend: stubBackend{target: target}, Cache: NewCache()}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp, res, _ := h.attemptForward(context.Background(), req, "qwen", nil, squallv1alpha1.ModelPhaseReady)
	if res != attemptCommit {
		t.Fatalf("attemptForward = %v, want commit", res)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading committed body: %v", err)
	}
	if len(got) != len(engineBody) {
		t.Fatalf("committed body length = %d, want %d (bytes past the classification peek limit were dropped)",
			len(got), len(engineBody))
	}
	if string(got) != engineBody {
		t.Fatal("committed body content diverges from upstream: peeking corrupted it, not just truncated it")
	}
}

// TestStreamCommit_NonNotFoundBodyIsByteForByte is the general form of
// LIVE-6's "a committed response reaches the client byte-for-byte"
// requirement: streamCommit itself never touches the body for anything
// other than 404 classification (which happens earlier, in attemptForward),
// so a large, arbitrary committed body must survive unchanged.
func TestStreamCommit_NonNotFoundBodyIsByteForByte(t *testing.T) {
	want := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 1000)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(want)),
	}
	rec := httptest.NewRecorder()
	if err := streamCommit(rec, resp); err != nil {
		t.Fatalf("streamCommit: %v", err)
	}
	if got := rec.Body.String(); got != want {
		t.Fatalf("streamCommit body length = %d, want %d; bodies differ", len(got), len(want))
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

// TestClassifyAttempt_404RetrySetMatchesBlockingPhases is the invariant the
// D44 fix turns on, stated once so the two halves cannot drift apart again.
// LIVE-6 re-expresses it rather than deleting it: 404 handling is no longer
// PURELY phase-derived (a dstack "service not found" body now overrides the
// phase), but the phase-derived half must still hold for every OTHER 404.
//
// decision.go decides WHETHER to hold; classifyAttempt decides whether a 404
// ENDS that hold. If a phase blocks but its (non-dstack) 404 commits, the
// hold is over before it began and the DeadlineState decision.go promised is
// unreachable — which is exactly how Dead shipped: Block + WaitRecreating on
// one side, attemptCommit on the other, and Dead is by definition the phase
// whose backend 404s.
//
// Two invariants, both checked for every phase:
//
//  1. A 404 whose body is NOT dstack's "service not found" shape (an
//     engine's own 404, or no body) retries EXACTLY on the phases
//     decision.go blocks on — unchanged from before LIVE-6. Derived from
//     Decide, not hand-listed, so a new phase cannot land on only one side.
//  2. A 404 whose body IS dstack's "service not found" shape ALWAYS retries
//     — even for Ready and Draining, which decision.go does not block on.
//     That is the LIVE-6 fix: the body may only WIDEN retry, never narrow
//     it, so this must go red if classifyAttempt ever stops reading body404
//     (e.g. Ready committing dstack's own 404 again) or if the matcher
//     somehow started narrowing a blocking phase's retry.
func TestClassifyAttempt_404RetrySetMatchesBlockingPhases(t *testing.T) {
	phases := []squallv1alpha1.ModelPhase{
		squallv1alpha1.ModelPhaseAsleep,
		squallv1alpha1.ModelPhaseWaking,
		squallv1alpha1.ModelPhaseReady,
		squallv1alpha1.ModelPhaseDraining,
		squallv1alpha1.ModelPhaseRecreating,
		squallv1alpha1.ModelPhaseDead,
	}
	engineBody := []byte(`{"detail":"Not Found"}`)
	dstackBody := []byte(`{"detail":"Service main/qwen3-8-27b not found"}`)
	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			blocks := Decide(phase, true, GatewayCode(0)).Block

			want := attemptCommit
			if blocks {
				want = attemptRetry
			}
			got := classifyAttempt(&http.Response{StatusCode: http.StatusNotFound}, nil, phase, engineBody)
			if got != want {
				t.Fatalf("phase %s: Decide().Block=%v but classifyAttempt(404, engine body)=%v, want %v\n"+
					"a phase that holds must not have its hold ended by the 404 it is guaranteed to see",
					phase, blocks, got, want)
			}

			if got := classifyAttempt(&http.Response{StatusCode: http.StatusNotFound}, nil, phase, dstackBody); got != attemptRetry {
				t.Fatalf("phase %s: classifyAttempt(404, dstack service-not-found body) = %v, want retry regardless of phase (LIVE-6)",
					phase, got)
			}
		})
	}
}
