package relay

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponsesViaChatPreservesChannelAndClient(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	settings := model_setting.GetGlobalSettings()
	previousPolicy := settings.ChatCompletionsToResponsesPolicy
	settings.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{Enabled: true, AllChannels: true, ModelPatterns: []string{".*"}}
	t.Cleanup(func() { settings.ChatCompletionsToResponsesPolicy = previousPolicy })
	for _, channelType := range []int{constant.ChannelTypeOpenAI, constant.ChannelTypeAzure, constant.ChannelTypeDeepSeek} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("channel_%d_stream_%t", channelType, stream), func(t *testing.T) {
				type observed struct {
					path, auth, organization string
					body                     []byte
				}
				calls := make(chan observed, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					b, _ := io.ReadAll(r.Body)
					auth := r.Header.Get("Authorization")
					if channelType == constant.ChannelTypeAzure {
						auth = r.Header.Get("api-key")
					}
					calls <- observed{r.URL.Path, auth, r.Header.Get("OpenAI-Organization"), b}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_test\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl_test\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5,\"prompt_cache_hit_tokens\":1}}\n\ndata: [DONE]\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"chatcmpl_test","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,"prompt_cache_hit_tokens":1}}`)
					}
				}))
				defer server.Close()
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				c.Request.Header.Set("Content-Type", "application/json")
				common.SetContextKey(c, constant.ContextKeyOriginalModel, "client-model")
				common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
				common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
				common.SetContextKey(c, constant.ContextKeyChannelKey, "test-key")
				common.SetContextKey(c, constant.ContextKeyChannelId, 73)
				common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{ResponsesViaChat: true})
				c.Set("channel_organization", "org-test")
				c.Set("model_mapping", `{"client-model":"upstream-model"}`)
				request := &dto.OpenAIResponsesRequest{Model: "client-model", Input: []byte(`"hello"`), Stream: common.GetPointer(stream)}
				info := relaycommon.GenRelayInfoResponses(c, request)
				originalPath := info.RequestURLPath
				adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)
				require.Nil(t, apiErr)
				defer closer.Close()
				upstreamURL, err := adaptor.GetRequestURL(info)
				require.NoError(t, err)
				assert.Contains(t, upstreamURL, "/chat/completions")
				assert.NotContains(t, upstreamURL, "/responses")
				response, err := adaptor.DoRequest(c, info, body)
				require.NoError(t, err)
				usage, apiErr := adaptor.DoResponse(c, response.(*http.Response), info)
				require.Nil(t, apiErr)
				require.IsType(t, &dto.Usage{}, usage)
				assert.Equal(t, 5, usage.(*dto.Usage).TotalTokens)
				if channelType == constant.ChannelTypeDeepSeek {
					assert.Contains(t, recorder.Body.String(), `"cached_tokens":1`)
				}
				call := <-calls
				assert.True(t, strings.HasSuffix(call.path, "/chat/completions"), call.path)
				assert.NotContains(t, call.path, "responses")
				if channelType == constant.ChannelTypeAzure {
					assert.Equal(t, "test-key", call.auth)
				} else {
					assert.Equal(t, "Bearer test-key", call.auth)
				}
				if channelType == constant.ChannelTypeOpenAI {
					assert.Equal(t, "org-test", call.organization)
				}
				var sent dto.GeneralOpenAIRequest
				require.NoError(t, common.Unmarshal(call.body, &sent))
				assert.Equal(t, "upstream-model", sent.Model)
				require.Len(t, sent.Messages, 1)
				assert.Equal(t, "hello", sent.Messages[0].StringContent())
				assert.Equal(t, "client-model", info.OriginModelName)
				assert.Equal(t, channelType, info.ChannelType)
				assert.Equal(t, 73, info.ChannelId)
				assert.Equal(t, relayconstant.RelayModeResponses, info.RelayMode)
				assert.Equal(t, types.RelayFormat(types.RelayFormatOpenAIResponses), info.RelayFormat)
				assert.Equal(t, originalPath, info.RequestURLPath)
				assert.Equal(t, []types.RelayFormat{types.RelayFormatOpenAIResponses, types.RelayFormatOpenAI}, info.RequestConversionChain)
				if stream {
					assert.Contains(t, recorder.Body.String(), "response.completed")
				} else {
					assert.Contains(t, recorder.Body.String(), `"object":"response"`)
				}
			})
		}
	}
}

func TestResponsesViaChatRejectsUnsupportedBeforeSend(t *testing.T) {
	for _, test := range []struct {
		name                string
		channel             int
		settings            dto.ChannelSettings
		compact, globalPass bool
	}{
		{name: "unsupported", channel: constant.ChannelTypeGemini, settings: dto.ChannelSettings{ResponsesViaChat: true}},
		{name: "unknown fallback", channel: 999, settings: dto.ChannelSettings{ResponsesViaChat: true}},
		{name: "channel passthrough", channel: constant.ChannelTypeOpenAI, settings: dto.ChannelSettings{ResponsesViaChat: true, PassThroughBodyEnabled: true}},
		{name: "global passthrough", channel: constant.ChannelTypeOpenAI, settings: dto.ChannelSettings{ResponsesViaChat: true}, globalPass: true},
		{name: "compact", channel: constant.ChannelTypeOpenAI, settings: dto.ChannelSettings{ResponsesViaChat: true}, compact: true},
		{name: "invalid profile", channel: constant.ChannelTypeOpenAI, settings: dto.ChannelSettings{ResponsesViaChat: true, ResponsesChatCompatibility: "other"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := model_setting.GetGlobalSettings()
			saved := settings.PassThroughRequestEnabled
			settings.PassThroughRequestEnabled = test.globalPass
			defer func() { settings.PassThroughRequestEnabled = saved }()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			common.SetContextKey(c, constant.ContextKeyChannelType, test.channel)
			common.SetContextKey(c, constant.ContextKeyChannelSetting, test.settings)
			request := &dto.OpenAIResponsesRequest{Model: "model", Input: []byte(`"hello"`)}
			info := relaycommon.GenRelayInfoResponses(c, request)
			if test.compact {
				info.RelayMode = relayconstant.RelayModeResponsesCompact
			}
			adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)
			require.NotNil(t, apiErr)
			assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
			assert.Nil(t, adaptor)
			assert.Nil(t, body)
			assert.Nil(t, closer)
		})
	}
}

func TestResponsesViaChatDisabledKeepsNativeResponses(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://example.com")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "model")
	request := &dto.OpenAIResponsesRequest{Model: "model", Input: []byte(`"hello"`)}
	info := relaycommon.GenRelayInfoResponses(c, request)
	adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)
	require.Nil(t, apiErr)
	defer closer.Close()
	url, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/v1/responses", url)
	raw, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"input":"hello"`)
	assert.NotContains(t, string(raw), `"messages"`)
	assert.Equal(t, []types.RelayFormat{types.RelayFormatOpenAIResponses}, info.RequestConversionChain)
}

