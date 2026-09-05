// SPDX-License-Identifier: MIT

package mock

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// wireDefaultReadyAfter mirrors internal/dstack's own defaultReadyAfter
// (wire.go). Kept as an independent copy, not an import: the two packages
// agree only on JSON shape, never on Go types (see the package doc).
const wireDefaultReadyAfter = 2

type replicasWire struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// runSpecInWire is what squall's client actually submits to get_plan: a
// PLAIN INT replica count, not yet normalised.
type runSpecInWire struct {
	RunName       string `json:"run_name"`
	Configuration struct {
		Replicas int `json:"replicas"`
		// IdleDuration is decoded as a POINTER so an absent key stays
		// distinguishable from an explicit 0 — the difference dstack itself
		// treats as two different specs, and the whole subject of D156.
		IdleDuration *int `json:"idle_duration"`
	} `json:"configuration"`
}

type getPlanRequestWire struct {
	RunSpec runSpecInWire `json:"run_spec"`
}

// normalizedRunSpecWire is what get_plan echoes back and apply receives:
// replicas normalised to {min,max}, exactly as measured (§8.2).
type normalizedRunSpecWire struct {
	RunName       string `json:"run_name"`
	Configuration struct {
		Replicas replicasWire `json:"replicas"`
		// omitempty is load-bearing: get_plan echoes this struct back and
		// apply decodes the echo, so an absent idle_duration has to survive
		// the round trip as absent rather than as 0.
		IdleDuration *int `json:"idle_duration,omitempty"`
	} `json:"configuration"`
}

type runPlanResponseWire struct {
	RunSpec         json.RawMessage `json:"run_spec"`
	CurrentResource json.RawMessage `json:"current_resource"`
	Action          string          `json:"action"`
}

type applyRequestWire struct {
	Plan struct {
		RunSpec         json.RawMessage `json:"run_spec"`
		CurrentResource json.RawMessage `json:"current_resource"`
	} `json:"plan"`
	Force bool `json:"force"`
}

// currentResourceWire is the ONLY part of a previous Run this fake needs
// back to evaluate the CAS (F18): identity (id) and generation
// (deployment_num). Real dstack compares the whole object; this fake's
// Apply only ever reads these two fields off Current (see mock.go), so
// decoding just this much off the wire is sufficient and never silently
// drops CAS-relevant state.
type currentResourceWire struct {
	ID            string `json:"id"`
	DeploymentNum int    `json:"deployment_num"`
}

type getRunRequestWire struct {
	RunName string `json:"run_name"`
}

type deleteRunsRequestWire struct {
	RunsNames []string `json:"runs_names"`
}

type stopRunsRequestWire struct {
	RunsNames []string `json:"runs_names"`
	Abort     bool     `json:"abort"`
}

type probeWire struct {
	SuccessStreak int `json:"success_streak"`
}

type jobSubmissionWire struct {
	DeploymentNum int         `json:"deployment_num"`
	Status        string      `json:"status"`
	Probes        []probeWire `json:"probes"`
}

type jobWire struct {
	JobSubmissions []jobSubmissionWire `json:"job_submissions"`
}

type serviceWire struct {
	URL string `json:"url"`
}

// runOutWire is the shape internal/dstack.decodeRun expects, measured
// against a real server: id, submitted_at, status, deployment_num, jobs
// (with job_submissions[].probes[].success_streak), service.url, and
// run_spec.configuration.replicas.{min,max}.
type runOutWire struct {
	ID            string       `json:"id"`
	SubmittedAt   time.Time    `json:"submitted_at"`
	Status        string       `json:"status"`
	DeploymentNum int          `json:"deployment_num"`
	Jobs          []jobWire    `json:"jobs"`
	Service       *serviceWire `json:"service"`
	RunSpec       struct {
		RunName       string `json:"run_name"`
		Configuration struct {
			Replicas     replicasWire `json:"replicas"`
			IdleDuration *int         `json:"idle_duration,omitempty"`
		} `json:"configuration"`
	} `json:"run_spec"`
}

