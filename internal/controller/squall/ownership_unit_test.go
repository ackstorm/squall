// SPDX-License-Identifier: MIT

package squall

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

func ownershipModel(uid string) *squallv1alpha1.Model {
	return &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Namespace: "squall", Name: "qwen", UID: types.UID(uid)},
	}
}

// TestClassifyRunOwnership is F1's second half. The run name is stable across
// a Model being deleted and recreated, so the UID stamped on the RUN is the
// only thing that can tell "my run" from "a run left by a previous Model of
// the same name".
func TestClassifyRunOwnership(t *testing.T) {
	tests := []struct {
		name string
		run  *dstack.Run
		uid  string
		want runOwnership
	}{
		{
			name: "no run at all -> nothing to disagree with",
			run:  nil,
			uid:  "uid-a",
			want: runOwnedByThisModel,
		},
		{
			name: "run carries this Model's UID",
			run:  &dstack.Run{Env: map[string]string{ModelUIDEnvKey: "uid-a"}},
			uid:  "uid-a",
			want: runOwnedByThisModel,
		},
		{
			// THE UPGRADE CASE, and the one D98 is about: every run minted
			// before this check existed has no UID. Reading that as "not
			// mine" would make an upgrade disown every live, paid GPU.
			name: "run predates the check -> unmarked, adopt it",
			run:  &dstack.Run{Env: nil},
			uid:  "uid-a",
			want: runUnmarked,
		},
		{
			name: "run has env but no UID key -> still unmarked",
			run:  &dstack.Run{Env: map[string]string{"VLLM_LOGGING_LEVEL": "INFO"}},
			uid:  "uid-a",
			want: runUnmarked,
		},
		{
			// The finding's actual scenario: Model deleted and recreated
			// under the same namespace/name while its run survived.
			name: "run carries a different UID -> another incarnation",
			run:  &dstack.Run{Env: map[string]string{ModelUIDEnvKey: "uid-old"}},
			uid:  "uid-new",
			want: runFromAnotherIncarnation,
		},
		{
			// A Model with no UID yet (never persisted) must not be read as
			// matching an empty recorded value into "owned".
			name: "unmarked run and an unset Model UID -> unmarked, not owned",
			run:  &dstack.Run{Env: map[string]string{}},
			uid:  "",
			want: runUnmarked,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyRunOwnership(tt.run, ownershipModel(tt.uid)); got != tt.want {
				t.Errorf("classifyRunOwnership = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestWithModelUID: the stamp must be added without mutating the caller's map,
// and must never overwrite a resolved value silently other than its own key.
func TestWithModelUID(t *testing.T) {
	env := map[string]string{"VLLM_LOGGING_LEVEL": "INFO"}
	got := withModelUID(env, ownershipModel("uid-a"))

	if got[ModelUIDEnvKey] != "uid-a" {
		t.Errorf("%s = %q, want %q", ModelUIDEnvKey, got[ModelUIDEnvKey], "uid-a")
	}
	if got["VLLM_LOGGING_LEVEL"] != "INFO" {
		t.Errorf("resolved env was dropped: %v", got)
	}
	if _, leaked := env[ModelUIDEnvKey]; leaked {
		t.Error("withModelUID mutated the caller's map; a cached env would leak one Model's UID into another's run")
	}
}
