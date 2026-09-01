// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"testing"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

func TestTemplateBackend_EmptyTemplateNeverResolves(t *testing.T) {
	b := TemplateBackend{}
	if _, ok := b.URL("qwen"); ok {
		t.Fatal("URL() ok = true with an empty Template, want false")
	}
}

func TestTemplateBackend_FormatsModelIntoTemplate(t *testing.T) {
	b := TemplateBackend{Template: "http://%s.svc.cluster.local:8000"}
	u, ok := b.URL("qwen")
	if !ok {
		t.Fatal("URL() ok = false, want true")
	}
	if got := u.String(); got != "http://qwen.svc.cluster.local:8000" {
		t.Fatalf("URL() = %q, want %q", got, "http://qwen.svc.cluster.local:8000")
	}
}

// TestStatusBackend_ResolvesFromTheModelStatus is D25. The forward target is
// no longer a printf template an operator has to guess and hand-configure:
// the controller writes what dstack told it onto the Model, and the proxy
// reads it from the informer cache it already runs.
func TestStatusBackend_ResolvesFromTheModelStatus(t *testing.T) {
	cache := NewCache()
	cache.Set("m", ModelSnapshot{
		Phase:      squallv1alpha1.ModelPhaseReady,
		ServiceURL: "/proxy/services/main/m/",
	})
	b := StatusBackend{Cache: cache, DstackBaseURL: "http://dstack:3000"}

	u, ok := b.URL("m")
	if !ok {
		t.Fatal("URL(m) not ok, want resolved from status.serviceURL")
	}
	if got := u.String(); got != "http://dstack:3000/proxy/services/main/m" {
		t.Fatalf("URL = %q, want the dstack base joined to the service path with no double slash", got)
	}
}

// TestStatusBackend_UnknownUntilTheControllerSaysSo: no serviceURL means the
// proxy has no target, and it must answer rather than invent one. Guessing
// is how a request reaches a stranger's service.
func TestStatusBackend_UnknownUntilTheControllerSaysSo(t *testing.T) {
	cache := NewCache()
	cache.Set("m", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady})
	b := StatusBackend{Cache: cache, DstackBaseURL: "http://dstack:3000"}
	if _, ok := b.URL("m"); ok {
		t.Fatal("URL(m) resolved with no status.serviceURL; it must not guess a target")
	}
	if _, ok := b.URL("nonexistent"); ok {
		t.Fatal("URL of an unknown model resolved")
	}
}
