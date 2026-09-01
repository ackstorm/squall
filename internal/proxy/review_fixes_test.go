// SPDX-License-Identifier: Apache-2.0

// Regression tests for the 2026-08-31 whole-branch review's proxy-side
// findings (ledger D103, D106, D113, D118, D121, D127).
package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/clock"
)

// TestDynamicPatcher_AllNamespacesMode_ResolvesTheModelsOwnNamespace is
// D103. The chart default SQUALL_NAMESPACE="" makes the informer watch
// every namespace — but Namespace "" on the patcher built a cluster-scoped
// URL for a namespaced CRD, so every demand patch 404'd (silently, because
// Signal swallows patch errors by design) and no Model could wake in a
// stock install. In all-namespaces mode the patch must land in the Model's
// OWN namespace, read off the cache snapshot.
func TestDynamicPatcher_AllNamespacesMode_ResolvesTheModelsOwnNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	initial := modelUnstructured("qwen", "Asleep", "30s") // lives in "default"
	fakeClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{ModelGVR: "ModelList"},
		initial,
	)

	cache := NewCache()
	cache.Set("qwen", ModelSnapshot{Namespace: "default", Phase: squallv1alpha1.ModelPhaseAsleep})

	p := &DynamicPatcher{Client: fakeClient, Namespace: "", Cache: cache}
	at := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	if err := p.PatchDemand(context.Background(), "qwen", at); err != nil {
		t.Fatalf("PatchDemand in all-namespaces mode: %v", err)
	}

	u, err := fakeClient.Resource(ModelGVR).Namespace("default").Get(context.Background(), "qwen", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := u.GetAnnotations()[squallv1alpha1.DemandAnnotation]; got != at.UTC().Format(time.RFC3339) {
		t.Fatalf("annotation = %q, want %q — the patch must land in the Model's own namespace", got, at.UTC().Format(time.RFC3339))
	}
}

// TestDynamicPatcher_AllNamespacesMode_UnknownModelIsAnError: guessing a
// namespace would 404 exactly like the bug D103 fixes, so a model the
// cache has never seen must be a loud error, not a silent no-op.
func TestDynamicPatcher_AllNamespacesMode_UnknownModelIsAnError(t *testing.T) {
	scheme := runtime.NewScheme()
	fakeClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{ModelGVR: "ModelList"},
	)
	p := &DynamicPatcher{Client: fakeClient, Namespace: "", Cache: NewCache()}
	if err := p.PatchDemand(context.Background(), "nobody", time.Now()); err == nil {
		t.Fatal("PatchDemand for a model the cache has never seen returned nil; want an error")
	}
}

