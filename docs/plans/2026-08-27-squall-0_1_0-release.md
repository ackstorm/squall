# Squall 0.1.0 Release Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make an installed `squall` chart able to serve a real model on real GPUs without any
hand-editing, and make every way it can fail say so on the `Model` instead of going silent.

**Architecture:** The `Model` CR becomes the single place both halves of the system meet. The
controller writes what it learned from dstack into `status` — the forward URL, the model the
replica actually serves, and conditions naming why a wake cannot proceed — and `squall-proxy`
reads all of it from the informer cache it already runs. No new coordination channel, no new
service discovery, no CRD-to-CRD lookups.

**Tech Stack:** Go 1.26.6, controller-runtime, kubebuilder CRDs, Helm, dstack 0.21.2 REST API,
kind + envtest for tests.

## Global Constraints

- Go **1.26.6**, and every `go`/`make`/`golangci-lint` invocation goes through `./scripts/dev.sh`.
  Never call bare `go` or `make` for build or test. The lint target is `qa-lint`, not `lint`.
- `make test-unit` must NEVER need a control plane. Envtest cases skip under `-short`; e2e files
  sit behind `//go:build e2e`.
- **Squall NEVER sends `force`.** `dstack.ApplyRequest` has no `Force` field. Do not add one.
- **Wake may tolerate uncertainty; sleep must not. `0→1` fails open, `1→0` fails safe.** Any new
  check that could block a wake must fail OPEN (proceed) when the check itself errors — never
  refuse to wake because a verification call timed out.
- The spec of record is `docs/specs/squall-spec-v0_18-RC.md`. Where this plan and the spec
  disagree, the spec wins — say so rather than silently picking one.
- Ledger discipline: append to `docs/references/deviations-and-findings.md`, never renumber,
  never delete an entry. Mark D25/D31/D58/D62/D65/D67 CLOSED only when their task lands.
- **Mutation-test every behaviour claim.** A test that stays green when you break the
  implementation is a finding, not a formality. Batch the sweep at each block boundary.
- Concurrency rules: never `git add -A`, never `git reset`/`--amend`/rebase past a commit you did
  not create, never `git checkout -- <file>` while the tree is dirty. Only add commits.

## Measured facts this plan depends on

Every one of these was verified against a running dstack 0.21.2 on 2026-08-27. Do not re-derive
them, and do not assume they generalise.

| Fact | Evidence |
|---|---|
| `dstack.Run` already carries `ServiceURL` and it is populated | `internal/dstack/client.go:163`, `internal/dstack/http.go:221` |
| A service's forward path is `/proxy/services/{project}/{run_name}/`, relative to the dstack server base URL — no gateway needed | measured; `docs/references/dstack-real-api.md` §8.3 |
| `POST /api/project/{p}/backends/{name}/config_info` returns **200** for a configured backend and **400** for one that is not | measured: `vastai`→200, `kubernetes`→200, `aws`→400 |
| `POST /api/project/{p}/fleets/list` returns the fleets, each with `instances` | measured |
| dstack `commands` is a SHELL SCRIPT joined with ` && `, run as `/bin/sh -i -c`, and it REPLACES the image CMD | D64, measured |
| vLLM, Ollama and llama.cpp all expose `GET /v1/models`, whose `data[].id` is the served name | vLLM measured; Ollama `docs/api/openai-compatibility.mdx` §Endpoints |
| Ollama requires `ollama pull <model>` before use, and `ollama cp <model> <alias>` renames it | Ollama `docs/api/openai-compatibility.mdx` §Models |
| `spec.placement.maxPricePerHour` is `*resource.Quantity` with `x-kubernetes-int-or-string`, so a bare YAML `1.20` (a JSON *number*) is rejected by the API server before Quantity ever sees it | D31, `api/squall/v1alpha1/model_types.go:240` |

---

## File Structure

**Blocks are review boundaries. Do not review per task.**

### Block 1 — the CR contract (Tasks 1–2)
Everything that changes the CRD, in one place, so there is exactly one `make manifests` and one
CRD review.

- `api/squall/v1alpha1/model_types.go` — add `spec.model`, `status.serviceURL`,
  `status.servedModel`; change `maxPricePerHour` to a permissive type.
- `api/squall/v1alpha1/price.go` (new) — the `Price` type accepting string OR number.
- `api/squall/v1alpha1/price_test.go` (new).
- `api/squall/v1alpha1/conditions.go` (new) — condition type/reason constants.
- `deploy/helm/squall/crds/` + `config/crd/` — regenerated.

### Block 2 — the controller learns and publishes (Tasks 3–5)
- `internal/controller/squall/engine.go` — `engineCommands` gains `spec.model`; add
  `engineServedName`.
- `internal/controller/squall/served.go` (new) — `GET /v1/models` reader.
- `internal/controller/squall/preflight.go` (new) — backend + fleet assertion.
- `internal/controller/squall/model_controller.go` — wire all three into reconcile.
- `internal/dstack/client.go`, `internal/dstack/http.go` — `BackendConfigured`, `HasFleet`.

### Block 3 — the proxy consumes it, then release (Tasks 6–8)
- `internal/proxy/backend.go` — `StatusBackend` replaces `TemplateBackend` as the default.
- `internal/proxy/cache.go` — carry `ServiceURL`, `ServedModel`, `Schedulable` in the snapshot.
- `internal/proxy/handler.go` — rewrite the outbound `model` field; short-circuit unschedulable.
- `cmd/proxy/main.go` — construct `StatusBackend`.
- `deploy/helm/squall/templates/*` — deployment renames.
- `CHANGELOG.md` (new), `README.md`, `Makefile`, `Chart.yaml`, `.gitignore`.

---

# BLOCK 1 — the CR contract

### Task 1: `spec.model`, and a price you can write unquoted

**Why together:** both are CRD schema changes. Splitting them means two `make manifests`, two CRD
reviews, and a task whose only deliverable is a regenerated YAML file.

**Files:**
- Modify: `api/squall/v1alpha1/model_types.go` (ModelSpec, ModelPlacement.MaxPricePerHour)
- Create: `api/squall/v1alpha1/price.go`
- Test: `api/squall/v1alpha1/price_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `ModelSpec.Model string`; `type Price string` with
  `func (p Price) String() string`, `func (p *Price) UnmarshalJSON([]byte) error`,
  `func (p Price) MarshalJSON() ([]byte, error)`;
  `ModelPlacement.MaxPricePerHour *Price`.

- [ ] **Step 1: Write the failing test**

`api/squall/v1alpha1/price_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"testing"
)

// TestPrice_AcceptsUnquotedDecimal is D31. `maxPricePerHour: 1.20` in YAML
// reaches the API server as a JSON NUMBER. The old type was
// *resource.Quantity behind x-kubernetes-int-or-string, whose schema is
// anyOf:[integer,string] — a float is neither, so the apply was rejected
// with `must be type integer,string: "number"` before Quantity's own
// unmarshaller ever ran. Both spellings must work now.
func TestPrice_AcceptsUnquotedDecimal(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"unquoted decimal", `1.20`, "1.20"},
		{"quoted decimal", `"1.20"`, "1.20"},
		{"unquoted integer", `2`, "2"},
		{"quoted integer", `"2"`, "2"},
		{"quantity suffix still works", `"2200m"`, "2200m"},
		{"trailing zeros preserved verbatim", `0.80`, "0.80"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p Price
			if err := json.Unmarshal([]byte(tc.in), &p); err != nil {
				t.Fatalf("Unmarshal(%s) errored: %v", tc.in, err)
			}
			if p.String() != tc.want {
				t.Fatalf("Unmarshal(%s) = %q, want %q", tc.in, p.String(), tc.want)
			}
		})
	}
}

