// SPDX-License-Identifier: Apache-2.0

package dstack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// post issues one POST and classifies the outcome. It is the ONLY place
// this package talks HTTP, so the error contract cannot drift between
// operations.
func (c *HTTPClient) post(ctx context.Context, path string, in any) ([]byte, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("dstack: encode %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("dstack: build %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dstack: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("dstack: read %s: %w", path, err)
	}
	if err := classifyError(resp.StatusCode, body); err != nil {
		return nil, err
	}
	return body, nil
}

// projectPath builds a path scoped to this client's project — every dstack
// route except ListRuns's (measured: `list` lives on the root router, not
// under /api/project/{p}/, see ListRuns below).
func (c *HTTPClient) projectPath(suffix string) string {
	return "/api/project/" + c.project + "/" + suffix
}

func (c *HTTPClient) runsPath(op string) string {
	return c.projectPath("runs/" + op)
}

// Apply is dstack's two-step apply: submit the desired spec to get_plan,
// then send the plan the server normalised back to apply. Squall never
// forces — newApplyRequest encodes the literal false (AC13).
func (c *HTTPClient) Apply(ctx context.Context, req ApplyRequest) (*Run, error) {
	plan, err := c.post(ctx, c.runsPath("get_plan"), getPlanRequest{
		RunSpec: runSpec(req),
	})
	if err != nil {
		return nil, err
	}
	var planned runPlanWire
	if err := json.Unmarshal(plan, &planned); err != nil {
		return nil, fmt.Errorf("dstack: decode run plan: %w", err)
	}

	// The CAS anchor is what WE last observed, not what get_plan just read:
	// get_plan re-reads current state, so echoing its current_resource back
	// would compare the server against itself and defeat F18 entirely.
	var current json.RawMessage
	if req.Current != nil {
		current = req.Current.raw
	}

	body, err := c.post(ctx, c.runsPath("apply"), newApplyRequest(applyPlanInput{
		RunSpec:         planned.RunSpec,
		CurrentResource: current,
	}))
	if err != nil {
		return nil, err
	}
	return decodeRun(body)
}

// Get returns current run state, or ErrNotFound if dstack no longer knows
// the run (F20: dead is not asleep).
func (c *HTTPClient) Get(ctx context.Context, name string) (*Run, error) {
	body, err := c.post(ctx, c.runsPath("get"), getRunRequest{RunName: name})
	if err != nil {
		return nil, err
	}
	return decodeRun(body)
}

// Stop terminates the run. `abort: true` is deliberate: by the time squall
// stops a run the drain gate (§5.2) has already decided it is safe to take
// capacity away, and a graceful stop would add an unbounded second wait on
// top of the bounded one we just finished.
func (c *HTTPClient) Stop(ctx context.Context, name string) error {
	_, err := c.post(ctx, c.runsPath("stop"), stopRunsRequest{RunsNames: []string{name}, Abort: true})
	return err
}

// Delete removes the run. Fleet instance release is dstack's own job via
// fleet.idleDuration (F21); Delete does not and must not model that.
func (c *HTTPClient) Delete(ctx context.Context, name string) error {
	_, err := c.post(ctx, c.runsPath("delete"), deleteRunsRequest{RunsNames: []string{name}})
	return err
}

// ListRuns backs the reconcile loop's orphan diff (§5.2).
func (c *HTTPClient) ListRuns(ctx context.Context) ([]Run, error) {
	// MEASURED: /api/project/{p}/runs/list answers 405. `list` lives on the
	// ROOT router (`/api/runs/list`), unlike every other run operation.
	body, err := c.post(ctx, "/api/runs/list", struct{}{})
	if err != nil {
		return nil, err
	}
	var wires []json.RawMessage
	if err := json.Unmarshal(body, &wires); err != nil {
		return nil, fmt.Errorf("dstack: decode run list: %w", err)
	}
	runs := make([]Run, 0, len(wires))
	for _, w := range wires {
		run, err := decodeRun(w)
		if err != nil {
			return nil, err
		}
		runs = append(runs, *run)
	}
	return runs, nil
}

// BackendConfigured reports whether backend is configured on the dstack
// server (D67). MEASURED: config_info answers 200 for a configured backend
// and 400 for one that is not. Any OTHER failure (timeout, 5xx, an
// unparseable body) is OUR ignorance, not the server's answer, and must be
// returned as an error rather than folded into false — preflight (the only
// caller) fails open on an error, exactly so an ignorant answer here can
// never be read as "not configured".
func (c *HTTPClient) BackendConfigured(ctx context.Context, backend string) (bool, error) {
	_, err := c.post(ctx, c.projectPath("backends/"+backend+"/config_info"), struct{}{})
	if err == nil {
		return true, nil
	}
	var he *HTTPError
	if errors.As(err, &he) && he.StatusCode == http.StatusBadRequest {
		return false, nil
	}
	return false, err
}

