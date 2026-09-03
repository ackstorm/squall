// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

func TestSleepDue_UsesTheDurableAnchorWhenNoReplicaHasData(t *testing.T) {
	now := time.Now()
	ev := &ActivityEvidence{Complete: true, AllIdle: true}
	stale := metav1.NewTime(now.Add(-10 * time.Minute))
	if !sleepDue(ev, &stale, 120, now) {
		t.Fatal("stale durable anchor must permit sleep")
	}
	fresh := metav1.NewTime(now.Add(-30 * time.Second))
	if sleepDue(ev, &fresh, 120, now) {
		t.Fatal("fresh durable anchor must block sleep")
	}
	if sleepDue(ev, nil, 120, now) {
		t.Fatal("missing durable anchor must block sleep")
	}
}

// TestSleepDue_WakeSeedsTheAnchor is finding #1. A Model woke, reached Ready
// on dstack's own probe, and no request was ever forwarded to it — the client
// that caused the wake gave up first. Without a seeded anchor sleepDue returns
// false forever while the evidence is complete.
func TestSleepDue_WakeSeedsTheAnchor(t *testing.T) {
	now := time.Now()
	ev := &ActivityEvidence{Complete: true, AllIdle: true, AnyData: false}
	woke := metav1.NewTime(now.Add(-10 * time.Minute))

	if !sleepDue(ev, &woke, 120, now) {
		t.Fatal("woke 10 minutes ago, never served a request: must sleep, not bill forever")
	}
}

func TestSleepDue_LiveDataWinsOverTheDurableAnchor(t *testing.T) {
	now := time.Now()
	ev := &ActivityEvidence{Complete: true, AllIdle: true, AnyData: true, NewestLastRequestAt: now.Add(-10 * time.Second)}
	ancient := metav1.NewTime(now.Add(-3 * time.Hour))
	if sleepDue(ev, &ancient, 120, now) {
		t.Fatal("live data must win over durable anchor")
	}
}

func TestSleepDue_IncompleteEvidenceNeverSleeps(t *testing.T) {
	now := time.Now()
	ancient := metav1.NewTime(now.Add(-3 * time.Hour))
	if sleepDue(&ActivityEvidence{}, &ancient, 120, now) {
		t.Fatal("incomplete evidence must block sleep")
	}
}

func TestUncontrolledTimeoutFor_Default(t *testing.T) {
	for _, tc := range []struct {
		delay int32
		want  time.Duration
	}{{120, 23 * time.Minute}, {900, 75 * time.Minute}, {1575, 2 * time.Hour}, {3600, 2 * time.Hour}} {
		if got := uncontrolledTimeoutFor(squallv1alpha1.ModelSpec{ScaleDownDelaySeconds: tc.delay}); got != tc.want {
			t.Fatalf("delay %d: got %v want %v", tc.delay, got, tc.want)
		}
	}
}

func TestUncontrolledTimeoutFor_ExplicitZeroFallsBackToDefault(t *testing.T) {
	zero := metav1.Duration{}
	if got := uncontrolledTimeoutFor(squallv1alpha1.ModelSpec{ScaleDownDelaySeconds: 120, UncontrolledTimeout: &zero}); got != 23*time.Minute {
		t.Fatalf("got %v", got)
	}
}

func TestUncontrolledTimeoutFor_ExplicitValueIsCappedAt24h(t *testing.T) {
	week := metav1.Duration{Duration: 7 * 24 * time.Hour}
	if got := uncontrolledTimeoutFor(squallv1alpha1.ModelSpec{UncontrolledTimeout: &week}); got != MaxExplicitUncontrolledTimeout {
		t.Fatalf("got %v", got)
	}
}

func TestUncontrolledDue(t *testing.T) {
	now := time.Now()
	old := metav1.NewTime(now.Add(-30 * time.Minute))
	recent := metav1.NewTime(now.Add(-5 * time.Minute))
	if !uncontrolledDue(&old, 23*time.Minute, false, now) || uncontrolledDue(&recent, 23*time.Minute, false, now) || uncontrolledDue(nil, 23*time.Minute, false, now) || uncontrolledDue(&old, 0, false, now) {
		t.Fatal("unexpected uncontrolled deadline result")
	}
}

