package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/backend"
	"agentdev/internal/config"
	"agentdev/internal/cost"
	"agentdev/internal/delivery"
	"agentdev/internal/llm"
	"agentdev/internal/planning"
	"agentdev/internal/state"
	"agentdev/internal/store"
	"agentdev/internal/testutil"
)

// ---- 测试替身 ----

type stubPlanner struct {
	plans []planning.Plan
	err   error
	i     int
}

func (s *stubPlanner) Plan(ctx context.Context, req planning.Requirement) (planning.Plan, error) {
	if s.err != nil {
		return planning.Plan{}, s.err
	}
	if s.i >= len(s.plans) {
		return planning.StaticPlanner{}.Plan(ctx, req)
	}
	p := s.plans[s.i]
	s.i++
	return p, nil
}

func setupEcho(t *testing.T) *fixture {
	f := setup(t)
	f.cfg.Repos["acme/app"] = config.RepoConfig{
		LocalPath:      f.repo,
		DefaultBranch:  "main",
		Remote:         "origin",
		VerifyCommands: config.VerifyCommands{{Name: "noop", Command: "echo ok"}},
		BudgetUSD:      2,
	}
	return f
}

// addBareRemote 为仓库配置一个本地裸仓库作为 origin，用于验证推送行为。
func addBareRemote(t *testing.T, repoPath string) string {
	t.Helper()
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	cmd := exec.Command("git", "init", "--bare", bare)
	cmd.Dir = base
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("创建裸仓库失败: %v\n%s", err, out)
	}
	testutil.Git(t, repoPath, "remote", "add", "origin", bare)
	return bare
}

func remoteHasBranch(t *testing.T, bare, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", bare, "rev-parse", "--verify", "refs/heads/"+branch)
	return cmd.Run() == nil
}

func eventsOfType(t *testing.T, st *store.SQLiteStore, taskID, kind string) []store.Event {
	t.Helper()
	events, err := st.ListEvents(taskID)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Event
	for _, e := range events {
		if e.Type == kind {
			out = append(out, e)
		}
	}
	return out
}

// ---- 预算熔断 ----

