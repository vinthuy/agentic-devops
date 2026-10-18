package workspace

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"agentdev/internal/config"
)

type Info struct {
	Path    string
	Branch  string
	BaseSHA string
}

type FileChange struct {
	Status byte
	Path   string
}

type Manager struct {
	cfg *config.App
}

func New(cfg *config.App) *Manager {
	return &Manager{cfg: cfg}
}

// Git 在 dir 中执行 git 命令，失败时错误信息包含 stderr。
func Git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s 失败: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func Slugify(text string, limit int) string {
	s := strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(text), "-"), "-")
	if len(s) > limit {
		s = strings.TrimRight(s[:limit], "-")
	}
	if s == "" {
		return "task"
	}
	return s
}

func repoDirName(repo string) string {
	return strings.ReplaceAll(repo, "/", "__")
}

// Create 基于默认分支创建 worktree 与新分支 <prefix><taskID>-<slug>。
func (m *Manager) Create(repoName string, repo config.RepoConfig, taskID, slugSource string) (Info, error) {
	base, err := Git(repo.LocalPath, "rev-parse", repo.DefaultBranch)
	if err != nil {
		return Info{}, err
	}
	base = strings.TrimSpace(base)
	branch := m.cfg.Delivery.BranchPrefix + taskID + "-" + Slugify(slugSource, 40)
	path := filepath.Join(m.cfg.WorkspacesDir(), repoDirName(repoName), taskID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Info{}, err
	}
	if _, err := Git(repo.LocalPath, "worktree", "add", "-b", branch, path, base); err != nil {
		return Info{}, err
	}
	return Info{Path: path, Branch: branch, BaseSHA: base}, nil
}

// StageAll 暂存全部改动，并把工具生成的产物移出暂存区。
//
// 移出而不是删除：产物仍留在工作区（可能是验证命令生成的），只是不进提交。
// 返回被排除的相对路径，便于调用方在报告中说明，避免“悄悄就不见了”。
func (m *Manager) StageAll(info Info) ([]string, error) {
	if _, err := Git(info.Path, "add", "-A"); err != nil {
		return nil, err
	}
	staged, err := Git(info.Path, "diff", "--cached", "--name-only")
	if err != nil {
		return nil, err
	}
	var excluded []string
	for _, line := range strings.Split(staged, "\n") {
		rel := strings.TrimSpace(line)
		if rel != "" && IsGeneratedArtifact(rel) {
			excluded = append(excluded, rel)
		}
	}
	if len(excluded) == 0 {
		return nil, nil
	}
	args := append([]string{"reset", "-q", "--"}, excluded...)
	if _, err := Git(info.Path, args...); err != nil {
		return nil, err
	}
	return excluded, nil
}

// ChangedFiles 返回相对基线提交的改动列表。
func (m *Manager) ChangedFiles(info Info) ([]FileChange, error) {
	if _, err := m.StageAll(info); err != nil {
		return nil, err
	}
	out, err := Git(info.Path, "diff", "--cached", "--name-status", info.BaseSHA)
	if err != nil {
		return nil, err
	}
	var changes []FileChange
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) >= 2 {
			changes = append(changes, FileChange{Status: parts[0][0], Path: parts[len(parts)-1]})
		}
	}
	return changes, nil
}

func (m *Manager) Diff(info Info) (string, error) {
	if _, err := m.StageAll(info); err != nil {
		return "", err
	}
	return Git(info.Path, "diff", "--cached", "--no-color", info.BaseSHA)
}

// Commit 提交当前改动并返回 SHA；没有改动时返回空字符串。
// excluded 为被排除的生成产物相对路径（见 StageAll）。
func (m *Manager) Commit(info Info, message string) (string, []string, error) {
	excluded, err := m.StageAll(info)
	if err != nil {
		return "", nil, err
	}
	staged, err := Git(info.Path, "diff", "--cached", "--name-only")
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(staged) == "" {
		return "", excluded, nil
	}
	if _, err := Git(info.Path,
		"-c", "user.name="+m.cfg.GitAuthorName,
		"-c", "user.email="+m.cfg.GitAuthorEmail,
		"commit", "-m", message); err != nil {
		return "", nil, err
	}
	sha, err := Git(info.Path, "rev-parse", "HEAD")
	return strings.TrimSpace(sha), excluded, err
}

// Remove 删除 worktree；keepBranch 为 false 时同时删除分支。
func (m *Manager) Remove(repo config.RepoConfig, info Info, keepBranch bool) error {
	_, _ = Git(repo.LocalPath, "worktree", "remove", "--force", info.Path)
	if !keepBranch {
		_, _ = Git(repo.LocalPath, "branch", "-D", info.Branch)
	}
	return nil
}
