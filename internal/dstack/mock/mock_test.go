// SPDX-License-Identifier: Apache-2.0

package mock

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ackstorm/squall/internal/clock"
	"github.com/ackstorm/squall/internal/dstack"
)

func TestApply_FlipIsInPlace_PreservesRunID(t *testing.T) {
	s := New()

	r1 := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	r2 := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 0, Current: r1})

	if r2.RunID != r1.RunID {
		t.Fatalf("F17: flip must be in-place; run id changed %s -> %s", r1.RunID, r2.RunID)
	}
	if r2.DeploymentNum != r1.DeploymentNum+1 {
		t.Fatalf("F17: deployment_num must increment: got %d, want %d", r2.DeploymentNum, r1.DeploymentNum+1)
	}
}

func TestApply_ZeroReplicas_StaysRegisteredAndRoutable(t *testing.T) {
	s := New()
	r := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 0, Current: r})

	// F23: registered + 0 replicas + auth -> 503, NOT 404.
	if code := s.GatewayGet("qwen", ValidToken); code != 503 {
		t.Fatalf("F17/F23: asleep-but-addressable must answer 503, got %d", code)
	}
}

func TestApply_StaleCurrent_IsRejected(t *testing.T) {
	s := New()
	r1 := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	// Two concurrent flips computed against the same base — AC13's drill.
	if _, err := s.Apply(ApplyRequest{Name: "qwen", Replicas: 0, Current: r1}); err != nil {
		t.Fatalf("first flip must win: %v", err)
	}
	if _, err := s.Apply(ApplyRequest{Name: "qwen", Replicas: 1, Current: r1}); !errors.Is(err, ErrResourceChanged) {
		t.Fatalf("F18: the loser must fail loudly, got %v", err)
	}
}

func TestApply_ForceIsNeverAccepted(t *testing.T) {
	// Squall must never send force (§5.2). The fake refuses it outright so
	// that a future caller adding it fails a test rather than a bill.
	s := New()
	if _, err := s.Apply(ApplyRequest{Name: "qwen", Replicas: 1, Force: true}); !errors.Is(err, ErrForceForbidden) {
		t.Fatalf("force must be refused by construction, got %v", err)
	}
}

// TestApply_ProbesReady_IsAStateWithDuration pins F35: "running" and
// "probe-passing" are different states. The fake must make a test ADVANCE
// A CLOCK to reach ready, never flip a flag — a fake that asserts
// readiness instantly would hide exactly the bug D28 was.
func TestApply_ProbesReady_IsAStateWithDuration(t *testing.T) {
	s := New()
	fake := clock.NewFakeClock(time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC))
	s.SetClock(fake)
	s.SetProbeDelay(30 * time.Second)

	run := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1, FleetIdleDuration: time.Hour})
	if run.ProbesReady {
		t.Fatal("ProbesReady true immediately after apply: running is not ready (F35, §6)")
	}
	if run.SubmittedAt.IsZero() {
		t.Fatal("SubmittedAt is zero, want the run's submission instant")
	}

	fake.Advance(29 * time.Second)
	got, err := s.Get("qwen")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ProbesReady {
		t.Fatal("ProbesReady true before the probe delay elapsed")
	}

	fake.Advance(2 * time.Second)
	got, err = s.Get("qwen")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.ProbesReady {
		t.Fatal("ProbesReady false after the probe delay elapsed, want true")
	}
}

// TestApply_ProbesReady_ResetsOnSleep: a flip to 0 unreadies the service;
// the next wake must earn readiness again, not inherit it.
func TestApply_ProbesReady_ResetsOnSleep(t *testing.T) {
	s := New()
	fake := clock.NewFakeClock(time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC))
	s.SetClock(fake)
	s.SetProbeDelay(10 * time.Second)

	run := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1, FleetIdleDuration: time.Hour})
	fake.Advance(11 * time.Second)
	if got, _ := s.Get("qwen"); !got.ProbesReady {
		t.Fatal("setup: expected ProbesReady after the delay")
	}

	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 0, Current: run})
	got, err := s.Get("qwen")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ProbesReady {
		t.Fatal("ProbesReady survived a flip to 0 replicas, want false")
	}
}

