// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
	mux := newMux(cache, proxy.NewActivityTracker(clock.RealClock{}), http.NotFoundHandler())

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
