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
	"bytes"

	"github.com/gin-gonic/gin"
)

type CustomResponseWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

func (w *CustomResponseWriter) Write(b []byte) (int, error) {
	return w.body.Write(b) // 仅写入缓冲区，避免直接写入响应
}

func (w *CustomResponseWriter) WriteString(s string) (int, error) {
	return w.body.WriteString(s) // 仅写入缓冲区，避免直接写入响应
}

func Relay(c *gin.Context) {
	relay := Path2Relay(c, c.Request.URL.Path)
	if relay == nil {
		common.AbortWithMessage(c, http.StatusNotFound, "Not Found")
		return
	}

	// Use the custom response writer
	customWriter := &CustomResponseWriter{ResponseWriter: c.Writer, body: &bytes.Buffer{}}
	c.Writer = customWriter

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
		// Modify the response body after successful relay handling
		modifyResponseBody(c, relay, customWriter.body.Bytes())
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

func modifyResponseBody(c *gin.Context, relay RelayBaseInterface, bodyBytes []byte) {
    // 如果响应体为空，直接返回
    if len(bodyBytes) == 0 {
        logger.LogError(c.Request.Context(), "response body is empty")
        return
    }

    // 解析响应体为 JSON
    var responseBody map[string]interface{}
    if err := json.Unmarshal(bodyBytes, &responseBody); err != nil {
        logger.LogError(c.Request.Context(), fmt.Sprintf("failed to decode response body: %v", err))
        c.Writer.WriteHeader(http.StatusInternalServerError)
        return
    }

    // 检查是否存在 "model" 字段
    if _, exists := responseBody["model"]; exists {
        logger.LogInfo(c.Request.Context(), fmt.Sprintf("Original model: %s", responseBody["model"]))
        responseBody["model"] = relay.getOriginalModel()
        logger.LogInfo(c.Request.Context(), fmt.Sprintf("Modified model: %s", relay.getOriginalModel()))
    } else {
        logger.LogError(c.Request.Context(), "response body does not contain 'model' field")
        // 如果字段不存在，可以选择是否要创建它
        // responseBody["model"] = relay.getOriginalModel()
    }

    // 重新编码修改后的响应体
    modifiedResponse, err := json.Marshal(responseBody)
    if err != nil {
        logger.LogError(c.Request.Context(), fmt.Sprintf("failed to encode modified response body: %v", err))
        c.Writer.WriteHeader(http.StatusInternalServerError)
        return
    }

    // 覆盖写入新的响应体
    c.Writer.Header().Set("Content-Length", fmt.Sprint(len(modifiedResponse)))
    c.Writer.WriteHeader(http.StatusOK)
    if _, err := c.Writer.Write(modifiedResponse); err != nil {
        logger.LogError(c.Request.Context(), fmt.Sprintf("failed to write modified response body: %v", err))
        return
    }
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
