package openaicompat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/samber/lo"
)

type responsesInputItem struct {
	Type    string          `json:"type,omitempty"`
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// function_call_output
	Output any `json:"output,omitempty"`
}

func (r *responsesInputItem) StringContent() string {
	if r.Content == nil {
		return ""
	}
	var s string
	if err := common.Unmarshal(r.Content, &s); err == nil {
		return s
	}
	return ""
}

// ResponsesRequestToChatRequest converts an OpenAI Responses API request into a Chat Completions request.
func ResponsesRequestToChatRequest(respReq *dto.OpenAIResponsesRequest) (*dto.GeneralOpenAIRequest, error) {
	if respReq == nil {
		return nil, errors.New("request is nil")
	}
	if respReq.Model == "" {
		return nil, errors.New("model is required")
	}

	chatReq := &dto.GeneralOpenAIRequest{
		Model:       respReq.Model,
		Temperature: respReq.Temperature,
		TopP:        respReq.TopP,
		Stream:      respReq.Stream,
	}

	if respReq.MaxOutputTokens != nil {
		chatReq.MaxTokens = lo.ToPtr(lo.FromPtr(respReq.MaxOutputTokens))
	}

	// Map reasoning
	if respReq.Reasoning != nil && respReq.Reasoning.Effort != "" {
		chatReq.ReasoningEffort = respReq.Reasoning.Effort
	}

	// Map tools
	respTools := respReq.GetToolsMap()
	if len(respTools) > 0 {
		chatTools := make([]dto.ToolCallRequest, 0, len(respTools))
		for _, tool := range respTools {
			toolType := common.Interface2String(tool["type"])
			switch toolType {
			case "function":
				chatTools = append(chatTools, dto.ToolCallRequest{
					Type: "function",
					Function: dto.FunctionRequest{
						Name:        common.Interface2String(tool["name"]),
						Description: common.Interface2String(tool["description"]),
						Parameters:  tool["parameters"],
					},
				})
			default:
				// Pass through unknown tool types
				toolJSON, _ := common.Marshal(tool)
				chatTools = append(chatTools, dto.ToolCallRequest{
					Type:   dto.CustomType,
					Custom: toolJSON,
				})
			}
		}
		chatReq.Tools = chatTools
	}

	// Map tool_choice
	chatReq.ToolChoice = mapResponsesToolChoiceToChat(respReq)

	// Map instructions → system message
	messages := make([]dto.Message, 0)
	if len(respReq.Instructions) > 0 {
		sysMsg := mapResponsesInstructionsToSystemMessage(respReq.Instructions)
		if sysMsg.Content != nil {
			messages = append(messages, sysMsg)
		}
	}

	// Map input items → messages
	inputMsgs, err := mapResponsesInputToMessages(respReq.Input)
	if err != nil {
		return nil, fmt.Errorf("parse responses input: %w", err)
	}
	messages = append(messages, inputMsgs...)

	chatReq.Messages = messages

	return chatReq, nil
}

func mapResponsesInstructionsToSystemMessage(instructions json.RawMessage) dto.Message {
	msg := dto.Message{Role: "system"}

	var str string
	if err := common.Unmarshal(instructions, &str); err == nil {
		msg.SetStringContent(str)
		return msg
	}

	var arr []map[string]any
	if err := common.Unmarshal(instructions, &arr); err == nil {
		var sb strings.Builder
		for _, item := range arr {
			if text, ok := item["text"].(string); ok && text != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(text)
			}
		}
		if sb.Len() > 0 {
			msg.SetStringContent(sb.String())
		}
	}

	return msg
}

func mapResponsesToolChoiceToChat(respReq *dto.OpenAIResponsesRequest) any {
	if len(respReq.ToolChoice) == 0 {
		return nil
	}

	var raw any
	if err := common.Unmarshal(respReq.ToolChoice, &raw); err != nil {
		return nil
	}

	switch v := raw.(type) {
	case string:
		return v
	case map[string]any:
		toolType := common.Interface2String(v["type"])
		if toolType == "function" {
			name := common.Interface2String(v["name"])
			if name != "" {
				return map[string]any{
					"type": "function",
					"function": map[string]any{
						"name": name,
					},
				}
			}
		}
		return v
	default:
		return raw
	}
}