func runToWire(run *Run) runOutWire {
	var out runOutWire
	out.ID = run.RunID
	out.SubmittedAt = run.SubmittedAt
	out.Status = run.Status
	out.DeploymentNum = run.DeploymentNum
	out.RunSpec.RunName = run.Name
	out.RunSpec.Configuration.Replicas = replicasWire{Min: run.Replicas, Max: run.Replicas}
	out.RunSpec.Configuration.IdleDuration = run.IdleDuration
	if run.ServiceURL != "" {
		out.Service = &serviceWire{URL: run.ServiceURL}
	}
	if run.Replicas > 0 {
		streak := 0
		if run.ProbesReady {
			streak = wireDefaultReadyAfter
		}
		out.Jobs = []jobWire{{JobSubmissions: []jobSubmissionWire{{
			DeploymentNum: run.DeploymentNum,
			Status:        "running",
			Probes:        []probeWire{{SuccessStreak: streak}},
		}}}}
	}
	return out
}

// NewHTTPServer mounts Server behind net/http on dstack's REAL run-
// management paths (measured, docs/references/dstack-real-api.md §1),
// requiring token as the bearer credential on every one of them. Every
// route decodes/encodes around the same Apply/Get/Delete/ListRuns/
// GatewayGet methods a direct Go caller uses, so the HTTP and direct-call
// surfaces exercise one state machine — there is no second implementation
// to drift. Meant for httptest.NewServer in client and envtest-based
// controller tests.
//
// GatewayGet's route keeps its own, separate token check against
// ValidToken (see its doc comment) — it is not part of dstack's real API
// surface and predates this rewrite.
func NewHTTPServer(s *Server, token string) http.Handler {
	mux := http.NewServeMux()

	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if bearerToken(r) != token {
				writeError(w, http.StatusForbidden, "Invalid token", "")
				return
			}
			next(w, r)
		}
	}

	mux.HandleFunc("POST /api/project/{project}/runs/get_plan", authed(s.handleGetPlan))
	mux.HandleFunc("POST /api/project/{project}/runs/apply", authed(s.handleApply))
	mux.HandleFunc("POST /api/project/{project}/runs/get", authed(s.handleGetRun))
	mux.HandleFunc("POST /api/project/{project}/runs/delete", authed(s.handleDeleteRun))
	mux.HandleFunc("POST /api/project/{project}/runs/stop", authed(s.handleStopRun))
	// MEASURED: `list` lives on the ROOT router, not the per-project one
	// every other run operation uses (§1). Only this path is mounted, so a
	// client regression calling the project-scoped path fails loudly
	// instead of silently working against the fake.
	mux.HandleFunc("POST /api/runs/list", authed(s.handleListRuns))

	// Task 5 (D58, D67): this fake models F17/F18/F20/F21/F23 (see the
	// package doc), not dstack's backend registry or fleet placement, so
	// both routes always answer the "everything is fine" default —
	// configured, and admitted by a fleet with no backend restriction.
	// Exercising BackendUnavailable/NoFleet themselves is
	// internal/controller/squall's preflight_test.go, against a scriptable
	// fake, not this HTTP surface.
	mux.HandleFunc("POST /api/project/{project}/backends/{name}/config_info", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, struct{}{})
	}))
	mux.HandleFunc("POST /api/project/{project}/fleets/list", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{{
			"status": "active",
			"spec":   map[string]any{"configuration": map[string]any{"backends": []string{}}},
		}})
	}))
	// LIVE-7 Branch B: EnsureFleet's create-only, level-triggered contract,
	// exercised over the real wire shape. get/get_plan/apply mirror the
	// real dstack routes measured in docs/references/dstack-real-api.md
	// §9.8 — only enough of each body is decoded to drive
	// Server.EnsureFleet/HasFleet, same "existence only" scope as
	// fleets/list above.
	mux.HandleFunc("POST /api/project/{project}/fleets/get", authed(s.handleGetFleet))
	mux.HandleFunc("POST /api/project/{project}/fleets/get_plan", authed(s.handleGetFleetPlan))
	mux.HandleFunc("POST /api/project/{project}/fleets/apply", authed(s.handleApplyFleet))

	mux.HandleFunc("GET /gateway/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(s.GatewayGet(r.PathValue("name"), bearerToken(r)))
	})

	return mux
}

