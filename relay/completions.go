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

type relayCompletions struct {
	relayBase
	request types.CompletionRequest
}

func NewRelayCompletions(c *gin.Context) *relayCompletions {
	relay := &relayCompletions{}
	relay.c = c
	return relay
}

func (r *relayCompletions) setRequest() error {
	if err := common.UnmarshalBodyReusable(r.c, &r.request); err != nil {
		return err
	}

	if r.request.MaxTokens < 0 || r.request.MaxTokens > math.MaxInt32/2 {
		return errors.New("max_tokens is invalid")
	}

	if !r.request.Stream && r.request.StreamOptions != nil {
		return errors.New("the 'stream_options' parameter is only allowed when 'stream' is enabled")
	}

	r.originalModel = r.request.Model

	return nil
}

func (r *relayCompletions) IsStream() bool {
	return r.request.Stream
}

func (r *relayCompletions) getRequest() interface{} {
	return &r.request
}

func (r *relayCompletions) getPromptTokens() (int, error) {
	return common.CountTokenInput(r.request.Prompt, r.modelName), nil
}

func (r *relayCompletions) send() (err *types.OpenAIErrorWithStatusCode, done bool) {
	provider, ok := r.provider.(providersBase.CompletionInterface)
	if !ok {
		err = common.StringErrorWrapperLocal("channel not implemented", "channel_error", http.StatusServiceUnavailable)
		done = true
		return
	}

	r.request.Model = r.modelName

	if r.request.Stream {
		var response requester.StreamReaderInterface[string]
		response, err = provider.CreateCompletionStream(&r.request)
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
		var response *types.CompletionResponse
		response, err = provider.CreateCompletion(&r.request)
		if err != nil {
			return
		}

		// 修改响应中的模型名称
		response.Model = r.originalModel

		err = responseJsonClient(r.c, response)
		r.cache.SetResponse(response)
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

func (r *relayCompletions) getUsageResponse() string {
	if r.request.StreamOptions != nil && r.request.StreamOptions.IncludeUsage {
		usageResponse := types.CompletionResponse{
			ID:      fmt.Sprintf("chatcmpl-%s", utils.GetUUID()),
			Object:  "chat.completion.chunk",
			Created: utils.GetTimestamp(),
			Model:   r.request.Model,
			Choices: []types.CompletionChoice{},
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
