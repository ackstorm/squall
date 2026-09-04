// SPDX-License-Identifier: MIT

package dstack

import (
	"encoding/json"
	"time"
)

// probeWire is dstack's entire per-probe state. Measured on 0.21.2: the
// model is `{success_streak: int}` and nothing else — no last-check time,
// no failure reason.
type probeWire struct {
	SuccessStreak int `json:"success_streak"`
}

// jobSubmissionWire is one attempt at running one replica.
type jobSubmissionWire struct {
	SubmittedAt              time.Time   `json:"submitted_at"`
	DeploymentNum            int         `json:"deployment_num"`
	Status                   string      `json:"status"`
	StatusMessage            string      `json:"status_message"`
	TerminationReason        string      `json:"termination_reason"`
	TerminationReasonMessage string      `json:"termination_reason_message"`
	Error                    string      `json:"error"`
	Probes                   []probeWire `json:"probes"`

	// JobProvisioningData is where the replica actually runs. Measured on a
	// live Vast.ai run: hostname 79.161.156.12, ssh_port 40097, username
	// root, ssh_proxy null, dockerized false.
	JobProvisioningData *jobProvisioningDataWire `json:"job_provisioning_data"`
}

// jobProvisioningDataWire is the subset of dstack's provisioning record that
// says how to REACH a replica. Only the fields a direct tunnel needs are
// decoded.
type jobProvisioningDataWire struct {
	Hostname string `json:"hostname"`
	SSHPort  int    `json:"ssh_port"`
	Username string `json:"username"`

	// Price is what this replica costs PER HOUR, as the backend quoted it.
	// MEASURED live 2026-08-31 against a real vastai run: 1.89445. It is the
	// observed half of AC19's declared/observed cost pair, and closes D26 —
	// the collector existed and was fed a hard-coded zero because nothing
	// carried this number, while dstack had been returning it all along.
	Price float64 `json:"price"`

	// SSHProxy and Dockerized both mean EXTRA HOPS. dstack's own
	// get_container_ssh_credentials returns an ordered chain of hosts for
	// those cases (a Kubernetes jump pod, or a container reached through its
	// instance). Squall implements the ONE-HOP case only, so a non-nil proxy
	// or dockerized: true must yield no endpoint at all rather than a
	// half-correct one — the caller then keeps using dstack's own proxy,
	// which handles every topology.
	SSHProxy   json.RawMessage `json:"ssh_proxy"`
	Dockerized bool            `json:"dockerized"`
}

// replicaPricePerHour is what the CURRENT deployment's live replica costs
// per hour, or 0 when nothing is provisioned. Same filtering as
// replicaEndpoint and for the same reason (D46): a terminated submission
// from a previous deployment keeps its old price forever, and reporting that
// as the running cost is worse than reporting nothing.
//
// 0 means "no observed price", never "free" — the collector must not emit
// the observed gauge on a zero, or a scaled-to-zero Model reads as a price
// crash (see ModelPriceCollector).
func replicaPricePerHour(jobs []jobWire, deploymentNum int) float64 {
	for _, j := range jobs {
		for _, s := range j.JobSubmissions {
			if s.DeploymentNum != deploymentNum || finishedJobStatuses[s.Status] {
				continue
			}
			if p := s.JobProvisioningData; p != nil && p.Price > 0 {
				return p.Price
			}
		}
	}
	return 0
}

// jobWire is one replica of the service. dstack appends a new submission
// per attempt, and — measured, D46 — KEEPS the jobs of previous
// deployments in the list after an in-place flip.
type jobWire struct {
	JobSubmissions []jobSubmissionWire `json:"job_submissions"`
	JobSpec        *jobSpecWire        `json:"job_spec"`
}

// jobSpecWire carries the port the engine listens on INSIDE the container —
// the far end of the tunnel. Measured: 8000 for vLLM.
type jobSpecWire struct {
	ServicePort int `json:"service_port"`
}

// replicaEndpoint extracts how to reach the live replica of the CURRENT
// deployment, or nil when squall cannot reach it directly.
//
// nil is the safe answer and the common one: any topology needing more than
// a single SSH hop, any submission that is not live, any missing field. The
// caller falls back to dstack's own service proxy, which handles everything.
// Returning a partially-correct endpoint would send real user traffic into a
// tunnel that cannot be built.
//
// The deploymentNum filter is load-bearing for the same reason it is in
// probesReady (D46): an in-place flip leaves the PREVIOUS deployment's
// replica in `jobs`, terminated, and its stale hostname would otherwise be
// served as if current.
func replicaEndpoint(jobs []jobWire, deploymentNum int) *ReplicaEndpoint {
	for _, j := range jobs {
		if j.JobSpec == nil || j.JobSpec.ServicePort <= 0 {
			continue
		}
		for _, s := range j.JobSubmissions {
			if s.DeploymentNum != deploymentNum || finishedJobStatuses[s.Status] {
				continue
			}
			p := s.JobProvisioningData
			if p == nil || p.Hostname == "" || p.SSHPort <= 0 || p.Username == "" {
				continue
			}
			if p.Dockerized || len(p.SSHProxy) > 0 && string(p.SSHProxy) != "null" {
				continue
			}
			return &ReplicaEndpoint{
				Host:        p.Hostname,
				SSHPort:     p.SSHPort,
				User:        p.Username,
				ServicePort: j.JobSpec.ServicePort,
			}
		}
	}
	return nil
}

// finishedJobStatuses mirrors dstack's own terminal set.
var finishedJobStatuses = map[string]bool{
	"terminated": true,
	"failed":     true,
	"done":       true,
	"aborted":    true,
}

// probesReady is §6 evidence (a), derived: a run is probe-ready when it has
// at least one LIVE submission of the CURRENT deployment, and every such
// submission has at least one probe with every probe's success streak at
// or above readyAfter.
//
// The deploymentNum filter is not a refinement, it is load-bearing (D46):
// an in-place flip leaves the previous deployment's replica in `jobs`,
// terminated, with `probes: []`, permanently. Counting it makes readiness
// unreachable for the life of the run.
//
// Absence is never readiness. No live submission, no probes, or a short
// streak all mean "not ready" — "dstack job running" is never Ready (§6),
// and a missing probe is exactly how that would smuggle itself back in.
func probesReady(jobs []jobWire, deploymentNum, readyAfter int) bool {
	if readyAfter < 1 {
		return false
	}
	live := 0
	for _, j := range jobs {
		for _, s := range j.JobSubmissions {
			if s.DeploymentNum != deploymentNum || finishedJobStatuses[s.Status] {
				continue
			}
			live++
			if len(s.Probes) == 0 {
				return false
			}
			for _, p := range s.Probes {
				if p.SuccessStreak < readyAfter {
					return false
				}
			}
		}
	}
	return live > 0
}
