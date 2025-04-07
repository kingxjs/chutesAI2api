package controller

import (
	"bytes"
	"chutesai2api/chutes-api"
	"chutesai2api/common"
	"chutesai2api/common/config"
	"chutesai2api/common/helper"
	logger "chutesai2api/common/loggger"
	"chutesai2api/model"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/deanxv/CycleTLS/cycletls"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/samber/lo"
	"github.com/samber/lo/mutable"
)

const (
	errNoValidCookies = "No valid cookies available"
	responseIDFormat  = "chatcmpl-%s"
)

// ChatForOpenAI @Summary OpenAI对话接口
// @Description OpenAI对话接口
// @Tags OpenAI
// @Accept json
// @Produce json
// @Param req body model.OpenAIChatCompletionRequest true "OpenAI对话请求"
// @Param Authorization header string true "Authorization API-KEY"
// @Router /v1/chat/completions [post]
func ChatForOpenAI(c *gin.Context) {
	client := cycletls.Init()
	defer safeClose(client)

	var openAIReq model.OpenAIChatCompletionRequest

	if err := c.BindJSON(&openAIReq); err != nil {
		logger.Errorf(c.Request.Context(), err.Error())
		c.JSON(http.StatusInternalServerError, model.OpenAIErrorResponse{
			OpenAIError: model.OpenAIError{
				Message: "Invalid request parameters",
				Type:    "request_error",
				Code:    "500",
			},
		})
		return
	}

	openAIReq = *openAIReq.RemoveEmptyContentMessages()

	if openAIReq.Stream {
		handleStreamRequest(c, client, openAIReq)
	} else {
		handleNonStreamRequest(c, client, openAIReq)
	}
}

