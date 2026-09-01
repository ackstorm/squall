// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

// TestReconcile_EndpointSliceChurn_NoPrematureSleep is Task 7.2 (block 7+8 plan
// §5): replicas coming and going between reconciles must never produce a
// premature sleep flip, and a fixed replica must be reflected on the very
// next reconcile — never held back by a stale snapshot from a prior pass
// (T4, T6). Driven by direct Reconcile calls (not the shared manager) in
// manualNamespace, mirroring TestReconcile_ConcurrentFlips_* below.
func TestReconcile_EndpointSliceChurn_NoPrematureSleep(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const name = "qwen-7-2"
	spec := exampleModelSpec()
	spec.ScaleDownDelaySeconds = 1 // small, so "aged past" needs no FakeClock/real sleep >1s
	model := &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  manualNamespace,
			Finalizers: []string{ModelFinalizer}, // pre-seeded: Reconcile's finalizer-add pass is exercised elsewhere (Task 8.0).
		},
		Spec: spec,
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create Model: %v", err)
	}

	// Seed a live run directly against dstack (bypassing Decide/wake): this
	// test is about the sleep evaluation, not about how the run got here.
	if _, err := dstackClient.Apply(ctx, dstack.ApplyRequest{Name: runNameIn(manualNamespace, name), Replicas: 1}); err != nil {
		t.Fatalf("seed dstack run: %v", err)
	}

	// The real envtest API server rejects loopback IPs in EndpointSlices (unlike
	// the fake client.Client used by activity_gather_test.go), so replicas
	// here are bound to the host's own non-loopback address instead of
	// httptest's default 127.0.0.1.
	nonLoopback := nonLoopbackIP(t)

	longAgo := time.Now().UTC().Add(-time.Hour)
	idleA, setA := nonLoopbackActivityServer(t, nonLoopback, squallv1alpha1.ActivityReport{
		Models: map[string]squallv1alpha1.ModelActivity{
			name: {InFlight: 0, LastRequestAt: longAgo},
		},
	})

	proxyKey := types.NamespacedName{Namespace: manualNamespace, Name: "squall-proxy-7-2"}
	endpoints := []*discoveryv1.EndpointSlice{
		endpointSliceForAddr(t, proxyKey.Name+"-a", proxyKey.Namespace, idleA.Listener.Addr().String(), proxyKey.Name),
		// Replica B is listed but unreachable (nothing listens on this
		// port): completeness must fail, not vacuously pass with A alone.
		endpointSliceForAddr(t, proxyKey.Name+"-b", proxyKey.Namespace, net.JoinHostPort(nonLoopback, "1"), proxyKey.Name),
	}
	for _, slice := range endpoints {
		if err := k8sClient.Create(ctx, slice); err != nil {
			t.Fatalf("create EndpointSlice: %v", err)
		}
	}

	r := &ModelReconciler{Client: k8sClient, DstackClient: dstackClient, ProxyService: proxyKey}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}

	// Pass 1: B unreachable -> incomplete evidence -> must NOT sleep, even
	// though A alone is idle and long past scaleDownDelaySeconds.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (B unreachable): %v", err)
	}
	run, err := dstackClient.Get(ctx, runNameIn(manualNamespace, name))
	if err != nil {
		t.Fatalf("get run after pass 1: %v", err)
	}
	if run.Replicas != 1 {
		t.Fatalf("Replicas = %d after an incomplete pass, want 1 (no premature sleep)", run.Replicas)
	}

	// Replica B is "fixed": its EndpointSlice now points it at a second idle server.
	idleB, _ := nonLoopbackActivityServer(t, nonLoopback, squallv1alpha1.ActivityReport{
		Models: map[string]squallv1alpha1.ModelActivity{
			name: {InFlight: 0, LastRequestAt: longAgo},
		},
	})
	var current discoveryv1.EndpointSlice
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: proxyKey.Namespace, Name: proxyKey.Name + "-b"}, &current); err != nil {
		t.Fatalf("get EndpointSlice before repair: %v", err)
	}
	repaired := endpointSliceForAddr(t, current.Name, current.Namespace, idleB.Listener.Addr().String(), proxyKey.Name)
	current.Endpoints = repaired.Endpoints
	current.Ports = repaired.Ports
	if err := k8sClient.Update(ctx, &current); err != nil {
		t.Fatalf("repair EndpointSlice: %v", err)
	}

	// Pass 2: both replicas now idle and long past the delay -> sleep.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (both idle): %v", err)
	}
	run, err = dstackClient.Get(ctx, runNameIn(manualNamespace, name))
	if err != nil {
		t.Fatalf("get run after pass 2: %v", err)
	}
	if run.Replicas != 0 {
		t.Fatalf("Replicas = %d after clean, aged, complete idle evidence, want 0 (sleep)", run.Replicas)
	}

	// Replica A picks up an in-flight request. This must not be read from
	// pass 2's now-stale snapshot: re-woken and re-evaluated fresh, the
	// model must not immediately re-sleep while A is busy.
	if _, err := dstackClient.Apply(ctx, dstack.ApplyRequest{Name: runNameIn(manualNamespace, name), Replicas: 1, Current: run}); err != nil {
		t.Fatalf("re-wake dstack run: %v", err)
	}
	setA(squallv1alpha1.ActivityReport{
		Models: map[string]squallv1alpha1.ModelActivity{
			name: {InFlight: 1, LastRequestAt: time.Now().UTC()},
		},
	})
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (A busy again): %v", err)
	}
	run, err = dstackClient.Get(ctx, runNameIn(manualNamespace, name))
	if err != nil {
		t.Fatalf("get run after pass 3: %v", err)
	}
	if run.Replicas != 1 {
		t.Fatalf("Replicas = %d with A back in-flight, want 1 (fresh re-evaluation, not a stale idle snapshot)", run.Replicas)
	}
}

func endpointSliceForAddr(t *testing.T, name, namespace, addr, serviceName string) *discoveryv1.EndpointSlice {
	t.Helper()
	host, port := splitHostPort(t, addr)
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels: map[string]string{discoveryv1.LabelServiceName: serviceName},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{host}}},
		Ports:       []discoveryv1.EndpointPort{{Port: &port}},
	}
}

// nonLoopbackIP returns the host's own non-loopback IPv4 address, since the
// real envtest API server (unlike the fake client.Client in
// activity_gather_test.go) rejects loopback IPs in EndpointSlices.
func nonLoopbackIP(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("list interface addrs: %v", err)
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipNet.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	t.Skip("no non-loopback IPv4 address available to bind a fake replica to")
	return ""
}

// nonLoopbackActivityServer is mutableActivityServer bound to ip instead of
// httptest's default 127.0.0.1, for use against a real API server that
// rejects loopback addresses in Endpoints.
func nonLoopbackActivityServer(t *testing.T, ip string, report squallv1alpha1.ActivityReport) (*httptest.Server, func(squallv1alpha1.ActivityReport)) {
	t.Helper()
	lis, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatalf("listen on %s: %v", ip, err)
	}

	var mu sync.Mutex
	current := report
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv.Listener.Close() //nolint:errcheck // replaced below, never used.
	srv.Listener = lis
	srv.Start()
	t.Cleanup(srv.Close)

	set := func(r squallv1alpha1.ActivityReport) {
		mu.Lock()
		current = r
		mu.Unlock()
	}
	return srv, set
}
