// SPDX-License-Identifier: MIT

package proxy

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ackstorm/squall/internal/clock"
)

// Patcher issues the coalesced demand-annotation patch (squallv1alpha1.
// DemandAnnotation) against a Model CR. Production implements this over
// the dynamic client (cmd/proxy/main.go); tests use a fake recorder.
type Patcher interface {
	PatchDemand(ctx context.Context, model string, at time.Time) error
}

// DemandCoalescer rate-limits Patcher calls per Model to at most one per
// cooldown, however many concurrent requests ask for one (task 9.3) — and,
// per the "demand is self-expiring" decision
// (docs/references/decisions-and-open-items.md), REFRESHES rather than
// writes once: a caller that keeps calling Signal while a request is held
// (via Await's tick hook) keeps re-arming the controller's TTL check
// (hasDemand) so a long wait cannot age the annotation out from under
// itself.
type DemandCoalescer struct {
	patcher  Patcher
	cooldown time.Duration
	clock    clock.Clock

	mu    sync.Mutex
	state map[string]*demandState
}

type demandState struct {
	mu          sync.Mutex
	lastSuccess time.Time
	everPatched bool
}

// NewDemandCoalescer builds a coalescer issuing at most one Patcher call
// per model per cooldown. clk defaults to clock.RealClock{} when nil.
func NewDemandCoalescer(p Patcher, cooldown time.Duration, clk clock.Clock) *DemandCoalescer {
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &DemandCoalescer{
		patcher:  p,
		cooldown: cooldown,
		clock:    clk,
		state:    make(map[string]*demandState),
	}
}

// Signal asks for a demand patch on model, coalesced per cooldown. Errors
// from Patcher are swallowed here by design: a demand patch is best-effort
// on the wake ("fails open") side (§7) — a failed attempt does NOT update
// lastSuccess, so the very next Signal call (from this or another blocked
// request) retries rather than being suppressed for a full cooldown window
// by a transient failure.
//
// The per-model state lock is held (blocking, not TryLock) across the
// cooldown check and the patch call, so N callers racing inside one
// cooldown window serialize onto exactly one Patcher invocation: the first
// one through sets lastSuccess to the shared clock's "now", and every
// caller still queued behind the lock — regardless of how many — observes
// that same "now" and finds the cooldown already satisfied.
func (c *DemandCoalescer) Signal(ctx context.Context, model string) {
	if c == nil || c.patcher == nil {
		return
	}
	st := c.stateFor(model)

	st.mu.Lock()
	defer st.mu.Unlock()

	now := c.clock.Now()
	if st.everPatched && now.Sub(st.lastSuccess) < c.cooldown {
		return
	}
	if err := c.patcher.PatchDemand(ctx, model, now); err != nil {
		// Still best-effort — the next Signal retries — but no longer
		// SILENT (D103): a patch that 404s on every attempt is a Model
		// that can never wake, and it was diagnosable only by reading
		// this function's source. Fires per retry (a failure does not arm
		// the cooldown, by design), so its cadence is the tick interval.
		slog.Warn("demand patch failed; model cannot signal demand until this succeeds",
			"model", model, "err", err)
		return
	}
	st.lastSuccess = now
	st.everPatched = true
}

func (c *DemandCoalescer) stateFor(model string) *demandState {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.state[model]
	if !ok {
		st = &demandState{}
		c.state[model] = st
	}
	return st
}