// TestPrice_RoundTripsAsAString: whatever went in, dstack receives a string
// (its max_price is a plain JSON field and squall passes ranges through
// opaquely, F33). Marshalling must not turn 0.80 into 0.8 — the value is
// the user's, not ours to normalise.
func TestPrice_RoundTripsAsAString(t *testing.T) {
	var p Price
	if err := json.Unmarshal([]byte(`0.80`), &p); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"0.80"` {
		t.Fatalf("Marshal = %s, want \"0.80\"", out)
	}
}

// TestPrice_RejectsNonsense: a permissive type is not a type-free one. A
// price that is not a number must not reach the provisioner as a silent
// pass-through — a bad max_price either provisions nothing (looks like an
// empty market, D58/D67) or, worse, is ignored.
func TestPrice_RejectsNonsense(t *testing.T) {
	for _, in := range []string{`"cheap"`, `"1.2.3"`, `true`, `{}`, `[]`, `""`} {
		var p Price
		if err := json.Unmarshal([]byte(in), &p); err == nil {
			t.Fatalf("Unmarshal(%s) succeeded as %q, want an error", in, p.String())
		}
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
./scripts/dev.sh go test ./api/squall/v1alpha1/... -run TestPrice -count=1 -short
```

Expected: FAIL, `undefined: Price`.

- [ ] **Step 3: Implement `Price`**

`api/squall/v1alpha1/price.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Price is a per-hour cost ceiling written the way a person writes money.
//
// It exists because the obvious type does not work. `*resource.Quantity`
// behind `x-kubernetes-int-or-string` publishes `anyOf: [integer, string]`,
// and a bare YAML `1.20` arrives as a JSON *number* — neither of those — so
// the API server rejects the object with `must be type integer,string:
// "number"` before Quantity's own permissive unmarshaller is ever reached
// (ledger D31). Quoting it works, but "quote your prices" is a trap laid for
// every user, and it is laid at the exact moment they are trying their first
// model.
//
// So: accept both spellings, keep the ORIGINAL TEXT, and hand dstack a
// string. The text is preserved deliberately — normalising 0.80 to 0.8 would
// silently rewrite a value the user chose, and `max_price` is opaque to
// squall by F33 anyway.
//
// +kubebuilder:validation:Type=""
// +kubebuilder:pruning:PreserveUnknownFields
type Price string

func (p Price) String() string { return string(p) }

// UnmarshalJSON accepts a JSON number or a JSON string. It validates that
// the result parses as a decimal — permissive about SPELLING is not the same
// as permissive about MEANING. An unparseable ceiling would either match no
// offer (indistinguishable from an empty market, D58) or be dropped.
func (p *Price) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var unquoted string
		if err := json.Unmarshal(b, &unquoted); err != nil {
			return fmt.Errorf("maxPricePerHour: %w", err)
		}
		s = unquoted
	}
	if s == "" {
		return fmt.Errorf("maxPricePerHour: empty")
	}
	// Quantity suffixes (e.g. "2200m") are still legal: they are what the
	// previous resource.Quantity typing produced and existing CRs carry them.
	if _, err := strconv.ParseFloat(strings.TrimSuffix(s, "m"), 64); err != nil {
		return fmt.Errorf("maxPricePerHour: %q is not a number", s)
	}
	*p = Price(s)
	return nil
}

// MarshalJSON always emits a string, so what is stored in etcd and what is
// sent to dstack agree, and a round-trip through the API server does not
// change the spelling the user wrote.
func (p Price) MarshalJSON() ([]byte, error) { return json.Marshal(string(p)) }
```

- [ ] **Step 4: Swap the field and add `spec.model`**

In `api/squall/v1alpha1/model_types.go`, replace the `MaxPricePerHour` field (currently at
`:240`) and delete the now-unused `k8s.io/apimachinery/pkg/api/resource` import if nothing else
in the file uses it:

```go
	// MaxPricePerHour is a cost ceiling, enforced by dstack BEFORE
	// provisioning. Write it however you like — `1.20` and `"1.20"` are both
	// accepted (D31).
	// +optional
	MaxPricePerHour *Price `json:"maxPricePerHour,omitempty"`
```

Add to `ModelSpec`, next to `Image`:

```go
	// Model is WHICH model to serve — the weights, not the engine.
	//
	// This is the field whose absence made an Ollama Model unrunnable
	// (ledger D62): spec.image names the ENGINE (`ollama/ollama`,
	// `vllm/vllm-openai`), and for Ollama and llama.cpp nothing else in the
	// CR could say what to load, so the CR started an empty server. Each
	// engine interprets it in its own dialect, because there is no shared
	// registry and pretending otherwise would be a lie:
	//
	//   vllm      a HuggingFace repo    Qwen/Qwen3.8-27B-FP8
	//   ollama    an Ollama tag         qwen3:8b
	//   llama-cpp a HuggingFace repo    bartowski/Qwen3-8B-GGUF
	//
	// Whatever it names, squall makes the replica ALSO answer to this
	// Model's metadata.name, so callers address every engine identically
	// (vLLM via --served-model-name, Ollama via `ollama cp`). That name is
	// what goes in the OpenAI request body.
	//
	// Optional: an image with the weights baked in needs nothing here, and
	// spec.args can always say it the engine's own way instead.
	// +optional
	Model string `json:"model,omitempty"`
```

- [ ] **Step 5: Regenerate and verify both spellings against a real API server**

```bash
./scripts/dev.sh make manifests generate
./scripts/dev.sh go build ./...
./scripts/dev.sh go test ./api/... -count=1 -short
```

Then, against the kind cluster (`export KUBECONFIG=$(pwd)/.gocache/kube/config`):

```bash
kubectl apply -f deploy/helm/squall/crds/
cat <<'EOF' | kubectl apply --dry-run=server -f -
apiVersion: squall.ackstorm.ai/v1alpha1
kind: Model
metadata: {name: price-unquoted, namespace: squall}
spec:
  engine: vllm
  image: vllm/vllm-openai:v0.28.0
  placement: {maxPricePerHour: 1.20}
EOF
```

Expected: `model.squall.ackstorm.ai/price-unquoted created (server dry run)`. Before this task
that exact input fails with `must be type integer,string: "number"`.

- [ ] **Step 6: Commit**

```bash
git add api/squall/v1alpha1/price.go api/squall/v1alpha1/price_test.go \
        api/squall/v1alpha1/model_types.go api/squall/v1alpha1/zz_generated.deepcopy.go \
        config/crd deploy/helm/squall/crds
git commit -m "feat(api): spec.model, and a maxPricePerHour you can write unquoted (D31, D62)"
```

---

### Task 2: status fields and condition vocabulary

**Files:**
- Modify: `api/squall/v1alpha1/model_types.go` (ModelStatus, printcolumns)
- Create: `api/squall/v1alpha1/conditions.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `ModelStatus.ServiceURL string`, `ModelStatus.ServedModel string`; constants
  `ConditionSchedulable`, `ConditionServedModelVerified`, `ReasonBackendUnavailable`,
  `ReasonNoFleet`, `ReasonNoOffers`, `ReasonServedModelMismatch`, `ReasonVerified`,
  `ReasonUnverified`, `ReasonSchedulable`.

- [ ] **Step 1: Add the status fields**

In `ModelStatus` (`api/squall/v1alpha1/model_types.go:431`), after `DeploymentNum`:

```go
	// ServiceURL is where a Ready replica's traffic goes, as dstack reported
	// it — a path relative to the dstack server's base URL, e.g.
	// "/proxy/services/main/qwen3-8-27b/".
	//
	// This closes D25. Before it, squall-proxy resolved a backend from a
	// printf template in an env var, which meant an installed chart could not
	// reach a real replica until an operator hand-edited it to match dstack's
	// routing. The controller already receives this on every Apply
	// (dstack.Run.ServiceURL); it just never wrote it down. Publishing it
	// here means the proxy learns it from the informer cache it is ALREADY
	// running — no new watch, no new API call on the request path.
	// +optional
	ServiceURL string `json:"serviceURL,omitempty"`

	// ServedModel is what the replica said it serves, read from its own
	// GET /v1/models once the run first probes green.
	//
	// This exists because a probe is not evidence about WHICH model answered.
	// On 2026-08-27 a run whose args were dropped served vLLM's built-in
	// default, Qwen/Qwen3-0.6B, with success_streak climbing and /health
	// returning 200 the whole time — a 0.6B model standing in for a 27B one,
	// with nothing anywhere disagreeing (ledger D65). This field is the
	// disagreement.
	// +optional
	ServedModel string `json:"servedModel,omitempty"`
```

- [ ] **Step 2: Add the condition vocabulary**

