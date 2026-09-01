// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/clock"
)

// ActivityTracker is squall-proxy's half of §6's idle contract: per-model
// InFlight and LastRequestAt, served at squallv1alpha1.ActivityPath for
// squall-controller's gatherActivity to poll. It uses the SAME wire
// contract the controller already decodes (api/squall/v1alpha1) rather
// than inventing a second shape.
type ActivityTracker struct {
	clock clock.Clock

	mu     sync.Mutex
	models map[string]*modelCounters
}

type modelCounters struct {
	inFlight             int
	lastRequestAt        time.Time
	lastSuccessAt        time.Time
	failuresSinceSuccess int
}

// NewActivityTracker builds a tracker. clk defaults to clock.RealClock{}
// when nil.
func NewActivityTracker(clk clock.Clock) *ActivityTracker {
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &ActivityTracker{clock: clk, models: make(map[string]*modelCounters)}
}

// Begin records that this replica has accepted a request for model, BEFORE
// any upstream call — ledger D11: incrementing at accept time rather than
// after a successful forward minimises a known TOCTOU window in the sleep
// decision (a request accepted but not yet reflected in InFlight is
// invisible to §6's aggregation). It returns a Done func the caller MUST
// call exactly once when the request finishes, forwarded or not.
func (t *ActivityTracker) Begin(model string) (done func()) {
	now := t.clock.Now()

	t.mu.Lock()
	c, ok := t.models[model]
	if !ok {
		c = &modelCounters{}
		t.models[model] = c
	}
	c.inFlight++
	c.lastRequestAt = now
	t.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			c.inFlight--
			// Advance the anchor to COMPLETION as well as accept. Begin's
			// accept-time stamp is what guarantees an accepted request is never
			// invisible to the sleep aggregation (D11); this stops that stamp
			// from going stale while the request is held.
			//
			// MEASURED 2026-09-01 on a live GPU: a cold wake held one request
			// for 213s. The moment it finished, the anchor was 213s old against
			// a 120s idle window, so the controller slept the machine
			// immediately -- the whole cold start bought exactly one answer, and
			// the next request paid the cold start again.
			//
			// The clock is read INSIDE the lock so the anchor cannot move
			// backwards: reading it outside allows a slow goroutine to overwrite
			// a newer accept with its own older timestamp. Under the lock the
			// value is monotonic by construction, so it can only ever DELAY a
			// sleep -- the direction `1->0 fails safe` requires.
			c.lastRequestAt = t.clock.Now()
		})
	}
}

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
	// The reset is the point. A lifetime failure total would condemn a replica
	// forever for one bad minute; what the unhealthy verdict needs to know is
	// whether anything has worked SINCE.
	c.failuresSinceSuccess = 0
}

// Failure records that a request for model reached a verdict about the replica
// and the replica did not deliver: a committed non-2xx, or a Ready Model the
// gateway would not serve. Counted so that the controller's unhealthy teardown
// has a floor under it and cannot fire on one bad request.
//
// It is deliberately NOT called for a client that disconnected mid-stream (not
// the replica's fault) nor for the ordinary wait-contract of a Model that is
// still waking (nothing has been promised yet). Counting either would let
// normal cold starts accumulate towards a teardown.
func (t *ActivityTracker) Failure(model string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.models[model]
	if !ok {
		c = &modelCounters{}
		t.models[model] = c
	}
	c.failuresSinceSuccess++
}

// Report is the ActivityReport for every model this tracker has ever
// Begin-run — a model never routed to is correctly absent (D4's "must
// distinguish no-data from 0 in-flight"), not defaulted to a zero entry.
func (t *ActivityTracker) Report() squallv1alpha1.ActivityReport {
	t.mu.Lock()
	defer t.mu.Unlock()

	report := squallv1alpha1.ActivityReport{Models: make(map[string]squallv1alpha1.ModelActivity, len(t.models))}
	for name, c := range t.models {
		report.Models[name] = squallv1alpha1.ModelActivity{
			InFlight:             c.inFlight,
			LastRequestAt:        c.lastRequestAt,
			LastSuccessAt:        c.lastSuccessAt,
			FailuresSinceSuccess: c.failuresSinceSuccess,
		}
	}
	return report
}

// ServeHTTP answers squallv1alpha1.ActivityPath with the current Report.
func (t *ActivityTracker) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(t.Report()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
