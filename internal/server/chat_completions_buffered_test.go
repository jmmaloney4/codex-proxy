package server

import (
	"strings"
	"testing"
)

// TestBufferChatCompletionFromSSE_CapturesUsage verifies that the buffered,
// non-streaming response carries the token usage reported by the upstream Codex
// /responses stream's final event. Regression test for usage being dropped
// (reported as absent / zero) on the /v1/chat/completions buffered path.
func TestBufferChatCompletionFromSSE_CapturesUsage(t *testing.T) {
	src := strings.Join([]string{
		`data: {"type":"response.output_text.delta","sequence_number":1,"delta":"pong"}`,
		"",
		`data: {"type":"response.completed","sequence_number":2,"response":{"usage":{"input_tokens":5307,"output_tokens":17,"total_tokens":5324}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	resp, err := bufferChatCompletionFromSSE(strings.NewReader(src), "gpt-5.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "pong" {
		t.Fatalf("expected aggregated content %q, got %+v", "pong", resp.Choices)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("expected finish_reason stop, got %q", resp.Choices[0].FinishReason)
	}

	if resp.Usage.PromptTokens != 5307 {
		t.Errorf("prompt_tokens = %d, want 5307", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 17 {
		t.Errorf("completion_tokens = %d, want 17", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != 5324 {
		t.Errorf("total_tokens = %d, want 5324", resp.Usage.TotalTokens)
	}
}

// TestBufferChatCompletionFromSSE_NoUsageZeroFallback verifies that when the
// upstream stream never reports usage, the response still serializes a
// well-formed zero-valued usage object rather than failing.
func TestBufferChatCompletionFromSSE_NoUsageZeroFallback(t *testing.T) {
	src := strings.Join([]string{
		`data: {"type":"response.output_text.delta","sequence_number":1,"delta":"hi"}`,
		"",
		`data: {"type":"response.completed","sequence_number":2,"response":{}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	resp, err := bufferChatCompletionFromSSE(strings.NewReader(src), "gpt-5.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Usage.PromptTokens != 0 || resp.Usage.CompletionTokens != 0 || resp.Usage.TotalTokens != 0 {
		t.Errorf("expected zero usage fallback, got %+v", resp.Usage)
	}
}
