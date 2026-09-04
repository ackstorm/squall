// SPDX-License-Identifier: MIT

// Package clock abstracts wall-clock time so anything timing-dependent can
// be driven deterministically in tests instead of sleeping for real:
// RealClock backs production, FakeClock backs tests. This is the seam the
// whole project reuses wherever a timer would otherwise call time.Now()
// directly — dstack/mock's fleet idle release (F21) today, Phase 7's sleep
// flip and Phase 8's drain later. No test may use time.Sleep; a test that
// owns a FakeClock advances it explicitly and re-drives whatever polls the
// clock (e.g. dstack/mock.Server.Tick).
package clock

import (
	"sync"
	"time"
)

// Clock abstracts wall-clock time.
type Clock interface {
	Now() time.Time
}

// RealClock is the production Clock: a thin wrapper over time.Now.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// FakeClock is a Clock a test owns and advances explicitly. It never moves
// on its own — callers must call Advance, then re-drive whatever polls the
// clock (e.g. dstack/mock.Server.Tick).
type FakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// NewFakeClock returns a FakeClock starting at t.
func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{t: t}
}

// Now returns the clock's current (fake) time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward by d. It never runs backwards implicitly;
// pass a negative d only if a test genuinely needs to rewind.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
