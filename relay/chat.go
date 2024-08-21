package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"one-api/common"
	"one-api/common/requester"
	"one-api/common/utils"
	providersBase "one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

type relayChat struct {
	relayBase
	chatRequest types.ChatCompletionRequest
}

func NewRelayChat(c *gin.Context) *relayChat {
	relay := &relayChat{}
	relay.c = c
	return relay
}

func (r *relayChat) setRequest() error {
	if err := common.UnmarshalBodyReusable(r.c, &r.chatRequest); err != nil {
		return err
	}

	if r.chatRequest.MaxTokens < 0 || r.chatRequest.MaxTokens > math.MaxInt32/2 {
		return errors.New("max_tokens is invalid")
	}

	if r.chatRequest.Tools != nil {
		r.c.Set("skip_only_chat", true)
	}

	if !r.chatRequest.Stream && r.chatRequest.StreamOptions != nil {
		return errors.New("the 'stream_options' parameter is only allowed when 'stream' is enabled")
	}

	r.originalModel = r.chatRequest.Model

	return nil
}

func (r *relayChat) getRequest() interface{} {
	return &r.chatRequest
}

func (r *relayChat) IsStream() bool {
	return r.chatRequest.Stream
}

func (r *relayChat) getPromptTokens() (int, error) {
	channel := r.provider.GetChannel()
	return common.CountTokenMessages(r.chatRequest.Messages, r.modelName, channel.PreCost), nil
}

func (r *relayChat) send() (err *types.OpenAIErrorWithStatusCode, done bool) {
	chatProvider, ok := r.provider.(providersBase.ChatInterface)
	if !ok {
		err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
		done = true
		return
	}

	r.chatRequest.Model = r.modelName

	if r.chatRequest.Stream {
		var response requester.StreamReaderInterface[string]
		response, err = chatProvider.CreateChatCompletionStream(&r.chatRequest)
		if err != nil {
			return
		}

		doneStr := func() string {
			return r.getUsageResponse()
		}

		// 包装 StreamReaderInterface 以修改模型名称
		wrappedResponse := &modelNameWrapper{
			StreamReaderInterface: response,
			originalModel:         r.originalModel,
		}

		err = responseStreamClient(r.c, wrappedResponse, r.cache, doneStr)
	} else {
		var response *types.ChatCompletionResponse
		response, err = chatProvider.CreateChatCompletion(&r.chatRequest)
		if err != nil {
			return
		}

		// 修改响应中的模型名称
		response.Model = r.originalModel

		err = responseJsonClient(r.c, response)

		if err == nil && response.GetContent() != "" {
			r.cache.SetResponse(response)
		}
	}

	if err != nil {
		done = true
	}

	return
}

// modelNameWrapper 包装 StreamReaderInterface 以修改流式响应中的模型名称
type modelNameWrapper struct {
	requester.StreamReaderInterface[string]
	originalModel string
}

func (w *modelNameWrapper) Recv() (<-chan string, <-chan error) {
	dataChan, errChan := w.StreamReaderInterface.Recv()
	wrappedDataChan := make(chan string)

	go func() {
		defer close(wrappedDataChan)
		for data := range dataChan {
			// 解析 JSON
			var jsonData map[string]interface{}
			if err := json.Unmarshal([]byte(data), &jsonData); err == nil {
				// 修改模型名称
				if _, ok := jsonData["model"]; ok {
					jsonData["model"] = w.originalModel
					// 重新编码为 JSON
					if modifiedData, err := json.Marshal(jsonData); err == nil {
						data = string(modifiedData)
					}
				}
			}
			wrappedDataChan <- data
		}
	}()

	return wrappedDataChan, errChan
}

func (r *relayChat) getUsageResponse() string {
	if r.chatRequest.StreamOptions != nil && r.chatRequest.StreamOptions.IncludeUsage {
		usageResponse := types.ChatCompletionStreamResponse{
			ID:      fmt.Sprintf("chatcmpl-%s", utils.GetUUID()),
			Object:  "chat.completion.chunk",
			Created: utils.GetTimestamp(),
			Model:   r.originalModel,
			Choices: []types.ChatCompletionStreamChoice{},
			Usage:   r.provider.GetUsage(),
		}

		responseBody, err := json.Marshal(usageResponse)
		if err != nil {
			return ""
		}

		return string(responseBody)
	}

	return ""
}
