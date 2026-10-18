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
	Passed   bool
	Outcomes []Outcome
}

// FailureSummary 返回第一个失败命令的摘要，供修复轮次使用。
func (r Report) FailureSummary() string {
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
	return map[string]any{"passed": r.Passed, "outcomes": outcomes}
}

type Verifier struct {
	Timeout      time.Duration
	ArtifactsDir string
	TailChars    int
}

// Run 按声明顺序执行验证命令，遇到第一个失败即停止。
func (v Verifier) Run(cwd string, commands []config.VerifyCommand, attempt int) (Report, error) {
	if err := os.MkdirAll(v.ArtifactsDir, 0o755); err != nil {
		return Report{}, err
	}
	tail := v.TailChars
	if tail <= 0 {
		tail = 3000
	}
	report := Report{Passed: true}
	for _, c := range commands {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), v.Timeout)
		cmd := tools.ShellCommand(ctx, c.Command)
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
		cancel()

		output := buf.String()
		logPath := filepath.Join(v.ArtifactsDir, fmt.Sprintf("verify-%d-%s.log", attempt, c.Name))
		if err := os.WriteFile(logPath, []byte(output), 0o644); err != nil {
			return Report{}, err
		}
		o := Outcome{
			Name:      c.Name,
			Command:   c.Command,
			ExitCode:  code,
			DurationS: time.Since(start).Seconds(),
			LogPath:   filepath.ToSlash(logPath),
			Tail:      lastRunes(output, tail),
		}
		report.Outcomes = append(report.Outcomes, o)
		if code != 0 {
			report.Passed = false
			break
		}
	}
	return report, nil
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
