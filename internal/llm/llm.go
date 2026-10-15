// Package llm 封装大模型调用。
//
// 设计要点：生产代码只依赖 Client 接口，测试可以注入 fake 实现
// 或基于 httptest 的假服务端，从而让「模型调用」这件事变得可测试、可复现。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// 消息角色，遵循 OpenAI Chat Completions 约定。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Message 表示一次对话中的单条消息。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client 抽象模型提供方。
type Client interface {
	Complete(ctx context.Context, msgs []Message) (string, error)
}

// StatusError 表示提供方返回了非 2xx 状态码。
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("llm: provider status %d: %s", e.Status, Truncate(e.Body, 160))
}

// Retryable 判断该错误是否值得重试：
// 限流（429）与服务端错误（5xx）通常可重试，参数错误（4xx）重试没有意义。
func (e *StatusError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

var (
	// ErrEmptyResponse 表示提供方返回 2xx 但内容为空。
	ErrEmptyResponse = errors.New("llm: provider returned empty response")
	// ErrNotConfigured 表示缺少必要配置（通常是 API Key）。
	ErrNotConfigured = errors.New("llm: LLM_API_KEY is not set")
)

// Truncate 按字符截断，避免在多字节字符中间切断。
func Truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// Config 是 OpenAI 兼容接口的配置。
type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	Timeout    time.Duration
	HTTPClient *http.Client
}

func (c Config) withDefaults() Config {
	if c.BaseURL == "" {
		c.BaseURL = "https://api.openai.com/v1"
	}
	if c.Model == "" {
		c.Model = "gpt-4o-mini"
	}
	if c.Timeout <= 0 {
		c.Timeout = 20 * time.Second
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{}
	}
	return c
}

// HTTPClient 通过 HTTP 调用 OpenAI 兼容的 /chat/completions 接口。
type HTTPClient struct {
	cfg Config
}

// NewClient 创建一个 HTTP 客户端。
func NewClient(cfg Config) *HTTPClient {
	return &HTTPClient{cfg: cfg.withDefaults()}
}

// FromEnv 从环境变量构造客户端。
// 缺少 API Key 时返回 ErrNotConfigured，调用方据此降级为「无 AI」模式。
//
//	LLM_API_KEY   必填
//	LLM_BASE_URL  选填，默认 https://api.openai.com/v1
//	LLM_MODEL     选填，默认 gpt-4o-mini
//	LLM_TIMEOUT   选填，Go duration 格式，如 20s
func FromEnv() (Client, error) {
	apiKey := strings.TrimSpace(os.Getenv("LLM_API_KEY"))
	if apiKey == "" {
		return nil, ErrNotConfigured
	}
	cfg := Config{
		BaseURL: strings.TrimSpace(os.Getenv("LLM_BASE_URL")),
		APIKey:  apiKey,
		Model:   strings.TrimSpace(os.Getenv("LLM_MODEL")),
	}
	if v := strings.TrimSpace(os.Getenv("LLM_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Timeout = d
		}
	}
	return NewClient(cfg), nil
}

type completionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

type completionResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// Complete 发起一次对话补全请求。
func (c *HTTPClient) Complete(ctx context.Context, msgs []Message) (string, error) {
	if len(msgs) == 0 {
		return "", errors.New("llm: empty conversation")
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	payload, err := json.Marshal(completionRequest{Model: c.cfg.Model, Messages: msgs})
	if err != nil {
		return "", fmt.Errorf("llm: encode request: %w", err)
	}

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("llm: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", &StatusError{Status: resp.StatusCode, Body: string(body)}
	}

	var out completionResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("llm: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", ErrEmptyResponse
	}
	content := strings.TrimSpace(out.Choices[0].Message.Content)
	if content == "" {
		return "", ErrEmptyResponse
	}
	return content, nil
}

var _ Client = (*HTTPClient)(nil)
