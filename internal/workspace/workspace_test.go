package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/config"
)

// 任务 worktree 必须从仓库默认分支的当前提交创建，且分支名带配置前缀（FR-020 / FR-040）。
func TestCreateWorktreeFromDefaultBranchHead(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := Git(root, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}

	git("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "main base")
	mainSHA := git("rev-parse", "HEAD")

	// 在另一个分支上再提交一次，确保 worktree 不是误从当前 HEAD 创建。
	git("checkout", "-b", "feature/other")
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "b.txt")
	git("-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "other work")

	cfg := &config.App{
		Home:          filepath.Join(root, ".agentdev"),
		Delivery:      config.DeliveryConfig{BranchPrefix: "agent/", DraftPR: true},
		GitAuthorName: "test", GitAuthorEmail: "test@example.com",
	}
	mgr := New(cfg)
	info, err := mgr.Create("acme/app", config.RepoConfig{
		LocalPath:     root,
		DefaultBranch: "main",
	}, "t_20261018_abc123", "fix the parser")
	if err != nil {
		t.Fatal(err)
	}

	if info.BaseSHA != mainSHA {
		t.Errorf("基线提交应为默认分支 HEAD %s，实际 %s", mainSHA, info.BaseSHA)
	}
	if !strings.HasPrefix(info.Branch, "agent/t_20261018_abc123-") {
		t.Errorf("分支名应带任务 ID 与配置前缀，实际 %q", info.Branch)
	}
	head, err := Git(info.Path, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(head) != mainSHA {
		t.Errorf("worktree HEAD 应等于默认分支 HEAD %s，实际 %s", mainSHA, strings.TrimSpace(head))
	}
	// 另一分支的文件不应出现在任务 worktree 中。
	if _, err := os.Stat(filepath.Join(info.Path, "b.txt")); !os.IsNotExist(err) {
		t.Errorf("worktree 不应包含其他分支的内容: %v", err)
	}
	if err := mgr.Remove(config.RepoConfig{LocalPath: root}, info, false); err != nil {
		t.Fatal(err)
	}
}
