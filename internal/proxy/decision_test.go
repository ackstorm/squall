// SPDX-License-Identifier: MIT

package proxy

import (
	"testing"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// TestDecide is spec v0.17-RC §7's decision table, transcribed verbatim:
// one test case per row, no exceptions (task 9.1).
func TestDecide(t *testing.T) {
	tests := []struct {
		name string

		phase       squallv1alpha1.ModelPhase
		hasCR       bool
		gatewayCode GatewayCode

		want Action
	}{
		{
			name:  "Ready -> forward",
			phase: squallv1alpha1.ModelPhaseReady,
			hasCR: true,
			want:  Action{Forward: true},
		},
		{
			name:  "Asleep -> demand patch, block, 503 asleep on deadline",
			phase: squallv1alpha1.ModelPhaseAsleep,
			hasCR: true,
			want: Action{
				DemandPatch: true, Block: true,
				DeadlineStatus: 503, DeadlineState: WaitAsleep,
			},
		},
		{
			name:  "Waking -> block (demand coalesced), 503 waking on deadline",
			phase: squallv1alpha1.ModelPhaseWaking,
			hasCR: true,
			want: Action{
				DemandPatch: true, Block: true,
				DeadlineStatus: 503, DeadlineState: WaitWaking,
			},
		},
		{
			name:  "Recreating -> block (demand coalesced), 503 recreating on deadline",
			phase: squallv1alpha1.ModelPhaseRecreating,
			hasCR: true,
			want: Action{
				DemandPatch: true, Block: true,
				DeadlineStatus: 503, DeadlineState: WaitRecreating,
			},
		},
		{
			// Dead: full cold-start deadline expectations ("recreating"),
			// distinct from Asleep, even though both block and demand-patch.
			name:  "Dead -> demand patch (recreate), block, 503 recreating on deadline",
			phase: squallv1alpha1.ModelPhaseDead,
			hasCR: true,
			want: Action{
				DemandPatch: true, Block: true,
				DeadlineStatus: 503, DeadlineState: WaitRecreating,
			},
		},
		{
			// Draining: new requests NEVER block — 404 immediately, not
			// held against capacity being torn down.
			name:  "Draining -> 404 immediately, never blocks",
			phase: squallv1alpha1.ModelPhaseDraining,
			hasCR: true,
			want:  Action{ImmediateStatus: 404},
		},
		{
			name:  "no CR -> 404, phase and gatewayCode irrelevant",
			hasCR: false,
			want:  Action{ImmediateStatus: 404},
		},
		{
			// Internal (F23): a gateway auth fault observed while
			// attempting to forward a Ready model is never a wake signal.
			name:        "Ready + gateway 403 -> alarm, 502 auth fault, never forward",
			phase:       squallv1alpha1.ModelPhaseReady,
			hasCR:       true,
			gatewayCode: 403,
			want:        Action{Alarm: true, ImmediateStatus: 502},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.phase, tt.hasCR, tt.gatewayCode)
			if got != tt.want {
				t.Errorf("Decide(%v, %v, %v) = %+v, want %+v", tt.phase, tt.hasCR, tt.gatewayCode, got, tt.want)
			}
		})
	}
}

// TestDecide_UnknownPhase_FailsToward404 covers a phase value outside the
// six ModelPhase constants (e.g. a future spec revision, or a decode bug
// upstream): it must never be read as Ready and forwarded blindly.
func TestDecide_UnknownPhase_FailsToward404(t *testing.T) {
	got := Decide(squallv1alpha1.ModelPhase("bogus"), true, 0)
	if got.Forward {
		t.Fatalf("Decide(bogus phase) forwarded, want fail-toward-404: %+v", got)
	}
	if got.ImmediateStatus != 404 {
		t.Errorf("ImmediateStatus = %d, want 404", got.ImmediateStatus)
	}
}
