// SPDX-License-Identifier: MIT

package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

func TestModelsHandler_ListsFromCacheSortedRegardlessOfPhase(t *testing.T) {
	c := NewCache()
	c.Set("zeta", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseAsleep})
	c.Set("alpha", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady})
	c.Set("mid", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseDead})

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	ModelsHandler(c)(rec, req)

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 3 {
		t.Fatalf("data length = %d, want 3 (discovery lists every known Model regardless of phase)", len(body.Data))
	}
	want := []string{"alpha", "mid", "zeta"}
	for i, w := range want {
		if body.Data[i].ID != w {
			t.Fatalf("data[%d].id = %q, want %q (sorted)", i, body.Data[i].ID, w)
		}
	}
}

// TestModelsHandler_EmitsKubeAICompatibleShape pins the wire shape against
// what a live KubeAI cluster actually serves. id/object/created/owned_by are
// OpenAI's required set for /v1/models — a listing carrying only {"id":...}
// is not OpenAI-compatible, and a consumer stricter than a bare id lookup
// would reject it (F27, F30, task 9.4).
func TestModelsHandler_EmitsKubeAICompatibleShape(t *testing.T) {
	created := time.Date(2026, 8, 27, 6, 59, 29, 0, time.UTC)
	c := NewCache()
	c.Set("qwen3.8-27b", ModelSnapshot{
		Phase:    squallv1alpha1.ModelPhaseAsleep,
		Created:  created,
		Features: []string{"TextGeneration"},
		Owner:    "",
	})

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	ModelsHandler(c)(rec, req)

	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID       string   `json:"id"`
			Created  int64    `json:"created"`
			Object   string   `json:"object"`
			OwnedBy  string   `json:"owned_by"`
			Features []string `json:"features"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Object != "list" {
		t.Fatalf("top-level object = %q, want %q", body.Object, "list")
	}
	if len(body.Data) != 1 {
		t.Fatalf("data length = %d, want 1", len(body.Data))
	}

	got := body.Data[0]
	if got.ID != "qwen3.8-27b" {
		t.Errorf("id = %q, want %q", got.ID, "qwen3.8-27b")
	}
	if got.Object != "model" {
		t.Errorf("object = %q, want %q", got.Object, "model")
	}
	// From the CR's creationTimestamp, not time.Now(): a `created` that moved
	// on every proxy restart would churn a discovery diff meant to be a no-op.
	if got.Created != created.Unix() {
		t.Errorf("created = %d, want %d (the Model CR's creationTimestamp)", got.Created, created.Unix())
	}
	if got.OwnedBy != "" {
		t.Errorf("owned_by = %q, want %q", got.OwnedBy, "")
	}
	// Declared on the CR (spec.features), never inferred by the proxy.
	if len(got.Features) != 1 || got.Features[0] != "TextGeneration" {
		t.Errorf("features = %v, want [TextGeneration] (from spec.features)", got.Features)
	}
}

func TestModelsHandler_EmptyCache(t *testing.T) {
	c := NewCache()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	ModelsHandler(c)(rec, req)

	var body struct {
		Data []interface{} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Data == nil {
		t.Fatal(`data field decoded as null, want "data":[] (empty, not omitted)`)
	}
	if len(body.Data) != 0 {
		t.Fatalf("data length = %d, want 0", len(body.Data))
	}
}
