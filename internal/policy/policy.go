package policy

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"agentdev/internal/config"
)

// Violation 表示被策略拒绝的操作。
type Violation struct {
	Kind   string
	Detail string
}

func (v *Violation) Error() string { return v.Detail }

func violation(kind, format string, args ...any) error {
	return &Violation{Kind: kind, Detail: fmt.Sprintf(format, args...)}
}

// IsViolation 判断错误是否为策略拒绝。
func IsViolation(err error) (*Violation, bool) {
	var v *Violation
	if errors.As(err, &v) {
		return v, true
	}
	return nil, false
}

var (
	globCache sync.Map
	shellMeta = regexp.MustCompile("[;&|`<>]|\\$\\(")
)

// GlobMatch 支持 *、?、** 的路径匹配，路径使用 / 分隔，且必须整体匹配。
func GlobMatch(relPosix, pattern string) bool {
	re, ok := globCache.Load(pattern)
	if !ok {
		re = compileGlob(pattern)
		globCache.Store(pattern, re)
	}
	return re.(*regexp.Regexp).MatchString(relPosix)
}

func compileGlob(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			b.WriteString("[^/]*")
			i++
		case pattern[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
			i++
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// Engine 对路径和命令做确定性检查。所有工具调用必须先经过这里。
type Engine struct {
	Policy       config.PolicyConfig
	Repo         config.RepoConfig
	Root         string
	AllowedPaths []string // nil 表示不限制写入范围
	denyCommands []*regexp.Regexp
}

func NewEngine(p config.PolicyConfig, repo config.RepoConfig, worktree string, allowed []string) (*Engine, error) {
	root, err := resolve(worktree)
	if err != nil {
		return nil, err
	}
	e := &Engine{Policy: p, Repo: repo, Root: root, AllowedPaths: allowed}
	for _, pat := range p.DenyCommandPatterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("deny_command_patterns 正则无效 %q: %w", pat, err)
		}
		e.denyCommands = append(e.denyCommands, re)
	}
	return e, nil
}

// resolve 解析符号链接；路径不存在时解析其最近的已存在祖先。
func resolve(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	var rest []string
	cur := abs
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				real = filepath.Join(real, rest[i])
			}
			return filepath.Clean(real), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(abs), nil
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// Relative 返回解析后的绝对路径及其相对工作区的 POSIX 形式。
func (e *Engine) Relative(raw string) (string, string, error) {
	if raw == "" || strings.ContainsRune(raw, 0) {
		return "", "", violation("path", "路径为空或包含非法字符")
	}
	if filepath.IsAbs(raw) || filepath.VolumeName(raw) != "" || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, `\`) {
		return "", "", violation("path", "不允许绝对路径: %s", raw)
	}
	resolved, err := resolve(filepath.Join(e.Root, raw))
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(e.Root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", violation("path", "路径越出工作区: %s", raw)
	}
	if rel == "." {
		return resolved, ".", nil
	}
	return resolved, filepath.ToSlash(rel), nil
}

func (e *Engine) IsDenied(relPosix string) bool {
	for _, pat := range e.Policy.DenyPaths {
		if GlobMatch(relPosix, pat) {
			return true
		}
	}
	// 仓库级敏感路径与全局黑名单同等对待，避免分级之外动到敏感文件。
	for _, pat := range e.Repo.SensitivePaths {
		if GlobMatch(relPosix, pat) {
			return true
		}
	}
	return false
}

// CheckPath 返回通过检查的绝对路径。
func (e *Engine) CheckPath(raw string, write bool) (string, error) {
	abs, rel, err := e.Relative(raw)
	if err != nil {
		return "", err
	}
	if e.IsDenied(rel) {
		return "", violation("path", "敏感路径禁止访问: %s", rel)
	}
	if write && e.AllowedPaths != nil {
		ok := false
		for _, pat := range e.AllowedPaths {
			if GlobMatch(rel, pat) {
				ok = true
				break
			}
		}
		if !ok {
			return "", violation("path", "不在计划允许的修改范围内: %s", rel)
		}
	}
	return abs, nil
}

// CheckCommand 检查命令是否可执行：黑名单、禁止组合命令、白名单前缀。
func (e *Engine) CheckCommand(cmd string) error {
	stripped := strings.TrimSpace(cmd)
	if stripped == "" {
		return violation("command", "命令为空")
	}
	for i, re := range e.denyCommands {
		if re.MatchString(stripped) {
			return violation("command", "命令命中黑名单 /%s/: %s", e.Policy.DenyCommandPatterns[i], stripped)
		}
	}
	if shellMeta.MatchString(stripped) {
		return violation("command", "不允许组合命令或重定向: %s", stripped)
	}
	var allowed []string
	for _, c := range e.Repo.VerifyCommands {
		allowed = append(allowed, c.Command)
	}
	allowed = append(allowed, e.Repo.AllowedCommands...)
	for _, a := range allowed {
		if stripped == a || strings.HasPrefix(stripped, a+" ") {
			return nil
		}
	}
	return violation("command", "命令不在白名单中: %s", stripped)
}