`api/squall/v1alpha1/conditions.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Condition types on a Model. Phase stays the six-value state machine the
// proxy branches on (§7's table); conditions carry WHY, which phase cannot.
//
// Deliberately NOT a new phase. `Unschedulable` reads well on a CR, but
// every phase value is an input to internal/proxy.Decide, whose two rules
// decide money and correctness — 0→1 fails open, 1→0 fails safe. Widening
// that table is not a change to make in the same commit as a diagnostic
// improvement. A condition reaches the same reader (the proxy's informer
// cache already carries status) without touching the state machine.
const (
	// ConditionSchedulable is False when this Model CANNOT provision for a
	// structural reason: a backend it names is not configured on the dstack
	// server, no fleet admits that backend, or the market returned no offers
	// matching its resources.
	//
	// All three used to be identical from the outside — get_plan returns
	// zero offers and no error — so a misconfigured server was
	// indistinguishable from a busy market, and both were silent (D58, D67).
	ConditionSchedulable = "Schedulable"

	// ConditionServedModelVerified is True once the replica's own
	// GET /v1/models confirmed it serves this Model's name (D65).
	ConditionServedModelVerified = "ServedModelVerified"
)

// Reasons. Kept specific: "the backend is not configured on the server" and
// "the market has nothing right now" call for opposite responses from an
// operator, and the whole point of these conditions is telling them apart.
const (
	ReasonSchedulable         = "Schedulable"
	ReasonBackendUnavailable  = "BackendUnavailable"
	ReasonNoFleet             = "NoFleet"
	ReasonNoOffers            = "NoOffers"
	ReasonVerified            = "Verified"
	ReasonUnverified          = "Unverified"
	ReasonServedModelMismatch = "ServedModelMismatch"
)
```

- [ ] **Step 3: Surface it in `kubectl get`**

Add to the printcolumn markers above `type Model struct` (near `:469`):

```go
// +kubebuilder:printcolumn:name="Served",type=string,JSONPath=`.status.servedModel`,priority=1
// +kubebuilder:printcolumn:name="Schedulable",type=string,JSONPath=`.status.conditions[?(@.type=="Schedulable")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Schedulable")].reason`,priority=1
```

- [ ] **Step 4: Regenerate, build, and eyeball the columns**

```bash
./scripts/dev.sh make manifests generate
./scripts/dev.sh go build ./...
kubectl apply -f deploy/helm/squall/crds/
kubectl get models -n squall           # Schedulable column present, empty for now
```

- [ ] **Step 5: Commit**

```bash
git add api/squall/v1alpha1/conditions.go api/squall/v1alpha1/model_types.go \
        api/squall/v1alpha1/zz_generated.deepcopy.go config/crd deploy/helm/squall/crds
git commit -m "feat(api): status.serviceURL, status.servedModel and Schedulable conditions"
```

**BLOCK 1 GATE — run once, here, not after each task:**

```bash
./scripts/dev.sh make test-unit
./scripts/dev.sh make qa-lint
```

Then dispatch ONE reviewer over the whole block.

---

# BLOCK 2 — the controller learns and publishes

### Task 3: engines can name a model (D62), and Ollama becomes usable

**Files:**
- Modify: `internal/controller/squall/engine.go`
- Test: `internal/controller/squall/engine_commands_test.go` (extend; do not rewrite — its
  existing D64 cases stay)

**Interfaces:**
- Consumes: `ModelSpec.Model` (Task 1).
- Produces: `func engineCommands(spec squallv1alpha1.ModelSpec, name string) []string` —
  **signature change**, it now takes the whole spec plus the Model's `metadata.name` instead of
  `(engine, args)`. `func engineServedName(spec squallv1alpha1.ModelSpec, name string) string`.
  `name` is needed because the alias every engine is given IS the Model's name.

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/squall/engine_commands_test.go`:

```go
// TestEngineCommands_VLLMNamesTheModelAndAliasesIt: --model loads it,
// --served-model-name makes it answer to the CR's name so a caller does not
// need to know the HuggingFace repo.
func TestEngineCommands_VLLMNamesTheModelAndAliasesIt(t *testing.T) {
	got := engineCommands(squallv1alpha1.ModelSpec{
		Engine: squallv1alpha1.ModelEngineVLLM,
		Model:  "Qwen/Qwen3.8-27B-FP8",
	}, "qwen3-8-27b")
	if len(got) != 1 {
		t.Fatalf("got %d elements %q, want 1", len(got), got)
	}
	for _, want := range []string{
		`'vllm' 'serve'`, `'--model' 'Qwen/Qwen3.8-27B-FP8'`, `'--served-model-name'`,
	} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("command %q missing %q", got[0], want)
		}
	}
}

// TestEngineCommands_OllamaPullsThenAliases is D62's actual fix. `ollama
// serve` is a server with NO model loaded; the weights arrive via `ollama
// pull`, and only then does the OpenAI endpoint know the name. `ollama cp`
// then gives it the CR's name, which is Ollama's equivalent of vLLM's
// --served-model-name and is what lets one proxy address every engine the
// same way.
func TestEngineCommands_OllamaPullsThenAliases(t *testing.T) {
	got := engineCommands(squallv1alpha1.ModelSpec{
		Engine: squallv1alpha1.ModelEngineOllama,
		Model:  "qwen3:8b",
	}, "my-model")
	if len(got) != 1 {
		t.Fatalf("got %d elements, want 1", len(got))
	}
	cmd := got[0]
	for _, want := range []string{"ollama serve", "ollama pull 'qwen3:8b'", "ollama cp"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command missing %q:\n%s", want, cmd)
		}
	}
	// The server must still be running when the command "ends", or dstack
	// tears the run down the moment the pull finishes.
	if !strings.Contains(cmd, "wait") {
		t.Fatalf("command must not exit after pulling; it has to keep serving:\n%s", cmd)
	}
	// CLAUDE.md bans naked polling loops: an unbounded `until` here would
	// hang the replica forever if the server dies during startup.
	if strings.Contains(cmd, "until ") && !strings.Contains(cmd, "seq 1") {
		t.Fatalf("unbounded wait loop in the replica command:\n%s", cmd)
	}
}

// TestEngineCommands_ModelAndArgsCoexist: spec.args is the escape hatch for
// anything the CR does not model, so setting spec.model must not silently
// discard it, and vice versa.
func TestEngineCommands_ModelAndArgsCoexist(t *testing.T) {
	got := engineCommands(squallv1alpha1.ModelSpec{
		Engine: squallv1alpha1.ModelEngineVLLM,
		Model:  "Qwen/Qwen3.8-27B-FP8",
		Args:   []string{"--max-model-len", "131072"},
	}, "qwen3-8-27b")
	cmd := got[0]
	if !strings.Contains(cmd, "'--model' 'Qwen/Qwen3.8-27B-FP8'") ||
		!strings.Contains(cmd, "'--max-model-len' '131072'") {
		t.Fatalf("spec.model and spec.args must both survive:\n%s", cmd)
	}
}

// TestEngineServedName_IsAlwaysTheCRName is the invariant that makes one
// proxy work across three engines: whatever dialect spec.model is written
// in, squall aliases the replica to the Model's own name (vLLM
// --served-model-name, Ollama `ollama cp`, llama-server --alias). D65's
// verification compares against this, and the proxy rewrites one field
// instead of learning three dialects.
func TestEngineServedName_IsAlwaysTheCRName(t *testing.T) {
	for _, e := range []squallv1alpha1.ModelEngine{
		squallv1alpha1.ModelEngineVLLM,
		squallv1alpha1.ModelEngineOllama,
		squallv1alpha1.ModelEngineLlamaCpp,
	} {
		spec := squallv1alpha1.ModelSpec{Engine: e, Model: "some/repo"}
		if got := engineServedName(spec, "my-model"); got != "my-model" {
			t.Fatalf("engine %s: engineServedName = %q, want the CR name", e, got)
		}
	}
}

