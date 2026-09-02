// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"context"
	"fmt"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

// preflightClient is the slice of dstack.Client preflight needs. Narrow on
// purpose: it makes the fake in the test three lines instead of thirty.
type preflightClient interface {
	BackendConfigured(ctx context.Context, backend string) (bool, error)
	HasFleetFor(ctx context.Context, backend string) (bool, error)
	EnsureFleet(ctx context.Context, spec dstack.FleetSpec) error
}

// preflight answers "why can this Model not provision?" BEFORE spending a
// get_plan on it, and returns ("", "") when there is no such reason.
//
// It exists because all the interesting failures looked identical from
// outside: an unconfigured backend, a backend no fleet admits, and a market
// with nothing available ALL surface as zero offers and no error (D58, D67).
// That cost 25 minutes of bisecting resource filters that were never the
// problem, and it is the first thing a new user will hit.
//
// LIVE-7 / Branch B (ledger D83): dstack 0.21.2 never auto-creates a fleet
// for a run with none (docs/references/dstack-real-api.md §9.8), so a
// backend with zero admitting fleets used to be a dead end reported here and
// nowhere fixed — exactly what let the hand-declared vastai fleet's disappearance
// go unnoticed until a live run needed it. NoFleet detection is now
// NoFleet remediation: on the wake path, a backend with no admitting fleet
// gets one created (EnsureFleet, create-only, idempotent) before it is ever
// counted against the Model. Only a backend EnsureFleet itself could not fix
// still contributes to the reason below.
//
// It FAILS OPEN by construction: any error talking to dstack — including a
// failed EnsureFleet — yields no BLOCKING behaviour; it only downgrades that
// one backend to "unfleeted" so the Schedulable condition can say so. A
// diagnostic (and its remediation) must never be what stops a wake.
func preflight(ctx context.Context, c preflightClient, backends []string) (reason, message string, fleets []squallv1alpha1.FleetStatus) {
	if len(backends) == 0 {
		// "Any configured backend" — nothing to check against.
		return "", "", nil
	}
	fleets = make([]squallv1alpha1.FleetStatus, 0, len(backends))
	var unconfigured, unfleeted []string
	for _, b := range backends {
		ok, err := c.BackendConfigured(ctx, b)
		if err != nil {
			return "", "", nil
		}
		if !ok {
			unconfigured = append(unconfigured, b)
			fleets = append(fleets, squallv1alpha1.FleetStatus{Backend: b, State: squallv1alpha1.FleetStateUnconfigured})
			continue
		}
		hasFleet, err := c.HasFleetFor(ctx, b)
		if err != nil {
			return "", "", nil
		}
		if !hasFleet {
			// Remediate, don't just report: create the fleet this backend is
			// missing. Level-triggered and create-only (D83) — a fleet that
			// already exists by this name is left untouched, so replaying
			// this on every reconcile is safe.
			if err := c.EnsureFleet(ctx, dstack.FleetSpec{
				Name:     dstack.FleetName(b),
				Backends: []string{b},
			}); err != nil {
				unfleeted = append(unfleeted, b)
				fleets = append(fleets, squallv1alpha1.FleetStatus{Backend: b, Name: dstack.FleetName(b), State: squallv1alpha1.FleetStateUnfleeted})
			} else {
				fleets = append(fleets, squallv1alpha1.FleetStatus{Backend: b, Name: dstack.FleetName(b), State: squallv1alpha1.FleetStateCreated})
			}
		} else {
			// No Name here on purpose (D149): HasFleetFor only answers THAT
			// some active fleet admits the backend, not WHICH. Stamping
			// dstack.FleetName(b) reported "squall-auto-kubernetes" to an
			// operator whose own declared fleet was the one admitting — a
			// fleet dstack's API did not even list.
			fleets = append(fleets, squallv1alpha1.FleetStatus{Backend: b, State: squallv1alpha1.FleetStateAdmitting})
		}
	}
	switch {
	case len(unconfigured) == len(backends):
		return squallv1alpha1.ReasonBackendUnavailable, fmt.Sprintf(
			"no backend in spec.placement.backends %v is configured on the dstack server; "+
				"provisioning would return zero offers with no error", backends), fleets
	case len(unfleeted)+len(unconfigured) == len(backends):
		return squallv1alpha1.ReasonNoFleet, fmt.Sprintf(
			"no active fleet admits %v and squall could not create one; dstack needs a "+
				"fleet per backend and returns zero offers without one", unfleeted), fleets
	default:
		return "", "", fleets
	}
}