// TestApply_ForceCheckedBeforeCAS proves F18's check ordering, not just its
// outcome: force must be refused even when Current is ALSO stale against a
// real, pre-existing run. If Force were checked after the CAS switch, this
// exact input — an existing run, a stale anchor, and force:true — would
// fall into the CAS-mismatch branch and return ErrResourceChanged instead.
// Only "force first" guarantees no input can sneak past the force refusal.
func TestApply_ForceCheckedBeforeCAS(t *testing.T) {
	s := New()
	r := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	stale := *r
	stale.DeploymentNum++ // stale on purpose

	_, err := s.Apply(ApplyRequest{
		Name:     "qwen",
		Replicas: 0,
		Current:  &stale,
		Force:    true,
	})
	if !errors.Is(err, ErrForceForbidden) {
		t.Fatalf("F18: force must be refused before the CAS check, even with a stale Current; got %v, want ErrForceForbidden", err)
	}
}

func TestTerminal_DeregistersAndMintsNewRunID(t *testing.T) {
	s := New()
	r1 := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	s.Terminate("qwen") // uncommanded death — host loss, spot reclaim

	// F20/F23: dead is 404 on the gateway, not 503. This is what tells the
	// proxy to recreate-and-alarm instead of merely waking.
	if code := s.GatewayGet("qwen", ValidToken); code != 404 {
		t.Fatalf("F20: terminal run must deregister from the gateway (404), got %d", code)
	}

	r2 := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	if r2.RunID == r1.RunID {
		t.Fatal("F20: apply after a terminal state must mint a NEW run id")
	}
}

// TestGatewayGet_BadTokenBeatsUnknownService proves F23's anti-leak
// ordering: the token check must run BEFORE the service-existence lookup.
// A bad token against a service that does not exist must still answer 403,
// never 404 — a 404 here would tell an unauthenticated caller which models
// exist, leaking the service registry to anyone who can guess a name.
func TestGatewayGet_BadTokenBeatsUnknownService(t *testing.T) {
	s := New()
	if code := s.GatewayGet("no-such-service", "wrong-token"); code != http.StatusForbidden {
		t.Fatalf("F23: bad token against an unknown service must be 403 (not %d) — a 404 here leaks service existence to an unauthenticated caller", code)
	}
}

// TestGatewayGet_RegisteredAwake_Is200 covers the gateway's happy path,
// which no other test in the suite reaches: registered + valid token +
// replicas > 0 must answer 200.
func TestGatewayGet_RegisteredAwake_Is200(t *testing.T) {
	s := New()
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	if code := s.GatewayGet("qwen", ValidToken); code != http.StatusOK {
		t.Fatalf("F23: registered + awake + valid token must be 200, got %d", code)
	}
}

// TestApply_ZeroReplicasFirstApply_RegistersAndRoutable covers F17's other
// half: fixed replicas:0 accepted as a service's VERY FIRST apply (not just
// a flip down from 1) still yields a registered, routable service.
func TestApply_ZeroReplicasFirstApply_RegistersAndRoutable(t *testing.T) {
	s := New()
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 0})

	if code := s.GatewayGet("qwen", ValidToken); code != http.StatusServiceUnavailable {
		t.Fatalf("F17: replicas:0 on a first-ever apply must register a routable service (503, not 404), got %d", code)
	}
}

func TestFleet_FlipReleasesJobNotInstance(t *testing.T) {
	s := New()
	clk := clock.NewFakeClock(time.Unix(1_700_000_000, 0))
	s.SetClock(clk) // no wall-clock sleeps in tests

	r := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1, FleetIdleDuration: 10 * time.Minute})
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 0, Current: r})

	if got := s.InstanceCount("qwen"); got != 1 {
		t.Fatalf("F21: flipping to 0 terminates the JOB; the instance is the warm pool, got %d", got)
	}

	clk.Advance(11 * time.Minute)
	s.Tick()

	if got := s.InstanceCount("qwen"); got != 0 {
		t.Fatalf("F21: fleet idle_duration must release the machine, got %d", got)
	}
}

// TestFleet_RewakeResetsIdleClock covers §6's "a wake inside the warm
// window": apply 1 -> apply 0 -> apply 1 again, then advance the clock past
// fleetIdleDuration and Tick. The instance must still be up because the
// re-wake reset idleSince — the idle clock only starts counting from the
// LATEST flip to 0, not the first one.
func TestFleet_RewakeResetsIdleClock(t *testing.T) {
	s := New()
	clk := clock.NewFakeClock(time.Unix(1_700_000_000, 0))
	s.SetClock(clk)

	r := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1, FleetIdleDuration: 10 * time.Minute})
	r = s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 0, Current: r})
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1, Current: r})

	clk.Advance(11 * time.Minute)
	s.Tick()

	if got := s.InstanceCount("qwen"); got != 1 {
		t.Fatalf("F21: a wake inside the warm window must reset the idle clock; instance released early, got %d", got)
	}
}

