package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

// Replay is an explicit channel opt-in: immutable full snapshots, at most 32
// turns and 1 MiB of canonical input/output JSON, retained for 24 hours.
// Previous top-level instructions are intentionally never inherited. Legacy
// conversation_responses are not read: they lack token/provider provenance.
const responsesReplayContextKey = "new-api.responses-local-replay"
const responsesReplayBilledKey = "new-api.responses-local-replay-billed"
const responsesReplayMaxBytes = 1024 * 1024
const responsesReplayMaxDepth = 32
const responsesReplayTTL = 24 * time.Hour

type responsesReplayClientError string

func (e responsesReplayClientError) Error() string { return string(e) }

func IsResponsesReplayClientError(err error) bool {
	var invalid responsesReplayClientError
	return errors.Is(err, model.ErrResponsesReplayNotFound) || errors.As(err, &invalid)
}

type responsesReplayContext struct {
	input json.RawMessage
	scope string
	depth int
	store bool
}

// MarkResponsesReplayBilled pins replay-enabled billing preparation to its exact
// route and original previous ID. A retry cannot newly enable or reroute replay
// after history token reservation was already calculated.
func MarkResponsesReplayBilled(c *gin.Context, info *relaycommon.RelayInfo, request *dto.OpenAIResponsesRequest, enabled bool) {
	marker := [2]string{}
	if enabled {
		marker = [2]string{responsesReplayScope(info), request.PreviousResponseID}
	}
	c.Set(responsesReplayBilledKey, marker)
}

