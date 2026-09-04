// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// TestQueryActivity_HTTPResponses closes D24: queryActivity's HTTP-response
// translation (non-200 status, JSON-decode error, missing-model-key) was
// previously exercised only through hand-built ActivityQuery structs
// (activity_test.go), never a real HTTP response. Each failure case here
// must fold into OK: false — the "ambiguous" signal aggregateActivity
// requires to keep a Model from being read as idle when a replica's answer
// cannot be trusted.
func TestQueryActivity_HTTPResponses(t *testing.T) {
	tests := []struct {
		name string

		status int
		body   string // raw response body; empty means no body written

		wantOK     bool
		wantNoData bool
	}{
		{
			name:   "non-200 status -> OK false",
			status: http.StatusInternalServerError,
			body:   `{"models":{"qwen":{"inFlight":0,"lastRequestAt":"2026-08-26T12:00:00Z"}}}`,
			wantOK: false,
		},
		{
			name:   "200 with unparseable JSON body -> OK false",
			status: http.StatusOK,
			body:   `{not-json`,
			wantOK: false,
		},
		{
			name:       "200, well-formed, but no key for this model -> OK true, NoData true",
			status:     http.StatusOK,
			body:       `{"models":{"other-model":{"inFlight":0,"lastRequestAt":"2026-08-26T12:00:00Z"}}}`,
			wantOK:     true,
			wantNoData: true,
		},
		{
			name:   "200, well-formed, key present -> OK true",
			status: http.StatusOK,
			body:   `{"models":{"qwen":{"inFlight":2,"lastRequestAt":"2026-08-26T12:00:00Z"}}}`,
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != squallv1alpha1.ActivityPath {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			r := &ModelReconciler{}
			got := r.queryActivity(context.Background(), srv.Listener.Addr().String(), "qwen")

			if got.OK != tt.wantOK {
				t.Fatalf("OK = %v, want %v (got %+v)", got.OK, tt.wantOK, got)
			}
			if tt.wantOK && got.NoData != tt.wantNoData {
				t.Errorf("NoData = %v, want %v", got.NoData, tt.wantNoData)
			}
		})
	}
}

// TestQueryActivity_TransportFailure_ResolvesToOKFalse is the non-response
// case (connection refused): no server at all backing the address.
func TestQueryActivity_TransportFailure_ResolvesToOKFalse(t *testing.T) {
	// Unroutable-but-syntactically-valid address: nothing listens here.
	r := &ModelReconciler{}
	got := r.queryActivity(context.Background(), "127.0.0.1:1", "qwen")
	if got.OK {
		t.Fatalf("OK = true, want false on a transport failure: %+v", got)
	}
}
