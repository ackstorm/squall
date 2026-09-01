// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

// reaperClient records what the reaper actually did to dstack.
type reaperClient struct {
	dstack.Client
	runs        []dstack.Run
	listErr     error
	stopped     []string
	stopErr     error
	listRunsCtx context.Context
}

func (c *reaperClient) ListRuns(ctx context.Context) ([]dstack.Run, error) {
	c.listRunsCtx = ctx
	return c.runs, c.listErr
}

func (c *reaperClient) Stop(_ context.Context, name string) error {
	if c.stopErr != nil {
		return c.stopErr
	}
	c.stopped = append(c.stopped, name)
	return nil
}

func reaperScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := squallv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func model(name string) *squallv1alpha1.Model {
	return &squallv1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "squall"}}
}

func newReaper(t *testing.T, dc *reaperClient, objs ...client.Object) *Reaper {
	t.Helper()
	now := time.Now()
	return &Reaper{
		Client:       fake.NewClientBuilder().WithScheme(reaperScheme(t)).WithObjects(objs...).Build(),
		DstackClient: dc,
		Clock:        func() time.Time { return now },
	}
}

type deadlineReader struct {
	client.Reader
	sawDeadline bool
}

func (r *deadlineReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	_, r.sawDeadline = ctx.Deadline()
	return r.Reader.List(ctx, list, opts...)
}

func TestReaper_SweepBoundsEveryExternalCall(t *testing.T) {
	dc := &reaperClient{}
	r := newReaper(t, dc)
	reader := &deadlineReader{Reader: r.Client}
	r.Client = reader

	if _, err := r.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !reader.sawDeadline {
		t.Fatal("Model list context has no deadline")
	}
	if dc.listRunsCtx == nil {
		t.Fatal("ListRuns did not receive a context")
	}
	if _, ok := dc.listRunsCtx.Deadline(); !ok {
		t.Fatal("dstack ListRuns context has no deadline")
	}
}

// TestReaper_StopsARunNoModelClaims is the reason this component exists: a
// run still burning money that nothing owns. The finalizer cannot recover
// this — the CR it would have hung off is gone. D108: the run carries
// squall's own UID stamp for a Model that no longer exists, which is what
// PROVES it is our orphan and not a stranger's job.
func TestReaper_StopsARunNoModelClaims(t *testing.T) {
	dc := &reaperClient{runs: []dstack.Run{{
		Name: "ghost", RunID: "r1", Status: "running",
		SubmittedAt: time.Now().Add(-time.Hour),
		Env:         map[string]string{ModelUIDEnvKey: "uid-of-a-deleted-model"},
	}}}
	r := newReaper(t, dc)

	got, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got != 1 || len(dc.stopped) != 1 || dc.stopped[0] != "ghost" {
		t.Fatalf("reaped=%d stopped=%v, want the orphan stopped", got, dc.stopped)
	}
}

// TestReaper_LeavesUnmarkedRunsAlone is D108's fail-safe half: ListRuns
// answers from dstack's un-scoped root route, so a run with no
// SQUALL_MODEL_UID stamp might be a colleague's `dstack apply` on a shared
// server. Stopping it on a name miss destroys somebody else's live GPU
// job; leaving it bills until a human looks — the tolerable error.
func TestReaper_LeavesUnmarkedRunsAlone(t *testing.T) {
	dc := &reaperClient{runs: []dstack.Run{{
		Name: "finetune-job", RunID: "r9", Status: "running",
		SubmittedAt: time.Now().Add(-time.Hour),
	}}}
	r := newReaper(t, dc)

	got, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got != 0 || len(dc.stopped) != 0 {
		t.Fatalf("reaped=%d stopped=%v, want an unmarked run left strictly alone", got, dc.stopped)
	}
}

