// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatCompletions(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantModel string
	}{
		{name: "echoes requested model", body: `{"model":"e2e-fixture-model"}`, wantModel: "e2e-fixture-model"},
		{name: "empty body still responds", body: ``, wantModel: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()

			chatCompletions(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}

			var resp chatResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Object != "chat.completion" {
				t.Errorf("object = %q, want %q", resp.Object, "chat.completion")
			}
			if resp.Model != tt.wantModel {
				t.Errorf("model = %q, want %q", resp.Model, tt.wantModel)
			}
			if len(resp.Choices) != 1 {
				t.Fatalf("choices = %+v, want exactly one", resp.Choices)
			}
			if resp.Choices[0].Message.Role != "assistant" {
				t.Errorf("message role = %q, want assistant", resp.Choices[0].Message.Role)
			}
			if resp.Choices[0].Message.Content == "" {
				t.Errorf("message content is empty, want deterministic content")
			}
			if resp.Usage.TotalTokens != resp.Usage.PromptTokens+resp.Usage.CompletionTokens {
				t.Errorf("usage totals inconsistent: %+v", resp.Usage)
			}
		})
	}
}

// /healthz is a one-liner (w.WriteHeader(http.StatusOK)) inlined in main() —
// no test for it: a test that doesn't call production code would be vacuous
// (see docs/references/testing-discipline.md).
