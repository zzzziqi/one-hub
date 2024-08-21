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
    headerWritten bool
}

func (w *CustomResponseWriter) Write(b []byte) (int, error) {
    return w.body.Write(b)
}

func (w *CustomResponseWriter) WriteString(s string) (int, error) {
    return w.body.WriteString(s)
}

func (w *CustomResponseWriter) WriteHeader(statusCode int) {
    if !w.headerWritten {
        w.ResponseWriter.WriteHeader(statusCode)
        w.headerWritten = true
    }
}

func Relay(c *gin.Context) {
	relay := Path2Relay(c, c.Request.URL.Path)
	if relay == nil {
		common.AbortWithMessage(c, http.StatusNotFound, "Not Found")
		return
	}

	customWriter := &CustomResponseWriter{ResponseWriter: c.Writer, body: &bytes.Buffer{}}
	c.Writer = customWriter

	if err := relay.setRequest(); err != nil {
		common.AbortWithMessage(c, http.StatusBadRequest, err.Error())
		return
	}

	cacheProps := relay.GetChatCache()
	cacheProps.SetHash(relay.getRequest())

	cache := cacheProps.GetCache()

	if cache != nil {
		cacheProcessing(c, cache, relay.IsStream())
		return
	}

	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		common.AbortWithMessage(c, http.StatusServiceUnavailable, err.Error())
		return
	}

	apiErr, done := RelayHandler(relay, c)
	if apiErr == nil {
		modifiedBody, err := modifyResponseBody(relay, customWriter.body.Bytes())
		if err != nil {
			logger.LogError(c.Request.Context(), err.Error())
			common.AbortWithMessage(c, http.StatusInternalServerError, "Internal Server Error")
			return
		}
		
		c.Writer.Header().Set("Content-Type", "application/json")
		c.Writer.Header().Set("Content-Length", fmt.Sprint(len(modifiedBody)))
		c.Writer.WriteHeader(http.StatusOK)
		_, err = c.Writer.Write(modifiedBody)
		if err != nil {
			logger.LogError(c.Request.Context(), fmt.Sprintf("failed to write modified response body: %v", err))
		}
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

func modifyResponseBody(relay RelayBaseInterface, bodyBytes []byte) ([]byte, error) {
    if len(bodyBytes) == 0 {
        return bodyBytes, nil
    }

    var responseBody map[string]interface{}
    if err := json.Unmarshal(bodyBytes, &responseBody); err != nil {
        return nil, fmt.Errorf("failed to decode response body: %v", err)
    }

    if _, exists := responseBody["model"]; exists {
        responseBody["model"] = relay.getOriginalModel()
    }

    modifiedResponse, err := json.Marshal(responseBody)
    if err != nil {
        return nil, fmt.Errorf("failed to encode modified response body: %v", err)
    }

    return modifiedResponse, nil
}

func cacheProcessing(c *gin.Context, cacheProps *relay_util.ChatCacheProps, isStream bool) {
	responseCache(c, cacheProps.Response, isStream)

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