func handleNonStreamRequest(c *gin.Context, client cycletls.CycleTLS, openAIReq model.OpenAIChatCompletionRequest) {
	var cookies []string
	cookies = append(cookies, "test")

	modelInfo, ok := common.GetModelInfo(openAIReq.Model)
	if !ok {
		c.JSON(500, gin.H{"error": "no model"})
		return
	}

	responseId := fmt.Sprintf(responseIDFormat, time.Now().Format("20060102150405"))
	ctx := c.Request.Context()

	mutable.Shuffle(cookies)

	maxRetries := len(cookies)
	forbiddenRetryCountMap := make(map[string]int)

	for attempt := 0; attempt < maxRetries; attempt++ {
		cookie := cookies[attempt]
		requestBody, err := createRequestBody(c, &openAIReq, modelInfo, cookie)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		jsonData, err := json.Marshal(requestBody)
		if err != nil {
			c.JSON(500, gin.H{"error": "Failed to marshal request body"})
			return
		}
		sseChan, err := chutes_api.MakeStreamChatRequest(c, client, modelInfo.Id, jsonData, cookie)
		if err != nil {
			logger.Errorf(ctx, "MakeStreamChatRequest err on attempt %d: %v", attempt+1, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		isRateLimit := false
		var assistantMsgContent string
		var delta string
		var shouldContinue bool
		for response := range sseChan {

			if response.Status == 403 {
				// 获取当前 cookie 的重试次数，如果不存在则为 0
				forbiddenRetryCount := forbiddenRetryCountMap[cookie]

				// 最大允许的 403 重试次数
				const maxForbiddenRetries = 5

				// 检查是否已达到最大重试次数
				if forbiddenRetryCount >= maxForbiddenRetries {
					logger.Errorf(ctx, "Reached the maximum number of 403 retries (%d), cookie: %s", maxForbiddenRetries, cookie)
					c.JSON(http.StatusForbidden, gin.H{"error": "403 Forbidden"})
					return
				}

				// 增加重试计数
				forbiddenRetryCount++
				forbiddenRetryCountMap[cookie] = forbiddenRetryCount

				logger.Warnf(ctx, "Received 403 Forbidden, retrying with the same cookie %d/%d: %s",
					forbiddenRetryCount, maxForbiddenRetries, cookie)

				// 使用相同的 cookie 重新发起请求
				newSseChan, newErr := chutes_api.MakeStreamChatRequest(c, client, modelInfo.Id, jsonData, cookie)
				if newErr != nil {
					logger.Errorf(ctx, "403 Retry %d when MakeStreamChatRequest error: %v", forbiddenRetryCount, newErr)
					return
				}

				// 替换当前的 SSE 通道
				sseChan = newSseChan

				// 重置标志位并继续处理新的 SSE 通道
				isRateLimit = false
				break
			}

			if response.Done {
				logger.Warnf(ctx, response.Data)
				return
			}

			data := response.Data
			if data == "" {
				continue
			}

			logger.Debug(ctx, strings.TrimSpace(data))

			switch {
			case common.IsCloudflareChallenge(data):
				c.JSON(http.StatusInternalServerError, gin.H{"error": "cf challenge"})
				return
			case common.IsNotLogin(data):
				isRateLimit = true
				logger.Warnf(ctx, "Cookie Not Login, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
				break
			}

			streamDelta, streamShouldContinue := processNoStreamData(c, data, responseId, openAIReq.Model, jsonData)
			delta = streamDelta
			shouldContinue = streamShouldContinue
			// 处理事件流数据
			if !shouldContinue {
				promptTokens := model.CountTokenText(string(jsonData), openAIReq.Model)
				completionTokens := model.CountTokenText(assistantMsgContent, openAIReq.Model)
				finishReason := "stop"

				c.JSON(http.StatusOK, model.OpenAIChatCompletionResponse{
					ID:      fmt.Sprintf(responseIDFormat, time.Now().Format("20060102150405")),
					Object:  "chat.completion",
					Created: time.Now().Unix(),
					Model:   openAIReq.Model,
					Choices: []model.OpenAIChoice{{
						Message: model.OpenAIMessage{
							Role:    "assistant",
							Content: assistantMsgContent,
						},
						FinishReason: &finishReason,
					}},
					Usage: model.OpenAIUsage{
						PromptTokens:     promptTokens,
						CompletionTokens: completionTokens,
						TotalTokens:      promptTokens + completionTokens,
					},
				})

				return
			} else {
				assistantMsgContent = assistantMsgContent + delta
			}

		}
		if !isRateLimit {
			return
		}

	}
	logger.Errorf(ctx, "All cookies exhausted after %d attempts", maxRetries)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "All cookies are temporarily unavailable."})
	return
}

func createRequestBody(c *gin.Context, openAIReq *model.OpenAIChatCompletionRequest, modelInfo common.ModelInfo, cookie string) (map[string]interface{}, error) {

	client := cycletls.Init()
	defer safeClose(client)

	if config.PRE_MESSAGES_JSON != "" {
		err := openAIReq.PrependMessagesFromJSON(config.PRE_MESSAGES_JSON)
		if err != nil {
			return nil, fmt.Errorf("PrependMessagesFromJSON err: %v JSON:%s", err, config.PRE_MESSAGES_JSON)
		}
	}

	messages := make([]map[string]interface{}, 0, len(openAIReq.Messages))

	for _, msg := range openAIReq.Messages {
		message := map[string]interface{}{
			"role":      msg.Role,
			"content":   msg.Content,
			"id":        uuid.New().String(),
			"createdOn": time.Now().UTC().Format(time.RFC3339Nano),
		}
		messages = append(messages, message)
	}

	requestBody := map[string]interface{}{
		"messages":  messages,
		"model":     modelInfo.Model,
		"chuteName": modelInfo.ChuteName,
	}

	// 创建请求体
	logger.Debug(c.Request.Context(), fmt.Sprintf("RequestBody: %v", requestBody))

	return requestBody, nil
}

// createStreamResponse 创建流式响应
func createStreamResponse(responseId, modelName string, jsonData []byte, delta model.OpenAIDelta, finishReason *string) model.OpenAIChatCompletionResponse {
	promptTokens := model.CountTokenText(string(jsonData), modelName)
	completionTokens := model.CountTokenText(delta.Content, modelName)
	return model.OpenAIChatCompletionResponse{
		ID:      responseId,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   modelName,
		Choices: []model.OpenAIChoice{
			{
				Index:        0,
				Delta:        delta,
				FinishReason: finishReason,
			},
		},
		Usage: model.OpenAIUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}
}

// handleDelta 处理消息字段增量
func handleDelta(c *gin.Context, delta string, responseId, modelName string, jsonData []byte) error {
	// 创建基础响应
	createResponse := func(content string) model.OpenAIChatCompletionResponse {
		return createStreamResponse(
			responseId,
			modelName,
			jsonData,
			model.OpenAIDelta{Content: content, Role: "assistant"},
			nil,
		)
	}

	// 发送基础事件
	var err error
	if err = sendSSEvent(c, createResponse(delta)); err != nil {
		return err
	}

	return err
}

// handleMessageResult 处理消息结果
func handleMessageResult(c *gin.Context, responseId, modelName string, jsonData []byte) bool {
	finishReason := "stop"
	var delta string

	streamResp := createStreamResponse(responseId, modelName, jsonData, model.OpenAIDelta{Content: delta, Role: "assistant"}, &finishReason)
	if err := sendSSEvent(c, streamResp); err != nil {
		logger.Warnf(c.Request.Context(), "sendSSEvent err: %v", err)
		return false
	}
	c.SSEvent("", " [DONE]")
	return false
}

// sendSSEvent 发送SSE事件
func sendSSEvent(c *gin.Context, response model.OpenAIChatCompletionResponse) error {
	jsonResp, err := json.Marshal(response)
	if err != nil {
		logger.Errorf(c.Request.Context(), "Failed to marshal response: %v", err)
		return err
	}
	c.SSEvent("", " "+string(jsonResp))
	c.Writer.Flush()
	return nil
}

func handleStreamRequest(c *gin.Context, client cycletls.CycleTLS, openAIReq model.OpenAIChatCompletionRequest) {

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	var cookies []string
	cookies = append(cookies, "test")

	modelInfo, ok := common.GetModelInfo(openAIReq.Model)
	if !ok {
		c.JSON(500, gin.H{"error": "no model"})
		return
	}

	responseId := fmt.Sprintf(responseIDFormat, time.Now().Format("20060102150405"))
	ctx := c.Request.Context()

	mutable.Shuffle(cookies)

	maxRetries := len(cookies)
	forbiddenRetryCountMap := make(map[string]int)

	c.Stream(func(w io.Writer) bool {
		for attempt := 0; attempt < maxRetries; attempt++ {
			cookie := cookies[attempt]

			requestBody, err := createRequestBody(c, &openAIReq, modelInfo, cookie)
			if err != nil {
				c.JSON(500, gin.H{"error": err.Error()})
				return false
			}

			jsonData, err := json.Marshal(requestBody)
			if err != nil {
				c.JSON(500, gin.H{"error": "Failed to marshal request body"})
				return false
			}
			sseChan, err := chutes_api.MakeStreamChatRequest(c, client, modelInfo.Id, jsonData, cookie)
			if err != nil {
				logger.Errorf(ctx, "MakeStreamChatRequest err on attempt %d: %v", attempt+1, err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return false
			}

			isRateLimit := false
		SSELoop:
			for response := range sseChan {

				if response.Status == 429 {
					isRateLimit = true
					logger.Warnf(ctx, "rate limit, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
					break SSELoop
				}

				if response.Status == 403 {
					// 获取当前 cookie 的重试次数，如果不存在则为 0
					forbiddenRetryCount := forbiddenRetryCountMap[cookie]

					// 最大允许的 403 重试次数
					const maxForbiddenRetries = 5

					// 检查是否已达到最大重试次数
					if forbiddenRetryCount >= maxForbiddenRetries {
						logger.Errorf(ctx, "Reached the maximum number of 403 retries (%d), cookie: %s", maxForbiddenRetries, cookie)
						c.JSON(http.StatusForbidden, gin.H{"error": "403 Forbidden"})
						return false
					}

					// 增加重试计数
					forbiddenRetryCount++
					forbiddenRetryCountMap[cookie] = forbiddenRetryCount

					logger.Warnf(ctx, "Received 403 Forbidden, retrying with the same cookie %d/%d: %s",
						forbiddenRetryCount, maxForbiddenRetries, cookie)

					// 使用相同的 cookie 重新发起请求
					newSseChan, newErr := chutes_api.MakeStreamChatRequest(c, client, modelInfo.Id, jsonData, cookie)
					if newErr != nil {
						logger.Errorf(ctx, "403 Retry %d when MakeStreamChatRequest error: %v", forbiddenRetryCount, newErr)
						return false
					}

					// 替换当前的 SSE 通道
					sseChan = newSseChan

					// 重置标志位并继续处理新的 SSE 通道
					isRateLimit = false
					break SSELoop
				}
				if response.Done {
					logger.Warnf(ctx, response.Data)
					return false
				}

				data := response.Data
				if data == "" {
					continue
				}

				logger.Debug(ctx, strings.TrimSpace(data))

				switch {
				case common.IsCloudflareChallenge(data):
					c.JSON(http.StatusInternalServerError, gin.H{"error": "cf challenge"})
					return false
				case common.IsNotLogin(data):
					isRateLimit = true
					logger.Warnf(ctx, "Cookie Not Login, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
					break SSELoop // 使用 label 跳出 SSE 循环
				}

				_, shouldContinue := processStreamData(c, data, responseId, openAIReq.Model, jsonData)
				// 处理事件流数据

				if !shouldContinue {
					return false
				}
			}

			if !isRateLimit {
				return true
			}

		}

		logger.Errorf(ctx, "All cookies exhausted after %d attempts", maxRetries)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "All cookies are temporarily unavailable."})
		return false
	})
}

// 处理流式数据的辅助函数，返回bool表示是否继续处理
func processStreamData(c *gin.Context, data string, responseId, model string, jsonData []byte) (string, bool) {
	data = strings.TrimSpace(data)
	data = strings.TrimPrefix(data, "data: ")

	if data == "[DONE]" {
		handleMessageResult(c, responseId, model, jsonData)
		return "", false
	}

	var event map[string]interface{}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		logger.Errorf(c.Request.Context(), "Failed to unmarshal event: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return "", false
	}

	choices, ok := event["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return "", false
	}

	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return "", false
	}

	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return "", false
	}

	content, ok := delta["content"].(string)
	if !ok {
		if delta["content"] == nil {
			return "", true
		}
		return "", false
	}

	if err := handleDelta(c, content, responseId, model, jsonData); err != nil {
		logger.Errorf(c.Request.Context(), "handleDelta err: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return "", false
	}
	return content, true

}

func processNoStreamData(c *gin.Context, data string, responseId, model string, jsonData []byte) (string, bool) {
	data = strings.TrimSpace(data)
	data = strings.TrimPrefix(data, "data: ")

	if data == "[DONE]" {
		return "", false
	}

	var event map[string]interface{}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		logger.Errorf(c.Request.Context(), "Failed to unmarshal event: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return "", false
	}

	choices, ok := event["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return "", false
	}

	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return "", false
	}

	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return "", false
	}

	content, ok := delta["content"].(string)
	if !ok {
		if delta["content"] == nil {
			return "", true
		}
		return "", false
	}

	return content, true

}

