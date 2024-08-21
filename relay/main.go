package relay

import (
	"encoding/json"
	"fmt"
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
		// 说明有缓存， 直接返回缓存内容
		cacheProcessing(c, cache, relay.IsStream())
		return
	}

	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		common.AbortWithMessage(c, http.StatusServiceUnavailable, err.Error())
		return
	}

	apiErr, done := RelayHandler(relay, c)
	if apiErr == nil {
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
		apiErr, done = RelayHandler(relay, c)
		if apiErr == nil {
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

func RelayHandler(relay RelayBaseInterface, c *gin.Context) (err *types.OpenAIErrorWithStatusCode, done bool) {
	promptTokens, tokenErr := relay.getPromptTokens()
	if tokenErr != nil {
		err = common.ErrorWrapperLocal(tokenErr, "token_error", http.StatusBadRequest)
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

	if relay.IsStream() {
		err = handleStreamResponse(relay, c)
	} else {
		err = handleNonStreamResponse(relay, c)
	}

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

func handleStreamResponse(relay RelayBaseInterface, c *gin.Context) *types.OpenAIErrorWithStatusCode {
	responseStream, err := relay.sendStream()
	if err != nil {
		return err
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	c.Stream(func(w io.Writer) bool {
		for chunk := range responseStream {
			// 修改 "model" 字段
			if chunk.Model != "" {
				chunk.Model = relay.getOriginalModel()
			}
			
			encodedChunk, err := json.Marshal(chunk)
			if err != nil {
				logger.LogError(c.Request.Context(), fmt.Sprintf("failed to encode chunk: %v", err))
				return false
			}
			
			c.SSEvent("", string(encodedChunk))
			return true
		}
		return false
	})

	return nil
}

func handleNonStreamResponse(relay RelayBaseInterface, c *gin.Context) *types.OpenAIErrorWithStatusCode {
	response, err := relay.send()
	if err != nil {
		return err
	}

	// 修改 "model" 字段
	if response.Model != "" {
		response.Model = relay.getOriginalModel()
	}

	// 编码并发送修改后的响应
	c.JSON(http.StatusOK, response)

	return nil
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
