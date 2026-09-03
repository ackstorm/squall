package metrics

import (
	"github.com/ackstorm/squall/internal/clock"
	"testing"
	"time"
)

func TestIdleCollector_EmitsOnlyForActiveRunsWithAnAnchor(t *testing.T) {
	now := time.Now()
	fc := clock.NewFakeClock(now)
	c := NewIdleCollector(fc)
	last := now.Add(-10 * time.Minute)
	c.Observe("default", "qwen", &last, false, 5*time.Minute)
	if findFamily(gather(t, c), "squall_model_idle_seconds") != nil {
		t.Fatal("inactive emitted")
	}
	c.Observe("default", "qwen", nil, true, 5*time.Minute)
	if findFamily(gather(t, c), "squall_model_idle_seconds") != nil {
		t.Fatal("no anchor emitted")
	}
	c.Observe("default", "qwen", &last, true, 5*time.Minute)
	fc.Advance(2 * time.Minute)
	if got := findFamily(gather(t, c), "squall_model_idle_seconds").GetMetric()[0].GetGauge().GetValue(); got != 720 {
		t.Fatalf("got %v", got)
	}
	if got := findFamily(gather(t, c), "squall_model_scale_down_delay_seconds").GetMetric()[0].GetGauge().GetValue(); got != 300 {
		t.Fatalf("got %v", got)
	}
	c.Forget("default", "qwen")
	if findFamily(gather(t, c), "squall_model_idle_seconds") != nil {
		t.Fatal("not forgotten")
	}
}
