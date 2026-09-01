// SPDX-License-Identifier: Apache-2.0

// Command squall-proxy is the Squall data path (spec §7): per request it
// forwards when the Model is Ready, blocks while it wakes, and answers the
// wait contract truthfully when its deadline expires. It is a separate
// binary from squall-controller by design (spec §11): separate failure
// domain, separate deploy cadence, stateless, >=2 replicas, and
// deliberately dependency-thin — no controller-runtime, only a small
// client-go dynamic informer (internal/proxy.RunInformerCache).
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"golang.org/x/crypto/ssh"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/clock"
	"github.com/ackstorm/squall/internal/proxy"
)

// Version is stamped at build time via -ldflags (see Makefile build-operator).
var Version = "dev"

// defaultRefreshCeiling is LIVE-3's fix, corrected: the first attempt made
// this a single flat interval for every Model, which is wrong by
// construction — Handler.RefreshInterval is one proxy-wide value but the TTL
// it must stay inside (spec.scaleDownDelaySeconds) is per-Model, and two
// Models CAN and DID disagree about it in the same cluster (a 300s
// production Model and a 2s e2e fixture). proxy.refreshIntervalFor now
// derives the actual per-hold cadence from each Model's own TTL; this
// constant is only the CEILING that derivation is clamped to, and what a
// Model with no TTL configured falls back to entirely. 30s is 1/10 of
// config/samples' representative 300s scaleDownDelaySeconds.
const defaultRefreshCeiling = 30 * time.Second

func main() {
	// D87: the per-request record is Debug for an ordinary forward — a busy
	// proxy must stay readable — while a HELD or failed request logs at Info
	// and is visible by default. SQUALL_LOG_LEVEL is what turns the full
	// per-request stream on when something actually needs diagnosing. Set
	// before anything else so even startup failures honour it.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: envLogLevel("SQUALL_LOG_LEVEL", slog.LevelInfo),
	})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := restConfig()
	if err != nil {
		slog.Error("build kube config failed", "err", err)
		os.Exit(1)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		slog.Error("build dynamic client failed", "err", err)
		os.Exit(1)
	}

	namespace := os.Getenv("SQUALL_NAMESPACE")

	cache := proxy.NewCache()
	go func() {
		if err := proxy.RunInformerCache(ctx, dyn, namespace, cache); err != nil && ctx.Err() == nil {
			slog.Error("informer cache stopped", "err", err)
		}
	}()

	cooldown := envDuration("SQUALL_DEMAND_COOLDOWN", 5*time.Second)
	// Cache resolves each Model's namespace when SQUALL_NAMESPACE is ""
	// (all-namespaces mode, the chart default) — D103.
	patcher := &proxy.DynamicPatcher{Client: dyn, Namespace: namespace, Cache: cache}
	demand := proxy.NewDemandCoalescer(patcher, cooldown, clock.RealClock{})
	activity := proxy.NewActivityTracker(clock.RealClock{})

	// LIVE-3: defaultRefreshInterval used to be cooldown/2 — an accident of
	// arithmetic, not a reasoned fraction of anything — and then, briefly, a
	// single flat interval applied to every Model. Both were wrong for the
	// same reason: cooldown only rate-limits how often a PatchDemand call is
	// allowed to land, and it has no relationship to scaleDownDelaySeconds,
	// the annotation's own expiry TTL, which is set PER MODEL, not once for
	// the whole proxy. proxy.refreshIntervalFor now derives each hold's
	// cadence from that Model's own scaleDownDelaySeconds; RefreshInterval
	// (below) only bounds that derivation from above. See
	// defaultRefreshCeiling.

	// D25: the forward target comes from status.serviceURL, which the
	// controller writes from what dstack reported — not from a printf
	// template an operator has to hand-configure. SQUALL_BACKEND_URL_TEMPLATE
	// is kept ONLY so the e2e model-mock fixtures, which have no dstack
	// server to resolve a status.serviceURL against, keep working.
	tmpl := os.Getenv("SQUALL_BACKEND_URL_TEMPLATE")
	backend := proxy.Backend(proxy.StatusBackend{Cache: cache, DstackBaseURL: os.Getenv("SQUALL_DSTACK_URL")})
	if tmpl != "" {
		backend = proxy.TemplateBackend{Template: tmpl}
	}

	// The measured data path: forward straight to the replica over SSH,
	// leaving dstack's server out of the request path entirely. Measured
	// 2026-08-28 against a live GPU, 128 concurrent: 128/128 at 1857 tok/s
	// direct, against 97/128 and 407 tok/s through dstack, whose proxy pins
	// two database connections per streamed response.
	//
	// It WRAPS the backend above rather than replacing it. Every model
	// without a published endpoint, every topology needing more than one SSH
	// hop, and every failed dial fall through to exactly what ran before, so
	// enabling this cannot fail a request that used to work.
	sshKeyNamespace := envOr("SQUALL_SSH_KEY_NAMESPACE", podNamespace())
	if sshKeyNamespace != "" {
		backend = &proxy.SSHBackend{
			Inner:      backend,
			Cache:      cache,
			LoadSigner: replicaKeyLoader(ctx, dyn, sshKeyNamespace),
		}
	} else {
		slog.Warn("no namespace for the replica SSH key: staying on dstack's proxy for every request",
			"hint", "set SQUALL_SSH_KEY_NAMESPACE")
	}

	// LIVE-4: dstack's service proxy demands a Bearer token by default (see
	// Handler.DstackToken's doc comment). Sourced the same way
	// squall-controller gets its dstack credential — a plain env var, never
	// a ConfigMap, never logged — so both binaries read one Secret-backed
	// Helm value (deploy/helm/squall/templates/proxy-deployment.yaml).
	dstackToken := os.Getenv("SQUALL_DSTACK_TOKEN")
	if msg := dstackAuthWarning(tmpl, dstackToken); msg != "" {
		slog.Error(msg)
	}

	handler := &proxy.Handler{
		Cache:              cache,
		Demand:             demand,
		Activity:           activity,
		Backend:            backend,
		DstackToken:        dstackToken,
		Clock:              clock.RealClock{},
		RefreshInterval:    envDuration("SQUALL_DEMAND_REFRESH_INTERVAL", defaultRefreshCeiling),
		MaxPendingPerModel: envInt("SQUALL_MAX_PENDING_PER_MODEL", 0),
	}

	mux := newMux(cache, activity, handler)

	addr := os.Getenv("SQUALL_PROXY_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("squall-proxy listening", "addr", addr, "version", Version)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("listen failed", "err", err)
		os.Exit(1)
	}
}