func responsesReplayScope(info *relaycommon.RelayInfo) string {
	// Hash routing identity, never credentials. A channel/model/provider change
	// cannot silently continue a conversation on an incompatible route.
	value := fmt.Sprintf("%d\x00%d\x00%d\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s", info.ChannelId, info.ChannelType, info.ChannelCreateTime, info.ApiType, info.ChannelBaseUrl, info.ApiVersion, info.Organization, info.OriginModelName, info.UpstreamModelName)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func responsesReplayInput(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var text string
	if common.Unmarshal(raw, &text) == nil {
		message, err := common.Marshal(map[string]any{"type": "message", "role": "user", "content": text})
		return []json.RawMessage{message}, err
	}
	var items []json.RawMessage
	if err := common.Unmarshal(raw, &items); err != nil {
		return nil, responsesReplayClientError("local responses replay requires string or array input")
	}
	return items, nil
}

// responsesReplayStoredInput accepts only history-bearing item types. Tool
// declarations are current-turn-only; hosted-tool configuration must not enter history.
// Function arguments and tool outputs remain opaque application data.
func responsesReplayStoredInput(items []json.RawMessage) (json.RawMessage, error) {
	filtered := make([]json.RawMessage, 0, len(items))
	for _, raw := range items {
		var item map[string]json.RawMessage
		if err := common.Unmarshal(raw, &item); err != nil || item == nil {
			return nil, responsesReplayClientError("local responses replay requires object input items")
		}
		var kind string
		if len(item["type"]) > 0 {
			if err := common.Unmarshal(item["type"], &kind); err != nil {
				return nil, responsesReplayClientError("invalid local responses replay item type")
			}
		}
		switch strings.TrimSpace(kind) {
		case "additional_tools":
			continue
		case "", "message", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "reasoning":
		default:
			return nil, responsesReplayClientError("local responses replay does not support this history item type; put tool declarations in top-level tools")
		}
		for key := range item {
			switch strings.ToLower(key) {
			case "authorization", "headers", "api_key", "access_token", "credentials", "tools":
				return nil, responsesReplayClientError("local responses replay does not support credential or tool declaration fields on history items")
			}
		}
		filtered = append(filtered, raw)
	}
	return common.Marshal(filtered)
}

// PrepareResponsesReplay expands only canonical input and saved output before
// protocol conversion. It never imports instructions from a previous request.
func PrepareResponsesReplay(c *gin.Context, info *relaycommon.RelayInfo, request *dto.OpenAIResponsesRequest, enabled bool) (*dto.OpenAIResponsesRequest, error) {
	c.Set(responsesReplayContextKey, (*responsesReplayContext)(nil))
	if !enabled {
		return request, nil
	}
	if request == nil {
		return nil, responsesReplayClientError("responses request is required")
	}
	if info == nil || info.ChannelMeta == nil || info.UserId <= 0 || info.TokenId <= 0 {
		return nil, responsesReplayClientError("local responses replay requires an authenticated user and API token")
	}
	raw, err := common.Marshal(request)
	if err != nil {
		return nil, err
	}
	var cloned dto.OpenAIResponsesRequest
	if err := common.Unmarshal(raw, &cloned); err != nil {
		return nil, err
	}
	store := true
	if len(cloned.Store) != 0 {
		if bytes.Equal(bytes.TrimSpace(cloned.Store), []byte("null")) || common.Unmarshal(cloned.Store, &store) != nil {
			return nil, responsesReplayClientError("store must be a boolean")
		}
	}
	current, err := responsesReplayInput(cloned.Input)
	if err != nil {
		return nil, err
	}
	scope := responsesReplayScope(info)
	if value, exists := c.Get(responsesReplayBilledKey); exists && cloned.PreviousResponseID != "" {
		marker, ok := value.([2]string)
		if !ok || marker != [2]string{scope, cloned.PreviousResponseID} {
			return nil, model.ErrResponsesReplayNotFound
		}
	}
	depth := 1
	if cloned.PreviousResponseID != "" {
		previous, err := model.FindResponsesReplay(c.Request.Context(), cloned.PreviousResponseID, info.UserId, info.TokenId, scope, time.Now().Unix())
		if err != nil {
			if errors.Is(err, model.ErrResponsesReplayNotFound) {
				return nil, err
			}
			return nil, errors.New("local responses replay storage could not be read")
		}
		if previous.Version != 1 || previous.Status != "completed" || previous.Depth < 1 {
			return nil, model.ErrResponsesReplayNotFound
		}
		if len(previous.Input)+len(previous.Output) > responsesReplayMaxBytes {
			return nil, errors.New("local responses replay snapshot is invalid")
		}
		depth = previous.Depth + 1
		if depth > responsesReplayMaxDepth {
			return nil, responsesReplayClientError("local responses replay exceeds 32 turns; start a new response")
		}
		var history, output []json.RawMessage
		if err := common.UnmarshalJsonStr(string(previous.Input), &history); err != nil {
			return nil, errors.New("local responses replay snapshot is invalid")
		}
		if err := common.UnmarshalJsonStr(string(previous.Output), &output); err != nil {
			return nil, errors.New("local responses replay snapshot is invalid")
		}
		current = append(append(history, output...), current...)
		cloned.PreviousResponseID = ""
	}
	cloned.Input, err = common.Marshal(current)
	if err != nil {
		return nil, err
	}
	if len(cloned.Input) > responsesReplayMaxBytes {
		return nil, responsesReplayClientError("local responses replay exceeds 1 MiB; start a new response")
	}
	// Store only canonical history, without current tool definitions.
	savedInput, err := responsesReplayStoredInput(current)
	if err != nil {
		return nil, err
	}
	c.Set(responsesReplayContextKey, &responsesReplayContext{input: savedInput, scope: scope, depth: depth, store: store})
	return &cloned, nil
}

// PersistResponsesReplay must run before delivering a success terminal. Failed,
// incomplete and cancelled responses never become usable previous response IDs.
func PersistResponsesReplay(c *gin.Context, info *relaycommon.RelayInfo, response *dto.OpenAIResponsesResponse) error {
	value, _ := c.Get(responsesReplayContextKey)
	replay, _ := value.(*responsesReplayContext)
	if replay == nil {
		return nil
	}
	response.Store = false
	if !replay.store {
		return nil
	}
	if err := c.Request.Context().Err(); err != nil {
		return err
	}
	var status string
	if common.Unmarshal(response.Status, &status) != nil || status != "completed" {
		return nil
	}
	if response.ID == "" || len(response.ID) > 128 {
		return errors.New("invalid local responses replay response ID")
	}
	output, err := common.Marshal(response.Output)
	if err != nil {
		return err
	}
	var outputItems []json.RawMessage
	if err := common.Unmarshal(output, &outputItems); err != nil {
		return err
	}
	if output, err = responsesReplayStoredInput(outputItems); err != nil {
		return errors.New("local responses replay cannot store unsupported output credential fields")
	}
	if len(replay.input)+len(output) > responsesReplayMaxBytes {
		return errors.New("local responses replay exceeds 1 MiB")
	}
	now := time.Now().Unix()
	snapshot := &model.ResponsesReplaySnapshot{ResponseID: response.ID, UserID: info.UserId, TokenID: info.TokenId, Route: replay.scope, Version: 1, Depth: replay.depth, Input: model.ReplayText(replay.input), Output: model.ReplayText(output), Status: status, CreatedAt: now, ExpiresAt: now + int64(responsesReplayTTL/time.Second)}
	if err := model.InsertResponsesReplay(c.Request.Context(), snapshot); err != nil {
		return errors.New("local responses replay storage could not be written")
	}
	response.Store = true
	return nil
}
