// Package llm 实现对 Anthropic Messages 协议的调用，供编码后端与规划器共用。
// 协议地址可替换，因此同一实现可指向 Anthropic 官方或 DeepSeek 等兼容端点。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const Version = "2023-06-01"

// Usage 为一次调用的 token 用量，字段名与协议一致，便于直接反序列化。
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_input_tokens"`
	CacheWriteTokens int `json:"cache_creation_input_tokens"`
}

func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + o.InputTokens,
		OutputTokens:     u.OutputTokens + o.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + o.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + o.CacheWriteTokens,
	}
}

// Empty 表示没有任何用量，用于判断是否产生了计费。
func (u Usage) Empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheWriteTokens == 0
}

type Request struct {
	Model      string
	MaxTokens  int
	System     string
	Messages   []any
	Tools      []map[string]any
	ToolChoice map[string]any
}

type Response struct {
	// Content 保留原始 JSON 块，原样回传可保留思考块及其签名。
	Content    []json.RawMessage
	StopReason string
	Usage      Usage
}

// APIError 表示模型端返回的错误。Retryable 为 true 时表示重试后仍失败。
type APIError struct {
	Status    int
	Body      string
	Retryable bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("模型调用失败 HTTP %d: %s", e.Status, e.Body)
}

type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
	Retries int
	// RetryBaseDelay 是重试退避的基准间隔，实际等待 attempt² × 该值。测试中可设为零。
	RetryBaseDelay time.Duration
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL:        strings.TrimRight(baseURL, "/"),
		APIKey:         apiKey,
		HTTP:           &http.Client{Timeout: 10 * time.Minute},
		Retries:        3,
		RetryBaseDelay: 2 * time.Second,
	}
}

// Messages 发送一次请求。限流与服务端错误会重试，其他 4xx 直接返回。
func (c *Client) Messages(ctx context.Context, req Request) (Response, error) {
	payload := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"messages":   req.Messages,
	}
	if req.System != "" {
		payload["system"] = req.System
	}
	if len(req.Tools) > 0 {
		payload["tools"] = req.Tools
	}
	if req.ToolChoice != nil {
		payload["tool_choice"] = req.ToolChoice
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, err
	}

	retries := c.Retries
	if retries < 1 {
		retries = 1
	}
	base := c.RetryBaseDelay
	if base < 0 {
		base = 0
	}
	var lastErr error
	for attempt := 0; attempt < retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Response{}, ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * base):
			}
		}
		resp, err := c.do(ctx, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var apiErr *APIError
		if asAPIError(err, &apiErr) && !apiErr.Retryable {
			return Response{}, err
		}
	}
	return Response{}, lastErr
}

func (c *Client) do(ctx context.Context, body []byte) (Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", Version)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Response{}, &APIError{Status: 0, Body: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, &APIError{Status: resp.StatusCode, Body: err.Error(), Retryable: true}
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return Response{}, &APIError{Status: resp.StatusCode, Body: snippet(data), Retryable: true}
	}
	if resp.StatusCode != http.StatusOK {
		return Response{}, &APIError{Status: resp.StatusCode, Body: snippet(data)}
	}
	var out struct {
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason"`
		Usage      Usage             `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Response{}, &APIError{Status: resp.StatusCode, Body: "无法解析响应: " + err.Error()}
	}
	return Response{Content: out.Content, StopReason: out.StopReason, Usage: out.Usage}, nil
}

func asAPIError(err error, target **APIError) bool {
	e, ok := err.(*APIError)
	if ok {
		*target = e
	}
	return ok
}

func snippet(b []byte) string {
	if len(b) > 500 {
		b = b[:500]
	}
	return string(b)
}

// Block 是内容块的公共字段视图；未知类型保留原始 JSON 以便原样回传。
type Block struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

func ParseBlock(raw json.RawMessage) (Block, error) {
	var b Block
	if err := json.Unmarshal(raw, &b); err != nil {
		return Block{}, err
	}
	return b, nil
}
