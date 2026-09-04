// SPDX-License-Identifier: MIT

package mock

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ackstorm/squall/internal/clock"
)

// ErrResourceChanged is returned by Apply when Current no longer matches
// current state — dstack's own optimistic-concurrency message (F18).
var ErrResourceChanged = errors.New("Resource has been changed. Try again or use force apply")

// ErrForceForbidden is returned by Apply whenever Force is set. Squall must
// never send force (§5.2); the fake refuses it unconditionally so a future
// caller adding it fails a test rather than a bill.
var ErrForceForbidden = errors.New("dstack mock: force apply is forbidden by construction; squall must never send force")

// ErrNotFound is returned by Get and Delete when the named service has never
// been applied, or has been deleted. Measured against a real server (§9.4):
// a TERMINAL run (an uncommanded death) is NOT ErrNotFound — dstack still
// answers Get successfully for it, with a terminal Status. Dead is not
// asleep, but neither of them is "not found"; only a name dstack has never
// registered, or one explicitly Delete'd, is.
var ErrNotFound = errors.New("dstack mock: run not found")

// ErrDeleteActiveRun mirrors real dstack, which refuses to delete a run
// that is not already terminal: HTTP 400 "Cannot delete active runs: [...]".
// The fake used to accept it, and that is precisely what let D56 ship — a
// teardown that called Delete on a live run, retried the 400 forever, and
// left a rented GPU billing with the CR wedged in Draining.
//
// The double is deliberately no more permissive than the real server. Same
// rule as ErrForceForbidden: a caller that gets this wrong must fail a test
// rather than a bill.
var ErrDeleteActiveRun = errors.New("dstack mock: Cannot delete active runs")

// ValidToken is the bearer token the fake's gateway route accepts (F23's
// three status branches). The run-management surface's token is whatever
// NewHTTPServer was constructed with — see its doc comment.
const ValidToken = "test-gateway-token"

// runSeq mints unique run ids across all services and Server instances —
// good enough for a test double where uniqueness, not unpredictability, is
// what matters.
var runSeq atomic.Uint64

func newRunID(name string) string {
	return fmt.Sprintf("run-%s-%d", name, runSeq.Add(1))
}

// ApplyRequest is the input to Apply — the fake's analogue of dstack's
// apply_plan.
type ApplyRequest struct {
	Name     string
	Replicas int
	// Current is the CAS anchor (F18), mirroring dstack's real, whole-object
	// semantics: nil means "expect no live run to exist". Apply rejects the
	// request (ErrResourceChanged) unless Current's RunID and DeploymentNum
	// both match the currently live run — ignored entirely when the run
	// doesn't exist yet or is terminal, since that path always mints a
	// fresh run (F20).
	Current *Run
	// Force must never be true for a real caller (§5.2); the fake refuses
	// it unconditionally (F18).
	Force bool
	// FleetIdleDuration sets how long an idle instance survives a flip to
	// zero replicas (F21). A zero value leaves the previously configured
	// duration untouched, so only the first Apply for a service needs to
	// set it.
	FleetIdleDuration time.Duration
}

// Run is the fake's view of dstack run state, returned by Apply, Get and
// ListRuns.
type Run struct {
	Name          string
	RunID         string
	DeploymentNum int
	Replicas      int
	ProbesReady   bool

	// Status mirrors dstack's own run status (§7, measured §9.4): "pending"
	// while asleep (registered, addressable, zero live replicas but NOT
	// dead), "running" while awake, "terminated" after an uncommanded death
	// (F20). Both asleep and dead answer the fake's gateway with the same
	// code (404/503 split below), but Status is what tells them apart on
	// the run-management surface, exactly as a real server does.
	Status string

	// ServiceURL mirrors dstack's own service proxy path (measured §8.3):
	// "/proxy/services/{project}/{name}/". The fake always uses "main" as
	// its project, matching ValidToken's own scope.
	ServiceURL string

	SubmittedAt time.Time
}

// service is the mutable per-name state the mock tracks.
type service struct {
	runID         string
	deploymentNum int
	replicas      int
	submittedAt   time.Time
	replicasUpAt  time.Time
	// terminal marks an uncommanded death (host loss, spot reclaim). A
	// terminal run is deregistered from the fake's gateway (F20/F23:
	// GatewayGet -> 404) and the next Apply mints a brand new run rather
	// than flipping in place. It remains fully visible on Get/ListRuns,
	// with Status "terminated" — measured (§9.4), unlike this fake's
	// earlier, invented assumption that a terminal run reads back as
	// ErrNotFound.
	terminal bool

	// instanceUp is the fleet-owned machine backing this service (F21).
	// Flipping replicas to 0 does NOT clear it — only Tick, once
	// fleetIdleDuration has elapsed since idleSince, does.
	instanceUp bool
	// idleSince is when replicas last flipped from >0 to 0; the zero
	// Time means "not currently idle" (either awake, or never flipped).
	idleSince time.Time
	// fleetIdleDuration is how long an idle instance survives a flip to
	// zero (F21). Set by the first Apply that supplies one; later Applies
	// with a zero value leave it untouched.
	fleetIdleDuration time.Duration

	// applyCount counts successful Applies against this run generation —
	// reset on a terminal recreate (F20). Callers use ApplyCount to assert
	// "exactly one effective wake action" under concurrent demand (AC4).
	applyCount int
}

