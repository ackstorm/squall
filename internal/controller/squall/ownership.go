// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

// ModelUIDEnvKey carries the owning Model's Kubernetes UID on the dstack run
// itself.
//
// It has to live on the RUN, not in status: F1 made the run name
// "<namespace>-<name>", which is deliberately STABLE across a Model being
// deleted and recreated — and a recreated Model starts with an empty status,
// so nothing squall writes there can tell it that the run under its name was
// minted for a previous incarnation. dstack exposes no labels or tags, so the
// configuration env is the carrier. A UID is an identifier, not a secret.
const ModelUIDEnvKey = "SQUALL_MODEL_UID"

// runOwnership is what the recorded UID says about a run squall found under
// this Model's name.
type runOwnership int

const (
	// runOwnedByThisModel: the run carries this Model's UID.
	runOwnedByThisModel runOwnership = iota

	// runUnmarked: the run carries no UID at all. Every run minted before
	// this check existed looks like this, so it MUST mean "adopt and start
	// recording", never "refuse". D98 is what happens when an upgrade treats
	// its own absent bookkeeping as evidence against a live, paid GPU.
	runUnmarked

	// runFromAnotherIncarnation: the run carries a DIFFERENT UID. The Model
	// under this name was deleted and recreated while its run survived.
	runFromAnotherIncarnation
)

// classifyRunOwnership compares the UID a run carries against the Model's own.
func classifyRunOwnership(run *dstack.Run, model *squallv1alpha1.Model) runOwnership {
	if run == nil {
		return runOwnedByThisModel // nothing to disagree with.
	}
	recorded := run.Env[ModelUIDEnvKey]
	switch {
	case recorded == "":
		return runUnmarked
	case recorded == string(model.UID):
		return runOwnedByThisModel
	default:
		return runFromAnotherIncarnation
	}
}

// withModelUID returns env plus the owning Model's UID, without mutating the
// caller's map — resolveEnv's result is built per pass, but aliasing it here
// would make a future caching change silently leak one Model's UID into
// another's run.
func withModelUID(env map[string]string, model *squallv1alpha1.Model) map[string]string {
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	out[ModelUIDEnvKey] = string(model.UID)
	return out
}

// reconcileRunOwnership records who the observed run was minted for, and says
// so loudly when that is not this Model.
//
// It REPORTS and does not veto. Refusing to act on a run from a previous
// incarnation would strand the Model until a human intervened, and `0->1 fails
// open` is explicit that a wrong wake costs money while a refusal costs
// service. Adoption is also self-correcting: the next Apply sends this Model's
// current image, args and resources under dstack's CAS, so a run adopted with
// stale configuration converges rather than persisting.
func (r *ModelReconciler) reconcileRunOwnership(
	model *squallv1alpha1.Model, run *dstack.Run, runName string, logger logr.Logger,
) {
	switch classifyRunOwnership(run, model) {
	case runFromAnotherIncarnation:
		model.Status.RunUID = run.Env[ModelUIDEnvKey]
		logger.Error(nil, "dstack run under this Model's name was minted for a DIFFERENT Model incarnation; adopting it, but its configuration predates this object",
			"runName", runName, "runUID", model.Status.RunUID, "modelUID", string(model.UID))
		meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
			Type: squallv1alpha1.ConditionSchedulable, Status: metav1.ConditionTrue,
			Reason: squallv1alpha1.ReasonRunFromAnotherIncarnation,
			Message: fmt.Sprintf("run %q carries Model UID %s, this Model is %s; adopted, and re-applied on the next replica change",
				runName, model.Status.RunUID, model.UID),
		})
	case runUnmarked:
		// Minted before this check existed. Adopt and start recording — an
		// upgrade must never read its own absent bookkeeping as evidence
		// against a live, paid GPU (D98).
		model.Status.RunUID = string(model.UID)
	case runOwnedByThisModel:
		model.Status.RunUID = string(model.UID)
	}
}
