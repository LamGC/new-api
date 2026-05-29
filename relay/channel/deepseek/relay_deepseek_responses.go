package deepseek

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// DeepSeekResponsesHandler handles non-streaming Chat response conversion to Responses format.
func DeepSeekResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	var chatResp dto.OpenAITextResponse
	if err := common.Unmarshal(body, &chatResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	if oaiErr := chatResp.GetOpenAIError(); oaiErr != nil && oaiErr.Type != "" {
		return nil, types.WithOpenAIError(*oaiErr, resp.StatusCode)
	}

	responseID := resolveResponseID(info)
	responsesResp := buildResponsesResponseFromChat(&chatResp, responseID, info.UpstreamModelName)

	responseBody, err := common.Marshal(responsesResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	if _, err := c.Writer.Write(responseBody); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	// Store assistant messages for conversation persistence
	if convInfo := info.ResponsesConversationInfo; convInfo != nil {
		for _, choice := range chatResp.Choices {
			convInfo.AssistantMessages = append(convInfo.AssistantMessages, choice.Message)
		}
	}

	return &chatResp.Usage, nil
}

// DeepSeekResponsesStreamHandler handles streaming Chat SSE response conversion to Responses SSE.
func DeepSeekResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)

	responseID := resolveResponseID(info)
	model := info.UpstreamModelName
	createdAt := time.Now().Unix()

	var (
		usage           = &dto.Usage{}
		responseText    strings.Builder
		streamErr       *types.NewAPIError
		sentCreated     bool
		sentItemAdded   bool
		sentContentPart bool
		finished        bool
		hasToolCalls    bool
		sentToolItems   = make(map[int]bool)

		// Tool call tracking
		toolCallIDByIndex    = make(map[int]string)
		toolCallNameByIndex  = make(map[int]string)
		toolCallArgsByIndex  = make(map[int]string)
		toolCallNameSent    = make(map[int]bool)

		// Reasoning tracking
		hasSentReasoning     bool
	)

	sendResponsesSSE := func(eventType string, data string) error {
		if c.Request.Context().Err() != nil {
			return c.Request.Context().Err()
		}
		helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: eventType}, data)
		return nil
	}

	maybeSendCreated := func() error {
		if sentCreated {
			return nil
		}
		createdEvent := map[string]any{
			"id":      responseID,
			"object":  "response",
			"model":   model,
			"status":  "in_progress",
			"created_at": createdAt,
			"output":  []map[string]any{},
		}
		createdJSON, _ := common.Marshal(createdEvent)
		if err := sendResponsesSSE("response.created", string(createdJSON)); err != nil {
			return err
		}
		sentCreated = true

		inProgressEvent := map[string]any{
			"id": responseID,
			"object": "response",
			"status": "in_progress",
		}
		inProgJSON, _ := common.Marshal(inProgressEvent)
		_ = sendResponsesSSE("response.in_progress", string(inProgJSON))
		return nil
	}

	maybeSendItemAndPart := func() error {
		if sentItemAdded {
			return nil
		}
		if err := maybeSendCreated(); err != nil {
			return err
		}

		itemID := "resp_item_" + responseID
		itemEvent := map[string]any{
			"id":      itemID,
			"type":    "message",
			"role":    "assistant",
			"status":  "in_progress",
		}
		itemJSON, _ := common.Marshal(itemEvent)
		if err := sendResponsesSSE("response.output_item.added", string(itemJSON)); err != nil {
			return err
		}
		sentItemAdded = true

		partEvent := map[string]any{
			"type": "output_text",
			"text": "",
		}
		partJSON, _ := common.Marshal(partEvent)
		contentPartData := fmt.Sprintf(`{"item_id":"%s","output_index":0,"content_index":0,"part":%s}`, itemID, string(partJSON))
		if err := sendResponsesSSE("response.content_part.added", contentPartData); err != nil {
			return err
		}
		sentContentPart = true
		return nil
	}

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if streamErr != nil {
			sr.Stop(streamErr)
			return
		}
		if finished {
			sr.Done()
			return
		}

		var streamResp dto.ChatCompletionsStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResp); err != nil {
			logger.LogError(c, "failed to unmarshal chat stream chunk: "+err.Error())
			sr.Error(err)
			return
		}

		if len(streamResp.Choices) == 0 {
			// Usage-only chunk
			if streamResp.Usage != nil && service.ValidUsage(streamResp.Usage) {
				*usage = *streamResp.Usage
			}
			return
		}

		choice := streamResp.Choices[0]
		delta := choice.Delta
		itemID := "resp_item_" + responseID

		// Handle tool calls
		if len(delta.ToolCalls) > 0 {
			hasToolCalls = true
			for _, tc := range delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}

				if tc.ID != "" {
					toolCallIDByIndex[idx] = tc.ID
				}

				if tc.Function.Name != "" {
					toolCallNameByIndex[idx] = tc.Function.Name
				}

				if !sentToolItems[idx] {
					if err := maybeSendCreated(); err != nil {
						streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
						sr.Stop(streamErr)
						return
					}
					callID := toolCallIDByIndex[idx]
					if callID == "" {
						callID = fmt.Sprintf("call_%s_%d", responseID, idx)
						toolCallIDByIndex[idx] = callID
					}
					toolItem := map[string]any{
						"id":      fmt.Sprintf("resp_tool_item_%s_%d", responseID, idx),
						"type":    "function_call",
						"status":  "in_progress",
						"call_id": callID,
					}
					if name := toolCallNameByIndex[idx]; name != "" && !toolCallNameSent[idx] {
						toolItem["name"] = name
						toolCallNameSent[idx] = true
					}
					toolItemJSON, _ := common.Marshal(toolItem)
					if err := sendResponsesSSE("response.output_item.added", string(toolItemJSON)); err != nil {
						streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
						sr.Stop(streamErr)
						return
					}
					sentToolItems[idx] = true
				}

			// Send function_call_arguments.delta
			if tc.Function.Arguments != "" {
				toolCallArgsByIndex[idx] += tc.Function.Arguments
				argsDelta := tc.Function.Arguments
					toolItemID := fmt.Sprintf("resp_tool_item_%s_%d", responseID, idx)
					argsData := fmt.Sprintf(`{"item_id":"%s","output_index":%d,"delta":"%s"}`,
						toolItemID, idx, jsonEscape(argsDelta))
					if err := sendResponsesSSE("response.function_call_arguments.delta", argsData); err != nil {
						streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
						sr.Stop(streamErr)
						return
					}
				}
			}
			return
		}

		// Handle text content
		content := delta.GetContentString()
		if content != "" {
			if !hasToolCalls {
				if err := maybeSendItemAndPart(); err != nil {
					streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
					sr.Stop(streamErr)
					return
				}
			}
			responseText.WriteString(content)
			textData := fmt.Sprintf(`{"item_id":"%s","output_index":0,"content_index":0,"delta":"%s"}`,
				itemID, jsonEscape(content))
			if err := sendResponsesSSE("response.output_text.delta", textData); err != nil {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
				sr.Stop(streamErr)
				return
			}
		}

		// Handle reasoning content
		reasoning := delta.GetReasoningContent()
		if reasoning != "" {
			if err := maybeSendCreated(); err != nil {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
				sr.Stop(streamErr)
				return
			}
			summaryData := fmt.Sprintf(`{"item_id":"%s","summary_index":0,"delta":"%s"}`,
				itemID, jsonEscape(reasoning))
			if err := sendResponsesSSE("response.reasoning_summary_text.delta", summaryData); err != nil {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
				sr.Stop(streamErr)
				return
			}
			hasSentReasoning = true
		}

		// Handle completion
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			if streamResp.Usage != nil && service.ValidUsage(streamResp.Usage) {
				*usage = *streamResp.Usage
			}

			// Send done events for text items
			if !hasToolCalls && sentContentPart {
				sendResponsesSSE("response.output_text.done",
					fmt.Sprintf(`{"item_id":"%s","output_index":0,"content_index":0,"text":"%s"}`,
						itemID, jsonEscape(responseText.String())))

				sendResponsesSSE("response.content_part.done",
					fmt.Sprintf(`{"item_id":"%s","output_index":0,"content_index":0,"part":{"type":"output_text","text":"%s"}}`,
						itemID, jsonEscape(responseText.String())))
			}

			// Send done events for tool call items
			for idx := range sentToolItems {
				toolItemID := fmt.Sprintf("resp_tool_item_%s_%d", responseID, idx)
				args := toolCallArgsByIndex[idx]
				callID := toolCallIDByIndex[idx]
				name := toolCallNameByIndex[idx]

				argsDone := fmt.Sprintf(`{"item_id":"%s","output_index":%d,"arguments":"%s"}`,
					toolItemID, idx, jsonEscape(args))
				sendResponsesSSE("response.function_call_arguments.done", argsDone)

				itemDone := map[string]any{
					"id":        toolItemID,
					"type":      "function_call",
					"status":    "completed",
					"call_id":   callID,
					"name":      name,
					"arguments": args,
				}
				itemDoneJSON, _ := common.Marshal(itemDone)
				sendResponsesSSE("response.output_item.done", string(itemDoneJSON))
			}

			// Send output_item.done for text message
			if !hasToolCalls && sentItemAdded {
				outputItem := map[string]any{
					"id":     itemID,
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{"type": "output_text", "text": responseText.String()},
					},
				}
				itemDoneJSON, _ := common.Marshal(outputItem)
				sendResponsesSSE("response.output_item.done", string(itemDoneJSON))
			}

			if hasSentReasoning {
				sendResponsesSSE("response.reasoning_summary_text.done",
					fmt.Sprintf(`{"item_id":"%s","summary_index":0,"text":"%s"}`, itemID, jsonEscape(reasoning)))
			}

			// Send response.completed
			completedEvent := map[string]any{
				"id":      responseID,
				"object":  "response",
				"model":   model,
				"status":  "completed",
				"created_at": createdAt,
				"usage":   usage,
			}
			completedJSON, _ := common.Marshal(completedEvent)
			if err := sendResponsesSSE("response.completed", string(completedJSON)); err != nil {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
				sr.Stop(streamErr)
				return
			}
			finished = true
			sr.Done()
		}
	})

	if streamErr != nil {
		return nil, streamErr
	}

	if usage.TotalTokens == 0 && responseText.Len() > 0 {
		usage = service.ResponseText2Usage(c, responseText.String(), info.UpstreamModelName, info.GetEstimatePromptTokens())
	}

	// Store assistant messages for conversation persistence
	if convInfo := info.ResponsesConversationInfo; convInfo != nil {
		assistantMsg := dto.Message{Role: "assistant"}
		if responseText.Len() > 0 {
			assistantMsg.SetStringContent(responseText.String())
		}
		// Collect tool calls if any
		var toolCalls []dto.ToolCallRequest
		for idx := range sentToolItems {
			callID := toolCallIDByIndex[idx]
			name := toolCallNameByIndex[idx]
			args := toolCallArgsByIndex[idx]
			toolCalls = append(toolCalls, dto.ToolCallRequest{
				ID:   callID,
				Type: "function",
				Function: dto.FunctionRequest{
					Name:      name,
					Arguments: args,
				},
			})
		}
		if len(toolCalls) > 0 {
			assistantMsg.SetToolCalls(toolCalls)
		}
		convInfo.AssistantMessages = append(convInfo.AssistantMessages, assistantMsg)
	}

	return usage, nil
}