// newMux wires squall-proxy's routes. Extracted from main so the routing
// itself is testable: which paths squall answers and which it forwards is a
// behaviour, not glue.
//
// /v1/models is OURS — squall answers it from the informer cache and never
// forwards it, so it is registered for GET and POST alike and every query
// parameter is simply accepted and ignored. A client that POSTs it (or adds
// filters) gets the model list, not a 400 from the forwarding path's
// "missing model" check.
func newMux(cache *proxy.ModelCache, activity *proxy.ActivityTracker, handler http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	// D111: /healthz is also the Deployment's readinessProbe, and it used
	// to answer 200 unconditionally while RunInformerCache synced in a
	// fire-and-forget goroutine — so kube-proxy routed real traffic at an
	// EMPTY cache: every request 404'd with no hold and no demand patch,
	// and /v1/models published a zero-model list to LiteLLM discovery on
	// every rollout. Ready means "my cache is a statement about the
	// cluster", nothing less.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !cache.Synced() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	models := proxy.ModelsHandler(cache)
	mux.HandleFunc("GET /v1/models", models)
	mux.HandleFunc("POST /v1/models", models)
	mux.HandleFunc("GET "+squallv1alpha1.ActivityPath, activity.ServeHTTP)
	mux.Handle("/", handler)
	return mux
}

// restConfig prefers in-cluster config (production) and falls back to
// KUBECONFIG / ~/.kube/config for local dev, matching cmd/controller's
// standard client-go bootstrap.
func restConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// envLogLevel parses a slog level name — debug, info, warn or error, in any
// case — falling back to def. Unset and unparseable both keep def rather than
// failing startup: a typo in a logging setting must never stop the data path
// from serving requests, and the fallback is the safe direction.
func envLogLevel(key string, def slog.Level) slog.Level {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(v)); err != nil {
		return def
	}
	return lvl
}

// dstackAuthWarning is LIVE-4's diagnosability requirement: a missing
// SQUALL_DSTACK_TOKEN must not manifest only as a per-request "gateway auth
// fault" 502 with nothing in the logs explaining why. Pulled out as a pure
// function so the condition is unit-testable without booting the process.
// A non-empty backendURLTemplate means TemplateBackend is in use (the e2e
// model-mock fixture, which fronts no real dstack server) — no token is
// needed on that path, so no warning fires.
func dstackAuthWarning(backendURLTemplate, dstackToken string) string {
	if backendURLTemplate != "" || dstackToken != "" {
		return ""
	}
	return "SQUALL_DSTACK_TOKEN is not set: forwarding to dstack's service proxy will get 403 on every " +
		"request (dstack's default per-service auth requires a Bearer token)"
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// podNamespace reads the namespace this Pod runs in. The replica SSH key
// lives beside the proxy, not beside the Models — SQUALL_NAMESPACE is the
// namespace being WATCHED and is usually a different one.
func podNamespace() string {
	b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// replicaKeyLoader reads squall's replica SSH private key from the Secret the
// controller mints.
//
// Read on DEMAND, not once at startup, and a failure is never cached: on a
// fresh install squall-proxy routinely starts before the controller has
// minted the key, and giving up permanently would strand the proxy on the
// slow path until somebody noticed and restarted it.
func replicaKeyLoader(ctx context.Context, dyn dynamic.Interface, namespace string) func() (ssh.Signer, error) {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	return func() (ssh.Signer, error) {
		u, err := dyn.Resource(gvr).Namespace(namespace).Get(ctx, replicaSSHKeySecret, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		encoded, found, err := unstructured.NestedString(u.Object, "data", replicaSSHKeyField)
		if err != nil || !found {
			return nil, fmt.Errorf("secret %s/%s has no %q", namespace, replicaSSHKeySecret, replicaSSHKeyField)
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode %s/%s: %w", namespace, replicaSSHKeySecret, err)
		}
		return proxy.ParseSigner(raw)
	}
}

const (
	// Must match internal/controller/squall.SSHKeySecretName. Not imported:
	// squall-proxy is deliberately dependency-thin and does not link the
	// controller (§11).
	replicaSSHKeySecret = "squall-replica-ssh-key"
	replicaSSHKeyField  = "id_ed25519"
)
