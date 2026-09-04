// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/clock"
	"github.com/ackstorm/squall/internal/proxy"
)

// TestMux_ModelsIsOurs_AnyMethodAnyParams: /v1/models is squall's own
// discovery surface, never forwarded. GET and POST both answer it, and
// query parameters are accepted and ignored rather than routed anywhere.
func TestMux_ModelsIsOurs_AnyMethodAnyParams(t *testing.T) {
	cache := proxy.NewCache()
	cache.Set("qwen", proxy.ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep})
	mux := newMux(cache, proxy.NewActivityTracker(clock.RealClock{}), http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("/v1/models reached the forwarding handler; it is squall's own route")
			w.WriteHeader(http.StatusTeapot)
		}), http.NotFoundHandler())

	for _, target := range []string{
		"/v1/models",
		"/v1/models?owned_by=ackstorm&limit=10",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(method, target, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s = %d, want 200", method, target, rec.Code)
			}
			var body struct {
				Object string `json:"object"`
				Data   []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s %s: decode body %q: %v", method, target, rec.Body.String(), err)
			}
			if body.Object != "list" || len(body.Data) != 1 || body.Data[0].ID != "qwen" {
				t.Fatalf("%s %s body = %q, want the model list", method, target, rec.Body.String())
			}
		}
	}
}

func TestMux_MetricsIsServed(t *testing.T) {
	mux := newMux(proxy.NewCache(), proxy.NewActivityTracker(clock.RealClock{}), http.NotFoundHandler(),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("squall_proxy_requests_total 1\n"))
		}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "squall_proxy_requests_total 1\n" {
		t.Fatalf("GET /metrics = %d %q", rec.Code, rec.Body.String())
	}
}