func ImagesForOpenAI(c *gin.Context) {

	client := cycletls.Init()
	defer safeClose(client)

	var openAIReq model.OpenAIImagesGenerationRequest
	if err := c.BindJSON(&openAIReq); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	resp, err := ImageProcess(c, client, openAIReq)
	if err != nil {
		logger.Errorf(c.Request.Context(), fmt.Sprintf("ImageProcess err  %v\n", err))
		c.JSON(http.StatusInternalServerError, model.OpenAIErrorResponse{
			OpenAIError: model.OpenAIError{
				Message: err.Error(),
				Type:    "request_error",
				Code:    "500",
			},
		})
		return
	} else {
		c.JSON(200, resp)
		return
	}
}

func ImageProcess(c *gin.Context, client cycletls.CycleTLS, openAIReq model.OpenAIImagesGenerationRequest) (*model.OpenAIImagesGenerationResponse, error) {

	var cookies []string
	cookies = append(cookies, "test")

	modelInfo, ok := common.GetImageModelInfo(openAIReq.Model)
	if !ok {
		c.JSON(500, gin.H{"error": "no model"})
		return nil, fmt.Errorf("no model")
	}

	ctx := c.Request.Context()

	mutable.Shuffle(cookies)

	maxRetries := len(cookies)

	for attempt := 0; attempt < maxRetries; attempt++ {
		cookie := cookies[attempt]
		requestBody, err := createImageRequestBody(c, &openAIReq)
		if err != nil {
			logger.Errorf(ctx, "Failed to create request body: %v", err)
			return nil, err
		}

		response, err := chutes_api.MakeImageRequest(c, client, requestBody, modelInfo.Id)
		if err != nil {
			logger.Errorf(ctx, "Failed to make image request: %v", err)
			return nil, err
		}

		body := response.Body

		switch {
		case common.IsRateLimit(body):
			logger.Warnf(ctx, "Cookie rate limited, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
			continue
		case common.IsNotLogin(body):
			logger.Warnf(ctx, "Cookie Not Login, switching to next cookie, attempt %d/%d, COOKIE:%s", attempt+1, maxRetries, cookie)
			continue
		}

		decodedBytes, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			logger.Errorf(ctx, "Failed to decode base64: %v body: %s", err, body)
			return nil, err
		}

		decodedStr := string(decodedBytes)

		var data map[string]interface{}
		err = json.Unmarshal([]byte(decodedStr), &data)
		if err != nil {
			logger.Errorf(ctx, "Failed to unmarshal response: %v", err)
			return nil, err
		}

		// 提取字段
		b64 := data["image_b64"].(string)

		// 获取";base64,"后的Base64编码部分
		dataParts := strings.Split(b64, ";base64,")
		if len(dataParts) != 2 {
			logger.Errorf(ctx, "Invalid base64 string: %s", b64)
			return nil, fmt.Errorf("invalid base64 string")
		}
		b64 = dataParts[1]

		result := &model.OpenAIImagesGenerationResponse{
			Created: time.Now().Unix(),
			Data:    make([]*model.OpenAIImagesGenerationDataResponse, 0, 1),
		}
		URL, err := UploadToXinyew(c, b64)
		if err != nil {
			logger.Errorf(ctx, "UploadToXinyew: %v", err)
		}
		// Process image URLs
		dataResp := &model.OpenAIImagesGenerationDataResponse{
			B64Json: b64,
			URL:     URL,
		}
		result.Data = append(result.Data, dataResp)
		return result, nil
	}

	logger.Errorf(ctx, "All cookies exhausted after %d attempts", maxRetries)
	return nil, fmt.Errorf("all cookies are temporarily unavailable")
}

