// SPDX-License-Identifier: MIT

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
		MinReplicas:         0,
		HoldTimeout:         metav1.Duration{Duration: 20 * time.Minute},
		IdleTimeout:         metav1.Duration{Duration: 5 * time.Minute},
		DrainTimeout:        metav1.Duration{Duration: 120 * time.Second},
		ProvisioningTimeout: metav1.Duration{Duration: 45 * time.Minute},
		MaxLifetime:         metav1.Duration{Duration: 168 * time.Hour},
	}
}

func TestValidate_UncontrolledTimeoutBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		d       time.Duration
		wantErr bool
	}{
		{"zero", 0, true}, {"negative", -time.Minute, true}, {"maximum", 24 * time.Hour, false}, {"above", 25 * time.Hour, true}, {"ordinary", 90 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := exampleModelSpec()
			spec.UncontrolledTimeout = &metav1.Duration{Duration: tc.d}
			_, err := ValidateWithWarnings(spec)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestValidate_HardStopBounds(t *testing.T) {
	for _, tc := range []struct {
		name         string
		hard, unctrl time.Duration
		wantErr      bool
	}{
		{"ordinary", 24 * time.Hour, 90 * time.Minute, false}, {"minimum", time.Hour, 30 * time.Minute, false},
		{"below minimum", 30 * time.Minute, 20 * time.Minute, true}, {"before deadline", 2 * time.Hour, 3 * time.Hour, true}, {"zero", 0, 90 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := exampleModelSpec()
			spec.HardStop = metav1.Duration{Duration: tc.hard}
			spec.UncontrolledTimeout = &metav1.Duration{Duration: tc.unctrl}
			_, err := ValidateWithWarnings(spec)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestValidate_HardStopZeroWarns(t *testing.T) {
	spec := exampleModelSpec()
	spec.HardStop = metav1.Duration{}
	warnings, err := ValidateWithWarnings(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "hardStop") {
			return
		}
	}
	t.Fatal("expected hardStop warning")
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

// TestValidate_ExampleCR_ValidatesCleanly asserts the spec §5.1 example CR
// (qwen3-8-27b, transcribed verbatim in exampleModelSpec) is accepted:
// none of the rejection rules fire against the reference example.
func TestValidate_ExampleCR_ValidatesCleanly(t *testing.T) {
	spec := exampleModelSpec()

	if err := Validate(spec); err != nil {
		t.Fatalf("Validate() error = %v, want nil for the spec §5.1 example CR", err)
	}
}

func TestValidate_RejectsNonPositiveIdleTimeout(t *testing.T) {
	spec := exampleModelSpec()
	spec.IdleTimeout = metav1.Duration{Duration: 0}
	if _, err := ValidateWithWarnings(spec); err == nil {
		t.Fatal("ValidateWithWarnings() = nil; a zero idleTimeout expires the demand " +
			"annotation the instant it lands, making an on-demand Model permanently unwakeable")
	}
}

func TestValidate_RejectsNonPositiveProvisioningTimeout(t *testing.T) {
	spec := exampleModelSpec()
	spec.ProvisioningTimeout = metav1.Duration{Duration: 0}
	spec.HoldTimeout = metav1.Duration{Duration: 0}
	if _, err := ValidateWithWarnings(spec); err == nil {
		t.Fatal("ValidateWithWarnings() = nil; provisioningDue is a no-op for a " +
			"non-positive timeout, and it is the primary bound on a run that never " +
			"reaches Ready")
	}
}

func TestValidate_WarnsWhenHoldCannotCoverAColdStart(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hold        time.Duration
		provisning  time.Duration
		wantWarning bool
	}{
		{"hold far below half the provisioning window", 5 * time.Minute, 30 * time.Minute, true},
		{"hold at half exactly is not warned", 15 * time.Minute, 30 * time.Minute, false},
		{"hold above half", 20 * time.Minute, 30 * time.Minute, false},
		{"hold disabled is not this warning's business", 0, 30 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := exampleModelSpec()
			spec.HoldTimeout = metav1.Duration{Duration: tc.hold}
			spec.ProvisioningTimeout = metav1.Duration{Duration: tc.provisning}

			warnings, err := ValidateWithWarnings(spec)
			if err != nil {
				t.Fatalf("ValidateWithWarnings() error = %v, want nil", err)
			}
			var got bool
			for _, w := range warnings {
				if strings.Contains(w, "cold start") {
					got = true
				}
			}
			if got != tc.wantWarning {
				t.Errorf("cold-start warning = %v, want %v (hold %s, provisioning %s); warnings = %v",
					got, tc.wantWarning, tc.hold, tc.provisning, warnings)
			}
		})
	}
}
