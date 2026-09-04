// SPDX-License-Identifier: MIT

package squall

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// TestEnginePort is the table the plan calls for: every engine the CRD's
// enum declares today must map to its own, explicit port. A wrong port
// makes every probe fail forever and the model never reach Ready —
// fail-safe in direction, opaque in diagnosis, so this table is what
// catches it before a real deploy does.
func TestEnginePort(t *testing.T) {
	tests := []struct {
		engine squallv1alpha1.ModelEngine
		want   int
	}{
		{squallv1alpha1.ModelEngineVLLM, 8000},
		{squallv1alpha1.ModelEngineLlamaCpp, 8080},
		{squallv1alpha1.ModelEngineOllama, 11434},
		{"unknown-future-engine", 8000}, // documented default, not silent
	}
	for _, tc := range tests {
		t.Run(string(tc.engine), func(t *testing.T) {
			if got := enginePort(tc.engine); got != tc.want {
				t.Errorf("enginePort(%q) = %d, want %d", tc.engine, got, tc.want)
			}
		})
	}
}

// TestEngineResources_PassesEveryFieldThrough is D55's unit guard. Every
// field dstack's ResourcesSpec accepts must survive the CR -> client hop
// verbatim (F33). A field dropped here does not fail: dstack substitutes its
// OWN default (2 cores, 8GB, 100GB disk, GPU count minimum ZERO), so the run
// provisions on hardware nobody asked for and bills for it.
func TestEngineResources_PassesEveryFieldThrough(t *testing.T) {
	got := engineResources(squallv1alpha1.ModelResources{
		CPU:     &squallv1alpha1.CPUSpec{Arch: "x86", Count: "4..8"},
		Memory:  "16GB..32GB",
		ShmSize: "8GB",
		Disk:    "200GB..",
		GPU: &squallv1alpha1.GPUSpec{
			Vendor:            "nvidia",
			Name:              []string{"A10G", "RTX3090"},
			Count:             "1..2",
			Memory:            "24GB..32GB",
			TotalMemory:       "48GB..",
			ComputeCapability: "7.5",
		},
	})
	if got == nil {
		t.Fatal("engineResources returned nil for a populated spec: dstack would apply ALL its own defaults")
	}
	for _, c := range []struct{ name, got, want string }{
		{"cpu.arch", got.CPUArch, "x86"},
		{"cpu.count", got.CPUCount, "4..8"},
		{"memory", got.Memory, "16GB..32GB"},
		{"shmSize", got.ShmSize, "8GB"},
		{"disk", got.Disk, "200GB.."},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if got.GPU == nil {
		t.Fatal("gpu dropped: the model would provision with NO GPU (dstack's default count minimum is 0)")
	}
	if got.GPU.Vendor != "nvidia" || got.GPU.Count != "1..2" || got.GPU.Memory != "24GB..32GB" ||
		got.GPU.TotalMemory != "48GB.." || got.GPU.ComputeCapability != "7.5" ||
		len(got.GPU.Name) != 2 || got.GPU.Name[0] != "A10G" {
		t.Errorf("gpu = %+v, want every field passed through verbatim", got.GPU)
	}
}

// TestEnginePlacement_SendsTheComplianceAllowlist: placement.backends is
// §12.3's workload-eligibility control, not a hint. Its own CRD comment says
// an empty list "would let the controller pick any backend, silently
// defeating the eligibility table" — dropping it on the way to dstack has
// exactly that effect, which is what D55 was.
func TestEnginePlacement_SendsTheComplianceAllowlist(t *testing.T) {
	price := squallv1alpha1.Price("0.80")
	got := enginePlacement(squallv1alpha1.ModelPlacement{
		Backends:        []string{"vastai", "aws"},
		Regions:         []string{"eu-west-1", "es"},
		MaxPricePerHour: &price,
	})
	if len(got.Backends) != 2 || got.Backends[0] != "vastai" || got.Backends[1] != "aws" {
		t.Errorf("backends = %v, want the allowlist passed through", got.Backends)
	}
	if len(got.Regions) != 2 || got.Regions[0] != "eu-west-1" {
		t.Errorf("regions = %v, want passed through", got.Regions)
	}
	if got.MaxPrice != "0.80" {
		t.Errorf("maxPrice = %q, want %q — an unsent price cap is an uncapped bill", got.MaxPrice, "0.80")
	}
}

// TestEnginePlacement_NoPriceIsNotZero: a nil MaxPricePerHour must send
// NOTHING, never "0". "0" is a cap of zero, which matches no offer at all.
func TestEnginePlacement_NoPriceIsNotZero(t *testing.T) {
	got := enginePlacement(squallv1alpha1.ModelPlacement{Backends: []string{"aws"}})
	if got.MaxPrice != "" {
		t.Errorf("maxPrice = %q for an unset cap, want empty: a literal 0 caps every offer out", got.MaxPrice)
	}
}

// TestEngineProbe_DefaultsPerEngine_AndCROverrides: the probe path is the
// difference between a healthy replica and one that never reaches Ready.
// It was hardcoded to /health for every engine, which is wrong for Ollama
// (no /health) and for any customised image — the same class as D55: a
// value the CR should own, decided in the binary instead.
func TestEngineProbe_DefaultsPerEngine_AndCROverrides(t *testing.T) {
	for engine, want := range map[squallv1alpha1.ModelEngine]string{
		squallv1alpha1.ModelEngineVLLM:     "/health",
		squallv1alpha1.ModelEngineLlamaCpp: "/health",
		squallv1alpha1.ModelEngineOllama:   "/",
	} {
		got := engineProbe(squallv1alpha1.ModelSpec{Engine: engine})
		if got == nil || got.Path != want {
			t.Errorf("engine %s: probe path = %v, want %q", engine, got, want)
		}
	}

	readyAfter := int32(5)
	got := engineProbe(squallv1alpha1.ModelSpec{
		Engine: squallv1alpha1.ModelEngineVLLM,
		Probe: &squallv1alpha1.ModelProbe{
			Path:       "/custom/ready",
			Method:     "HEAD",
			Interval:   &metav1.Duration{Duration: 15 * time.Second},
			Timeout:    &metav1.Duration{Duration: 3 * time.Second},
			ReadyAfter: &readyAfter,
		},
	})
	if got.Path != "/custom/ready" {
		t.Errorf("path = %q, want the CR to override the engine default", got.Path)
	}
	if got.Method != "HEAD" || got.IntervalSeconds != 15 || got.TimeoutSeconds != 3 || got.ReadyAfter != 5 {
		t.Errorf("probe = %+v, want every CR field passed through", got)
	}
}

// TestEngineProbe_IsNeverNil: unlike resources, there is no "let dstack
// decide" for the probe — dstack's default is NO probe, and §6 forbids
// "the job is running" standing in for readiness.
func TestEngineProbe_IsNeverNil(t *testing.T) {
	if engineProbe(squallv1alpha1.ModelSpec{}) == nil {
		t.Fatal("engineProbe returned nil: the run would be submitted with no probe and §6 evidence (a) could never arrive")
	}
}
