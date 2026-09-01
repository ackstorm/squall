// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// DynamicPatcher implements Patcher by merge-patching a Model's
// DemandAnnotation over the dynamic client — the production Patcher
// DemandCoalescer drives.
//
// D103: the fixed-Namespace-only version of this cashed in the exact
// hazard its own ponytail comment predicted. The chart's default is
// SQUALL_NAMESPACE="", which RunInformerCache reads as "watch every
// namespace" — but a namespaced CRD cannot be patched through a
// cluster-scoped URL, so Namespace "" made every demand patch 404 and no
// Model could wake in a stock install. When Namespace is empty, the
// Model's own namespace is read off the cache snapshot instead.
type DynamicPatcher struct {
	Client dynamic.Interface
	// Namespace pins every patch to one namespace when set — the
	// single-namespace deployment shape. Empty means per-model resolution
	// via Cache, matching the informer's own all-namespaces reading of "".
	Namespace string
	// Cache resolves a model's namespace in all-namespaces mode. May be
	// nil when Namespace is set.
	Cache *ModelCache
}

func (p *DynamicPatcher) PatchDemand(ctx context.Context, model string, at time.Time) error {
	ns := p.Namespace
	if ns == "" {
		var snap ModelSnapshot
		var ok bool
		if p.Cache != nil {
			snap, ok = p.Cache.Get(model)
		}
		if !ok || snap.Namespace == "" {
			// No CR in the cache means nothing to patch — and guessing a
			// namespace would 404 exactly like the bug this fixes.
			return fmt.Errorf("demand patch for %q: model namespace unknown", model)
		}
		ns = snap.Namespace
	}
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				squallv1alpha1.DemandAnnotation: at.UTC().Format(time.RFC3339),
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = p.Client.Resource(ModelGVR).Namespace(ns).Patch(ctx, model, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}
