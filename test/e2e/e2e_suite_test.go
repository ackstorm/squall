//go:build e2e

// SPDX-License-Identifier: MIT

package e2e

import (
	"fmt"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ackstorm/squall/test/utils"
)

// TestE2E runs the squall end-to-end suite (Task 11.3) against an already
// hydrated kind cluster. Unlike the stock kubebuilder scaffold this
// replaced, this suite does NOT build images, install CRDs, or deploy the
// controller itself — that lifecycle belongs to hack/cluster.sh (Task
// 11.1), driven via `make cluster-up` / `make e2e-full`. Keeping the two
// concerns apart means `make e2e-run` can be re-run against a cluster
// left up from a previous `make cluster-up`, which is the whole point of
// `make cluster-status` existing as its own target.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting squall e2e suite\n")
	RunSpecs(t, "squall e2e suite")
}

var _ = BeforeSuite(func() {
	By("checking the cluster was hydrated (hack/cluster.sh hydrate / make cluster-up)")
	for _, dep := range []struct{ ns, name string }{
		// D116: this said "fake-dstack" long after 3d405d5 replaced that
		// Deployment with model-mock — so the whole suite aborted at
		// BeforeSuite and ran ZERO specs, while looking like a test run.
		{workloadNamespace, "model-mock"},
		{controlNamespace, "squall-operator"},
		{controlNamespace, "squall-proxy"},
	} {
		cmd := exec.Command("kubectl", "-n", dep.ns, "get", "deployment", dep.name)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(),
			fmt.Sprintf("deployment %s/%s not found — run `make cluster-up` first", dep.ns, dep.name))
	}
})