// TestReaper_LeavesRunsStampedByALiveModelAlone: a name mismatch with a
// LIVE owner (rename, migration residue) is a conflict to report, never a
// run to stop — the stamp outranks the string miss (D108/D109).
func TestReaper_LeavesRunsStampedByALiveModelAlone(t *testing.T) {
	m := model("renamed")
	m.UID = "live-uid"
	dc := &reaperClient{runs: []dstack.Run{{
		Name: "old-name", RunID: "r2", Status: "running",
		SubmittedAt: time.Now().Add(-time.Hour),
		Env:         map[string]string{ModelUIDEnvKey: "live-uid"},
	}}}
	r := newReaper(t, dc, m)

	got, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got != 0 || len(dc.stopped) != 0 {
		t.Fatalf("reaped=%d stopped=%v, want a live Model's stamped run left alone", got, dc.stopped)
	}
}

// TestReaper_LeavesOwnedRunsAlone: the reaper must never touch a run whose
// Model still exists. Getting this wrong kills live production capacity.
func TestReaper_LeavesOwnedRunsAlone(t *testing.T) {
	// F1: the run a Model actually creates is namespace-qualified, so the
	// owned fixture must be too — model("owned") in namespace "squall"
	// mints "squall-owned". A run named plainly "owned" is now genuinely
	// unowned, and reaping it is correct rather than a regression.
	dc := &reaperClient{runs: []dstack.Run{{
		Name: "squall-owned", RunID: "r1", Status: "running",
		SubmittedAt: time.Now().Add(-time.Hour),
	}}}
	r := newReaper(t, dc, model("owned"))

	got, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got != 0 || len(dc.stopped) != 0 {
		t.Fatalf("reaped=%d stopped=%v, want an owned run left strictly alone", got, dc.stopped)
	}
}

// TestReaper_RefusesToActOnAPartialView is the single most important
// property here. If the API server cannot be listed, "no Models exist" is
// indistinguishable from "every Model is gone" — and acting on that reading
// would tear down every model in production during an API server blip.
func TestReaper_RefusesToActOnAPartialView(t *testing.T) {
	dc := &reaperClient{runs: []dstack.Run{{
		Name: "live", Status: "running", SubmittedAt: time.Now().Add(-time.Hour),
	}}}
	r := newReaper(t, dc)
	r.Client = brokenLister{}

	got, err := r.Sweep(context.Background())
	if err == nil {
		t.Fatal("sweep succeeded on an unreadable API server; it must refuse")
	}
	if got != 0 || len(dc.stopped) != 0 {
		t.Fatalf("reaped=%d stopped=%v — the reaper acted without knowing who owns what", got, dc.stopped)
	}
}

// TestReaper_HonoursTheGracePeriod: a run created seconds ago may belong to
// a Model this pass has not observed yet. Reaping it would kill a wake in
// progress.
func TestReaper_HonoursTheGracePeriod(t *testing.T) {
	dc := &reaperClient{runs: []dstack.Run{{
		Name: "just-born", Status: "provisioning", SubmittedAt: time.Now().Add(-time.Second),
	}}}
	r := newReaper(t, dc)

	got, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got != 0 || len(dc.stopped) != 0 {
		t.Fatalf("reaped=%d stopped=%v, want a freshly-created run spared", got, dc.stopped)
	}
}

// TestReaper_IgnoresTerminalOrphans: a terminated run bills nothing, and
// dstack keeps terminal runs listed by design (D52).
func TestReaper_IgnoresTerminalOrphans(t *testing.T) {
	dc := &reaperClient{runs: []dstack.Run{{
		Name: "dead", Status: "terminated", SubmittedAt: time.Now().Add(-time.Hour),
	}}}
	r := newReaper(t, dc)

	got, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got != 0 || len(dc.stopped) != 0 {
		t.Fatalf("reaped=%d stopped=%v, want terminal runs ignored", got, dc.stopped)
	}
}

// TestReaper_OneFailureDoesNotHideTheRest: the sweep exists to shrink the
// leak every minute. Aborting on the first error would let a second orphan
// bill indefinitely behind a first one that happens to fail.
func TestReaper_OneFailureDoesNotHideTheRest(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	orphanEnv := map[string]string{ModelUIDEnvKey: "uid-of-a-deleted-model"}
	dc := &reaperClient{runs: []dstack.Run{
		{Name: "a", Status: "running", SubmittedAt: old, Env: orphanEnv},
		{Name: "b", Status: "running", SubmittedAt: old, Env: orphanEnv},
	}}
	r := newReaper(t, dc)
	// First Stop fails, second must still be attempted.
	calls := 0
	failing := &failFirstStop{reaperClient: dc, calls: &calls}
	r.DstackClient = failing

	_, err := r.Sweep(context.Background())
	if err == nil {
		t.Fatal("want the failure reported")
	}
	if calls != 2 {
		t.Fatalf("Stop called %d times, want 2: a failure on the first orphan must not skip the second", calls)
	}
}