// TestSSHBackend_DeadTunnelIsEvictedAndRedialled is D106, the finding an
// agent verified against a real in-process SSH server: a tunnel that dies
// AFTER establishment was reused on endpoint equality forever — every
// forward failed with "use of closed network connection", Inner was never
// reached, and each failure fed the replica's unhealthy teardown. The
// watch goroutine must evict the dead tunnel so the next request redials.
// Mutation check: removing `go b.watch(model, t)` leaves this red — the
// dead client is handed out until the deadline expires.
func TestSSHBackend_DeadTunnelIsEvictedAndRedialled(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(engine.Close)

	signer := testSigner(t)
	replica := startFakeReplica(t, strings.TrimPrefix(engine.URL, "http://"), testSigner(t), signer.PublicKey())
	b := newSSHBackendFor(t, replica, signer, NewCache(), backendThatMustNotBeUsed{t})

	first, ok := b.Client("qwen")
	if !ok {
		t.Fatal("no tunnel established")
	}

	// Kill the live SSH connection out from under the backend — an sshd
	// restart, a NAT idle drop, a host reboot; status.replica unchanged.
	b.mu.Lock()
	tun := b.conns["qwen"]
	b.mu.Unlock()
	if tun == nil {
		t.Fatal("no tunnel registered")
	}
	_ = tun.client.Close()

	// Bounded wait: the watch goroutine's Wait() fires on the close and
	// evicts; the next Client() call then redials the (still healthy)
	// replica and hands out a NEW transport.
	deadline := time.Now().Add(10 * time.Second)
	for {
		second, ok2 := b.Client("qwen")
		if ok2 && second != first {
			resp, err := second.Get("http://replica/health")
			if err != nil {
				t.Fatalf("request over the redialled tunnel: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("dead tunnel was never evicted: Client() kept returning the closed transport")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSSHBackend_EndpointChangeDoesNotCutInFlightStreams is D113: ONE SSH
// connection carries every generation for a model, and tunnelFor used to
// Close() it synchronously the moment the cached endpoint changed —
// terminating all of them mid-token on the strength of a routing update.
// A retired tunnel must let its in-flight streams finish and close itself
// only when the last one ends.
func TestSSHBackend_EndpointChangeDoesNotCutInFlightStreams(t *testing.T) {
	// An engine that streams its body slowly enough for the endpoint
	// change to land mid-response.
	release := make(chan struct{})
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = w.Write([]byte("first-half."))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
		_, _ = w.Write([]byte("second-half"))
	}))
	t.Cleanup(engine.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	engine2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("new-replica"))
	}))
	t.Cleanup(engine2.Close)

	signer := testSigner(t)
	replica := startFakeReplica(t, strings.TrimPrefix(engine.URL, "http://"), testSigner(t), signer.PublicKey())
	replica2 := startFakeReplica(t, strings.TrimPrefix(engine2.URL, "http://"), testSigner(t), signer.PublicKey())
	cache := NewCache()
	b := newSSHBackendFor(t, replica, signer, cache, backendThatMustNotBeUsed{t})

	oldClient, ok := b.Client("qwen")
	if !ok {
		t.Fatal("no tunnel established")
	}

	// Start a stream over the OLD tunnel and park it mid-body.
	resp, err := oldClient.Get("http://replica/generate")
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	firstHalf := make([]byte, len("first-half."))
	if _, err := io.ReadFull(resp.Body, firstHalf); err != nil {
		t.Fatalf("read first half: %v", err)
	}

	// The informer reports a NEW replica; the next resolution must retire
	// the old tunnel, not close it.
	host, portStr, err := net.SplitHostPort(replica2.addr)
	if err != nil {
		t.Fatalf("split replica2 addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse replica2 port: %v", err)
	}
	cache.Set("qwen", ModelSnapshot{
		Phase:   "Ready",
		Replica: &ReplicaEndpoint{Host: host, SSHPort: port, User: "squall", ServicePort: replica2.servicePort},
	})
	if newClient, ok := b.Client("qwen"); !ok || newClient == oldClient {
		t.Fatal("endpoint change did not produce a fresh tunnel")
	}

	// The parked stream must still complete over the retired connection.
	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("in-flight stream died after the endpoint change: %v — a routing decision terminated active work", err)
	}
	_ = resp.Body.Close()
	if string(firstHalf)+string(rest) != "first-half.second-half" {
		t.Fatalf("stream = %q + %q, want the full body", firstHalf, rest)
	}
}

// routeRecordingBackend implements RouteBackend and records that URL and
// client were requested as ONE resolution (D121); upstream is a live
// httptest server so the forward really happens.
type routeRecordingBackend struct {
	target *url.URL
	calls  int
}

func (r *routeRecordingBackend) URL(string) (*url.URL, bool) { return r.target, true }
func (r *routeRecordingBackend) Route(string) (*url.URL, *http.Client, bool) {
	r.calls++
	return r.target, nil, true
}

