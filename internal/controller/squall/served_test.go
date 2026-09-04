// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestServedModels_ReadsOpenAIModelList: /v1/models is the one endpoint all
// three engines agree on, and data[].id is the served name.
func TestServedModels_ReadsOpenAIModelList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy/services/main/m/v1/models" {
			t.Errorf("path = %q, want the service URL with /v1/models appended", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want the dstack token", got)
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen3-8-27b","object":"model"}]}`))
	}))
	defer srv.Close()

	r := HTTPServedModelReader{BaseURL: srv.URL, Token: "tok"}
	got, err := r.ServedModels(context.Background(), "/proxy/services/main/m/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "qwen3-8-27b" {
		t.Fatalf("ServedModels = %v, want [qwen3-8-27b]", got)
	}
}

// TestServedModels_EmptyServiceURLIsAnError: the fixture-based reconcile
// test (TestReconcile_ServedModel_FailsOpenWhenServiceURLEmpty) points
// BaseURL at an unroutable host, so it fails open either way — via this
// guard, or, if the guard is ever deleted, via a DNS failure on the still-
// bogus URL that gets built anyway. That masks a deleted guard. Here
// BaseURL is a live, reachable server that would happily answer if this
// call were ever allowed through, isolating the guard as the only thing
// standing between an empty serviceURL and a false "verified" result.
func TestServedModels_EmptyServiceURLIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"should-never-be-reached"}]}`))
	}))
	defer srv.Close()

	r := HTTPServedModelReader{BaseURL: srv.URL}
	if got, err := r.ServedModels(context.Background(), ""); err == nil {
		t.Fatalf("empty serviceURL returned %v and no error; there is no replica to have asked", got)
	}
}

// TestServedModels_ErrorsAreErrorsNotEmptyLists. An empty slice would read
// as "the replica serves nothing", which the caller must treat as a
// MISMATCH. A transport failure means "unknown" and must never be allowed to
// look like evidence — 0->1 fails open.
func TestServedModels_ErrorsAreErrorsNotEmptyLists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	r := HTTPServedModelReader{BaseURL: srv.URL}
	got, err := r.ServedModels(context.Background(), "/proxy/services/main/m/")
	if err == nil {
		t.Fatalf("404 returned %v and no error; an unreachable replica is not evidence", got)
	}
}

// TestVerifyServedModel_MismatchIsReported is the D65 scenario exactly: the
// engine is healthy and answering, and it is serving something else.
func TestVerifyServedModel_MismatchIsReported(t *testing.T) {
	tests := []struct {
		name    string
		served  []string
		want    string
		matched bool
	}{
		{"exact match", []string{"qwen3-8-27b"}, "qwen3-8-27b", true},
		{"the D65 failure", []string{"Qwen/Qwen3-0.6B"}, "qwen3-8-27b", false},
		{"one of several", []string{"other", "qwen3-8-27b"}, "qwen3-8-27b", true},
		{"serving nothing", nil, "qwen3-8-27b", false},
		// I3, block 2 review: `ollama cp <weights> <name>` aliases the pull
		// to Ollama's implicit ":latest" tag, and Ollama's /v1/models then
		// reports it as "<name>:latest", not bare "<name>".
		{"ollama's implicit :latest tag is a match", []string{"qwen3-8-27b:latest"}, "qwen3-8-27b", true},
		{"a DIFFERENT model's :latest tag is still a mismatch", []string{"Qwen/Qwen3-0.6B:latest"}, "qwen3-8-27b", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := servedModelMatches(tc.served, tc.want); got != tc.matched {
				t.Fatalf("servedModelMatches(%v, %q) = %v, want %v",
					tc.served, tc.want, got, tc.matched)
			}
		})
	}
}

// TestServedModels_HTTP500IsAnError: a 500 is exactly as uninformative as a
// 404 — both are "the request did not get a clean answer", never evidence.
//
// The body is a well-formed, DECODABLE model list on purpose: a status
// error framework that also happened to return an empty/unparseable body
// would let this pass for the wrong reason (the JSON-decode-error path
// papering over a dropped status-code check, not the check itself).
// Mutation-tested: dropping the `resp.StatusCode != http.StatusOK` guard
// left every OTHER test in this file green (a non-200 response with an
// empty body still fails to decode as JSON), and only failed here, where a
// dropped check would otherwise happily return the body's models as fact.
func TestServedModels_HTTP500IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"looks-fine-but-isnt"}]}`))
	}))
	defer srv.Close()

	r := HTTPServedModelReader{BaseURL: srv.URL}
	if got, err := r.ServedModels(context.Background(), "/proxy/services/main/m/"); err == nil {
		t.Fatalf("500 returned %v and no error; a server error is not evidence, however well-formed its body", got)
	}
}

// TestServedModels_UnparseableJSONIsAnError: weights mid-download or a
// misbehaving proxy can answer 200 with a body that isn't the OpenAI model
// list shape at all. That must surface as an error, not as a silent empty
// list a caller could mistake for "serving nothing" (a real mismatch).
func TestServedModels_UnparseableJSONIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	r := HTTPServedModelReader{BaseURL: srv.URL}
	if got, err := r.ServedModels(context.Background(), "/proxy/services/main/m/"); err == nil {
		t.Fatalf("unparseable body returned %v and no error; bad JSON is not evidence", got)
	}
}

// TestServedModels_ConnectionRefusedIsAnError: the replica may not be
// listening at all yet (mid-boot, or the service URL is simply wrong) —
// this is the "hangs or refuses" half of the fail-open contract that a
// live HTTP server can't simulate.
func TestServedModels_ConnectionRefusedIsAnError(t *testing.T) {
	// Bind and immediately close: the port is very likely still refusing
	// connections for this process's lifetime, without relying on a
	// hardcoded port that might be in use elsewhere on this host.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	r := HTTPServedModelReader{BaseURL: "http://" + addr}
	if got, err := r.ServedModels(context.Background(), "/proxy/services/main/m/"); err == nil {
		t.Fatalf("connection refused returned %v and no error; an unreachable replica is not evidence", got)
	}
}

// TestServedModels_TimeoutIsAnError: a replica mid-download of weights may
// accept the connection and simply never answer. The bounded client
// (Client == nil -> 10s default) exists for exactly this; here a request
// context that is already expired proves the same fail-open path without a
// real multi-second sleep in the test suite.
func TestServedModels_TimeoutIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	r := HTTPServedModelReader{BaseURL: srv.URL}
	if got, err := r.ServedModels(ctx, "/proxy/services/main/m/"); err == nil {
		t.Fatalf("timed-out request returned %v and no error; a hung replica is not evidence", got)
	}
}

// TestServedModels_EmptyListIsNotAnError: a replica that answers 200 with
// no models loaded is real, useful evidence (§the D65 "serving nothing"
// case) — not a transport failure, and must not be reported as one.
func TestServedModels_EmptyListIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	r := HTTPServedModelReader{BaseURL: srv.URL}
	got, err := r.ServedModels(context.Background(), "/proxy/services/main/m/")
	if err != nil {
		t.Fatalf("ServedModels error = %v, want nil (an empty list is a valid answer)", err)
	}
	if len(got) != 0 {
		t.Fatalf("ServedModels = %v, want empty", got)
	}
}