// fleet is the mock's minimal view of a created fleet: just enough to
// answer fleets/get truthfully for EnsureFleet's create-only contract
// (LIVE-7 Branch B). Like HasFleetFor's own route (NewHTTPServer's doc
// comment), this fake models no offers, no instances, no real placement —
// only existence.
type fleet struct {
	backends []string
}

// Server is an in-memory, concurrency-safe fake of the dstack server +
// gateway. Zero value is not usable — construct with New.
type Server struct {
	mu         sync.Mutex
	services   map[string]*service
	fleets     map[string]fleet
	clock      clock.Clock
	probeDelay time.Duration
}

// New constructs an empty Server using the real wall clock. Use SetClock in
// tests that exercise fleet idle release (F21).
func New() *Server {
	return &Server{services: make(map[string]*service), fleets: make(map[string]fleet), clock: clock.RealClock{}}
}

// EnsureFleet is the fake's analogue of internal/dstack.HTTPClient's
// EnsureFleet: CREATE-ONLY. A name already present is left untouched —
// its backends are never updated, matching the real client's contract
// that an existing auto fleet is never mutated by squall, only created
// once.
func (s *Server) EnsureFleet(name string, backends []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.fleets[name]; ok {
		return
	}
	s.fleets[name] = fleet{backends: backends}
}

// HasFleet reports whether EnsureFleet has created name — for tests
// asserting the create-only, level-triggered contract end to end (e.g. "a
// second EnsureFleet call for the same name is a no-op").
func (s *Server) HasFleet(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.fleets[name]
	return ok
}

// SetClock swaps the Server's time source. Intended for tests only.
func (s *Server) SetClock(c clock.Clock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = c
}

// SetProbeDelay sets how long after a wake the fake's probes begin
// passing. Intended for tests exercising the running-vs-ready distinction.
func (s *Server) SetProbeDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probeDelay = d
}

// probesReady reports whether the fake's probes are passing: replicas up,
// and probeDelay elapsed since they came up (F35).
func (s *Server) probesReady(svc *service) bool {
	if svc.replicas == 0 || svc.replicasUpAt.IsZero() {
		return false
	}
	return !s.clock.Now().Before(svc.replicasUpAt.Add(s.probeDelay))
}

// dstack's own run statuses this fake produces (§7, measured §9.4).
const (
	statusTerminated = "terminated"
	statusPending    = "pending"
	statusRunning    = "running"
)

// statusFor derives dstack's own run status (§7, measured §9.4) from the
// fake's internal state.
func statusFor(svc *service) string {
	switch {
	case svc.terminal:
		return statusTerminated
	case svc.replicas == 0:
		return statusPending
	default:
		return statusRunning
	}
}

// runFor builds the Run view Apply/Get/ListRuns all return — one
// construction site so the three surfaces cannot drift.
func (s *Server) runFor(name string, svc *service) *Run {
	return &Run{
		Name:          name,
		RunID:         svc.runID,
		DeploymentNum: svc.deploymentNum,
		Replicas:      svc.replicas,
		ProbesReady:   s.probesReady(svc),
		Status:        statusFor(svc),
		ServiceURL:    "/proxy/services/main/" + name + "/",
		SubmittedAt:   svc.submittedAt,
	}
}

// Apply mutates the named service in place, minting a run on first use or
// after a terminal death (F20). It never honors Force (F18) and rejects an
// Current anchor that does not match the current live run's identity
// (RunID + DeploymentNum) — the loser of a race fails loudly.
func (s *Server) Apply(req ApplyRequest) (*Run, error) {
	if req.Force {
		return nil, ErrForceForbidden
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	svc, ok := s.services[req.Name]
	switch {
	case !ok || svc.terminal:
		// F20: no run, or the previous one died — mint a fresh one. The
		// caller's Current is irrelevant here, same as a first-ever Apply:
		// there is nothing live to CAS against.
		svc = &service{runID: newRunID(req.Name), submittedAt: s.clock.Now()}
		s.services[req.Name] = svc
	case req.Current == nil || req.Current.RunID != svc.runID || req.Current.DeploymentNum != svc.deploymentNum:
		return nil, ErrResourceChanged
	}

	wasAwake := svc.replicas > 0
	svc.deploymentNum++
	svc.replicas = req.Replicas
	svc.applyCount++
	svc.instanceUp = true // this Apply call itself never releases the machine (F21)
	if req.FleetIdleDuration > 0 {
		svc.fleetIdleDuration = req.FleetIdleDuration
	}

	switch {
	case svc.replicas == 0:
		svc.replicasUpAt = time.Time{}
		if wasAwake {
			// The flip that just terminated the job starts the fleet's idle
			// clock; Tick releases the instance once fleetIdleDuration passes.
			svc.idleSince = s.clock.Now()
		}
	case svc.replicas > 0:
		svc.idleSince = time.Time{}
		if !wasAwake {
			svc.replicasUpAt = s.clock.Now()
		}
	}

	return s.runFor(req.Name, svc), nil
}

// Tick evaluates fleet idle release (F21): any service whose job has been
// at 0 replicas for at least fleetIdleDuration loses its instance. Call it
// after advancing a FakeClock — nothing expires on its own.
func (s *Server) Tick() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	for _, svc := range s.services {
		if svc.instanceUp && svc.replicas == 0 && !svc.idleSince.IsZero() &&
			svc.fleetIdleDuration > 0 && now.Sub(svc.idleSince) >= svc.fleetIdleDuration {
			svc.instanceUp = false
		}
	}
}

