// SPDX-License-Identifier: MIT

package squall

import (
	"fmt"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// Validate enforces the Model cross-field rules spec §5.1 lists under
// "Validation (webhook/CEL), enforced not documented" that OpenAPI schema
// cannot express structurally — deadline ordering, the idleTimeout/
// provisioningTimeout floors, and the §12.3 backends allowlist. Structural
// rules (the engine/minReplicas enums, placement.backends' MinItems=1, the
// image digest pattern) are already enforced by the CRD schema generated
// from api/squall/v1alpha1/model_types.go and are deliberately NOT
// duplicated here.
func Validate(spec squallv1alpha1.ModelSpec) error {
	_, err := ValidateWithWarnings(spec)
	return err
}

// ValidateWithWarnings runs the same rejection rules as Validate, plus the
// §5.1 cold-start rule, which is advisory: a holdTimeout that habitually
// waits out a full cold start is "a misconfiguration in all but intent" —
// legal, but worth surfacing.
func ValidateWithWarnings(spec squallv1alpha1.ModelSpec) ([]string, error) {
	if len(spec.Placement.Backends) == 0 {
		return nil, fmt.Errorf("placement.backends must not be empty: it enforces the §12.3 workload-eligibility allowlist, not merely documents it")
	}

	if u := spec.UncontrolledTimeout; u != nil {
		if u.Duration <= 0 {
			return nil, fmt.Errorf("uncontrolledTimeout must be > 0 (omit it for the default)")
		}
		if u.Duration > MaxExplicitUncontrolledTimeout {
			return nil, fmt.Errorf("uncontrolledTimeout (%s) exceeds the %s maximum", u.Duration, MaxExplicitUncontrolledTimeout)
		}
	}
	if h := spec.HardStop.Duration; h > 0 {
		if h < MinHardStop {
			return nil, fmt.Errorf("hardStop (%s) must be at least %s", h, MinHardStop)
		}
		if h < uncontrolledTimeoutFor(spec) {
			return nil, fmt.Errorf("hardStop (%s) must not be shorter than the uncontrolled deadline (%s)", h, uncontrolledTimeoutFor(spec))
		}
	}

	if spec.HoldTimeout.Duration > spec.ProvisioningTimeout.Duration {
		return nil, fmt.Errorf("holdTimeout (%s) must not exceed provisioningTimeout (%s): a hold that can outlive the destructive provisioning deadline can never be satisfied by a successful wake",
			spec.HoldTimeout.Duration, spec.ProvisioningTimeout.Duration)
	}

	if spec.IdleTimeout.Duration <= 0 {
		return nil, fmt.Errorf("idleTimeout must be > 0: it is also the demand annotation's TTL, so a zero expires demand the instant the proxy writes it and the Model can never wake")
	}
	if spec.ProvisioningTimeout.Duration <= 0 {
		return nil, fmt.Errorf("provisioningTimeout must be > 0: it is the only bound on a run that never reaches Ready, and provisioningDue does nothing for a non-positive value")
	}

	var warnings []string
	// Replaces the per-backend warm-window arithmetic (D158, D164), which
	// measured a window that no longer exists. This warns in the direction
	// of the mistake actually made in production: a hold too short to
	// outlast a cold start answers 503 to EVERY cold request. The
	// comparison is against provisioningTimeout — the operator's own
	// statement of how long a wake may take — so it needs no knowledge of
	// backends. Half is a heuristic and the text says so: a measured 9m53s
	// cold start against a 30m provisioningTimeout sits just above the
	// line, and the 5m hold that would 503 everything sits well below it.
	if spec.HoldTimeout.Duration > 0 && spec.HoldTimeout.Duration*2 < spec.ProvisioningTimeout.Duration {
		warnings = append(warnings, fmt.Sprintf(
			"holdTimeout (%s) is less than half of provisioningTimeout (%s): there is no warm pool on any backend, so every wake is a full cold start and a hold this short will answer 503 to cold requests rather than serve them. This is a heuristic, not a rule — raise holdTimeout, or lower provisioningTimeout if your wakes really are that fast",
			spec.HoldTimeout.Duration, spec.ProvisioningTimeout.Duration))
	}
	if spec.HardStop.Duration == 0 && spec.MinReplicas == 0 {
		warnings = append(warnings, "hardStop is disabled: nothing would stop this Model's capacity if the controller died — and note hardStop does not currently fire on the Kubernetes backend either (D161), so enabling it is not by itself a dead-man's switch")
	}

	return warnings, nil
}