func TestUncontrolledDue_FreshDemandResetsClock(t *testing.T) {
	now := time.Now()
	old := metav1.NewTime(now.Add(-30 * time.Minute))
	if uncontrolledDue(&old, 23*time.Minute, true, now) {
		t.Fatal("fresh demand must reset the uncontrolled deadline")
	}
	if !uncontrolledDue(&old, 23*time.Minute, false, now) {
		t.Fatal("expired demand must allow the uncontrolled deadline to fire")
	}
}

func TestProvisioningDue(t *testing.T) {
	now := time.Now()
	old := metav1.NewTime(now.Add(-25 * time.Minute))
	recent := metav1.NewTime(now.Add(-5 * time.Minute))
	if !provisioningDue(&old, false, 20*time.Minute, now) || provisioningDue(&recent, false, 20*time.Minute, now) || provisioningDue(&old, true, 20*time.Minute, now) || provisioningDue(nil, false, 20*time.Minute, now) || provisioningDue(&old, false, 0, now) {
		t.Fatal("unexpected provisioning deadline result")
	}
}

func TestDecide_ProvisioningTimeoutMarksDeadNotAsleep(t *testing.T) {
	now := time.Now()
	woke := metav1.NewTime(now.Add(-30 * time.Minute))
	phase, action := Decide(
		Observed{Run: &dstack.Run{Name: "squall-qwen", Replicas: 1, Status: "provisioning"}},
		squallv1alpha1.ModelStatus{WakeStartedAt: &woke, RunID: "r1"},
		squallv1alpha1.ModelSpec{MinReplicas: 0, ProvisioningTimeout: metav1.Duration{Duration: 20 * time.Minute}},
		true, now)
	if phase != squallv1alpha1.ModelPhaseDead {
		t.Fatalf("a wake that never landed is Dead, not %s", phase)
	}
	if !action.Apply || action.Replicas != 0 || !action.Alarm || !action.ProvisioningTimedOut {
		t.Fatalf("must destroy and alarm, got %+v", action)
	}
}

