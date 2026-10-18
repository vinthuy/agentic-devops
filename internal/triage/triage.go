// Package triage 依据需求文本与仓库配置为任务分级（S / M / L）。
// 规则始终生效；可选叠加一次模型估算，取更保守的结果。
package triage

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"agentdev/internal/config"
	"agentdev/internal/llm"
)

// Level 是任务分级，可用于比较大小：S < M < L。
type Level int

const (
	S Level = iota
	M
	L
)

func (l Level) String() string {
	switch l {
	case L:
		return "L"
	case M:
		return "M"
	default:
		return "S"
	}
}

// Parse 解析用户输入的分级；空字符串表示未指定。
func Parse(s string) (Level, bool, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "":
		return S, false, nil
	case "S":
		return S, true, nil
	case "M":
		return M, true, nil
	case "L":
		return L, true, nil
	}
	return S, false, fmt.Errorf("未知分级 %q，只能是 S / M / L", s)
}

type Result struct {
	Level     Level
	Reasons   []string
	ModelUsed bool
	Usage     llm.Usage
}

var (
	broadSignals = []string{
		"整个仓库", "全部模块", "全部仓库", "整体重构", "大规模重构", "跨模块", "多个服务", "多个仓库",
		"rewrite the whole", "across the codebase", "entire codebase", "large refactor", "major refactor",
	}
	riskySignals = []string{
		"迁移", "migration", "schema", "表结构", "数据库结构",
		"认证", "鉴权", "权限", "登录", "auth", "oauth", "token", "密钥",
		"依赖", "dependency", "dependencies", "升级", "upgrade", "版本号",
		"ci", "流水线", "pipeline", "workflow", "部署", "deploy", "infra",
		"breaking", "不兼容", "公开 api", "public api", "接口协议",
	}
)

var tokenRe = regexp.MustCompile(`[a-zA-Z]{3,}`)

// Rules 只做确定性判断，不调用模型。
func Rules(requirement string, repo config.RepoConfig) Result {
	lower := strings.ToLower(requirement)
	var reasons []string

	for _, sig := range broadSignals {
		if strings.Contains(lower, strings.ToLower(sig)) {
			reasons = append(reasons, "需求涉及大范围改动（命中 \""+sig+"\"）")
			return Result{Level: L, Reasons: reasons}
		}
	}

	for _, sig := range riskySignals {
		if strings.Contains(lower, strings.ToLower(sig)) {
			reasons = append(reasons, "需求涉及高风险领域（命中 \""+sig+"\"）")
			break
		}
	}
	for _, pat := range repo.SensitivePaths {
		for _, tok := range tokenRe.FindAllString(strings.ToLower(pat), -1) {
			if strings.Contains(lower, tok) {
				reasons = append(reasons, "需求涉及敏感路径 "+pat)
				break
			}
		}
	}

	if len(reasons) > 0 {
		return Result{Level: M, Reasons: reasons}
	}
	return Result{Level: S, Reasons: []string{"未命中任何高风险信号"}}
}

//go:embed prompts/triage.v1.md
var triagePrompt string

// ModelEstimator 用一次结构化调用估算改动规模。
type ModelEstimator struct {
	Client    *llm.Client
	Model     string
	MaxTokens int
}

type estimate struct {
	Level      string   `json:"level"`
	Files      int      `json:"estimated_files"`
	Lines      int      `json:"estimated_lines"`
	Reasons    []string `json:"reasons"`
	NeedsInfo  bool     `json:"needs_info"`
	Confidence string   `json:"confidence"`
}

// Estimate 返回模型给出的分级与理由。任何未识别的取值都按 S 处理，避免误升。
func (m *ModelEstimator) Estimate(ctx context.Context, requirement string, repo config.RepoConfig, source ...string) (Result, error) {
	var b strings.Builder
	tag, trusted := "user", true
	if len(source) > 0 && source[0] == "issue" {
		tag, trusted = "issue", false
	}
	fmt.Fprintf(&b, "<task_requirement source=\"%s\" trusted=\"%t\">\n%s\n</task_requirement>\n\n", tag, trusted, requirement)
	b.WriteString("<repo_context trusted=\"true\">\n")
	fmt.Fprintf(&b, "默认分支: %s\n", repo.DefaultBranch)
	if len(repo.SensitivePaths) > 0 {
		fmt.Fprintf(&b, "敏感路径: %s\n", strings.Join(repo.SensitivePaths, ", "))
	}
	var names []string
	for _, c := range repo.VerifyCommands {
		names = append(names, c.Name)
	}
	fmt.Fprintf(&b, "验证命令: %s\n", strings.Join(names, ", "))
	b.WriteString("</repo_context>")

	resp, input, err := m.Client.CallTool(ctx, llm.Request{
		Model:     m.Model,
		MaxTokens: m.MaxTokens,
		System:    triagePrompt,
		Messages:  []any{map[string]any{"role": "user", "content": b.String()}},
		Tools:     []map[string]any{submitEstimateTool()},
	}, "submit_estimate")
	if err != nil {
		return Result{Usage: resp.Usage}, err
	}

	data, err := json.Marshal(input)
	if err != nil {
		return Result{}, err
	}
	var est estimate
	if err := json.Unmarshal(data, &est); err != nil {
		return Result{}, fmt.Errorf("分级结果无法解析: %w", err)
	}
	lvl := S
	switch strings.ToUpper(est.Level) {
	case "L":
		lvl = L
	case "M":
		lvl = M
	}
	reasons := append([]string(nil), est.Reasons...)
	if est.Files > 0 {
		reasons = append(reasons, fmt.Sprintf("预估改动 %d 个文件 / %d 行", est.Files, est.Lines))
	}
	return Result{Level: lvl, Reasons: reasons, ModelUsed: true, Usage: resp.Usage}, nil
}

// Combine 取两个结果中更保守的一个，并保留双方理由。
func Combine(a, b Result) Result {
	out := a
	if b.Level > out.Level {
		out.Level = b.Level
	}
	out.Reasons = append(append([]string(nil), a.Reasons...), b.Reasons...)
	out.ModelUsed = a.ModelUsed || b.ModelUsed
	out.Usage = a.Usage.Add(b.Usage)
	return out
}

func submitEstimateTool() map[string]any {
	return map[string]any{
		"name":        "submit_estimate",
		"description": "提交任务分级估算。",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"level": map[string]any{
					"type":        "string",
					"enum":        []string{"S", "M", "L"},
					"description": "S=单点小改动；M=涉及敏感区域或需要审批；L=跨模块大改动，应拆分",
				},
				"estimated_files": map[string]any{"type": "integer"},
				"estimated_lines": map[string]any{"type": "integer"},
				"reasons":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"confidence":      map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}},
			},
			"required": []string{"level", "reasons"},
		},
	}
}
