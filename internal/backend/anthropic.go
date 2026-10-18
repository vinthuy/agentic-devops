package backend

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	_ "embed"
)

//go:embed prompts/coder.v1.md
var coderPrompt string

const anthropicVersion = "2023-06-01"

// Anthropic 基于 Messages 协议的工具调用循环（SDD 5.3）。
// 同一协议可指向 Anthropic 官方或兼容端点，例如 DeepSeek 的 https://api.deepseek.com/anthropic。
type Anthropic struct {
	Model     string
	MaxTokens int
	BaseURL   string
	APIKey    string
	HTTP      *http.Client
}

func NewAnthropic(model string, maxTokens int, baseURL, apiKey string) *Anthropic {
	return &Anthropic{
		Model:     model,
		MaxTokens: maxTokens,
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIKey:    apiKey,
		HTTP:      &http.Client{Timeout: 10 * time.Minute},
	}
}

func (a *Anthropic) Name() string { return "anthropic" }

type apiResponse struct {
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type contentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

func (a *Anthropic) RunStep(ctx StepContext) (StepResult, error) {
	messages := []any{map[string]any{"role": "user", "content": UserMessage(ctx)}}
	tools := ctx.Tools.Schemas()
	var usage Usage

	for turn := 1; turn <= ctx.MaxTurns; turn++ {
		resp, err := a.call(messages, tools)
		if err != nil {
			return StepResult{}, err
		}
		usage.InputTokens += resp.Usage.InputTokens
		usage.OutputTokens += resp.Usage.OutputTokens

		// 助手内容块原样回传，保留思考块及其签名，多轮工具调用才能被模型正确接续。
		messages = append(messages, map[string]any{"role": "assistant", "content": resp.Content})

		var texts []string
		var results []any
		for _, raw := range resp.Content {
			var block contentBlock
			if err := json.Unmarshal(raw, &block); err != nil {
				return StepResult{}, Errorf("无法解析内容块: %v", err)
			}
			switch block.Type {
			case "text":
				texts = append(texts, block.Text)
			case "tool_use":
				res := ctx.Tools.Execute(block.Name, block.Input)
				results = append(results, map[string]any{
					"type":        "tool_result",
					"tool_use_id": block.ID,
					"content":     res.Content,
					"is_error":    !res.OK,
				})
			}
		}

		if resp.StopReason != "tool_use" {
			return StepResult{FinalText: strings.Join(texts, "\n"), Turns: turn, Usage: usage}, nil
		}
		messages = append(messages, map[string]any{"role": "user", "content": results})
	}
	return StepResult{}, Errorf("超过最大轮数 %d，未完成本步骤", ctx.MaxTurns)
}

// call 发送一次 Messages 请求，对限流与服务端错误做有限次退避重试。
func (a *Anthropic) call(messages []any, tools []map[string]any) (apiResponse, error) {
	body, err := json.Marshal(map[string]any{
		"model":      a.Model,
		"max_tokens": a.MaxTokens,
		"system":     coderPrompt,
		"tools":      tools,
		"messages":   messages,
	})
	if err != nil {
		return apiResponse{}, err
	}

	const attempts = 3
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i*i) * 2 * time.Second)
		}
		req, err := http.NewRequest(http.MethodPost, a.BaseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return apiResponse{}, err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-api-key", a.APIKey)
		req.Header.Set("anthropic-version", anthropicVersion)

		resp, err := a.HTTP.Do(req)
		if err != nil {
			lastErr = Errorf("模型调用失败: %v", err)
			continue
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = Errorf("读取响应失败: %v", readErr)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = Errorf("模型调用失败 HTTP %d: %s", resp.StatusCode, snippet(data))
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return apiResponse{}, Errorf("模型调用失败 HTTP %d: %s", resp.StatusCode, snippet(data))
		}
		var out apiResponse
		if err := json.Unmarshal(data, &out); err != nil {
			return apiResponse{}, Errorf("无法解析模型响应: %v", err)
		}
		return out, nil
	}
	return apiResponse{}, lastErr
}

func snippet(b []byte) string {
	if len(b) > 500 {
		b = b[:500]
	}
	return string(b)
}
