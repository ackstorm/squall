// SPDX-License-Identifier: MIT

package proxy

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	k8scache "k8s.io/client-go/tools/cache"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// ModelGVR is the Model CRD's GroupVersionResource. squall-proxy watches it
// via a plain client-go dynamic informer rather than a generated typed
// clientset or controller-runtime's cache — the one informer-cache
// exception to the "no controller-runtime" rule (spec §11, plan Phase 9).
var ModelGVR = schema.GroupVersionResource{
	Group:    squallv1alpha1.GroupVersion.Group,
	Version:  squallv1alpha1.GroupVersion.Version,
	Resource: "models",
}

// ModelSnapshot is one Model's routing-relevant state as seen by the
// proxy's cache — just enough for Decide (§7). The proxy never probes the
// engine or the gateway to learn this (§10's two-lane rule): it reads
// status.phase from the informer cache only.
type ModelSnapshot struct {
	Phase squallv1alpha1.ModelPhase

	// Namespace is the Model CR's own namespace, carried so DynamicPatcher
	// can address the demand patch when the proxy watches ALL namespaces
	// (D103: SQUALL_NAMESPACE="" made the informer cluster-wide but built
	// the patcher a cluster-scoped URL for a namespaced CRD — every patch
	// 404'd, silently, and no Model could wake in a stock install).
	Namespace string

	// Replica is status.replica: the direct SSH path to the live replica's
	// engine port, when the controller has one to offer. nil means forward
	// through dstack's own service proxy instead (ServiceURL below), which is
	// always correct and always available — the direct path is an
	// optimisation, never a requirement.
	Replica *ReplicaEndpoint

	// HoldTimeout is spec.holdTimeout (task 9.2): how long Await blocks a
	// cold request before answering the wait contract on deadline.
	HoldTimeout time.Duration

	// IdleTimeout is spec.idleTimeout — this Model's OWN demand-annotation
	// TTL (LIVE-3, corrected). refreshIntervalFor derives this hold's
	// demand-refresh cadence from it: a single proxy-wide interval cannot be
	// right for every Model at once, since two Models can disagree about
	// their TTL in the same cluster (measured live: a 300s production Model
	// and a 2s e2e fixture, at the same time).
	IdleTimeout time.Duration

	// Created is the Model CR's creationTimestamp, surfaced as OpenAI's
	// required `created` field on /v1/models. It comes from the CR rather
	// than from time.Now() so the listing is stable across proxy restarts —
	// a `created` that moved on every restart would churn a discovery diff
	// that is supposed to be a no-op.
	Created time.Time

	// Features is spec.features, surfaced verbatim on /v1/models. Declared
	// by the CR author, never inferred: nothing the proxy can observe tells
	// it whether an image serves text generation or embeddings.
	Features []string

	// Owner is spec.owner, surfaced as `owned_by`. Empty is normal.
	Owner string

	// ServiceURL is status.serviceURL: dstack's forward path for this
	// Model's run. Empty means the controller has not resolved one, and the
	// proxy must not guess (D25).
	ServiceURL string

	// ServedModel is status.servedModel — what the replica's own
	// /v1/models reported (D65). DIAGNOSTIC ONLY: it can be a comma-joined
	// list, so nothing may forward under it. Use ForwardModel.
	ServedModel string

	// ForwardModel is status.forwardModel: the SINGLE served id the outbound
	// body's "model" field may be rewritten to, so callers only ever need
	// the CR's name. Empty means there is no safe single answer and the
	// caller's own value must be left alone (D100).
	ForwardModel string

	// Schedulable mirrors the Schedulable condition. False means the
	// controller established this Model cannot provision at all, so holding
	// a request for holdTimeout would only delay a certain failure.
	Schedulable bool
}

// ModelCache is the read surface Decide/Await/the /v1/models handler need.
// Production is kept in sync by RunInformerCache; tests construct one with
// NewCache and drive it directly with Set/Delete — no control plane needed
// (a dynamic-informer-over-a-fake-client test still needs none: the fake
// dynamic client is in-process, not envtest).
type ModelCache struct {
	mu     sync.RWMutex
	byName map[string]ModelSnapshot

	subMu sync.Mutex
	subs  map[string]map[chan struct{}]struct{}

	// synced flips once the informer's first full LIST has landed (D111).
	// Until then the cache's emptiness means "haven't looked yet", not
	// "zero Models" — and /healthz must not let kube-proxy route real
	// traffic at a cache that would 404 every model and publish an empty
	// /v1/models list to LiteLLM discovery.
	synced atomic.Bool
}

