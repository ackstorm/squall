// SPDX-License-Identifier: MIT

package dstack

import "testing"

func sub(status string, dep int, streaks ...int) jobSubmissionWire {
	probes := make([]probeWire, 0, len(streaks))
	for _, s := range streaks {
		probes = append(probes, probeWire{SuccessStreak: s})
	}
	return jobSubmissionWire{Status: status, DeploymentNum: dep, Probes: probes}
}

// TestProbesReady is §6 evidence (a), derived. dstack's Probe model is
// literally `{success_streak: int}` (measured, 0.21.2) — there is no ready
// flag on the wire, so the client computes readiness from the streak and
// the ready_after WE submitted.
//
// The governing invariant decides every ambiguous row: absence is never
// readiness. A model wrongly held un-Ready costs a held request; a model
// wrongly called Ready sends a user's tokens into a cold engine and gets
// the model evicted from LiteLLM's fallback chain.
func TestProbesReady(t *testing.T) {
	tests := []struct {
		name       string
		jobs       []jobWire
		dep        int
		readyAfter int
		want       bool
	}{
		{"no jobs at all is not ready", nil, 0, 2, false},
		{"a live submission with no probes is NOT ready",
			[]jobWire{{JobSubmissions: []jobSubmissionWire{sub("running", 0)}}}, 0, 2, false},
		{"streak below ready_after is not ready",
			[]jobWire{{JobSubmissions: []jobSubmissionWire{sub("running", 0, 1)}}}, 0, 2, false},
		{"streak equal to ready_after is ready",
			[]jobWire{{JobSubmissions: []jobSubmissionWire{sub("running", 0, 2)}}}, 0, 2, true},
		{"every probe must pass, not just one",
			[]jobWire{{JobSubmissions: []jobSubmissionWire{sub("running", 0, 5, 1)}}}, 0, 2, false},
		{"every live replica must pass, not just one",
			[]jobWire{
				{JobSubmissions: []jobSubmissionWire{sub("running", 0, 5)}},
				{JobSubmissions: []jobSubmissionWire{sub("running", 0, 0)}},
			}, 0, 2, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := probesReady(tc.jobs, tc.dep, tc.readyAfter); got != tc.want {
				t.Fatalf("probesReady(dep=%d, readyAfter=%d) = %v, want %v", tc.dep, tc.readyAfter, got, tc.want)
			}
		})
	}
}

// TestProbesReady_IgnoresPreviousDeployments is D46, measured on a real
// server: after an in-place flip (F17) the PREVIOUS deployment's replica
// stays in `jobs` forever — terminated, with `probes: []`. Counting it
// makes readiness unreachable, which is D28 all over again.
func TestProbesReady_IgnoresPreviousDeployments(t *testing.T) {
	jobs := []jobWire{
		{JobSubmissions: []jobSubmissionWire{sub("terminated", 0)}},  // the old replica
		{JobSubmissions: []jobSubmissionWire{sub("running", 1, 20)}}, // the live one
	}

	if !probesReady(jobs, 1, 2) {
		t.Fatal("probesReady = false: a terminated replica from deployment 0 was counted against deployment 1 (D46)")
	}
}

// TestProbesReady_IgnoresPreviousDeployments_EvenWhenNotFinished isolates
// the deploymentNum filter from the finished-status filter: D46's own
// measurement always shows the old replica as "terminated", so a test built
// only from that measurement leaves the deploymentNum check unfalsified —
// dropping it entirely still passes as long as the finished-status filter
// stays, because "terminated" alone already excludes it. This pins the
// filter's OWN job: an old deployment's submission that is somehow still
// non-finished must NOT be read as live evidence against the CURRENT
// deployment's readiness either way.
func TestProbesReady_IgnoresPreviousDeployments_EvenWhenNotFinished(t *testing.T) {
	jobs := []jobWire{
		{JobSubmissions: []jobSubmissionWire{sub("running", 0)}},    // old deployment, not finished, no probes
		{JobSubmissions: []jobSubmissionWire{sub("running", 1, 2)}}, // current deployment, ready
	}

	if !probesReady(jobs, 1, 2) {
		t.Fatal("probesReady = false: an old deployment's non-finished, probe-less submission was counted against the CURRENT deployment's readiness (deploymentNum filter must run independently of the finished-status filter)")
	}
}

// TestProbesReady_IgnoresFinishedSubmissionsOfTheCurrentDeployment: a
// replica of the CURRENT deployment that has died is not evidence either
// way, but a run with no live submission at all is never ready.
func TestProbesReady_IgnoresFinishedSubmissionsOfTheCurrentDeployment(t *testing.T) {
	jobs := []jobWire{{JobSubmissions: []jobSubmissionWire{sub("failed", 1, 9)}}}

	if probesReady(jobs, 1, 2) {
		t.Fatal("probesReady = true from a FAILED submission's stale streak; only live submissions count")
	}
}

func TestReplicaPricePerHour_UsesOnlyLiveCurrentDeployment(t *testing.T) {
	tests := []struct {
		name string
		jobs []jobWire
		want float64
	}{
		{
			name: "current live price",
			jobs: []jobWire{{JobSubmissions: []jobSubmissionWire{{DeploymentNum: 2, Status: "running", JobProvisioningData: &jobProvisioningDataWire{Price: 1.89445}}}}},
			want: 1.89445,
		},
		{
			name: "old deployment ignored",
			jobs: []jobWire{{JobSubmissions: []jobSubmissionWire{{DeploymentNum: 1, Status: "running", JobProvisioningData: &jobProvisioningDataWire{Price: 2}}}}},
		},
		{
			name: "finished submission ignored",
			jobs: []jobWire{{JobSubmissions: []jobSubmissionWire{{DeploymentNum: 2, Status: "terminated", JobProvisioningData: &jobProvisioningDataWire{Price: 2}}}}},
		},
		{
			name: "missing or zero price is absent",
			jobs: []jobWire{{JobSubmissions: []jobSubmissionWire{{DeploymentNum: 2, Status: "running"}, {DeploymentNum: 2, Status: "running", JobProvisioningData: &jobProvisioningDataWire{Price: 0}}}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := replicaPricePerHour(tt.jobs, 2); got != tt.want {
				t.Fatalf("replicaPricePerHour = %v, want %v", got, tt.want)
			}
		})
	}
}