func mapResponsesInputToMessages(input json.RawMessage) ([]dto.Message, error) {
	if len(input) == 0 {
		return nil, nil
	}

	var items []responsesInputItem
	if err := common.Unmarshal(input, &items); err != nil {
		return nil, err
	}

	messages := make([]dto.Message, 0, len(items))

	for _, item := range items {
		if item.Type == "function_call" {
			caller := findOrCreateLastAssistantMessage(&messages)
			toolCalls := caller.ParseToolCalls()
			toolCalls = append(toolCalls, dto.ToolCallRequest{
				ID:   item.CallID,
				Type: "function",
				Function: dto.FunctionRequest{
					Name:      item.Name,
					Arguments: item.Arguments,
				},
			})
			caller.SetToolCalls(toolCalls)
			continue
		}

		if item.Type == "function_call_output" {
			toolMsg := dto.Message{
				Role:       "tool",
				ToolCallId: item.CallID,
			}
			toolMsg.SetStringContent(fmt.Sprintf("%v", item.Output))
			messages = append(messages, toolMsg)
			continue
		}

		role := item.Role
		if role == "" {
			role = "user"
		}

		msg := dto.Message{Role: role}

		if item.Content != nil {
			s := item.StringContent()
			if s != "" {
				msg.SetStringContent(s)
			} else {
				mediaContents := mapResponsesContentToMediaContent(item.Content)
				if len(mediaContents) > 0 {
					msg.SetMediaContent(mediaContents)
				} else {
					msg.SetNullContent()
				}
			}
		}

		messages = append(messages, msg)
	}

	return messages, nil
}

func findOrCreateLastAssistantMessage(messages *[]dto.Message) *dto.Message {
	for i := len(*messages) - 1; i >= 0; i-- {
		if (*messages)[i].Role == "assistant" {
			return &(*messages)[i]
		}
	}
	msg := dto.Message{Role: "assistant"}
	*messages = append(*messages, msg)
	return &(*messages)[len(*messages)-1]
}

func mapResponsesContentToMediaContent(content json.RawMessage) []dto.MediaContent {
	var parts []map[string]any
	if err := common.Unmarshal(content, &parts); err != nil {
		return nil
	}

	result := make([]dto.MediaContent, 0, len(parts))
	for _, part := range parts {
		partType := common.Interface2String(part["type"])
		switch partType {
		case "input_text", "output_text":
			result = append(result, dto.MediaContent{
				Type: dto.ContentTypeText,
				Text: common.Interface2String(part["text"]),
			})
		case "input_image":
			imageURL := part["image_url"]
			if s, ok := imageURL.(string); ok {
				result = append(result, dto.MediaContent{
					Type:     dto.ContentTypeImageURL,
					ImageUrl: &dto.MessageImageUrl{Url: s},
				})
			} else if m, ok := imageURL.(map[string]any); ok {
				result = append(result, dto.MediaContent{
					Type:     dto.ContentTypeImageURL,
					ImageUrl: &dto.MessageImageUrl{Url: common.Interface2String(m["url"]), Detail: common.Interface2String(m["detail"])},
				})
			}
		case "input_audio":
			result = append(result, dto.MediaContent{
				Type:       dto.ContentTypeInputAudio,
				InputAudio: part["input_audio"],
			})
		case "input_file":
			result = append(result, dto.MediaContent{
				Type: dto.ContentTypeFile,
				File: part["file"],
			})
		case "input_video":
			videoURL := part["video_url"]
			if s, ok := videoURL.(string); ok {
				result = append(result, dto.MediaContent{
					Type:     dto.ContentTypeVideoUrl,
					VideoUrl: &dto.MessageVideoUrl{Url: s},
				})
			}
		default:
			// Best-effort: keep unknown types as-is
		}
	}

	return result
}
