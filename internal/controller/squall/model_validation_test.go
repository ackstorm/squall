// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// exampleModelSpec returns the spec §5.1 example CR, transcribed verbatim
// (qwen3-8-27b) as a ModelSpec. Individual tests mutate a copy of this
// baseline rather than building specs from scratch, so each case exercises
// exactly one rule against an otherwise-valid CR.
func exampleModelSpec() squallv1alpha1.ModelSpec {
	return squallv1alpha1.ModelSpec{
		Engine:   squallv1alpha1.ModelEngineOllama,
		Image:    "ollama/ollama@sha256:" + strings.Repeat("a", 64),
		Features: []squallv1alpha1.ModelFeature{squallv1alpha1.ModelFeatureTextGeneration},
		Args:     []string{},
		Env: map[string]string{
			"OLLAMA_CONTEXT_LENGTH":  "65536",
			"OLLAMA_FLASH_ATTENTION": "1",
			"OLLAMA_KV_CACHE_TYPE":   "q8_0",
		},
		Resources: squallv1alpha1.ModelResources{
			GPU: &squallv1alpha1.GPUSpec{
				Name:   []string{"A10G", "RTX3090", "RTX5090"},
				Memory: "24GB..32GB",
			},
		},
		Placement: squallv1alpha1.ModelPlacement{
			Backends: []string{"vastai"},
			Regions:  []string{},
		},
		MinReplicas:           0,
		HoldTimeout:           metav1.Duration{Duration: 20 * time.Minute},
		ScaleDownDelaySeconds: 300,
		Fleet: squallv1alpha1.ModelFleet{
			IdleDuration: metav1.Duration{Duration: 10 * time.Minute},
		},
		DrainTimeout:        metav1.Duration{Duration: 120 * time.Second},
		ProvisioningTimeout: metav1.Duration{Duration: 45 * time.Minute},
		MaxLifetime:         metav1.Duration{Duration: 168 * time.Hour},
	}
}

// TestValidate_HoldTimeoutExceedsProvisioningTimeout covers spec §5.1's
// deadline ordering: "Deadlines are ordered: holdTimeout ≤
// provisioningTimeout". A hold that outlives the destructive provisioning
// deadline can never be satisfied by a successful wake, so it is rejected
// rather than merely warned about.
func TestValidate_HoldTimeoutExceedsProvisioningTimeout(t *testing.T) {
	spec := exampleModelSpec()
	spec.ProvisioningTimeout = metav1.Duration{Duration: 10 * time.Minute}
	spec.HoldTimeout = metav1.Duration{Duration: 20 * time.Minute}

	if err := Validate(spec); err == nil {
		t.Fatal("Validate() = nil, want an error: holdTimeout (20m) > provisioningTimeout (10m)")
	}
}

// TestValidate_FleetIdleDurationRequired covers F21: dstack's own fleet
// idle-release default is 3 days, so the CR MUST NOT be allowed to leave
// fleet.idleDuration unset (the Go zero value) or explicitly zero — both
// collapse to the same "no release window configured" state.
func TestValidate_FleetIdleDurationRequired(t *testing.T) {
	tests := []struct {
		name     string
		duration metav1.Duration
	}{
		{"absent (zero value)", metav1.Duration{}},
		{"explicit zero", metav1.Duration{Duration: 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := exampleModelSpec()
			spec.Fleet.IdleDuration = tt.duration

			if err := Validate(spec); err == nil {
				t.Fatal("Validate() = nil, want an error: fleet.idleDuration must be > 0 (F21)")
			}
		})
	}
}

// TestValidate_PlacementBackendsEmpty covers §12.3: placement.backends is
// the enforcement point for the workload-eligibility table, not
// documentation beside it. An empty list would let the controller pick any
// backend, silently defeating the classification.
func TestValidate_PlacementBackendsEmpty(t *testing.T) {
	spec := exampleModelSpec()
	spec.Placement.Backends = nil

	if err := Validate(spec); err == nil {
		t.Fatal("Validate() = nil, want an error: placement.backends must not be empty (§12.3)")
	}
}

// TestValidate_HoldTimeoutOutlastsWarmWindow_Warns covers §5.1's warm-window
// rule: a holdTimeout that habitually waits out a full cold start (because
// the warm window — scaleDownDelaySeconds + fleet.idleDuration — is short
// relative to it) is "a misconfiguration in all but intent" and MUST be a
// warning, never a rejection.
func TestValidate_HoldTimeoutOutlastsWarmWindow_Warns(t *testing.T) {
	spec := exampleModelSpec()
	spec.ScaleDownDelaySeconds = 60                                      // 1m
	spec.Fleet.IdleDuration = metav1.Duration{Duration: 2 * time.Minute} // warm window = 3m
	spec.HoldTimeout = metav1.Duration{Duration: 20 * time.Minute}       // >> warm window
	spec.ProvisioningTimeout = metav1.Duration{Duration: 45 * time.Minute}

	warnings, err := ValidateWithWarnings(spec)
	if err != nil {
		t.Fatalf("ValidateWithWarnings() error = %v, want nil (this is a warning, not a rejection)", err)
	}
	if len(warnings) == 0 {
		t.Fatal("ValidateWithWarnings() warnings = [], want a warning: holdTimeout (20m) far outlasts the warm window (3m)")
	}
}

// TestValidate_ExampleCR_ValidatesCleanly asserts the spec §5.1 example CR
// (qwen3-8-27b, transcribed verbatim in exampleModelSpec) is accepted:
// none of the rejection rules fire against the reference example.
func TestValidate_ExampleCR_ValidatesCleanly(t *testing.T) {
	spec := exampleModelSpec()

	if err := Validate(spec); err != nil {
		t.Fatalf("Validate() error = %v, want nil for the spec §5.1 example CR", err)
	}
}
