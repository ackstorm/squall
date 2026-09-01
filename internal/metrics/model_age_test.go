// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/ackstorm/squall/internal/clock"
)

func gather(t *testing.T, collectors ...prometheus.Collector) []*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewRegistry()
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	return mfs
}

func findFamily(mfs []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	return nil
}

func labelValue(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

func TestModelAgeCollector_EmitsPairWhenDeclared(t *testing.T) {
	fc := clock.NewFakeClock(time.Now())
	c := NewModelAgeCollector(fc)

	c.Observe("default", "qwen", "run-1", 24*time.Hour, fc.Now())
	fc.Advance(90 * time.Second)

	mfs := gather(t, c)

	ageFam := findFamily(mfs, "squall_model_age_seconds")
	if ageFam == nil || len(ageFam.GetMetric()) != 1 {
		t.Fatalf("squall_model_age_seconds: got %v, want exactly 1 sample", ageFam)
	}
	if got := ageFam.GetMetric()[0].GetGauge().GetValue(); got != 90 {
		t.Fatalf("age = %v, want 90 (FakeClock advanced by exactly 90s)", got)
	}
	if got := labelValue(ageFam.GetMetric()[0], "name"); got != "qwen" {
		t.Fatalf("name label = %q, want qwen", got)
	}

	maxFam := findFamily(mfs, "squall_model_max_lifetime_seconds")
	if maxFam == nil || len(maxFam.GetMetric()) != 1 {
		t.Fatalf("squall_model_max_lifetime_seconds: got %v, want exactly 1 sample", maxFam)
	}
	if got := maxFam.GetMetric()[0].GetGauge().GetValue(); got != 86400 {
		t.Fatalf("declared = %v, want 86400 (24h)", got)
	}
}

func TestModelAgeCollector_NoDeclaredMaxLifetime_EmitsNothing(t *testing.T) {
	c := NewModelAgeCollector(nil)
	c.Observe("default", "qwen", "run-1", 0, time.Now())

	mfs := gather(t, c)
	if fam := findFamily(mfs, "squall_model_age_seconds"); fam != nil && len(fam.GetMetric()) != 0 {
		t.Fatalf("expected no samples with MaxLifetime unset (0), got %d", len(fam.GetMetric()))
	}
}

func TestModelAgeCollector_NoRun_ForgetsAndEmitsNothing(t *testing.T) {
	c := NewModelAgeCollector(nil)
	c.Observe("default", "qwen", "run-1", time.Hour, time.Now())
	c.Observe("default", "qwen", "", time.Hour, time.Now()) // run gone.

	mfs := gather(t, c)
	if fam := findFamily(mfs, "squall_model_age_seconds"); fam != nil && len(fam.GetMetric()) != 0 {
		t.Fatalf("expected no samples once runID is empty, got %d", len(fam.GetMetric()))
	}
}

func TestModelAgeCollector_RunIDChange_ResetsAgeBaseline(t *testing.T) {
	fc := clock.NewFakeClock(time.Now())
	c := NewModelAgeCollector(fc)

	c.Observe("default", "qwen", "run-1", time.Hour, fc.Now())
	fc.Advance(30 * time.Minute)
	c.Observe("default", "qwen", "run-2", time.Hour, fc.Now()) // F20: a recreate mints a new run id.

	mfs := gather(t, c)
	fam := findFamily(mfs, "squall_model_age_seconds")
	if fam == nil || len(fam.GetMetric()) != 1 {
		t.Fatalf("expected exactly 1 sample, got %v", fam)
	}
	if got := fam.GetMetric()[0].GetGauge().GetValue(); got != 0 {
		t.Fatalf("age after a runID change = %v, want 0 (baseline reset, F20 recreate is a new generation)", got)
	}
}

func TestModelAgeCollector_Forget_DropsFromScrape(t *testing.T) {
	c := NewModelAgeCollector(nil)
	c.Observe("default", "qwen", "run-1", time.Hour, time.Now())
	c.Forget("default", "qwen")
	c.Forget("default", "qwen") // idempotent.

	mfs := gather(t, c)
	if fam := findFamily(mfs, "squall_model_age_seconds"); fam != nil && len(fam.GetMetric()) != 0 {
		t.Fatalf("expected no samples after Forget, got %d", len(fam.GetMetric()))
	}
}