type failFirstStop struct {
	*reaperClient
	calls *int
}

func (f *failFirstStop) Stop(ctx context.Context, name string) error {
	*f.calls++
	if *f.calls == 1 {
		return errors.New("boom")
	}
	return f.reaperClient.Stop(ctx, name)
}

type brokenLister struct{ client.Client }

func (brokenLister) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("api server unreachable")
}

/* stubUtilisation is an obsolete engine probe test block.
// stubUtilisation is an engine that reports whatever the test says, including
// "I could not be read", which is the branch that must never reap.
type stubUtilisation struct {
	busy      int
	completed uint64
	err       error
	probes    int // probe call count
}

func (s *stubUtilisation) Sample(context.Context, *dstack.ReplicaEndpoint) (Utilisation, error) {
	s.probes++
	if s.err != nil {
		return Utilisation{}, s.err
	}
	return Utilisation{InFlight: s.busy, Completed: s.completed}, nil
}

func idleRun() dstack.Run {
	const name = "squall-qwen"
	return dstack.Run{
		Name:         name,
		RunID:        "run-" + name,
		Replicas:     1,
		Status:       "running",
		SubmittedAt:  time.Now().Add(-time.Hour),
		PricePerHour: 1.894,
		Replica: &dstack.ReplicaEndpoint{
			Host: "10.0.0.1", SSHPort: 22, User: "root", ServicePort: 8000,
		},
	}
}

// TestReaper_ReapsOwnedButIdleCapacity is D100, MEASURED LIVE 2026-08-31: a
// proxy rollout wiped the activity map of a Ready Model, which made §6's idle
// evidence permanently incomplete and the 1->0 flip unreachable. The GPU billed
// for 2h21m while serving zero requests, and nothing in squall could see it —
// the Model existed, so the orphan sweep skipped it, and the sleep path could
// not form a verdict. The engine's own counters cannot be wiped that way.
func TestReaper_ReapsOwnedButIdleCapacity(t *testing.T) {
	dc := &reaperClient{runs: []dstack.Run{idleRun()}}
	r := newReaper(t, dc, model("qwen"))
	r.Utilisation = &stubUtilisation{busy: 0}
	r.IdleLimit = 15 * time.Minute

	base := time.Now()
	r.Clock = func() time.Time { return base }
	if n, err := r.Sweep(context.Background()); err != nil || n != 0 {
		t.Fatalf("first idle observation reaped (n=%d, err=%v); it must only start the clock", n, err)
	}
	if len(dc.stopped) != 0 {
		t.Fatalf("stopped %v on the FIRST idle sample; thin evidence must not destroy capacity", dc.stopped)
	}

	// Still inside the window.
	r.Clock = func() time.Time { return base.Add(14 * time.Minute) }
	if _, err := r.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(dc.stopped) != 0 {
		t.Fatalf("reaped after 14m with a 15m limit: %v", dc.stopped)
	}

	// Past it.
	r.Clock = func() time.Time { return base.Add(16 * time.Minute) }
	n, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 || len(dc.stopped) != 1 || dc.stopped[0] != "squall-qwen" {
		t.Fatalf("idle capacity past the limit was not reaped: n=%d stopped=%v", n, dc.stopped)
	}
}

// TestReaper_NeverReapsOnUnreadableUtilisation is `1->0 fails safe` applied to
// the reaper itself. An engine we cannot reach is the SAME observation as a
// busy one for a component whose action is destructive, and it must stay that
// way however long the condition lasts — an unreachable replica for an hour is
// still not evidence that it is idle.
func TestReaper_NeverReapsOnUnreadableUtilisation(t *testing.T) {
	dc := &reaperClient{runs: []dstack.Run{idleRun()}}
	r := newReaper(t, dc, model("qwen"))
	probe := &stubUtilisation{err: errors.New("dial tcp: i/o timeout")}
	r.Utilisation = probe
	r.IdleLimit = time.Minute

	base := time.Now()
	for i := 0; i < 60; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		r.Clock = func() time.Time { return at }
		if _, err := r.Sweep(context.Background()); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if len(dc.stopped) != 0 {
		t.Fatalf("reaped capacity it could not read for an hour: %v — unknown is not idle", dc.stopped)
	}
	if probe.probes == 0 {
		t.Fatal("the probe was never called; this test proves nothing")
	}
}

// TestReaper_BusyCapacityResetsTheClock keeps the countdown honest: a replica
// that goes quiet, serves one request, then goes quiet again must get a FULL
// fresh window, not the remainder of the old one.
func TestReaper_BusyCapacityResetsTheClock(t *testing.T) {
	dc := &reaperClient{runs: []dstack.Run{idleRun()}}
	r := newReaper(t, dc, model("qwen"))
	probe := &stubUtilisation{busy: 0}
	r.Utilisation = probe
	r.IdleLimit = 15 * time.Minute

	base := time.Now()
	r.Clock = func() time.Time { return base }
	_, _ = r.Sweep(context.Background())

	// One request lands 14 minutes in.
	probe.busy = 1
	r.Clock = func() time.Time { return base.Add(14 * time.Minute) }
	_, _ = r.Sweep(context.Background())

	// Quiet again; 2 minutes later is 16 from the original idle start, but
	// only 2 from the real one.
	probe.busy = 0
	r.Clock = func() time.Time { return base.Add(16 * time.Minute) }
	if _, err := r.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(dc.stopped) != 0 {
		t.Fatalf("reaped %v 2 minutes after real traffic; the clock did not reset", dc.stopped)
	}
}

// TestReaper_NilUtilisationDisablesIdleReaping is the safe default: a
// deployment that cannot probe its replicas must lose money, never capacity.
func TestReaper_NilUtilisationDisablesIdleReaping(t *testing.T) {
	dc := &reaperClient{runs: []dstack.Run{idleRun()}}
	r := newReaper(t, dc, model("qwen"))
	r.IdleLimit = time.Nanosecond // would fire instantly if it fired at all

	base := time.Now()
	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * time.Hour)
		r.Clock = func() time.Time { return at }
		if _, err := r.Sweep(context.Background()); err != nil {
			t.Fatalf("sweep: %v", err)
		}
	}
	if len(dc.stopped) != 0 {
		t.Fatalf("reaped with no probe configured: %v", dc.stopped)
	}
}

// TestReaper_WorkBetweenSweepsResetsTheClock is the whole reason Utilisation
// carries an odometer and not just a gauge. A replica serving one 20-second
// request every five minutes is BUSY, and is worth its bill -- but a
// once-a-minute gauge reads zero at every single sample, because no sample
// ever lands inside a request. The gauge alone would destroy it mid-use.
func TestReaper_WorkBetweenSweepsResetsTheClock(t *testing.T) {
	dc := &reaperClient{runs: []dstack.Run{idleRun()}}
	r := newReaper(t, dc, model("qwen"))
	probe := &stubUtilisation{busy: 0, completed: 100}
	r.Utilisation = probe
	r.IdleLimit = 5 * time.Minute

	base := time.Now()
	for i := 0; i < 20; i++ {
		// The gauge NEVER reports load: every request began and ended in
		// the gap between two sweeps. Only the odometer saw them.
		probe.completed += 7
		r.Clock = func() time.Time { return base.Add(time.Duration(i) * time.Minute) }
		if _, err := r.Sweep(context.Background()); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if len(dc.stopped) != 0 {
		t.Fatalf("reaped %v while the work counter was still climbing; the reaper is gauge-blind", dc.stopped)
	}

	// Work stops. Now -- and only now -- the countdown may run to the limit.
	for i := 20; i < 27; i++ {
		r.Clock = func() time.Time { return base.Add(time.Duration(i) * time.Minute) }
		if _, err := r.Sweep(context.Background()); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if len(dc.stopped) != 1 {
		t.Fatalf("odometer went flat for 6 minutes past a 5m limit and nothing was reaped: stopped=%v", dc.stopped)
	}
}
*/
