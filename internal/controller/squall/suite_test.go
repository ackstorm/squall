// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
	"github.com/ackstorm/squall/internal/dstack/mock"
)

// managedNamespace is watched by the shared manager started below: Tasks
// 6.2 and 6.3 create Models here and rely on the real controller loop
// (watch -> workqueue -> Reconcile) to drive them, since AC4's coalescing
// guarantee lives in controller-runtime's per-key workqueue, not in
// anything this package reimplements.
//
// manualNamespace is deliberately left OUT of the manager's watched set:
// Task 6.4 drives two Reconcile calls by hand to force a deterministic
// dstack-side CAS race (F18), and must not have the live controller also
// reconciling the same object as an uncontrolled third actor.
//
// Neither may collide with the pre-existing "squall" namespace used by
// model_sample_envtest_test.go, whose TestSampleModel_AppliesAndDefaultsMaterialise
// asserts status.Phase == "" — a real controller reconciling that CR would
// break that assertion.
const (
	managedNamespace = "squall-managed"
	manualNamespace  = "squall-manual"
)

// Test-global state shared across every file in this package's test suite.
// TestMain populates these; individual Test* functions read them.
var (
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client

	// dstackFake is the in-memory dstack+gateway double (internal/dstack/mock)
	// backing every envtest-based controller test in this package.
	// dstackServer exposes it over real HTTP; dstackClient is the
	// production internal/dstack.Client wired against that server — the
	// same client the reconciler uses, so these tests exercise the real
	// wire path (Task 6.2-6.4), not a hand-rolled fake Client.
	dstackFake   *mock.Server
	dstackServer *httptest.Server
	dstackClient dstack.Client
)

// TestMain is the envtest bootstrap: a real etcd+kube-apiserver pair
// against this repo's own CRDs (config/crd/bases), a direct client for
// test fixtures, a fake dstack server, and (Task 6.2) a real
// ctrl.Manager running ModelReconciler against managedNamespace only.
// Ported from alitellm-operator's internal/controller/suite_test.go.
func TestMain(m *testing.M) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	if code := setupAndRun(m); code != 0 {
		os.Exit(code)
	}
}

// setupAndRun is split out so deferred cleanup runs before os.Exit
// (deferred funcs do NOT run after os.Exit).
func setupAndRun(m *testing.M) int {
	// `make test-unit` runs the whole tree with -short and must never need a
	// control plane (spec: unit is Phase 1, envtest is Phase 2). Parse flags
	// first — testing.Short() is only meaningful after flag.Parse, and
	// TestMain runs before the testing package would do it for us.
	flag.Parse()
	if testing.Short() {
		return m.Run()
	}

	_, thisFile, _, _ := runtime.Caller(0)
	crdDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "config", "crd", "bases")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest start failed: %v\n", err)
		return 1
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "envtest stop: %v\n", err)
		}
	}()

	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(squallv1alpha1.AddToScheme(scheme))

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "k8s client: %v\n", err)
		return 1
	}

	for _, ns := range []string{managedNamespace, manualNamespace} {
		obj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		if err := k8sClient.Create(context.Background(), obj); err != nil && !apierrors.IsAlreadyExists(err) {
			fmt.Fprintf(os.Stderr, "create namespace %q: %v\n", ns, err)
			return 1
		}
	}

	dstackFake = mock.New()
	dstackServer = httptest.NewServer(dstackFake.Handler())
	defer dstackServer.Close()
	dstackClient = dstack.NewHTTPClient(dstackServer.URL, "main", mock.ValidToken, nil)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			// Restricts the shared manager's watch to managedNamespace only
			// (see the const block doc above) — this is what keeps the
			// pre-existing "squall" fixture and Task 6.4's manualNamespace
			// safe from an uncommanded, real reconcile.
			DefaultNamespaces: map[string]cache.Config{managedNamespace: {}},
		},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "new manager: %v\n", err)
		return 1
	}

	if err := (&ModelReconciler{
		Client:       mgr.GetClient(),
		Scheme:       scheme,
		DstackClient: dstackClient,
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "setup reconciler: %v\n", err)
		return 1
	}

	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- mgr.Start(mgrCtx) }()
	if !mgr.GetCache().WaitForCacheSync(mgrCtx) {
		mgrCancel()
		fmt.Fprintln(os.Stderr, "manager cache did not sync")
		return 1
	}
	// Registered after testEnv's own stop-defer above, so by LIFO ordering
	// this runs FIRST: the manager must stop talking to the API server
	// before testEnv tears that server down.
	defer func() {
		mgrCancel()
		select {
		case err := <-mgrDone:
			if err != nil {
				fmt.Fprintf(os.Stderr, "manager exited with error: %v\n", err)
			}
		case <-time.After(10 * time.Second):
			fmt.Fprintln(os.Stderr, "manager did not stop within 10s")
		}
	}()

	return m.Run()
}

// runNameIn is the dstack run name that a Model called name in namespace ns
// is filed under (F1, dstackRunName). Envtest fixtures seed and query dstack
// DIRECTLY, bypassing Reconcile, so they have to spell the same identity
// Reconcile uses — before F1 the two happened to coincide and every fixture
// silently relied on that.
func runNameIn(ns, name string) string { return ns + "-" + name }
