package bench

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agentdev/internal/delivery"
	"agentdev/internal/workspace"
)

// CIRemote 让基准用例在真实远端仓库上运行，从而用**真实 CI** 判定一次交付是否
// 首次即通过（PRD 的 CI 首次通过率）。
//
// 每个用例把自己的 fixture 提交到一个独立基线分支，并让该分支成为这个用例
// 的“默认分支”：
//
//	bench/<case>[-runN]   ← fixture + 通用 CI 定义（本地验证与 CI 看到的代码一致）
//	        ↑ PR base
//	agent/<task-id>-<slug> ← Agent 的改动，由 delivery 推送并开草稿 PR
//
// 因此用例之间互不可见，仓库的默认分支也不会被改动。跑完可关闭 PR 并删除两条
// 分支（Cleanup），避免 20 个用例在仓库里留下 20 个实验性 PR。
type CIRemote struct {
	// Repo 是 owner/name，用于推导远端地址与输出。
	Repo string
	// RemoteURL 覆盖推导出的远端地址（测试可指向本地裸仓库）。
	RemoteURL string
	// RemoteName 是本地 git 的远端名，默认 origin。
	RemoteName string
	// BasePrefix 是基线分支前缀，默认 bench/。
	BasePrefix string
	// WorkflowPath 是 CI 定义在仓库中的路径，默认 .github/workflows/ci.yml。
	WorkflowPath string
	// Workflow 是写入基线分支的 CI 定义；为空时使用 defaultCIWorkflow。
	Workflow string
	// Cleanup 为 true 时，用例结束后关闭 PR 并删除远端分支。
	Cleanup bool
	// PollSeconds 是 CI 轮询间隔，默认 15。
	PollSeconds int
	// TimeoutMinutes 是单个用例等待 CI 的上限，默认 10。
	TimeoutMinutes int
	// GhPath 是 gh 可执行文件，空则用 PATH。
	GhPath string
}

func (c CIRemote) withDefaults() CIRemote {
	if c.RemoteName == "" {
		c.RemoteName = "origin"
	}
	if c.BasePrefix == "" {
		c.BasePrefix = "bench/"
	}
	if c.WorkflowPath == "" {
		c.WorkflowPath = ".github/workflows/ci.yml"
	}
	if c.Workflow == "" {
		c.Workflow = defaultCIWorkflow
	}
	if c.PollSeconds <= 0 {
		c.PollSeconds = 15
	}
	if c.TimeoutMinutes <= 0 {
		c.TimeoutMinutes = 10
	}
	return c
}

func (c CIRemote) remoteURL() string {
	if c.RemoteURL != "" {
		return c.RemoteURL
	}
	return "https://github.com/" + c.Repo + ".git"
}

// defaultCIWorkflow 刻意与基准用例的本地验证命令一致（各语言的官方入口），
// 这样“本地通过、CI 失败”反映的是环境差异，而不是两边跑的东西不同。
const defaultCIWorkflow = `# 由 agentdev bench --ci-repo 生成：只跑 PR，避免 push 与 pull_request
# 各触发一次同名检查、把同一个结果算两遍。
name: ci

on:
  pull_request:

permissions:
  contents: read

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        if: hashFiles('go.mod') != ''
        with:
          go-version-file: go.mod
      - uses: actions/setup-python@v5
        if: hashFiles('*.py') != ''
        with:
          python-version: "3.12"
      - uses: actions/setup-node@v4
        if: hashFiles('package.json') != '' || hashFiles('*.mjs') != ''
        with:
          node-version: "20"
      - name: run tests
        run: |
          set -euo pipefail
          ran=0
          if [ -f go.mod ]; then go test ./...; ran=1; fi
          if [ -f package.json ] || ls *.mjs >/dev/null 2>&1; then node --test; ran=1; fi
          if ls *.py >/dev/null 2>&1; then python -m unittest -q; ran=1; fi
          if [ "$ran" -eq 0 ]; then
            echo "没有识别到测试入口，拒绝把空跑当成通过"
            exit 1
          fi
`

// baseBranchFor 生成用例独占的基线分支名。run>1 时带轮次后缀，
// 保证重复采样之间也互不干扰。
func baseBranchFor(prefix, id string, run int) string {
	name := sanitizeBranch(id)
	if run > 1 {
		name = fmt.Sprintf("%s-run%d", name, run)
	}
	return strings.TrimSuffix(prefix, "/") + "/" + name
}

