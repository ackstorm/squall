// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/clock"
	"github.com/ackstorm/squall/internal/dstack"
	"github.com/ackstorm/squall/internal/dstack/mock"
	"github.com/ackstorm/squall/internal/metrics"
)

// Restart drills: the controller or dstack dies at an awkward moment and the
// outcome must still be "the GPU is stopped or has a deadline". They run the
// real dstack.HTTPClient against the fake's HTTP handler, so what the
// controller stamps on a run travels over the wire like it does in production.

// flakyTransport fails requests the way an unreachable dstack does. stopOnly
// restricts the outage to the stop route.
type flakyTransport struct {
	down     atomic.Bool
	stopOnly atomic.Bool
}

func (f *flakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.down.Load() && (!f.stopOnly.Load() || strings.HasSuffix(r.URL.Path, "/runs/stop")) {
		return nil, errors.New("dial tcp: connection refused")
	}
	return http.DefaultTransport.RoundTrip(r)
}

type drill struct {
	fake  *mock.Server
	dc    *dstack.HTTPClient
	flaky *flakyTransport
	clk   *clock.FakeClock
}

func newDrill(t *testing.T) *drill {
	t.Helper()
	d := &drill{fake: mock.New(), flaky: &flakyTransport{}, clk: clock.NewFakeClock(time.Now().Truncate(time.Second))}
	d.fake.SetClock(d.clk)
	srv := httptest.NewServer(d.fake.Handler())
	t.Cleanup(srv.Close)
	d.dc = dstack.NewHTTPClient(srv.URL, "main", mock.ValidToken, &http.Client{Transport: d.flaky, Timeout: 5 * time.Second})
	return d
}

func drillModel(name string, uid types.UID, now time.Time) *squallv1alpha1.Model {
	m := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: uid,
			Finalizers:  []string{ModelFinalizer},
			Annotations: map[string]string{squallv1alpha1.DemandAnnotation: now.UTC().Format(time.RFC3339)},
		},
		Spec: exampleModelSpec(),
	}
	return m
}

func drillK8s(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	if err := squallv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(funcs).
		WithStatusSubresource(&squallv1alpha1.Model{}).WithObjects(objs...).Build()
}

func (d *drill) reconcile(t *testing.T, k8s client.Client, m *squallv1alpha1.Model) error {
	t.Helper()
	// A fresh reconciler per pass: nothing is carried in memory.
	r := &ModelReconciler{Client: k8s, DstackClient: d.dc, Clock: d.clk}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
	return err
}

// The controller dies between creating the GPU and recording it. The run must
// still be squall's, the next pass must seed the deadline anchor, and once
// provisioningTimeout passes on a run that never became Ready the deadline
// takes it down.
func TestDrill_DiesBetweenCreatingTheGPUAndRecordingIt(t *testing.T) {
	d := newDrill(t)
	d.fake.SetProbeDelay(1000 * time.Hour) // never Ready
	m := drillModel("restart", "uid-restart", d.clk.Now())
	runName := runNameFor(m)

	failStatus := interceptor.Funcs{SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
		return errors.New("status patch lost")
	}}
	if err := d.reconcile(t, drillK8s(t, failStatus, m.DeepCopy()), m); err == nil {
		t.Fatal("first pass: want the failed status patch reported")
	}
	run, err := d.fake.Get(runName)
	if err != nil || run.Replicas != 1 {
		t.Fatalf("after the lost patch: run=%+v err=%v, want a live run", run, err)
	}
	if run.Env[ModelUIDEnvKey] != "uid-restart" {
		t.Fatalf("run stamp = %q, want the Model's UID", run.Env[ModelUIDEnvKey])
	}

	// The restarted controller: the Model has no recorded anchor.
	k8s := drillK8s(t, interceptor.Funcs{}, m.DeepCopy())
	d.clk.Advance(time.Minute)
	if err := d.reconcile(t, k8s, m); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	var stored squallv1alpha1.Model
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(m), &stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status.WakeStartedAt == nil {
		t.Fatal("a live run was observed and status.wakeStartedAt is still nil: it has no deadline")
	}

	d.clk.Advance(m.Spec.ProvisioningTimeout.Duration + time.Minute)
	if err := d.reconcile(t, k8s, m); err != nil {
		t.Fatalf("deadline pass: %v", err)
	}
	if run, err := d.fake.Get(runName); err != nil || run.Replicas != 0 {
		t.Fatalf("after the deadline: run=%+v err=%v, want the never-Ready run taken to zero replicas", run, err)
	}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(m), &stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status.Phase != squallv1alpha1.ModelPhaseDead {
		t.Fatalf("phase = %q, want Dead: the stop must come from the provisioning deadline", stored.Status.Phase)
	}
	if c := meta.FindStatusCondition(stored.Status.Conditions, squallv1alpha1.ConditionHealthy); c == nil ||
		c.Reason != squallv1alpha1.ReasonProvisioningTimeout {
		t.Fatalf("Healthy condition = %+v, want reason %s", c, squallv1alpha1.ReasonProvisioningTimeout)
	}
}

// reaperGauge reads one gauge off the collector through a registry.
func reaperGauge(t *testing.T, c *metrics.ReaperCollector, name string) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("%s not exported", name)
	return 0
}