func createImageRequestBody(c *gin.Context, openAIReq *model.OpenAIImagesGenerationRequest) (chutes_api.MakeImageReq, error) {
	width := "1024"
	height := "1024"
	if openAIReq.Size != "" {
		// size 传值格式 1024x1024
		if strings.Contains(openAIReq.Size, "x") {
			parts := strings.Split(openAIReq.Size, "x")
			if len(parts) == 2 {
				width = parts[0]
				height = parts[1]
			}
		}
	}
	makeImageReq := chutes_api.MakeImageReq{
		Prompt: openAIReq.Prompt,
		Width:  width,
		Height: height,
		Seed:   openAIReq.Seed,
	}
	logger.Debug(c.Request.Context(), fmt.Sprintf("RequestBody: %v", makeImageReq))
	logger.Debug(c.Request.Context(), fmt.Sprintf("openAIReq: %v", openAIReq))
	return makeImageReq, nil

}

// OpenaiModels @Summary OpenAI模型列表接口
// @Description OpenAI模型列表接口
// @Tags OpenAI
// @Accept json
// @Produce json
// @Param Authorization header string true "Authorization API-KEY"
// @Success 200 {object} common.ResponseResult{data=model.OpenaiModelListResponse} "成功"
// @Router /v1/models [get]
func OpenaiModels(c *gin.Context) {
	var modelsResp []string

	modelsResp = lo.Union(common.GetModelList(), common.GetImageModelList())

	var openaiModelListResponse model.OpenaiModelListResponse
	var openaiModelResponse []model.OpenaiModelResponse
	openaiModelListResponse.Object = "list"

	for _, modelResp := range modelsResp {
		openaiModelResponse = append(openaiModelResponse, model.OpenaiModelResponse{
			ID:     modelResp,
			Object: "model",
		})
	}
	openaiModelListResponse.Data = openaiModelResponse
	c.JSON(http.StatusOK, openaiModelListResponse)
	return
}

