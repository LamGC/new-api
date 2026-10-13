package service

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestResponsesReplay(t *testing.T) {
	databases := []struct{ name, dsn string }{{"sqlite", "file:responses-replay?mode=memory&cache=shared"}, {"mysql", os.Getenv("REPLAY_TEST_MYSQL_DSN")}, {"postgres", os.Getenv("REPLAY_TEST_POSTGRES_DSN")}}
	for _, database := range databases {
		t.Run(database.name, func(t *testing.T) {
			if database.dsn == "" {
				t.Skip("real database DSN not available")
			}
			var dialector gorm.Dialector
			switch database.name {
			case "mysql":
				dialector = mysql.Open(database.dsn)
			case "postgres":
				dialector = postgres.Open(database.dsn)
			default:
				dialector = sqlite.Open(database.dsn)
			}
			db, err := gorm.Open(dialector, &gorm.Config{})
			require.NoError(t, err)
			var version string
			versionQuery := "SELECT version()"
			if database.name == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("Database version: %s", version)
			// Only explicit dedicated test databases are accepted by these env vars.
			original := model.DB
			model.DB = db
			t.Cleanup(func() {
				model.DB = original
				sqlDB, err := db.DB()
				if err == nil {
					_ = sqlDB.Close()
				}
			})
			require.NoError(t, db.Migrator().DropTable(&model.ResponsesReplaySnapshot{}))
			require.NoError(t, db.AutoMigrate(&model.ResponsesReplaySnapshot{}))
			info := &relaycommon.RelayInfo{UserId: 11, TokenId: 22, OriginModelName: "public-model", ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 33, ChannelType: 1, ChannelBaseUrl: "https://example.test", UpstreamModelName: "upstream-model"}}
			makeContext := func() *gin.Context {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				return c
			}
			c := makeContext()
			request := &dto.OpenAIResponsesRequest{Input: json.RawMessage(`[{"role":"system","content":"keep explicit system"},{"role":"user","content":"hello"}]`), Instructions: json.RawMessage(`"do not inherit"`), Tools: json.RawMessage(`[{"type":"function","name":"original.tool","parameters":{"type":"object"}}]`)}
			prepared, err := PrepareResponsesReplay(c, info, request, true)
			require.NoError(t, err)
			assert.Equal(t, request.Instructions, prepared.Instructions)
			response := &dto.OpenAIResponsesResponse{ID: "resp_saved", Status: json.RawMessage(`"completed"`), Output: []dto.ResponsesOutput{{Type: "function_call", CallId: "call_1", Name: "original.tool", Arguments: json.RawMessage(`"{}"`)}}}
			require.NoError(t, PersistResponsesReplay(c, info, response))
			assert.True(t, response.Store)

			c = makeContext()
			credentialRequest := &dto.OpenAIResponsesRequest{Input: json.RawMessage(`"safe input"`), Tools: json.RawMessage(`[{"type":"mcp","server_label":"test","authorization":"never-store-this-secret"}]`)}
			_, err = PrepareResponsesReplay(c, info, credentialRequest, true)
			require.NoError(t, err)
			require.NoError(t, PersistResponsesReplay(c, info, &dto.OpenAIResponsesResponse{ID: "resp_credentials", Status: json.RawMessage(`"completed"`)}))
			var saved map[string]any
			require.NoError(t, db.Table("responses_replay_snapshots").Where("response_id = ?", "resp_credentials").Take(&saved).Error)
			savedJSON, err := common.Marshal(saved)
			require.NoError(t, err)
			assert.NotContains(t, string(savedJSON), "never-store-this-secret")
			assert.NotContains(t, string(savedJSON), "authorization")
			assert.NotContains(t, saved, "tools")

			c = makeContext()
			declarationInput := json.RawMessage(`[{"type":"additional_tools","tools":[{"type":"mcp","authorization":"inline-secret"}]},{"role":"user","content":"hello"}]`)
			declarationRequest, err := PrepareResponsesReplay(c, info, &dto.OpenAIResponsesRequest{Input: declarationInput}, true)
			require.NoError(t, err)
			assert.JSONEq(t, string(declarationInput), string(declarationRequest.Input))
			require.NoError(t, PersistResponsesReplay(c, info, &dto.OpenAIResponsesResponse{ID: "resp_inline_tools", Status: json.RawMessage(`"completed"`)}))
			savedInline, err := model.FindResponsesReplay(context.Background(), "resp_inline_tools", info.UserId, info.TokenId, responsesReplayScope(info), time.Now().Unix())
			require.NoError(t, err)
			assert.NotContains(t, string(savedInline.Input), "inline-secret")
			assert.JSONEq(t, `[{"role":"user","content":"hello"}]`, string(savedInline.Input))
			for _, input := range []string{
				`[{"type":"unsupported_configuration","authorization":"secret"}]`,
				`[{"type":"message","role":"user","content":"safe","authorization":"secret"}]`,
			} {
				_, err := PrepareResponsesReplay(makeContext(), info, &dto.OpenAIResponsesRequest{Input: json.RawMessage(input)}, true)
				require.Error(t, err)
				assert.True(t, IsResponsesReplayClientError(err))
			}
			_, err = PrepareResponsesReplay(makeContext(), info, &dto.OpenAIResponsesRequest{Input: json.RawMessage(`[{"type":"function_call_output","call_id":"call_business","output":{"headers":"business data","token":"business token"}}]`)}, true)
			require.NoError(t, err)
			// Repeated migration preserves the immutable row and indexes.
			require.NoError(t, db.AutoMigrate(&model.ResponsesReplaySnapshot{}))
			require.NoError(t, db.AutoMigrate(&model.ResponsesReplaySnapshot{}))
			next, err := PrepareResponsesReplay(makeContext(), info, &dto.OpenAIResponsesRequest{PreviousResponseID: "resp_saved", Input: json.RawMessage(`[{"type":"function_call_output","call_id":"call_1","output":"ok"}]`)}, true)
			require.NoError(t, err)
			assert.Empty(t, next.PreviousResponseID)
			assert.Empty(t, next.Instructions)
			assert.Empty(t, next.Tools)
			var items []map[string]any
			require.NoError(t, common.Unmarshal(next.Input, &items))
			require.Len(t, items, 4)
			assert.Equal(t, "system", items[0]["role"])
			assert.Equal(t, "original.tool", items[2]["name"])
			for _, change := range []string{"missing", "user", "token", "route"} {
				t.Run(change, func(t *testing.T) {
					scoped := *info
					meta := *info.ChannelMeta
					scoped.ChannelMeta = &meta
					id := "resp_saved"
					switch change {
					case "missing":
						id = "resp_unknown"
					case "user":
						scoped.UserId++
					case "token":
						scoped.TokenId++
					case "route":
						scoped.ChannelBaseUrl = "https://different.test"
					}
					_, err := PrepareResponsesReplay(makeContext(), &scoped, &dto.OpenAIResponsesRequest{PreviousResponseID: id, Input: json.RawMessage(`"next"`)}, true)
					assert.ErrorIs(t, err, model.ErrResponsesReplayNotFound)
				})
			}
			for _, status := range []string{"completed", "incomplete", "failed", "cancelled"} {
				c := makeContext()
				_, err := PrepareResponsesReplay(c, info, &dto.OpenAIResponsesRequest{Input: json.RawMessage(`"private"`), Store: json.RawMessage(`false`)}, true)
				require.NoError(t, err)
				r := &dto.OpenAIResponsesResponse{ID: "resp_no_store_" + status, Status: json.RawMessage(`"` + status + `"`)}
				require.NoError(t, PersistResponsesReplay(c, info, r))
				assert.False(t, r.Store)
				_, err = model.FindResponsesReplay(context.Background(), r.ID, info.UserId, info.TokenId, responsesReplayScope(info), time.Now().Unix())
				assert.ErrorIs(t, err, model.ErrResponsesReplayNotFound)
			}

			for _, status := range []string{"incomplete", "failed", "cancelled"} {
				c := makeContext()
				_, err := PrepareResponsesReplay(c, info, &dto.OpenAIResponsesRequest{Input: json.RawMessage(`"partial"`)}, true)
				require.NoError(t, err)
				r := &dto.OpenAIResponsesResponse{ID: "resp_partial_" + status, Status: json.RawMessage(`"` + status + `"`)}
				require.NoError(t, PersistResponsesReplay(c, info, r))
				assert.False(t, r.Store)
			}
			largeInput, err := common.Marshal(strings.Repeat("x", 70*1024))
			require.NoError(t, err)
			c = makeContext()
			_, err = PrepareResponsesReplay(c, info, &dto.OpenAIResponsesRequest{Input: largeInput}, true)
			require.NoError(t, err)
			require.NoError(t, PersistResponsesReplay(c, info, &dto.OpenAIResponsesResponse{ID: "resp_large", Status: json.RawMessage(`"completed"`)}))
			large, err := PrepareResponsesReplay(makeContext(), info, &dto.OpenAIResponsesRequest{PreviousResponseID: "resp_large", Input: json.RawMessage(`"next"`)}, true)
			require.NoError(t, err)
			assert.Greater(t, len(large.Input), 65536)
			require.NoError(t, db.Model(&model.ResponsesReplaySnapshot{}).Where("response_id = ?", "resp_large").Update("depth", 32).Error)
			_, err = PrepareResponsesReplay(makeContext(), info, &dto.OpenAIResponsesRequest{PreviousResponseID: "resp_large"}, true)
			require.Error(t, err)
			assert.True(t, IsResponsesReplayClientError(err))
			overLimit, _ := common.Marshal(strings.Repeat("x", 1024*1024))
			_, err = PrepareResponsesReplay(makeContext(), info, &dto.OpenAIResponsesRequest{Input: overLimit}, true)
			require.Error(t, err)
			assert.True(t, IsResponsesReplayClientError(err))
			c = makeContext()
			MarkResponsesReplayBilled(c, info, &dto.OpenAIResponsesRequest{PreviousResponseID: "resp_saved"}, false)
			_, err = PrepareResponsesReplay(c, info, &dto.OpenAIResponsesRequest{PreviousResponseID: "resp_saved"}, true)
			assert.ErrorIs(t, err, model.ErrResponsesReplayNotFound)
			c = makeContext()
			_, err = PrepareResponsesReplay(c, info, &dto.OpenAIResponsesRequest{Input: json.RawMessage(`"cancel"`)}, true)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(c.Request.Context())
			cancel()
			c.Request = c.Request.WithContext(ctx)
			require.ErrorIs(t, PersistResponsesReplay(c, info, &dto.OpenAIResponsesResponse{ID: "resp_cancel", Status: json.RawMessage(`"completed"`)}), context.Canceled)
			require.NoError(t, db.Model(&model.ResponsesReplaySnapshot{}).Where("response_id = ?", "resp_saved").Update("expires_at", time.Now().Unix()-1).Error)
			_, err = PrepareResponsesReplay(makeContext(), info, &dto.OpenAIResponsesRequest{PreviousResponseID: "resp_saved"}, true)
			assert.ErrorIs(t, err, model.ErrResponsesReplayNotFound)
			c = makeContext()
			_, err = PrepareResponsesReplay(c, info, &dto.OpenAIResponsesRequest{Input: json.RawMessage(`"purge"`)}, true)
			require.NoError(t, err)
			require.NoError(t, PersistResponsesReplay(c, info, &dto.OpenAIResponsesResponse{ID: "resp_purge", Status: json.RawMessage(`"completed"`)}))
			var expiredCount int64
			require.NoError(t, db.Model(&model.ResponsesReplaySnapshot{}).Where("response_id = ?", "resp_saved").Count(&expiredCount).Error)
			assert.Zero(t, expiredCount)
			require.NoError(t, db.Migrator().DropTable(&model.ResponsesReplaySnapshot{}))
			_, err = PrepareResponsesReplay(makeContext(), info, &dto.OpenAIResponsesRequest{PreviousResponseID: "resp_saved"}, true)
			require.Error(t, err)
			assert.NotErrorIs(t, err, model.ErrResponsesReplayNotFound)
		})
	}
}