// usageServer 每次调用都返回 write_file，并按配置返回 token 用量。
func usageServer(t *testing.T, inputTokensPerCall int) *httptest.Server {
	t.Helper()
	var calls int
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		usage := map[string]any{"input_tokens": inputTokensPerCall, "output_tokens": 0}
		if calls >= 3 {
			payload := map[string]any{
				"content":     []any{map[string]any{"type": "text", "text": "完成"}},
				"stop_reason": "end_turn", "usage": usage,
			}
			_ = json.NewEncoder(w).Encode(payload)
			return
		}
		payload := map[string]any{
			"content": []any{map[string]any{
				"type": "tool_use", "id": "tu", "name": "write_file",
				"input": map[string]any{"path": "hello.txt", "content": "hello\n"},
			}},
			"stop_reason": "tool_use", "usage": usage,
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
}

func TestBudgetWarningThenCircuitBreaker(t *testing.T) {
	f := setup(t)
	// 预算 $10，每轮 $8：第一轮触发预警，第二轮触发熔断。
	task := f.taskWithBudget("S", 10)
	srv := usageServer(t, 800_000)
	defer srv.Close()

	client := llm.NewClient(srv.URL, "k")
	orch := New(f.st, f.cfg, backend.NewAnthropic(client, "m", 1024),
		WithModel(client, "m", 1024, cost.Table{"m": {Input: 10}}))

	done, err := orch.RunTask(task.ID)
	if err != nil {
		t.Fatalf("RunTask 报错: %v", err)
	}
	if done.Status != state.NeedsHuman {
		t.Fatalf("超预算应挂起等待人工，实际 %s（%s）", done.Status, lastReason(t, f.st, task.ID))
	}
	if r := lastReason(t, f.st, task.ID); !strings.Contains(r, "预算") {
		t.Errorf("挂起原因应说明预算: %s", r)
	}
	if len(eventsOfType(t, f.st, task.ID, "budget_warning")) != 1 {
		t.Error("应先发出一次预算预警")
	}
	if done.SpentUSD < 15.9 || done.SpentUSD > 16.1 {
		t.Errorf("花费应为两轮共 $16，实际 %v", done.SpentUSD)
	}
	entries, err := f.st.ListCosts(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("应有 2 条计费记录，实际 %d", len(entries))
	}
	for _, e := range entries {
		if e.Role != "coder" || e.Model != "m" || e.CostUSD < 7.9 || e.CostUSD > 8.1 {
			t.Errorf("计费记录异常: %+v", e)
		}
	}
}

func TestUnknownPricingIsReportedNotSilentlyFree(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	srv := usageServer(t, 1000)
	defer srv.Close()

	client := llm.NewClient(srv.URL, "k")
	orch := New(f.st, f.cfg, backend.NewAnthropic(client, "m", 1024),
		WithModel(client, "m", 1024, cost.Table{}))

	if _, err := orch.RunTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if len(eventsOfType(t, f.st, task.ID, "cost_pricing_missing")) != 1 {
		t.Error("未配置单价时必须显式记录事件，避免把未知当成免费")
	}
}

// ---- 交付：推送与草稿 PR ----

func TestDeliveryPushesBranchAndOpensDraftPR(t *testing.T) {
	f := setup(t)
	bare := addBareRemote(t, f.repo)
	f.cfg.Delivery = config.DeliveryConfig{Enabled: true, BranchPrefix: "agent/", DraftPR: true}

	task := f.task("S")
	host := &delivery.Fake{Result: delivery.PRResult{Number: 5, URL: "https://example.invalid/pull/5"}}
	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}), WithCodeHost(host)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.Done || done.PRURL != host.Result.URL || done.PRNumber != 5 {
		t.Fatalf("交付结果异常: status=%s pr=%q #%d", done.Status, done.PRURL, done.PRNumber)
	}
	if !remoteHasBranch(t, bare, done.Branch) {
		t.Error("分支应当已推送到远端")
	}
	if len(host.Requests) != 1 {
		t.Fatalf("应创建 1 个 PR，实际 %d", len(host.Requests))
	}
	req := host.Requests[0]
	if !req.Draft {
		t.Error("PR 必须是草稿")
	}
	if req.Base != "main" || req.Head != done.Branch {
		t.Errorf("PR 分支参数错误: %+v", req)
	}
	if !strings.Contains(req.Body, "不会自动合并") {
		t.Errorf("PR 正文应声明不自动合并: %s", req.Body)
	}
}

func TestSecretScanBlocksPush(t *testing.T) {
	f := setup(t)
	bare := addBareRemote(t, f.repo)
	f.cfg.Delivery = config.DeliveryConfig{Enabled: true, BranchPrefix: "agent/", DraftPR: true}

	task := f.task("S")
	leak := []backend.ToolCall{
		{Tool: "write_file", Args: map[string]any{"path": "hello.txt", "content": "hello\n"}},
		{Tool: "write_file", Args: map[string]any{
			"path":    "config/settings.py",
			"content": "API_KEY = \"sk-abcdefghijklmnopqrstuvwx\"\n",
		}},
	}
	host := &delivery.Fake{}
	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{leak}), WithCodeHost(host)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.NeedsHuman {
		t.Fatalf("命中密钥应挂起，实际 %s", done.Status)
	}
	if r := lastReason(t, f.st, task.ID); !strings.Contains(r, "密钥") {
		t.Errorf("原因应说明密钥扫描: %s", r)
	}
	if remoteHasBranch(t, bare, done.Branch) {
		t.Error("命中密钥时绝不能推送")
	}
	if len(host.Requests) != 0 {
		t.Error("命中密钥时不应创建 PR")
	}
	if len(eventsOfType(t, f.st, task.ID, "secret_scan_failed")) != 1 {
		t.Error("应记录 secret_scan_failed 事件")
	}
}

func TestDeliveryDisabledNeedsNoRemote(t *testing.T) {
	f := setup(t) // 仓库没有配置 origin
	task := f.task("S")
	done := f.run(backend.NewFake([][]backend.ToolCall{good}), task.ID)
	if done.Status != state.Done {
		t.Fatalf("未开启交付时应直接完成，实际 %s（%s）", done.Status, lastReason(t, f.st, task.ID))
	}
	if done.PRURL != "" {
		t.Error("未开启交付时不应有 PR")
	}
}

