package service

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

func newRelayInfo() *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
}

func TestClaudeToOpenAIRequest_PreservesThinkingOnAssistantMessages(t *testing.T) {
	thinkingText := "Let me think about this step by step."
	claudeReq := dto.ClaudeRequest{
		Model: "claude-sonnet-4-5-20250929",
		Messages: []dto.ClaudeMessage{
			{Role: "user", Content: "Hello"},
			{
				Role: "assistant",
				Content: []dto.ClaudeMediaMessage{
					{Type: "thinking", Thinking: &thinkingText},
					{Type: "text", Text: strPtr("The answer is 42.")},
				},
			},
		},
	}

	info := newRelayInfo()
	result, err := ClaudeToOpenAIRequest(claudeReq, info)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result.Messages))
	}

	assistantMsg := result.Messages[1]
	if assistantMsg.Role != "assistant" {
		t.Fatalf("expected assistant role, got %s", assistantMsg.Role)
	}

	if assistantMsg.ReasoningContent == nil {
		t.Fatal("expected reasoning_content to be set from thinking block, got nil")
	}

	if *assistantMsg.ReasoningContent != thinkingText {
		t.Fatalf("reasoning_content mismatch: expected %q, got %q", thinkingText, *assistantMsg.ReasoningContent)
	}

	contents := assistantMsg.ParseContent()
	if len(contents) == 0 {
		t.Fatal("expected content from text block")
	}
	if contents[0].Text != "The answer is 42." {
		t.Fatalf("text content mismatch: %q", contents[0].Text)
	}
}

func TestClaudeToOpenAIRequest_PreservesMultipleThinkingBlocks(t *testing.T) {
	t1 := "First reasoning step."
	t2 := "Second reasoning step."
	claudeReq := dto.ClaudeRequest{
		Model: "claude-sonnet-4-5-20250929",
		Messages: []dto.ClaudeMessage{
			{Role: "user", Content: "Hello"},
			{
				Role: "assistant",
				Content: []dto.ClaudeMediaMessage{
					{Type: "thinking", Thinking: &t1},
					{Type: "text", Text: strPtr("Part 1.")},
					{Type: "thinking", Thinking: &t2},
					{Type: "text", Text: strPtr("Part 2.")},
				},
			},
		},
	}

	info := newRelayInfo()
	result, err := ClaudeToOpenAIRequest(claudeReq, info)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assistantMsg := result.Messages[1]
	if assistantMsg.ReasoningContent == nil {
		t.Fatal("expected reasoning_content to be set")
	}

	expected := t1 + "\n" + t2
	if *assistantMsg.ReasoningContent != expected {
		t.Fatalf("reasoning_content mismatch: expected %q, got %q", expected, *assistantMsg.ReasoningContent)
	}
}

func TestClaudeToOpenAIRequest_MessageWithOnlyThinking(t *testing.T) {
	thinkingText := "I'm just thinking, no text output."
	claudeReq := dto.ClaudeRequest{
		Model: "claude-sonnet-4-5-20250929",
		Messages: []dto.ClaudeMessage{
			{Role: "user", Content: "Hello"},
			{
				Role: "assistant",
				Content: []dto.ClaudeMediaMessage{
					{Type: "thinking", Thinking: &thinkingText},
				},
			},
		},
	}

	info := newRelayInfo()
	result, err := ClaudeToOpenAIRequest(claudeReq, info)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assistant message with only thinking should still be included
	assistantMsg := result.Messages[1]
	if assistantMsg.ReasoningContent == nil {
		t.Fatal("expected reasoning_content to be set")
	}
	if *assistantMsg.ReasoningContent != thinkingText {
		t.Fatalf("reasoning_content mismatch: expected %q, got %q", thinkingText, *assistantMsg.ReasoningContent)
	}
}

func strPtr(s string) *string {
	return &s
}
