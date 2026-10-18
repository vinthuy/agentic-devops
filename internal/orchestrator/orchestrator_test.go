package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/backend"
	"agentdev/internal/config"
	"agentdev/internal/state"
	"agentdev/internal/store"
	"agentdev/internal/testutil"
)

var (
	good = []backend.ToolCall{{Tool: "write_file", Args: map[string]any{"path": "hello.txt", "content": "hello\n"}}}
	bad  = []backend.ToolCall{{Tool: "write_file", Args: map[string]any{"path": "hello.txt", "content": "bye\n"}}}
)

type fixture struct {
	t    *testing.T
	st   *store.SQLiteStore
	cfg  *config.App
	repo string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	repoPath := testutil.NewRepo(t)
	cfg := &config.App{
		Home: home,
		Repos: map[string]config.RepoConfig{
			"acme/app": {
				LocalPath:         repoPath,
				DefaultBranch:     "main",
				Remote:            "origin",
				VerifyCommands:    config.VerifyCommands{{Name: "check", Command: "go run check.go"}},
				VerifyConfigFiles: []string{"check.go"},
				BudgetUSD:         2,
			},
		},
		Policy: config.PolicyConfig{
			DenyPaths:           config.DefaultDenyPaths,
			DenyCommandPatterns: config.DefaultDenyCommandPatterns,
			MaxFixAttempts:      2,
			MaxTurnsPerStep:     10,
			ToolOutputMaxChars:  8000,
			CommandTimeoutS:     120,
		},
		Delivery:       config.DeliveryConfig{BranchPrefix: "agent/"},
		GitAuthorName:  "agentdev",
		GitAuthorEmail: "agentdev@localhost",
	}
	st, err := store.Open(filepath.Join(home, "agentdev.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &fixture{t: t, st: st, cfg: cfg, repo: repoPath}
}

func (f *fixture) task(level string) store.Task {
	f.t.Helper()
	task, err := f.st.CreateTask(store.NewTask{
		Repo:          "acme/app",
		Requirement:   "在仓库根目录创建 hello.txt，内容为 hello",
		BudgetUSD:     2,
		Level:         level,
		LevelOverride: level != "",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return task
}

func (f *fixture) taskWithRequirement(requirement string) store.Task {
	f.t.Helper()
	task, err := f.st.CreateTask(store.NewTask{
		Repo: "acme/app", Requirement: requirement, BudgetUSD: 2,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return task
}

func (f *fixture) taskWithBudget(level string, budget float64) store.Task {
	f.t.Helper()
	task, err := f.st.CreateTask(store.NewTask{
		Repo: "acme/app", Requirement: "在仓库根目录创建 hello.txt，内容为 hello",
		BudgetUSD: budget, Level: level, LevelOverride: level != "",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return task
}

func (f *fixture) run(be backend.ExecutionBackend, taskID string) store.Task {
	f.t.Helper()
	task, err := New(f.st, f.cfg, be).RunTask(taskID)
	if err != nil {
		f.t.Fatalf("RunTask 返回错误: %v", err)
	}
	return task
}

func lastReason(t *testing.T, st *store.SQLiteStore, taskID string) string {
	t.Helper()
	events, err := st.ListEvents(taskID)
	if err != nil {
		t.Fatal(err)
	}
	reason, _ := events[len(events)-1].Payload["reason"].(string)
	return reason
}

func TestHappyPathCommitsAndWritesReport(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	done := f.run(backend.NewFake([][]backend.ToolCall{good}), task.ID)

	if done.Status != state.Done {
		t.Fatalf("状态应为 DONE，实际 %s（%s）", done.Status, lastReason(t, f.st, task.ID))
	}
	if len(done.CommitSHA) != 40 || done.FixAttempts != 0 {
		t.Errorf("提交或修复次数异常: sha=%q attempts=%d", done.CommitSHA, done.FixAttempts)
	}
	if !strings.HasPrefix(done.Branch, "agent/"+task.ID) {
		t.Errorf("分支命名异常: %s", done.Branch)
	}
	log := testutil.Git(t, f.repo, "log", "--all", "--format=%s")
	if !strings.Contains(log, task.ID) {
		t.Errorf("提交信息应包含任务 ID: %s", log)
	}
	report, err := os.ReadFile(filepath.Join(f.cfg.RunsDir(), task.ID, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "DONE") || !strings.Contains(string(report), "`go run check.go`") {
		t.Errorf("报告内容不完整:\n%s", report)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.RunsDir(), task.ID, "events.jsonl")); err != nil {
		t.Errorf("events.jsonl 未生成: %v", err)
	}
}

func TestFixLoopRecoversAfterFailedVerification(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	done := f.run(backend.NewFake([][]backend.ToolCall{bad, good}), task.ID)

	if done.Status != state.Done || done.FixAttempts != 1 {
		t.Fatalf("应在一次修复后完成: status=%s attempts=%d", done.Status, done.FixAttempts)
	}
	vs, _ := f.st.ListVerifications(task.ID)
	if len(vs) != 2 || vs[0].Passed || !vs[1].Passed {
		t.Errorf("验证记录应为 [失败, 通过]: %+v", vs)
	}
}

func TestFixLimitLeadsToNeedsHuman(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	done := f.run(backend.NewFake([][]backend.ToolCall{bad, bad, bad}), task.ID)

	if done.Status != state.NeedsHuman {
		t.Fatalf("应进入 NEEDS_HUMAN，实际 %s", done.Status)
	}
	if done.FixAttempts != f.cfg.Policy.MaxFixAttempts {
		t.Errorf("修复次数应达到上限 %d，实际 %d", f.cfg.Policy.MaxFixAttempts, done.FixAttempts)
	}
	if r := lastReason(t, f.st, task.ID); !strings.Contains(r, "修复次数") {
		t.Errorf("原因异常: %s", r)
	}
}

func TestTamperingWithVerifyConfigNeedsHuman(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	tamper := []backend.ToolCall{{Tool: "write_file", Args: map[string]any{
		"path": "check.go", "content": "package main\n\nfunc main() {}\n",
	}}}
	done := f.run(backend.NewFake([][]backend.ToolCall{tamper}), task.ID)

	if done.Status != state.NeedsHuman {
		t.Fatalf("应进入 NEEDS_HUMAN，实际 %s", done.Status)
	}
	events, _ := f.st.ListEvents(task.ID)
	last := events[len(events)-1]
	if last.Payload["reason"] != "防作弊检查命中" {
		t.Errorf("原因异常: %v", last.Payload)
	}
	findings, _ := last.Payload["findings"].([]any)
	if len(findings) == 0 || !strings.Contains(findings[0].(string), "验证配置") {
		t.Errorf("应列出防作弊发现: %v", findings)
	}
}

func TestLevelLRejectedWithoutRunningBackend(t *testing.T) {
	f := setup(t)
	task := f.task("L")
	be := backend.NewFake(nil)
	done := f.run(be, task.ID)
	if done.Status != state.Rejected {
		t.Fatalf("应为 REJECTED，实际 %s", done.Status)
	}
	if len(be.Calls) != 0 {
		t.Error("L 级任务不应调用后端")
	}
}

func TestLevelMParksForApproval(t *testing.T) {
	f := setup(t)
	task := f.task("M")
	be := backend.NewFake([][]backend.ToolCall{good})
	done := f.run(be, task.ID)
	if done.Status != state.AwaitingApproval {
		t.Fatalf("M 级应停在 AWAITING_APPROVAL，实际 %s", done.Status)
	}
	if len(be.Calls) != 0 {
		t.Error("审批前不应调用后端")
	}
	// 审批后继续执行，直到完成。
	if _, err := f.st.Transition(task.ID, done.Version, state.Coding, map[string]any{"approved_by": "test"}, nil); err != nil {
		t.Fatal(err)
	}
	final := f.run(be, task.ID)
	if final.Status != state.Done {
		t.Fatalf("审批后应完成，实际 %s（%s）", final.Status, lastReason(t, f.st, task.ID))
	}
}

func TestPolicyViolationsBlockedThenTaskCompletes(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	script := [][]backend.ToolCall{
		{
			{Tool: "write_file", Args: map[string]any{"path": ".env", "content": "TOKEN=1\n"}},
			{Tool: "run_command", Args: map[string]any{"command": "rm -rf ."}},
		},
		good,
	}
	done := f.run(backend.NewFake(script), task.ID)
	if done.Status != state.Done {
		t.Fatalf("应当完成，实际 %s（%s）", done.Status, lastReason(t, f.st, task.ID))
	}
	vio, _ := f.st.ListViolations(task.ID)
	kinds := map[string]int{}
	for _, v := range vio {
		kinds[v.Kind]++
		if !v.Blocked {
			t.Errorf("违规应当被拦截: %+v", v)
		}
	}
	if kinds["path"] < 1 || kinds["command"] < 1 {
		t.Errorf("违规类型记录不全: %v", kinds)
	}
}

type errBackend struct{ err error }

func (b errBackend) Name() string { return "err" }
func (b errBackend) RunStep(backend.StepContext) (backend.StepResult, error) {
	return backend.StepResult{}, b.err
}

func TestRecoverableBackendErrorLeadsToNeedsHuman(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	done := f.run(errBackend{err: backend.Errorf("模型限流")}, task.ID)
	if done.Status != state.NeedsHuman {
		t.Fatalf("应为 NEEDS_HUMAN，实际 %s", done.Status)
	}
	if r := lastReason(t, f.st, task.ID); !strings.Contains(r, "限流") {
		t.Errorf("原因异常: %s", r)
	}
}

func TestUnexpectedErrorMarksTaskFailed(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	done := f.run(errBackend{err: errors.New("bug in backend")}, task.ID)
	if done.Status != state.Failed || done.FinishedAt == "" {
		t.Fatalf("应为 FAILED 且有结束时间，实际 %s / %q", done.Status, done.FinishedAt)
	}
}

func TestRunningNonQueuedTaskErrors(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	f.run(backend.NewFake([][]backend.ToolCall{good}), task.ID)
	if _, err := New(f.st, f.cfg, backend.NewFake(nil)).RunTask(task.ID); err == nil {
		t.Error("重复执行非 QUEUED 任务应当报错")
	}
}

func TestUnknownRepoFailsTask(t *testing.T) {
	f := setup(t)
	task, err := f.st.CreateTask(store.NewTask{Repo: "ghost/x", Requirement: "x", BudgetUSD: 1, Level: "S"})
	if err != nil {
		t.Fatal(err)
	}
	done := f.run(backend.NewFake(nil), task.ID)
	if done.Status != state.Failed {
		t.Errorf("应为 FAILED，实际 %s", done.Status)
	}
}
