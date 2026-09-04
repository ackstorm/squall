// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

func secretEnvScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("core scheme: %v", err)
	}
	if err := squallv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("squall scheme: %v", err)
	}
	return s
}

func hfSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "hf", Namespace: "squall"},
		Data:       map[string][]byte{"token": []byte("hf_real_value")},
	}
}

func modelWithSecretEnv(env map[string]string, secretEnv map[string]squallv1alpha1.SecretKeyRef) *squallv1alpha1.Model {
	return &squallv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "squall"},
		Spec:       squallv1alpha1.ModelSpec{Env: env, SecretEnv: secretEnv},
	}
}

// TestResolveEnv_ReadsTheSecretValue is D63's happy path: the credential
// reaches the run spec while the CR carries only a reference, so nothing
// sensitive is ever committed to Git.
func TestResolveEnv_ReadsTheSecretValue(t *testing.T) {
	m := modelWithSecretEnv(
		map[string]string{"VLLM_LOG": "info"},
		map[string]squallv1alpha1.SecretKeyRef{"HF_TOKEN": {Name: "hf", Key: "token"}},
	)
	c := fake.NewClientBuilder().WithScheme(secretEnvScheme(t)).WithObjects(hfSecret(), m).Build()

	got, err := resolveEnv(context.Background(), c, m)
	if err != nil {
		t.Fatalf("resolveEnv: %v", err)
	}
	if got["HF_TOKEN"] != "hf_real_value" {
		t.Errorf("HF_TOKEN = %q, want the Secret's value", got["HF_TOKEN"])
	}
	if got["VLLM_LOG"] != "info" {
		t.Errorf("plain env was dropped: %v", got)
	}
	// The CR itself must be untouched — a resolved credential written back
	// onto the object would be persisted, and Git is exactly what this
	// field exists to keep it out of.
	if _, leaked := m.Spec.Env["HF_TOKEN"]; leaked {
		t.Error("the resolved secret was written back onto the Model spec")
	}
}

// TestResolveEnv_MissingSecretIsAnError: never degrade to an empty value.
// A run started with HF_TOKEN="" provisions, BILLS, and then fails to
// download — which reads as a broken model rather than a missing
// credential. Same failure shape as D54.
func TestResolveEnv_MissingSecretIsAnError(t *testing.T) {
	m := modelWithSecretEnv(nil, map[string]squallv1alpha1.SecretKeyRef{"HF_TOKEN": {Name: "absent", Key: "token"}})
	c := fake.NewClientBuilder().WithScheme(secretEnvScheme(t)).WithObjects(m).Build()

	got, err := resolveEnv(context.Background(), c, m)
	if err == nil {
		t.Fatalf("resolveEnv succeeded with a missing Secret, returning %v", got)
	}
	if got != nil {
		t.Errorf("returned env %v alongside the error; the Apply must not proceed at all", got)
	}
}

// TestResolveEnv_MissingKeyIsAnError: the Secret existing is not enough.
func TestResolveEnv_MissingKeyIsAnError(t *testing.T) {
	m := modelWithSecretEnv(nil, map[string]squallv1alpha1.SecretKeyRef{"HF_TOKEN": {Name: "hf", Key: "wrong"}})
	c := fake.NewClientBuilder().WithScheme(secretEnvScheme(t)).WithObjects(hfSecret(), m).Build()

	if _, err := resolveEnv(context.Background(), c, m); err == nil {
		t.Fatal("resolveEnv succeeded with a missing key")
	} else if !strings.Contains(err.Error(), "no key") {
		t.Errorf("error = %v, want it to name the missing key", err)
	}
}

// TestResolveEnv_ClashIsRejected: a name in both env and secretEnv must not
// silently resolve one way. Which one won would be invisible in the CR, and
// guessing wrong on a credential is not recoverable.
func TestResolveEnv_ClashIsRejected(t *testing.T) {
	m := modelWithSecretEnv(
		map[string]string{"HF_TOKEN": "placeholder-committed-to-git"},
		map[string]squallv1alpha1.SecretKeyRef{"HF_TOKEN": {Name: "hf", Key: "token"}},
	)
	c := fake.NewClientBuilder().WithScheme(secretEnvScheme(t)).WithObjects(hfSecret(), m).Build()

	if _, err := resolveEnv(context.Background(), c, m); err == nil {
		t.Fatal("a name set in BOTH env and secretEnv was accepted silently")
	}
}

// TestResolveEnv_NoSecretEnvIsAPassthrough keeps the common case free of
// any Secret read at all.
func TestResolveEnv_NoSecretEnvIsAPassthrough(t *testing.T) {
	m := modelWithSecretEnv(map[string]string{"A": "b"}, nil)
	c := fake.NewClientBuilder().WithScheme(secretEnvScheme(t)).WithObjects(m).Build()

	got, err := resolveEnv(context.Background(), c, m)
	if err != nil || got["A"] != "b" {
		t.Fatalf("resolveEnv = %v, %v; want a plain passthrough", got, err)
	}
}