// NewCache returns an empty, ready-to-use ModelCache.
func NewCache() *ModelCache {
	return &ModelCache{
		byName: make(map[string]ModelSnapshot),
		subs:   make(map[string]map[chan struct{}]struct{}),
	}
}

// SetSynced marks the informer's initial sync complete. Called exactly once
// by RunInformerCache; tests that drive the cache directly may call it too.
func (c *ModelCache) SetSynced() { c.synced.Store(true) }

// Synced reports whether the initial informer sync has completed — the
// readiness gate /healthz answers from (D111).
func (c *ModelCache) Synced() bool { return c.synced.Load() }

// Get reports name's current snapshot and whether it exists at all — hasCR
// in Decide's signature. A model this cache has never seen (or has seen
// deleted) reports hasCR: false, matching "no such Model CR in desired
// state" (§7).
func (c *ModelCache) Get(name string) (ModelSnapshot, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	snap, ok := c.byName[name]
	return snap, ok
}

// List returns every Model name currently known — the /v1/models discovery
// surface's source (task 9.4), read from the cache, never by probing.
func (c *ModelCache) List() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	names := make([]string, 0, len(c.byName))
	for name := range c.byName {
		names = append(names, name)
	}
	return names
}

// Set records name's snapshot and wakes name's Subscribe-rs.
func (c *ModelCache) Set(name string, snap ModelSnapshot) {
	c.mu.Lock()
	c.byName[name] = snap
	c.mu.Unlock()
	c.broadcast(name)
}

// Delete removes name (e.g. the informer observed its deletion) and wakes
// name's Subscribe-rs.
func (c *ModelCache) Delete(name string) {
	c.mu.Lock()
	delete(c.byName, name)
	c.mu.Unlock()
	c.broadcast(name)
}

// Subscribe returns a channel that receives a value whenever NAME's
// snapshot may have changed (Set or Delete), plus a cancel func the caller
// MUST call once done. The channel is buffered by 1 and sends are
// non-blocking, so a burst of changes coalesces into at most one pending
// notification per subscriber rather than backing up.
//
// Keyed by model (D119): Await's notification branch runs tick — a full
// attemptForward — so a GLOBAL broadcast made every unrelated Model status
// write in the cluster fire a forward from every held request: a forward
// storm against a replica that is still waking, scaling with (held
// requests) x (cluster-wide status write rate). A held request only cares
// about its own Model.
func (c *ModelCache) Subscribe(name string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	c.subMu.Lock()
	if c.subs[name] == nil {
		c.subs[name] = make(map[chan struct{}]struct{})
	}
	c.subs[name][ch] = struct{}{}
	c.subMu.Unlock()

	cancel := func() {
		c.subMu.Lock()
		delete(c.subs[name], ch)
		if len(c.subs[name]) == 0 {
			delete(c.subs, name)
		}
		c.subMu.Unlock()
	}
	return ch, cancel
}

func (c *ModelCache) broadcast(name string) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	for ch := range c.subs[name] {
		select {
		case ch <- struct{}{}:
		default: // already has a pending notification; coalesce.
		}
	}
}

