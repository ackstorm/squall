// SPDX-License-Identifier: Apache-2.0

package squall

import (
	"fmt"
	"strings"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

// enginePort is where each engine template listens. These are the engines'
// own defaults, not a squall choice, and they live next to the Apply call
// site (model_controller.go) rather than in internal/dstack — the client
// should not know what vLLM is. ModelSpec carries no port field (verified
// against api/squall/v1alpha1/model_types.go); this derivation is why none
// is needed.
func enginePort(e squallv1alpha1.ModelEngine) int {
	switch e {
	case squallv1alpha1.ModelEngineVLLM:
		return 8000
	case squallv1alpha1.ModelEngineLlamaCpp:
		return 8080
	case squallv1alpha1.ModelEngineOllama:
		return 11434
	default:
		return 8000
	}
}

// engineResources maps spec.resources onto the client's passthrough type
// (F33: no translation layer — every range stays the opaque string the CR
// carried).
//
// It returns a NON-nil Resources whenever the CR has a resources block,
// even a partially-filled one, because a nil hands the entire block to
// dstack's defaults: 2 cores, 8GB RAM, 100GB disk and a GPU count minimum
// of ZERO. Dropping this mapping is what D55 was.
func engineResources(r squallv1alpha1.ModelResources) *dstack.Resources {
	out := &dstack.Resources{
		Memory:  r.Memory,
		ShmSize: r.ShmSize,
		Disk:    r.Disk,
	}
	if r.CPU != nil {
		out.CPUArch = r.CPU.Arch
		out.CPUCount = r.CPU.Count
	}
	if r.GPU != nil {
		out.GPU = &dstack.GPU{
			Vendor:            r.GPU.Vendor,
			Name:              r.GPU.Name,
			Count:             r.GPU.Count,
			Memory:            r.GPU.Memory,
			TotalMemory:       r.GPU.TotalMemory,
			ComputeCapability: r.GPU.ComputeCapability,
		}
	}
	return out
}

// enginePlacement maps spec.placement onto the client's passthrough type.
// Backends is §12.3's compliance allowlist: the CRD enforces MinItems=1, so
// the only way this arrives empty is a CR that predates that validation.
// Passing it through is what makes the eligibility table an actual control
// rather than documentation.
func enginePlacement(p squallv1alpha1.ModelPlacement) dstack.Placement {
	out := dstack.Placement{Backends: p.Backends, Regions: p.Regions}
	if p.MaxPricePerHour != nil {
		out.MaxPrice = p.MaxPricePerHour.String()
	}
	return out
}

// engineProbePath is the readiness path a STOCK image for each engine
// serves. Same rule and same reason as enginePort: the engines' own
// defaults, kept next to the Apply call site rather than in
// internal/dstack, which should not know what vLLM is.
//
// It is only a DEFAULT. spec.probe.path overrides it, because a customised
// image, a sidecar, or an auth-fronted engine can serve none of these.
func engineProbePath(e squallv1alpha1.ModelEngine) string {
	switch e {
	case squallv1alpha1.ModelEngineOllama:
		// Ollama has no /health; its root answers "Ollama is running".
		return "/"
	case squallv1alpha1.ModelEngineVLLM, squallv1alpha1.ModelEngineLlamaCpp:
		return "/health"
	default:
		return "/health"
	}
}

// engineProbe builds the probe to submit: the CR's values where set, the
// engine-derived path otherwise. Returns a non-nil Probe always — §6
// requires probe evidence to exist, and dstack's own default is NO probe,
// so there is no "let dstack decide" option here (unlike resources).
func engineProbe(spec squallv1alpha1.ModelSpec) *dstack.Probe {
	out := &dstack.Probe{Path: engineProbePath(spec.Engine)}
	p := spec.Probe
	if p == nil {
		return out
	}
	if p.Path != "" {
		out.Path = p.Path
	}
	out.Method = p.Method
	if p.Timeout != nil {
		out.TimeoutSeconds = int(p.Timeout.Duration.Seconds())
	}
	if p.Interval != nil {
		out.IntervalSeconds = int(p.Interval.Duration.Seconds())
	}
	if p.ReadyAfter != nil {
		out.ReadyAfter = int(*p.ReadyAfter)
	}
	return out
}

// engineCommands builds the replica's start command.
//
// dstack's `commands` is not argv: the runner joins the list with ` && ` and
// hands it to `/bin/sh -i -c`, and a non-empty list REPLACES the image CMD
// (ledger D64/D65 — both measured, both after a GPU had been billed). So
// this returns exactly ONE element, and it restates the engine's entrypoint.
//
// MEASURED 2026-08-27 on dstack 0.21.2. Sending spec.args straight through
// produced this job_spec, which is not a mistake anyone catches by reading:
//
//	["/bin/sh","-i","-c","--model && Qwen/Qwen3.8-27B-FP8 && --max-model-len && 131072 && ..."]
//
// It provisions a GPU, bills for it, and then fails — the flags are being
// run as programs.
//
// spec.model (D62) is rendered in each engine's own dialect, and in every
// case the replica is ALSO made to answer to the Model's name so callers do
// not need to know which dialect was used.
//
// Returns nil when there is nothing to say (no spec.model, no spec.args),
// which leaves the image's own CMD in place — the right behaviour for an
// image with baked-in weights and a correct entrypoint.
func engineCommands(spec squallv1alpha1.ModelSpec, name string) []string {
	if spec.Model == "" && len(spec.Args) == 0 {
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
		// `ollama serve` starts a server with NO model loaded. The weights
		// arrive via `ollama pull`, which needs the server already up, and
		// `ollama cp` then aliases them to the CR's name — Ollama's
		// equivalent of vLLM's --served-model-name.
		//
		// spec.Args is appended to `ollama serve`'s own argv, exactly like
		// the spec.Model == "" branch above and like vLLM/llama.cpp — NOT
		// spliced into the `&&` chain as a separate command. It used to be
		// (I1, block 2 review): a quoted `VAR=value` word is a COMMAND NAME
		// to the shell, not an assignment, so the block's own example
		// (OLLAMA_KEEP_ALIVE=1h — an environment variable, which belongs in
		// spec.env, which this CRD already has) made the run `sh: not
		// found` -> exit 1, killing the replica after the GPU was paid for
		// and the weights pulled — the single most expensive place to fail.
		// (Checked against ollama/ollama's own cmd/cmd.go, 2026-08-28:
		// `ollama serve` registers no subcommand flags of its own today, so
		// this is a passthrough for whatever it gains next, not something
		// exercised in production.)
		//
		// The readiness wait is BOUNDED with an explicit failure path. An
		// `until ollama list; do sleep; done` would spin forever if the
		// server died at startup, leaving a rented GPU running a loop.
		//
		// The pull/cp chain is gated with `|| { ...; exit 1; }` ahead of
		// `wait`. Without that gate, a failed pull short-circuits past
		// `ollama cp` — no alias is ever created — and the script falls
		// straight through to `wait` anyway: the backgrounded `ollama
		// serve` is healthy, `/` answers 200 with no model loaded, and
		// squall marks the replica Ready over a GPU billing for nothing.
		// That is D65's exact failure shape, so any failure in this chain
		// must exit the whole script before `wait`.
		return []string{fmt.Sprintf(
			`%s & `+
				`for i in $(seq 1 60); do ollama list >/dev/null 2>&1 && break; sleep 2; done; `+
				`ollama list >/dev/null 2>&1 || { echo "ollama serve never came up" >&2; exit 1; }; `+
				`ollama pull %s && ollama cp %s %s || { echo "ollama pull or cp failed; refusing to serve an unaliased model" >&2; exit 1; }; wait`,
			shellJoin(append([]string{"ollama", "serve"}, spec.Args...)),
			shellJoin([]string{spec.Model}),
			shellJoin([]string{spec.Model}),
			shellJoin([]string{name}),
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
			// Refusing means FAILING the run, not returning nil: nil (per
			// this function's own doc comment) leaves the image's own CMD
			// in place, which is guessing — the exact D65 shape this guard
			// exists to prevent. There is no known dialect for an engine
			// that reaches this branch (spec.Engine is CRD-enum-validated,
			// so today this is unreachable), so the only safe move is to
			// fail the run outright rather than let an unknown entrypoint
			// silently serve whatever the image defaults to.
			return []string{`echo "squall: unknown engine, cannot honour spec.model" >&2; exit 1`}
		}
		return []string{shellJoin(spec.Args)}
	}
}

// engineServedName is the name the replica will answer to once
// engineCommands has done its work: always the Model's own name, for every
// engine. That uniformity is what D65's check compares against and what
// lets squall-proxy rewrite a single field instead of learning three
// dialects.
func engineServedName(spec squallv1alpha1.ModelSpec, name string) string {
	if spec.Model == "" {
		return ""
	}
	return name
}

// shellJoin renders argv as a single POSIX shell word list. Single-quoting
// everything is deliberate: an engine flag is data, and a value like
// --chat-template '{{ ... }}' must not be reinterpreted by the shell dstack
// wraps it in.
func shellJoin(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(a, "'", `'\''`))
		b.WriteByte('\'')
	}
	return b.String()
}
