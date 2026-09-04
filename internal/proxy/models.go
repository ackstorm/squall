// SPDX-License-Identifier: MIT

package proxy

import (
	"encoding/json"
	"net/http"
	"sort"
)

// modelListEntry is one element of the /v1/models "data" array.
//
// The shape mirrors what KubeAI actually serves, verified against a live
// KubeAI cluster, because `type: kubeai` discovery consumes this listing
// (F27, F30, task 9.4):
//
//	{"id":"qwen3-8b-awq","created":1786036412,"object":"model",
//	 "owned_by":"","features":["TextGeneration"]}
//
// id/object/created/owned_by are OpenAI's required set for /v1/models, so
// emitting only {"id":...} is not OpenAI-compatible and a stricter consumer
// than a bare id lookup would reject it. `features` is KubeAI's extension,
// carried so a kubeai-shaped consumer sees the field it expects.
type modelListEntry struct {
	ID       string   `json:"id"`
	Created  int64    `json:"created"`
	Object   string   `json:"object"`
	OwnedBy  string   `json:"owned_by"`
	Features []string `json:"features"`
}

// modelListResponse is the top-level listing. `object: "list"` is part of
// the OpenAI envelope and is what KubeAI emits.
type modelListResponse struct {
	Object string           `json:"object"`
	Data   []modelListEntry `json:"data"`
}

// ModelsHandler serves /v1/models from cache.List() — the informer cache,
// never by probing capacity (§10's two-lane rule: monitoring must not
// generate demand, and the proxy never probes the engine or the gateway to
// answer this). Every Model the cache currently knows about is listed
// regardless of phase: discovery lists desired state, not live readiness —
// Ready-vs-not is what Decide's per-request table is for.
func ModelsHandler(cache *ModelCache) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		names := cache.List()
		sort.Strings(names) // stable output; a discovery diff should not churn on map order.

		resp := modelListResponse{
			Object: "list",
			Data:   make([]modelListEntry, 0, len(names)),
		}
		for _, name := range names {
			snap, ok := cache.Get(name)
			if !ok {
				continue // deleted between List and Get; discovery just omits it.
			}
			features := snap.Features
			if features == nil {
				features = []string{} // encode as [], never null: a consumer indexing the array must not see a nil.
			}
			resp.Data = append(resp.Data, modelListEntry{
				ID:       name,
				Created:  snap.Created.Unix(),
				Object:   "model",
				OwnedBy:  snap.Owner,
				Features: features,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
