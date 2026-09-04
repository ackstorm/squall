// SPDX-License-Identifier: MIT

package metrics

import "testing"

func TestModelPriceCollector_EmitsPairOnlyWhenBothSidesPresent(t *testing.T) {
	c := NewModelPriceCollector()
	c.Observe("default", "qwen", 1.23, true, 2.00, true)

	mfs := gather(t, c)

	obsFam := findFamily(mfs, "squall_model_price_per_hour")
	if obsFam == nil || len(obsFam.GetMetric()) != 1 {
		t.Fatalf("squall_model_price_per_hour: got %v, want exactly 1 sample", obsFam)
	}
	if got := obsFam.GetMetric()[0].GetGauge().GetValue(); got != 1.23 {
		t.Fatalf("observed = %v, want 1.23", got)
	}

	declFam := findFamily(mfs, "squall_model_max_price_per_hour")
	if declFam == nil || len(declFam.GetMetric()) != 1 {
		t.Fatalf("squall_model_max_price_per_hour: got %v, want exactly 1 sample", declFam)
	}
	if got := declFam.GetMetric()[0].GetGauge().GetValue(); got != 2.00 {
		t.Fatalf("declared = %v, want 2.00", got)
	}
}

func TestModelPriceCollector_MissingObserved_EmitsNeitherSide(t *testing.T) {
	// D26: an absent observed price must not produce a declared-only series —
	// it would misrepresent
	// "no data" as a real comparison point for the observed > declared
	// alert.
	c := NewModelPriceCollector()
	c.Observe("default", "qwen", 0, false, 2.00, true)

	mfs := gather(t, c)
	if fam := findFamily(mfs, "squall_model_price_per_hour"); fam != nil && len(fam.GetMetric()) != 0 {
		t.Fatalf("expected no observed samples, got %d", len(fam.GetMetric()))
	}
	if fam := findFamily(mfs, "squall_model_max_price_per_hour"); fam != nil && len(fam.GetMetric()) != 0 {
		t.Fatalf("expected no declared samples either (pair is all-or-nothing), got %d", len(fam.GetMetric()))
	}
}

func TestModelPriceCollector_MissingDeclared_EmitsNeitherSide(t *testing.T) {
	c := NewModelPriceCollector()
	c.Observe("default", "qwen", 1.23, true, 0, false)

	mfs := gather(t, c)
	if fam := findFamily(mfs, "squall_model_price_per_hour"); fam != nil && len(fam.GetMetric()) != 0 {
		t.Fatalf("expected no samples when maxPricePerHour is unset, got %d", len(fam.GetMetric()))
	}
}

func TestModelPriceCollector_Forget_DropsFromScrape(t *testing.T) {
	c := NewModelPriceCollector()
	c.Observe("default", "qwen", 1.23, true, 2.00, true)
	c.Forget("default", "qwen")
	c.Forget("default", "qwen") // idempotent.

	mfs := gather(t, c)
	if fam := findFamily(mfs, "squall_model_price_per_hour"); fam != nil && len(fam.GetMetric()) != 0 {
		t.Fatalf("expected no samples after Forget, got %d", len(fam.GetMetric()))
	}
}
