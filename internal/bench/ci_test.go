package bench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/state"
	"agentdev/internal/store"
	"agentdev/internal/testutil"
)

// newBareRemote 建一个裸仓库充当远端，用来验证分支推送与删除，
// 不需要网络与 GitHub 凭据。
func newBareRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, dir, "init", "--bare", "-b", "main")
	return dir
}

// newSourceRepo 建一个含 fixture 的本地仓库，等价于 runCase 里 initRepo 之后的状态。
func newSourceRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "calc.py"), []byte("def add(a, b):\n    return a - b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ci := CIRemote{}
	if err := writeCIWorkflow(dir, ci.withDefaults()); err != nil {
		t.Fatal(err)
	}
	if err := initRepo(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPushBaselineCreatesIsolatedBranchWithWorkflow(t *testing.T) {
	remote := newBareRemote(t)
	repo := newSourceRepo(t)
	ci := CIRemote{Repo: "acme/demo", RemoteURL: remote, Cleanup: true}.withDefaults()

	branch, err := pushBaseline(repo, "py-add", 1, ci)
	if err != nil {
		t.Fatalf("推送基线失败: %v", err)
	}
	if branch != "bench/py-add" {
		t.Fatalf("基线分支名不符: %s", branch)
	}
	// 远端必须有该分支，且默认分支（main）完全没被推送：用例不能改动默认分支。
	refs := testutil.Git(t, remote, "branch", "--list")
	if !strings.Contains(refs, "bench/py-add") {
		t.Fatalf("远端缺少基线分支: %s", refs)
	}
	if strings.Contains(refs, "main") {
		t.Fatalf("默认分支不应被推送到远端: %s", refs)
	}
	workflow := testutil.Git(t, remote, "show", "bench/py-add:.github/workflows/ci.yml")
	if !strings.Contains(workflow, "pull_request") {
		t.Fatalf("CI 定义应只监听 pull_request，避免同一提交算两次:\n%s", workflow)
	}
	if !strings.Contains(workflow, "python -m unittest -q") || !strings.Contains(workflow, "go test ./...") {
		t.Fatalf("CI 定义应覆盖各语言入口，与本地验证命令一致:\n%s", workflow)
	}
}

func TestPushBaselineSuffixesRepeatedRuns(t *testing.T) {
	remote := newBareRemote(t)
	ci := CIRemote{Repo: "acme/demo", RemoteURL: remote}.withDefaults()

	first, err := pushBaseline(newSourceRepo(t), "py-add", 1, ci)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pushBaseline(newSourceRepo(t), "py-add", 2, ci)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("重复采样必须使用不同基线分支，实际都是 %s", first)
	}
	if !strings.HasSuffix(second, "-run2") {
		t.Fatalf("第 2 轮基线分支应带轮次后缀: %s", second)
	}
}

func TestCleanupDeletesBothBranches(t *testing.T) {
	remote := newBareRemote(t)
	repo := newSourceRepo(t)
	ci := CIRemote{Repo: "acme/demo", RemoteURL: remote, Cleanup: true}.withDefaults()

	base, err := pushBaseline(repo, "py-add", 1, ci)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 Agent 分支已推送。
	testutil.Git(t, repo, "branch", "agent/t1-py-add")
	testutil.Git(t, repo, "push", "origin", "agent/t1-py-add")

	cleanupCase(context.Background(), repo, "agent/t1-py-add", base, 0, ci, nil)

	refs := testutil.Git(t, remote, "branch", "--list")
	if strings.Contains(refs, "bench/py-add") || strings.Contains(refs, "agent/t1-py-add") {
		t.Fatalf("清理后远端不应残留分支: %s", refs)
	}
}

func TestCIVerdictDistinguishesPassedFromNoCI(t *testing.T) {
	cases := []struct {
		name   string
		events []store.Event
		want   string
	}{
		{
			name:   "CI 通过",
			events: []store.Event{{Type: "state_changed", ToStatus: string(state.Done), Payload: map[string]any{"ci": "passed"}}},
			want:   "passed",
		},
		{
			name:   "没有配置 CI 不是通过",
			events: []store.Event{{Type: "state_changed", ToStatus: string(state.Done), Payload: map[string]any{"ci": "none"}}},
			want:   "none",
		},
		{
			name:   "未开启监听",
			events: []store.Event{{Type: "state_changed", ToStatus: string(state.Done), Payload: map[string]any{"ci": "skipped"}}},
			want:   "unknown",
		},
		{
			name:   "没有 Done 事件",
			events: []store.Event{{Type: "state_changed", ToStatus: string(state.NeedsHuman)}},
			want:   "unknown",
		},
		{
			name: "CI 失败后挂起也算失败，不能退化成未知",
			events: []store.Event{
				{Type: "ci_failed"},
				{Type: "state_changed", ToStatus: string(state.NeedsHuman)},
			},
			want: "failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ciVerdict(tc.events); got != tc.want {
				t.Fatalf("ciVerdict = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

func TestSummarizeCIFirstPassRate(t *testing.T) {
	outcomes := []Outcome{
		{ID: "a", Status: "DONE", Passed: true, CIVerdict: "passed", CIFirstPass: true},
		{ID: "b", Status: "DONE", Passed: true, CIVerdict: "failed", CIAttempts: 1},
		{ID: "c", Status: "DONE", Passed: true, CIVerdict: "none"},
		{ID: "d", Status: "DONE", Passed: true, CIVerdict: "unknown"},
		{ID: "e", Status: "NEEDS_HUMAN", CIVerdict: "failed", Blocked: true},
	}
	s := summarize(outcomes, 5)

	if s.CIRuns != 2 {
		t.Fatalf("CIRuns = %d, 期望 2（只统计真的跑过检查的交付）", s.CIRuns)
	}
	if s.CIFirstPass != 1 {
		t.Fatalf("CIFirstPass = %d, 期望 1", s.CIFirstPass)
	}
	if s.CIFirstPassRate != 0.5 {
		t.Fatalf("CIFirstPassRate = %v, 期望 0.5", s.CIFirstPassRate)
	}
	if s.CINone != 1 {
		t.Fatalf("CINone = %d, 期望 1（没有 CI 必须单独披露）", s.CINone)
	}
	if s.Blocked != 1 {
		t.Fatalf("Blocked = %d, 期望 1", s.Blocked)
	}
}

func TestSummarizeCIFirstPassRateWithoutCIRuns(t *testing.T) {
	// 全部交付都没有跑过 CI 时，比率必须保持 0 且 CIRuns 为 0，
	// 不能靠“0/0”制造出一个看似达标的数字。
	s := summarize([]Outcome{{ID: "a", Status: "DONE", Passed: true, CIVerdict: "none"}}, 1)
	if s.CIRuns != 0 || s.CIFirstPassRate != 0 {
		t.Fatalf("无 CI 时不应产生通过率: runs=%d rate=%v", s.CIRuns, s.CIFirstPassRate)
	}
}

func TestSanitizeBranch(t *testing.T) {
	if got := sanitizeBranch("py/slugify #2"); got != "py-slugify--2" {
		t.Fatalf("sanitizeBranch = %q", got)
	}
	if got := sanitizeBranch(""); got != "case" {
		t.Fatalf("空 id 应退化为 case，实际 %q", got)
	}
}
