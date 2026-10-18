package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"agentdev/internal/config"
	"agentdev/internal/planning"
	"agentdev/internal/policy"
	"agentdev/internal/tools"
	"agentdev/internal/workspace"
)

type Outcome struct {
	Name      string  `json:"name"`
	Command   string  `json:"command"`
	ExitCode  int     `json:"exit_code"`
	DurationS float64 `json:"duration_s"`
	LogPath   string  `json:"log_path"`
	Tail      string  `json:"-"`
}

type Report struct {
	Passed     bool
	Outcomes   []Outcome
	Acceptance []AcceptanceOutcome
}

// AcceptanceOutcome 是一条验收标准的机器校验结果。
type AcceptanceOutcome struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Target      string `json:"target"`
	Passed      bool   `json:"passed"`
	Evidence    string `json:"evidence"`
	// Invalid 表示这条验收标准本身不可执行（例如选择器匹配不到任何测试）。
	// 这类失败不能靠改代码解决，必须回到规划或转人工，否则会白烧修复轮次。
	Invalid bool `json:"invalid,omitempty"`
}

// InvalidAcceptance 返回不可执行的验收标准。
func (r Report) InvalidAcceptance() []AcceptanceOutcome {
	var out []AcceptanceOutcome
	for _, a := range r.Acceptance {
		if a.Invalid {
			out = append(out, a)
		}
	}
	return out
}

// noTestsPattern 匹配“选择器本身无法执行”的输出：没有选中测试、选择器语法不被
// 运行器接受、或不存在的测试名。这类失败不是代码缺陷，改代码永远修不好。
var noTestsPattern = regexp.MustCompile(
	`(?i)(ran 0 tests|no tests ran|no tests to run|no test files|no tests found|` +
		`no matching tests|no tests matched|matched no packages|no packages to test|` +
		`warning: no tests|unittest\.loader\._failedtest|named files must be \.go files|` +
		`no go files|could not find any test files|cannot find module|no test specified|` +
		`error: file or directory not found)`,
)

func looksLikeNoTests(output string) bool {
	return noTestsPattern.MatchString(output)
}

// FailureSummary 返回第一个失败的验收标准或验证命令的摘要，供修复轮次使用。
func (r Report) FailureSummary() string {
	for _, a := range r.Acceptance {
		if !a.Passed {
			return fmt.Sprintf("验收标准 `%s` 未满足（%s）。证据:\n%s", a.ID, a.Description, a.Evidence)
		}
	}
	for _, o := range r.Outcomes {
		if o.ExitCode != 0 {
			return fmt.Sprintf("命令 `%s` (%s) 失败，退出码 %d。输出末尾:\n%s", o.Command, o.Name, o.ExitCode, o.Tail)
		}
	}
	return ""
}

func (r Report) ToMap() map[string]any {
	outcomes := make([]any, 0, len(r.Outcomes))
	for _, o := range r.Outcomes {
		outcomes = append(outcomes, map[string]any{
			"name": o.Name, "command": o.Command, "exit_code": o.ExitCode,
			"duration_s": fmt.Sprintf("%.2f", o.DurationS), "log_path": o.LogPath,
		})
	}
	acceptance := make([]any, 0, len(r.Acceptance))
	for _, a := range r.Acceptance {
		acceptance = append(acceptance, map[string]any{
			"id": a.ID, "description": a.Description, "target": a.Target,
			"passed": a.Passed, "evidence": a.Evidence, "invalid": a.Invalid,
		})
	}
	return map[string]any{"passed": r.Passed, "outcomes": outcomes, "acceptance": acceptance}
}

type Verifier struct {
	Timeout      time.Duration
	ArtifactsDir string
	TailChars    int
}

// Run 按声明顺序执行验证命令，遇到第一个失败即停止。
func (v Verifier) Run(cwd string, commands []config.VerifyCommand, attempt int) (Report, error) {
	report := Report{Passed: true}
	for _, c := range commands {
		o, err := v.runOne(cwd, c.Name, c.Command, attempt, "verify")
		if err != nil {
			return Report{}, err
		}
		report.Outcomes = append(report.Outcomes, o)
		if o.ExitCode != 0 {
			report.Passed = false
			break
		}
	}
	return report, nil
}

