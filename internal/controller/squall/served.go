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
//
// An id equal to want, or to want+":latest", is a match. The ":latest" case
// is Ollama-specific, not a fuzzy rule: engineCommands' Ollama branch runs
// `ollama cp <weights> <name>` to alias the pulled weights to the Model's
// name (D62), and Ollama's own /v1/models then reports that alias as
// "<name>:latest" — the implicit tag Ollama attaches to any name pushed
// through `ollama cp`/`ollama pull` without one of its own. Left unhandled
// (I3, block 2 review) this made servedModelMatches always fail for every
// Ollama Model, reporting a permanent ServedModelMismatch on a replica that
// is in fact serving exactly what was asked. vLLM and llama.cpp never add a
// tag, so this extra check never masks a real mismatch there.
func servedModelMatches(served []string, want string) bool {
	_, ok := servedModelMatch(served, want)
	return ok
}

// servedModelMatch is servedModelMatches plus the id that actually matched.
// The id is what the proxy must rewrite a request's "model" field to, so it
// has to be ONE name — see servedModelToForward.
func servedModelMatch(served []string, want string) (string, bool) {
	for _, s := range served {
		if s == want || s == want+":latest" {
			return s, true
		}
	}
	return "", false
}

// servedModelToForward picks the single served id that squall-proxy may
// rewrite an outbound request's "model" field to, or "" when there is no
// safe single answer.
//
// D100, measured live 2026-08-31: status.servedModel used to be
// strings.Join(served, ","), and attemptForward rewrites the request's model
// field to whatever this field holds. With Ollama — which always reports at
// least the `ollama cp` alias AND the source weights (D62) — every request
// after verification succeeded was rewritten to
// "ollama-tiny:latest,qwen2.5:0.5b" and answered 400. The check passing is
// what broke serving, which is the worst possible failure direction. vLLM
// hid it: run with --served-model-name it reports exactly one id, so the
// join was a no-op.
//
// The field now means ONE thing: the name to forward under. The full list
// stays diagnostic, in the ServedModelVerified condition's message.
func servedModelToForward(served []string, want string) string {
	if want != "" {
		// An expectation exists: forward under the id that satisfied it, and
		// under nothing at all if none did. Publishing anything on a mismatch
		// would have the proxy rewrite requests to a name the operator never
		// asked for.
		matched, _ := servedModelMatch(served, want)
		return matched
	}
	// No expectation (baked-in weights, engineServedName returns ""). One
	// served id is unambiguous, so the proxy can still bridge a caller's
	// Model name to the engine's own name. Several is a guess, and guessing
	// here would route a payload at a model nobody chose.
	if len(served) == 1 {
		return served[0]
	}
	return ""
}
