// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ackstorm/squall/internal/clock"
)

type fakePatcher struct {
	mu    sync.Mutex
	calls []time.Time
	err   error
}

func (p *fakePatcher) PatchDemand(_ context.Context, _ string, at time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.calls = append(p.calls, at)
	return nil
}

func (p *fakePatcher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func TestDemandCoalescer_ConcurrentSignalsCollapseToOnePatch(t *testing.T) {
	fc := clock.NewFakeClock(time.Now())
	p := &fakePatcher{}
	c := NewDemandCoalescer(p, time.Minute, fc)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Signal(context.Background(), "qwen")
		}()
	}
	wg.Wait()

	if got := p.count(); got != 1 {
		t.Fatalf("Patcher calls = %d, want exactly 1 for 20 concurrent Signals inside one cooldown", got)
	}
}

func TestDemandCoalescer_RefreshesAfterCooldownElapses(t *testing.T) {
	fc := clock.NewFakeClock(time.Now())
	p := &fakePatcher{}
	c := NewDemandCoalescer(p, time.Minute, fc)

	c.Signal(context.Background(), "qwen")
	c.Signal(context.Background(), "qwen") // still inside cooldown -> suppressed.
	if got := p.count(); got != 1 {
		t.Fatalf("after 2 Signals inside cooldown: calls = %d, want 1", got)
	}

	fc.Advance(time.Minute + time.Second) // demand persists, self-expiring annotation must be refreshed.
	c.Signal(context.Background(), "qwen")
	if got := p.count(); got != 2 {
		t.Fatalf("after cooldown elapsed: calls = %d, want 2 (a refresh, not a one-shot write)", got)
	}
}

func TestDemandCoalescer_PerModelIndependence(t *testing.T) {
	fc := clock.NewFakeClock(time.Now())
	p := &fakePatcher{}
	c := NewDemandCoalescer(p, time.Minute, fc)

	c.Signal(context.Background(), "qwen")
	c.Signal(context.Background(), "llama")
	if got := p.count(); got != 2 {
		t.Fatalf("calls = %d, want 2 (independent per-model cooldowns)", got)
	}
}

func TestDemandCoalescer_FailedPatchDoesNotSuppressNextSignal(t *testing.T) {
	fc := clock.NewFakeClock(time.Now())
	p := &fakePatcher{err: errors.New("patch failed")}
	c := NewDemandCoalescer(p, time.Minute, fc)

	c.Signal(context.Background(), "qwen")
	if got := p.count(); got != 0 {
		t.Fatalf("calls with a failing Patcher = %d, want 0 (nothing recorded, but no panic)", got)
	}

	p.mu.Lock()
	p.err = nil
	p.mu.Unlock()

	// A failed attempt must not have set lastSuccess, so the very next
	// Signal (still inside what would have been the cooldown window) must
	// retry rather than being suppressed for a full cooldown by the
	// transient failure.
	c.Signal(context.Background(), "qwen")
	if got := p.count(); got != 1 {
		t.Fatalf("calls after retry following a failure = %d, want 1", got)
	}
}