// RunAll 先按顺序执行验证命令，全部通过后再校验验收标准。
// 任一验证命令失败时跳过验收标准（依赖的测试环境已经不可信）。
func (v Verifier) RunAll(
	cwd string, commands []config.VerifyCommand, acceptance []planning.Acceptance, attempt int,
) (Report, error) {
	report, err := v.Run(cwd, commands, attempt)
	if err != nil {
		return Report{}, err
	}
	if !report.Passed {
		return report, nil
	}
	outcomes, err := v.checkAcceptance(cwd, commands, acceptance, attempt, report.Outcomes)
	if err != nil {
		return Report{}, err
	}
	report.Acceptance = outcomes
	for _, a := range outcomes {
		if !a.Passed {
			report.Passed = false
			break
		}
	}
	return report, nil
}

func (v Verifier) runOne(cwd, name, command string, attempt int, prefix string) (Outcome, error) {
	if err := os.MkdirAll(v.ArtifactsDir, 0o755); err != nil {
		return Outcome{}, err
	}
	tail := v.TailChars
	if tail <= 0 {
		tail = 3000
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), v.Timeout)
	defer cancel()
	cmd := tools.ShellCommand(ctx, command)
	cmd.Dir = cwd
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	code := 0
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		code = 124
		fmt.Fprintf(&buf, "\n超时（>%s）", v.Timeout)
	} else if runErr != nil {
		var exitErr interface{ ExitCode() int }
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = 1
			fmt.Fprintf(&buf, "\n执行失败: %v", runErr)
		}
	}

	output := buf.String()
	logPath := filepath.Join(v.ArtifactsDir, fmt.Sprintf("%s-%d-%s.log", prefix, attempt, sanitizeName(name)))
	if err := os.WriteFile(logPath, []byte(output), 0o644); err != nil {
		return Outcome{}, err
	}
	return Outcome{
		Name:      name,
		Command:   command,
		ExitCode:  code,
		DurationS: time.Since(start).Seconds(),
		LogPath:   filepath.ToSlash(logPath),
		Tail:      lastRunes(output, tail),
	}, nil
}

func sanitizeName(name string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '-'
	}, name)
	if out == "" {
		return "check"
	}
	return out
}

// checkAcceptance 逐条校验验收标准。模型给出的 selector 会再次做元字符检查（纵深防御）。
// 与已执行的验证命令完全等价的 command 检查会复用结果，避免重复执行测试。
func (v Verifier) checkAcceptance(
	cwd string, commands []config.VerifyCommand, acceptance []planning.Acceptance, attempt int, prior []Outcome,
) ([]AcceptanceOutcome, error) {
	byName := make(map[string]string, len(commands))
	for _, c := range commands {
		byName[c.Name] = c.Command
	}
	executed := make(map[string]Outcome, len(prior))
	for _, o := range prior {
		executed[o.Name] = o
	}

	out := make([]AcceptanceOutcome, 0, len(acceptance))
	for _, ac := range acceptance {
		res := AcceptanceOutcome{ID: ac.ID, Description: ac.Description}
		switch ac.Check.Type {
		case planning.CheckCommand:
			cmd, ok := byName[ac.Check.Name]
			if !ok {
				res.Evidence = fmt.Sprintf("验证命令 %q 未配置", ac.Check.Name)
				break
			}
			if o, reused := executed[ac.Check.Name]; reused {
				res.Target = cmd
				res.Passed = o.ExitCode == 0
				res.Evidence = fmt.Sprintf("复用验证命令 `%s` 结果：退出码 %d，日志 %s", cmd, o.ExitCode, o.LogPath)
				break
			}
			r, err := v.runAcceptance(cwd, "acceptance-"+ac.ID, cmd, attempt)
			if err != nil {
				return nil, err
			}
			res.Target = cmd
			res.Passed = r.Passed
			res.Evidence = r.Evidence
		case planning.CheckTest:
			base, ok := byName[ac.Check.Name]
			if !ok {
				res.Evidence = fmt.Sprintf("验证命令 %q 未配置", ac.Check.Name)
				break
			}
			cmd := base
			if ac.Check.Selector != "" {
				if unsafeSelector.MatchString(ac.Check.Selector) {
					res.Evidence = "选择器含禁止字符，已拒绝执行"
					break
				}
				cmd = base + " " + ac.Check.Selector
			}
			r, err := v.runAcceptance(cwd, "acceptance-"+ac.ID, cmd, attempt)
			if err != nil {
				return nil, err
			}
			res.Target = cmd
			res.Passed = r.Passed
			res.Evidence = r.Evidence
			// 选择器没选中任何测试：标准本身写错了，不是代码没修好。
			// 注意即使退出码为 0 也算无效——某些运行器（如 `go test -run 不存在`）
			// 会返回成功，但这条检查实际上什么都没验证，属于假通过。
			if looksLikeNoTests(r.Evidence) {
				res.Invalid = true
				res.Passed = false
				res.Evidence = "选择器未匹配到任何测试（验收标准本身不可执行）\n" + r.Evidence
			}
		case planning.CheckFileExists:
			res.Target = ac.Check.Path
			_, err := os.Stat(filepath.Join(cwd, filepath.FromSlash(ac.Check.Path)))
			res.Passed = err == nil
			res.Evidence = fmt.Sprintf("文件 %s 存在=%v", ac.Check.Path, res.Passed)
		case planning.CheckGrep:
			res.Target = ac.Check.Path
			got, detail, err := grepFile(filepath.Join(cwd, filepath.FromSlash(ac.Check.Path)), ac.Check.Pattern)
			if err != nil {
				res.Evidence = detail
				break
			}
			want := ac.Check.Expect == "present"
			res.Passed = got == want
			res.Evidence = detail
		default:
			res.Evidence = "不支持的验收标准类型: " + string(ac.Check.Type)
		}
		out = append(out, res)
	}
	return out, nil
}

