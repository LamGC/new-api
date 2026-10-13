package oaichat

import (
	"context"
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAIChatRequestToClaudeMessagesNormalizesToolInputSchema(t *testing.T) {
	tests := []struct {
		name       string
		parameters any
		wantSchema map[string]any
	}{
		{
			name:       "omitted parameters",
			parameters: nil,
			wantSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			name: "missing type and properties",
			parameters: map[string]any{
				"additionalProperties": false,
			},
			wantSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
		{
			name: "non-string type",
			parameters: map[string]any{
				"type":       123,
				"properties": map[string]any{},
			},
			wantSchema: map[string]any{
				"type":       123,
				"properties": map[string]any{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maxTokens := uint(1024)
			got, err := OpenAIChatRequestToClaudeMessages(context.Background(), nil, dto.GeneralOpenAIRequest{
				Model:     "claude-test",
				MaxTokens: &maxTokens,
				Messages: []dto.Message{
					{Role: "user", Content: "Call the tool."},
				},
				Tools: []dto.ToolCallRequest{
					{
						Type: "function",
						Function: dto.FunctionRequest{
							Name:        "get_current_time",
							Description: "Get the current time",
							Parameters:  tt.parameters,
						},
					},
				},
			})

			require.NoError(t, err)
			tools, ok := got.Tools.([]any)
			require.True(t, ok)
			require.Len(t, tools, 1)
			tool, ok := tools[0].(*dto.Tool)
			require.True(t, ok)
			assert.Equal(t, "get_current_time", tool.Name)
			assert.Equal(t, tt.wantSchema, tool.InputSchema)
		})
	}
}

func TestChatHistoryThinkingOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			maxTokens := uint(1024)
			first, second := "first", "second"
			req := dto.GeneralOpenAIRequest{Model: "claude-test", MaxTokens: &maxTokens, Messages: []dto.Message{
				{Role: "user", Content: "question"},
				{Role: "assistant", Content: "answer one", ReasoningContent: &first},
				{Role: "assistant", Content: "answer two", Reasoning: &second},
				{Role: "user", Content: "continue"},
				{Role: "assistant", Content: "", ReasoningContent: &first},
			}}
			req.Messages[4].SetToolCalls([]dto.ToolCallRequest{{ID: "call_1", Type: "function", Function: dto.FunctionRequest{Name: "search", Arguments: `{}`}}})
			meta := &convmeta.Values{Options: &convmeta.Options{PreserveHistoryThinking: enabled}}
			got, err := OpenAIChatRequestToClaudeMessages(context.Background(), meta, req)
			require.NoError(t, err)
			require.Len(t, got.Messages, 4)
			if enabled {
				blocks, err := got.Messages[1].ParseContent()
				require.NoError(t, err)
				require.GreaterOrEqual(t, len(blocks), 2)
				assert.Equal(t, "thinking", blocks[0].Type)
				require.NotNil(t, blocks[0].Thinking)
				assert.Equal(t, "first\nsecond", *blocks[0].Thinking)
				assert.Empty(t, blocks[0].Signature)
				toolBlocks, err := got.Messages[3].ParseContent()
				require.NoError(t, err)
				assert.Equal(t, "thinking", toolBlocks[0].Type)
				assert.Equal(t, "tool_use", toolBlocks[len(toolBlocks)-1].Type)
				assert.Equal(t, "call_1", toolBlocks[len(toolBlocks)-1].Id)
			} else {
				assert.Equal(t, "answer one answer two", got.Messages[1].GetStringContent())
				blocks, err := got.Messages[3].ParseContent()
				require.NoError(t, err)
				for _, block := range blocks {
					assert.NotEqual(t, "thinking", block.Type)
				}
			}
		})
	}
}
