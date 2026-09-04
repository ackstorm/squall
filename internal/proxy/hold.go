// SPDX-License-Identifier: MIT

package proxy

import (
	"context"
	"time"
)

// Await is task 9.2's blocking hold (F32, KubeAI's mechanism): it blocks
// the caller's goroutine — writing NOTHING to the client itself, that is
// the caller's job — until either the cache reports a snapshot Decide would
// no longer Block on, or deadline elapses. It returns the freshest snapshot
// it has (for the caller to re-run Decide against) and timedOut, which is
// true only when the deadline was reached with no such change observed.
//
// tick, when non-nil, is called once immediately and then again on every
// cache-change notification and every refreshInterval tick while still
// blocked — the seam task 9.3's demand refresh hangs off, so a held request
// keeps the coalesced demand annotation from self-expiring mid-wait
// (decisions-and-open-items.md's "demand is self-expiring"). refreshInterval
// <= 0 disables the periodic tick (only the immediate one and change-driven
// ones still fire); callers that pass nil tick can pass 0 too.
//
// The deadline itself is real wall-clock (context.WithDeadline's underlying
// timer has no fake-clock seam) — F32's hold is fundamentally about a real
// client actually waiting, so production always uses real time regardless
// of which clock.Clock computed the deadline instant upstream.
func Await(ctx context.Context, cache *ModelCache, model string, deadline time.Time, refreshInterval time.Duration, tick func()) (snap ModelSnapshot, hasCR bool, timedOut bool) {
	if tick != nil {
		tick()
	}
	if snap, hasCR = cache.Get(model); !Decide(snap.Phase, hasCR, 0).Block {
		return snap, hasCR, false
	}

	notify, cancel := cache.Subscribe(model)
	defer cancel()

	var tickerC <-chan time.Time
	if refreshInterval > 0 {
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		tickerC = ticker.C
	}

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			snap, hasCR = cache.Get(model)
			return snap, hasCR, true
		}

		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			snap, hasCR = cache.Get(model)
			return snap, hasCR, true
		case <-timer.C:
			snap, hasCR = cache.Get(model)
			return snap, hasCR, true
		case <-notify:
			timer.Stop()
			if tick != nil {
				tick()
			}
		case <-tickerC:
			timer.Stop()
			if tick != nil {
				tick()
			}
		}

		snap, hasCR = cache.Get(model)
		if !Decide(snap.Phase, hasCR, 0).Block {
			return snap, hasCR, false
		}
	}
}
