// SPDX-License-Identifier: MIT

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
