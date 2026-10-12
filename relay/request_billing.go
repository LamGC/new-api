package relay

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	constant2 "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

// PrepareRequestBilling estimates and reserves one request's charge. Transports
// provide the current request body through BodyStorage or BillingRequestInput;
// channel retries retain the resulting billing session and pricing snapshot.
func PrepareRequestBilling(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	billingRequest, apiErr := prepareResponsesReplayBilling(c, info)
	if apiErr != nil {
		return apiErr
	}
	needSensitiveCheck := setting.ShouldCheckPromptSensitive()
	meta := &types.TokenCountMeta{TokenType: types.TokenTypeTokenizer}
	if billingRequest != nil && (needSensitiveCheck || constant.CountToken) {
		meta = billingRequest.GetTokenCountMeta()
	} else {
		// Avoid building CombineText when only the pricing quantities are needed.
		switch request := billingRequest.(type) {
		case *dto.GeneralOpenAIRequest:
			meta.MaxTokens = int(max(lo.FromPtr(request.MaxTokens), lo.FromPtr(request.MaxCompletionTokens)))
		case *dto.OpenAIResponsesRequest:
			meta.MaxTokens = int(lo.FromPtr(request.MaxOutputTokens))
		case *dto.ClaudeRequest:
			meta.MaxTokens = int(lo.FromPtr(request.MaxTokens))
		case *dto.ImageRequest:
			meta = request.GetTokenCountMeta()
		}
	}

	if needSensitiveCheck && meta != nil {
		if contains, words := service.CheckSensitiveText(meta.CombineText); contains {
			service.RequestPolicy(c).AddEvent(service.PolicyEvent{ErrorCode: string(types.ErrorCodeSensitiveWordsDetected), ErrorSource: "local", Decision: service.PolicyDecision{Action: "stop", Reason: "local_rejection", Source: "global"}, Health: "unchanged"})
			message := fmt.Sprintf("user sensitive words detected: %s", strings.Join(words, ", "))
			logger.LogWarn(c, message)
			return types.NewError(errors.New(message), types.ErrorCodeSensitiveWordsDetected)
		}
	}

	tokens, err := service.EstimateRequestToken(c, meta, info)
	if err != nil {
		return types.NewError(err, types.ErrorCodeCountTokenFailed)
	}
	info.SetEstimatePromptTokens(tokens)

	priceData, err := helper.ModelPriceHelper(c, info, tokens, meta)
	if err != nil {
		return types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithStatusCode(http.StatusBadRequest))
	}
	if priceData.FreeModel {
		logger.LogInfo(c, common.LogText("model %s is free, skipping pre-consume", info.OriginModelName))
		return nil
	}
	return service.PreConsumeBilling(c, priceData.QuotaToPreConsume, info)
}

// prepareResponsesReplayBilling expands local history before sensitive-content
// checks and prompt reservation. It must not initialize the real ChannelMeta:
// the controller uses a nil ChannelMeta to retain the already selected first
// channel rather than selecting a retry before the first upstream request.
func prepareResponsesReplayBilling(c *gin.Context, info *relaycommon.RelayInfo) (dto.Request, *types.NewAPIError) {
	request, ok := info.Request.(*dto.OpenAIResponsesRequest)
	if !ok {
		return info.Request, nil
	}
	settings, _ := common.GetContextKeyType[dto.ChannelSettings](c, constant.ContextKeyChannelSetting)
	if !settings.ResponsesLocalReplay {
		service.MarkResponsesReplayBilled(c, info, request, false)
		return request, nil
	}
	channelType := common.GetContextKeyInt(c, constant.ContextKeyChannelType)
	if err := settings.ValidateResponsesViaChat(common.SupportsResponsesViaChat(channelType)); err != nil {
		return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	if info.RelayMode != constant2.RelayModeResponses || model_setting.GetGlobalSettings().PassThroughRequestEnabled {
		return nil, types.NewErrorWithStatusCode(errors.New("local responses replay requires HTTP Responses conversion without passthrough"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	cloned, err := common.DeepCopy(request)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}
	preparedInfo := &relaycommon.RelayInfo{
		UserId: info.UserId, TokenId: info.TokenId, OriginModelName: info.OriginModelName,
		RelayFormat: info.RelayFormat, RelayMode: info.RelayMode, Request: cloned,
	}
	preparedInfo.InitChannelMeta(c)
	if err := helper.ModelMappedHelper(c, preparedInfo, cloned); err != nil {
		return nil, newConvertRequestFailedError(c, preparedInfo, err)
	}
	if err := helper.ApplyReasoningModelSuffix(c, preparedInfo, cloned); err != nil {
		return nil, newConvertRequestFailedError(c, preparedInfo, err)
	}
	expanded, err := service.PrepareResponsesReplay(c, preparedInfo, cloned, true)
	if err != nil {
		return nil, newConvertRequestFailedError(c, preparedInfo, err)
	}
	service.MarkResponsesReplayBilled(c, preparedInfo, request, true)
	return expanded, nil
}

// RefundFailedRequestBilling applies the common final-failure policy after all
// eligible attempts have ended. A settled BillingSession never refunds again.
func RefundFailedRequestBilling(c *gin.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError) *types.NewAPIError {
	if apiErr == nil {
		return nil
	}
	apiErr = service.NormalizeViolationFeeError(apiErr)
	if info.Billing != nil {
		info.Billing.Refund(c)
	}
	service.ChargeViolationFeeIfNeeded(c, info, apiErr)
	return apiErr
}