// HasFleetFor reports whether some active fleet admits backend (D58): a run
// needs a fleet on every backend it names, and without one get_plan returns
// zero offers with no error at all.
func (c *HTTPClient) HasFleetFor(ctx context.Context, backend string) (bool, error) {
	body, err := c.post(ctx, c.projectPath("fleets/list"), struct{}{})
	if err != nil {
		return false, err
	}
	var fleets []struct {
		Status string `json:"status"`
		Spec   struct {
			Configuration struct {
				Backends []string `json:"backends"`
			} `json:"configuration"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &fleets); err != nil {
		return false, fmt.Errorf("dstack: decode fleets: %w", err)
	}
	for _, f := range fleets {
		if f.Status != "active" {
			continue
		}
		// A fleet with no backends listed accepts any of them.
		if len(f.Spec.Configuration.Backends) == 0 {
			return true, nil
		}
		for _, b := range f.Spec.Configuration.Backends {
			if b == backend {
				return true, nil
			}
		}
	}
	return false, nil
}

// EnsureFleet checks fleets/get for spec.Name first and creates the fleet
// ONLY when that lookup answers ErrNotFound (see the Client interface doc:
// create-only, never update, never delete). Any OTHER error from the
// existence check — a transport failure, an auth fault, dstack's own
// ignorance — is returned as-is rather than folded into "go ahead and
// create": an ignorant answer here must never be read as "missing", the
// same discipline BackendConfigured already follows for its own 200-vs-400
// split.
func (c *HTTPClient) EnsureFleet(ctx context.Context, spec FleetSpec) error {
	_, err := c.post(ctx, c.projectPath("fleets/get"), getFleetRequest{Name: spec.Name})
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	return c.createFleet(ctx, spec)
}

// createFleet mirrors Apply's own get_plan-then-apply shape (see Apply's
// doc): submit the desired configuration to fleets/get_plan, then echo the
// plan's own (possibly server-normalised) spec back to fleets/apply with no
// current_resource — the create case. Unlike Apply's CAS, there is nothing
// to race here: EnsureFleet only reaches this after fleets/get just proved
// the name does not exist, and force stays hard-coded false regardless
// (applyFleetPlanRequest has no way to set it, mirroring AC13).
func (c *HTTPClient) createFleet(ctx context.Context, spec FleetSpec) error {
	plan, err := c.post(ctx, c.projectPath("fleets/get_plan"), getFleetPlanRequest{
		Spec: fleetSpecWire{
			Configuration: fleetConfigurationWire{
				Type:         "fleet",
				Name:         spec.Name,
				Nodes:        "0..",
				Resources:    fleetFloorResources,
				Backends:     spec.Backends,
				IdleDuration: dstackDuration(spec.IdleDuration),
			},
		},
	})
	if err != nil {
		return err
	}
	var planned fleetPlanWire
	if err := json.Unmarshal(plan, &planned); err != nil {
		return fmt.Errorf("dstack: decode fleet plan: %w", err)
	}
	// FleetPlan.get_effective_spec() falls back to Spec whenever
	// EffectiveSpec is absent or null (see fleetPlanWire's doc).
	effective := planned.Spec
	if len(planned.EffectiveSpec) > 0 && string(planned.EffectiveSpec) != "null" {
		effective = planned.EffectiveSpec
	}
	_, err = c.post(ctx, c.projectPath("fleets/apply"), newApplyFleetPlanRequest(applyFleetPlanInput{
		Spec: effective,
	}))
	return err
}

func runSpec(req ApplyRequest) runSpecWire {
	return runSpecWire{
		RunName:   req.Name,
		SSHKeyPub: req.SSHKeyPub,
		Configuration: configurationWire{
			Type:         "service",
			Name:         req.Name,
			Image:        req.Image,
			Port:         req.Port,
			Replicas:     req.Replicas,
			Probes:       []probeConfigWire{probe(req.Probe)},
			Env:          req.Env,
			Commands:     req.Args,
			Resources:    resources(req.Resources),
			Backends:     req.Placement.Backends,
			Regions:      req.Placement.Regions,
			MaxPrice:     req.Placement.MaxPrice,
			IdleDuration: dstackDuration(req.IdleDuration),
			MaxDuration:  dstackDuration(req.MaxDuration),
		},
	}
}

// probe maps the passthrough type onto the wire, applying squall's own
// defaults for anything the CR left unset. Unlike resources, an absent
// probe is NOT delegated to dstack: §6 requires probe evidence to exist,
// and dstack's default is no probe at all.
func probe(p *Probe) probeConfigWire {
	w := probeConfigWire{
		Type:       "http",
		URL:        defaultProbePath,
		Interval:   probeIntervalSeconds,
		ReadyAfter: defaultReadyAfter,
	}
	if p == nil {
		return w
	}
	if p.Path != "" {
		w.URL = p.Path
	}
	if p.IntervalSeconds > 0 {
		w.Interval = p.IntervalSeconds
	}
	if p.ReadyAfter > 0 {
		w.ReadyAfter = p.ReadyAfter
	}
	w.Method = p.Method
	w.Timeout = p.TimeoutSeconds
	return w
}

// resources maps the passthrough type onto the wire. A nil Resources
// encodes NOTHING, which hands the whole block to dstack's defaults — see
// ApplyRequest.Resources for why that is a decision and not a no-op.
func resources(r *Resources) *resourcesWire {
	if r == nil {
		return nil
	}
	w := &resourcesWire{
		Memory:  r.Memory,
		ShmSize: r.ShmSize,
	}
	if r.CPUArch != "" || r.CPUCount != "" {
		w.CPU = &cpuWire{Arch: r.CPUArch, Count: r.CPUCount}
	}
	if r.Disk != "" {
		w.Disk = &diskWire{Size: r.Disk}
	}
	if g := r.GPU; g != nil {
		w.GPU = &gpuWire{
			Vendor:            g.Vendor,
			Name:              g.Name,
			Count:             g.Count,
			Memory:            g.Memory,
			TotalMemory:       g.TotalMemory,
			ComputeCapability: g.ComputeCapability,
		}
	}
	return w
}

// decodeRun keeps the verbatim body on the Run so a later Apply can hand it
// back as dstack's whole-object CAS anchor.
func decodeRun(body []byte) (*Run, error) {
	var w runWire
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("dstack: decode run: %w", err)
	}
	return &Run{
		Name:                w.RunSpec.RunName,
		RunID:               w.ID,
		DeploymentNum:       w.DeploymentNum,
		Replicas:            w.RunSpec.Configuration.Replicas.Min,
		ServiceURL:          serviceURL(w.Service),
		Replica:             replicaEndpoint(w.Jobs, w.DeploymentNum),
		PricePerHour:        replicaPricePerHour(w.Jobs, w.DeploymentNum),
		ProbesReady:         probesReady(w.Jobs, w.DeploymentNum, echoedReadyAfter(w)),
		Env:                 w.RunSpec.Configuration.Env,
		SSHKeyPub:           w.RunSpec.SSHKeyPub,
		IdleDuration:        wireSeconds(w.RunSpec.Configuration.IdleDuration),
		MaxDuration:         wireSeconds(w.RunSpec.Configuration.MaxDuration),
		Status:              w.Status,
		SubmittedAt:         w.SubmittedAt,
		ProvisioningFailure: decodeProvisioningFailure(w),
		raw:                 append(json.RawMessage(nil), body...),
	}, nil
}

func decodeProvisioningFailure(w runWire) *ProvisioningFailure {
	if !finishedRunStatuses[w.Status] {
		return nil
	}
	reason, message := w.TerminationReason, w.StatusMessage
	if message == "" {
		message = w.Error
	}
	if s := w.LatestJobSubmission; s != nil {
		if s.TerminationReason != "" {
			reason = s.TerminationReason
		}
		for _, candidate := range []string{s.TerminationReasonMessage, s.StatusMessage, s.Error} {
			if candidate != "" {
				message = candidate
				break
			}
		}
	}
	if reason == "" && message == "" {
		return nil
	}
	return &ProvisioningFailure{RunID: w.ID, Reason: reason, Message: message}
}

func serviceURL(s *serviceWire) string {
	if s == nil {
		return ""
	}
	return s.URL
}

// echoedReadyAfter reads the ready_after this run was actually created
// with. Judging a streak against a constant would silently mis-evaluate any
// run submitted with a different ReadyAfter — including every run that
// predates a change to the CR.
func echoedReadyAfter(w runWire) int {
	for _, p := range w.RunSpec.Configuration.Probes {
		if p.ReadyAfter > 0 {
			return p.ReadyAfter
		}
	}
	return defaultReadyAfter
}