func TestMissingRemoteSuspendsAfterCommit(t *testing.T) {
	f := setup(t)
	f.cfg.Delivery = config.DeliveryConfig{Enabled: true, BranchPrefix: "agent/", DraftPR: true}
	task := f.task("S")
	done := f.run(backend.NewFake([][]backend.ToolCall{good}), task.ID)
	if done.Status != state.NeedsHuman {
		t.Fatalf("远端不存在时应挂起，实际 %s", done.Status)
	}
	if done.CommitSHA == "" {
		t.Error("挂起前应已本地提交，便于人工处理")
	}
	if r := lastReason(t, f.st, task.ID); !strings.Contains(r, "origin") {
		t.Errorf("原因应指出远端: %s", r)
	}
}

// ---- 计划与验收标准 ----

func TestNeedsInfoFlowThenReplyResumes(t *testing.T) {
	f := setup(t)
	task := f.taskWithRequirement("改进页面的提示文案")
	planner := &stubPlanner{plans: []planning.Plan{
		{Status: planning.StatusNeedsInfo, Questions: []string{"要改哪个页面？", "是否允许改动后端接口？"}},
		{Status: planning.StatusReady, Summary: "写 hello.txt",
			Acceptance: []planning.Acceptance{{ID: "ac1", Description: "hello.txt 存在",
				Check: planning.Check{Type: planning.CheckFileExists, Path: "hello.txt"}}}},
	}}
	orch := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}), WithPlanner(planner))

	paused, err := orch.RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != state.NeedsInfo {
		t.Fatalf("应进入 NEEDS_INFO，实际 %s", paused.Status)
	}
	events := eventsOfType(t, f.st, task.ID, "state_changed")
	last := events[len(events)-1]
	questions, _ := last.Payload["questions"].([]any)
	if len(questions) != 2 {
		t.Errorf("澄清问题应被记录: %+v", last.Payload)
	}
	if paused.PreviousStatus != state.Planning {
		t.Errorf("挂起前状态应为 PLANNING，实际 %s", paused.PreviousStatus)
	}

	// 模拟 reply：补全需求并回到 PLANNING。
	full, err := f.st.Transition(task.ID, paused.Version, state.Planning, nil,
		map[string]any{"requirement": "改进页面的提示文案\n\n[补充说明]\n只改前端页面"})
	if err != nil {
		t.Fatal(err)
	}
	final, err := orch.RunTask(full.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != state.Done {
		t.Fatalf("补充说明后应完成，实际 %s（%s）", final.Status, lastReason(t, f.st, task.ID))
	}
}

func TestPlannerFailureFallsBackToStaticPlan(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	orch := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}),
		WithPlanner(&stubPlanner{err: errPlanner}))
	done, err := orch.RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.Done {
		t.Fatalf("规划失败应降级而非中断，实际 %s（%s）", done.Status, lastReason(t, f.st, task.ID))
	}
	if len(eventsOfType(t, f.st, task.ID, "plan_failed")) != 1 {
		t.Error("应记录 plan_failed 事件")
	}
}

