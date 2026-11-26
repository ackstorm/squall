// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ackstorm/squall/internal/clock"
	"github.com/ackstorm/squall/internal/proxy"
)

// TestHealthz_NotReadyUntilInformerSynced is D111: /healthz is also the
// Deployment's readinessProbe, and answering 200 while the informer still
// syncs in its goroutine let kube-proxy route real traffic at an EMPTY
// cache — every request 404'd, and /v1/models published a zero-model list
// to LiteLLM discovery on every rollout.
func TestHealthz_NotReadyUntilInformerSynced(t *testing.T) {
	cache := proxy.NewCache()
	mux := newMux(cache, proxy.NewActivityTracker(clock.RealClock{}), http.NotFoundHandler(), http.NotFoundHandler())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz before sync = %d, want 503 — Ready with an empty cache 404s every model", rec.Code)
	}

	cache.SetSynced()
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz after sync = %d, want 200", rec.Code)
	}
}

// TestDstackAuthWarning is LIVE-4's diagnosability requirement: a missing
// SQUALL_DSTACK_TOKEN must produce a clear startup log line, not just a
// silent per-request "gateway auth fault" 502 loop. It must also stay quiet
// when the e2e model-mock's TemplateBackend is in use, since that path
// fronts no real dstack server and needs no token.
func TestDstackAuthWarning(t *testing.T) {
	tests := []struct {
		name               string
		backendURLTemplate string
		dstackToken        string
		wantWarning        bool
	}{
		{"no template, no token: warn", "", "", true},
		{"no template, token set: quiet", "", "some-token", false},
		{"template set, no token: quiet (TemplateBackend needs no dstack auth)", "http://%s.svc:8080", "", false},
		{"template set, token set: quiet", "http://%s.svc:8080", "some-token", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dstackAuthWarning(tc.backendURLTemplate, tc.dstackToken)
			if (got != "") != tc.wantWarning {
				t.Fatalf("dstackAuthWarning(%q, %q) = %q, want non-empty=%v",
					tc.backendURLTemplate, tc.dstackToken, got, tc.wantWarning)
			}
		})
	}
}

// serve must not return while a request is still being handled: the drain is
// what lets a streaming generation finish after SIGTERM.
func TestServe_DrainsInFlightRequestsBeforeReturning(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "complete")
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Addr = ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, 30*time.Second) }()

	body := make(chan string, 1)
	go func() {
		var resp *http.Response
		var err error
		for i := 0; i < 100; i++ { // the listener comes up asynchronously
			if resp, err = http.Get("http://" + srv.Addr + "/"); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			body <- "get failed: " + err.Error()
			return
		}
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never started")
	}
	cancel()

	select {
	case err := <-served:
		t.Fatalf("serve returned (%v) while a request was still in flight", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	select {
	case got := <-body:
		if got != "complete" {
			t.Fatalf("client read %q, want %q", got, "complete")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("client never finished")
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve never returned after the drain")
	}
}
