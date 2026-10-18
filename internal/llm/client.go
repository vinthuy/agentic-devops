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
	"sync"
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

// UnsupportedToolChoice 判断错误是否为“该端点不支持强制工具调用”。
// DeepSeek 的 thinking 模式会返回：
// `Thinking mode does not support this tool_choice`。
// 这里刻意只匹配“tool_choice + 不支持”的组合：其他 tool_choice 相关的 4xx
// 属于真实的参数错误，应当暴露出来而不是被当成能力缺失悄悄绕开。
func (e *APIError) UnsupportedToolChoice() bool {
	if e.Status < 400 || e.Status >= 500 || !strings.Contains(e.Body, "tool_choice") {
		return false
	}
	for _, phrase := range []string{"does not support", "not supported", "unsupported"} {
		if strings.Contains(e.Body, phrase) {
			return true
		}
	}
	return false
}

type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
	Retries int
	// RetryBaseDelay 是重试退避的基准间隔，实际等待 attempt² × 该值。测试中可设为零。
	RetryBaseDelay time.Duration

	mu sync.Mutex
	// forcedToolChoiceDisabled 由运行时探测得出：某些兼容端点不支持强制
	// tool_choice，首次遇到后记住，后续请求不再发送该参数。
	forcedToolChoiceDisabled bool
}

// ToolChoiceDisabled 报告该端点的强制工具调用能力是否已被探测为不可用。
// 供编排器在报告中说明“结构化输出走了降级路径”。
func (c *Client) ToolChoiceDisabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.forcedToolChoiceDisabled
}

func (c *Client) disableToolChoice() {
	c.mu.Lock()
	c.forcedToolChoiceDisabled = true
	c.mu.Unlock()
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

// Messages 发送一次请求，必要时自动绕过端点的强制工具调用限制。
//
// 部分兼容端点（例如 DeepSeek 的 thinking 模式）不支持 tool_choice。这类端点
// 会返回 4xx，此时去掉 tool_choice 重发一次，并记住该能力以免每次白跑一趟。
// 调用方拿到的仍然是模型的选择结果，只是不再由服务端强制。
func (c *Client) Messages(ctx context.Context, req Request) (Response, error) {
	if req.ToolChoice != nil && c.ToolChoiceDisabled() {
		req.ToolChoice = nil
	}
	resp, err := c.messagesWithRetry(ctx, req)
	if err == nil || req.ToolChoice == nil {
		return resp, err
	}
	var apiErr *APIError
	if asAPIError(err, &apiErr) && apiErr.UnsupportedToolChoice() {
		c.disableToolChoice()
		req.ToolChoice = nil
		return c.messagesWithRetry(ctx, req)
	}
	return resp, err
}

// messagesWithRetry 发送请求。限流与服务端错误会重试，其他 4xx 直接返回。
func (c *Client) messagesWithRetry(ctx context.Context, req Request) (Response, error) {
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

// maxToolCallAttempts 是一次结构化输出的最大尝试次数：
// 首次请求 + 一次“你没有调用工具”的追加提示。
const maxToolCallAttempts = 2

// CallTool 请求一次结构化工具调用，返回该工具的入参。
//
// 与 Messages 的差别有两点：
//   - 优先使用强制 tool_choice；端点不支持时由 Messages 自动降级并记住能力。
//   - 模型用文本回答而没有调用工具时，追加一次明确的提示重试，而不是直接失败。
//
// 两次尝试的用量会累加，避免降级路径漏计费。
func (c *Client) CallTool(ctx context.Context, req Request, toolName string) (Response, map[string]any, error) {
	if req.ToolChoice == nil {
		req.ToolChoice = map[string]any{"type": "tool", "name": toolName}
	}
	base := req.Messages
	var total Response
	for attempt := 0; attempt < maxToolCallAttempts; attempt++ {
		cur := req
		cur.Messages = base
		resp, err := c.Messages(ctx, cur)
		if err != nil {
			return Response{}, nil, err
		}
		total.Usage = total.Usage.Add(resp.Usage)
		total.Content = resp.Content
		total.StopReason = resp.StopReason
		if input, ok := FindToolInput(resp, toolName); ok {
			return total, input, nil
		}
		if attempt == maxToolCallAttempts-1 {
			break
		}
		nudged := make([]any, 0, len(base)+2)
		nudged = append(nudged, base...)
		nudged = append(nudged,
			map[string]any{"role": "assistant", "content": resp.Content},
			map[string]any{"role": "user", "content": fmt.Sprintf(
				"你没有调用 %s 工具。请立即通过 %s 工具提交结果，不要在文本中回答。", toolName, toolName)})
		base = nudged
	}
	return total, nil, fmt.Errorf("模型未通过 %s 工具返回结构化结果", toolName)
}

// FindToolInput 在响应中查找指定工具的入参。
func FindToolInput(resp Response, toolName string) (map[string]any, bool) {
	for _, raw := range resp.Content {
		block, err := ParseBlock(raw)
		if err != nil {
			continue
		}
		if block.Type == "tool_use" && block.Name == toolName && block.Input != nil {
			return block.Input, true
		}
	}
	return nil, false
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
