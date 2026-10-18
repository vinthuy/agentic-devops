package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/backend"
	"agentdev/internal/config"
	"agentdev/internal/delivery"
	"agentdev/internal/planning"
	"agentdev/internal/state"
	"agentdev/internal/testutil"
)

// recordingPlanner 记录每次规划请求，便于断言重新规划时带上了反馈。
type recordingPlanner struct {
	plans []planning.Plan
	reqs  []planning.Requirement
}

func (p *recordingPlanner) Plan(ctx context.Context, req planning.Requirement) (planning.Plan, error) {
	p.reqs = append(p.reqs, req)
	if len(p.plans) == 0 {
		return planning.StaticPlanner{}.Plan(ctx, req)
	}
	plan := p.plans[0]
	p.plans = p.plans[1:]
	return plan, nil
}

func goTestRepo(t *testing.T, f *fixture) {
	t.Helper()
	repoPath := t.TempDir()
	testutil.Git(t, repoPath, "init", "-b", "main")
	testutil.Git(t, repoPath, "config", "user.name", "test")
	testutil.Git(t, repoPath, "config", "user.email", "test@example.com")
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(repoPath, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module fixture\n\ngo 1.21\n")
	write("x_test.go", "package fixture\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n")
	testutil.Git(t, repoPath, "add", "-A")
	testutil.Git(t, repoPath, "commit", "-m", "init")
	f.repo = repoPath
	f.cfg.Repos["acme/app"] = config.RepoConfig{
		LocalPath:      repoPath,
		DefaultBranch:  "main",
		Remote:         "origin",
		VerifyCommands: config.VerifyCommands{{Name: "test", Command: "go test ./..."}},
		BudgetUSD:      2,
	}
}

func invalidPlan(selector string) planning.Plan {
	return planning.Plan{
		Status:  planning.StatusReady,
		Summary: "写文件并运行指定测试",
		Scope:   planning.Scope{AllowedPaths: []string{"hello.txt"}},
		Steps:   []planning.Step{{ID: "s1", Description: "写 hello.txt", Files: []string{"hello.txt"}}},
		Acceptance: []planning.Acceptance{{
			ID: "ac1", Description: "指定测试通过",
			Check: planning.Check{Type: planning.CheckTest, Name: "test", Selector: selector},
		}},
	}
}

func validPlan() planning.Plan {
	return planning.Plan{
		Status:  planning.StatusReady,
		Summary: "写文件并运行测试",
		Scope:   planning.Scope{AllowedPaths: []string{"hello.txt"}},
		Steps:   []planning.Step{{ID: "s1", Description: "写 hello.txt", Files: []string{"hello.txt"}}},
		Acceptance: []planning.Acceptance{{
			ID: "ac1", Description: "测试命令通过",
			Check: planning.Check{Type: planning.CheckCommand, Name: "test"},
		}},
	}
}

// 主验证通过、只有 grep 断言匹配不到时，应如实归因为“验收标准可能写错”。
func TestAuxiliaryOnlyFailureReportsPlanIssue(t *testing.T) {
	f := setup(t)
	goTestRepo(t, f)
	task := f.task("S")
	plan := planning.Plan{
		Status:  planning.StatusReady,
		Summary: "写文件并运行测试",
		Scope:   planning.Scope{AllowedPaths: []string{"hello.txt"}},
		Steps:   []planning.Step{{ID: "s1", Description: "写 hello.txt", Files: []string{"hello.txt"}}},
		Acceptance: []planning.Acceptance{
			{ID: "ac-run", Description: "测试通过", Check: planning.Check{Type: planning.CheckCommand, Name: "test"}},
			{ID: "ac-grep", Description: "结构断言", Check: planning.Check{
				Type: planning.CheckGrep, Path: "hello.txt",
				Pattern: "这个模式永远不会匹配-xyzzy", Expect: "present",
			}},
		},
	}
	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good, good, good}),
		WithPlanner(&recordingPlanner{plans: []planning.Plan{plan}})).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.NeedsHuman {
		t.Fatalf("附加断言始终不满足应挂起，实际 %s", done.Status)
	}
	if reason := lastReason(t, f.st, task.ID); !strings.Contains(reason, "可能是验收标准本身写错") {
		t.Errorf("归因应指向验收标准而非代码: %q", reason)
	}
}

// 验收选择器匹配不到测试时，应回到规划并携带反馈，而不是空耗修复轮次。
func TestInvalidAcceptanceReplansWithFeedback(t *testing.T) {
	f := setup(t)
	goTestRepo(t, f)
	task := f.task("S")
	planner := &recordingPlanner{plans: []planning.Plan{invalidPlan("-run NoSuchTest"), validPlan()}}

	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good, good}),
		WithPlanner(planner)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.Done {
		t.Fatalf("重新规划后应完成，实际 %s", done.Status)
	}
	if len(planner.reqs) < 2 {
		t.Fatalf("应至少规划两次，实际 %d", len(planner.reqs))
	}
	if !strings.Contains(planner.reqs[1].Feedback, "不可执行") {
		t.Errorf("第二次规划应携带反馈，实际 %q", planner.reqs[1].Feedback)
	}
	if len(eventsOfType(t, f.st, task.ID, "acceptance_invalid")) != 1 {
		t.Error("应记录一次 acceptance_invalid")
	}
	if done.FixAttempts != 0 {
		t.Errorf("不可执行的验收标准不应消耗修复次数，实际 %d", done.FixAttempts)
	}
}