func TestAcceptanceFailureDrivesFixLoop(t *testing.T) {
	f := setupEcho(t)
	task := f.task("S")
	planner := &stubPlanner{plans: []planning.Plan{{
		Status: planning.StatusReady, Summary: "写两个文件",
		Acceptance: []planning.Acceptance{{
			ID: "ac1", Description: "extra.txt 存在",
			Check: planning.Check{Type: planning.CheckFileExists, Path: "extra.txt"},
		}},
	}}}
	script := [][]backend.ToolCall{
		good, // 只写 hello.txt：验证命令通过，但验收标准不满足
		{{Tool: "write_file", Args: map[string]any{"path": "extra.txt", "content": "x\n"}}},
	}
	done, err := New(f.st, f.cfg, backend.NewFake(script), WithPlanner(planner)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.Done || done.FixAttempts != 1 {
		t.Fatalf("应因验收标准未满足修复一次后完成: status=%s attempts=%d（%s）",
			done.Status, done.FixAttempts, lastReason(t, f.st, task.ID))
	}
	fixes := eventsOfType(t, f.st, task.ID, "fix_requested")
	if len(fixes) != 1 || !strings.Contains(fixes[0].Payload["failure"].(string), "ac1") {
		t.Errorf("修复请求应指向未满足的验收标准: %+v", fixes)
	}
	// 验收标准失败也必须影响 PR 正文中的验证结论。
	runs, _ := f.st.ListVerifications(task.ID)
	if len(runs) != 2 || runs[0].Passed || !runs[1].Passed {
		t.Fatalf("验证记录应为 [失败, 通过]: %+v", runs)
	}
	acceptance, ok := runs[0].Report["acceptance"].([]any)
	if !ok || len(acceptance) != 1 {
		t.Fatalf("失败轮次应记录验收标准结果: %+v", runs[0].Report)
	}
}

// ---- 分级与恢复 ----

func TestTriageEscalatesToApprovalWithoutCallingBackend(t *testing.T) {
	f := setup(t)
	// 未显式指定级别：规则命中高风险领域 → M → 需要审批。
	task := f.taskWithRequirement("修改 auth 模块的 token 校验逻辑")
	be := backend.NewFake([][]backend.ToolCall{good})
	done := f.run(be, task.ID)

	if done.Level != "M" || done.Status != state.AwaitingApproval {
		t.Fatalf("应分级为 M 并等待审批: level=%s status=%s", done.Level, done.Status)
	}
	if len(be.Calls) != 0 {
		t.Error("审批前不应调用后端")
	}
	var reasons []any
	for _, e := range eventsOfType(t, f.st, task.ID, "state_changed") {
		if e.ToStatus == string(state.Planning) {
			reasons, _ = e.Payload["reasons"].([]any)
		}
	}
	if len(reasons) == 0 {
		t.Errorf("分级理由应被记录: %+v", reasons)
	}
}

func TestResumeFromNeedsHumanContinues(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	// 后端先失败，任务挂起在 CODING。
	paused := f.run(errBackend{err: backend.Errorf("模型限流")}, task.ID)
	if paused.Status != state.NeedsHuman || paused.PreviousStatus != state.Coding {
		t.Fatalf("应挂起在 CODING: %s / %s", paused.Status, paused.PreviousStatus)
	}

	resumed, err := f.st.Transition(task.ID, paused.Version, state.Coding, map[string]any{"resumed_to": "CODING"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := f.run(backend.NewFake([][]backend.ToolCall{good}), resumed.ID)
	if done.Status != state.Done {
		t.Fatalf("恢复后应完成，实际 %s（%s）", done.Status, lastReason(t, f.st, task.ID))
	}
}

func TestRunTaskRejectsSuspendedTask(t *testing.T) {
	f := setup(t)
	task := f.taskWithRequirement("修改 auth 模块的 token 校验逻辑")
	f.run(backend.NewFake(nil), task.ID)
	if _, err := New(f.st, f.cfg, backend.NewFake(nil)).RunTask(task.ID); err == nil {
		t.Error("对挂起状态的任务直接 run 应当报错，提示先审批")
	}
}

func TestReportIncludesCostAndPR(t *testing.T) {
	f := setup(t)
	addBareRemote(t, f.repo)
	f.cfg.Delivery = config.DeliveryConfig{Enabled: true, BranchPrefix: "agent/", DraftPR: true}
	task := f.task("S")
	host := &delivery.Fake{Result: delivery.PRResult{Number: 9, URL: "https://example.invalid/pull/9"}}
	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}), WithCodeHost(host)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(f.cfg.RunsDir(), task.ID, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{"草稿 PR", host.Result.URL, "## 成本", "已用"} {
		if !strings.Contains(content, want) {
			t.Errorf("报告缺少 %q:\n%s", want, content)
		}
	}
	if done.Status != state.Done {
		t.Fatalf("状态应为 DONE: %s", done.Status)
	}
}

// ---- 非模型路径 ----

var errPlanner = &planningError{}

type planningError struct{}

func (e *planningError) Error() string { return "planner boom" }

var _ = io.Discard
