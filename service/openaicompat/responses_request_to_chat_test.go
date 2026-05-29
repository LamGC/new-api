package openaicompat

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/samber/lo"
)

func mustJSON(v any) json.RawMessage {
	b, _ := common.Marshal(v)
	return b
}

func TestResponsesRequestToChatRequest_SimpleText(t *testing.T) {
	req := &dto.OpenAIResponsesRequest{
		Model:    "deepseek-chat",
		Input:    mustJSON([]map[string]any{
			{"role": "user", "content": "Hello"},
		}),
	}

	result, err := ResponsesRequestToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}
	m := result.Messages[0]
	if m.Role != "user" {
		t.Errorf("expected role user, got %s", m.Role)
	}
	if m.StringContent() != "Hello" {
		t.Errorf("expected content 'Hello', got %q", m.StringContent())
	}
}

func TestResponsesRequestToChatRequest_WithInstructions(t *testing.T) {
	req := &dto.OpenAIResponsesRequest{
		Model:        "deepseek-chat",
		Instructions: mustJSON("You are a helpful assistant"),
		Input: mustJSON([]map[string]any{
			{"role": "user", "content": "Hello"},
		}),
	}

	result, err := ResponsesRequestToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(result.Messages))
	}
	if result.Messages[0].Role != "system" {
		t.Errorf("expected first message to be system, got %s", result.Messages[0].Role)
	}
	if result.Messages[1].Role != "user" {
		t.Errorf("expected second message to be user, got %s", result.Messages[1].Role)
	}
}

func TestResponsesRequestToChatRequest_MultiTurn(t *testing.T) {
	req := &dto.OpenAIResponsesRequest{
		Model: "deepseek-chat",
		Input: mustJSON([]map[string]any{
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": "Hi there!"},
			{"role": "user", "content": "How are you?"},
		}),
	}

	result, err := ResponsesRequestToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result.Messages))
	}

	roles := []string{"user", "assistant", "user"}
	for i, r := range roles {
		if result.Messages[i].Role != r {
			t.Errorf("message %d: expected role %s, got %s", i, r, result.Messages[i].Role)
		}
	}
}

func TestResponsesRequestToChatRequest_WithTools(t *testing.T) {
	req := &dto.OpenAIResponsesRequest{
		Model: "deepseek-chat",
		Input: mustJSON([]map[string]any{
			{"role": "user", "content": "What's the weather?"},
		}),
		Tools: mustJSON([]map[string]any{
			{
				"type": "function",
				"name": "get_weather",
				"description": "Get weather for a city",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"city": map[string]any{"type": "string"},
					},
				},
			},
		}),
	}

	result, err := ResponsesRequestToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result.Tools))
	}
	if result.Tools[0].Function.Name != "get_weather" {
		t.Errorf("expected tool name get_weather, got %s", result.Tools[0].Function.Name)
	}
}

func TestResponsesRequestToChatRequest_FunctionCalls(t *testing.T) {
	req := &dto.OpenAIResponsesRequest{
		Model: "deepseek-chat",
		Input: mustJSON([]map[string]any{
			{"role": "user", "content": "What's the weather in Tokyo?"},
			{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": `{"city":"Tokyo"}`},
			{"type": "function_call_output", "call_id": "call_1", "output": "Sunny, 25C"},
		}),
	}

	result, err := ResponsesRequestToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expected: user msg, assistant msg (with tool_calls), tool msg
	if len(result.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result.Messages))
	}

	// Assistant should have tool_calls
	assistant := result.Messages[1]
	if assistant.Role != "assistant" {
		t.Errorf("msg[1]: expected assistant, got %s", assistant.Role)
	}
	toolCalls := assistant.ParseToolCalls()
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(toolCalls))
	}
	if toolCalls[0].ID != "call_1" {
		t.Errorf("expected call_id call_1, got %s", toolCalls[0].ID)
	}

	// Tool result
	toolMsg := result.Messages[2]
	if toolMsg.Role != "tool" {
		t.Errorf("msg[2]: expected tool, got %s", toolMsg.Role)
	}
}

func TestResponsesRequestToChatRequest_Reasoning(t *testing.T) {
	req := &dto.OpenAIResponsesRequest{
		Model: "deepseek-chat",
		Input: mustJSON([]map[string]any{
			{"role": "user", "content": "Hello"},
		}),
		Reasoning: &dto.Reasoning{
			Effort:  "medium",
			Summary: "detailed",
		},
	}

	result, err := ResponsesRequestToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.ReasoningEffort != "medium" {
		t.Errorf("expected reasoning_effort medium, got %s", result.ReasoningEffort)
	}
}

