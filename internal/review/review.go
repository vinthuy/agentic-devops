// Package review 提供独立的 Reviewer：阅读 diff 与验收结果，判断改动是否满足需求。
//
// 设计要点（SDD 5.6 / FR-034）：
//   - Reviewer 与编码不是同一个步骤，看不到编码过程，只看到最终 diff 与验收证据，
//     避免“自己写、自己判”。
//   - 只允许因**可指认的问题**打回：需求未满足、正确性缺陷、安全问题、测试被削弱。
//     风格、命名、格式等主观偏好不得打回，否则会退化成“模型心情闸门”。
//   - 每条意见必须指向 diff 中的具体文件（尽量带行号），便于编码步骤直接修复。
package review

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"agentdev/internal/llm"
)

type Decision string

const (
	Approve        Decision = "approve"
	RequestChanges Decision = "request_changes"
)

type Severity string

const (
	SeverityHigh   Severity = "high"
	SeverityMedium Severity = "medium"
	SeverityLow    Severity = "low"
)

// Finding 是一条可指认的审查意见。
type Finding struct {
	Path     string   `json:"path"`
	Line     int      `json:"line"`
	Severity Severity `json:"severity"`
	Comment  string   `json:"comment"`
}

// Review 是 Reviewer 的结论。
type Review struct {
	Decision Decision  `json:"decision"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

// Approved 报告是否通过。
func (r Review) Approved() bool { return r.Decision == Approve }

// Blockers 返回需要修复的意见：高、中severity 都算阻塞，低 severity 只做提示。
func (r Review) Blockers() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Severity == SeverityHigh || f.Severity == SeverityMedium {
			out = append(out, f)
		}
	}
	return out
}

// Format 把审查意见渲染成可回传给编码步骤的文本。
func (r Review) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Reviewer 判定不通过：%s\n", r.Summary)
	for _, f := range r.Findings {
		loc := f.Path
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
		}
		fmt.Fprintf(&b, "- [%s] %s %s\n", f.Severity, loc, f.Comment)
	}
	return strings.TrimRight(b.String(), "\n")
}

var validSeverity = map[Severity]bool{
	SeverityHigh: true, SeverityMedium: true, SeverityLow: true,
}

// Validate 校验结论的内部一致性。
//
// 这里刻意不接受“decision=approve 但存在 high 级问题”：两者矛盾时宁可报错重试，
// 也不要让一条高危意见被 approve 掩盖过去。
func Validate(r Review) error {
	switch r.Decision {
	case Approve:
		if strings.TrimSpace(r.Summary) == "" {
			return fmt.Errorf("decision=approve 时必须给出 summary")
		}
		for _, f := range r.Findings {
			if f.Severity == SeverityHigh || f.Severity == SeverityMedium {
				return fmt.Errorf("存在 %s 级阻塞问题时不能判定 approve: %s", f.Severity, f.Comment)
			}
		}
	case RequestChanges:
		if len(r.Blockers()) == 0 {
			return fmt.Errorf("decision=request_changes 时必须给出至少一条 high 或 medium 级阻塞意见")
		}
	default:
		return fmt.Errorf("decision 只能是 %s 或 %s，实际 %q", Approve, RequestChanges, r.Decision)
	}
	for i, f := range r.Findings {
		if !validSeverity[f.Severity] {
			return fmt.Errorf("第 %d 条意见的 severity 非法: %q", i+1, f.Severity)
		}
		if strings.TrimSpace(f.Comment) == "" {
			return fmt.Errorf("第 %d 条意见缺少说明", i+1)
		}
		if strings.TrimSpace(f.Path) == "" {
			return fmt.Errorf("第 %d 条意见缺少文件路径", i+1)
		}
	}
	return nil
}

// Input 是 Reviewer 能看到的全部信息。
type Input struct {
	Source      string
	Requirement string
	Plan        string
	Diff        string
	Acceptance  string
	// CIFailure 是本轮改动要解决的 CI 失败输出（来自真实 CI，非模型推断）。
	//
	// 它是需求的**客观延伸**：检查已经失败，说明某条行为确实不被接受。
	// 不传这个字段时，审查者只看到需求与 diff，会把「为满足失败检查而加的
	// 入参校验」判成超范围的画蛇添足，进而要求回退——那会把唯一能让 CI 变绿
	// 的改动撤掉，任务必然转人工。
	CIFailure string
}

// Reviewer 审查一次改动。
type Reviewer interface {
	Review(ctx context.Context, in Input) (Review, error)
}

// UsageReporter 允许审查器上报用量，用于计费。
type UsageReporter func(role string, u llm.Usage)

// ModelReviewer 通过强制工具调用获取结构化结论。
type ModelReviewer struct {
	Client    *llm.Client
	Model     string
	MaxTokens int
	OnUsage   UsageReporter
}

//go:embed prompts/reviewer.v1.md
var reviewerPrompt string

// maxDiffChars 限制放进上下文的 diff 大小；超长时保留首尾，
// 因为改动通常头部是新增、尾部是收尾，中间多为无关上下文。
const maxDiffChars = 60000

func (m *ModelReviewer) Review(ctx context.Context, in Input) (Review, error) {
	var b strings.Builder
	source, trusted := "user", true
	if in.Source == "issue" {
		source, trusted = "issue", false
	}
	fmt.Fprintf(&b, "<task_requirement source=\"%s\" trusted=\"%t\">\n%s\n</task_requirement>\n\n", source, trusted, in.Requirement)
	if in.Plan != "" {
		fmt.Fprintf(&b, "<plan trusted=\"true\">\n%s\n</plan>\n\n", in.Plan)
	}
	if in.Acceptance != "" {
		fmt.Fprintf(&b, "<acceptance_results trusted=\"true\">\n%s\n</acceptance_results>\n\n", in.Acceptance)
	}
	if in.CIFailure != "" {
		fmt.Fprintf(&b, "<ci_failure trusted=\"true\">\n%s\n</ci_failure>\n\n", in.CIFailure)
	}
	fmt.Fprintf(&b, "<diff trusted=\"false\">\n%s\n</diff>", clampMiddle(in.Diff, maxDiffChars))

	resp, input, err := m.Client.CallTool(ctx, llm.Request{
		Model:     m.Model,
		MaxTokens: m.MaxTokens,
		System:    reviewerPrompt,
		Messages:  []any{map[string]any{"role": "user", "content": b.String()}},
		Tools:     []map[string]any{submitReviewTool()},
	}, "submit_review")
	if m.OnUsage != nil && !resp.Usage.Empty() {
		m.OnUsage("reviewer", resp.Usage)
	}
	if err != nil {
		return Review{}, err
	}
	data, err := json.Marshal(input)
	if err != nil {
		return Review{}, err
	}
	var r Review
	if err := json.Unmarshal(data, &r); err != nil {
		return Review{}, fmt.Errorf("审查结论无法解析: %w", err)
	}
	if err := ValidateAgainstDiff(r, in.Diff); err != nil {
		return Review{}, fmt.Errorf("审查结论不合法: %w", err)
	}
	return r, nil
}

// ValidateAgainstDiff 对所有 Reviewer 实现的结果做程序侧复核，不能只信任模型实现。
func ValidateAgainstDiff(r Review, diff string) error {
	if err := Validate(r); err != nil {
		return err
	}
	return validateFindingPaths(r, diff)
}

func validateFindingPaths(r Review, diff string) error {
	if len(r.Findings) == 0 {
		return nil
	}
	changed := map[string]bool{}
	for _, line := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(line, "+++ ") && !strings.HasPrefix(line, "--- ") {
			continue
		}
		line = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "--- ")
		line = strings.TrimPrefix(line, "b/")
		line = strings.TrimPrefix(line, "a/")
		changed[line] = true
	}
	for _, f := range r.Findings {
		if !changed[f.Path] {
			return fmt.Errorf("审查意见引用了 diff 中不存在的文件 %q", f.Path)
		}
	}
	return nil
}

// clampMiddle 超长时保留首尾各一半，并在中间标注截断。
func clampMiddle(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	half := limit / 2
	return string(runes[:half]) + "\n…（中间部分已截断）…\n" + string(runes[len(runes)-half:])
}

func submitReviewTool() map[string]any {
	return map[string]any{
		"name":        "submit_review",
		"description": "提交本次代码审查结论。",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"decision": map[string]any{
					"type":        "string",
					"enum":        []string{string(Approve), string(RequestChanges)},
					"description": "approve=可以交付；request_changes=必须回到编码修复",
				},
				"summary": map[string]any{
					"type":        "string",
					"description": "一两句话说明结论依据",
				},
				"findings": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":     map[string]any{"type": "string", "description": "diff 中的文件路径"},
							"line":     map[string]any{"type": "integer", "description": "新增行号，未知填 0"},
							"severity": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
							"comment":  map[string]any{"type": "string", "description": "具体问题与修复方向"},
						},
						"required": []string{"path", "severity", "comment"},
					},
				},
			},
			"required": []string{"decision", "summary"},
		},
	}
}
