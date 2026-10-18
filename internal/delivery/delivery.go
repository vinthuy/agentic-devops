// Package delivery 负责把已提交的改动安全地交付到远端：密钥扫描、推送分支、创建草稿 PR。
package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"agentdev/internal/workspace"
)

// Finding 是一处疑似密钥或敏感文件。
type Finding struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d [%s] %s", f.Path, f.Line, f.Rule, f.Detail)
}

type secretRule struct {
	name string
	re   *regexp.Regexp
}

// 规则刻意保守：宁可让少数误报进入人工复核，也不放行真实密钥。
var secretRules = []secretRule{
	{"aws_access_key_id", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"github_token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)},
	{"openai_style_key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)},
	{"slack_token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
	{"bearer_token", regexp.MustCompile(`(?i)authorization:\s*bearer\s+[A-Za-z0-9._\-]{20,}`)},
	{"assigned_secret", regexp.MustCompile(`(?i)(api[_-]?key|apikey|secret|passwd|password|access[_-]?token|auth[_-]?token)\s*[:=]\s*["'][^"']{12,}["']`)},
}

var sensitiveFileName = regexp.MustCompile(`(?i)(^|/)(\.env(\..+)?|[^/]*\.(pem|p12|pfx|key)|credentials(\..+)?|id_rsa|\.npmrc|\.netrc)$`)

// ScanDiff 扫描 unified diff 中新增的行，返回疑似泄露。
// 只看新增行：删除密钥本身不是风险，把密钥写进仓库才是。
func ScanDiff(diff string) []Finding {
	var findings []Finding
	path := ""
	line := 0
	for _, raw := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(raw, "+++ "):
			path = strings.TrimPrefix(raw, "+++ ")
			path = strings.TrimPrefix(path, "b/")
			if path == "/dev/null" {
				path = ""
			}
			if path != "" && sensitiveFileName.MatchString(path) {
				findings = append(findings, Finding{
					Path: path, Rule: "sensitive_file",
					Detail: "新增或修改了敏感文件名",
				})
			}
		case strings.HasPrefix(raw, "@@"):
			line = parseHunkStart(raw)
		case strings.HasPrefix(raw, "+") && !strings.HasPrefix(raw, "+++"):
			content := raw[1:]
			for _, r := range secretRules {
				if loc := r.re.FindString(content); loc != "" {
					findings = append(findings, Finding{
						Path: path, Line: line, Rule: r.name,
						Detail: "疑似密钥: " + redact(loc),
					})
				}
			}
			line++
		case strings.HasPrefix(raw, " "):
			line++
		}
	}
	return findings
}

var hunkRe = regexp.MustCompile(`@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

func parseHunkStart(hunk string) int {
	m := hunkRe.FindStringSubmatch(hunk)
	if len(m) < 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// redact 只保留前后各 4 个字符，避免把疑似密钥完整写进日志与报告。
func redact(s string) string {
	r := []rune(s)
	if len(r) <= 12 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:4]) + strings.Repeat("*", len(r)-8) + string(r[len(r)-4:])
}

// RedactText 脱敏模型/CI 文本中命中的已知密钥格式，供事件、报告及回传上下文使用。
// 文本不一定是 diff，故不复用 ScanDiff 的行/路径提取；只替换命中的 token 字符串。
func RedactText(text string) string {
	for _, rule := range secretRules {
		text = rule.re.ReplaceAllStringFunc(text, redact)
	}
	return text
}

// RemoteExists 检查远端名是否已配置。
func RemoteExists(worktree, remote string) (bool, error) {
	out, err := workspace.Git(worktree, "remote")
	if err != nil {
		return false, err
	}
	for _, name := range strings.Fields(out) {
		if name == remote {
			return true, nil
		}
	}
	return false, nil
}

// PushRequest 描述一次分支推送。
type PushRequest struct {
	Worktree string
	Remote   string
	Branch   string
	// Protected 是仓库默认分支名；推送它会被拒绝。
	Protected string
	// AllowedPrefix 非空时，分支必须以该前缀开头。
	AllowedPrefix string
}

// 常见默认分支名。即使配置里的 default_branch 写错了，也不能把任务改动直接
// 推到这些分支上。
var protectedBranchNames = map[string]bool{"main": true, "master": true}

// EnsurePushable 检查分支是否可以推送。
//
// 这是最后一道程序侧护栏：Agent 无法调用 git push（命令策略禁止），
// 但编排器自身也可能因为配置错误而把任务分支算成默认分支。
func EnsurePushable(req PushRequest) error {
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		return errors.New("拒绝推送：分支名为空")
	}
	if protectedBranchNames[strings.ToLower(branch)] {
		return fmt.Errorf("拒绝推送受保护分支 %q", branch)
	}
	if protected := strings.TrimSpace(req.Protected); protected != "" &&
		strings.EqualFold(branch, protected) {
		return fmt.Errorf("拒绝推送仓库默认分支 %q", branch)
	}
	if prefix := req.AllowedPrefix; prefix != "" && !strings.HasPrefix(branch, prefix) {
		return fmt.Errorf("拒绝推送分支 %q：必须以 %q 开头", branch, prefix)
	}
	return nil
}

// Push 校验并推送分支，同时建立追踪关系。
func Push(req PushRequest) error {
	if err := EnsurePushable(req); err != nil {
		return err
	}
	if _, err := workspace.Git(req.Worktree, "push", "--set-upstream", req.Remote, req.Branch); err != nil {
		return fmt.Errorf("推送分支失败: %w", err)
	}
	return nil
}

// PRRequest 描述一次 PR 创建请求。
type PRRequest struct {
	Worktree string
	Base     string
	Head     string
	Title    string
	Body     string
	Draft    bool
}

type PRResult struct {
	Number int64  `json:"number"`
	URL    string `json:"url"`
	// Created 为 true 表示本次确实新建了 PR；false 表示更新了已有 PR。
	Created bool `json:"created"`
}

// CodeHost 抽象 PR 托管平台，便于测试与后续接入 GitLab。
type CodeHost interface {
	Name() string
	// EnsurePR 幂等地保证分支对应一个 PR：不存在则创建，已存在则更新标题与正文。
	// CI 失败会回到 CODING 再走一遍交付流程，如果不幂等就会撞上
	// “a pull request already exists”。
	EnsurePR(ctx context.Context, req PRRequest) (PRResult, error)
}

// GHCli 通过 gh CLI 创建 PR。gh 使用当前用户的凭据，本项目不保存任何 token。
type GHCli struct {
	Path   string // gh 可执行文件路径，空则用 PATH 中的 gh
	Runner CommandRunner
}

// CommandRunner 允许测试替换 gh 子进程，同时保留正式实现走 exec.CommandContext。
type CommandRunner interface {
	Run(ctx context.Context, bin, dir string, args []string, stdin string) (stdout, stderr string, err error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, bin, dir string, args []string, stdin string) (string, string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (g GHCli) Name() string { return "gh" }

func (g GHCli) bin() string {
	if g.Path != "" {
		return g.Path
	}
	return "gh"
}

func (g GHCli) run(ctx context.Context, dir string, args []string, stdin string) (string, string, error) {
	runner := g.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	return runner.Run(ctx, g.bin(), dir, args, stdin)
}

func (g GHCli) EnsurePR(ctx context.Context, req PRRequest) (PRResult, error) {
	existing, found, err := g.findPR(ctx, req.Worktree, req.Head)
	if err != nil {
		return PRResult{}, err
	}
	if found {
		if err := g.editPR(ctx, req.Worktree, existing.Number, req.Title, req.Body); err != nil {
			return PRResult{}, err
		}
		return existing, nil
	}
	return g.createPR(ctx, req)
}

// findPR 查找该分支已存在的 PR。gh pr view 在不存在时以非零码退出，
// 这里只在明确的“没有 PR”提示下当作不存在，避免把鉴权失败误判成需要新建。
func (g GHCli) findPR(ctx context.Context, worktree, head string) (PRResult, bool, error) {
	stdout, stderr, err := g.run(ctx, worktree, []string{"pr", "view", head, "--json", "number,url"}, "")
	if err != nil {
		msg := strings.TrimSpace(stderr)
		if isNoPRMessage(msg) {
			return PRResult{}, false, nil
		}
		return PRResult{}, false, fmt.Errorf("gh pr view 失败: %s", msg)
	}
	var pr struct {
		Number int64  `json:"number"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal([]byte(stdout), &pr); err != nil {
		return PRResult{}, false, fmt.Errorf("解析 gh pr view 输出失败: %w", err)
	}
	if pr.Number == 0 {
		return PRResult{}, false, nil
	}
	return PRResult{Number: pr.Number, URL: pr.URL}, true, nil
}

func isNoPRMessage(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "no pull requests found") ||
		strings.Contains(lower, "no open pull requests")
}

