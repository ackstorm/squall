// SPDX-License-Identifier: MIT

package metrics

import (
	"testing"
	"time"
)

func TestReaperCollector_ZeroBeforeFirstSuccessThenTracksBoth(t *testing.T) {
	c := NewReaperCollector()
	val := func(name string) float64 {
		f := findFamily(gather(t, c), name)
		if f == nil {
			t.Fatalf("%s absent", name)
		}
		return f.GetMetric()[0].GetGauge().GetValue()
	}
	if got := val("squall_reaper_last_success_timestamp_seconds"); got != 0 {
		t.Fatalf("timestamp before first success = %v, want 0", got)
	}
	if got := val("squall_reaper_refused_runs"); got != 0 {
		t.Fatalf("refused = %v, want 0", got)
	}
	c.SweepSucceeded(time.Unix(1_800_000_000, 0))
	c.SetRefused(3)
	if got := val("squall_reaper_last_success_timestamp_seconds"); got != 1_800_000_000 {
		t.Fatalf("timestamp = %v", got)
	}
	if got := val("squall_reaper_refused_runs"); got != 3 {
		t.Fatalf("refused = %v, want 3", got)
	}
}
