// SPDX-License-Identifier: Apache-2.0

package squall

import (
	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// dstackRunName is F1's run identity: a dstack run name is global to the
// dstack server, but a Model is namespaced. Keying the run on the bare
// Model name therefore let `a/qwen` and `b/qwen` drive ONE run — each
// controller pass fighting the other over replica count, and either
// Model's deletion tearing down the other's GPU.
func dstackRunName(m *squallv1alpha1.Model) string {
	if m.Namespace == "" {
		return m.Name
	}
	return m.Namespace + "-" + m.Name
}

// runNameFor returns the name this Model's dstack run is filed under:
// whatever status recorded, else the namespace-qualified name.
//
// status.runName is READ rather than always recomputed because a run name
// is a foreign key held by dstack, and dstack is where the money is. D98:
// changing how the name is computed, with live runs filed under the old
// answer, made every one of them simultaneously invisible to observe() and
// unowned by the reaper — a working replica stopped mid-generation while a
// duplicate was provisioned to replace it. Recording the name means a
// future change to dstackRunName cannot repeat that: existing runs keep
// answering to what status already says.
func runNameFor(m *squallv1alpha1.Model) string {
	if m.Status.RunName != "" {
		return m.Status.RunName
	}
	return dstackRunName(m)
}

// runNamesFor is every name a Model could legitimately have its run filed
// under. The orphan reaper uses it rather than a single name because
// reaping is destructive and 1->0 fails safe: a Model reconciled before it
// could persist status.runName still owns the name it would compute, and a
// sweep landing in that window must not stop a live replica.
func runNamesFor(m *squallv1alpha1.Model) []string {
	names := []string{dstackRunName(m)}
	if m.Status.RunName != "" && m.Status.RunName != names[0] {
		names = append(names, m.Status.RunName)
	}
	return names
}
