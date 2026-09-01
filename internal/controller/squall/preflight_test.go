// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"context"
	"errors"
	"reflect"
	"testing"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

type fakePreflight struct {
	configured map[string]bool
	fleets     map[string]bool
	err        error

	// ensureFleetErr, keyed by backend, lets a test make EnsureFleet fail for
	// one specific backend without affecting BackendConfigured/HasFleetFor.
	ensureFleetErr map[string]error
	// ensured records every backend EnsureFleet was actually called for, so
	// a test can assert it was (or was not) invoked.
	ensured []string
}

func (f *fakePreflight) BackendConfigured(_ context.Context, b string) (bool, error) {
	return f.configured[b], f.err
}
func (f *fakePreflight) HasFleetFor(_ context.Context, b string) (bool, error) {
	return f.fleets[b], f.err
}

func (f *fakePreflight) EnsureFleet(_ context.Context, spec dstack.FleetSpec) error {
	f.ensured = append(f.ensured, spec.Backends...)
	return f.ensureFleetErr[spec.Backends[0]]
}

// TestPreflight_NamesTheActualProblem. Before this, a backend that was not
// configured, a backend with no fleet, and a genuinely empty market were all
// the same observable: get_plan returns zero offers and no error (D58, D67).
// Telling them apart is the whole point — they call for opposite responses.
func TestPreflight_NamesTheActualProblem(t *testing.T) {
	tests := []struct {
		name       string
		backends   []string
		fake       *fakePreflight
		wantReason string
	}{
		{
			name:     "all good",
			backends: []string{"vastai"},
			fake: &fakePreflight{
				configured: map[string]bool{"vastai": true},
				fleets:     map[string]bool{"vastai": true},
			},
			wantReason: "",
		},
		{
			name:       "backend not configured on the server",
			backends:   []string{"vastai"},
			fake:       &fakePreflight{configured: map[string]bool{}},
			wantReason: squallv1alpha1.ReasonBackendUnavailable,
		},
		{
			// LIVE-7 / Branch B (D83): a configured backend with no admitting
			// fleet is REMEDIATED, not merely reported. EnsureFleet succeeds
			// here, so the Model is schedulable in the same reconcile.
			name:     "configured, no fleet, but squall creates one",
			backends: []string{"vastai"},
			fake: &fakePreflight{
				configured: map[string]bool{"vastai": true},
				fleets:     map[string]bool{},
			},
			wantReason: "",
		},
		{
			// The remaining failure mode: EnsureFleet itself cannot fix it
			// (e.g. dstack rejects the fleet create). Only then does the
			// backend still count as unfleeted.
			name:     "configured, no fleet, and squall cannot create one either",
			backends: []string{"vastai"},
			fake: &fakePreflight{
				configured:     map[string]bool{"vastai": true},
				fleets:         map[string]bool{},
				ensureFleetErr: map[string]error{"vastai": errors.New("dstack rejected fleet create")},
			},
			wantReason: squallv1alpha1.ReasonNoFleet,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg, _ := preflight(context.Background(), tc.fake, tc.backends)
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q (msg %q)", reason, tc.wantReason, msg)
			}
			if reason != "" && msg == "" {
				t.Fatal("a reason with no message tells an operator nothing")
			}
		})
	}
}

// TestPreflight_EnsureFleetIsCalledOnlyWhenNeeded. A backend that already has
// an admitting fleet must not trigger a redundant create call — EnsureFleet
// is create-only and idempotent server-side, but calling it unconditionally
// would still mean a wasted round trip on every single reconcile.
func TestPreflight_EnsureFleetIsCalledOnlyWhenNeeded(t *testing.T) {
	fake := &fakePreflight{
		configured: map[string]bool{"vastai": true},
		fleets:     map[string]bool{"vastai": true},
	}
	preflight(context.Background(), fake, []string{"vastai"})
	if len(fake.ensured) != 0 {
		t.Fatalf("EnsureFleet called %v, want none: a fleet already admits this backend", fake.ensured)
	}
}

// TestPreflight_FailsOpenWhenItCannotTell. 0->1 fails open: a preflight that
// could not run must not block a wake. Paying for a GPU a little longer is
// always preferable to refusing to serve because a diagnostic call failed.
func TestPreflight_FailsOpenWhenItCannotTell(t *testing.T) {
	f := &fakePreflight{err: errors.New("dstack unreachable")}
	if reason, _, _ := preflight(context.Background(), f, []string{"vastai"}); reason != "" {
		t.Fatalf("reason = %q, want none: an unreachable dstack is not proof of misconfiguration", reason)
	}
}

// TestPreflight_EmptyBackendListIsSchedulable: an empty spec.placement.backends
// means "any configured backend", which squall cannot pre-check.
func TestPreflight_EmptyBackendListIsSchedulable(t *testing.T) {
	if reason, _, _ := preflight(context.Background(), &fakePreflight{}, nil); reason != "" {
		t.Fatalf("reason = %q, want none for an unconstrained Model", reason)
	}
}

func TestPreflight_ReportsFleetStatePerBackend(t *testing.T) {
	c := &fakePreflight{
		configured:     map[string]bool{"vastai": true, "aws": true, "gcp": false},
		fleets:         map[string]bool{"vastai": true, "aws": false},
		ensureFleetErr: map[string]error{},
	}
	_, _, fleets := preflight(context.Background(), c, []string{"vastai", "aws", "gcp"})
	want := []squallv1alpha1.FleetStatus{
		{Backend: "vastai", Name: "squall-auto-vastai", State: squallv1alpha1.FleetStateAdmitting},
		{Backend: "aws", Name: "squall-auto-aws", State: squallv1alpha1.FleetStateCreated},
		{Backend: "gcp", State: squallv1alpha1.FleetStateUnconfigured},
	}
	if !reflect.DeepEqual(fleets, want) {
		t.Fatalf("fleet mirror:\n got %+v\nwant %+v", fleets, want)
	}
}

func TestPreflight_UnfleetedWhenCreationFails(t *testing.T) {
	c := &fakePreflight{
		configured:     map[string]bool{"vastai": true},
		fleets:         map[string]bool{"vastai": false},
		ensureFleetErr: map[string]error{"vastai": errors.New("dstack said no")},
	}
	_, _, fleets := preflight(context.Background(), c, []string{"vastai"})
	if len(fleets) != 1 || fleets[0].State != squallv1alpha1.FleetStateUnfleeted {
		t.Fatalf("a failed EnsureFleet must report Unfleeted, got %+v", fleets)
	}
}

func TestPreflight_DstackErrorPublishesNoMirror(t *testing.T) {
	c := &fakePreflight{err: errors.New("connection refused")}
	reason, _, fleets := preflight(context.Background(), c, []string{"vastai"})
	if reason != "" || fleets != nil {
		t.Fatalf("a dstack error must yield no reason and no mirror, got %q / %+v", reason, fleets)
	}
}
