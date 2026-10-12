package relay

import (
	"fmt"
	"io"
	"math/big"
	"net/http"

	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	openai "github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// responsesViaChatAdaptor retains the selected channel's transport and request
// normalization. Only the upstream protocol changes; client identity and billing
// remain Responses throughout the request lifecycle.
type responsesViaChatAdaptor struct{ relaychannel.Adaptor }

func (a *responsesViaChatAdaptor) Init(info *relaycommon.RelayInfo) {
	mode, path := info.RelayMode, info.RequestURLPath
	info.RelayMode, info.RequestURLPath = relayconstant.RelayModeChatCompletions, "/v1/chat/completions"
	defer func() { info.RelayMode, info.RequestURLPath = mode, path }()
	a.Adaptor.Init(info)
}

func (a *responsesViaChatAdaptor) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	expanded, err := service.PrepareResponsesReplay(c, info, &request, info.ChannelSetting.ResponsesLocalReplay)
	if err != nil {
		return nil, err
	}
	result, err := service.ConvertRequestByID(c, info, relayconvert.ConverterOpenAIResponsesToOpenAIChat, expanded)
	if err != nil {
		return nil, err
	}
	chat, ok := result.Value.(*dto.GeneralOpenAIRequest)
	if !ok {
		return nil, fmt.Errorf("expected Chat request, got %T", result.Value)
	}
	if info.IsStream && info.SupportStreamOptions {
		chat.StreamOptions = &dto.StreamOptions{IncludeUsage: true}
	}
	return a.Adaptor.ConvertOpenAIRequest(c, info, chat)
}

func (a *responsesViaChatAdaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	mode, path := info.RelayMode, info.RequestURLPath
	info.RelayMode, info.RequestURLPath = relayconstant.RelayModeChatCompletions, "/v1/chat/completions"
	defer func() { info.RelayMode, info.RequestURLPath = mode, path }()
	return a.Adaptor.GetRequestURL(info)
}

func (a *responsesViaChatAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, body io.Reader) (any, error) {
	// Original DoRequest invokes its own GetRequestURL, so the scoped upstream
	// mode must encompass that call, rather than only overriding the wrapper URL.
	mode, path := info.RelayMode, info.RequestURLPath
	info.RelayMode, info.RequestURLPath = relayconstant.RelayModeChatCompletions, "/v1/chat/completions"
	defer func() { info.RelayMode, info.RequestURLPath = mode, path }()
	return a.Adaptor.DoRequest(c, info, body)
}

func (a *responsesViaChatAdaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	if info.IsStream {
		return openai.OaiChatToResponsesStreamHandler(c, info, resp)
	}
	return openai.OaiChatToResponsesHandler(c, info, resp)
}

// validateResponsesViaChatOverrides keeps replay snapshots and tool aliases tied
// to the actual upstream conversation, while allowing sampling-only overrides.
func validateResponsesViaChatOverrides(before, after []byte, replay bool) error {
	result := gjson.ParseBytes(after)
	if n := result.Get("n"); n.Exists() && !responsesReplayJSONEqual(n, gjson.Parse("1")) {
		return fmt.Errorf("responses_via_chat requires n to be 1")
	}
	if !replay {
		return nil
	}
	original := gjson.ParseBytes(before)
	for _, field := range []string{"messages", "model", "tools", "tool_choice"} {
		if !responsesReplayJSONEqual(original.Get(field), result.Get(field)) {
			return fmt.Errorf("responses_local_replay does not allow parameter overrides to change %s", field)
		}
	}
	return nil
}

// Compare decoded values without rounding schema numbers through float64.
func responsesReplayJSONEqual(left, right gjson.Result) bool {
	if left.Exists() != right.Exists() || left.Type != right.Type {
		return false
	}
	switch left.Type {
	case gjson.Number:
		precision := uint(max(len(left.Raw), len(right.Raw))*4 + 64)
		a, _, errA := big.ParseFloat(left.Raw, 10, precision, big.ToNearestEven)
		b, _, errB := big.ParseFloat(right.Raw, 10, precision, big.ToNearestEven)
		return errA == nil && errB == nil && a.Cmp(b) == 0
	case gjson.String:
		return left.Str == right.Str
	case gjson.JSON:
		if left.IsArray() != right.IsArray() {
			return false
		}
		if left.IsArray() {
			a, b := left.Array(), right.Array()
			if len(a) != len(b) {
				return false
			}
			for i := range a {
				if !responsesReplayJSONEqual(a[i], b[i]) {
					return false
				}
			}
			return true
		}
		a, b := left.Map(), right.Map()
		if len(a) != len(b) {
			return false
		}
		for key, value := range a {
			if !responsesReplayJSONEqual(value, b[key]) {
				return false
			}
		}
	}
	return true
}