const (
	lastSuccessGauge = "squall_reaper_last_success_timestamp_seconds"
	runTerminated    = "terminated"
)

// A Model deleted while the controller was down leaves a stamped run and no
// Model. A sweep while dstack is unreachable reaps nothing
// and does not count as a success; the next one, once dstack answers, stops the
// run. The stamp reaches the reaper over the wire.
func TestDrill_ModelGoneAndDstackUnreachableThenBack(t *testing.T) {
	d := newDrill(t)
	if _, err := d.dc.Apply(context.Background(), dstack.ApplyRequest{
		Name: "default-gone", Replicas: 1, Env: map[string]string{ModelUIDEnvKey: "uid-of-a-deleted-model"},
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	sweepAt := time.Now().Add(DefaultReapGrace + time.Minute)
	rm := metrics.NewReaperCollector()
	r := &Reaper{
		Client: drillK8s(t, interceptor.Funcs{}), DstackClient: d.dc,
		Clock: func() time.Time { return sweepAt }, Metrics: rm,
	}

	d.flaky.down.Store(true)
	if reaped, err := r.Sweep(context.Background()); err == nil || reaped != 0 {
		t.Fatalf("sweep with dstack down: reaped=%d err=%v, want an error and nothing reaped", reaped, err)
	}
	if got := reaperGauge(t, rm, lastSuccessGauge); got != 0 {
		t.Fatalf("a failed sweep advanced the success timestamp to %v", got)
	}
	if run, _ := d.fake.Get("default-gone"); run == nil || run.Status == runTerminated {
		t.Fatalf("the run was touched while dstack was unreachable: %+v", run)
	}

	d.flaky.down.Store(false)
	reaped, err := r.Sweep(context.Background())
	if err != nil || reaped != 1 {
		t.Fatalf("sweep after dstack returned: reaped=%d err=%v, want the orphan reaped", reaped, err)
	}
	if run, _ := d.fake.Get("default-gone"); run == nil || run.Status != runTerminated {
		t.Fatalf("orphan not stopped: %+v", run)
	}
	if got := reaperGauge(t, rm, lastSuccessGauge); got != float64(sweepAt.Unix()) {
		t.Fatalf("success timestamp = %v, want %d", got, sweepAt.Unix())
	}
}

// dstack is unreachable during a delete. The finalizer must stay and
// the run must survive untouched until dstack answers; only then is the run
// stopped, and only after it is terminal is the finalizer released.
func TestDrill_DstackUnreachableDuringDelete(t *testing.T) {
	d := newDrill(t)
	m := drillModel("teardown", "uid-teardown", d.clk.Now())
	m.Spec.DrainTimeout = metav1.Duration{Duration: time.Second}
	runName := runNameFor(m)
	if _, err := d.dc.Apply(context.Background(), dstack.ApplyRequest{
		Name: runName, Replicas: 1, Env: map[string]string{ModelUIDEnvKey: "uid-teardown"},
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	k8s := drillK8s(t, interceptor.Funcs{}, m.DeepCopy())
	if err := k8s.Delete(context.Background(), m.DeepCopy()); err != nil {
		t.Fatalf("delete model: %v", err)
	}
	d.clk.Advance(time.Minute) // past DeletionTimestamp + drainTimeout

	hasFinalizer := func() bool {
		var got squallv1alpha1.Model
		err := k8s.Get(context.Background(), client.ObjectKeyFromObject(m), &got)
		return err == nil && controllerutil.ContainsFinalizer(&got, ModelFinalizer)
	}
	liveRun := func(when string) {
		t.Helper()
		if run, err := d.fake.Get(runName); err != nil || run.Status == runTerminated {
			t.Fatalf("%s: run=%+v err=%v, want it untouched", when, run, err)
		}
	}

	// dstack down entirely, then only the stop route down.
	for _, stopOnly := range []bool{false, true} {
		d.flaky.down.Store(true)
		d.flaky.stopOnly.Store(stopOnly)
		if err := d.reconcile(t, k8s, m); err == nil {
			t.Fatalf("stopOnly=%v: want the dstack error returned", stopOnly)
		}
		if !hasFinalizer() {
			t.Fatalf("stopOnly=%v: finalizer released while the run could not be stopped", stopOnly)
		}
		liveRun("dstack unreachable")
	}

	d.flaky.down.Store(false)
	for i := 0; i < 5 && hasFinalizer(); i++ {
		if err := d.reconcile(t, k8s, m); err != nil {
			t.Fatalf("pass %d after dstack returned: %v", i, err)
		}
		if i == 0 {
			if run, _ := d.fake.Get(runName); run == nil || run.Status != runTerminated {
				t.Fatalf("first pass after recovery did not stop the run: %+v", run)
			}
			if !hasFinalizer() {
				t.Fatal("finalizer released in the same pass that stopped the run")
			}
		}
	}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(m), &squallv1alpha1.Model{}); !apierrors.IsNotFound(err) {
		t.Fatalf("model still present after recovery: err=%v", err)
	}
	if _, err := d.fake.Get(runName); !errors.Is(err, mock.ErrNotFound) {
		t.Fatalf("run record after teardown: err=%v, want it deleted", err)
	}
}