func TestResponsesRequestToChatRequest_Parameters(t *testing.T) {
	maxTokens := uint(100)
	temp := 0.7
	topP := 0.9
	stream := true

	req := &dto.OpenAIResponsesRequest{
		Model:          "deepseek-chat",
		MaxOutputTokens: &maxTokens,
		Temperature:    &temp,
		TopP:           &topP,
		Stream:         &stream,
		Input: mustJSON([]map[string]any{
			{"role": "user", "content": "Hello"},
		}),
	}

	result, err := ResponsesRequestToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if lo.FromPtr(result.MaxTokens) != maxTokens {
		t.Errorf("expected max_tokens %d, got %d", maxTokens, lo.FromPtr(result.MaxTokens))
	}
	if lo.FromPtr(result.Temperature) != temp {
		t.Errorf("expected temperature %f, got %f", temp, lo.FromPtr(result.Temperature))
	}
	if lo.FromPtr(result.TopP) != topP {
		t.Errorf("expected top_p %f, got %f", topP, lo.FromPtr(result.TopP))
	}
	if lo.FromPtr(result.Stream) != stream {
		t.Errorf("expected stream true, got %t", lo.FromPtr(result.Stream))
	}
}

func TestResponsesRequestToChatRequest_ToolChoice(t *testing.T) {
	req := &dto.OpenAIResponsesRequest{
		Model:      "deepseek-chat",
		ToolChoice: mustJSON("auto"),
		Input: mustJSON([]map[string]any{
			{"role": "user", "content": "Hello"},
		}),
	}

	result, err := ResponsesRequestToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if tc, ok := result.ToolChoice.(string); !ok || tc != "auto" {
		t.Errorf("expected tool_choice 'auto', got %v", result.ToolChoice)
	}

	// Named function tool_choice
	req2 := &dto.OpenAIResponsesRequest{
		Model: "deepseek-chat",
		ToolChoice: mustJSON(map[string]any{
			"type": "function",
			"name": "get_weather",
		}),
		Input: mustJSON([]map[string]any{
			{"role": "user", "content": "Hello"},
		}),
	}

	result2, err := ResponsesRequestToChatRequest(req2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if tc, ok := result2.ToolChoice.(map[string]any); ok {
		if fn, ok := tc["function"].(map[string]any); ok {
			if fn["name"] != "get_weather" {
				t.Errorf("expected tool_choice function name 'get_weather', got %v", fn["name"])
			}
		}
	}
}

func TestResponsesRequestToChatRequest_ImageContent(t *testing.T) {
	req := &dto.OpenAIResponsesRequest{
		Model: "deepseek-chat",
		Input: mustJSON([]map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": "Describe this"},
					{"type": "input_image", "image_url": "https://example.com/img.jpg"},
				},
			},
		}),
	}

	result, err := ResponsesRequestToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}

	contents := result.Messages[0].ParseContent()
	foundImage := false
	for _, c := range contents {
		if c.Type == dto.ContentTypeImageURL {
			foundImage = true
			if img := c.GetImageMedia(); img == nil || img.Url != "https://example.com/img.jpg" {
				t.Errorf("unexpected image url: %v", c.ImageUrl)
			}
		}
	}
	if !foundImage {
		t.Error("expected image content, none found")
	}
}

func TestResponsesRequestToChatRequest_DeveloperRoleMapping(t *testing.T) {
	req := &dto.OpenAIResponsesRequest{
		Model: "deepseek-chat",
		Input: mustJSON([]map[string]any{
			{"role": "developer", "content": "You are a helpful coding assistant."},
			{"role": "user", "content": "Write a function."},
		}),
	}

	result, err := ResponsesRequestToChatRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result.Messages))
	}

	// "developer" role should be mapped to "system"
	if result.Messages[0].Role != "system" {
		t.Errorf("expected developer→system mapping, got role %q", result.Messages[0].Role)
	}
	if result.Messages[0].StringContent() != "You are a helpful coding assistant." {
		t.Errorf("unexpected content: %q", result.Messages[0].StringContent())
	}
	if result.Messages[1].Role != "user" {
		t.Errorf("expected user role, got %q", result.Messages[1].Role)
	}
}
