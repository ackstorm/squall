// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"testing"
	"time"

	"github.com/ackstorm/squall/internal/clock"
)

func TestUncontrolledCollector_EmitsObservedAndDeclared(t *testing.T) {
	now := time.Now()
	fc := clock.NewFakeClock(now)
	c := NewUncontrolledCollector(fc)
	c.Observe("default", "qwen", nil, 23*time.Minute)
	mfs := gather(t, c)
	if findFamily(mfs, "squall_model_uncontrolled_seconds").GetMetric()[0].GetGauge().GetValue() != 0 {
		t.Fatal("controlled value must be zero")
	}
	if findFamily(mfs, "squall_model_uncontrolled_timeout_seconds").GetMetric()[0].GetGauge().GetValue() != 23*60 {
		t.Fatal("wrong timeout")
	}
	since := now.Add(-10 * time.Minute)
	c.Observe("default", "qwen", &since, 0)
	fc.Advance(5 * time.Minute)
	mfs = gather(t, c)
	if got := findFamily(mfs, "squall_model_uncontrolled_seconds").GetMetric()[0].GetGauge().GetValue(); got != 15*60 {
		t.Fatalf("uncontrolled seconds=%v", got)
	}
	if got := findFamily(mfs, "squall_model_uncontrolled_timeout_seconds").GetMetric()[0].GetGauge().GetValue(); got != 0 {
		t.Fatalf("opt-out timeout=%v", got)
	}
	c.Forget("default", "qwen")
	if fam := findFamily(gather(t, c), "squall_model_uncontrolled_seconds"); fam != nil {
		t.Fatal("expected forgotten series to disappear")
	}
}
