package report

import (
	"encoding/json"
	"strings"
	"testing"

	"agentdev/internal/config"
	"agentdev/internal/planning"
)

func planJSON(t *testing.T, p planning.Plan) string {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func repoCfg() config.RepoConfig {
	return config.RepoConfig{
		VerifyConfigFiles: []string{"test_*.py", "*_test.go"},
		SensitivePaths:    []string{".github/workflows/**"},
	}
}

func kinds(r PlanReview) []string {
	out := make([]string, 0, len(r.Warnings))
	for _, w := range r.Warnings {
		out = append(out, w.Kind)
	}
	return out
}

func hasKind(r PlanReview, kind string) bool {
	for _, w := range r.Warnings {
		if w.Kind == kind {
			return true
		}
	}
	return false
}

// 审批最该拦的一件事：计划自己要改验证配置文件。
// 在这之前 plan_json 没有任何渲染路径，这条信息对审批者完全不可见。
func TestReviewPlanFlagsVerifyConfigEdits(t *testing.T) {
	r := ReviewPlan(planJSON(t, planning.Plan{
		Status:  planning.StatusReady,
		Summary: "让测试通过",
		Scope:   planning.Scope{AllowedPaths: []string{"**"}},
		Steps: []planning.Step{{
			ID: "s1", Description: "放宽断言", Files: []string{"test_session.py"},
		}},
	}), repoCfg(), config.PolicyConfig{})

	if !hasKind(r, WarnVerifyConfig) {
		t.Fatalf("应标出改动验证配置文件，实际 %v", kinds(r))
	}
	if r.Empty {
		t.Error("有计划内容时不应标记为空")
	}
	lines := strings.Join(r.Lines(), "\n")
	if !strings.Contains(lines, "test_session.py") || !strings.Contains(lines, "验证配置文件") {
		t.Errorf("渲染应写明具体文件与原因:\n%s", lines)
	}
}

func TestReviewPlanFlagsSensitivePaths(t *testing.T) {
	r := ReviewPlan(planJSON(t, planning.Plan{
		Status: planning.StatusReady, Summary: "改 CI",
		Scope: planning.Scope{AllowedPaths: []string{"**"}},
		Steps: []planning.Step{{
			ID: "s1", Description: "改流水线", Files: []string{".github/workflows/ci.yml"},
		}},
	}), repoCfg(), config.PolicyConfig{})

	if !hasKind(r, WarnSensitive) {
		t.Fatalf("应标出敏感路径，实际 %v", kinds(r))
	}
}

// 真实输出里的误报：step「运行 python -m unittest test_core.py 验证修复」
// 会把 test_core.py 列进 files。它不是「要改的文件」——allowed_paths 只有 calc.py，
// 写它会被策略拦住。据此报「计划要改测试文件」是纯噪音。
func TestReviewPlanDoesNotFlagTestFilesOutsideScope(t *testing.T) {
	r := ReviewPlan(planJSON(t, planning.Plan{
		Status: planning.StatusReady, Summary: "修 calc",
		Scope: planning.Scope{AllowedPaths: []string{"calc.py"}},
		Steps: []planning.Step{
			{ID: "s1", Description: "改 add", Files: []string{"calc.py"}},
			{ID: "s2", Description: "运行测试验证", Files: []string{"test_core.py"}},
		},
	}), repoCfg(), config.PolicyConfig{})

	if hasKind(r, WarnVerifyConfig) {
		t.Errorf("可写范围外的测试文件只是被运行，不应报成「要改测试」: %v", kinds(r))
	}
	if hasKind(r, WarnSensitive) {
		t.Errorf("不应报敏感路径: %v", kinds(r))
	}
}

// 但可写范围**覆盖**测试文件时必须报：这时策略不会拦，只有审批者能拦。
func TestReviewPlanFlagsTestFilesInsideScope(t *testing.T) {
	r := ReviewPlan(planJSON(t, planning.Plan{
		Status: planning.StatusReady, Summary: "放宽断言",
		Scope: planning.Scope{AllowedPaths: []string{"**"}},
		Steps: []planning.Step{{
			ID: "s1", Description: "改测试", Files: []string{"test_session.py"},
		}},
	}), repoCfg(), config.PolicyConfig{})

	if !hasKind(r, WarnVerifyConfig) {
		t.Fatalf("可写范围覆盖测试文件时必须提醒，实际 %v", kinds(r))
	}
	if !strings.Contains(strings.Join(r.Lines(), "\n"), "test_session.py") {
		t.Error("渲染应写明具体文件")
	}
}

// 未限定 allowed_paths 时，列出的测试文件都可能被改，必须报。
func TestReviewPlanFlagsTestFilesWhenScopeUnrestricted(t *testing.T) {
	r := ReviewPlan(planJSON(t, planning.Plan{
		Status: planning.StatusReady, Summary: "x",
		Steps: []planning.Step{{ID: "s1", Description: "改", Files: []string{"test_a.py"}}},
	}), repoCfg(), config.PolicyConfig{})

	if !hasKind(r, WarnVerifyConfig) {
		t.Fatalf("范围未限定时应报警，实际 %v", kinds(r))
	}
	if !hasKind(r, WarnUnrestricted) {
		t.Errorf("应同时提示范围未限定，实际 %v", kinds(r))
	}
}

// 模型经常不填 steps[].files。此时改动范围是**未知**，不能因此说「没有风险」。
func TestReviewPlanSaysFilesUnknownWhenNotListed(t *testing.T) {
	r := ReviewPlan(planJSON(t, planning.Plan{
		Status: planning.StatusReady, Summary: "修一下",
		Scope: planning.Scope{AllowedPaths: []string{"src/**"}},
		Steps: []planning.Step{{ID: "s1", Description: "改点东西"}},
	}), repoCfg(), config.PolicyConfig{})

	if !hasKind(r, WarnNoFilesListed) {
		t.Fatalf("未列文件时应明确说明无法预判，实际 %v", kinds(r))
	}
}

func TestReviewPlanFlagsUnrestrictedScope(t *testing.T) {
	r := ReviewPlan(planJSON(t, planning.Plan{
		Status: planning.StatusReady, Summary: "x",
	}), repoCfg(), config.PolicyConfig{})
	if !hasKind(r, WarnUnrestricted) {
		t.Fatalf("未限定 allowed_paths 时应提示，实际 %v", kinds(r))
	}
}

// 没有计划时不能渲染出半截章节：审批门之前调用这个函数必须安全。
func TestReviewPlanEmptyAndUnparsable(t *testing.T) {
	for name, input := range map[string]string{
		"空字符串":   "",
		"只有空格":   "   ",
		"解析失败":   "{not json",
		"零值计划对象": `{"status":"ready"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := ReviewPlan(input, repoCfg(), config.PolicyConfig{})
			if !r.Empty {
				t.Errorf("应标记为空计划，实际 %+v", r)
			}
			if lines := r.Lines(); len(lines) != 0 {
				t.Errorf("空计划不应产生渲染行: %v", lines)
			}
		})
	}
	// 解析失败要单独说清原因，否则审批者会以为「还没规划」。
	r := ReviewPlan("{not json", repoCfg(), config.PolicyConfig{})
	if !hasKind(r, WarnUnparsable) {
		t.Errorf("解析失败应给出警告，实际 %v", kinds(r))
	}
}

// 渲染要包含审批者真正需要的四件事：摘要、步骤、范围、验收标准。
func TestPlanReviewLinesCoverWhatApproverNeeds(t *testing.T) {
	r := ReviewPlan(planJSON(t, planning.Plan{
		Status:  planning.StatusReady,
		Summary: "修复过期语义",
		Scope:   planning.Scope{AllowedPaths: []string{"clock.py", "session.py"}},
		Steps: []planning.Step{
			{ID: "s1", Description: "改过期判定", Files: []string{"clock.py"}},
		},
		Acceptance: []planning.Acceptance{{
			ID: "ac1", Description: "满 ttl 即过期",
			Check: planning.Check{Type: planning.CheckTest, Name: "test", Selector: "-run TestExpiry"},
		}},
		RiskFlags:        []string{"touches-auth"},
		EstimatedCostUSD: 0.02,
	}), repoCfg(), config.PolicyConfig{})

	got := strings.Join(r.Lines(), "\n")
	for _, want := range []string{
		"修复过期语义", "改过期判定", "clock.py", "session.py",
		"ac1", "满 ttl 即过期", "test -run TestExpiry",
		"touches-auth", "$0.0200",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("渲染缺少 %q:\n%s", want, got)
		}
	}
}