// TestAttemptForward_ResolvesRouteAtomically_AndPreservesQuery covers
// D121 + D127's request half: the route (URL + transport) comes from ONE
// RouteBackend resolution, the query string survives the forward, and
// hop-by-hop headers do not cross the proxy.
func TestAttemptForward_ResolvesRouteAtomically_AndPreservesQuery(t *testing.T) {
	var gotQuery, gotConnection, gotTE string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotConnection = r.Header.Get("Connection")
		gotTE = r.Header.Get("TE")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	target, _ := url.Parse(upstream.URL)

	backend := &routeRecordingBackend{target: target}
	h := &Handler{Cache: NewCache(), Backend: backend}

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?stream=true&user=abc", strings.NewReader(`{}`))
	r.Header.Set("Connection", "keep-alive")
	r.Header.Set("TE", "trailers")
	resp, res, err := h.attemptForward(context.Background(), r, "qwen", []byte(`{"model":"qwen"}`), squallv1alpha1.ModelPhaseReady)
	if err != nil || res != attemptCommit {
		t.Fatalf("attemptForward = %v, %v; want commit", res, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if backend.calls != 1 {
		t.Fatalf("Route resolved %d times, want exactly 1 — URL and transport must come from one lookup (D121)", backend.calls)
	}
	if gotQuery != "stream=true&user=abc" {
		t.Fatalf("upstream saw query %q, want it preserved (D127)", gotQuery)
	}
	if gotConnection != "" || gotTE != "" {
		t.Fatalf("hop-by-hop headers crossed the proxy: Connection=%q TE=%q (D127)", gotConnection, gotTE)
	}
}

// urllessBackend has no route at all — the proxy's own misconfiguration
// (empty status.serviceURL / SQUALL_DSTACK_URL), not the replica's fault.
type urllessBackend struct{}

func (urllessBackend) URL(string) (*url.URL, bool) { return nil, false }

// TestServeHTTP_NoBackendURL_IsNotChargedToTheReplica is D118: "no backend
// url" says nothing about the replica, and counting it toward
// FailuresSinceSuccess feeds the threshold-of-3 unhealthy teardown — D99's
// bug shape (client weather charged to the GPU) with a config fault as the
// trigger. Mutation check: removing the errNoBackendURL guard around
// h.Activity.Failure turns this red.
func TestServeHTTP_NoBackendURL_IsNotChargedToTheReplica(t *testing.T) {
	cache := NewCache()
	cache.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady, Schedulable: true})
	tracker := NewActivityTracker(clock.RealClock{})
	h := &Handler{Cache: cache, Backend: urllessBackend{}, Activity: tracker, Demand: NewDemandCoalescer(noopPatcher{}, time.Second, nil)}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen"}`)))

	report := tracker.Report()
	if got := report.Models["qwen"].FailuresSinceSuccess; got != 0 {
		t.Fatalf("FailuresSinceSuccess = %d after a proxy-side config fault, want 0 — three of these tear down a healthy GPU (D118)", got)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the wait contract's 503", rec.Code)
	}
}

// TestErrNoBackendURL_IsTheSentinel pins the errors.Is contract ServeHTTP's
// guard depends on: attemptForward's no-route failure must BE the sentinel,
// not merely resemble it in text.
func TestErrNoBackendURL_IsTheSentinel(t *testing.T) {
	h := &Handler{Cache: NewCache(), Backend: urllessBackend{}}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	_, res, err := h.attemptForward(context.Background(), r, "qwen", nil, squallv1alpha1.ModelPhaseReady)
	if res != attemptRetry || !errors.Is(err, errNoBackendURL) {
		t.Fatalf("attemptForward = %v, %v; want attemptRetry with errNoBackendURL", res, err)
	}
}

// TestPeekAndRestore_PreservesTheTransportsCloser is D122: NopCloser here
// made every later resp.Body.Close() a no-op, leaking the connection and
// its readLoop goroutine whenever a peeked body was abandoned — which is
// exactly the retry path.
func TestPeekAndRestore_PreservesTheTransportsCloser(t *testing.T) {
	closed := false
	resp := &http.Response{Body: &closeRecorder{Reader: strings.NewReader(`{"detail":"x"}`), closed: &closed}}
	peeked := peekAndRestore(resp, 4)
	if string(peeked) != `{"de` {
		t.Fatalf("peeked = %q, want the first 4 bytes", peeked)
	}
	// The peeked bytes are SPLICED BACK, so the restored body reads the
	// whole original from the start — that is peekAndRestore's contract.
	rest, err := io.ReadAll(resp.Body)
	if err != nil || string(rest) != `{"detail":"x"}` {
		t.Fatalf("restored body = %q (%v), want the original intact from the start", rest, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !closed {
		t.Fatal("Close never reached the transport's own closer — the connection and its readLoop leak (D122)")
	}
}

type closeRecorder struct {
	io.Reader
	closed *bool
}

func (c *closeRecorder) Close() error { *c.closed = true; return nil }

// TestServeHTTP_UnknownModelIsNeverTracked is D126: ActivityTracker is
// keyed by a CALLER-SUPPLIED string and was populated before the
// CR-existence check, with no eviction — 5000 junk names grew the
// /internal/activity report to 698 KB, decoded by the controller per
// replica per reconcile, on an endpoint with no auth. A name with no CR is
// answered immediately and reaches no upstream, so it must leave no key.
func TestServeHTTP_UnknownModelIsNeverTracked(t *testing.T) {
	tracker := NewActivityTracker(clock.RealClock{})
	h := &Handler{Cache: NewCache(), Backend: urllessBackend{}, Activity: tracker}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"junk-name-9999"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a model with no CR", rec.Code)
	}
	if _, tracked := tracker.Report().Models["junk-name-9999"]; tracked {
		t.Fatal("a caller-invented name entered the activity tracker (D126)")
	}
}
