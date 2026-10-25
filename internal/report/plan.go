package report

import (
	"fmt"
	"strings"

	"agentdev/internal/config"
	"agentdev/internal/planning"
	"agentdev/internal/policy"
)

// PlanWarning 是审批者必须看见的风险提示。
//
// 这些提示全部由计划内容**确定性**推出，不依赖模型判断：审批人看到的是「计划自己
// 说要改 test_session.py」，而不是「模型觉得可能有风险」。
type PlanWarning struct {
	Kind   string
	Detail string
}

// 警告类型，按严重程度从高到低。
const (
	WarnVerifyConfig  = "verify_config"   // 可写范围内含验证配置文件：验证可能被削弱
	WarnSensitive     = "sensitive"       // 可写范围内含敏感路径
	WarnUnrestricted  = "unrestricted"    // 没限定 allowed_paths，只能靠全局策略
	WarnNoFilesListed = "no_files_listed" // 计划没列涉及文件，无法预判范围
	WarnUnparsable    = "unparsable"      // plan_json 解析失败
)

// PlanReview 汇总审批一个计划所需的全部内容。
type PlanReview struct {
	Summary          string
	Steps            []planning.Step
	AllowedPaths     []string
	Acceptance       []planning.Acceptance
	RiskFlags        []string
	EstimatedCostUSD float64
	Warnings         []PlanWarning
	// Empty 表示没有可展示的计划（尚未规划，或计划内容为空）。
	Empty bool
}

// ReviewPlan 从 plan_json 抽出审批要看的内容，并推出风险提示。
//
// 之所以要单独做一层：plan_json 此前是只写不读的数据——审批者要裁决一个 M 级计划，
// 却看不到摘要、步骤、范围和验收标准，只能凭需求原文猜。审批因此退化成「对着
// 一句话点头」。同时，计划是否要动测试文件/敏感路径这一最该拦的信息完全不可见。
func ReviewPlan(planJSON string, repo config.RepoConfig, pol config.PolicyConfig) PlanReview {
	rv := PlanReview{}
	plan, err := planning.Parse(planJSON)
	if err != nil {
		rv.Empty = true
		rv.Warnings = []PlanWarning{{Kind: WarnUnparsable, Detail: "计划 JSON 无法解析: " + err.Error()}}
		return rv
	}
	if plan.Summary == "" && len(plan.Steps) == 0 && len(plan.Acceptance) == 0 {
		rv.Empty = true
		return rv
	}

	rv.Summary = plan.Summary
	rv.Steps = plan.Steps
	rv.AllowedPaths = plan.Scope.AllowedPaths
	rv.Acceptance = plan.Acceptance
	rv.RiskFlags = plan.RiskFlags
	rv.EstimatedCostUSD = plan.EstimatedCostUSD
	rv.Warnings = planWarnings(plan, repo, pol)
	return rv
}

func planWarnings(plan planning.Plan, repo config.RepoConfig, pol config.PolicyConfig) []PlanWarning {
	var verifyHits, sensitiveHits []string
	listed := 0
	for _, step := range plan.Steps {
		for _, raw := range step.Files {
			f := normalizePlanPath(raw)
			if f == "" {
				continue
			}
			listed++
			// files 的语义是「本步骤涉及的文件」，把运行测试也算进去了。真实输出里
			// step「运行 python -m unittest test_core.py」就会把 test_core.py 列进来，
			// 若据此报「计划要改测试文件」就是纯误报。
			//
			// 判断是否**可能被修改**只看 allowed_paths：不在可写范围内的文件，写它会被
			// 策略直接拦下，不需要人来操心；而 allowed_paths 覆盖到的测试文件，策略
			// 不会拦——那正是只有审批者能拦的地方，也是这里该报警的地方。
			if !writable(f, plan.Scope.AllowedPaths) {
				continue
			}
			if matchesAny(f, repo.VerifyConfigFiles) {
				verifyHits = appendUnique(verifyHits, f)
				continue
			}
			if matchesAny(f, repo.SensitivePaths) || matchesAny(f, pol.DenyPaths) {
				sensitiveHits = appendUnique(sensitiveHits, f)
			}
		}
	}

	var out []PlanWarning
	if len(verifyHits) > 0 {
		out = append(out, PlanWarning{Kind: WarnVerifyConfig,
			Detail: "计划的**可写范围**覆盖验证配置文件：" + strings.Join(verifyHits, "、") +
				"（任何改动这类文件的写操作都不受策略拦截，只有你能拦；请确认需求本身要求改测试/校验配置）"})
	}
	if len(sensitiveHits) > 0 {
		out = append(out, PlanWarning{Kind: WarnSensitive,
			Detail: "计划的**可写范围**覆盖敏感路径：" + strings.Join(sensitiveHits, "、")})
	}
	if len(plan.Scope.AllowedPaths) == 0 {
		out = append(out, PlanWarning{Kind: WarnUnrestricted,
			Detail: "计划没有限定 allowed_paths：任何文件都可能被改，写入范围仅受全局策略限制"})
	}
	if listed == 0 {
		// 不能因为文件列表为空就说「没有风险」。模型经常不填 files，
		// 这时真正的改动范围是未知的，必须如实说明。
		out = append(out, PlanWarning{Kind: WarnNoFilesListed,
			Detail: "计划没有列出涉及文件：无法预判它会改哪些文件"})
	}
	return out
}

