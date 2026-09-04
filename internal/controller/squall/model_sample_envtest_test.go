// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// TestSampleModel_AppliesAndDefaultsMaterialise applies config/samples/
// squall_v1alpha1_model.yaml — the spec §5.1 example CR — against a real
// envtest API server and reads it back. It asserts the server-populated
// defaults appear (uid, resourceVersion, generation) and that the CR's own
// values round-trip unchanged. fleet.idleDuration deliberately has no
// default (spec §5.1, F21); health, scaleDownDelaySeconds and drainTimeout
// DO default (D105/D123 — the sample sets all three explicitly, so its
// values must still round-trip untouched here; the defaults themselves are
// asserted by TestMinimalModel_DefaultsScaleDownDelayAndDrainTimeout).
func TestSampleModel_AppliesAndDefaultsMaterialise(t *testing.T) {
	// Needs a control plane, so it belongs to `make test-envtest`, not
	// `make test-unit` (-short). TestMain skips envtest startup under
	// -short, which would leave k8sClient nil here.
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "samples", "squall_v1alpha1_model.yaml"))
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}

	var model squallv1alpha1.Model
	if err := yaml.Unmarshal(raw, &model); err != nil {
		t.Fatalf("unmarshal sample: %v", err)
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: model.Namespace}}
	if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %q: %v", model.Namespace, err)
	}

	if err := k8sClient.Create(ctx, &model); err != nil {
		t.Fatalf("create sample Model: %v", err)
	}

	got := &squallv1alpha1.Model{}
	key := types.NamespacedName{Name: model.Name, Namespace: model.Namespace}
	if err := k8sClient.Get(ctx, key, got); err != nil {
		t.Fatalf("get sample Model: %v", err)
	}

	// Server-populated defaults materialised.
	if got.UID == "" {
		t.Error("metadata.uid not populated by the API server")
	}
	if got.ResourceVersion == "" {
		t.Error("metadata.resourceVersion not populated by the API server")
	}
	if got.Generation != 1 {
		t.Errorf("metadata.generation = %d, want 1", got.Generation)
	}

	// The example's own values round-trip unchanged — no field defaulting
	// silently altered them.
	if got.Spec.Engine != squallv1alpha1.ModelEngineVLLM {
		t.Errorf("spec.engine = %q, want %q", got.Spec.Engine, squallv1alpha1.ModelEngineVLLM)
	}
	if got.Spec.MinReplicas != 0 {
		t.Errorf("spec.minReplicas = %d, want 0", got.Spec.MinReplicas)
	}
	if got.Spec.Fleet.IdleDuration.Duration != 10*time.Minute {
		t.Errorf("spec.fleet.idleDuration = %s, want 10m", got.Spec.Fleet.IdleDuration.Duration)
	}
	if got.Spec.HoldTimeout.Duration != 20*time.Minute {
		t.Errorf("spec.holdTimeout = %s, want 20m", got.Spec.HoldTimeout.Duration)
	}
	if len(got.Spec.Placement.Backends) != 1 || got.Spec.Placement.Backends[0] != "vastai" {
		t.Errorf("spec.placement.backends = %v, want [vastai]", got.Spec.Placement.Backends)
	}

	// status carries no default phase — the controller is the sole writer
	// (§5.2) and hasn't reconciled this fixture.
	if got.Status.Phase != "" {
		t.Errorf("status.phase = %q, want empty (no controller has reconciled this CR)", got.Status.Phase)
	}

	// The CR the sample carries validates cleanly against the Go
	// cross-field rules too.
	if err := Validate(got.Spec); err != nil {
		t.Errorf("Validate(sample) error = %v, want nil", err)
	}
}

// TestMinimalModel_DefaultsScaleDownDelayAndDrainTimeout is D105/D123: a
// schema-valid Model that omits scaleDownDelaySeconds was permanently
// unwakeable (the field is hasDemand's TTL, so zero made the demand
// annotation expire the instant it landed — silently, with no event or
// condition), and one that omits drainTimeout got ZERO drain on delete
// (pastDeadline true on the finalizer's first pass). Both now default at
// admission to the spec's own §5.1 example values; a real API server is
// the only place structural-schema defaulting actually runs, which is why
// this is envtest.
func TestMinimalModel_DefaultsScaleDownDelayAndDrainTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest test: run via make test-envtest")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Built as unstructured YAML, NOT the typed struct: metav1.Duration is
	// a struct, so the typed client serializes an explicit `drainTimeout:
	// "0s"` (omitempty never drops a struct) and admission correctly
	// leaves a PRESENT field alone. Real users write YAML that omits the
	// field — this is that path. The explicit-"0s" case is covered by
	// drainTimeoutOrDefault's own unit test.
	raw := []byte(`
apiVersion: squall.ackstorm.ai/v1alpha1
kind: Model
metadata:
  name: minimal-defaults
  namespace: default
spec:
  engine: ollama
  image: ollama/ollama@sha256:c622a7adec67cf5bd7fe1802b7e26aa583a955a54e91d132889301f50c3e0bd0
  features: [TextGeneration]
  minReplicas: 0
  provisioningTimeout: 30m
  placement:
    backends: [vastai]
  fleet:
    idleDuration: 10m
`)
	var asMap map[string]interface{}
	if err := yaml.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal minimal Model to map: %v", err)
	}
	u := &unstructured.Unstructured{Object: asMap}
	if err := k8sClient.Create(ctx, u); err != nil {
		t.Fatalf("create minimal Model: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), u) })

	got := &squallv1alpha1.Model{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "minimal-defaults", Namespace: "default"}, got); err != nil {
		t.Fatalf("get minimal Model: %v", err)
	}
	if got.Spec.ScaleDownDelaySeconds != 300 {
		t.Errorf("spec.scaleDownDelaySeconds = %d, want the §5.1 default 300 — zero means this Model can never wake (D105)",
			got.Spec.ScaleDownDelaySeconds)
	}
	if got.Spec.DrainTimeout.Duration != 120*time.Second {
		t.Errorf("spec.drainTimeout = %s, want the §5.1 default 120s — zero means zero drain on delete (D123)",
			got.Spec.DrainTimeout.Duration)
	}
}
