// SPDX-License-Identifier: MIT

// Command model-mock is a minimal OpenAI-compatible chat-completions
// engine, standing in for vLLM/Ollama behind a Ready Model. It exists so
// Phase 11's kind e2e cluster can prove squall-proxy's forwarding path
// (internal/proxy/backend.go's TemplateBackend, internal/proxy/handler.go's
// forward) against a real HTTP hop, not just a unit test.
//
// This is test infrastructure, not a production binary: it is built and
// kind-loaded only by test/e2e/cluster (see hack/cluster.sh), never shipped
// in a release image. Deliberately not realistic: no streaming, no
// tokenizer, no config surface beyond the listen address and an optional
// artificial delay. Every response is deterministic so assertions are
// stable.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

// chatRequest is the subset of an OpenAI chat-completion request this mock
// needs: which model to echo back (squall-proxy's handler already peeked
// this same field to route the request here).
type chatRequest struct {
	Model string `json:"model"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   chatUsage    `json:"usage"`
}

// chatCompletions answers POST /v1/chat/completions with a deterministic,
// well-formed OpenAI chat-completion body, echoing back whatever model name
// the request asked for. A malformed/empty body still gets a response (with
// an empty model field) rather than an error — this mock stands in for an
// engine's happy path only, not its input validation.
func chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	if delay := os.Getenv("MODEL_MOCK_DELAY"); delay != "" {
		if d, err := time.ParseDuration(delay); err == nil {
			time.Sleep(d)
		}
	}

	resp := chatResponse{
		ID:      "chatcmpl-mock",
		Object:  "chat.completion",
		Created: 1700000000,
		Model:   req.Model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      chatMessage{Role: "assistant", Content: "mock response from model-mock"},
			FinishReason: "stop",
		}},
		Usage: chatUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func main() {
	addr := os.Getenv("MODEL_MOCK_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/chat/completions", chatCompletions)

	log.Printf("model-mock listening on %s", addr)
	//nolint:gosec // e2e test double, not internet-facing.
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("model-mock: listen failed: %v", err)
	}
}