var unsafeSelector = regexp.MustCompile("[;&|`<>$()\n\r\t]")

// acceptanceRun 是一次验收命令执行的结果。
type acceptanceRun struct {
	Passed   bool
	Evidence string
}

func (v Verifier) runAcceptance(cwd, name, command string, attempt int) (acceptanceRun, error) {
	o, err := v.runOne(cwd, name, command, attempt, "acceptance")
	if err != nil {
		return acceptanceRun{}, err
	}
	return acceptanceRun{
		Passed:   o.ExitCode == 0,
		Evidence: fmt.Sprintf("退出码 %d，日志 %s\n%s", o.ExitCode, o.LogPath, o.Tail),
	}, nil
}

func grepFile(path, pattern string) (bool, string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false, "正则无效: " + err.Error(), err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, "无法读取 " + path + ": " + err.Error(), nil
	}
	for i, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if re.MatchString(line) {
			return true, fmt.Sprintf("%s:%d: %s", path, i+1, strings.TrimSpace(line)), nil
		}
	}
	return false, "未匹配到 " + pattern, nil
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

var (
	skipPattern     = regexp.MustCompile(`pytest\.mark\.skip|@pytest\.mark\.xfail|\bxfail\b|\bit\.skip\(|\bdescribe\.skip\(|\btest\.skip\(|\bit\.todo\(|@unittest\.skip|\bt\.Skip\(`)
	testDefPattern  = regexp.MustCompile(`^\s*(def test_|it\(|test\(|func Test|@Test\b)`)
	testFilePattern = regexp.MustCompile(`(^|/)(tests?/|__tests__/)|(test_[^/]*\.py$)|(\.(test|spec)\.[jt]sx?$)|(_test\.go$)`)
)

// Anticheat 检测可能的“作弊”改动，返回可疑原因列表；空切片表示未发现问题。
func Anticheat(changes []workspace.FileChange, diff string, verifyConfigPatterns []string) []string {
	var findings []string
	for _, c := range changes {
		posix := filepath.ToSlash(c.Path)
		for _, pat := range verifyConfigPatterns {
			if policy.GlobMatch(posix, pat) {
				findings = append(findings, "修改了验证配置文件: "+posix)
				break
			}
		}
		if c.Status == 'D' && testFilePattern.MatchString(posix) {
			findings = append(findings, "删除了测试文件: "+posix)
		}
	}

	added, removed := changedLines(diff)

	addedSkips := countMatches(skipPattern, added)
	removedSkips := countMatches(skipPattern, removed)
	if addedSkips > removedSkips {
		findings = append(findings, fmt.Sprintf("新增了跳过/预期失败标记 %d 处", addedSkips-removedSkips))
	}

	addedTests := countMatches(testDefPattern, added)
	removedTests := countMatches(testDefPattern, removed)
	if removedTests > addedTests {
		findings = append(findings, fmt.Sprintf("测试用例数量减少 %d 个", removedTests-addedTests))
	}
	return findings
}

func countMatches(re *regexp.Regexp, lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(re.FindAllString(l, -1))
	}
	return n
}

func changedLines(diff string) (added, removed []string) {
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "+"):
			added = append(added, line[1:])
		case strings.HasPrefix(line, "-"):
			removed = append(removed, line[1:])
		}
	}
	return added, removed
}