// sanitizeBranch 只保留 git 分支名安全的字符。
func sanitizeBranch(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		out = "case"
	}
	return out
}

// writeCIWorkflow 把 CI 定义写进仓库，随 fixture 一起提交到基线分支。
func writeCIWorkflow(repoDir string, ci CIRemote) error {
	path := filepath.Join(repoDir, filepath.FromSlash(ci.WorkflowPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(ci.Workflow), 0o644); err != nil {
		return err
	}
	// 防止 runner 以 CRLF 检出后把 shell 脚本读坏（与仓库根 .gitattributes 同因）。
	attrs := filepath.Join(repoDir, ".gitattributes")
	if _, err := os.Stat(attrs); os.IsNotExist(err) {
		return os.WriteFile(attrs, []byte("* text=auto eol=lf\n"), 0o644)
	}
	return nil
}

// pushBaseline 把当前 HEAD（fixture 基线）推到远端的新分支，返回该分支名。
// 远端只接受“已存在的分支”，因此这里显式推一个新分支，不改动默认分支。
//
// 用强制推送让这一步幂等：`bench/<case>` 是**该用例独占的一次性 ref**，不会有
// 别人往上面推。上一次运行若因网络中断没能清理，留在远端的同名分支会以
// `! [rejected] ... (fetch first)` 挡住这一次——一个崩溃残留于是变成后续所有
// 运行的拦路石，而这与产品行为毫无关系。实测中一次 6 用例的测量就是这样丢掉
// 了一个样本。
func pushBaseline(repoDir, id string, run int, ci CIRemote) (string, error) {
	ci = ci.withDefaults()
	branch := baseBranchFor(ci.BasePrefix, id, run)
	if _, err := workspace.Git(repoDir, "branch", "--force", branch, "HEAD"); err != nil {
		return "", fmt.Errorf("创建基线分支失败: %w", err)
	}
	if _, err := workspace.Git(repoDir, "remote", "add", ci.RemoteName, ci.remoteURL()); err != nil {
		return "", fmt.Errorf("配置远端失败: %w", err)
	}
	if err := delivery.PushRemoteForce(repoDir, ci.RemoteName, branch); err != nil {
		return "", fmt.Errorf("推送基线分支失败: %w", err)
	}
	// 本地也要能按名字解析该分支，worktree 才能从它创建。
	return branch, nil
}

// cleanupCase 关闭 PR 并删除该用例在远端留下的分支。
// 失败只记录、不改变用例结论：清理是卫生问题，不是测量结果。
func cleanupCase(ctx context.Context, repoDir, headBranch, baseBranch string, prNumber int64, ci CIRemote, log io.Writer) {
	ci = ci.withDefaults()
	warn := func(format string, args ...any) {
		if log != nil {
			fmt.Fprintf(log, "[WARN] 清理 %s 失败: %s\n", baseBranch, fmt.Sprintf(format, args...))
		}
	}
	if prNumber > 0 {
		// 只关闭 PR，不要 --delete-branch：gh 会尝试删除**本地**分支，
		// 而该分支正被 worktree 占用，必然失败并报错。
		// 远端分支统一由下面的 push --delete 处理。
		if out, err := runGH(ctx, repoDir, ci.GhPath, "pr", "close", fmt.Sprint(prNumber)); err != nil {
			warn("gh pr close: %v %s", err, strings.TrimSpace(out))
		}
	}
	if headBranch != "" {
		if _, err := workspace.Git(repoDir, "push", ci.RemoteName, "--delete", headBranch); err != nil && !isMissingRef(err) {
			warn("删除头分支 %s: %v", headBranch, err)
		}
	}
	if baseBranch != "" {
		if _, err := workspace.Git(repoDir, "push", ci.RemoteName, "--delete", baseBranch); err != nil && !isMissingRef(err) {
			warn("删除基线分支 %s: %v", baseBranch, err)
		}
	}
}

// isMissingRef 判断删除失败是否只是“分支已经不在”。
// gh pr close --delete-branch 会先删掉头分支，随后的显式删除必然撞上这种情况；
// 把它报成清理失败只会制造噪音。
func isMissingRef(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "remote ref does not exist") ||
		strings.Contains(msg, "couldn't find remote ref")
}

func runGH(ctx context.Context, dir, ghPath string, args ...string) (string, error) {
	bin := ghPath
	if bin == "" {
		bin = "gh"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