// writable 报告某个文件是否落在计划声明的可写范围内。
// 未声明 allowed_paths 时视为「都可能被改」——这正是需要提示的情形。
func writable(rel string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	return matchesAny(rel, allowed)
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

func normalizePlanPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	return strings.TrimPrefix(p, "./")
}

func matchesAny(rel string, patterns []string) bool {
	for _, pat := range patterns {
		if pat != "" && policy.GlobMatch(rel, pat) {
			return true
		}
	}
	return false
}

// Lines 把计划渲染成给人看的文本行。调用方负责决定缩进与去向
// （`task status` 直接打印，`report.md` 写成章节）。
func (r PlanReview) Lines() []string {
	if r.Empty {
		return nil
	}
	var out []string
	out = append(out, "摘要: "+orDash(r.Summary))

	if len(r.Steps) > 0 {
		out = append(out, "步骤:")
		for i, s := range r.Steps {
			desc := s.Description
			if id := s.ID; id != "" {
				desc = "[" + id + "] " + desc
			}
			if len(s.Files) > 0 {
				desc += "（文件：" + strings.Join(s.Files, "、") + "）"
			}
			out = append(out, fmt.Sprintf("  %d. %s", i+1, desc))
		}
	}
	if len(r.AllowedPaths) > 0 {
		out = append(out, "可改动范围: "+strings.Join(r.AllowedPaths, "、"))
	}
	if len(r.Acceptance) > 0 {
		out = append(out, "验收标准:")
		for _, a := range r.Acceptance {
			out = append(out, "  ["+a.ID+"] "+a.Description+" — "+describeCheck(a.Check))
		}
	}
	if len(r.RiskFlags) > 0 {
		out = append(out, "风险标记: "+strings.Join(r.RiskFlags, "、"))
	}
	if r.EstimatedCostUSD > 0 {
		// 这个数字来自模型自己的 estimated_cost_usd，系统从不校验也从不覆盖它。
		// 实测出现过把预算（$1.00）填进来的情况，而真实花费是 $0.0045——直接把
		// 它当成「预估花费」展示等于给审批者一个已知不可靠的数字。标注来源，
		// 让他知道该信多少。
		out = append(out, fmt.Sprintf("预估花费: $%.4f（模型自估，未经校准，仅供参考）", r.EstimatedCostUSD))
	}
	for _, w := range r.Warnings {
		out = append(out, "⚠ "+w.Detail)
	}
	return out
}

func describeCheck(c planning.Check) string {
	switch c.Type {
	case planning.CheckCommand, planning.CheckTest:
		if c.Name == "" {
			return string(c.Type)
		}
		if c.Selector != "" {
			return fmt.Sprintf("%s %s %s", c.Type, c.Name, c.Selector)
		}
		return fmt.Sprintf("%s %s", c.Type, c.Name)
	case planning.CheckGrep:
		return fmt.Sprintf("grep %s %q (expect=%s)", c.Path, c.Pattern, c.Expect)
	case planning.CheckFileExists:
		return "file_exists " + c.Path
	default:
		return string(c.Type)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
