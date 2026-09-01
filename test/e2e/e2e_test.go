//go:build e2e

// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ackstorm/squall/test/utils"
)

// workloadNamespace holds model-mock and every test Model CR
// (test/e2e/cluster/00-namespaces, 01-model-mock). controlNamespace holds
// the operator — squall-operator and squall-proxy
// (config/default's namespace transform via test/e2e/cluster/02-operator).
const (
	workloadNamespace = "squall"
	controlNamespace  = "squall-system"
	loopModelName     = "e2e-loop-model"

	// fixtureModelName is test/e2e/cluster/03-fixtures/model.yaml's static
	// fixture — applied once at `cluster-hydrate` and, per that file's own
	// comment, untouched by every other spec in this suite. That makes it
	// the safe target for the forwarding spec below, which must hijack its
	// status.phase directly (see that Describe block's comment) without
	// disturbing loopModelName's own lifecycle assertions.
	fixtureModelName = "e2e-fixture-model"

	// controllerDeploymentName is config/default's manager Deployment after
	// its namePrefix transform ("squall-" + "controller-manager").
	controllerDeploymentName = "squall-operator"
)

// loopModelYAML is deliberately NOT test/e2e/cluster/03-fixtures/model.yaml
// — that fixture is documented (in its own comment) as existing only for
// manual `make cluster-status` exploration, precisely so this suite's
// timing assertions start from a clock it controls rather than from
// whenever `cluster-hydrate` last ran. Timers here are compressed further
// still (single-digit seconds throughout) to keep the full
// wake -> idle -> sleep -> fleet-idle-window loop inside about 20s.
const loopModelYAML = `
apiVersion: squall.ackstorm.ai/v1alpha1
kind: Model
metadata:
  name: ` + loopModelName + `
  namespace: ` + workloadNamespace + `
spec:
  engine: ollama
  features:
    - TextGeneration
  image: ghcr.io/nginxinc/nginx-unprivileged@sha256:7051ba7b4b6575ac1b2ac7f14e58afa4646ae02bda31fb04225a312d08f7bef1
  args: []
  env: {}
  resources:
    gpu:
      name: [A10G]
      memory: 24GB..32GB
  placement:
    backends: [kubernetes]
    regions: []
    maxPricePerHour: "0.80"
  minReplicas: 0
  holdTimeout: 5s
  scaleDownDelaySeconds: 2
  fleet:
    idleDuration: 4s
  drainTimeout: 10s
  provisioningTimeout: 5m
  maxLifetime: 168h
`

// modelStatus mirrors the subset of squallv1alpha1.ModelStatus this suite
// asserts on, decoded from `kubectl get -o json` rather than pulling in a
// controller-runtime client — this package talks to the cluster entirely
// through kubectl/exec, matching the convention the removed kubebuilder
// scaffold already established here.
type modelStatus struct {
	Phase         string `json:"phase"`
	RunID         string `json:"runId"`
	DeploymentNum int    `json:"deploymentNum"`
}

func getModelStatus(g Gomega, name string) modelStatus {
	out, err := utils.Run(exec.Command("kubectl", "-n", workloadNamespace,
		"get", "model", name, "-o", "jsonpath={.status}"))
	g.Expect(err).NotTo(HaveOccurred())
	var st modelStatus
	if strings.TrimSpace(out) == "" {
		return st // no status written yet
	}
	g.Expect(json.Unmarshal([]byte(out), &st)).To(Succeed())
	return st
}