// TestEngineServedName_EmptyWhenNothingWasNamed: with no spec.model squall
// did not choose the entrypoint, so it cannot claim to know what the image
// serves — and D65 must report Unknown rather than a false match.
func TestEngineServedName_EmptyWhenNothingWasNamed(t *testing.T) {
	spec := squallv1alpha1.ModelSpec{Engine: squallv1alpha1.ModelEngineVLLM}
	if got := engineServedName(spec, "my-model"); got != "" {
		t.Fatalf("engineServedName = %q, want empty when spec.model is unset", got)
	}
}
```

- [ ] **Step 2: Run and watch it fail**

```bash
./scripts/dev.sh go test ./internal/controller/squall/... -run 'EngineCommands|EngineServedName' -count=1 -short
```

Expected: FAIL — `too many arguments in call to engineCommands`.

- [ ] **Step 3: Rewrite `engineCommands` and add `engineServedName`**

Replace the body of `engineCommands` in `internal/controller/squall/engine.go`. Keep its existing
doc comment about dstack's `&&`-joined shell (D64) — it is still true and still the reason this
function exists.

```go
// engineCommands builds the replica's start command.
//
// dstack's `commands` is not argv: the runner joins the list with ` && ` and
// hands it to `/bin/sh -i -c`, and a non-empty list REPLACES the image CMD
// (ledger D64/D65 — both measured, both after a GPU had been billed). So
// this returns exactly ONE element, and it restates the engine's entrypoint.
//
// spec.model is rendered in each engine's own dialect, and in every case the
// replica is ALSO made to answer to the Model's name so callers do not need
// to know which dialect was used.
func engineCommands(spec squallv1alpha1.ModelSpec, name string) []string {
	if spec.Model == "" && len(spec.Args) == 0 {
		// Nothing to say: leave the image's own CMD alone. An image with
		// baked-in weights and a correct entrypoint needs no help.
		return nil
	}

	switch spec.Engine {
	case squallv1alpha1.ModelEngineVLLM:
		argv := []string{"vllm", "serve"}
		if spec.Model != "" {
			argv = append(argv, "--model", spec.Model, "--served-model-name", name)
		}
		argv = append(argv, spec.Args...)
		return []string{shellJoin(argv)}

	case squallv1alpha1.ModelEngineOllama:
		if spec.Model == "" {
			return []string{shellJoin(append([]string{"ollama", "serve"}, spec.Args...))}
		}
		// `ollama serve` starts a server with NO model. The weights arrive
		// via `ollama pull`, which needs the server already up, and `ollama
		// cp` then aliases them to the CR's name — Ollama's equivalent of
		// vLLM's --served-model-name.
		//
		// The readiness wait is BOUNDED with an explicit failure path. An
		// `until ollama list; do sleep; done` would spin forever if the
		// server died at startup, leaving a rented GPU running a loop.
		return []string{fmt.Sprintf(
			`ollama serve & `+
				`for i in $(seq 1 60); do ollama list >/dev/null 2>&1 && break; sleep 2; done; `+
				`ollama list >/dev/null 2>&1 || { echo "ollama serve never came up" >&2; exit 1; }; `+
				`ollama pull %s && ollama cp %s %s%s; wait`,
			shellJoin([]string{spec.Model}),
			shellJoin([]string{spec.Model}),
			shellJoin([]string{name}),
			extraArgsSuffix(spec.Args),
		)}

	case squallv1alpha1.ModelEngineLlamaCpp:
		argv := []string{"llama-server", "--host", "0.0.0.0"}
		if spec.Model != "" {
			// llama-server resolves `-hf user/repo` against HuggingFace and
			// --alias is its --served-model-name.
			argv = append(argv, "-hf", spec.Model, "--alias", name)
		}
		argv = append(argv, spec.Args...)
		return []string{shellJoin(argv)}

	default:
		if spec.Model != "" {
			// Refusing is safer than guessing: a wrong entrypoint bills a GPU
			// to run nothing (D65).
			return nil
		}
		return []string{shellJoin(spec.Args)}
	}
}

// extraArgsSuffix appends spec.args to the Ollama form, which is a script
// rather than a single argv.
func extraArgsSuffix(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return " && " + shellJoin(args)
}

// engineServedName is the name the replica will answer to once
// engineCommands has done its work: always the Model's own name, for every
// engine. That uniformity is what D65's check compares against and what lets
// squall-proxy rewrite a single field instead of learning three dialects.
func engineServedName(spec squallv1alpha1.ModelSpec, name string) string {
	if spec.Model == "" {
		return ""
	}
	return name
}
```

Add `"fmt"` to the imports.

- [ ] **Step 4: Update the one call site**

`internal/controller/squall/model_controller.go`, in the `dstack.ApplyRequest`:

```go
			Args:      engineCommands(model.Spec, model.Name),
```

- [ ] **Step 5: Fix the test signatures and run**

The existing D64 tests call `engineCommands(engine, args)`. Update them to
`engineCommands(squallv1alpha1.ModelSpec{Engine: ..., Args: ...}, "test-model")` — keep every
assertion; only the call changes. Same for `engineServedName` in the test written in Step 1
(it takes two arguments).

```bash
./scripts/dev.sh go test ./internal/controller/squall/... -count=1 -short
```

- [ ] **Step 6: Commit**

```bash
git add internal/controller/squall/engine.go \
        internal/controller/squall/engine_commands_test.go \
        internal/controller/squall/model_controller.go
git commit -m "feat(controller): spec.model in every engine's dialect, Ollama included (D62)"
```

---

### Task 4: read the replica's own `/v1/models` (D65)

**Files:**
- Create: `internal/controller/squall/served.go`
- Test: `internal/controller/squall/served_test.go`
- Modify: `internal/controller/squall/model_controller.go`

**Interfaces:**
- Consumes: `ModelStatus.ServiceURL`, `ConditionServedModelVerified` (Tasks 1–2).
- Produces: `type ServedModelReader interface { ServedModels(ctx context.Context, serviceURL string) ([]string, error) }`,
  `type HTTPServedModelReader struct { BaseURL string; Token string; Client *http.Client }`.

- [ ] **Step 1: Write the failing test**

`internal/controller/squall/served_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestServedModels_ReadsOpenAIModelList: /v1/models is the one endpoint all
// three engines agree on, and data[].id is the served name.
func TestServedModels_ReadsOpenAIModelList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy/services/main/m/v1/models" {
			t.Errorf("path = %q, want the service URL with /v1/models appended", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want the dstack token", got)
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen3-8-27b","object":"model"}]}`))
	}))
	defer srv.Close()

	r := HTTPServedModelReader{BaseURL: srv.URL, Token: "tok"}
	got, err := r.ServedModels(context.Background(), "/proxy/services/main/m/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "qwen3-8-27b" {
		t.Fatalf("ServedModels = %v, want [qwen3-8-27b]", got)
	}
}

// TestServedModels_ErrorsAreErrorsNotEmptyLists. An empty slice would read
// as "the replica serves nothing", which the caller must treat as a
// MISMATCH. A transport failure means "unknown" and must never be allowed to
// look like evidence — 0->1 fails open.
func TestServedModels_ErrorsAreErrorsNotEmptyLists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	r := HTTPServedModelReader{BaseURL: srv.URL}
	got, err := r.ServedModels(context.Background(), "/proxy/services/main/m/")
	if err == nil {
		t.Fatalf("404 returned %v and no error; an unreachable replica is not evidence", got)
	}
}

// TestVerifyServedModel_MismatchIsReported is the D65 scenario exactly: the
// engine is healthy and answering, and it is serving something else.
func TestVerifyServedModel_MismatchIsReported(t *testing.T) {
	tests := []struct {
		name    string
		served  []string
		want    string
		matched bool
	}{
		{"exact match", []string{"qwen3-8-27b"}, "qwen3-8-27b", true},
		{"the D65 failure", []string{"Qwen/Qwen3-0.6B"}, "qwen3-8-27b", false},
		{"one of several", []string{"other", "qwen3-8-27b"}, "qwen3-8-27b", true},
		{"serving nothing", nil, "qwen3-8-27b", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := servedModelMatches(tc.served, tc.want); got != tc.matched {
				t.Fatalf("servedModelMatches(%v, %q) = %v, want %v",
					tc.served, tc.want, got, tc.matched)
			}
		})
	}
}
```

- [ ] **Step 2: Run and watch it fail**

```bash
./scripts/dev.sh go test ./internal/controller/squall/... -run 'ServedModel' -count=1 -short
```

Expected: FAIL, `undefined: HTTPServedModelReader`.

- [ ] **Step 3: Implement**

`internal/controller/squall/served.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ServedModelReader asks a replica what it is serving.
//
// This exists because a green probe is not evidence about WHICH model
// answered. Measured 2026-08-27: a run whose args had been dropped served
// vLLM's built-in default (Qwen/Qwen3-0.6B) while success_streak climbed and
// /health returned 200 — a 0.6B model standing in for a 27B one, with every
// signal squall had agreeing it was fine (ledger D65).
//
// GET /v1/models is the one endpoint vLLM, Ollama and llama.cpp all expose,
// and data[].id is the served name.
type ServedModelReader interface {
	ServedModels(ctx context.Context, serviceURL string) ([]string, error)
}