// Handler mounts Server with ValidToken as the run-management credential —
// the default every existing caller (cmd/fake-dstack, envtest suites) uses.
func (s *Server) Handler() http.Handler {
	return NewHTTPServer(s, ValidToken)
}

func (s *Server) handleGetPlan(w http.ResponseWriter, r *http.Request) {
	var in getPlanRequestWire
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "error")
		return
	}

	// MEASURED (§8.2): get_plan normalises a plain int replica count into
	// {min,max}. current_resource is always emitted null here — squall's
	// client never reads it (it CASes against its own last-observed Run,
	// not against what get_plan just re-read; see internal/dstack/http.go).
	var normalized normalizedRunSpecWire
	normalized.RunName = in.RunSpec.RunName
	normalized.Configuration.Replicas = replicasWire{Min: in.RunSpec.Configuration.Replicas, Max: in.RunSpec.Configuration.Replicas}
	normalized.Configuration.IdleDuration = in.RunSpec.Configuration.IdleDuration
	specBody, err := json.Marshal(normalized)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "error")
		return
	}

	writeJSON(w, runPlanResponseWire{RunSpec: specBody, Action: "create"})
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var in applyRequestWire
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "error")
		return
	}

	var spec normalizedRunSpecWire
	if err := json.Unmarshal(in.Plan.RunSpec, &spec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid run_spec: "+err.Error(), "error")
		return
	}

	var current *Run
	if len(in.Plan.CurrentResource) > 0 && string(in.Plan.CurrentResource) != "null" {
		var c currentResourceWire
		if err := json.Unmarshal(in.Plan.CurrentResource, &c); err != nil {
			writeError(w, http.StatusBadRequest, "invalid current_resource: "+err.Error(), "error")
			return
		}
		current = &Run{RunID: c.ID, DeploymentNum: c.DeploymentNum}
	}

	run, err := s.Apply(ApplyRequest{
		Name:         spec.RunName,
		Replicas:     spec.Configuration.Replicas.Min,
		Current:      current,
		Force:        in.Force,
		IdleDuration: spec.Configuration.IdleDuration,
	})
	switch {
	case errors.Is(err, ErrCannotOverride):
		// MEASURED (D156 leg C): HTTP 400, generic "error" code, and the
		// message is the only discriminator — exactly like the CAS conflict.
		writeError(w, http.StatusBadRequest, "Cannot override active run. Stop the run first.", "error")
		return
	case errors.Is(err, ErrForceForbidden):
		writeError(w, http.StatusBadRequest, err.Error(), "error")
		return
	case errors.Is(err, ErrResourceChanged):
		// MEASURED (§8.1): the CAS conflict is HTTP 400, distinguished from
		// "not found" only by message — dstack tags it with the generic
		// code "error", not a dedicated one.
		writeError(w, http.StatusBadRequest, err.Error(), "error")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error(), "error")
		return
	}

	writeJSON(w, runToWire(run))
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	var in getRunRequestWire
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "error")
		return
	}

	run, err := s.Get(in.RunName)
	if errors.Is(err, ErrNotFound) {
		// MEASURED (§8.1): 400 + resource_not_exists, never 404.
		writeError(w, http.StatusBadRequest, "Run not found", "resource_not_exists")
		return
	}
	writeJSON(w, runToWire(run))
}

func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	var in deleteRunsRequestWire
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "error")
		return
	}
	if len(in.RunsNames) != 1 {
		writeError(w, http.StatusBadRequest, "runs_names must carry exactly one name", "error")
		return
	}

	err := s.Delete(in.RunsNames[0])
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusBadRequest, "Run not found", "resource_not_exists")
		return
	}
	if errors.Is(err, ErrDeleteActiveRun) {
		// MEASURED on dstack 0.21.2 (D56): the real server answers 400 with
		// this message for a run that is not already terminal. The wording
		// is what a caller sees, so it is reproduced verbatim.
		writeError(w, http.StatusBadRequest,
			"Cannot delete active runs: ["+in.RunsNames[0]+"]", "error")
		return
	}
	writeJSON(w, struct{}{})
}

