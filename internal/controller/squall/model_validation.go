// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"fmt"
	"time"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// Validate enforces the Model cross-field rules spec §5.1 lists under
// "Validation (webhook/CEL), enforced not documented" that OpenAPI schema
// cannot express structurally — deadline ordering, the F21 fleet idle
// trap, and the §12.3 backends allowlist. Structural rules (the engine/
// minReplicas enums, placement.backends' MinItems=1, the image digest
// pattern) are already enforced by the CRD schema generated from
// api/squall/v1alpha1/model_types.go and are deliberately NOT duplicated
// here.
func Validate(spec squallv1alpha1.ModelSpec) error {
	_, err := ValidateWithWarnings(spec)
	return err
}

// ValidateWithWarnings runs the same rejection rules as Validate, plus the
// §5.1 warm-window rule, which is advisory: a holdTimeout that habitually
// waits out a full cold start is "a misconfiguration in all but intent" —
// legal, but worth surfacing.
func ValidateWithWarnings(spec squallv1alpha1.ModelSpec) ([]string, error) {
	if len(spec.Placement.Backends) == 0 {
		return nil, fmt.Errorf("placement.backends must not be empty: it enforces the §12.3 workload-eligibility allowlist, not merely documents it")
	}

	if spec.Fleet.IdleDuration.Duration <= 0 {
		return nil, fmt.Errorf("fleet.idleDuration is required and must be > 0: dstack's own default is 3 days (F21), so leaving it unset is not a safe fallback")
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

	var warnings []string
	warmWindow := time.Duration(spec.ScaleDownDelaySeconds)*time.Second + spec.Fleet.IdleDuration.Duration
	if spec.HoldTimeout.Duration > 0 && spec.HoldTimeout.Duration > warmWindow {
		warnings = append(warnings, fmt.Sprintf(
			"holdTimeout (%s) exceeds the warm window (%s = scaleDownDelaySeconds + fleet.idleDuration): most wakes will pay a full cold start, which is a misconfiguration in all but intent",
			spec.HoldTimeout.Duration, warmWindow))
	}
	if spec.HardStop.Duration == 0 && spec.MinReplicas == 0 {
		warnings = append(warnings, "hardStop is disabled: nothing will stop this Model's capacity if the controller dies")
	}

	return warnings, nil
}
