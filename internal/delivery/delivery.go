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

// Push 把当前分支推送到远端并建立追踪关系。
func Push(worktree, remote, branch string) error {
	if _, err := workspace.Git(worktree, "push", "--set-upstream", remote, branch); err != nil {
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
}

// CodeHost 抽象 PR 托管平台，便于测试与后续接入 GitLab。
type CodeHost interface {
	Name() string
	CreatePR(ctx context.Context, req PRRequest) (PRResult, error)
}

// GHCli 通过 gh CLI 创建 PR。gh 使用当前用户的凭据，本项目不保存任何 token。
type GHCli struct {
	Path string // gh 可执行文件路径，空则用 PATH 中的 gh
}

func (g GHCli) Name() string { return "gh" }

func (g GHCli) CreatePR(ctx context.Context, req PRRequest) (PRResult, error) {
	bin := g.Path
	if bin == "" {
		bin = "gh"
	}
	args := []string{"pr", "create",
		"--base", req.Base,
		"--head", req.Head,
		"--title", req.Title,
		"--body-file", "-",
	}
	if req.Draft {
		args = append(args, "--draft")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = req.Worktree
	cmd.Stdin = strings.NewReader(req.Body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return PRResult{}, fmt.Errorf("gh pr create 失败: %s", strings.TrimSpace(stderr.String()))
	}
	url := strings.TrimSpace(stdout.String())
	if i := strings.Index(url, "\n"); i >= 0 {
		url = strings.TrimSpace(url[:i])
	}
	if url == "" {
		return PRResult{}, errors.New("gh pr create 未返回 PR 地址")
	}
	return PRResult{Number: prNumberFromURL(url), URL: url}, nil
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
	Result   PRResult
	Err      error
	Requests []PRRequest
}

func (f *Fake) Name() string { return "fake" }

func (f *Fake) CreatePR(_ context.Context, req PRRequest) (PRResult, error) {
	f.Requests = append(f.Requests, req)
	if f.Err != nil {
		return PRResult{}, f.Err
	}
	res := f.Result
	if res.URL == "" {
		res = PRResult{Number: 1, URL: "https://example.invalid/pull/1"}
	}
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