func safeClose(client cycletls.CycleTLS) {
	if client.ReqChan != nil {
		close(client.ReqChan)
	}
	if client.RespChan != nil {
		close(client.RespChan)
	}
}

// UploadToXinyew 上传图片到新野图床并返回URL
func UploadToXinyew(c *gin.Context, imageBase64 string) (string, error) {
	// 生成随机文件名
	filename := helper.GenRequestID()

	logger.Debug(c.Request.Context(), fmt.Sprintf("Base64 data length: %d\n", len(imageBase64)))
	logger.Debug(c.Request.Context(), fmt.Sprintf("Generated filename: %s\n", filename))

	// 解码base64图片数据
	var imageData []byte
	var err error

	if strings.Contains(imageBase64, ",") {
		parts := strings.SplitN(imageBase64, ",", 2)
		imageData, err = base64.StdEncoding.DecodeString(parts[1])
	} else {
		imageData, err = base64.StdEncoding.DecodeString(imageBase64)
	}

	if err != nil {
		logger.Errorf(c.Request.Context(), fmt.Sprintf("Error decoding base64: %v\n", err))
		return "", err
	}

	// 创建临时文件
	tempFilePath := filepath.Join(os.TempDir(), filename)
	err = ioutil.WriteFile(tempFilePath, imageData, 0644)
	if err != nil {
		logger.Errorf(c.Request.Context(), fmt.Sprintf("Error creating temp file: %v\n", err))
		return "", err
	}

	// 确保临时文件会被删除
	defer func() {
		err := os.Remove(tempFilePath)
		if err != nil {
			logger.Errorf(c.Request.Context(), fmt.Sprintf("Error removing temp file: %v\n", err))
		}
	}()

	// 准备文件上传
	var requestBody bytes.Buffer
	multipartWriter := multipart.NewWriter(&requestBody)

	fileWriter, err := multipartWriter.CreateFormFile("file", filename)
	if err != nil {
		logger.Errorf(c.Request.Context(), fmt.Sprintf("Error creating form file: %v\n", err))
		return "", err
	}

	// 打开临时文件
	fileHandle, err := os.Open(tempFilePath)
	if err != nil {
		logger.Errorf(c.Request.Context(), fmt.Sprintf("Error opening temp file: %v\n", err))
		return "", err
	}
	defer fileHandle.Close()

	// 复制文件内容到请求体
	_, err = io.Copy(fileWriter, fileHandle)
	if err != nil {
		logger.Errorf(c.Request.Context(), fmt.Sprintf("Error copying file to request: %v\n", err))
		return "", err
	}

	// 关闭multipart writer
	err = multipartWriter.Close()
	if err != nil {
		logger.Errorf(c.Request.Context(), fmt.Sprintf("Error closing multipart writer: %v\n", err))
		return "", err
	}

	// 创建HTTP请求
	logger.Debug(c.Request.Context(), fmt.Sprintf("Sending request to xinyew.cn..."))
	req, err := http.NewRequest("POST", "https://api.xinyew.cn/api/jdtc", &requestBody)
	if err != nil {
		logger.Errorf(c.Request.Context(), fmt.Sprintf("Error creating request: %v\n", err))
		return "", err
	}

	// 设置Content-Type
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())

	// 发送请求
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		logger.Errorf(c.Request.Context(), fmt.Sprintf("Error sending request: %v\n", err))
		return "", err
	}
	defer resp.Body.Close()

	// 读取响应内容
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logger.Errorf(c.Request.Context(), fmt.Sprintf("Error reading response body: %v\n", err))
		return "", err
	}

	if resp.StatusCode == http.StatusOK {
		var result model.XinyewResponse
		err = json.Unmarshal(respBody, &result)
		if err != nil {
			logger.Errorf(c.Request.Context(), fmt.Sprintf("Error parsing JSON response: %v\n", err))
			logger.Errorf(c.Request.Context(), fmt.Sprintf("Response content: %s\n", string(respBody)))
			return "", err
		}

		logger.Debug(c.Request.Context(), fmt.Sprintf("Upload response: %+v\n", result))

		if result.Errno == 0 {
			url := result.Data.URL
			if url != "" {
				logger.Debug(c.Request.Context(), fmt.Sprintf("Successfully got image URL: %s\n", url))
				return url, nil
			}
		}
	} else {
		logger.Debug(c.Request.Context(), fmt.Sprintf("Upload failed with status %d\n", resp.StatusCode))
		logger.Debug(c.Request.Context(), fmt.Sprintf("Response content: %s\n", string(respBody)))
	}

	return "", fmt.Errorf("failed to upload image")
}