func buildResponsesResponseFromChat(chatResp *dto.OpenAITextResponse, responseID string, model string) *dto.OpenAIResponsesResponse {
	output := make([]dto.ResponsesOutput, 0)

	for _, choice := range chatResp.Choices {
		item := dto.ResponsesOutput{
			Type:   "message",
			ID:     "resp_item_" + responseID,
			Role:   "assistant",
			Status: "completed",
		}

		text := choice.Message.StringContent()
		if text != "" {
			item.Content = []dto.ResponsesOutputContent{
				{
					Type: "output_text",
					Text: text,
				},
			}
		}

		if choice.Message.ReasoningContent != nil && *choice.Message.ReasoningContent != "" {
			// Add reasoning as a separate output item
			reasoningItem := dto.ResponsesOutput{
				Type:   "message",
				ID:     "resp_item_reasoning_" + responseID,
				Role:   "assistant",
				Status: "completed",
				Content: []dto.ResponsesOutputContent{
					{
						Type: "output_text",
						Text: "",
					},
				},
			}
			_ = reasoningItem
			// reasoning is usually represented as reasoning_content in Chat, but
			// Responses API puts it at the response level. For now, omit explicit
			// reasoning output items since Chat API doesn't separate them clearly.
		}

		output = append(output, item)

		// Tool calls
		for _, tc := range choice.Message.ParseToolCalls() {
			output = append(output, dto.ResponsesOutput{
				Type:      "function_call",
				ID:        "resp_tool_item_" + responseID + "_" + tc.ID,
				Status:    "completed",
				CallId:    tc.ID,
				Name:      tc.Function.Name,
				Arguments: json.RawMessage(fmt.Sprintf(`"%s"`, jsonEscape(tc.Function.Arguments))),
			})
		}
	}

	responsesUsage := &dto.Usage{
		PromptTokens:     chatResp.Usage.PromptTokens,
		CompletionTokens: chatResp.Usage.CompletionTokens,
		TotalTokens:      chatResp.Usage.TotalTokens,
		InputTokens:      chatResp.Usage.PromptTokens,
		OutputTokens:     chatResp.Usage.CompletionTokens,
	}

	return &dto.OpenAIResponsesResponse{
		ID:        responseID,
		Object:    "response",
		CreatedAt: int(time.Now().Unix()),
		Model:     model,
		Status:    json.RawMessage(`"completed"`),
		Output:    output,
		Usage:     responsesUsage,
	}
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func resolveResponseID(info *relaycommon.RelayInfo) string {
	if info.ResponsesConversationInfo != nil && info.ResponsesConversationInfo.NewResponseID != "" {
		return info.ResponsesConversationInfo.NewResponseID
	}
	return "resp_" + common.GetTimeString() + common.GetRandomString(8)
}