// HTTPServedModelReader reads through dstack's own service proxy, so it
// needs no route to the replica that the controller does not already have.
type HTTPServedModelReader struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func (r HTTPServedModelReader) ServedModels(ctx context.Context, serviceURL string) ([]string, error) {
	if serviceURL == "" {
		return nil, fmt.Errorf("served models: no service URL")
	}
	url := strings.TrimSuffix(r.BaseURL, "/") + "/" + strings.Trim(serviceURL, "/") + "/v1/models"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}

	c := r.Client
	if c == nil {
		// Bounded: this runs inside a reconcile, and a replica that hangs
		// must not hold the work queue.
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("served models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// NOT an empty list. An empty list means "serving nothing", which is
		// a mismatch; an unreachable replica means "unknown", which must
		// never be turned into evidence.
		return nil, fmt.Errorf("served models: HTTP %d", resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("served models: decode: %w", err)
	}
	out := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// servedModelMatches reports whether the replica answers to want.
func servedModelMatches(served []string, want string) bool {
	for _, s := range served {
		if s == want {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Wire it into reconcile**

In `internal/controller/squall/model_controller.go`, add a `ServedModels ServedModelReader` field
to `ModelReconciler`, and after the phase decision resolves to Ready, before writing status:

```go
	// D65: a probe proves an HTTP server answered, not which model it was.
	// Ask the replica directly, ONCE per run generation (skip when
	// status.servedModel is already set for this deployment).
	if phase == squallv1alpha1.ModelPhaseReady && r.ServedModels != nil && model.Status.ServedModel == "" {
		want := engineServedName(model.Spec, model.Name)
		served, err := r.ServedModels.ServedModels(ctx, model.Status.ServiceURL)
		switch {
		case err != nil:
			// FAILS OPEN. A verification that could not run is not a
			// verdict, and refusing to serve because a check timed out
			// would break the wake path for a diagnostic.
			log.Info("could not verify served model; continuing", "error", err)
			meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
				Type: squallv1alpha1.ConditionServedModelVerified, Status: metav1.ConditionUnknown,
				Reason: squallv1alpha1.ReasonUnverified, Message: err.Error(),
			})
		case want != "" && !servedModelMatches(served, want):
			// LOUD. This is the case that shipped a 0.6B model as a 27B one.
			log.Error(nil, "REPLICA IS SERVING THE WRONG MODEL",
				"want", want, "served", served)
			model.Status.ServedModel = strings.Join(served, ",")
			meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
				Type: squallv1alpha1.ConditionServedModelVerified, Status: metav1.ConditionFalse,
				Reason:  squallv1alpha1.ReasonServedModelMismatch,
				Message: fmt.Sprintf("replica serves %v, expected %q", served, want),
			})
		default:
			model.Status.ServedModel = strings.Join(served, ",")
			meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
				Type: squallv1alpha1.ConditionServedModelVerified, Status: metav1.ConditionTrue,
				Reason: squallv1alpha1.ReasonVerified,
			})
		}
	}
```

Clear `model.Status.ServedModel` wherever `RunID` is cleared (a new run generation must re-verify).

In `cmd/controller/main.go`, construct it next to the dstack client:

```go
		ServedModels: squall.HTTPServedModelReader{
			BaseURL: os.Getenv("SQUALL_DSTACK_URL"),
			Token:   os.Getenv("SQUALL_DSTACK_TOKEN"),
		},
```

- [ ] **Step 5: Run**

```bash
./scripts/dev.sh go test ./internal/controller/squall/... -count=1 -short
```

- [ ] **Step 6: Commit**

```bash
git add internal/controller/squall/served.go internal/controller/squall/served_test.go \
        internal/controller/squall/model_controller.go cmd/controller/main.go
git commit -m "feat(controller): verify the replica serves the model the CR asked for (D65)"
```

---

### Task 5: say why a wake cannot happen (D58, D67)

**Files:**
- Create: `internal/controller/squall/preflight.go`
- Test: `internal/controller/squall/preflight_test.go`
- Modify: `internal/dstack/client.go`, `internal/dstack/http.go`, `internal/dstack/mock/mock.go`,
  `internal/controller/squall/model_controller.go`

**Interfaces:**
- Consumes: `ConditionSchedulable` and its reasons (Task 2).
- Produces: on `dstack.Client`, `BackendConfigured(ctx context.Context, backend string) (bool, error)`
  and `HasFleetFor(ctx context.Context, backend string) (bool, error)`;
  `func preflight(ctx context.Context, c dstack.Client, backends []string) (reason, message string)`
  returning `("", "")` when schedulable.

- [ ] **Step 1: Write the failing test**

`internal/controller/squall/preflight_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"context"
	"errors"
	"testing"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

type fakePreflight struct {
	configured map[string]bool
	fleets     map[string]bool
	err        error
}

func (f fakePreflight) BackendConfigured(_ context.Context, b string) (bool, error) {
	return f.configured[b], f.err
}
func (f fakePreflight) HasFleetFor(_ context.Context, b string) (bool, error) {
	return f.fleets[b], f.err
}

// TestPreflight_NamesTheActualProblem. Before this, a backend that was not
// configured, a backend with no fleet, and a genuinely empty market were all
// the same observable: get_plan returns zero offers and no error (D58, D67).
// Telling them apart is the whole point — they call for opposite responses.
func TestPreflight_NamesTheActualProblem(t *testing.T) {
	tests := []struct {
		name       string
		backends   []string
		fake       fakePreflight
		wantReason string
	}{
		{
			name:     "all good",
			backends: []string{"vastai"},
			fake: fakePreflight{
				configured: map[string]bool{"vastai": true},
				fleets:     map[string]bool{"vastai": true},
			},
			wantReason: "",
		},
		{
			name:     "backend not configured on the server",
			backends: []string{"vastai"},
			fake:     fakePreflight{configured: map[string]bool{}},
			wantReason: squallv1alpha1.ReasonBackendUnavailable,
		},
		{
			name:     "configured but no fleet admits it",
			backends: []string{"vastai"},
			fake: fakePreflight{
				configured: map[string]bool{"vastai": true},
				fleets:     map[string]bool{},
			},
			wantReason: squallv1alpha1.ReasonNoFleet,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := preflight(context.Background(), tc.fake, tc.backends)
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q (msg %q)", reason, tc.wantReason, msg)
			}
			if reason != "" && msg == "" {
				t.Fatal("a reason with no message tells an operator nothing")
			}
		})
	}
}

// TestPreflight_FailsOpenWhenItCannotTell. 0->1 fails open: a preflight that
// could not run must not block a wake. Paying for a GPU a little longer is
// always preferable to refusing to serve because a diagnostic call failed.
func TestPreflight_FailsOpenWhenItCannotTell(t *testing.T) {
	f := fakePreflight{err: errors.New("dstack unreachable")}
	if reason, _ := preflight(context.Background(), f, []string{"vastai"}); reason != "" {
		t.Fatalf("reason = %q, want none: an unreachable dstack is not proof of misconfiguration", reason)
	}
}

// TestPreflight_EmptyBackendListIsSchedulable: an empty spec.placement.backends
// means "any configured backend", which squall cannot pre-check.
func TestPreflight_EmptyBackendListIsSchedulable(t *testing.T) {
	if reason, _ := preflight(context.Background(), fakePreflight{}, nil); reason != "" {
		t.Fatalf("reason = %q, want none for an unconstrained Model", reason)
	}
}
```

- [ ] **Step 2: Run and watch it fail**

```bash
./scripts/dev.sh go test ./internal/controller/squall/... -run TestPreflight -count=1 -short
```

Expected: FAIL, `undefined: preflight`.

- [ ] **Step 3: Add the two client methods**

In `internal/dstack/client.go`, extend the `Client` interface:

```go
	// BackendConfigured reports whether backend is configured on the dstack
	// server for this project. MEASURED: POST
	// /api/project/{p}/backends/{name}/config_info answers 200 when it is and
	// 400 when it is not.
	BackendConfigured(ctx context.Context, backend string) (bool, error)

	// HasFleetFor reports whether some active fleet admits backend. A run
	// needs a fleet on EVERY backend, not just Kubernetes, and without one
	// get_plan returns zero offers silently (D58).
	HasFleetFor(ctx context.Context, backend string) (bool, error)
```

In `internal/dstack/http.go`:

```go
func (c *HTTPClient) BackendConfigured(ctx context.Context, backend string) (bool, error) {
	_, err := c.post(ctx, c.projectPath("backends/"+backend+"/config_info"), struct{}{})
	if err == nil {
		return true, nil
	}
	// 400 is dstack's answer for "no such backend in this project". Any
	// other failure is OUR ignorance, not the server's answer, and the
	// caller must be able to tell those apart to fail open.
	var he *HTTPError
	if errors.As(err, &he) && he.StatusCode == http.StatusBadRequest {
		return false, nil
	}
	return false, err
}

func (c *HTTPClient) HasFleetFor(ctx context.Context, backend string) (bool, error) {
	body, err := c.post(ctx, c.projectPath("fleets/list"), struct{}{})
	if err != nil {
		return false, err
	}
	var fleets []struct {
		Status string `json:"status"`
		Spec   struct {
			Configuration struct {
				Backends []string `json:"backends"`
			} `json:"configuration"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &fleets); err != nil {
		return false, fmt.Errorf("dstack: decode fleets: %w", err)
	}
	for _, f := range fleets {
		if f.Status != "active" {
			continue
		}
		// A fleet with no backends listed accepts any of them.
		if len(f.Spec.Configuration.Backends) == 0 {
			return true, nil
		}
		for _, b := range f.Spec.Configuration.Backends {
			if b == backend {
				return true, nil
			}
		}
	}
	return false, nil
}
```

If `HTTPError` and `projectPath` do not already exist with these shapes, adapt to what
`internal/dstack/http.go` actually has rather than inventing them — check first, and record any
deviation in the ledger.

Add matching methods to `internal/dstack/mock/mock.go` (default: configured and fleeted, so
existing tests keep passing) and to any other `Client` implementation the build reveals.

- [ ] **Step 4: Implement `preflight`**

`internal/controller/squall/preflight.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"context"
	"fmt"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// preflightClient is the slice of dstack.Client preflight needs. Narrow on
// purpose: it makes the fake in the test three lines instead of thirty.
type preflightClient interface {
	BackendConfigured(ctx context.Context, backend string) (bool, error)
	HasFleetFor(ctx context.Context, backend string) (bool, error)
}

// preflight answers "why can this Model not provision?" BEFORE spending a
// get_plan on it, and returns ("", "") when there is no such reason.
//
// It exists because all the interesting failures looked identical from
// outside: an unconfigured backend, a backend no fleet admits, and a market
// with nothing available ALL surface as zero offers and no error (D58, D67).
// That cost 25 minutes of bisecting resource filters that were never the
// problem, and it is the first thing a new user will hit.
//
// It FAILS OPEN by construction: any error talking to dstack yields no
// reason at all. A diagnostic must never be what stops a wake.
func preflight(ctx context.Context, c preflightClient, backends []string) (reason, message string) {
	if len(backends) == 0 {
		// "Any configured backend" — nothing to check against.
		return "", ""
	}
	var unconfigured, unfleeted []string
	for _, b := range backends {
		ok, err := c.BackendConfigured(ctx, b)
		if err != nil {
			return "", ""
		}
		if !ok {
			unconfigured = append(unconfigured, b)
			continue
		}
		hasFleet, err := c.HasFleetFor(ctx, b)
		if err != nil {
			return "", ""
		}
		if !hasFleet {
			unfleeted = append(unfleeted, b)
		}
	}
	switch {
	case len(unconfigured) == len(backends):
		return squallv1alpha1.ReasonBackendUnavailable, fmt.Sprintf(
			"no backend in spec.placement.backends %v is configured on the dstack server; "+
				"provisioning would return zero offers with no error", backends)
	case len(unfleeted)+len(unconfigured) == len(backends):
		return squallv1alpha1.ReasonNoFleet, fmt.Sprintf(
			"no active fleet admits %v; dstack needs a fleet per backend and "+
				"returns zero offers without one", unfleeted)
	default:
		return "", ""
	}
}
```

- [ ] **Step 5: Wire it into reconcile, and log it**

In `model_controller.go`, immediately before the `0→1` Apply:

```go
	if reason, msg := preflight(ctx, r.DstackClient, enginePlacement(model.Spec.Placement).Backends); reason != "" {
		// The operator log, because a condition nobody reads is still
		// silence — and this is the failure a first-time user hits.
		log.Error(nil, "model cannot be scheduled", "reason", reason, "detail", msg)
		meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
			Type: squallv1alpha1.ConditionSchedulable, Status: metav1.ConditionFalse,
			Reason: reason, Message: msg,
		})
	} else {
		meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
			Type: squallv1alpha1.ConditionSchedulable, Status: metav1.ConditionTrue,
			Reason: squallv1alpha1.ReasonSchedulable,
		})
	}
