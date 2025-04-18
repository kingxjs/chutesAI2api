package common

import (
	"bytes"
	"chutesai2api/common/config"
	logger "chutesai2api/common/loggger"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"time"
)

type TaskRequest struct {
	ClientKey string `json:"clientKey"`
	Type      string `json:"type"`
	URL       string `json:"url"`
	UserAgent string `json:"userAgent"`
	Proxy     string `json:"proxy,omitempty"`
}

type TaskResponse struct {
	TaskID string `json:"taskId"`
}

type TaskResultRequest struct {
	ClientKey string `json:"clientKey"`
	TaskID    string `json:"taskId"`
}

type TaskResultResponse struct {
	Status string      `json:"status"`
	Result interface{} `json:"result,omitempty"`
}

// 使用互斥锁保护全局变量
var (
	cfClearance string
	cfMutex     sync.RWMutex
)

// SetCfClearance 安全地设置 cf_clearance 值
func SetCfClearance(value string) {
	cfMutex.Lock()
	defer cfMutex.Unlock()
	cfClearance = value
}

// GetCfClearance 安全地获取 cf_clearance 值
func GetCfClearance() string {
	cfMutex.RLock()
	defer cfMutex.RUnlock()
	return cfClearance
}

// VerifyCloudflareChallenge 验证 Cloudflare 挑战结果
func VerifyCloudflareChallenge(ctx context.Context, result map[string]interface{}) bool {
	logger.Debug(ctx, "Verifying cloudflare challenge result")

	try := func() (bool, error) {
		// 从响应中提取必要信息
		responseData, ok := result["response"].(map[string]interface{})
		if !ok {
			return false, fmt.Errorf("missing response data")
		}

		cookies, ok := responseData["cookies"].(map[string]interface{})
		if !ok {
			return false, fmt.Errorf("missing cookies")
		}

		headers, ok := responseData["headers"].(map[string]interface{})
		if !ok {
			return false, fmt.Errorf("missing headers")
		}

		dataObj, ok := result["data"].(map[string]interface{})
		if !ok {
			return false, fmt.Errorf("missing data object")
		}

		url, ok := dataObj["url"].(string)
		if !ok {
			return false, fmt.Errorf("missing URL")
		}

		// 准备请求
		client := &http.Client{
			Timeout: 30 * time.Second,
		}

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return false, err
		}

		// 设置 User-Agent
		userAgent, ok := headers["User-Agent"].(string)
		if ok {
			req.Header.Set("User-Agent", userAgent)
		}

		// 设置 cookies
		for name, value := range cookies {
			if strValue, ok := value.(string); ok {
				req.AddCookie(&http.Cookie{
					Name:  name,
					Value: strValue,
				})
			}
		}

		// 发送请求
		resp, err := client.Do(req)
		if err != nil {
			return false, err
		}
		defer resp.Body.Close()

		// 读取响应内容
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return false, err
		}

		// 检查响应是否包含成功标记
		return strings.Contains(string(bodyBytes), "Captcha is passed successfully!"), nil
	}

	success, err := try()
	if err != nil {
		logger.Errorf(ctx, "Error verifying cloudflare challenge: %v", err)
		return false
	}

	return success
}

// CreateTask 创建任务
func CreateTask(ctx context.Context, data TaskRequest) (*TaskResponse, error) {
	var proxyURL string

	// 从配置中获取代理 URL
	if config.RecaptchaProxyUrl != "" {
		proxyURL = config.RecaptchaProxyUrl
	} else {
		proxyURL = "http://127.0.0.1:3000"
	}

	taskURL := fmt.Sprintf("%s/createTask", proxyURL)

	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", taskURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var response TaskResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}

	return &response, nil
}

// GetTaskResult 获取任务结果
func GetTaskResult(ctx context.Context, taskID string, clientKey string) (*TaskResultResponse, error) {
	var proxyURL string

	// 从配置中获取代理 URL
	if config.RecaptchaProxyUrl != "" {
		proxyURL = config.RecaptchaProxyUrl
	} else {
		proxyURL = "http://127.0.0.1:3000"
	}

	taskURL := fmt.Sprintf("%s/getTaskResult", proxyURL)

	data := TaskResultRequest{
		ClientKey: clientKey,
		TaskID:    taskID,
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", taskURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var response TaskResultResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}

	return &response, nil
}

// PollTaskResult 轮询任务结果
func PollTaskResult(ctx context.Context, taskID string, clientKey string) (map[string]interface{}, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			response, err := GetTaskResult(ctx, taskID, clientKey)
			if err != nil {
				return nil, err
			}

			if response.Status == "completed" {
				if result, ok := response.Result.(map[string]interface{}); ok {
					return result, nil
				}
				return nil, fmt.Errorf("invalid result format")
			}

			time.Sleep(3 * time.Second)
		}
	}
}

// HandleCloudflareChallenge 处理 Cloudflare 挑战
func HandleCloudflareChallenge(ctx context.Context, url string) (string, error) {
	logger.Debug(ctx, "Handling cloudflare challenge for URL: "+url)

	if config.RecaptchaProxyUrl == "" || config.RecaptchaProxyClientKey == "" {
		return "", fmt.Errorf("Recaptcha proxy URL or client key is not set")
	}

	data := TaskRequest{
		ClientKey: config.RecaptchaProxyClientKey,
		Type:      "CloudflareChallenge",
		URL:       url,
		UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36 Edg/134.0.0.0",
	}

	taskInfo, err := CreateTask(ctx, data)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %v", err)
	}

	logger.Debug(ctx, "Task created with ID: "+taskInfo.TaskID)

	result, err := PollTaskResult(ctx, taskInfo.TaskID, config.RecaptchaProxyClientKey)
	if err != nil {
		return "", fmt.Errorf("failed to poll task result: %v", err)
	}

	resultJSON, _ := json.MarshalIndent(result, "", "  ")
	logger.Debug(ctx, "Challenge result: "+string(resultJSON))

	success := VerifyCloudflareChallenge(ctx, result)
	logger.Debug(ctx, "Challenge verification result: "+fmt.Sprintf("%t", success))
	var cf_clearance_value string
	if responseData, ok := result["response"].(map[string]interface{}); ok {
		if cookies, ok := responseData["cookies"].(map[string]interface{}); ok {
			if clearance, ok := cookies["cf_clearance"].(string); ok {
				cf_clearance_value = clearance
			}
		}
	}
	if cf_clearance_value != "" {
		cookieValue := "cf_clearance=" + cf_clearance_value
		SetCfClearance(cookieValue) // 安全地更新全局变量
	}

	return cf_clearance_value, nil
}