// RunInformerCache starts a dynamic informer over Model resources in
// namespace (empty string = all namespaces) and keeps cache in sync until
// ctx is cancelled. It blocks until the informer's local store has synced
// once and then blocks further until ctx is done — callers run it in a
// goroutine.
//
// Reading status.phase off an *unstructured.Unstructured rather than a
// generated typed Model client is what keeps this dependency-thin (spec
// §11): no controller-runtime, no generated clientset for squall.ackstorm.ai
// — just client-go's dynamic package, already required by this module.
func RunInformerCache(ctx context.Context, client dynamic.Interface, namespace string, cache *ModelCache) error {
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, 0, namespace, nil)
	informer := factory.ForResource(ModelGVR).Informer()

	upsert := func(obj interface{}) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
		phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
		holdTimeout, _, _ := unstructured.NestedString(u.Object, "spec", "holdTimeout")
		hold, _ := time.ParseDuration(holdTimeout) // zero value on unset/unparseable — Await then has no deadline slack, which is the safe direction (task 9's "fails open" is about wake, not about inventing a hold).
		idleTimeout, _, _ := unstructured.NestedString(u.Object, "spec", "idleTimeout")
		idle, err := time.ParseDuration(idleTimeout)
		if err != nil {
			// Unparseable or absent resolves to zero, which refreshIntervalFor
			// reads as "no TTL configured" and answers with the proxy-wide
			// ceiling. Guessing a duration here would be worse: a held request
			// refreshing too slowly ages its own demand anchor out (LIVE-3).
			idle = 0
		}
		features, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "features")
		owner, _, _ := unstructured.NestedString(u.Object, "spec", "owner")
		serviceURL, _, _ := unstructured.NestedString(u.Object, "status", "serviceURL")
		servedModel, _, _ := unstructured.NestedString(u.Object, "status", "servedModel")
		forwardModel, _, _ := unstructured.NestedString(u.Object, "status", "forwardModel")
		// Absent conditions mean "not yet evaluated", which must read as
		// schedulable — a Model the controller has not looked at yet is not
		// a Model it has ruled out. 0->1 fails open.
		schedulable := true
		conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		for _, c := range conds {
			m, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			if m["type"] == "Schedulable" && m["status"] == "False" {
				schedulable = false
			}
		}
		cache.Set(u.GetName(), ModelSnapshot{
			Namespace:    u.GetNamespace(),
			Replica:      replicaFromStatus(u),
			Phase:        squallv1alpha1.ModelPhase(phase),
			HoldTimeout:  hold,
			IdleTimeout:  idle,
			Created:      u.GetCreationTimestamp().Time,
			Features:     features,
			Owner:        owner,
			ServiceURL:   serviceURL,
			ServedModel:  servedModel,
			ForwardModel: forwardModel,
			Schedulable:  schedulable,
		})
	}
	remove := func(obj interface{}) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			if tomb, isTomb := obj.(k8scache.DeletedFinalStateUnknown); isTomb {
				u, ok = tomb.Obj.(*unstructured.Unstructured)
			}
			if !ok {
				return
			}
		}
		cache.Delete(u.GetName())
	}

	if _, err := informer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc:    upsert,
		UpdateFunc: func(_, newObj interface{}) { upsert(newObj) },
		DeleteFunc: remove,
	}); err != nil {
		return err
	}

	factory.Start(ctx.Done())
	if !k8scache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return ctx.Err()
	}
	// D111: only now is the cache's content a statement about the cluster
	// rather than about our own empty memory — /healthz keys off this.
	cache.SetSynced()
	<-ctx.Done()
	return nil
}

// ReplicaEndpoint mirrors status.replica. Kept as a proxy-local type rather
// than importing the API struct so this package stays dependency-thin (§11),
// exactly as ModelSnapshot already does for every other field.
type ReplicaEndpoint struct {
	Host        string
	SSHPort     int
	User        string
	ServicePort int
}

// replicaFromStatus reads status.replica, and returns nil unless EVERY field
// needed to actually dial is present. A partial endpoint is worse than none:
// the caller would build a tunnel that cannot connect and fail real user
// requests, where nil simply routes them through dstack's proxy as before.
func replicaFromStatus(u *unstructured.Unstructured) *ReplicaEndpoint {
	m, found, err := unstructured.NestedMap(u.Object, "status", "replica")
	if !found || err != nil {
		return nil
	}
	host, _, _ := unstructured.NestedString(m, "host")
	user, _, _ := unstructured.NestedString(m, "user")
	sshPort, _, _ := unstructured.NestedInt64(m, "sshPort")
	servicePort, _, _ := unstructured.NestedInt64(m, "servicePort")
	if host == "" || user == "" || sshPort <= 0 || servicePort <= 0 {
		return nil
	}
	return &ReplicaEndpoint{
		Host:        host,
		SSHPort:     int(sshPort),
		User:        user,
		ServicePort: int(servicePort),
	}
}
