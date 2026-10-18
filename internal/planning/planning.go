// Package planning 定义结构化的实施计划，并提供基于模型与静态两种规划器。
package planning

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"agentdev/internal/llm"
)

// CheckType 是验收标准的可执行检查类型。
type CheckType string

const (
	CheckCommand    CheckType = "command"     // 运行 repos.yaml 中配置的验证命令
	CheckTest       CheckType = "test"        // 运行验证命令并附加测试选择器
	CheckFileExists CheckType = "file_exists" // 文件存在
	CheckGrep       CheckType = "grep"        // 文件内容匹配
)

// Check 描述如何机器校验一条验收标准。
type Check struct {
	Type     CheckType `json:"type"`
	Name     string    `json:"name,omitempty"`
	Selector string    `json:"selector,omitempty"`
	Path     string    `json:"path,omitempty"`
	Pattern  string    `json:"pattern,omitempty"`
	Expect   string    `json:"expect,omitempty"` // present | absent
}

type Acceptance struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Check       Check  `json:"check"`
}

type Step struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Files       []string `json:"files,omitempty"`
}

type Scope struct {
	AllowedPaths   []string `json:"allowed_paths,omitempty"`
	EstimatedFiles int      `json:"estimated_files,omitempty"`
	EstimatedLines int      `json:"estimated_lines,omitempty"`
}

const (
	StatusReady     = "ready"
	StatusNeedsInfo = "needs_info"
)

type Plan struct {
	Status           string       `json:"status"`
	Questions        []string     `json:"questions,omitempty"`
	Summary          string       `json:"summary"`
	Scope            Scope        `json:"scope"`
	Steps            []Step       `json:"steps,omitempty"`
	Acceptance       []Acceptance `json:"acceptance,omitempty"`
	RiskFlags        []string     `json:"risk_flags,omitempty"`
	EstimatedCostUSD float64      `json:"estimated_cost_usd,omitempty"`
}

func (p Plan) NeedsInfo() bool { return p.Status == StatusNeedsInfo }

// Parse 从 plan_json 反序列化；空字符串返回零值计划。
func Parse(planJSON string) (Plan, error) {
	var p Plan
	if strings.TrimSpace(planJSON) == "" {
		return p, nil
	}
	if err := json.Unmarshal([]byte(planJSON), &p); err != nil {
		return Plan{}, err
	}
	return p, nil
}