// 重新规划后仍不可执行时，应转人工而不是无限重规划。
func TestInvalidAcceptanceAfterReplanSuspends(t *testing.T) {
	f := setup(t)
	goTestRepo(t, f)
	task := f.task("S")

	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good, good, good}),
		WithPlanner(&recordingPlanner{plans: []planning.Plan{
			invalidPlan("-run NoSuchTest"), invalidPlan("-run StillMissing"),
		}})).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.NeedsHuman {
		t.Fatalf("仍不可执行时应挂起，实际 %s", done.Status)
	}
	if reason := lastReason(t, f.st, task.ID); !strings.Contains(reason, "仍不可执行") {
		t.Errorf("挂起原因应说明验收标准仍不可执行: %q", reason)
	}
	if len(eventsOfType(t, f.st, task.ID, "plan_replanned")) != 1 {
		t.Error("只应自动重规划一次")
	}
}

// G-01：推送前必须拒绝默认分支与非预期前缀。
func TestPushGuardRejectsProtectedBranches(t *testing.T) {
	cases := []struct {
		name string
		req  delivery.PushRequest
	}{
		{"默认分支 main", delivery.PushRequest{Branch: "main"}},
		{"默认分支大小写", delivery.PushRequest{Branch: "MASTER"}},
		{"配置的默认分支", delivery.PushRequest{Branch: "release/1.0", Protected: "release/1.0"}},
		{"缺少配置前缀", delivery.PushRequest{Branch: "feature/x", AllowedPrefix: "agent/"}},
		{"前缀不匹配", delivery.PushRequest{Branch: "agent/x", AllowedPrefix: "feature/"}},
		{"空分支", delivery.PushRequest{Branch: ""}},
	}
	for _, c := range cases {
		if err := delivery.EnsurePushable(c.req); err == nil {
			t.Errorf("%s: 应当拒绝推送", c.name)
		}
	}
	if err := delivery.EnsurePushable(delivery.PushRequest{
		Branch: "agent/t_20261018_abc123-fix", Protected: "main", AllowedPrefix: "agent/",
	}); err != nil {
		t.Errorf("合法的任务分支应允许推送: %v", err)
	}
}

// G-02：每次工具调用都要进入事件表，且不得记录文件正文。
func TestToolCallsAreAuditedWithoutContent(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	secretContent := "hello-secret-payload\n"
	script := []backend.ToolCall{{Tool: "write_file", Args: map[string]any{
		"path": "hello.txt", "content": secretContent,
	}}}
	if _, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{script})).RunTask(task.ID); err != nil {
		t.Fatal(err)
	}

	calls := eventsOfType(t, f.st, task.ID, "tool_call")
	if len(calls) == 0 {
		t.Fatal("应记录工具调用事件")
	}
	var found bool
	for _, e := range calls {
		if e.Payload["tool"] != "write_file" {
			continue
		}
		found = true
		if e.Payload["path"] != "hello.txt" {
			t.Errorf("应记录写入路径: %+v", e.Payload)
		}
		if e.Payload["ok"] != true {
			t.Errorf("成功调用应标记 ok: %+v", e.Payload)
		}
		raw, _ := json.Marshal(e.Payload)
		if strings.Contains(string(raw), "hello-secret-payload") {
			t.Errorf("事件不应包含文件正文: %s", raw)
		}
		if _, ok := e.Payload["bytes"]; !ok {
			t.Errorf("应记录字节数而非内容: %+v", e.Payload)
		}
	}
	if !found {
		t.Errorf("没有找到 write_file 调用记录: %+v", calls)
	}
}

// G-03：提交信息包含任务 ID，PR 正文包含需求、验收矩阵、成本与报告路径。
func TestCommitMessageAndPRBodyContent(t *testing.T) {
	f := setup(t)
	task := f.task("S")

	message := commitMessage(task)
	if !strings.Contains(message, task.ID) {
		t.Errorf("提交信息应包含任务 ID: %q", message)
	}
	if !strings.Contains(message, "\n\nTask: "+task.ID) {
		t.Errorf("提交信息应有任务 trailer: %q", message)
	}

	plan := planning.Plan{
		Status:  planning.StatusReady,
		Summary: "把 + 号加入邮箱字符类",
		Scope:   planning.Scope{AllowedPaths: []string{"hello.txt"}},
		Steps:   []planning.Step{{ID: "s1", Description: "改文件", Files: []string{"hello.txt"}}},
		Acceptance: []planning.Acceptance{{
			ID: "ac1", Description: "测试通过",
			Check: planning.Check{Type: planning.CheckCommand, Name: "check"},
		}},
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	task.PlanJSON = string(planJSON)
	task.Source = "issue"
	task.SourceRef = "https://github.com/acme/app/issues/7"
	task.SpentUSD = 0.0123

	if err := f.st.RecordVerification(task.ID, 0, true, map[string]any{
		"passed": true,
		"outcomes": []any{map[string]any{
			"command": "go run check.go", "name": "check", "exit_code": float64(0),
		}},
		"acceptance": []any{map[string]any{
			"id": "ac1", "description": "测试通过", "passed": true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.AppendEvent(task.ID, "plan_ready", map[string]any{"source": "model"}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.AppendEvent(task.ID, "review_result", map[string]any{
		"decision": "approve", "summary": "改动符合需求",
	}); err != nil {
		t.Fatal(err)
	}

	body := prBody(f.st, f.cfg, task)
	for _, want := range []string{
		task.ID,
		task.SourceRef,
		"把 + 号加入邮箱字符类",
		"hello.txt",
		"| 验收标准 | 结果 |",
		"满足",
		"`go run check.go`",
		"$0.0123",
		"运行报告",
		"模型生成",
		"approve",
		"不会自动合并",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR 正文缺少 %q:\n%s", want, body)
		}
	}
}
