// SPDX-License-Identifier: MIT

package proxy

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

func TestAwait_ReturnsImmediatelyWhenAlreadyNotBlocking(t *testing.T) {
	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady})

	start := time.Now()
	snap, hasCR, timedOut := Await(context.Background(), c, "qwen", start.Add(time.Hour), 0, nil)
	if timedOut || !hasCR || snap.Phase != squallv1alpha1.ModelPhaseReady {
		t.Fatalf("Await() = %+v, %v, %v; want Ready, true, false", snap, hasCR, timedOut)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Await blocked for %v on an already-Ready model", elapsed)
	}
}

func TestAwait_UnblocksOnCacheChangeBeforeDeadline(t *testing.T) {
	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep})

	go func() {
		time.Sleep(20 * time.Millisecond)
		c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady})
	}()

	start := time.Now()
	snap, hasCR, timedOut := Await(context.Background(), c, "qwen", start.Add(2*time.Second), 0, nil)
	elapsed := time.Since(start)

	if timedOut || !hasCR || snap.Phase != squallv1alpha1.ModelPhaseReady {
		t.Fatalf("Await() = %+v, %v, %v; want Ready, true, false", snap, hasCR, timedOut)
	}
	if elapsed >= time.Second {
		t.Fatalf("Await took %v, want well under the 2s deadline (should unblock on the change)", elapsed)
	}
}

func TestAwait_TimesOutStillBlocking(t *testing.T) {
	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep})

	start := time.Now()
	snap, hasCR, timedOut := Await(context.Background(), c, "qwen", start.Add(30*time.Millisecond), 0, nil)
	elapsed := time.Since(start)

	if !timedOut || !hasCR || snap.Phase != squallv1alpha1.ModelPhaseAsleep {
		t.Fatalf("Await() = %+v, %v, %v; want Asleep, true, true", snap, hasCR, timedOut)
	}
	if elapsed < 25*time.Millisecond {
		t.Fatalf("Await returned after only %v, before its 30ms deadline", elapsed)
	}
}

func TestAwait_CancelledContextTimesOut(t *testing.T) {
	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, _, timedOut := Await(ctx, c, "qwen", time.Now().Add(2*time.Second), 0, nil)
	if !timedOut {
		t.Fatalf("Await() timedOut = false on context cancellation, want true")
	}
}

func TestAwait_TicksImmediatelyAndOnRefreshInterval(t *testing.T) {
	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep})

	var ticks int32
	tick := func() { atomic.AddInt32(&ticks, 1) }

	Await(context.Background(), c, "qwen", time.Now().Add(55*time.Millisecond), 15*time.Millisecond, tick)

	// One immediate tick plus at least two refresh-interval ticks over ~55ms
	// at a 15ms interval.
	if got := atomic.LoadInt32(&ticks); got < 3 {
		t.Fatalf("ticks = %d, want at least 3 (1 immediate + periodic refreshes)", got)
	}
}

func TestAwait_TicksOnCacheChangeToo(t *testing.T) {
	c := NewCache()
	c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep})

	var ticks int32
	tick := func() { atomic.AddInt32(&ticks, 1) }

	go func() {
		time.Sleep(10 * time.Millisecond)
		c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseWaking}) // still Block=true; Decide keeps looping.
		time.Sleep(10 * time.Millisecond)
		c.Set("qwen", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady})
	}()

	Await(context.Background(), c, "qwen", time.Now().Add(2*time.Second), 0, tick)

	// Immediate tick + at least one change-driven tick (the Waking update).
	if got := atomic.LoadInt32(&ticks); got < 2 {
		t.Fatalf("ticks = %d, want at least 2 (1 immediate + 1 on the Waking change)", got)
	}
}