// TestDecide is the table test for phase.go's pure state machine (Task
// 6.1). Every row is a §5.2/§6 transition; the two the plan calls out by
// name (Asleep→flip and Dead→recreate+alarm) are here, but so is every
// other reachable combination of (observed dstack state, prior status,
// demand) the wake-only ("0→1, fails open") half of the flip can see.
// Written first, red, before phase.go existed (Task 6.1's instruction).
func TestDecide(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	onDemandSpec := squallv1alpha1.ModelSpec{MinReplicas: 0}
	pinnedSpec := squallv1alpha1.ModelSpec{MinReplicas: 1}

	tests := []struct {
		name string

		observed  Observed
		prior     squallv1alpha1.ModelStatus
		spec      squallv1alpha1.ModelSpec
		hasDemand bool

		wantPhase    squallv1alpha1.ModelPhase
		wantApply    bool
		wantReplicas int
		wantAlarm    bool
	}{
		{
			// Never-applied, no demand, not pinned: the CR exists but
			// nobody has ever asked for it. Nothing to actuate.
			name:      "cold, no demand, on-demand -> stays Asleep, no-op",
			observed:  Observed{},
			prior:     squallv1alpha1.ModelStatus{},
			spec:      onDemandSpec,
			hasDemand: false,
			wantPhase: squallv1alpha1.ModelPhaseAsleep,
			wantApply: false,
			wantAlarm: false,
		},
		{
			// The scenario Task 6.2 drives end-to-end: a brand new CR
			// gets its first demand patch. Never applied before (no prior
			// RunID) — this is a plain first wake, not a recreate, and
			// must not alarm (nothing died).
			name:         "cold, demand arrives -> Waking, apply, no alarm",
			observed:     Observed{},
			prior:        squallv1alpha1.ModelStatus{},
			spec:         onDemandSpec,
			hasDemand:    true,
			wantPhase:    squallv1alpha1.ModelPhaseWaking,
			wantApply:    true,
			wantReplicas: 1,
			wantAlarm:    false,
		},
		{
			// F17/F23: registered, replicas 0, gateway would answer 503.
			// No demand, not pinned -> stay put.
			name: "Asleep, no demand, on-demand -> stays Asleep, no-op",
			observed: Observed{
				Run: &dstack.Run{Name: "qwen", RunID: "run-1", DeploymentNum: 3, Replicas: 0},
			},
			prior:     squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseAsleep, RunID: "run-1"},
			spec:      onDemandSpec,
			hasDemand: false,
			wantPhase: squallv1alpha1.ModelPhaseAsleep,
			wantApply: false,
			wantAlarm: false,
		},
		{
			// The plan's named example: Asleep -> flip. In-place apply
			// (F17), CAS'd on the observed DeploymentNum (F18) — never a
			// new run, never an alarm.
			name: "Asleep -> flip: demand wakes the existing run in place",
			observed: Observed{
				Run: &dstack.Run{Name: "qwen", RunID: "run-1", DeploymentNum: 3, Replicas: 0},
			},
			prior:        squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseAsleep, RunID: "run-1"},
			spec:         onDemandSpec,
			hasDemand:    true,
			wantPhase:    squallv1alpha1.ModelPhaseWaking,
			wantApply:    true,
			wantReplicas: 1,
			wantAlarm:    false,
		},
		{
			// Pinning alone (minReplicas: 1) must wake it, no proxy
			// demand needed — AC17's "pinned never sleeps" starts here.
			name: "Asleep, pinned, no demand -> still flips",
			observed: Observed{
				Run: &dstack.Run{Name: "qwen", RunID: "run-1", DeploymentNum: 1, Replicas: 0},
			},
			prior:        squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseAsleep, RunID: "run-1"},
			spec:         pinnedSpec,
			hasDemand:    false,
			wantPhase:    squallv1alpha1.ModelPhaseWaking,
			wantApply:    true,
			wantReplicas: 1,
			wantAlarm:    false,
		},
		{
			// The plan's other named example: Dead -> recreate + alarm.
			// prior.RunID != "" but dstack no longer knows the run (F20):
			// this is an uncommanded death, materially different from a
			// cold first-ever wake even though both mint a fresh run.
			name:         "Dead -> recreate + alarm: a tracked run vanished and demand wants it back",
			observed:     Observed{},
			prior:        squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"},
			spec:         onDemandSpec,
			hasDemand:    true,
			wantPhase:    squallv1alpha1.ModelPhaseRecreating,
			wantApply:    true,
			wantReplicas: 1,
			wantAlarm:    true,
		},
		{
			// Dead with nobody currently asking for it: alarm (an
			// operator should still be told something died), but no
			// costly recreate for a model nobody wants right now.
			name:      "Dead, no demand -> alarm only, no recreate",
			observed:  Observed{},
			prior:     squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"},
			spec:      onDemandSpec,
			hasDemand: false,
			wantPhase: squallv1alpha1.ModelPhaseDead,
			wantApply: false,
			wantAlarm: true,
		},
		{
			// A pinned model that dies must be recreated even without a
			// fresh proxy demand patch — pinning IS the demand.
			name:         "Dead, pinned, no proxy demand -> still recreates + alarms",
			observed:     Observed{},
			prior:        squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"},
			spec:         pinnedSpec,
			hasDemand:    false,
			wantPhase:    squallv1alpha1.ModelPhaseRecreating,
			wantApply:    true,
			wantReplicas: 1,
			wantAlarm:    true,
		},
		{
			// Level-trigger no-op: dstack already reports the run up and
			// engine-health evidence says Ready. Repeated demand must not
			// re-apply (this is the core of AC4's coalescing at the
			// state-machine level, independent of the workqueue).
			name: "already Ready -> no-op even with demand",
			observed: Observed{
				Run:   &dstack.Run{Name: "qwen", RunID: "run-1", DeploymentNum: 4, Replicas: 1},
				Ready: true,
			},
			prior:     squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"},
			spec:      onDemandSpec,
			hasDemand: true,
			wantPhase: squallv1alpha1.ModelPhaseReady,
			wantApply: false,
			wantAlarm: false,
		},
		{
			// Replicas already 1 but no readiness evidence yet, and we
			// got here via a plain wake (prior phase Waking, not
			// Recreating): stays Waking, no-op.
			name: "replicas up, not yet Ready, prior Waking -> stays Waking, no-op",
			observed: Observed{
				Run:   &dstack.Run{Name: "qwen", RunID: "run-1", DeploymentNum: 4, Replicas: 1},
				Ready: false,
			},
			prior:     squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseWaking, RunID: "run-1"},
			spec:      onDemandSpec,
			hasDemand: true,
			wantPhase: squallv1alpha1.ModelPhaseWaking,
			wantApply: false,
			wantAlarm: false,
		},
		{
			// The same "up but not ready" observation, reached via a
			// recreate instead: the Recreating label must survive until
			// Ready, not silently collapse into Waking (an operator
			// reading status should still see this came from a death).
			name: "replicas up, not yet Ready, prior Recreating -> stays Recreating, no-op",
			observed: Observed{
				Run:   &dstack.Run{Name: "qwen", RunID: "run-2", DeploymentNum: 1, Replicas: 1},
				Ready: false,
			},
			prior:     squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseRecreating, RunID: "run-1"},
			spec:      onDemandSpec,
			hasDemand: true,
			wantPhase: squallv1alpha1.ModelPhaseRecreating,
			wantApply: false,
			wantAlarm: false,
		},
		{
			// Fail-safe direction is out of scope here (Phase 7): even
			// with no demand and not pinned, Decide never turns a running
			// model off by itself.
			name: "replicas up, no demand -> still no-op, never flips 1->0 here",
			observed: Observed{
				Run:   &dstack.Run{Name: "qwen", RunID: "run-1", DeploymentNum: 4, Replicas: 1},
				Ready: true,
			},
			prior:     squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"},
			spec:      onDemandSpec,
			hasDemand: false,
			wantPhase: squallv1alpha1.ModelPhaseReady,
			wantApply: false,
			wantAlarm: false,
		},
		{
			// T7: pinned models never sleep, even with a clean idle
			// window that would otherwise flip an on-demand model.
			// Mutation: delete the spec.MinReplicas == 0 gate.
			name: "pinned, replicas up, clean idle window past scaleDownDelaySeconds -> stays Ready, no sleep",
			observed: Observed{
				Run:   &dstack.Run{Name: "qwen", RunID: "run-1", DeploymentNum: 4, Replicas: 1},
				Ready: true,
				Activity: &ActivityEvidence{
					Complete:            true,
					AllIdle:             true,
					NewestLastRequestAt: now.Add(-time.Hour),
				},
			},
			prior: squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"},
			spec: squallv1alpha1.ModelSpec{
				MinReplicas:           1,
				ScaleDownDelaySeconds: 300,
			},
			hasDemand: false,
			wantPhase: squallv1alpha1.ModelPhaseReady,
			wantApply: false,
			wantAlarm: false,
		},
		{
			// T1: clean, complete, idle evidence aged past
			// scaleDownDelaySeconds flips an on-demand model to Asleep,
			// in place (CAS'd on the observed DeploymentNum, same as the
			// wake flip). hasDemand plays no part in this gate.
			name: "on-demand, replicas up, clean idle window past scaleDownDelaySeconds -> flips to Asleep",
			observed: Observed{
				Run:   &dstack.Run{Name: "qwen", RunID: "run-1", DeploymentNum: 4, Replicas: 1},
				Ready: true,
				Activity: &ActivityEvidence{
					Complete:            true,
					AllIdle:             true,
					NewestLastRequestAt: now.Add(-301 * time.Second),
				},
			},
			prior:        squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"},
			spec:         squallv1alpha1.ModelSpec{MinReplicas: 0, ScaleDownDelaySeconds: 300},
			hasDemand:    true,
			wantPhase:    squallv1alpha1.ModelPhaseAsleep,
			wantApply:    true,
			wantReplicas: 0,
			wantAlarm:    false,
		},
		{
			// T1's boundary: exactly at scaleDownDelaySeconds (not yet
			// strictly older) must not flip.
			name: "on-demand, idle exactly at scaleDownDelaySeconds boundary -> no flip yet",
			observed: Observed{
				Run:   &dstack.Run{Name: "qwen", RunID: "run-1", DeploymentNum: 4, Replicas: 1},
				Ready: true,
				Activity: &ActivityEvidence{
					Complete:            true,
					AllIdle:             true,
					NewestLastRequestAt: now.Add(-300 * time.Second),
				},
			},
			prior:     squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"},
			spec:      squallv1alpha1.ModelSpec{MinReplicas: 0, ScaleDownDelaySeconds: 300},
			hasDemand: false,
			wantPhase: squallv1alpha1.ModelPhaseReady,
			wantApply: false,
			wantAlarm: false,
		},
		{
			// Complete and idle, but not yet aged past the delay -> stay
			// awake.
			name: "on-demand, complete+idle but not yet aged -> no flip",
			observed: Observed{
				Run:   &dstack.Run{Name: "qwen", RunID: "run-1", DeploymentNum: 4, Replicas: 1},
				Ready: true,
				Activity: &ActivityEvidence{
					Complete:            true,
					AllIdle:             true,
					NewestLastRequestAt: now.Add(-10 * time.Second),
				},
			},
			prior:     squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"},
			spec:      squallv1alpha1.ModelSpec{MinReplicas: 0, ScaleDownDelaySeconds: 300},
			hasDemand: false,
			wantPhase: squallv1alpha1.ModelPhaseReady,
			wantApply: false,
			wantAlarm: false,
		},
		{
			// Fail-safe: an unevaluated (nil) Activity must never be
			// read as idle, no matter how old the window logically is.
			name: "on-demand, activity not yet evaluated (nil) -> no flip",
			observed: Observed{
				Run:      &dstack.Run{Name: "qwen", RunID: "run-1", DeploymentNum: 4, Replicas: 1},
				Ready:    true,
				Activity: nil,
			},
			prior:     squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"},
			spec:      squallv1alpha1.ModelSpec{MinReplicas: 0, ScaleDownDelaySeconds: 300},
			hasDemand: false,
			wantPhase: squallv1alpha1.ModelPhaseReady,
			wantApply: false,
			wantAlarm: false,
		},
		{
			// Fail-safe: incomplete evidence (T3/T4's outcome one layer
			// up) must never flip, however old the last known request.
			name: "on-demand, activity incomplete -> no flip",
			observed: Observed{
				Run:   &dstack.Run{Name: "qwen", RunID: "run-1", DeploymentNum: 4, Replicas: 1},
				Ready: true,
				Activity: &ActivityEvidence{
					Complete: false,
				},
			},
			prior:     squallv1alpha1.ModelStatus{Phase: squallv1alpha1.ModelPhaseReady, RunID: "run-1"},
			spec:      squallv1alpha1.ModelSpec{MinReplicas: 0, ScaleDownDelaySeconds: 300},
			hasDemand: false,
			wantPhase: squallv1alpha1.ModelPhaseReady,
			wantApply: false,
			wantAlarm: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPhase, gotAction := Decide(tt.observed, tt.prior, tt.spec, tt.hasDemand, now)

			if gotPhase != tt.wantPhase {
				t.Errorf("phase = %q, want %q", gotPhase, tt.wantPhase)
			}
			if gotAction.Apply != tt.wantApply {
				t.Errorf("Action.Apply = %v, want %v", gotAction.Apply, tt.wantApply)
			}
			// Current is always exactly Observed.Run: dstack's real CAS
			// compares the whole previous object (F18), so Decide never
			// synthesises this anchor — it hands back what it was given,
			// or nil when there is nothing live to CAS against.
			if tt.wantApply && gotAction.Current != tt.observed.Run {
				t.Errorf("Action.Current = %+v, want %+v (must be exactly Observed.Run)", gotAction.Current, tt.observed.Run)
			}
			if tt.wantApply && gotAction.Replicas != tt.wantReplicas {
				t.Errorf("Action.Replicas = %d, want %d", gotAction.Replicas, tt.wantReplicas)
			}
			if gotAction.Alarm != tt.wantAlarm {
				t.Errorf("Action.Alarm = %v, want %v", gotAction.Alarm, tt.wantAlarm)
			}
			if !gotAction.At.Equal(now) {
				t.Errorf("Action.At = %v, want %v (must thread the caller's now, never read a clock)", gotAction.At, now)
			}
		})
	}
}

