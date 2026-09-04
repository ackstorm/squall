// SPDX-License-Identifier: MIT

package squall

import (
	"fmt"
	"strings"
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
	scaleDown := time.Duration(spec.ScaleDownDelaySeconds) * time.Second
	warmWindow, formula := scaleDown, "scaleDownDelaySeconds"
	if backendsHoldAWarmPool(spec.Placement.Backends) {
		warmWindow += spec.Fleet.IdleDuration.Duration
		formula = "scaleDownDelaySeconds + fleet.idleDuration"
	}
	if spec.HoldTimeout.Duration > 0 && spec.HoldTimeout.Duration > warmWindow {
		warnings = append(warnings, fmt.Sprintf(
			"holdTimeout (%s) exceeds the warm window (%s = %s): most wakes will pay a full cold start, which is a misconfiguration in all but intent",
			spec.HoldTimeout.Duration, warmWindow, formula))
	}
	if spec.HardStop.Duration == 0 && spec.MinReplicas == 0 {
		warnings = append(warnings, "hardStop is disabled: nothing would stop this Model's capacity if the controller died — and note hardStop does not currently fire on the Kubernetes backend either (D161), so enabling it is not by itself a dead-man's switch")
	}

	return warnings, nil
}

// nonDockerizedBackends are the dstack backends whose provisioning data
// carries dockerized: false. That flag is not a detail: dstack forces
// termination_idle_time to 0 and never reads the profile's idle_duration
// unless it is true (D158, and docs/references/dstack-real-api.md §9.9),
// so on these backends a fleet keeps no warm instance and every wake is a
// full cold provision. Source-verified against dstack a70d98b, then
// measured on both Vast.ai and Kubernetes.
var nonDockerizedBackends = map[string]struct{}{
	"vastai":     {},
	"kubernetes": {},
	"runpod":     {},
}

// backendsHoldAWarmPool reports whether fleet.idleDuration can contribute
// anything to the warm window for this placement.
//
// Deliberately pessimistic: ONE non-dockerized backend in the allowlist is
// enough to answer false, because dstack is free to place the wake there
// and that wake would be cold. Counting idleDuration anyway is how the
// warning came to under-report the exact case it exists for — a live
// Vast.ai Model was told its warm window was 15m when it was 5m, and its
// demand anchor then expired 14 minutes before the GPU it was holding
// open finished provisioning.
func backendsHoldAWarmPool(backends []string) bool {
	if len(backends) == 0 {
		return false
	}
	for _, b := range backends {
		if _, cold := nonDockerizedBackends[strings.ToLower(strings.TrimSpace(b))]; cold {
			return false
		}
	}
	return true
}