// sendChatRequests fires n concurrent OpenAI-shaped requests at squall-proxy
// for model and waits for all of them to finish. The model never reaches
// Ready in this suite — there is no real engine behind model-mock, and §8's
// readiness probe is out of scope (see internal/controller/squall/phase.go's
// Observed.Ready doc comment) — so every call is expected to either receive
// squall-proxy's §7 wait-contract response or time out; only each call's
// side effects matter here: an immediate demand signal (Await ticks once up
// front, see internal/proxy/hold.go), and, once the call completes, an
// activity-tracker entry recording this Model idle. n > 1 is unused by the
// current suite (the e2e overlay pins squall-proxy to a single replica —
// see 02-operator/proxy-patch.yaml — precisely so one call is always
// enough) but is kept as a parameter rather than hardcoding 1, since the
// concurrency itself is trivial and a future multi-replica scenario would
// otherwise have to re-derive it.
func sendChatRequests(addr, model string, n int) {
	client := &http.Client{Timeout: 20 * time.Second}
	body, _ := json.Marshal(map[string]string{"model": model})

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions", bytes.NewReader(body))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}

// startPortForward tunnels a local port to target (e.g. "svc/proxy") in ns
// through the API server (kubectl port-forward), so this out-of-cluster Go
// test binary can reach an in-cluster Service the same way LiteLLM would in
// production. Returns the running command (the caller must kill it) and the
// local address to dial.
func startPortForward(ns, target string, remotePort int) (*exec.Cmd, string, error) {
	const localPort = 18080 // fixed: nothing else in this suite binds it
	cmd := exec.Command("kubectl", "-n", ns, "port-forward", target,
		fmt.Sprintf("%d:%d", localPort, remotePort))
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	// kubectl port-forward prints "Forwarding from ..." once ready, but
	// capturing that reliably needs pipe plumbing this suite doesn't
	// otherwise need — a short fixed wait is simpler and just as safe:
	// worst case the very first dial fails and the caller's Eventually
	// (or, here, sendChatRequests's own retry-free single attempt inside
	// a test already wrapped in Eventually) covers it.
	time.Sleep(2 * time.Second)
	return cmd, fmt.Sprintf("127.0.0.1:%d", localPort), nil
}

// chatCompletionResponse mirrors the subset of cmd/model-mock's response
// shape this suite asserts on: a well-formed OpenAI chat-completion body
// with the model name echoed back.
type chatCompletionResponse struct {
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// postChatCompletion sends one real chat-completion POST through
// squall-proxy at addr and decodes the response — used by the forwarding
// spec below to assert on the body squall-proxy's reverse proxy relayed
// back from the model-mock backend, not just a status code.
func postChatCompletion(addr, model string) (int, chatCompletionResponse, error) {
	body, _ := json.Marshal(map[string]string{"model": model})
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, chatCompletionResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, chatCompletionResponse{}, err
	}
	defer resp.Body.Close()

	var parsed chatCompletionResponse
	if resp.StatusCode == http.StatusOK {
		if decErr := json.NewDecoder(resp.Body).Decode(&parsed); decErr != nil {
			return resp.StatusCode, chatCompletionResponse{}, decErr
		}
	}
	return resp.StatusCode, parsed, nil
}

var _ = Describe("Model lifecycle (Task 11.3)", Ordered, func() {
	var portForward *exec.Cmd
	var proxyAddr string
	var wakeDeploymentNum int
	var wakeRunID string

	BeforeAll(func() {
		By("applying the loop test Model")
		apply := exec.Command("kubectl", "apply", "-f", "-")
		apply.Stdin = strings.NewReader(loopModelYAML)
		_, err := utils.Run(apply)
		Expect(err).NotTo(HaveOccurred(), "failed to apply the loop test Model")

		By("port-forwarding the proxy Service")
		pf, addr, err := startPortForward(controlNamespace, "svc/squall-proxy", 8080)
		Expect(err).NotTo(HaveOccurred(), "failed to port-forward svc/squall-proxy")
		portForward, proxyAddr = pf, addr
	})

	AfterAll(func() {
		if portForward != nil && portForward.Process != nil {
			_ = portForward.Process.Kill()
		}
		_, _ = utils.Run(exec.Command("kubectl", "-n", workloadNamespace,
			"delete", "model", loopModelName, "--ignore-not-found", "--wait=false"))
	})

	It("starts Asleep with no demand", func() {
		Eventually(func(g Gomega) string {
			return getModelStatus(g, loopModelName).Phase
		}, 30*time.Second, time.Second).Should(Equal("Asleep"))
	})

	It("wakes on a real proxy request and reaches Waking with a live run", func() {
		go sendChatRequests(proxyAddr, loopModelName, 1)

		Eventually(func(g Gomega) string {
			return getModelStatus(g, loopModelName).Phase
		}, 15*time.Second, time.Second).Should(Equal("Waking"))

		st := getModelStatus(Default, loopModelName)
		Expect(st.RunID).NotTo(BeEmpty(), "Waking must carry a dstack run id")
		wakeDeploymentNum, wakeRunID = st.DeploymentNum, st.RunID
	})

	It("flips back to Asleep once demand and proxy activity go idle", func() {
		By("driving one more request through so the activity tracker records this Model idle")
		sendChatRequests(proxyAddr, loopModelName, 1)

		// scaleDownDelaySeconds: 2s in loopModelYAML — give the
		// reconciler (SQUALL_IDLE_REQUEUE_INTERVAL, see
		// 02-operator/controller-patch.yaml) comfortably longer than
		// that to notice.
		Eventually(func(g Gomega) string {
			return getModelStatus(g, loopModelName).Phase
		}, 30*time.Second, time.Second).Should(Equal("Asleep"))
	})

	It("keeps the same run across the sleep flip (F20: flip is not recreate)", func() {
		st := getModelStatus(Default, loopModelName)
		Expect(st.RunID).To(Equal(wakeRunID),
			"sleeping must reuse the run id — a new one would mean F20's flip/recreate distinction broke")
		Expect(st.DeploymentNum).To(BeNumerically(">", wakeDeploymentNum),
			"the sleep flip's dstack Apply must CAS forward from the observed deploymentNum")
	})

	It("stays Asleep on the same run past the fleet's idle window", func() {
		// fleet.idleDuration: 4s in loopModelYAML. fake-dstack's own
		// ticker (cmd/fake-dstack/main.go) advances real wall-clock time
		// and releases the underlying fleet instance once idleSince ages
		// past idleDuration (internal/dstack/mock's F21) — but that
		// release is intentionally invisible on the wire: neither
		// dstack.Client.Get's Run nor this CRD's status expose
		// instanceUp (see internal/dstack/mock/mock.go's InstanceCount, a
		// direct-call-only accessor already covered by that package's own
		// unit tests via SetClock+Tick). Adding an HTTP route to surface
		// it here would diverge the fake from dstack's real (frozen) wire
		// shape for a black-box check the mock's own tests already make.
		// What this suite can and does assert is that the release is
		// inert from the controller's point of view: no spurious
		// re-Apply, same run, still Asleep.
		time.Sleep(6 * time.Second)

		st := getModelStatus(Default, loopModelName)
		Expect(st.Phase).To(Equal("Asleep"))
		Expect(st.RunID).To(Equal(wakeRunID))
	})
})

// Model forwarding proves squall-proxy's other half of the data path: a
// real HTTP hop from Handler.attemptForward (internal/proxy/attempt.go)
// through TemplateBackend (internal/proxy/backend.go) to a real backend
// (cmd/model-mock, wired in test/e2e/cluster/01-model-mock and
// 02-operator/proxy-patch.yaml's SQUALL_BACKEND_URL_TEMPLATE). The Model
// reaches Ready through the live controller (§6 evidence (a)/(b)), and the
// proxy streams the real chat completion.
//
// fixtureModelName is used, never loopModelName: it is untouched by every
// other spec (see its own const doc comment), so driving it here
// cannot race or interfere with the "Model lifecycle" block above,
// regardless of Ginkgo's top-level container ordering.
var _ = Describe("Model forwarding (TemplateBackend end to end)", Ordered, func() {
	var portForward *exec.Cmd
	var proxyAddr string

	BeforeAll(func() {
		By("waiting for the controller to have written an initial status for the fixture Model")
		Eventually(func(g Gomega) string {
			return getModelStatus(g, fixtureModelName).Phase
		}, 30*time.Second, time.Second).ShouldNot(BeEmpty())

		By("port-forwarding the proxy Service")
		pf, addr, err := startPortForward(controlNamespace, "svc/squall-proxy", 8080)
		Expect(err).NotTo(HaveOccurred(), "failed to port-forward svc/squall-proxy")
		portForward, proxyAddr = pf, addr
	})

	AfterAll(func() {
		if portForward != nil && portForward.Process != nil {
			_ = portForward.Process.Kill()
		}
	})

	It("forwards a real chat-completion request to the model-mock backend and relays a well-formed body", func() {
		var status int
		var body chatCompletionResponse
		Eventually(func(g Gomega) {
			var err error
			status, body, err = postChatCompletion(proxyAddr, fixtureModelName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal(http.StatusOK))
		}, 30*time.Second, time.Second).Should(Succeed())

		Expect(body.Object).To(Equal("chat.completion"))
		Expect(body.Model).To(Equal(fixtureModelName), "TemplateBackend's %s must resolve to this Model's own Service")
		Expect(body.Choices).To(HaveLen(1))
		Expect(body.Choices[0].Message.Role).To(Equal("assistant"))
		Expect(body.Choices[0].Message.Content).NotTo(BeEmpty())
		Expect(body.Usage.TotalTokens).To(BeNumerically(">", 0))
	})
})