```

Note it does NOT skip the Apply: `0→1` fails open, and the market may prove the preflight wrong.
The condition is diagnosis, not a veto.

- [ ] **Step 6: Also write `status.serviceURL` (D25's controller half)**

Right after a successful `Apply`, alongside `model.Status.RunID`:

```go
		model.Status.ServiceURL = run.ServiceURL
```

- [ ] **Step 7: Run**

```bash
./scripts/dev.sh go test ./internal/controller/squall/... ./internal/dstack/... -count=1 -short
```

- [ ] **Step 8: Commit**

```bash
git add internal/controller/squall/preflight.go internal/controller/squall/preflight_test.go \
        internal/controller/squall/model_controller.go internal/dstack/client.go \
        internal/dstack/http.go internal/dstack/mock/mock.go
git commit -m "feat(controller): say why a wake cannot happen instead of returning zero offers (D58, D67)"
```

**BLOCK 2 GATE:**

```bash
./scripts/dev.sh make test-unit
./scripts/dev.sh make test-envtest
./scripts/dev.sh make qa-lint
```

**Mutation sweep for Block 2 — run all of these, each must go RED:**

| Mutation | Test that must fail |
|---|---|
| `engineCommands` drops `--served-model-name` | `TestEngineCommands_VLLMNamesTheModelAndAliasesIt` |
| Ollama branch omits `ollama cp` | `TestEngineCommands_OllamaPullsThenAliases` |
| Ollama branch drops the trailing `wait` | `TestEngineCommands_OllamaPullsThenAliases` |
| `ServedModels` returns `nil, nil` on a non-200 | `TestServedModels_ErrorsAreErrorsNotEmptyLists` |
| `servedModelMatches` returns `true` unconditionally | `TestVerifyServedModel_MismatchIsReported` |
| `preflight` returns `ReasonBackendUnavailable` on client error | `TestPreflight_FailsOpenWhenItCannotTell` |
| `preflight` conflates NoFleet and BackendUnavailable | `TestPreflight_NamesTheActualProblem` |

Then dispatch ONE reviewer over the whole block.

---

# BLOCK 3 — the proxy consumes it, then release

### Task 6: the proxy learns the forward URL from the Model (D25)

**Files:**
- Modify: `internal/proxy/backend.go`, `internal/proxy/cache.go`, `internal/proxy/handler.go`,
  `internal/proxy/attempt.go`, `cmd/proxy/main.go`
- Test: `internal/proxy/backend_test.go` (extend), `internal/proxy/handler_test.go` (extend)

**Interfaces:**
- Consumes: `ModelStatus.ServiceURL`, `ModelStatus.ServedModel`, `ConditionSchedulable` (Block 1),
  written by Block 2.
- Produces: `type StatusBackend struct { Cache *ModelCache; DstackBaseURL string }` implementing
  `Backend`; `ModelSnapshot` gains `ServiceURL string`, `ServedModel string`, `Schedulable bool`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/proxy/backend_test.go`:

```go
// TestStatusBackend_ResolvesFromTheModelStatus is D25. The forward target is
// no longer a printf template an operator has to guess and hand-configure:
// the controller writes what dstack told it onto the Model, and the proxy
// reads it from the informer cache it already runs.
func TestStatusBackend_ResolvesFromTheModelStatus(t *testing.T) {
	cache := NewModelCache()
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
	cache := NewModelCache()
	cache.Set("m", ModelSnapshot{Phase: squallv1alpha1.ModelPhaseReady})
	b := StatusBackend{Cache: cache, DstackBaseURL: "http://dstack:3000"}
	if _, ok := b.URL("m"); ok {
		t.Fatal("URL(m) resolved with no status.serviceURL; it must not guess a target")
	}
	if _, ok := b.URL("nonexistent"); ok {
		t.Fatal("URL of an unknown model resolved")
	}
}
```

Append to `internal/proxy/handler_test.go`:

```go
// TestServeHTTP_RewritesTheModelFieldToTheServedName: callers address a
// Model by its Kubernetes name. Every engine is made to answer to that name
// (vLLM --served-model-name, Ollama `ollama cp`), so this is normally a
// no-op — but when status.servedModel says otherwise, the upstream body must
// carry what the ENGINE knows or it answers 404 for its own model.
func TestServeHTTP_RewritesTheModelFieldToTheServedName(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = body.Model
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cache := NewModelCache()
	cache.Set("m", ModelSnapshot{
		Phase: squallv1alpha1.ModelPhaseReady, ServiceURL: "/", ServedModel: "qwen3:8b",
	})
	h := &Handler{
		Cache:    cache,
		Demand:   noopDemand(t),
		Activity: NewActivityTracker(),
		Backend:  StatusBackend{Cache: cache, DstackBaseURL: upstream.URL},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "qwen3:8b" {
		t.Fatalf("upstream saw model=%q, want the served name qwen3:8b", seen)
	}
}

// TestServeHTTP_UnschedulableAnswersImmediately: holding a request for
// spec.holdTimeout is right while a model is COMING UP. When the controller
// has already established it cannot provision at all (no backend, no fleet),
// a 20-minute hold is 20 minutes of lying to the caller.
func TestServeHTTP_UnschedulableAnswersImmediately(t *testing.T) {
	cache := NewModelCache()
	cache.Set("m", ModelSnapshot{
		Phase:       squallv1alpha1.ModelPhaseAsleep,
		HoldTimeout: time.Hour,
		Schedulable: false,
	})
	h := &Handler{Cache: cache, Demand: noopDemand(t), Activity: NewActivityTracker()}

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[]}`)))
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("held an unschedulable model; the caller should be told, not stalled")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
```

- [ ] **Step 2: Run and watch them fail**

```bash
./scripts/dev.sh go test ./internal/proxy/... -run 'StatusBackend|Rewrites|Unschedulable' -count=1 -short
```

- [ ] **Step 3: Carry the new fields in the snapshot**

`internal/proxy/cache.go` — add to `ModelSnapshot`:

```go
	// ServiceURL is status.serviceURL: dstack's forward path for this
	// Model's run. Empty means the controller has not resolved one, and the
	// proxy must not guess (D25).
	ServiceURL string

	// ServedModel is status.servedModel — what the replica's own
	// /v1/models reported (D65). The outbound body's "model" field is
	// rewritten to this so callers only ever need the CR's name.
	ServedModel string

	// Schedulable mirrors the Schedulable condition. False means the
	// controller established this Model cannot provision at all, so holding
	// a request for holdTimeout would only delay a certain failure.
	Schedulable bool
```

In `RunInformerCache`'s `upsert` (`internal/proxy/cache.go:163`):

```go
		serviceURL, _, _ := unstructured.NestedString(u.Object, "status", "serviceURL")
		servedModel, _, _ := unstructured.NestedString(u.Object, "status", "servedModel")
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
```

and pass them into `cache.Set`.

- [ ] **Step 4: Implement `StatusBackend`**

Append to `internal/proxy/backend.go` (keep `TemplateBackend` — the e2e model-mock fixtures use
it, and deleting it is not this task's job):

```go
// StatusBackend resolves a Model's forward target from its own status,
// which is where the controller records what dstack told it (D25).
//
// The previous default was a printf template in an env var. That is why an
// installed chart could not reach a real replica without an operator first
// working out dstack's routing and hand-editing a value — and why a
// mis-set template produced a proxy that was healthy and inert (D54).
type StatusBackend struct {
	Cache *ModelCache
	// DstackBaseURL is the dstack server, e.g.
	// http://dstack.squall-system.svc.cluster.local:3000. status.serviceURL
	// is a path relative to it, not an absolute URL.
	DstackBaseURL string
}

func (b StatusBackend) URL(model string) (*url.URL, bool) {
	if b.Cache == nil || b.DstackBaseURL == "" {
		return nil, false
	}
	snap, ok := b.Cache.Get(model)
	if !ok || snap.ServiceURL == "" {
		// The controller has not resolved a target. Answering 502 is right;
		// guessing one would forward a caller's payload at an address nobody
		// chose.
		return nil, false
	}
	u, err := url.Parse(strings.TrimSuffix(b.DstackBaseURL, "/") + "/" + strings.Trim(snap.ServiceURL, "/"))
	if err != nil {
		return nil, false
	}
	return u, true
}
```

Add `"strings"` to the imports.

- [ ] **Step 5: Rewrite the outbound model field, and short-circuit unschedulable**

In `internal/proxy/attempt.go`'s `attemptForward`, after the body is chosen and before the
request is built, swap the `model` field when the served name differs:

```go
	// The caller addresses a Model by its Kubernetes name; the engine knows
	// whatever name it was started with. squall makes those equal for every
	// engine it starts, so this is normally a no-op — but when they differ
	// the engine 404s for its own model, and rewriting one field is cheaper
	// than teaching the proxy three engines' dialects.
	if snap, ok := h.Cache.Get(model); ok && snap.ServedModel != "" && snap.ServedModel != model {
		if rewritten, err := rewriteModelField(body, snap.ServedModel); err == nil {
			body = rewritten
		}
	}
