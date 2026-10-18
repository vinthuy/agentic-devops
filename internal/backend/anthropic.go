package backend

import (
	"context"
	_ "embed"
	"strings"

	"agentdev/internal/llm"
)

//go:embed prompts/coder.v1.md
var coderPrompt string

// Anthropic 基于 Messages 协议的工具调用循环（SDD 5.3）。
// 同一协议可指向 Anthropic 官方或兼容端点，例如 DeepSeek 的 https://api.deepseek.com/anthropic。
type Anthropic struct {
	Client    *llm.Client
	Model     string
	MaxTokens int
	// OnUsage 逐轮上报用量；返回错误会中止本步骤。
	OnUsage func(llm.Usage) error
}

func NewAnthropic(client *llm.Client, model string, maxTokens int) *Anthropic {
	return &Anthropic{Client: client, Model: model, MaxTokens: maxTokens}
}

func (a *Anthropic) Name() string { return "anthropic" }

func (a *Anthropic) RunStep(ctx StepContext) (StepResult, error) {
	messages := []any{map[string]any{"role": "user", "content": UserMessage(ctx)}}
	tools := ctx.Tools.Schemas()
	var usage llm.Usage

	for turn := 1; turn <= ctx.MaxTurns; turn++ {
		resp, err := a.Client.Messages(context.Background(), llm.Request{
			Model:     a.Model,
			MaxTokens: a.MaxTokens,
			System:    coderPrompt,
			Tools:     tools,
			Messages:  messages,
		})
		if err != nil {
			return StepResult{}, Errorf("%v", err)
		}
		if ctx.OnUsage != nil {
			if err := ctx.OnUsage(resp.Usage); err != nil {
				return StepResult{}, err
			}
		}
		usage = usage.Add(resp.Usage)

		// 助手内容块原样回传，保留思考块及其签名，多轮工具调用才能被模型正确接续。
		messages = append(messages, map[string]any{"role": "assistant", "content": resp.Content})

		// 澄清是控制流，不是普通工具副作用。先扫完整批 tool_use：若同一批次里
		// 同时出现 request_info 与 write/edit，优先暂停，不能因块顺序先执行写入。
		for _, raw := range resp.Content {
			block, err := llm.ParseBlock(raw)
			if err != nil {
				return StepResult{}, Errorf("无法解析内容块: %v", err)
			}
			if block.Type != "tool_use" || block.Name != "request_info" {
				continue
			}
			res := ctx.Tools.Execute(block.Name, block.Input)
			if len(res.InfoQuestions) > 0 {
				return StepResult{}, &InfoNeededError{Questions: res.InfoQuestions}
			}
		}

		var texts []string
		var results []any
		for _, raw := range resp.Content {
			block, err := llm.ParseBlock(raw)
			if err != nil {
				return StepResult{}, Errorf("无法解析内容块: %v", err)
			}
			switch block.Type {
			case "text":
				texts = append(texts, block.Text)
			case "tool_use":
				res := ctx.Tools.Execute(block.Name, block.Input)
				if len(res.InfoQuestions) > 0 {
					return StepResult{}, &InfoNeededError{Questions: res.InfoQuestions}
				}
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
