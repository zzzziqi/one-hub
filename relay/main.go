package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/model"
	"one-api/relay/relay_util"
	"one-api/types"
	"time"

	"github.com/gin-gonic/gin"
)

func Relay(c *gin.Context) {
	relay := Path2Relay(c, c.Request.URL.Path)
	if relay == nil {
		common.AbortWithMessage(c, http.StatusNotFound, "Not Found")
		return
	}

	if err := relay.setRequest(); err != nil {
		common.AbortWithMessage(c, http.StatusBadRequest, err.Error())
		return
	}

	cacheProps := relay.GetChatCache()
	cacheProps.SetHash(relay.getRequest())

	// 获取缓存
	cache := cacheProps.GetCache()

	if cache != nil {
		// 说明有缓存，直接返回缓存内容
		cacheProcessing(c, cache, relay.IsStream())
		return
	}

	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		common.AbortWithMessage(c, http.StatusServiceUnavailable, err.Error())
		return
	}

	apiErr, done := RelayHandler(relay)
	if apiErr == nil {
		// 修改响应体
		modifyResponseBody(c, relay)
		return
	}

	channel := relay.getProvider().GetChannel()
	go processChannelRelayError(c.Request.Context(), channel.Id, channel.Name, apiErr, channel.Type)

	retryTimes := config.RetryTimes
	if done || !shouldRetry(c, apiErr, channel.Type) {
		logger.LogError(c.Request.Context(), fmt.Sprintf("relay error happen, status code is %d, won't retry in this case", apiErr.StatusCode))
		retryTimes = 0
	}

	for i := retryTimes; i > 0; i-- {
		// 冻结通道
		model.ChannelGroup.Cooldowns(channel.Id)
		if err := relay.setProvider(relay.getOriginalModel()); err != nil {
			continue
		}

		channel = relay.getProvider().GetChannel()
		logger.LogError(c.Request.Context(), fmt.Sprintf("using channel #%d(%s) to retry (remain times %d)", channel.Id, channel.Name, i))
		apiErr, done = RelayHandler(relay)
		if apiErr == nil {
			// 修改响应体
			modifyResponseBody(c, relay)
			return
		}
		go processChannelRelayError(c.Request.Context(), channel.Id, channel.Name, apiErr, channel.Type)
		if done || !shouldRetry(c, apiErr, channel.Type) {
			break
		}
	}

	if apiErr != nil {
		if apiErr.StatusCode == http.StatusTooManyRequests {
			apiErr.OpenAIError.Message = "当前分组上游负载已饱和，请稍后再试"
		}
		relayResponseWithErr(c, apiErr)
	}
}

// 修改ResponseBody中的model字段为relay.originalModel
func modifyResponseBody(c *gin.Context, relay RelayBaseInterface) {
	// 获取原始响应体
	responseBody := extractResponseBody(c.Writer)
	if responseBody == nil {
		return
	}

	// 修改"model"字段为 relay.originalModel
	if _, exists := responseBody["model"]; exists {
		responseBody["model"] = relay.getOriginalModel()
	}

	// 将修改后的响应体重新编码并设置为响应
	modifiedResponse, err := json.Marshal(responseBody)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("failed to encode modified response body: %v", err))
		return
	}

	// 重置响应体，并将新的响应体写入
	c.Writer.WriteHeaderNow()
	c.Writer.Write(modifiedResponse)
}

// 提取响应体内容
func extractResponseBody(w gin.ResponseWriter) map[string]interface{} {
	var bodyBytes []byte
	if w, ok := w.(io.Writer); ok {
		buf := new(bytes.Buffer)
		_, err := buf.ReadFrom(w)
		if err == nil {
			bodyBytes = buf.Bytes()
		}
	}

	if len(bodyBytes) == 0 {
		return nil
	}

	var responseBody map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &responseBody); err != nil {
		return nil
	}

	return responseBody
}

func RelayHandler(relay RelayBaseInterface) (err *types.OpenAIErrorWithStatusCode, done bool) {
	promptTokens, tonkeErr := relay.getPromptTokens()
	if tonkeErr != nil {
		err = common.ErrorWrapperLocal(tonkeErr, "token_error", http.StatusBadRequest)
		done = true
		return
	}

	usage := &types.Usage{
		PromptTokens: promptTokens,
	}

	relay.getProvider().SetUsage(usage)

	var quota *relay_util.Quota
	quota, err = relay_util.NewQuota(relay.getContext(), relay.getModelName(), promptTokens)
	if err != nil {
		done = true
		return
	}

	err, done = relay.send()

	if err != nil {
		quota.Undo(relay.getContext())
		return
	}

	quota.Consume(relay.getContext(), usage)
	if usage.CompletionTokens > 0 {
		cacheProps := relay.GetChatCache()
		go cacheProps.StoreCache(relay.getContext().GetInt("channel_id"), usage.PromptTokens, usage.CompletionTokens, relay.getModelName())
	}

	return
}

func cacheProcessing(c *gin.Context, cacheProps *relay_util.ChatCacheProps, isStream bool) {
	responseCache(c, cacheProps.Response, isStream)

	// 写入日志
	tokenName := c.GetString("token_name")

	requestTime := 0
	requestStartTimeValue := c.Request.Context().Value("requestStartTime")
	if requestStartTimeValue != nil {
		requestStartTime, ok := requestStartTimeValue.(time.Time)
		if ok {
			requestTime = int(time.Since(requestStartTime).Milliseconds())
		}
	}

	model.RecordConsumeLog(c.Request.Context(), cacheProps.UserId, cacheProps.ChannelID, cacheProps.PromptTokens, cacheProps.CompletionTokens, cacheProps.ModelName, tokenName, 0, "缓存", requestTime)
}