// unhealthyEvidence builds complete evidence with 5 failures — comfortably over
// the default threshold — so these cases exercise the TIME half. The evidence
// floor gets its own cases at the bottom of the table.
func unhealthyEvidence(lastReq, lastOK time.Time) *ActivityEvidence {
	return &ActivityEvidence{
		Complete: true, NewestLastRequestAt: lastReq,
		NewestLastSuccessAt: lastOK, FailuresSinceSuccess: 5,
	}
}

func withFailures(lastReq, lastOK time.Time, n int) *ActivityEvidence {
	e := unhealthyEvidence(lastReq, lastOK)
	e.FailuresSinceSuccess = n
	return e
}

func ptrTime(t time.Time) *metav1.Time { m := metav1.NewTime(t); return &m }

// TestUnhealthyDue is the pure table for the 2026-08-31 liveness verdict: a
// replica taking traffic and delivering nothing must be torn down, and
// everything else must not.
func TestUnhealthyDue(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	woke := metav1.NewTime(now.Add(-2 * time.Hour))
	const scaleDown int32 = 300
	const after = 15 * time.Minute

	tests := []struct {
		name      string
		activity  *ActivityEvidence
		ready     bool
		woke      *metav1.Time
		after     time.Duration
		threshold int32
		want      bool
	}{
		{
			// THE CASE THIS EXISTS FOR: requests still arriving, last delivered
			// 2xx is 20 minutes old, plenty of failures behind it.
			name:     "traffic arriving, no success for 20m -> unhealthy",
			activity: unhealthyEvidence(now.Add(-10*time.Second), now.Add(-20*time.Minute)),
			ready:    true, woke: &woke, after: after, threshold: 3, want: true,
		},
		{
			name:     "successes are recent -> healthy",
			activity: unhealthyEvidence(now.Add(-10*time.Second), now.Add(-2*time.Second)),
			ready:    true, woke: &woke, after: after, threshold: 3, want: false,
		},
		{
			name:     "no success for 20m but no traffic either -> idle, not unhealthy",
			activity: unhealthyEvidence(now.Add(-20*time.Minute), now.Add(-20*time.Minute)),
			ready:    true, woke: &woke, after: after, threshold: 3, want: false,
		},
		{
			// Anchored at wakeStartedAt: a run that woke a minute ago and has
			// never served anything is not unhealthy, it is new.
			name:     "never succeeded but only just woke -> not unhealthy",
			activity: unhealthyEvidence(now.Add(-1*time.Second), time.Time{}),
			ready:    true, woke: ptrTime(now.Add(-time.Minute)), after: after, threshold: 3, want: false,
		},
		{
			name:     "never succeeded and woke 20m ago -> unhealthy",
			activity: unhealthyEvidence(now.Add(-1*time.Second), time.Time{}),
			ready:    true, woke: ptrTime(now.Add(-20 * time.Minute)), after: after, threshold: 3, want: true,
		},
		{
			// Provisioning owns the pre-Ready window, not this check.
			name:     "not ready yet -> never unhealthy",
			activity: unhealthyEvidence(now.Add(-1*time.Second), time.Time{}),
			ready:    false, woke: ptrTime(now.Add(-20 * time.Minute)), after: after, threshold: 3, want: false,
		},
		{
			name:     "incomplete evidence -> no verdict",
			activity: &ActivityEvidence{Complete: false},
			ready:    true, woke: &woke, after: after, threshold: 3, want: false,
		},
		{
			name:     "nil evidence -> no verdict",
			activity: nil, ready: true, woke: &woke, after: after, threshold: 3, want: false,
		},
		{
			name:     "disabled by zero unhealthyAfter",
			activity: unhealthyEvidence(now.Add(-10*time.Second), now.Add(-20*time.Minute)),
			ready:    true, woke: &woke, after: 0, threshold: 3, want: false,
		},
		{
			// No anchor at all: never succeeded AND never journalled a wake.
			// 1->0 fails safe, so no anchor means no verdict.
			name:     "no success and no wakeStartedAt -> no verdict",
			activity: unhealthyEvidence(now.Add(-1*time.Second), time.Time{}),
			ready:    true, woke: nil, after: after, threshold: 3, want: false,
		},
		{
			// THE THIN-EVIDENCE CASE the threshold exists for: served fine,
			// went quiet for 20 minutes, then ONE request failed. Time says
			// yes; the evidence floor says no, and it wins.
			name:     "20m since success but only 1 failure -> not enough evidence",
			activity: withFailures(now.Add(-1*time.Second), now.Add(-20*time.Minute), 1),
			ready:    true, woke: &woke, after: after, threshold: 3, want: false,
		},
		{
			name:     "one below the threshold -> still not enough",
			activity: withFailures(now.Add(-1*time.Second), now.Add(-20*time.Minute), 2),
			ready:    true, woke: &woke, after: after, threshold: 3, want: false,
		},
		{
			name:     "exactly at the threshold -> unhealthy",
			activity: withFailures(now.Add(-1*time.Second), now.Add(-20*time.Minute), 3),
			ready:    true, woke: &woke, after: after, threshold: 3, want: true,
		},
		{
			// Many failures but the time window has NOT elapsed: a burst of
			// errors inside a good minute is not a broken replica.
			name:     "threshold met but success is recent -> healthy",
			activity: withFailures(now.Add(-1*time.Second), now.Add(-2*time.Second), 50),
			ready:    true, woke: &woke, after: after, threshold: 3, want: false,
		},
		{
			// A non-positive threshold must read as the DEFAULT floor, never as
			// "no floor". One failure must not be a verdict just because the
			// field was left unset.
			name:     "threshold 0 falls back to the default, so 1 failure is not enough",
			activity: withFailures(now.Add(-1*time.Second), now.Add(-20*time.Minute), 1),
			ready:    true, woke: &woke, after: after, threshold: 0, want: false,
		},
		{
			name:     "threshold 0 falls back to the default, which 3 failures meet",
			activity: withFailures(now.Add(-1*time.Second), now.Add(-20*time.Minute), 3),
			ready:    true, woke: &woke, after: after, threshold: 0, want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unhealthyDue(tt.activity, tt.ready, tt.woke, tt.after, tt.threshold, scaleDown, now)
			if got != tt.want {
				t.Errorf("unhealthyDue = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDecide_UnhealthyFlipsToZeroAndReportsWhy proves the verdict reaches the
// ACTUATION, not just the predicate. Without this, a mutation that computes the
// verdict correctly and then reports Unhealthy: false leaves the whole suite
// green — the flip still happens, but the reconciler can no longer tell it
// apart from a plain idle sleep, so the Healthy condition is never written and
// an operator finding an Asleep Model has no way to know it was pushed.
// (Found exactly that way: mutation 9 of the 2026-08-31 sweep survived.)
func TestDecide_UnhealthyFlipsToZeroAndReportsWhy(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	woke := metav1.NewTime(now.Add(-time.Hour))
	run := &dstack.Run{Name: "qwen", RunID: "run-1", DeploymentNum: 3, Replicas: 1}

	observed := Observed{
		Run:   run,
		Ready: true,
		Activity: &ActivityEvidence{
			Complete:             true,
			NewestLastRequestAt:  now.Add(-time.Second),
			NewestLastSuccessAt:  now.Add(-30 * time.Minute),
			FailuresSinceSuccess: 40,
		},
	}
	prior := squallv1alpha1.ModelStatus{
		RunID: "run-1", Phase: squallv1alpha1.ModelPhaseReady, WakeStartedAt: &woke,
	}
	spec := squallv1alpha1.ModelSpec{
		MinReplicas:           0,
		ScaleDownDelaySeconds: 300,
		Health: squallv1alpha1.ModelHealth{
			UnhealthyAfter:   metav1.Duration{Duration: 15 * time.Minute},
			FailureThreshold: 3,
		},
	}

	// hasDemand is TRUE: requests are arriving. The whole point is that this
	// fires anyway — demand is not a veto on a replica that cannot serve it.
	phase, action := Decide(observed, prior, spec, true, now)

	if phase != squallv1alpha1.ModelPhaseAsleep {
		t.Errorf("phase = %q, want Asleep", phase)
	}
	if !action.Apply || action.Replicas != 0 {
		t.Errorf("action = %+v, want Apply with Replicas 0", action)
	}
	if !action.Unhealthy {
		t.Error("action.Unhealthy = false, want true so the reconciler can say WHY it slept")
	}
	if action.Current != run {
		t.Error("Current must carry the observed run so the CAS has something to compare")
	}
}

// TestDecide_IdleSleepIsNotReportedAsUnhealthy is the other half: an ordinary
// idle sleep must NOT slander the replica. Both flips actuate identically, so
// only Action.Unhealthy separates "nobody is using it" from "it is broken".
func TestDecide_IdleSleepIsNotReportedAsUnhealthy(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	woke := metav1.NewTime(now.Add(-time.Hour))
	run := &dstack.Run{Name: "qwen", RunID: "run-1", DeploymentNum: 3, Replicas: 1}

	observed := Observed{
		Run:   run,
		Ready: true,
		Activity: &ActivityEvidence{
			Complete: true, AllIdle: true,
			NewestLastRequestAt: now.Add(-30 * time.Minute),
			NewestLastSuccessAt: now.Add(-30 * time.Minute),
		},
	}
	prior := squallv1alpha1.ModelStatus{RunID: "run-1", Phase: squallv1alpha1.ModelPhaseReady, WakeStartedAt: &woke}
	spec := squallv1alpha1.ModelSpec{
		MinReplicas:           0,
		ScaleDownDelaySeconds: 300,
		Health: squallv1alpha1.ModelHealth{
			UnhealthyAfter:   metav1.Duration{Duration: 15 * time.Minute},
			FailureThreshold: 3,
		},
	}

	phase, action := Decide(observed, prior, spec, false, now)

	if phase != squallv1alpha1.ModelPhaseAsleep || !action.Apply || action.Replicas != 0 {
		t.Fatalf("phase=%q action=%+v, want an ordinary sleep", phase, action)
	}
	if action.Unhealthy {
		t.Error("action.Unhealthy = true on a plain idle sleep; nobody was asking, nothing was broken")
	}
}

// TestSleepDue_ReWakeDoesNotSleepMidWake documents sleepDue's CONTRACT: given a
// fresh anchor it declines to sleep, given a stale one it agrees. It is NOT the
// regression guard for the re-wake bug, and an earlier version of this comment
// claimed it was -- wrongly. It hand-feeds sleepDue an already-advanced anchor,
// so reverting the advance in updateActivityStatus leaves it green: it asserts
// on this function's arguments, not on what the controller writes.
//
// The real guards are TestUpdateActivityStatus_ReWakeAdvancesTheAnchor and
// TestUpdateActivityStatus_WakeNeverRewindsTheAnchor. Caught by the code-review
// agent, 2026-09-01; kept because the contract is still worth pinning, renamed
// in intent rather than deleted.
func TestSleepDue_ReWakeDoesNotSleepMidWake(t *testing.T) {
	now := time.Now()
	ev := &ActivityEvidence{Complete: true, AllIdle: true, AnyData: false}

	// The previous cycle's last request, long past the 2-minute delay.
	stale := metav1.NewTime(now.Add(-3 * time.Hour))
	if !sleepDue(ev, &stale, 120, now) {
		t.Fatal("precondition: a 3h-old anchor with no live data must otherwise sleep")
	}

	// After the wake advanced it, the same evidence must NOT sleep.
	woke := metav1.NewTime(now.Add(-5 * time.Second))
	if sleepDue(ev, &woke, 120, now) {
		t.Fatal("woke 5s ago and nothing has been forwarded yet: sleeping here " +
			"kills the wake that a held request is still waiting on")
	}
}