func TestResponsesViaChatTransportFailureRestoresClientRoute(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, ":invalid-url")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "model")
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{ResponsesViaChat: true})
	request := &dto.OpenAIResponsesRequest{Model: "model", Input: []byte(`"hello"`)}
	info := relaycommon.GenRelayInfoResponses(c, request)
	adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)
	require.Nil(t, apiErr)
	defer closer.Close()
	_, err := adaptor.DoRequest(c, info, body)
	require.Error(t, err)
	assert.Equal(t, relayconstant.RelayModeResponses, info.RelayMode)
	assert.Equal(t, "/v1/responses", info.RequestURLPath)
	assert.Equal(t, types.RelayFormat(types.RelayFormatOpenAIResponses), info.RelayFormat)
}

func TestResponsesViaChatOverrideGuards(t *testing.T) {
	before := []byte(`{"model":"mapped","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"maximum":9007199254740992}}}],"tool_choice":"auto"}`)
	for _, tc := range []struct {
		name, override  string
		replay, allowed bool
	}{
		{"sampling", `{"temperature":0.3}`, true, true},
		{"same model", `{"model":"mapped"}`, true, true},
		{"same messages reordered keys", `{"messages":[{"content":"hello","role":"user"}]}`, true, true},
		{"same tool choice", `{"tool_choice":"auto"}`, true, true},
		{"changed model", `{"model":"other"}`, true, false},
		{"changed message", `{"messages":[{"role":"user","content":"different"}]}`, true, false},
		{"changed tools", `{"tools":[]}`, true, false},
		{"changed tool choice", `{"tool_choice":"none"}`, true, false},
		{"large schema number changed", `{"tools":[{"type":"function","function":{"name":"lookup","parameters":{"maximum":9007199254740993}}}]}`, true, false},
		{"one choice", `{"n":1}`, true, true},
		{"two choices", `{"n":2}`, true, false},
		{"zero choices", `{"n":0}`, true, false},
		{"string choices", `{"n":"1"}`, false, false},
		{"multiple without replay", `{"n":2}`, false, false},
		{"model change without replay", `{"model":"other"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Raw field merging retains exact JSON numbers in tool schemas.
			var source, changes map[string]common.RawMessage
			require.NoError(t, common.Unmarshal(before, &source))
			require.NoError(t, common.Unmarshal([]byte(tc.override), &changes))
			for key, value := range changes {
				source[key] = value
			}
			after, err := common.Marshal(source)
			require.NoError(t, err)
			err = validateResponsesViaChatOverrides(before, after, tc.replay)
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	require.Error(t, validateResponsesViaChatOverrides(before, []byte(`{"model":"mapped","messages":[],"n":1}`), true))
}

func TestResponsesViaChatRejectsMultipleChoicesBeforeSend(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "model")
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{ResponsesViaChat: true})
	common.SetContextKey(c, constant.ContextKeyChannelParamOverride, map[string]any{"n": 2})
	request := &dto.OpenAIResponsesRequest{Model: "model", Input: []byte(`"hello"`)}
	info := relaycommon.GenRelayInfoResponses(c, request)
	adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	assert.Nil(t, adaptor)
	assert.Nil(t, body)
	assert.Nil(t, closer)
}

func TestResponsesLocalReplayParameterOverrides(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override map[string]any
		allowed  bool
	}{
		{"sampling remains usable", map[string]any{"temperature": 0.25}, true},
		{"unchanged model remains usable", map[string]any{"model": "model"}, true},
		{"changed model rejected", map[string]any{"model": "other"}, false},
		{"changed messages rejected", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "replacement"}}}, false},
		{"added tools rejected", map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "extra"}}}}, false},
		{"added tool choice rejected", map[string]any{"tool_choice": "none"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "model")
			common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{ResponsesViaChat: true, ResponsesLocalReplay: true})
			common.SetContextKey(c, constant.ContextKeyChannelParamOverride, tc.override)
			common.SetContextKey(c, constant.ContextKeyChannelHeaderOverride, map[string]any{"X-Test": "preserved"})
			request := &dto.OpenAIResponsesRequest{Model: "model", Input: []byte(`"hello"`)}
			info := relaycommon.GenRelayInfoResponses(c, request)
			info.UserId = 1
			info.TokenId = 2
			adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)
			if tc.allowed {
				require.Nil(t, apiErr)
				require.NotNil(t, body)
				require.NotNil(t, adaptor)
				require.NoError(t, closer.Close())
				assert.Equal(t, "preserved", info.HeadersOverride["X-Test"])
			} else {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
				assert.Nil(t, adaptor)
				assert.Nil(t, body)
				assert.Nil(t, closer)
			}
		})
	}
}