```

Add to `internal/proxy/attempt.go`:

```go
// rewriteModelField replaces the OpenAI body's "model" with served, leaving
// every other field untouched. Returns an error rather than a guess when the
// body is not the JSON object we already parsed it as upstream.
func rewriteModelField(body []byte, served string) ([]byte, error) {
	if body == nil {
		return nil, errors.New("no buffered body")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(served)
	if err != nil {
		return nil, err
	}
	obj["model"] = raw
	return json.Marshal(obj)
}
```

In `internal/proxy/handler.go`, inside `ServeHTTP` right after `action := Decide(...)`:

```go
	// A Model the controller has ruled unschedulable will not become Ready
	// by waiting. Hold only while something is actually coming up (§7).
	if action.Block && hasCR && !snap.Schedulable {
		h.answerWait(w, Action{
			DeadlineStatus: http.StatusServiceUnavailable,
			DeadlineState:  WaitAsleep,
		})
		return
	}
```

- [ ] **Step 6: Construct it in `cmd/proxy/main.go`**

Replace the `Backend:` line:

```go
		Backend: proxy.StatusBackend{
			Cache:         cache,
			DstackBaseURL: os.Getenv("SQUALL_DSTACK_URL"),
		},
```

Keep `SQUALL_BACKEND_URL_TEMPLATE` honoured when set, so the model-mock e2e fixtures keep working:

```go
		backend := proxy.Backend(proxy.StatusBackend{Cache: cache, DstackBaseURL: os.Getenv("SQUALL_DSTACK_URL")})
		if tmpl := os.Getenv("SQUALL_BACKEND_URL_TEMPLATE"); tmpl != "" {
			backend = proxy.TemplateBackend{Template: tmpl}
		}
```

- [ ] **Step 7: Chart wiring**

`deploy/helm/squall/templates/proxy-deployment.yaml` — add `SQUALL_DSTACK_URL` from
`.Values.controller.env.dstackURL`, and make `backendURLTemplate` default to empty in
`values.yaml` with a comment saying it is an override for tests only.

- [ ] **Step 8: Run**

```bash
./scripts/dev.sh go test ./internal/proxy/... -count=1 -short
```

- [ ] **Step 9: Commit**

```bash
git add internal/proxy cmd/proxy/main.go deploy/helm/squall/templates/proxy-deployment.yaml \
        deploy/helm/squall/values.yaml
git commit -m "feat(proxy): resolve the forward target from the Model's status (D25)"
```

---

### Task 7: rename the deployments

**Files:**
- Modify: `deploy/helm/squall/templates/proxy-deployment.yaml` →
  `deploy/helm/squall/templates/squall-proxy-deployment.yaml`;
  `controller-deployment.yaml` → `squall-operator-deployment.yaml`; the matching Service and RBAC
  templates; `hack/cluster.sh` (`E2E_DEPLOYMENTS`, `E2E_IMAGES`); `test/e2e/cluster/helm-values.yaml`;
  `docs/references/*` where the old names appear.

**Why now:** at 0.1.0 these names become public API. `proxy` is also dangerously generic for a
name in a shared namespace.

- [ ] **Step 1: Rename**

`squall-controller-manager` → **`squall-operator`**, `proxy` → **`squall-proxy`**. The Service in
front of the proxy becomes `squall-proxy` too; `SQUALL_PROXY_SERVICE_NAME` in the controller's env
must follow, or the controller enumerates the endpoints of a Service that no longer exists.

```bash
grep -rn "squall-controller-manager\|name: proxy\|proxy-deployment\|\"proxy\"" \
  deploy/helm hack test/e2e config docs/references | grep -v '\.git'
```

Work through every hit. `internal/` should need no change — the names reach it through env vars.

- [ ] **Step 2: Verify nothing still points at the old names**

```bash
grep -rn "squall-controller-manager" . --exclude-dir=.git --exclude-dir=.worktrees \
  --exclude-dir=docs/plans --exclude-dir=.gocache || echo "clean"
```

Hits inside `docs/references/deviations-and-findings.md` are HISTORY and must stay — the ledger
records what happened, not what is current.

- [ ] **Step 3: Reinstall from scratch and prove it**

```bash
export KUBECONFIG=$(pwd)/.gocache/kube/config
./scripts/dev.sh hack/cluster.sh down
./scripts/dev.sh hack/cluster.sh up
kubectl get deploy -A | grep squall
```

Expected: `squall-operator` and `squall-proxy`, and no `proxy` or `squall-controller-manager`.
A full recreate is deliberate: an in-place upgrade would leave the old Deployments running and
hide exactly the kind of dangling reference this task is about.

- [ ] **Step 4: Commit**

```bash
git add deploy/helm hack/cluster.sh test/e2e docs/references
git commit -m "refactor: rename deployments to squall-operator and squall-proxy"
```

---

### Task 8: release mechanics

**Files:**
- Create: `CHANGELOG.md`
- Modify: `Makefile` (VERSION), `deploy/helm/squall/Chart.yaml`, `README.md`, `.gitignore`
- Delete: `cover-unit.out`, `cover-envtest.out` (committed by accident)

- [ ] **Step 1: Stop committing coverage output**

```bash
printf '\n# Coverage output, produced by `make test-unit` / `make test-envtest`.\ncover-*.out\n' >> .gitignore
git rm --cached cover-unit.out cover-envtest.out
```

- [ ] **Step 2: Stamp a real version**

`Makefile` line 5:

```make
# Overridden by CI from the git tag. `dev` is what a working tree is, and it
# is what the binaries reported before 0.1.0 — including on a real GPU, where
# `version=dev` in the log gave no way to tell which build was running.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
```

Verify it reaches the binary:

```bash
./scripts/dev.sh make build-image-controller
docker run --rm --entrypoint /app squall-controller:e2e --version 2>&1 | head -2
```

- [ ] **Step 3: Align the chart**

`deploy/helm/squall/Chart.yaml`:

```yaml
version: 0.1.0
appVersion: "0.1.0"
```

The chart was at `0.2.0` while the app was `0.1.0`, which reads as the chart being ahead of a
release that never happened. For the first tagged release they are the same number.

- [ ] **Step 4: Write `CHANGELOG.md`**

Keep-a-Changelog format. `0.1.0` must state the scope honestly, including what does NOT work:

```markdown
# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-08-27  <!-- set to the tag date -->

First release. Verified end to end on real hardware: a `Model` CR provisioned
an RTX PRO 6000 (96GB) on Vast.ai, served chat, streaming and vision through
`squall-proxy`, then slept on idle and released the instance unattended.

### Added
- `Model` CRD with 0↔1 replica scaling driven by real request demand.
- `squall-operator`: reconciliation, drain-first teardown via finalizer,
  orphan reaping every minute.
- `squall-proxy`: request-holding wake path with a truthful wait contract.
- Engines: vLLM, Ollama, llama.cpp — `spec.model` names the weights in each
  engine's own dialect and the replica is made to answer to the CR's name.
- Backends: whatever dstack 0.21.2 supports; exercised on Vast.ai and
  in-cluster Kubernetes.
- The replica's served model is verified against the CR and published as
  `status.servedModel`.
- `Schedulable` condition distinguishing an unconfigured backend, a missing
  fleet and an empty market — all three of which dstack reports identically.

### Known limitations
- Squall never sends `force` to dstack, by construction.
- A marketplace host is not a data processor: §12.3 restricts these workloads
  to internal, non-regulated use.
- dstack writes the replica's environment, including resolved secrets, to its
  own diagnostic logs in cleartext (ledger D69). Scope and rotate tokens.
- Region strings are gpuhunt's normalised names, matched exactly; there is no
  `europe` grouping (D59).
- dstack applies a hardcoded offer filter to Vast.ai that squall cannot reach
  (D60).
```

- [ ] **Step 5: README**

Replace the quickstart with the verified runbook from
`config/samples/squall_v1alpha1_model.yaml`'s header: install the chart, configure a backend and
a fleet, apply a Model, send one request. State the two invariants and link
`docs/references/`. Say plainly that the first wake takes 10–15 minutes for a 27B model.

- [ ] **Step 6: Close the ledger entries**

In `docs/references/deviations-and-findings.md`, move D25, D31, D58, D62, D65, D67 to CLOSED with
a one-line note naming the task that closed each. Do not renumber anything.

- [ ] **Step 7: Full gate, then tag**

```bash
./scripts/dev.sh make test-unit
./scripts/dev.sh make test-envtest
./scripts/dev.sh make qa-lint
export KUBECONFIG=$(pwd)/.gocache/kube/config
./scripts/dev.sh hack/cluster.sh down && ./scripts/dev.sh hack/cluster.sh up
```

- [ ] **Step 8: Commit**

```bash
git add CHANGELOG.md README.md Makefile deploy/helm/squall/Chart.yaml .gitignore \
        docs/references/deviations-and-findings.md
git commit -m "chore(release): prepare 0.1.0"
```

**BLOCK 3 GATE + FINAL VERIFICATION**

The only acceptance test that counts is the one that already found five bugs: run the real thing.

- [ ] Configure a real backend, apply `config/samples/squall_v1alpha1_model.yaml` **unmodified**,
      send one request, get a completion.
- [ ] `kubectl get model -n squall qwen3-8-27b` shows `Schedulable=True` and a `Served` column
      matching the CR name.
- [ ] Delete the Model; confirm zero active instances at the provider.
- [ ] Apply a Model naming a backend that is NOT configured; confirm
      `Schedulable=False`, `Reason=BackendUnavailable`, a matching operator log line, and that a
      request is answered promptly rather than held.
- [ ] Apply an **Ollama** Model with `spec.model: qwen3:0.6b`; confirm it serves and that
      `status.servedModel` is the CR's name.

---

## Human-only, cannot be done by an agent

- **Create the GitHub repository and push.** No git remote exists; nothing has ever been pushed.
  Outward-facing and irreversible — confirm the name (`ackstorm/squall`?) first.
- **Tag `v0.1.0`** once the final verification above is green.
- **File the dstack upstream issues**: host-key verification, admin token on stdout, and now D69
  (secrets in diagnostic logs).
- **Rotate the Vast.ai API key and the HuggingFace token** used during development — both appear
  in session transcripts.

## Explicitly out of scope for 0.1.0

Named so nobody adds them mid-flight:

- MkDocs site (follow-up 3). The README carries 0.1.0.
- D26 `squall_model_price_per_hour` has no production data source.
- D37 run identity is `model.Name` alone, not uid-based.
- D10 CAS livelock, D11 sleep-decision TOCTOU, D42 unbounded body read in `modelFromRequest`.
- D39 the cross-field validation layer is dead code with zero callers.
- Multi-replica serving. `minReplicas`/`maxReplicas` beyond 0↔1 is not this release.
