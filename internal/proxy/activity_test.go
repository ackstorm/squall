// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/clock"
)

func TestActivityTracker_BeginIncrementsBeforeUpstreamCall(t *testing.T) {
	fc := clock.NewFakeClock(time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC))
	tr := NewActivityTracker(fc)

	done := tr.Begin("qwen")
	report := tr.Report()
	got, ok := report.Models["qwen"]
	if !ok || got.InFlight != 1 {
		t.Fatalf("Report() after Begin, before any upstream call = %+v, ok=%v; want InFlight=1", got, ok)
	}
	if !got.LastRequestAt.Equal(fc.Now()) {
		t.Fatalf("LastRequestAt = %v, want %v", got.LastRequestAt, fc.Now())
	}

	done()
	report = tr.Report()
	if got := report.Models["qwen"].InFlight; got != 0 {
		t.Fatalf("InFlight after done() = %d, want 0", got)
	}
}

func TestActivityTracker_DoneIsIdempotent(t *testing.T) {
	tr := NewActivityTracker(nil)
	done := tr.Begin("qwen")
	done()
	done() // must not double-decrement below zero.
	if got := tr.Report().Models["qwen"].InFlight; got != 0 {
		t.Fatalf("InFlight after calling done() twice = %d, want 0", got)
	}
}

func TestActivityTracker_UnroutedModelAbsentNotZero(t *testing.T) {
	tr := NewActivityTracker(nil)
	tr.Begin("qwen")

	report := tr.Report()
	if _, ok := report.Models["never-routed"]; ok {
		t.Fatal("a model never Begin-run must be absent from Report(), not present with InFlight: 0")
	}
}

func TestActivityTracker_ServeHTTP(t *testing.T) {
	tr := NewActivityTracker(nil)
	done := tr.Begin("qwen")
	defer done()

	srv := httptest.NewServer(http.HandlerFunc(tr.ServeHTTP))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + squallv1alpha1.ActivityPath)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close on a read-only GET.

	var report squallv1alpha1.ActivityReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := report.Models["qwen"].InFlight; got != 1 {
		t.Fatalf("InFlight over the wire = %d, want 1", got)
	}
}

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

// TestActivityTracker_FailuresSinceSuccess pins the consecutive-failure
// counter: it is the evidence floor for the controller's unhealthy teardown,
// so "reset on success" is the load-bearing half, not the increment. Without
// the reset the count is a lifetime total and would condemn a replica forever
// for one bad minute.
func TestActivityTracker_FailuresSinceSuccess(t *testing.T) {
	tr := NewActivityTracker(nil)

	if got := tr.Report().Models["m"].FailuresSinceSuccess; got != 0 {
		t.Fatalf("unseen model reports %d failures, want 0", got)
	}

	tr.Failure("m")
	tr.Failure("m")
	if got := tr.Report().Models["m"].FailuresSinceSuccess; got != 2 {
		t.Fatalf("after 2 failures = %d, want 2", got)
	}

	tr.Success("m")
	if got := tr.Report().Models["m"].FailuresSinceSuccess; got != 0 {
		t.Fatalf("a success must reset the counter, got %d, want 0", got)
	}

	tr.Failure("m")
	if got := tr.Report().Models["m"].FailuresSinceSuccess; got != 1 {
		t.Fatalf("after the reset = %d, want 1", got)
	}
}
