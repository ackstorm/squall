// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

func TestAggregate_NoReplicaHasTheKey_IsCompleteAndIdleWithoutData(t *testing.T) {
	now := time.Now()
	ev := aggregateActivity([]string{"10.0.0.1:8080", "10.0.0.2:8080"}, []ActivityQuery{
		{Address: "10.0.0.1:8080", OK: true, NoData: true},
		{Address: "10.0.0.2:8080", OK: true, NoData: true},
	}, now)
	if !ev.Complete || !ev.AllIdle || ev.AnyData || !ev.NewestLastRequestAt.IsZero() {
		t.Fatalf("expected complete idle evidence without data, got %+v", ev)
	}
}

func TestAggregate_UnreachableReplicaIsStillIncomplete(t *testing.T) {
	now := time.Now()
	ev := aggregateActivity([]string{"10.0.0.1:8080", "10.0.0.2:8080"}, []ActivityQuery{
		{Address: "10.0.0.1:8080", OK: true, NoData: true},
		{Address: "10.0.0.2:8080", OK: false},
	}, now)
	if ev.Complete || ev.AllIdle || ev.AnyData {
		t.Fatalf("expected incomplete evidence, got %+v", ev)
	}
}

func TestGatherActivity_TerminatingServingReplicaIsSeen(t *testing.T) {
	now := time.Now().UTC()
	server := activityServer(t, squallv1alpha1.ActivityReport{
		Models: map[string]squallv1alpha1.ModelActivity{
			"qwen": {InFlight: 0, LastRequestAt: now.Add(-time.Hour)},
		},
	})
	slices := endpointSlicesForServers(t, server)
	slice := slices[0].(*discoveryv1.EndpointSlice)
	ready, serving, terminating := false, true, true
	slice.Endpoints[0].Conditions = discoveryv1.EndpointConditions{
		Ready: &ready, Serving: &serving, Terminating: &terminating,
	}
	r := &ModelReconciler{
		Client:       newFakeClient(t, slice),
		ProxyService: types.NamespacedName{Namespace: testProxyNamespace, Name: testProxyService},
	}

	got := r.gatherActivity(context.Background(), "qwen", now)
	if got == nil || !got.Complete || !got.AllIdle {
		t.Fatalf("gatherActivity = %+v, want terminating serving endpoint included and idle", got)
	}
}

// These are Task 7.1's impure-layer tests (gatherActivity/queryActivity):
// a fake client.Client for the proxy Service's EndpointSlices, plus real
// httptest servers standing in for proxy replicas. No envtest control
// plane is needed for any of this, so it belongs in test-unit.

const testProxyService = "squall-proxy"
const testProxyNamespace = "squall-system"

func activityServer(t *testing.T, report squallv1alpha1.ActivityReport) *httptest.Server {
	t.Helper()
	srv, _ := mutableActivityServer(t, report)
	return srv
}

// mutableActivityServer is activityServer plus a setter, so a test can
// change what a replica reports for a later gatherActivity call — the only
// way to tell a genuinely fresh re-query apart from a memoized one (T6).
func mutableActivityServer(t *testing.T, report squallv1alpha1.ActivityReport) (*httptest.Server, func(squallv1alpha1.ActivityReport)) {
	t.Helper()
	var mu sync.Mutex
	current := report
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != squallv1alpha1.ActivityPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		toSend := current
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(toSend); err != nil {
			t.Fatalf("encode activity report: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	set := func(r squallv1alpha1.ActivityReport) {
		mu.Lock()
		current = r
		mu.Unlock()
	}
	return srv, set
}

// endpointSliceFor builds an EndpointSlice from addrPorts (host:port pairs,
// matching httptest.Server.Listener.Addr()) — one Subset per entry, since a
// real EndpointSubset's Ports list applies uniformly to every Address in
// it (all pods behind a Service share one containerPort), which httptest
// servers on 127.0.0.1 with independently random ports do not: cramming
// them into a single shared-port Subset would silently alias every address
// onto whichever port came last.
func endpointSliceFor(t *testing.T, addrPort string) *discoveryv1.EndpointSlice {
	t.Helper()
	host, port := splitHostPort(t, addrPort)
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", testProxyService, port),
			Namespace: testProxyNamespace,
			Labels:    map[string]string{discoveryv1.LabelServiceName: testProxyService},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{host}}},
		Ports:       []discoveryv1.EndpointPort{{Port: &port}},
	}
}

func splitHostPort(t *testing.T, hostPort string) (string, int32) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("split host:port %q: %v", hostPort, err)
	}
	port, err := strconv.ParseInt(portStr, 10, 32)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return host, int32(port)
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add clientgo scheme: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discovery scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// errorOnEndpointSliceListClient wraps a client.Client and fails every List
// for EndpointSlices, simulating an API-server read failure (T4).
type errorOnEndpointSliceListClient struct {
	client.Client
	err error
}

func (e *errorOnEndpointSliceListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*discoveryv1.EndpointSliceList); ok {
		return e.err
	}
	return e.Client.List(ctx, list, opts...)
}