// handleStopRun terminates a run, which is what makes it deletable. dstack
// takes `abort`; the fake terminates either way, since it models no
// graceful-shutdown delay.
func (s *Server) handleStopRun(w http.ResponseWriter, r *http.Request) {
	var in stopRunsRequestWire
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "error")
		return
	}
	if len(in.RunsNames) != 1 {
		writeError(w, http.StatusBadRequest, "runs_names must carry exactly one name", "error")
		return
	}
	if errors.Is(s.Stop(in.RunsNames[0]), ErrNotFound) {
		writeError(w, http.StatusBadRequest, "Run not found", "resource_not_exists")
		return
	}
	writeJSON(w, struct{}{})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	runs := s.ListRuns()
	wires := make([]runOutWire, len(runs))
	for i, run := range runs {
		wires[i] = runToWire(&run)
	}
	writeJSON(w, wires)
}

type getFleetRequestWire struct {
	Name string `json:"name"`
}

type fleetConfigurationWire struct {
	Name     string   `json:"name"`
	Backends []string `json:"backends"`
}

type fleetSpecWire struct {
	Configuration fleetConfigurationWire `json:"configuration"`
}

type getFleetPlanRequestWire struct {
	Spec fleetSpecWire `json:"spec"`
}

type fleetPlanResponseWire struct {
	Spec json.RawMessage `json:"spec"`
}

type applyFleetPlanRequestWire struct {
	Plan struct {
		Spec json.RawMessage `json:"spec"`
	} `json:"plan"`
	Force bool `json:"force"`
}

// handleGetFleet answers EnsureFleet's existence check. MEASURED shape
// (§9.8): a name the server does not know answers 400 +
// "resource_not_exists", the SAME discriminator classifyError already
// treats as ErrNotFound everywhere else.
func (s *Server) handleGetFleet(w http.ResponseWriter, r *http.Request) {
	var in getFleetRequestWire
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "error")
		return
	}
	if !s.HasFleet(in.Name) {
		writeError(w, http.StatusBadRequest, "Resource not found", "resource_not_exists")
		return
	}
	writeJSON(w, map[string]any{"name": in.Name, "status": "active"})
}

// handleGetFleetPlan echoes the submitted spec back verbatim, mirroring
// runs/get_plan's shape (spec normalisation is not part of what this fake
// models — see the package doc's "no fleet placement" scope note).
func (s *Server) handleGetFleetPlan(w http.ResponseWriter, r *http.Request) {
	var in getFleetPlanRequestWire
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "error")
		return
	}
	specBody, err := json.Marshal(in.Spec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "error")
		return
	}
	writeJSON(w, fleetPlanResponseWire{Spec: specBody})
}

// handleApplyFleet creates the fleet named in the echoed spec — CREATE-ONLY
// via Server.EnsureFleet, which is itself a no-op on a name that already
// exists (see its doc). force is decoded but never consulted: this fake
// refuses nothing on the fleet surface today because EnsureFleet's only
// caller (internal/dstack.HTTPClient.EnsureFleet) never reaches the apply
// step except on the create path, where there is nothing to CAS against.
func (s *Server) handleApplyFleet(w http.ResponseWriter, r *http.Request) {
	var in applyFleetPlanRequestWire
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "error")
		return
	}
	var spec fleetSpecWire
	if err := json.Unmarshal(in.Plan.Spec, &spec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid spec: "+err.Error(), "error")
		return
	}
	s.EnsureFleet(spec.Configuration.Name, spec.Configuration.Backends)
	writeJSON(w, map[string]any{"name": spec.Configuration.Name, "status": "active"})
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	return strings.TrimPrefix(auth, prefix)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError emits dstack's MEASURED error shape (§8.1). The status codes
// are upstream's, not ours: dstack answers 400 for BOTH "not found" and the
// CAS conflict, and distinguishes them only in the body. A fake that
// answered 404/409 here would let a status-code-keyed client pass its tests
// and then fail against a real server — which is exactly the bug this
// rewrite fixes.
func writeError(w http.ResponseWriter, status int, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"detail": []map[string]any{{"msg": msg, "code": code}},
	})
}
