package relay

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestResponsesReplayBillingIncludesHistoryWithoutChangingFirstChannel(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "replay.db")), &gorm.Config{})
	require.NoError(t, err)
	oldDB := model.DB
	model.DB = db
	t.Cleanup(func() {
		model.DB = oldDB
		conn, err := db.DB()
		if err == nil {
			_ = conn.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.ResponsesReplaySnapshot{}))
	newContext := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		common.SetContextKey(c, constant.ContextKeyOriginalModel, "public-model")
		common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
		common.SetContextKey(c, constant.ContextKeyChannelId, 7)
		common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://upstream.test")
		common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{ResponsesViaChat: true, ResponsesLocalReplay: true})
		c.Set("model_mapping", `{"public-model":"wire-model"}`)
		return c
	}
	first := &dto.OpenAIResponsesRequest{Model: "public-model", Input: json.RawMessage(`"important historical context"`)}
	initial := &relaycommon.RelayInfo{UserId: 11, TokenId: 22, OriginModelName: "public-model", RelayFormat: types.RelayFormatOpenAIResponses, RelayMode: relayconstant.RelayModeResponses, Request: first}
	firstContext := newContext()
	initial.InitChannelMeta(firstContext)
	require.NoError(t, helper.ModelMappedHelper(firstContext, initial, first))
	_, err = service.PrepareResponsesReplay(firstContext, initial, first, true)
	require.NoError(t, err)
	require.NoError(t, service.PersistResponsesReplay(firstContext, initial, &dto.OpenAIResponsesResponse{ID: "resp_history", Status: json.RawMessage(`"completed"`), Output: []dto.ResponsesOutput{{Type: "message", Role: "assistant", Content: []dto.ResponsesOutputContent{{Type: "output_text", Text: "previous answer"}}}}}))
	current := &dto.OpenAIResponsesRequest{Model: "public-model", PreviousResponseID: "resp_history", Input: json.RawMessage(`"continue"`)}
	info := &relaycommon.RelayInfo{UserId: 11, TokenId: 22, OriginModelName: "public-model", RelayFormat: types.RelayFormatOpenAIResponses, RelayMode: relayconstant.RelayModeResponses, Request: current}
	c := newContext()
	expanded, apiErr := prepareResponsesReplayBilling(c, info)
	require.Nil(t, apiErr)
	require.Nil(t, info.ChannelMeta, "first channel selection must remain untouched")
	assert.Equal(t, "public-model", current.Model)
	assert.Equal(t, "resp_history", current.PreviousResponseID)
	assert.Contains(t, expanded.GetTokenCountMeta().CombineText, "important historical context")
	assert.Contains(t, expanded.GetTokenCountMeta().CombineText, "previous answer")
	assert.Contains(t, expanded.GetTokenCountMeta().CombineText, "continue")
	// Actual conversion expands the original once again, never an already expanded copy.
	info.InitChannelMeta(c)
	outbound, err := common.DeepCopy(current)
	require.NoError(t, err)
	require.NoError(t, helper.ModelMappedHelper(c, info, outbound))
	actual, err := service.PrepareResponsesReplay(c, info, outbound, true)
	require.NoError(t, err)
	assert.JSONEq(t, string(expanded.(*dto.OpenAIResponsesRequest).Input), string(actual.Input))
	// A retry cannot switch local history to another routing identity.
	info.ChannelId++
	_, err = service.PrepareResponsesReplay(c, info, outbound, true)
	require.ErrorIs(t, err, model.ErrResponsesReplayNotFound)
}