// PromptText 把计划渲染成给编码模型看的文本（步骤 + 验收标准）。
func (p Plan) PromptText() string {
	if p.Summary == "" && len(p.Steps) == 0 && len(p.Acceptance) == 0 {
		return ""
	}
	var b strings.Builder
	if p.Summary != "" {
		b.WriteString("概要: " + p.Summary + "\n")
	}
	if len(p.Steps) > 0 {
		b.WriteString("步骤:\n")
		for _, s := range p.Steps {
			fmt.Fprintf(&b, "- %s %s", s.ID, s.Description)
			if len(s.Files) > 0 {
				fmt.Fprintf(&b, "（涉及文件: %s）", strings.Join(s.Files, ", "))
			}
			b.WriteString("\n")
		}
	}
	if len(p.Acceptance) > 0 {
		b.WriteString("必须满足的验收标准:\n")
		for _, a := range p.Acceptance {
			fmt.Fprintf(&b, "- [%s] %s\n", a.ID, a.Description)
		}
	}
	if len(p.Scope.AllowedPaths) > 0 {
		fmt.Fprintf(&b, "只允许修改: %s\n", strings.Join(p.Scope.AllowedPaths, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// shellMeta 用于拒绝含 shell 元字符的选择器，避免把模型的输出变成任意命令执行。
var shellMeta = regexp.MustCompile("[;&|`<>$()\n\r\t]")

var validCheckTypes = map[CheckType]bool{
	CheckCommand: true, CheckTest: true, CheckFileExists: true, CheckGrep: true,
}

// Validate 校验计划的内部一致性。verifyNames 为 repos.yaml 中可用的验证命令名。
func Validate(p Plan, verifyNames map[string]bool) error {
	switch p.Status {
	case StatusNeedsInfo:
		if len(p.Questions) == 0 {
			return fmt.Errorf("status=needs_info 时必须给出澄清问题")
		}
		return nil
	case StatusReady:
	default:
		return fmt.Errorf("status 只能是 %s 或 %s，实际 %q", StatusReady, StatusNeedsInfo, p.Status)
	}

	if len(p.Acceptance) == 0 {
		return fmt.Errorf("验收标准不能为空：必须给出可机器校验的检查")
	}
	seen := map[string]bool{}
	for i, ac := range p.Acceptance {
		if ac.ID == "" {
			return fmt.Errorf("验收标准 #%d 缺少 id", i+1)
		}
		if seen[ac.ID] {
			return fmt.Errorf("验收标准 id 重复: %s", ac.ID)
		}
		seen[ac.ID] = true
		if ac.Description == "" {
			return fmt.Errorf("验收标准 %s 缺少 description", ac.ID)
		}
		if err := validateCheck(ac.Check, verifyNames); err != nil {
			return fmt.Errorf("验收标准 %s: %w", ac.ID, err)
		}
	}
	for _, path := range p.Scope.AllowedPaths {
		if err := validateRelPath(path); err != nil {
			return fmt.Errorf("scope.allowed_paths: %w", err)
		}
	}
	return nil
}

func validateCheck(c Check, verifyNames map[string]bool) error {
	if !validCheckTypes[c.Type] {
		return fmt.Errorf("check.type 不支持: %q", c.Type)
	}
	switch c.Type {
	case CheckCommand:
		if !verifyNames[c.Name] {
			return fmt.Errorf("check.name %q 不是 repos.yaml 中配置的验证命令", c.Name)
		}
	case CheckTest:
		if !verifyNames[c.Name] {
			return fmt.Errorf("check.name %q 不是 repos.yaml 中配置的验证命令", c.Name)
		}
		if c.Selector != "" && shellMeta.MatchString(c.Selector) {
			return fmt.Errorf("check.selector 含禁止字符: %q", c.Selector)
		}
	case CheckFileExists:
		if err := validateRelPath(c.Path); err != nil {
			return fmt.Errorf("check.path: %w", err)
		}
	case CheckGrep:
		if err := validateRelPath(c.Path); err != nil {
			return fmt.Errorf("check.path: %w", err)
		}
		if c.Pattern == "" {
			return fmt.Errorf("check.pattern 不能为空")
		}
		if _, err := regexp.Compile(c.Pattern); err != nil {
			return fmt.Errorf("check.pattern 不是合法正则: %w", err)
		}
		if c.Expect != "present" && c.Expect != "absent" {
			return fmt.Errorf("check.expect 只能是 present 或 absent")
		}
	}
	return nil
}

func validateRelPath(p string) error {
	if p == "" {
		return fmt.Errorf("路径不能为空")
	}
	norm := strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(norm, "/") || strings.Contains(norm, ":") {
		return fmt.Errorf("必须是工作区内的相对路径: %q", p)
	}
	for _, seg := range strings.Split(norm, "/") {
		if seg == ".." {
			return fmt.Errorf("不允许包含 ..: %q", p)
		}
	}
	return nil
}

// Requirement 是规划器的输入。
type Requirement struct {
	TaskID      string
	Source      string
	Text        string
	VerifyNames []string
	Commands    map[string]string
	RepoSummary string
}

// Planner 生成实施计划。
type Planner interface {
	Plan(ctx context.Context, req Requirement) (Plan, error)
}

// UsageReporter 允许规划器上报用量，用于计费。
type UsageReporter func(role string, u llm.Usage)

// ModelPlanner 通过强制工具调用获取结构化计划（ADR：不用自由文本解析）。
type ModelPlanner struct {
	Client    *llm.Client
	Model     string
	MaxTokens int
	OnUsage   UsageReporter
}

//go:embed prompts/planner.v1.md
var plannerPrompt string

func (p *ModelPlanner) Plan(ctx context.Context, req Requirement) (Plan, error) {
	var b strings.Builder
	source, trusted := requirementTag(req.Source)
	fmt.Fprintf(&b, "<task_requirement source=\"%s\" trusted=\"%t\">\n%s\n</task_requirement>\n\n", source, trusted, req.Text)
	b.WriteString("<repo_context trusted=\"true\">\n")
	fmt.Fprintf(&b, "可用的验证命令名: %s\n", strings.Join(req.VerifyNames, ", "))
	for name, cmd := range req.Commands {
		fmt.Fprintf(&b, "- %s: %s\n", name, cmd)
	}
	if req.RepoSummary != "" {
		fmt.Fprintf(&b, "仓库文件概览:\n%s\n", req.RepoSummary)
	}
	b.WriteString("</repo_context>")

	resp, input, err := p.Client.CallTool(ctx, llm.Request{
		Model:     p.Model,
		MaxTokens: p.MaxTokens,
		System:    plannerPrompt,
		Messages:  []any{map[string]any{"role": "user", "content": b.String()}},
		Tools:     []map[string]any{submitPlanTool()},
	}, "submit_plan")
	if p.OnUsage != nil && !resp.Usage.Empty() {
		p.OnUsage("planner", resp.Usage)
	}
	if err != nil {
		return Plan{}, err
	}

	data, err := json.Marshal(input)
	if err != nil {
		return Plan{}, err
	}
	var plan Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		return Plan{}, fmt.Errorf("计划格式无法解析: %w", err)
	}
	if err := Validate(plan, nameSet(req.VerifyNames)); err != nil {
		return Plan{}, fmt.Errorf("计划校验失败: %w", err)
	}
	return plan, nil
}

func requirementTag(source string) (string, bool) {
	if source == "issue" {
		return "issue", false
	}
	return "user", true
}

func nameSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// StaticPlanner 不调用模型：把每个配置的验证命令作为验收标准。
// 用于离线运行与测试，保证确定性。
type StaticPlanner struct {
	VerifyNames []string
	Commands    map[string]string
}

func (s StaticPlanner) Plan(_ context.Context, req Requirement) (Plan, error) {
	names := req.VerifyNames
	if names == nil {
		names = s.VerifyNames
	}
	plan := Plan{
		Status:  StatusReady,
		Summary: fmt.Sprintf("按需求直接实施，并以全部验证命令作为验收标准（共 %d 项）", len(names)),
		Scope:   Scope{},
	}
	for i, name := range names {
		plan.Acceptance = append(plan.Acceptance, Acceptance{
			ID:          fmt.Sprintf("ac%d", i+1),
			Description: fmt.Sprintf("验证命令 %s 通过", name),
			Check:       Check{Type: CheckCommand, Name: name},
		})
	}
	return plan, nil
}

func submitPlanTool() map[string]any {
	checkSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"type":     map[string]any{"type": "string", "enum": []string{"command", "test", "file_exists", "grep"}},
			"name":     map[string]any{"type": "string", "description": "verify 命令名，type 为 command/test 时必填"},
			"selector": map[string]any{"type": "string", "description": "测试选择器，会作为参数附加到命令后"},
			"path":     map[string]any{"type": "string", "description": "工作区内的相对路径"},
			"pattern":  map[string]any{"type": "string", "description": "正则，type 为 grep 时必填"},
			"expect":   map[string]any{"type": "string", "enum": []string{"present", "absent"}},
		},
		"required": []string{"type"},
	}
	return map[string]any{
		"name":        "submit_plan",
		"description": "提交结构化的实施计划。status 为 needs_info 时只需给出问题。",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status":    map[string]any{"type": "string", "enum": []string{"ready", "needs_info"}},
				"questions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"summary":   map[string]any{"type": "string"},
				"scope": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"allowed_paths":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"estimated_files": map[string]any{"type": "integer"},
						"estimated_lines": map[string]any{"type": "integer"},
					},
				},
				"steps": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":          map[string]any{"type": "string"},
							"description": map[string]any{"type": "string"},
							"files":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
						"required": []string{"id", "description"},
					},
				},
				"acceptance": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":          map[string]any{"type": "string"},
							"description": map[string]any{"type": "string"},
							"check":       checkSchema,
						},
						"required": []string{"id", "description", "check"},
					},
				},
				"risk_flags":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"estimated_cost_usd": map[string]any{"type": "number"},
			},
			"required": []string{"status", "summary", "acceptance"},
		},
	}
}
