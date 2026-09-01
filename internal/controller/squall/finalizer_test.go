// SPDX-License-Identifier: Apache-2.0

package squall

import "testing"

func TestDrainEvidenceClean_NoKeyAnywhereIsCleanDrain(t *testing.T) {
	if !drainEvidenceClean(&ActivityEvidence{Complete: true, AllIdle: true}) {
		t.Fatal("complete no-key evidence should permit clean drain")
	}
}

func TestDrainEvidenceClean_SilentReplicaBlocksTeardown(t *testing.T) {
	if drainEvidenceClean(&ActivityEvidence{Complete: false}) || drainEvidenceClean(nil) ||
		drainEvidenceClean(&ActivityEvidence{Complete: true, AnyData: true, AllIdle: false}) {
		t.Fatal("incomplete or busy evidence must block teardown")
	}
}
