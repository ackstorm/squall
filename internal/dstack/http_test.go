// SPDX-License-Identifier: MIT

package dstack

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRunSpec_EncodesResourcesAndPlacement pins the JSON that actually
// leaves the process. The mapping being right in Go is not enough — D55 was
// a complete, correct-looking type that no encoder ever read.
func TestRunSpec_EncodesResourcesAndPlacement(t *testing.T) {
	body, err := json.Marshal(runSpec(ApplyRequest{
		Name: "m", Image: "img@sha256:x", Port: 8000, Replicas: 1,
		Resources: &Resources{
			CPUArch: "x86", CPUCount: "4..8",
			Memory: "16GB..", ShmSize: "8GB", Disk: "200GB..",
			GPU: &GPU{Name: []string{"A10G"}, Memory: "24GB..32GB"},
		},
		Env:       map[string]string{"OLLAMA_CONTEXT_LENGTH": "65536", "OLLAMA_KV_CACHE_TYPE": "q8_0"},
		Args:      []string{"--flash-attn"},
		Placement: Placement{Backends: []string{"vastai"}, Regions: []string{"es"}, MaxPrice: "0.80"},
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		`"cpu":{"arch":"x86","count":"4..8"}`,
		`"memory":"16GB.."`,
		`"shm_size":"8GB"`,
		`"disk":{"size":"200GB.."}`,
		`"name":["A10G"]`,
		`"backends":["vastai"]`,
		`"regions":["es"]`,
		`"max_price":"0.80"`,
		// F29's VRAM budget is computed assuming these are set. An engine
		// started without them takes its own defaults — 4k context, f16 KV
		// cache — and OOMs the 24GB card the CR asked for.
		`"OLLAMA_CONTEXT_LENGTH":"65536"`,
		`"OLLAMA_KV_CACHE_TYPE":"q8_0"`,
		`"commands":["--flash-attn"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("encoded run_spec is missing %s\ngot: %s", want, got)
		}
	}
}

// TestRunSpec_NilResourcesSendsNoBlock: omitting `resources` entirely is
// what hands dstack its own defaults. That must be a deliberate, visible
// state, not something a half-filled struct produces by accident.
func TestRunSpec_NilResourcesSendsNoBlock(t *testing.T) {
	body, err := json.Marshal(runSpec(ApplyRequest{Name: "m", Replicas: 1}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), `"resources"`) {
		t.Errorf("a nil Resources encoded a resources block: %s", body)
	}
}

// TestEchoedReadyAfter_UsesTheRunsOwnValue: readiness must be judged
// against the ready_after the run was CREATED with, which dstack echoes
// back, not against a package constant. A run submitted with ReadyAfter 5
// judged against the constant 2 would be called Ready three probes early.
func TestEchoedReadyAfter_UsesTheRunsOwnValue(t *testing.T) {
	var w runWire
	w.RunSpec.Configuration.Probes = []probeConfigWire{{ReadyAfter: 5}}
	if got := echoedReadyAfter(w); got != 5 {
		t.Errorf("echoedReadyAfter = %d, want 5 (the run's own value)", got)
	}
	if got := echoedReadyAfter(runWire{}); got != defaultReadyAfter {
		t.Errorf("echoedReadyAfter with no echo = %d, want the %d fallback", got, defaultReadyAfter)
	}
}

// TestProbe_CRValuesReachTheWire pins that a configured probe is what gets
// submitted, defaults included.
func TestProbe_CRValuesReachTheWire(t *testing.T) {
	got := probe(&Probe{Path: "/ready", Method: "HEAD", IntervalSeconds: 15, ReadyAfter: 5, TimeoutSeconds: 3})
	if got.URL != "/ready" || got.Method != "HEAD" || got.Interval != 15 || got.ReadyAfter != 5 || got.Timeout != 3 {
		t.Errorf("probe = %+v, want the CR's values", got)
	}
	def := probe(nil)
	if def.URL != defaultProbePath || def.Interval != probeIntervalSeconds || def.ReadyAfter != defaultReadyAfter {
		t.Errorf("default probe = %+v, want squall's defaults", def)
	}
	if def.Type != "http" {
		t.Errorf("probe type = %q, want http", def.Type)
	}
}

func TestSSHKeyPub_EncodesAndDecodes(t *testing.T) {
	for _, tt := range []struct {
		name string
		key  string
	}{
		{name: "authorized key", key: "ssh-ed25519 AAAA-replica comment"},
		{name: "empty key", key: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := json.Marshal(runSpec(ApplyRequest{Name: "qwen", SSHKeyPub: tt.key}))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var spec runSpecWire
			if err := json.Unmarshal(encoded, &spec); err != nil {
				t.Fatalf("unmarshal run spec: %v", err)
			}
			if spec.SSHKeyPub != tt.key {
				t.Fatalf("encoded ssh_key_pub = %q, want %q", spec.SSHKeyPub, tt.key)
			}

			body := []byte(`{"id":"run-1","run_spec":{"ssh_key_pub":"` + tt.key + `","configuration":{"replicas":{"min":1,"max":1}}}}`)
			run, err := decodeRun(body)
			if err != nil {
				t.Fatalf("decode run: %v", err)
			}
			if run.SSHKeyPub != tt.key {
				t.Fatalf("decoded SSHKeyPub = %q, want %q", run.SSHKeyPub, tt.key)
			}
		})
	}
}
