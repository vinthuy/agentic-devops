package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"agentdev/internal/policy"
)

const (
	maxReadBytes     = 2_000_000
	maxSearchResults = 100
)

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "__pycache__": true, ".venv": true,
	"venv": true, "dist": true, "build": true,
}

// ViolationHook 在策略拒绝时被调用，用于写入 policy_violations。
type ViolationHook func(kind, detail string, blocked bool)

type Result struct {
	OK      bool
	Content string
}

type Runtime struct {
	policy      *policy.Engine
	maxOutput   int
	cmdTimeout  time.Duration
	onViolation ViolationHook
}

func New(p *policy.Engine, maxOutput, commandTimeoutS int, hook ViolationHook) *Runtime {
	return &Runtime{
		policy:      p,
		maxOutput:   maxOutput,
		cmdTimeout:  time.Duration(commandTimeoutS) * time.Second,
		onViolation: hook,
	}
}

// CommandHelp 返回给模型看的可用命令清单。
//
// run_command 只允许执行白名单内的命令；如果不把清单显式告诉模型，模型会反复
// 试 `pytest`、`python test_x.py` 之类被拒绝的写法，白白烧掉轮次和费用。
func (r *Runtime) CommandHelp() string {
	var b strings.Builder
	if verify := r.policy.Repo.VerifyCommands; len(verify) > 0 {
		b.WriteString("验证命令（由仓库配置，运行时会据此复核验收标准）：\n")
		for _, c := range verify {
			fmt.Fprintf(&b, "- %s: %s\n", c.Name, c.Command)
		}
	}
	if allowed := r.policy.Repo.AllowedCommands; len(allowed) > 0 {
		b.WriteString("其他允许的命令（前缀匹配）：\n")
		for _, c := range allowed {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	b.WriteString("除上表之外，run_command 会拒绝执行任何命令。")
	return b.String()
}

// Schemas 返回 Anthropic Messages API 的 tools 定义。
func (r *Runtime) Schemas() []map[string]any {
	return []map[string]any{
		{
			"name":        "read_file",
			"description": "读取工作区内的文本文件，可指定起止行号（从 1 开始，含端点）。",
			"input_schema": obj(map[string]any{
				"path":  prop("string"),
				"start": prop("integer"),
				"end":   prop("integer"),
			}, "path"),
		},
		{
			"name":        "write_file",
			"description": "整文件写入工作区内的文件（不存在则创建）。",
			"input_schema": obj(map[string]any{
				"path":    prop("string"),
				"content": prop("string"),
			}, "path", "content"),
		},
		{
			"name":        "edit_file",
			"description": "把文件中唯一出现的 old 文本替换为 new。old 必须在文件中恰好出现一次。",
			"input_schema": obj(map[string]any{
				"path": prop("string"),
				"old":  prop("string"),
				"new":  prop("string"),
			}, "path", "old", "new"),
		},
		{
			"name":        "list_dir",
			"description": "列出目录内容，目录名以 / 结尾。",
			"input_schema": obj(map[string]any{
				"path": prop("string"),
			}),
		},
		{
			"name":        "search",
			"description": "在工作区内用正则搜索文本，返回 相对路径:行号: 内容。",
			"input_schema": obj(map[string]any{
				"pattern": prop("string"),
				"path":    prop("string"),
			}, "pattern"),
		},
		{
			"name":        "run_command",
			"description": "在工作区根目录执行命令。只允许 <allowed_commands> 中列出的命令，且必须原样使用；不支持管道、重定向和组合命令。",
			"input_schema": obj(map[string]any{
				"command": prop("string"),
			}, "command"),
		},
	}
}

func prop(t string) map[string]any { return map[string]any{"type": t} }

func obj(props map[string]any, required ...string) map[string]any {
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// Execute 分派工具调用。策略拒绝和普通错误都转为 Result 返回给模型，不会中断流程。
func (r *Runtime) Execute(name string, args map[string]any) Result {
	if args == nil {
		args = map[string]any{}
	}
	var (
		content string
		err     error
	)
	switch name {
	case "read_file":
		content, err = r.readFile(args)
	case "write_file":
		content, err = r.writeFile(args)
	case "edit_file":
		content, err = r.editFile(args)
	case "list_dir":
		content, err = r.listDir(args)
	case "search":
		content, err = r.search(args)
	case "run_command":
		return r.runCommand(args)
	default:
		return Result{false, "未知工具: " + name}
	}
	if err != nil {
		if v, ok := policy.IsViolation(err); ok {
			if r.onViolation != nil {
				r.onViolation(v.Kind, v.Detail, true)
			}
			return Result{false, fmt.Sprintf("策略拒绝 [%s]: %s", v.Kind, v.Detail)}
		}
		return Result{false, err.Error()}
	}
	return Result{true, content}
}

func strArg(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("缺少参数: %s", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("参数 %s 必须是字符串", key)
	}
	return s, nil
}

func optStr(args map[string]any, key, def string) string {
	if s, ok := args[key].(string); ok && s != "" {
		return s
	}
	return def
}

func optInt(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

func (r *Runtime) readFile(args map[string]any) (string, error) {
	raw, err := strArg(args, "path")
	if err != nil {
		return "", err
	}
	path, err := r.policy.CheckPath(raw, false)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("不是文件: %s", raw)
	}
	if info.Size() > maxReadBytes {
		return "", errors.New("文件过大，拒绝读取")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := splitLines(string(data))
	start := 1
	if s, ok := optInt(args, "start"); ok && s > 1 {
		start = s
	}
	end := len(lines)
	if e, ok := optInt(args, "end"); ok && e > 0 && e < end {
		end = e
	}
	var b strings.Builder
	for i := start; i <= end && i <= len(lines); i++ {
		fmt.Fprintf(&b, "%d: %s\n", i, lines[i-1])
	}
	return Truncate(strings.TrimSuffix(b.String(), "\n"), r.maxOutput), nil
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func (r *Runtime) writeFile(args map[string]any) (string, error) {
	raw, err := strArg(args, "path")
	if err != nil {
		return "", err
	}
	content, err := strArg(args, "content")
	if err != nil {
		return "", err
	}
	path, err := r.policy.CheckPath(raw, true)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("已写入 %s (%d 字符)", raw, utf8.RuneCountInString(content)), nil
}

func (r *Runtime) editFile(args map[string]any) (string, error) {
	raw, err := strArg(args, "path")
	if err != nil {
		return "", err
	}
	oldText, err := strArg(args, "old")
	if err != nil {
		return "", err
	}
	newText, err := strArg(args, "new")
	if err != nil {
		return "", err
	}
	path, err := r.policy.CheckPath(raw, true)
	if err != nil {
		return "", err
	}
	if oldText == "" {
		return "", errors.New("old 不能为空")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("文件不存在: %s", raw)
	}
	text := string(data)
	if count := strings.Count(text, oldText); count != 1 {
		return "", fmt.Errorf("old 必须恰好出现一次，实际出现 %d 次", count)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(text, oldText, newText, 1)), 0o644); err != nil {
		return "", err
	}
	return "已编辑 " + raw, nil
}

func (r *Runtime) listDir(args map[string]any) (string, error) {
	raw := optStr(args, "path", ".")
	path, err := r.policy.CheckPath(raw, false)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf("不是目录: %s", raw)
	}
	sort.Slice(entries, func(i, j int) bool {
		di, dj := entries[i].IsDir(), entries[j].IsDir()
		if di != dj {
			return di
		}
		return entries[i].Name() < entries[j].Name()
	})
	var out []string
	for _, e := range entries {
		if skipDirs[e.Name()] {
			continue
		}
		_, rel, err := r.policy.Relative(filepath.Join(raw, e.Name()))
		if err != nil || r.policy.IsDenied(rel) {
			continue
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return "(空目录)", nil
	}
	return strings.Join(out, "\n"), nil
}

func (r *Runtime) search(args map[string]any) (string, error) {
	pattern, err := strArg(args, "pattern")
	if err != nil {
		return "", err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("正则无效: %w", err)
	}
	base, err := r.policy.CheckPath(optStr(args, "path", "."), false)
	if err != nil {
		return "", err
	}
	var results []string
	walkErr := walkFiles(base, func(file string) error {
		rel, err := filepath.Rel(r.policy.Root, file)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if r.policy.IsDenied(rel) {
			return nil
		}
		info, err := os.Stat(file)
		if err != nil || info.Size() > maxReadBytes {
			return nil
		}
		data, err := os.ReadFile(file)
		if err != nil || !utf8.Valid(data) {
			return nil
		}
		for n, line := range splitLines(string(data)) {
			if re.MatchString(line) {
				results = append(results, fmt.Sprintf("%s:%d: %s", rel, n+1, strings.TrimSpace(line)))
				if len(results) >= maxSearchResults {
					return errStop
				}
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errStop) {
		return "", walkErr
	}
	if errors.Is(walkErr, errStop) {
		results = append(results, fmt.Sprintf("...结果已达上限 %d 条", maxSearchResults))
	}
	if len(results) == 0 {
		return "(无匹配)", nil
	}
	return Truncate(strings.Join(results, "\n"), r.maxOutput), nil
}

var errStop = errors.New("stop")

func walkFiles(base string, fn func(string) error) error {
	info, err := os.Stat(base)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fn(base)
	}
	return filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != base && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			return fn(path)
		}
		return nil
	})
}

func (r *Runtime) runCommand(args map[string]any) Result {
	command, err := strArg(args, "command")
	if err != nil {
		return Result{false, err.Error()}
	}
	if err := r.policy.CheckCommand(command); err != nil {
		if v, ok := policy.IsViolation(err); ok {
			if r.onViolation != nil {
				r.onViolation(v.Kind, v.Detail, true)
			}
			return Result{false, fmt.Sprintf("策略拒绝 [%s]: %s", v.Kind, v.Detail)}
		}
		return Result{false, err.Error()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.cmdTimeout)
	defer cancel()
	cmd := ShellCommand(ctx, command)
	cmd.Dir = r.policy.Root
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Result{false, fmt.Sprintf("命令超时（>%s）: %s", r.cmdTimeout, command)}
	}
	code := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			return Result{false, "执行失败: " + runErr.Error()}
		}
	}
	body := Truncate(buf.String(), r.maxOutput)
	if body == "" {
		body = "(无输出)"
	}
	return Result{code == 0, fmt.Sprintf("exit=%d\n%s", code, body)}
}

// ShellCommand 在当前平台的 shell 中构造命令（Windows 用 cmd /C，其他平台用 sh -c）。
func ShellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command)
	}
	return exec.CommandContext(ctx, "sh", "-c", command)
}

// Truncate 按字符数截断，保留头部约 60% 与尾部，中间标注省略数量。
func Truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	head := int(float64(limit) * 0.6)
	tail := limit - head
	omitted := len(runes) - limit
	return fmt.Sprintf("%s\n...[已截断 %d 字符]...\n%s", string(runes[:head]), omitted, string(runes[len(runes)-tail:]))
}