func (g GHCli) editPR(ctx context.Context, worktree string, number int64, title, body string) error {
	args := []string{"pr", "edit", strconv.FormatInt(number, 10),
		"--title", title, "--body-file", "-"}
	_, stderr, err := g.run(ctx, worktree, args, body)
	if err != nil {
		return fmt.Errorf("gh pr edit 失败: %s", strings.TrimSpace(stderr))
	}
	return nil
}

func (g GHCli) createPR(ctx context.Context, req PRRequest) (PRResult, error) {
	args := []string{"pr", "create",
		"--base", req.Base,
		"--head", req.Head,
		"--title", req.Title,
		"--body-file", "-",
	}
	if req.Draft {
		args = append(args, "--draft")
	}
	stdout, stderr, err := g.run(ctx, req.Worktree, args, req.Body)
	if err != nil {
		return PRResult{}, fmt.Errorf("gh pr create 失败: %s", strings.TrimSpace(stderr))
	}
	url := strings.TrimSpace(stdout)
	if i := strings.Index(url, "\n"); i >= 0 {
		url = strings.TrimSpace(url[:i])
	}
	if url == "" {
		return PRResult{}, errors.New("gh pr create 未返回 PR 地址")
	}
	return PRResult{Number: prNumberFromURL(url), URL: url, Created: true}, nil
}

var prNumberRe = regexp.MustCompile(`/pull/(\d+)`)

func prNumberFromURL(url string) int64 {
	m := prNumberRe.FindStringSubmatch(url)
	if len(m) < 2 {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

// Available 判断 gh CLI 是否可用。
func Available() bool {
	_, err := exec.LookPath("gh")
	return err == nil
}

// Fake 供测试使用；记录收到的请求。
type Fake struct {
	Result PRResult
	Err    error
	// Existing 为 true 时模拟“该分支已有 PR”，EnsurePR 应返回 Created=false。
	Existing bool
	Requests []PRRequest
}

func (f *Fake) Name() string { return "fake" }

func (f *Fake) EnsurePR(_ context.Context, req PRRequest) (PRResult, error) {
	f.Requests = append(f.Requests, req)
	if f.Err != nil {
		return PRResult{}, f.Err
	}
	res := f.Result
	if res.URL == "" {
		res = PRResult{Number: 1, URL: "https://example.invalid/pull/1"}
	}
	res.Created = !f.Existing
	return res, nil
}

// MarshalFindings 便于写入事件负载。
func MarshalFindings(findings []Finding) []any {
	out := make([]any, 0, len(findings))
	for _, f := range findings {
		b, _ := json.Marshal(f)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		out = append(out, m)
	}
	return out
}
