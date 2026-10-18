package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/state"
	"agentdev/internal/store"
)

func TestGitHubRepoSlugParsing(t *testing.T) {
	cases := []struct {
		remote string
		want   string
	}{
		{"git@github.com:acme/app.git", "acme/app"},
		{"https://github.com/acme/app", "acme/app"},
		{"https://github.com/acme/app.git", "acme/app"},
		{"ssh://git@github.com/acme/app.git", "acme/app"},
	}
	for _, c := range cases {
		got, ok := parseGitHubSlug(c.remote)
		if !ok || got != c.want {
			t.Errorf("parseGitHubSlug(%q) = %q, %v; want %q", c.remote, got, ok, c.want)
		}
	}
	for _, remote := range []string{"https://gitlab.com/acme/app.git", "origin", "https://github.com/acme"} {
		if _, ok := parseGitHubSlug(remote); ok {
			t.Errorf("non-GitHub remote should fail: %q", remote)
		}
	}
}

func TestTaskCreateRejectsAmbiguousIssueAndTextSources(t *testing.T) {
	home := writeHome(t, "not-used")
	code, _, errOut := invoke(t, "task", "create", "--repo", "acme/app", "--text", "x", "--issue", "1", "--home", home)
	if code != ExitUsage || !strings.Contains(errOut, "只能指定一个") {
		t.Errorf("--text 与 --issue 应互斥 (%d): %s", code, errOut)
	}
	code, _, errOut = invoke(t, "task", "create", "--repo", "acme/app", "--issue", "bad", "--home", home)
	if code != ExitUsage || !strings.Contains(errOut, "正整数") {
		t.Errorf("Issue 编号必须是正整数 (%d): %s", code, errOut)
	}
}

func TestCostReportAggregatesAcrossTasks(t *testing.T) {
	home := writeHome(t, "not-used")
	s, err := store.Open(filepath.Join(home, "agentdev.db"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateTask(store.NewTask{Repo: "acme/app", Requirement: "a", BudgetUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTask(store.NewTask{Repo: "acme/app", Requirement: "b", BudgetUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCost(a.ID, "coder", "model-a", store.CostEntry{InputTokens: 10, OutputTokens: 2, CostUSD: 0.01}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCost(b.ID, "reviewer", "model-b", store.CostEntry{InputTokens: 20, OutputTokens: 3, CostUSD: 0.02}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(b.ID, "cost_pricing_missing", map[string]any{"model": "model-b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := invoke(t, "cost", "report", "--home", home)
	if code != ExitOK {
		t.Fatalf("cost report 失败 (%d): %s", code, errOut)
	}
	for _, want := range []string{"2 个任务", "2 次模型调用", "$0.030000", "[WARN]", "model-b", "按角色", "coder", "reviewer", "按模型", "model-a"} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard missing %q:\n%s", want, out)
		}
	}
}

func TestResumeResetsAllM2AttemptCounters(t *testing.T) {
	home := writeHome(t, "not-used")
	s, err := store.Open(filepath.Join(home, "agentdev.db"))
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(store.NewTask{Repo: "acme/app", Requirement: "x", BudgetUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []state.Status{state.Triaging, state.Planning, state.Coding, state.Verifying, state.Reviewing} {
		task, err = s.Transition(task.ID, task.Version, next, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	task, err = s.Transition(task.ID, task.Version, state.NeedsHuman,
		map[string]any{"reason": "达到尝试上限"},
		map[string]any{"fix_attempts": 3, "review_attempts": 2, "ci_attempts": 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := invoke(t, "task", "resume", task.ID, "--home", home)
	if code != ExitOK {
		t.Fatalf("resume failed (%d): %s", code, errOut)
	}
	s, err = store.Open(filepath.Join(home, "agentdev.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	resumed, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != state.Reviewing || resumed.FixAttempts != 0 || resumed.ReviewAttempts != 0 || resumed.CIAttempts != 0 {
		t.Errorf("resume 应恢复状态并重置所有计数: %+v", resumed)
	}
}