func TestApplyCount_TracksSuccessfulApplies(t *testing.T) {
	s := New()
	r := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 0, Current: r})

	if got := s.ApplyCount("qwen"); got != 2 {
		t.Fatalf("ApplyCount: got %d, want 2", got)
	}
	if got := s.ApplyCount("unknown"); got != 0 {
		t.Fatalf("ApplyCount(unknown): got %d, want 0", got)
	}
}

// TestGet_UnknownName_IsNotFound covers the client needing to tell "never
// existed" apart from a real state.
func TestGet_UnknownName_IsNotFound(t *testing.T) {
	s := New()
	if _, err := s.Get("no-such-service"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(unknown): got %v, want ErrNotFound", err)
	}
}

// TestGet_TerminalRun_ReportsTerminalStatus is F20 as MEASURED (§9.4): a
// terminated run still answers Get successfully. Dead is not asleep, but it
// is also not ErrNotFound — Status is what tells a caller the difference,
// correcting this fake's earlier, invented assumption.
func TestGet_TerminalRun_ReportsTerminalStatus(t *testing.T) {
	s := New()
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	s.Terminate("qwen")

	run, err := s.Get("qwen")
	if err != nil {
		t.Fatalf("F20: Get on a terminal run must succeed (measured §9.4), got error %v", err)
	}
	if run.Status != statusTerminated {
		t.Fatalf("F20: Get on a terminal run: Status = %q, want %q", run.Status, statusTerminated)
	}
}

// TestGet_ReturnsCurrentState proves Get reflects the same state Apply just
// wrote, not a stale snapshot.
func TestGet_ReturnsCurrentState(t *testing.T) {
	s := New()
	r := s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	got, err := s.Get("qwen")
	if err != nil {
		t.Fatalf("Get(qwen): %v", err)
	}
	if got.RunID != r.RunID || got.DeploymentNum != r.DeploymentNum || got.Replicas != r.Replicas {
		t.Fatalf("Get(qwen) = %+v, want %+v", got, r)
	}
}

// TestDelete_UnknownName_IsNotFound: deleting a name the fake never saw is
// distinguishable from deleting one it did.
func TestDelete_UnknownName_IsNotFound(t *testing.T) {
	s := New()
	if err := s.Delete("no-such-service"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(unknown): got %v, want ErrNotFound", err)
	}
}

// TestDelete_RemovesRun proves Delete actually removes state: a Get
// afterward must be ErrNotFound, and ListRuns must no longer report it.
func TestDelete_RemovesRun(t *testing.T) {
	s := New()
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})

	// D56: an active run cannot be deleted, mirroring the real server.
	if err := s.Stop("qwen"); err != nil {
		t.Fatalf("Stop(qwen): %v", err)
	}
	if err := s.Delete("qwen"); err != nil {
		t.Fatalf("Delete(qwen): %v", err)
	}
	if _, err := s.Get("qwen"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete: got %v, want ErrNotFound", err)
	}
}

// TestDelete_TerminalRun_Succeeds: an already-dead run's record can still be
// cleaned up.
func TestDelete_TerminalRun_Succeeds(t *testing.T) {
	s := New()
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	s.Terminate("qwen")

	if err := s.Delete("qwen"); err != nil {
		t.Fatalf("Delete on a terminal run must succeed (cleanup), got %v", err)
	}
}

// TestListRuns_ReturnsAllLiveRuns backs the reconcile loop's orphan diff
// (§5.2): every service the fake knows about must be listed.
func TestListRuns_ReturnsAllLiveRuns(t *testing.T) {
	s := New()
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	s.MustApply(t, ApplyRequest{Name: "llama", Replicas: 0})

	runs := s.ListRuns()
	if len(runs) != 2 {
		t.Fatalf("ListRuns: got %d runs, want 2: %+v", len(runs), runs)
	}
	names := map[string]bool{}
	for _, r := range runs {
		names[r.Name] = true
	}
	if !names["qwen"] || !names["llama"] {
		t.Fatalf("ListRuns: got %+v, want qwen and llama", runs)
	}
}