// InstanceCount reports whether the named service's fleet instance is
// currently up: 1 if so, 0 if released or unknown. There is at most one
// instance per service in this fake (offer selection and multi-instance
// fleets are deliberately not modelled — see the package doc).
func (s *Server) InstanceCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	svc, ok := s.services[name]
	if !ok || !svc.instanceUp {
		return 0
	}
	return 1
}

// ApplyCount reports how many successful Applies have landed against the
// named service's current run generation. Returns 0 for an unknown name.
func (s *Server) ApplyCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	svc, ok := s.services[name]
	if !ok {
		return 0
	}
	return svc.applyCount
}

// MustApply calls Apply and fails the test immediately on error.
func (s *Server) MustApply(t *testing.T, req ApplyRequest) *Run {
	t.Helper()
	run, err := s.Apply(req)
	if err != nil {
		t.Fatalf("Apply(%+v): %v", req, err)
	}
	return run
}

// GatewayGet answers the way dstack's gateway answers a data-plane request
// against the named service (F23): bad/missing token -> 403; unregistered
// or terminal -> 404; registered with 0 replicas -> 503; otherwise 200.
//
// This is a DIFFERENT surface from dstack's real service proxy (measured
// §9.5: the real proxy answers 404 through the WHOLE wake, not 503) — it
// models this fake's own simplified F23 story and is exercised only by this
// package's own tests, not by squall-proxy's actual forwarding path (see
// internal/proxy.TemplateBackend). Kept as-is; task 4b's D44 fix is proven
// against synthetic responses and the real Tier-1 e2e, not against this
// route.
func (s *Server) GatewayGet(name, token string) int {
	if token != ValidToken {
		return http.StatusForbidden
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	svc, ok := s.services[name]
	if !ok || svc.terminal {
		return http.StatusNotFound
	}
	if svc.replicas == 0 {
		return http.StatusServiceUnavailable
	}
	return http.StatusOK
}

// Get returns the named service's current run state. Measured (§9.4): a
// terminal run is STILL known to dstack — Get succeeds, with Status
// "terminated" — so only a name never applied, or one explicitly Delete'd,
// is ErrNotFound. This is the direct-call analogue of the run-management
// surface (POST .../runs/get), distinct from GatewayGet's 404-for-either
// data-plane view.
func (s *Server) Get(name string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	svc, ok := s.services[name]
	if !ok {
		return nil, ErrNotFound
	}
	return s.runFor(name, svc), nil
}

// ListRuns returns every service the fake has ever applied, live or
// terminal — what backs the reconcile loop's orphan diff (§5.2). Measured
// (§9.4): dstack keeps a terminal run's record; Status, not presence, is
// what tells a caller it is dead. Delete is the only surface that removes a
// run from this list.
func (s *Server) ListRuns() []Run {
	s.mu.Lock()
	defer s.mu.Unlock()

	runs := make([]Run, 0, len(s.services))
	for name, svc := range s.services {
		runs = append(runs, *s.runFor(name, svc))
	}
	return runs
}

// Delete removes the named service's record entirely, live or terminal.
// Only a name the fake has never seen is ErrNotFound.
func (s *Server) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	svc, ok := s.services[name]
	if !ok {
		return ErrNotFound
	}
	if !svc.terminal {
		// MEASURED against dstack 0.21.2 (D56): an active run cannot be
		// deleted. Stop it first.
		return ErrDeleteActiveRun
	}
	delete(s.services, name)
	return nil
}

// Stop terminates the named service, which is what makes it deletable. It
// is idempotent: stopping an already-terminal run is a no-op, and stopping
// an unknown name reports ErrNotFound so a caller can tell "already gone"
// from "still there".
func (s *Server) Stop(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	svc, ok := s.services[name]
	if !ok {
		return ErrNotFound
	}
	svc.terminal = true
	svc.instanceUp = false
	svc.replicas = 0
	return nil
}

// Terminate marks the named service dead — an uncommanded death such as
// host loss or a spot reclaim (F20). A terminal run is deregistered from
// the gateway (404, not 503); the next Apply mints a new run id. Terminating
// an unknown name is a no-op.
func (s *Server) Terminate(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if svc, ok := s.services[name]; ok {
		svc.terminal = true
		svc.instanceUp = false // the host is gone too — dead is not asleep
	}
}
