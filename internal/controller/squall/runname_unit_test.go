// SPDX-License-Identifier: MIT

package squall

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

const (
	testModelName = "qwen"
	testRunName   = "squall-" + testModelName
)

func runNameModel(ns string) *squallv1alpha1.Model {
	return &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: testModelName},
	}
}

// TestDstackRunName is F1 itself: a dstack run name is global to the dstack
// server while a Model is namespaced, so the run identity must carry the
// namespace or two teams silently share one GPU.
func TestDstackRunName(t *testing.T) {
	if got := dstackRunName(runNameModel("squall")); got != testRunName {
		t.Errorf("dstackRunName = %q, want %q", got, testRunName)
	}
	// The collision F1 exists to kill: same name, different namespaces.
	if a, b := dstackRunName(runNameModel("squall")), dstackRunName(runNameModel("team-b")); a == b {
		t.Errorf("two namespaces produced the same run name %q: F1 is not fixed", a)
	}
}

// TestRunNameFor covers the recorded-identity rule: status wins over
// recomputation, because the name is a foreign key dstack holds and D98 is
// what happens when a live run's name is recomputed out from under it.
func TestRunNameFor(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   string
	}{
		{name: "nothing recorded -> the namespace-qualified name", status: "", want: testRunName},
		{
			// The D98 guarantee. If dstackRunName ever changes again, a run
			// already filed under the old answer must keep answering to it
			// rather than becoming invisible AND unowned in one step.
			name:   "status pins the name, even when it disagrees with the computed one",
			status: "some-older-name",
			want:   "some-older-name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := runNameModel("squall")
			m.Status.RunName = tt.status
			if got := runNameFor(m); got != tt.want {
				t.Errorf("runNameFor = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRunNamesFor guards the reaper's ownership set. Reaping is
// destructive, so the set must cover every name this Model's run could be
// filed under — and nothing else, or the reaper never collects an orphan.
func TestRunNamesFor(t *testing.T) {
	owns := func(m *squallv1alpha1.Model, name string) bool {
		for _, n := range runNamesFor(m) {
			if n == name {
				return true
			}
		}
		return false
	}

	t.Run("before status is persisted, the computed name is owned", func(t *testing.T) {
		m := runNameModel("squall")
		if !owns(m, testRunName) {
			t.Errorf("runNamesFor = %v, does not own its own computed name: the reaper would stop a live run", runNamesFor(m))
		}
	})

	t.Run("a recorded name that differs from the computed one is also owned", func(t *testing.T) {
		m := runNameModel("squall")
		m.Status.RunName = "some-older-name"
		if !owns(m, "some-older-name") {
			t.Errorf("runNamesFor = %v, does not own the name status recorded", runNamesFor(m))
		}
		if !owns(m, testRunName) {
			t.Errorf("runNamesFor = %v, dropped the computed name", runNamesFor(m))
		}
	})

	t.Run("no duplicate when status matches the computed name", func(t *testing.T) {
		m := runNameModel("squall")
		m.Status.RunName = testRunName
		if got := runNamesFor(m); len(got) != 1 {
			t.Errorf("runNamesFor = %v, want exactly one name", got)
		}
	})

	t.Run("does not claim an unrelated run", func(t *testing.T) {
		m := runNameModel("squall")
		m.Status.RunName = testRunName
		// Notably including the pre-F1 bare name: the adoption shim is gone
		// (owner's call 2026-08-31, verified 0 Vast instances and every
		// qwen3-8-27b run terminal), so a legacy-named run is now an orphan
		// and the reaper is SUPPOSED to collect it.
		for _, other := range []string{testModelName, "someone-elses-run", "team-b-" + testModelName} {
			if owns(m, other) {
				t.Errorf("runNamesFor = %v claims %q: the reaper would never collect that orphan", runNamesFor(m), other)
			}
		}
	})
}