// TestGatherActivity_ProxyServiceNotConfigured_ReturnsNil confirms OQ5's
// resolution never silently reads an empty/unconfigured proxy as idle: a
// zero-value ProxyService means "not evaluated", not "zero replicas".
func TestGatherActivity_ProxyServiceNotConfigured_ReturnsNil(t *testing.T) {
	r := &ModelReconciler{Client: newFakeClient(t)}
	got := r.gatherActivity(context.Background(), "qwen", time.Now())
	if got != nil {
		t.Fatalf("gatherActivity = %+v, want nil (ProxyService unset)", got)
	}
}

// TestGatherActivity_EndpointSliceListError_ReturnsIncompleteEvidence is T4: an
// EndpointSlice list failure must produce Complete: false, never fall through
// to a vacuous "zero addresses expected -> complete" reading.
func TestGatherActivity_EndpointSliceListError_ReturnsIncompleteEvidence(t *testing.T) {
	inner := newFakeClient(t)
	r := &ModelReconciler{
		Client:       &errorOnEndpointSliceListClient{Client: inner, err: assertErr},
		ProxyService: types.NamespacedName{Namespace: testProxyNamespace, Name: testProxyService},
	}

	got := r.gatherActivity(context.Background(), "qwen", time.Now())
	if got == nil {
		t.Fatal("gatherActivity = nil, want non-nil incomplete evidence")
	}
	if got.Complete {
		t.Errorf("Complete = true, want false on an Endpoints read error")
	}
}

// TestGatherActivity_QueriesAllReplicasAndAggregates is the end-to-end
// happy path: two real HTTP servers standing in for proxy replicas, one
// idle, one in-flight -> Complete: true, AllIdle: false (AND semantics,
// same invariant as aggregateActivity's T2, exercised here through the
// full impure path).
func TestGatherActivity_QueriesAllReplicasAndAggregates(t *testing.T) {
	now := time.Now().UTC()
	idle := activityServer(t, squallv1alpha1.ActivityReport{
		Models: map[string]squallv1alpha1.ModelActivity{
			"qwen": {InFlight: 0, LastRequestAt: now.Add(-time.Minute)},
		},
	})
	busy := activityServer(t, squallv1alpha1.ActivityReport{
		Models: map[string]squallv1alpha1.ModelActivity{
			"qwen": {InFlight: 1, LastRequestAt: now},
		},
	})

	r := &ModelReconciler{
		Client:       newFakeClient(t, endpointSlicesForServers(t, idle, busy)...),
		ProxyService: types.NamespacedName{Namespace: testProxyNamespace, Name: testProxyService},
	}

	got := r.gatherActivity(context.Background(), "qwen", now.Add(time.Hour))
	if got == nil || !got.Complete {
		t.Fatalf("gatherActivity = %+v, want Complete: true", got)
	}
	if got.AllIdle {
		t.Errorf("AllIdle = true, want false: one replica has InFlight: 1")
	}
}

// TestGatherActivity_NoCaching_ReflectsCurrentEndpointSlicesEveryCall is T6: a
// replica's state changing between two reconciles must be reflected in the
// very next call — gatherActivity must re-List EndpointSlices and re-query
// fresh every time, never memoize a prior pass's addresses or reports. A
// version of this test that only re-asserts the same outcome twice would
// pass just as well with a one-shot cache, so the second pass here forces
// an outcome (AllIdle: true -> false) that a memoized first pass could not
// produce.
func TestGatherActivity_NoCaching_ReflectsCurrentEndpointsEveryCall(t *testing.T) {
	now := time.Now().UTC()
	idleReport := squallv1alpha1.ActivityReport{
		Models: map[string]squallv1alpha1.ModelActivity{
			"qwen": {InFlight: 0, LastRequestAt: now.Add(-time.Minute)},
		},
	}
	srvA, setA := mutableActivityServer(t, idleReport)
	srvB := activityServer(t, idleReport)

	fakeClient := newFakeClient(t, endpointSlicesForServers(t, srvA, srvB)...)
	r := &ModelReconciler{
		Client:       fakeClient,
		ProxyService: types.NamespacedName{Namespace: testProxyNamespace, Name: testProxyService},
	}

	first := r.gatherActivity(context.Background(), "qwen", now.Add(time.Hour))
	if first == nil || !first.Complete || !first.AllIdle {
		t.Fatalf("first pass = %+v, want Complete && AllIdle with both replicas idle", first)
	}

	// Replica A picks up an in-flight request before the next reconcile.
	setA(squallv1alpha1.ActivityReport{
		Models: map[string]squallv1alpha1.ModelActivity{
			"qwen": {InFlight: 1, LastRequestAt: now},
		},
	})

	second := r.gatherActivity(context.Background(), "qwen", now.Add(time.Hour))
	if second == nil || !second.Complete {
		t.Fatalf("second pass = %+v, want Complete: true", second)
	}
	if second.AllIdle {
		t.Fatalf("AllIdle = true on the second pass, want false: replica A is now in-flight; " +
			"a cached first pass would wrongly still report all-idle")
	}
}

func endpointSlicesForServers(t *testing.T, servers ...*httptest.Server) []client.Object {
	t.Helper()
	slices := make([]client.Object, 0, len(servers))
	for _, s := range servers {
		slices = append(slices, endpointSliceFor(t, s.Listener.Addr().String()))
	}
	return slices
}

var assertErr = &staticErr{"endpoints unreachable"}

type staticErr struct{ msg string }

func (e *staticErr) Error() string { return e.msg }
