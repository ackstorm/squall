// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	internalclock "github.com/ackstorm/squall/internal/clock"
	squallcontroller "github.com/ackstorm/squall/internal/controller/squall"
	"github.com/ackstorm/squall/internal/dstack"
	"github.com/ackstorm/squall/internal/metrics"
	// +kubebuilder:scaffold:imports
)

// Version is stamped at build time via -ldflags (see Makefile build-operator).
var Version = "dev"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(squallv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsServerOptions,
		// D63's RBAC is `get` on secrets and deliberately NOT list/watch, so a
		// compromised controller cannot enumerate a namespace's credentials.
		// controller-runtime's default client serves reads from an informer
		// cache, and an informer needs list+watch — so the first
		// spec.secretEnv resolution starts a Secret informer that is instantly
		// forbidden, and the wake fails with nothing but a reflector warning.
		// Reading Secrets live keeps the narrow RBAC that was the point.
		// MEASURED 2026-08-27 on a real cluster; envtest never sees it because
		// envtest runs as admin.
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "f257cf47.ackstorm.ai",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// §10/AC19 declared/observed gauge pairs, registered with
	// controller-runtime's own metrics registry so they're served on the
	// same /metrics endpoint as everything else above, no second server.
	ageMetrics := metrics.NewModelAgeCollector(nil)
	priceMetrics := metrics.NewModelPriceCollector()
	uncontrolledMetrics := metrics.NewUncontrolledCollector(internalclock.RealClock{})
	ctrlmetrics.Registry.MustRegister(ageMetrics, priceMetrics, uncontrolledMetrics)

	// ModelReconciler.DstackClient has no supported nil mode (see its own
	// doc comment) — every real caller must set it, so fail fast rather
	// than start a manager that panics on its first reconcile. Found
	// missing while wiring Phase 11's e2e cluster: nothing had exercised
	// this binary against a real (or fake) dstack server before.
	dstackURL := os.Getenv("SQUALL_DSTACK_URL")
	if dstackURL == "" {
		setupLog.Error(nil, "SQUALL_DSTACK_URL is required")
		os.Exit(1)
	}
	// dstack scopes every path by project and has no server-wide run
	// namespace (measured, docs/references/dstack-real-api.md §1);
	// SQUALL_DSTACK_PROJECT unset defaults to "main", dstack's own default
	// project name.
	dstackProject := os.Getenv("SQUALL_DSTACK_PROJECT")
	if dstackProject == "" {
		dstackProject = "main"
	}
	dstackHTTPClient := &http.Client{Timeout: 30 * time.Second}
	dstackClient := dstack.NewHTTPClient(dstackURL, dstackProject, os.Getenv("SQUALL_DSTACK_TOKEN"), dstackHTTPClient)

	// ProxyService is the one cluster-wide proxy Service §6's idle
	// evidence is gathered from (see ModelReconciler.ProxyService's doc
	// comment). Both env vars unset leaves it at its zero value, which
	// keeps sleep unreachable until the proxy is wired. Once configured, an
	// empty EndpointSlice list is still treated as incomplete by gatherActivity.
	proxyService := types.NamespacedName{
		Namespace: os.Getenv("SQUALL_PROXY_SERVICE_NAMESPACE"),
		Name:      os.Getenv("SQUALL_PROXY_SERVICE_NAME"),
	}

	idleRequeueInterval, err := time.ParseDuration(os.Getenv("SQUALL_IDLE_REQUEUE_INTERVAL"))
	if err != nil {
		idleRequeueInterval = 0 // ModelReconciler.idleRequeueInterval() defaults this to 15s.
	}

	if err = (&squallcontroller.ModelReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		DstackClient:        dstackClient,
		ProxyService:        proxyService,
		IdleRequeueInterval: idleRequeueInterval,
		AgeMetrics:          ageMetrics,
		PriceMetrics:        priceMetrics,
		UncontrolledMetrics: uncontrolledMetrics,
		// D65: verify a Ready replica serves what spec.model asked for,
		// through dstack's own service proxy — the same base URL and
		// token dstackClient above already uses.
		ServedModels: squallcontroller.HTTPServedModelReader{
			BaseURL: dstackURL,
			Token:   os.Getenv("SQUALL_DSTACK_TOKEN"),
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Model")
		os.Exit(1)
	}

	// D56's belt-and-braces. The finalizer is the primary teardown path;
	// this is the audit that assumes it failed. A delete that silently does
	// not happen bills a GPU forever, so once a minute we re-derive, from
	// scratch, whether anything is running that no Model claims.
	if err = mgr.Add(&squallcontroller.Reaper{
		// D107: the API reader, NEVER mgr.GetClient(). A degraded informer
		// returns a short Model list with a nil error, and every Model
		// missing from that list has its run reaped — the uncached reader
		// is what makes the "refuse to reap on a partial view" abort real.
		Client:       mgr.GetAPIReader(),
		DstackClient: dstackClient,
	}); err != nil {
		setupLog.Error(err, "unable to start the orphan reaper")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "version", Version)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
