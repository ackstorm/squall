// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// TestEngineCommands_IsOneShellWordNotOneCommandPerArg is the whole point.
// dstack joins `commands` with ` && ` and runs the result under /bin/sh, so
// N elements means N programs. spec.args is argv, so it must arrive as ONE
// element or the flags get executed as commands — which is what billed a
// real GPU on 2026-08-27.
func TestEngineCommands_IsOneShellWordNotOneCommandPerArg(t *testing.T) {
	got := engineCommands(squallv1alpha1.ModelSpec{
		Engine: squallv1alpha1.ModelEngineVLLM,
		Args:   []string{"--model", "Qwen/Qwen3.8-27B-FP8", "--max-model-len", "131072"},
	}, "test-model")

	if len(got) != 1 {
		t.Fatalf("engineCommands returned %d elements %q, want exactly 1:\n"+
			"dstack runs `sh -c` over these joined by ' && ', so more than one "+
			"element executes the flags as programs", len(got), got)
	}
	if strings.Contains(got[0], "&&") {
		t.Fatalf("engineCommands = %q, must not contain '&&'", got[0])
	}
}

// TestEngineCommands_RestatesTheEngineEntrypoint: a non-empty `commands`
// REPLACES the image CMD. Omitting the entrypoint is not a cosmetic slip —
// it is what made an args-less vLLM run serve the image's default model
// (Qwen/Qwen3-0.6B) while every probe reported healthy.
func TestEngineCommands_RestatesTheEngineEntrypoint(t *testing.T) {
	for _, tc := range []struct {
		engine squallv1alpha1.ModelEngine
		prefix string
	}{
		{squallv1alpha1.ModelEngineVLLM, "'vllm' 'serve'"},
		{squallv1alpha1.ModelEngineLlamaCpp, "'llama-server'"},
		{squallv1alpha1.ModelEngineOllama, "'ollama' 'serve'"},
	} {
		got := engineCommands(squallv1alpha1.ModelSpec{Engine: tc.engine, Args: []string{"--flag"}}, "test-model")
		if len(got) != 1 || !strings.HasPrefix(got[0], tc.prefix) {
			t.Fatalf("engine %s: engineCommands = %q, want it to start with %q",
				tc.engine, got, tc.prefix)
		}
	}
}

// TestEngineCommands_NoArgsLeavesTheImageCMDAlone: returning an empty
// non-nil slice would replace the CMD with nothing.
func TestEngineCommands_NoArgsLeavesTheImageCMDAlone(t *testing.T) {
	if got := engineCommands(squallv1alpha1.ModelSpec{Engine: squallv1alpha1.ModelEngineVLLM}, "test-model"); got != nil {
		t.Fatalf("engineCommands(no args) = %q, want nil so the image CMD survives", got)
	}
}

// TestShellJoin_QuotesValuesTheShellWouldOtherwiseEat: engine flags carry
// braces, spaces and quotes (--chat-template being the obvious one), and
// dstack hands the string to a shell.
func TestShellJoin_QuotesValuesTheShellWouldOtherwiseEat(t *testing.T) {
	tests := []struct {
		argv []string
		want string
	}{
		{[]string{"vllm", "serve"}, `'vllm' 'serve'`},
		{[]string{"--chat-template", "{{ bos }}"}, `'--chat-template' '{{ bos }}'`},
		{[]string{"--x", "a'b"}, `'--x' 'a'\''b'`},
		{[]string{"--x", "$(rm -rf /)"}, `'--x' '$(rm -rf /)'`},
	}
	for _, tc := range tests {
		if got := shellJoin(tc.argv); got != tc.want {
			t.Fatalf("shellJoin(%q) = %s, want %s", tc.argv, got, tc.want)
		}
	}
}

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

// TestEngineCommands_OllamaModelAndArgsCoexist: spec.args must survive
// alongside spec.model for Ollama too, and it must land as an argument to
// `ollama serve` itself — matching the spec.Model == "" branch and
// vLLM/llama.cpp's own treatment of spec.args (I1, block 2 review). The
// previous version of this test used `OLLAMA_KEEP_ALIVE=1h` as its example,
// which is an ENVIRONMENT VARIABLE (spec.env already exists for that), and
// spliced it into the shell as a separate command — a quoted `VAR=value`
// word is a command name, not an assignment, so that "example" made the
// generated script fatal (`sh: not found` -> exit 1) on a GPU that had
// already been provisioned. Uses a flag-shaped value here instead; do not
// reintroduce an env-var-shaped example.
func TestEngineCommands_OllamaModelAndArgsCoexist(t *testing.T) {
	got := engineCommands(squallv1alpha1.ModelSpec{
		Engine: squallv1alpha1.ModelEngineOllama,
		Model:  "qwen3:8b",
		Args:   []string{"--fake-flag"},
	}, "my-model")
	cmd := got[0]
	if !strings.Contains(cmd, "'ollama' 'serve' '--fake-flag'") {
		t.Fatalf("spec.args must be arguments to `ollama serve`, not a separate shell command:\n%s", cmd)
	}
}

// TestEngineCommands_OllamaPullFailureIsFatal is fix round 1, Finding 1: a
// failed `ollama pull` must not let the script reach `wait` — the
// backgrounded `ollama serve` is healthy with no model loaded, `/` answers
// 200 either way, and squall would mark the replica Ready over a GPU
// billing to serve nothing (D65, reintroduced once already in this
// function).
//
// A string search for "exit 1" would not discriminate this from the
// readiness-timeout's own "exit 1", so this test actually RUNS the
// generated script under a stub `ollama` whose `pull` fails, and checks the
// two things that matter: the script exits non-zero, and `ollama cp` never
// ran.
func TestEngineCommands_OllamaPullFailureIsFatal(t *testing.T) {
	got := engineCommands(squallv1alpha1.ModelSpec{
		Engine: squallv1alpha1.ModelEngineOllama,
		Model:  "qwen3:8b",
	}, "my-model")
	cmd := got[0]

	dir := t.TempDir()
	cpMarker := filepath.Join(dir, "cp-ran")
	// Stub `ollama`: `list` succeeds immediately (so the readiness loop
	// exits on its first iteration instead of sleeping), `pull` fails, and
	// `cp` — if it ever runs — leaves a marker the test can check for.
	stub := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  list) exit 0 ;;\n" +
		"  pull) exit 1 ;;\n" +
		"  cp) touch '" + cpMarker + "'; exit 0 ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "ollama"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write ollama stub: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sh := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	// Put the stub ahead of the real PATH rather than replacing it, so
	// `ollama` resolves to the stub while `seq`, `sh` and friends still
	// resolve normally. The LAST "PATH=" entry in Env wins, so this must be
	// appended after os.Environ(), not prepended before it.
	sh.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := sh.CombinedOutput()

	if err == nil {
		t.Fatalf("script exited 0 after a failed `ollama pull`; want non-zero so squall's "+
			"reconciler sees the run fail instead of a false-healthy replica:\n%s", out)
	}
	if _, statErr := os.Stat(cpMarker); statErr == nil {
		t.Fatalf("`ollama cp` ran despite the preceding `ollama pull` having failed:\n%s", out)
	}
}