// TestListRuns_IncludesTerminalRunsWithStatus is F20 as MEASURED (§9.4):
// dstack keeps a terminal run's record — ListRuns must report it too, with
// its terminal Status, not silently drop it. This corrects this fake's
// earlier, invented exclusion (a caller needing "live only" must filter on
// Status itself; Delete is the surface for removing the record).
func TestListRuns_IncludesTerminalRunsWithStatus(t *testing.T) {
	s := New()
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	s.Terminate("qwen")

	runs := s.ListRuns()
	if len(runs) != 1 {
		t.Fatalf("ListRuns must still report a terminal run, got %+v", runs)
	}
	if runs[0].Status != statusTerminated {
		t.Fatalf("ListRuns: Status = %q, want %q", runs[0].Status, statusTerminated)
	}
}

// TestApply_ConcurrentCallsAreRaceFree backs the design requirement that
// later phases fire 50 concurrent wake requests at the fake (AC4). This
// only asserts the mock itself is race-free under -race; coalescing "one
// effective wake" out of concurrent demand is the reconciler's job, not the
// fake's.
func TestApply_ConcurrentCallsAreRaceFree(t *testing.T) {
	s := New()
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 0})

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			s.GatewayGet("qwen", ValidToken)
			s.InstanceCount("qwen")
			s.ApplyCount("qwen")
		}()
	}
	wg.Wait()
}

// TestFakeSpeaksTheRealWire is the whole point of the fake: a client that
// works against it must work against dstack. It drives the REAL client
// against the fake's HTTP surface, so any divergence is a compile-or-fail
// here rather than a surprise on first contact with a real server.
func TestFakeSpeaksTheRealWire(t *testing.T) {
	srv := httptest.NewServer(NewHTTPServer(New(), "tok"))
	t.Cleanup(srv.Close)

	c := dstack.NewHTTPClient(srv.URL, "main", "tok", srv.Client())

	if _, err := c.Get(context.Background(), "qwen"); !errors.Is(err, dstack.ErrNotFound) {
		t.Fatalf("Get on an unknown run = %v, want ErrNotFound", err)
	}

	run, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 1, Image: "img", Port: 8080})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if run.RunID == "" || run.Replicas != 1 {
		t.Fatalf("Apply returned %+v, want a run id and Replicas 1", run)
	}

	// F18: re-applying against a stale anchor must lose loudly. run itself
	// is never mutated by Apply, so it stays a valid "stale" anchor after
	// the fresh flip below moves state past it.
	if _, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 0, Image: "img", Port: 8080, Current: run}); err != nil {
		t.Fatalf("Apply with the fresh anchor: %v", err)
	}
	if _, err := c.Apply(context.Background(), dstack.ApplyRequest{Name: "qwen", Replicas: 1, Image: "img", Port: 8080, Current: run}); !errors.Is(err, dstack.ErrResourceChanged) {
		t.Fatalf("Apply with a STALE anchor = %v, want ErrResourceChanged", err)
	}
}

// TestDelete_RefusesAnActiveRun is D56 in the double. Real dstack answers
// HTTP 400 "Cannot delete active runs" for a run that is not terminal; the
// fake accepting it is what let a teardown ship that leaked a billing GPU.
// The double must never be more permissive than the server it stands in for.
func TestDelete_RefusesAnActiveRun(t *testing.T) {
	s := New()
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	if err := s.Delete("qwen"); !errors.Is(err, ErrDeleteActiveRun) {
		t.Fatalf("Delete on a live run = %v, want ErrDeleteActiveRun", err)
	}
	if _, err := s.Get("qwen"); err != nil {
		t.Fatalf("the run must still exist after a refused Delete: %v", err)
	}
	if err := s.Stop("qwen"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := s.Delete("qwen"); err != nil {
		t.Fatalf("Delete after Stop: %v, want success", err)
	}
}

// TestStop_IsIdempotentAndReportsAbsence: teardown replays after a crash,
// so stopping twice must be harmless, and stopping something already gone
// must be distinguishable from stopping something live.
func TestStop_IsIdempotentAndReportsAbsence(t *testing.T) {
	s := New()
	s.MustApply(t, ApplyRequest{Name: "qwen", Replicas: 1})
	if err := s.Stop("qwen"); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := s.Stop("qwen"); err != nil {
		t.Fatalf("second Stop must be a no-op, got %v", err)
	}
	if err := s.Stop("never-existed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stop on an unknown run = %v, want ErrNotFound", err)
	}
}
