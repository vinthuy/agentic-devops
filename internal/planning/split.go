package planning

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agentdev/internal/llm"
)

// Slice 是 L 级需求里的一段。顺序就是依赖顺序：下一段必须等上一段完成。
type Slice struct {
	Summary      string       `json:"summary"`
	AllowedPaths []string     `json:"allowed_paths"`
	Acceptance   []Acceptance `json:"acceptance"`
}

// SplitPlan 是按顺序执行的分段计划。
type SplitPlan struct {
	Slices []Slice `json:"slices"`
}

// Splitter 把 L 级需求拆成多段。拆不开时返回错误。
type Splitter interface {
	Split(ctx context.Context, req Requirement) (SplitPlan, error)
}

// ParseSplit 读取父任务上保存的分段计划。
func ParseSplit(raw string) (SplitPlan, error) {
	var sp SplitPlan
	if strings.TrimSpace(raw) == "" {
		return sp, fmt.Errorf("拆分计划为空")
	}
	if err := json.Unmarshal([]byte(raw), &sp); err != nil {
		return SplitPlan{}, err
	}
	return sp, nil
}

// ValidateSplit 要求至少两段，且每段都有摘要、允许路径和可执行的验收标准。
func ValidateSplit(sp SplitPlan, verifyNames map[string]bool) error {
	if len(sp.Slices) < 2 {
		return fmt.Errorf("拆分至少需要两段")
	}
	for i, sl := range sp.Slices {
		if strings.TrimSpace(sl.Summary) == "" {
			return fmt.Errorf("第 %d 段缺少摘要", i+1)
		}
		if len(sl.AllowedPaths) == 0 {
			return fmt.Errorf("第 %d 段缺少允许路径", i+1)
		}
		plan := Plan{
			Status:     StatusReady,
			Summary:    sl.Summary,
			Scope:      Scope{AllowedPaths: sl.AllowedPaths},
			Acceptance: sl.Acceptance,
		}
		if err := Validate(plan, verifyNames); err != nil {
			return fmt.Errorf("第 %d 段: %w", i+1, err)
		}
	}
	return nil
}

// ModelSplitter 用与规划相同的端点把 L 级需求拆成多段。
type ModelSplitter struct {
	Client    *llm.Client
	Model     string
	MaxTokens int
	OnUsage   UsageReporter
}

func (m *ModelSplitter) Split(ctx context.Context, req Requirement) (SplitPlan, error) {
	if m == nil || m.Client == nil {
		return SplitPlan{}, fmt.Errorf("没有可用的拆分模型")
	}
	resp, input, err := m.Client.CallTool(ctx, llm.Request{
		Model:     m.Model,
		MaxTokens: m.MaxTokens,
		System:    "把需求拆成至少两段、必须按顺序单独审批的实施段落。每一段只做一件可以独立验收的事。不要放宽路径、命令或审批限制。",
		Messages:  []any{map[string]any{"role": "user", "content": req.Text}},
		Tools:     []map[string]any{submitSplitTool()},
	}, "submit_split")
	if m.OnUsage != nil && !resp.Usage.Empty() {
		m.OnUsage("planner", resp.Usage)
	}
	if err != nil {
		return SplitPlan{}, err
	}
	data, err := json.Marshal(input)
	if err != nil {
		return SplitPlan{}, err
	}
	var sp SplitPlan
	if err := json.Unmarshal(data, &sp); err != nil {
		return SplitPlan{}, fmt.Errorf("拆分结果无法解析: %w", err)
	}
	names := map[string]bool{}
	for _, name := range req.VerifyNames {
		names[name] = true
	}
	if err := ValidateSplit(sp, names); err != nil {
		return SplitPlan{}, err
	}
	return sp, nil
}

func submitSplitTool() map[string]any {
	return map[string]any{
		"name":        "submit_split",
		"description": "提交按顺序执行的分段计划。至少两段。",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"slices": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"summary":       map[string]any{"type": "string"},
							"allowed_paths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"acceptance":    map[string]any{"type": "array"},
						},
						"required": []string{"summary", "allowed_paths", "acceptance"},
					},
				},
			},
			"required": []string{"slices"},
		},
	}
}
