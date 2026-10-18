package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"agentdev/internal/backend"
	"agentdev/internal/bench"
	"agentdev/internal/config"
	"agentdev/internal/cost"
	"agentdev/internal/llm"
	"agentdev/internal/orchestrator"
	"agentdev/internal/planning"
	"agentdev/internal/report"
	"agentdev/internal/state"
	"agentdev/internal/store"
	"agentdev/internal/triage"
)

const Version = "0.2.0"

const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
	ExitHuman = 3
)

// ExitErr 携带进程退出码。Msg 为空时不打印额外信息。
type ExitErr struct {
	Code int
	Msg  string
}

func (e *ExitErr) Error() string { return e.Msg }

func fail(code int, format string, args ...any) error {
	return &ExitErr{Code: code, Msg: fmt.Sprintf(format, args...)}
}

type options struct {
	home        string
	issueReader issueReader
}

// NewRoot 构造根命令。
func NewRoot() *cobra.Command {
	opts := &options{}
	root := &cobra.Command{
		Use:           "agentdev",
		Short:         "需求到 PR 的自动化工程流水线",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&opts.home, "home", "", "数据目录，默认读取 AGENTDEV_HOME 或 ~/.agentdev")

	root.AddCommand(versionCmd(), configCmd(opts), doctorCmd(opts), taskCmd(opts), runCmd(opts),
		costCmd(opts), benchCmd(opts))
	return root
}

// Execute 运行 CLI 并返回进程退出码。
func Execute(args []string, stdout, stderr io.Writer) int {
	root := NewRoot()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.Execute()
	if err == nil {
		return ExitOK
	}
	var exitErr *ExitErr
	if errors.As(err, &exitErr) {
		if exitErr.Msg != "" {
			fmt.Fprintln(stderr, "错误:", exitErr.Msg)
		}
		return exitErr.Code
	}
	fmt.Fprintln(stderr, "错误:", err)
	return ExitError
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "显示版本号",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), Version)
		},
	}
}

func loadConfig(opts *options) (*config.App, error) {
	home, err := config.ResolveHome(opts.home)
	if err != nil {
		return nil, fail(ExitError, "%v", err)
	}
	cfg, err := config.Load(home)
	if err != nil {
		return nil, fail(ExitUsage, "%v", err)
	}
	return cfg, nil
}

func openStore(cfg *config.App) (*store.SQLiteStore, error) {
	if err := os.MkdirAll(cfg.Home, 0o755); err != nil {
		return nil, err
	}
	return store.Open(cfg.DBPath())
}

func configCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "配置相关命令"}
	cmd.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "加载并校验配置文件",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "数据目录: %s\n", cfg.Home)
			fmt.Fprintf(out, "已配置仓库: %d\n", len(cfg.Repos))
			if problems := config.CheckRepoPaths(cfg); len(problems) > 0 {
				for _, p := range problems {
					fmt.Fprintf(out, "[FAIL] %s\n", p)
				}
				return fail(ExitError, "仓库配置存在问题")
			}
			fmt.Fprintln(out, "[OK] 配置校验通过")
			return nil
		},
	})
	return cmd
}

func doctorCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "检查运行环境",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			type check struct {
				ok       bool
				required bool
				msg      string
			}
			var checks []check
			_, gitErr := exec.LookPath("git")
			checks = append(checks, check{gitErr == nil, true, "git 可用"})
			_, ghErr := exec.LookPath("gh")
			checks = append(checks, check{ghErr == nil, false, "gh CLI 可用（推送功能需要）"})
			keyEnv := cfg.Models.APIKeyEnv
			checks = append(checks, check{os.Getenv(keyEnv) != "", false, keyEnv + " 已设置（真实模型需要）"})

			if err := os.MkdirAll(cfg.Home, 0o755); err != nil {
				checks = append(checks, check{false, true, "数据目录不可创建: " + err.Error()})
			} else {
				probe := filepath.Join(cfg.Home, ".write-probe")
				if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
					checks = append(checks, check{false, true, "数据目录不可写: " + err.Error()})
				} else {
					_ = os.Remove(probe)
					checks = append(checks, check{true, true, "数据目录可写: " + cfg.Home})
				}
			}

			if len(cfg.Repos) == 0 {
				checks = append(checks, check{false, true, "repos.yaml 中没有配置任何仓库"})
			}
			for _, p := range config.CheckRepoPaths(cfg) {
				checks = append(checks, check{false, true, p})
			}
			for name := range cfg.Repos {
				checks = append(checks, check{true, true, "仓库已配置: " + name})
			}

			failed := false
			out := cmd.OutOrStdout()
			for _, c := range checks {
				mark := "[OK]"
				if !c.ok {
					mark = "[FAIL]"
					if c.required {
						failed = true
					}
				}
				fmt.Fprintf(out, "%s %s\n", mark, c.msg)
			}
			if failed {
				return &ExitErr{Code: ExitError}
			}
			return nil
		},
	}
}

func taskCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{Use: "task", Short: "任务相关命令"}
	cmd.AddCommand(taskCreateCmd(opts), taskListCmd(opts), taskStatusCmd(opts), taskLogsCmd(opts),
		taskApproveCmd(opts), taskReplyCmd(opts), taskResumeCmd(opts), taskCancelCmd(opts))
	return cmd
}

func taskCreateCmd(opts *options) *cobra.Command {
	var (
		repoName  string
		text      string
		file      string
		level     string
		budget    float64
		title     string
		issue     string
		issueRepo string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "创建一个任务（状态为 QUEUED）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			repoCfg, ok := cfg.Repos[repoName]
			if !ok {
				return fail(ExitUsage, "仓库未配置: %s（请检查 repos.yaml）", repoName)
			}
			textSet := cmd.Flags().Changed("text")
			fileSet := cmd.Flags().Changed("file")
			issueSet := cmd.Flags().Changed("issue")
			if boolCount(textSet, fileSet, issueSet) != 1 {
				return fail(ExitUsage, "--text、--file、--issue 必须且只能指定一个")
			}
			requirement := text
			issueRef := issue
			if fileSet {
				data, err := os.ReadFile(file)
				if err != nil {
					return fail(ExitUsage, "读取需求文件失败: %v", err)
				}
				requirement = string(data)
			}
			if issueSet {
				number, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(issue), "#"), 10, 64)
				if err != nil || number < 1 {
					return fail(ExitUsage, "--issue 必须是正整数 Issue 编号")
				}
				ghRepo := strings.TrimSpace(issueRepo)
				if ghRepo == "" {
					ghRepo, err = githubRepoSlug(repoCfg, repoName)
					if err != nil {
						return fail(ExitUsage, "无法确定 GitHub 仓库，请通过 --issue-repo owner/name 指定：%v", err)
					}
				}
				reader := opts.issueReader
				if reader == nil {
					reader = ghIssueReader{}
				}
				issueData, err := reader.Read(cmd.Context(), ghRepo, number)
				if err != nil {
					return fail(ExitError, "读取 Issue 失败: %v", err)
				}
				title = issueData.Title
				requirement = issueData.Title
				if body := strings.TrimSpace(issueData.Body); body != "" {
					requirement += "\n\n" + body
				}
				issueRef = issueData.URL
				if issueRef == "" {
					issueRef = fmt.Sprintf("%s#%d", ghRepo, number)
				}
			}
			requirement = strings.TrimSpace(requirement)
			if requirement == "" {
				return fail(ExitUsage, "需求内容为空")
			}
			level = strings.ToUpper(strings.TrimSpace(level))
			levelOverride := cmd.Flags().Changed("level")
			if _, _, err := triage.Parse(level); err != nil {
				return fail(ExitUsage, "--level 只能是 S / M / L")
			}
			if !cmd.Flags().Changed("budget") {
				budget = repoCfg.BudgetUSD
			}
			if budget <= 0 {
				return fail(ExitUsage, "--budget 必须大于 0")
			}

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()
			task, err := st.CreateTask(store.NewTask{
				Repo:          repoName,
				Requirement:   requirement,
				BudgetUSD:     budget,
				Level:         level,
				LevelOverride: levelOverride,
				Title:         title,
				Source:        sourceOf(issue),
				SourceRef:     issueRef,
			})
			if err != nil {
				return err
			}
			levelDesc := "自动分级"
			if levelOverride {
				levelDesc = level + " 级（指定）"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "[OK] 已创建任务 %s（%s，%s，预算 $%.2f）\n",
				task.ID, repoName, levelDesc, budget)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&repoName, "repo", "", "owner/name，需已在 repos.yaml 中配置")
	f.StringVar(&text, "text", "", "需求文本")
	f.StringVar(&file, "file", "", "从文件读取需求")
	f.StringVar(&level, "level", "", "S / M / L，留空则由分级决定")
	f.Float64Var(&budget, "budget", 0, "预算上限（美元），默认取仓库配置")
	f.StringVar(&title, "title", "", "任务标题，用于分支名")
	f.StringVar(&issue, "issue", "", "从 GitHub 导入 Issue 编号（与 --text/--file 互斥）")
	f.StringVar(&issueRepo, "issue-repo", "", "Issue 所属 GitHub 仓库 owner/name（默认从 git remote 推断）")
	_ = cmd.MarkFlagRequired("repo")
	return cmd
}

func sourceOf(issue string) string {
	if issue != "" {
		return "issue"
	}
	return "cli"
}

func boolCount(values ...bool) int {
	n := 0
	for _, value := range values {
		if value {
			n++
		}
	}
	return n
}

type githubIssue struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
}

type issueReader interface {
	Read(ctx context.Context, repo string, number int64) (githubIssue, error)
}

type ghIssueReader struct{}

func (ghIssueReader) Read(ctx context.Context, repo string, number int64) (githubIssue, error) {
	return fetchGitHubIssue(ctx, repo, number)
}

func fetchGitHubIssue(parent context.Context, repo string, number int64) (githubIssue, error) {
	if !validRepoSlug(repo) {
		return githubIssue{}, fmt.Errorf("仓库名必须是 owner/name，实际 %q", repo)
	}
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "issue", "view", strconv.FormatInt(number, 10),
		"--repo", repo, "--json", "title,body,url")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return githubIssue{}, fmt.Errorf("gh issue view 失败: %s", strings.TrimSpace(stderr.String()))
	}
	var issue githubIssue
	if err := json.Unmarshal([]byte(stdout.String()), &issue); err != nil {
		return githubIssue{}, fmt.Errorf("解析 Issue 数据失败: %w", err)
	}
	if strings.TrimSpace(issue.Title) == "" {
		return githubIssue{}, errors.New("Issue 缺少标题")
	}
	return issue, nil
}

func validRepoSlug(repo string) bool {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, "-") {
			return false
		}
		for _, r := range part {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
				return false
			}
		}
	}
	return true
}

func githubRepoSlug(repo config.RepoConfig, configuredName string) (string, error) {
	cmd := exec.Command("git", "-C", repo.LocalPath, "remote", "get-url", repo.Remote)
	if out, err := cmd.Output(); err == nil {
		if slug, ok := parseGitHubSlug(strings.TrimSpace(string(out))); ok {
			return slug, nil
		}
	}
	// 配置名本身是 owner/name 时也可直接用作 GitHub slug。
	if validRepoSlug(configuredName) {
		return configuredName, nil
	}
	return "", fmt.Errorf("remote %q 的 URL 不是 github.com owner/name", repo.Remote)
}

func parseGitHubSlug(remote string) (string, bool) {
	var tail string
	switch {
	case strings.HasPrefix(remote, "git@github.com:"):
		tail = strings.TrimPrefix(remote, "git@github.com:")
	case strings.HasPrefix(remote, "https://github.com/"):
		tail = strings.TrimPrefix(remote, "https://github.com/")
	case strings.HasPrefix(remote, "http://github.com/"):
		tail = strings.TrimPrefix(remote, "http://github.com/")
	case strings.HasPrefix(remote, "ssh://git@github.com/"):
		tail = strings.TrimPrefix(remote, "ssh://git@github.com/")
	default:
		return "", false
	}
	tail = strings.TrimSuffix(strings.Trim(tail, "/"), ".git")
	return tail, validRepoSlug(tail)
}

func taskListCmd(opts *options) *cobra.Command {
	var statusFlag, repoFilter string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出任务",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			var status state.Status
			if statusFlag != "" {
				status = state.Status(strings.ToUpper(statusFlag))
				if !status.Valid() {
					return fail(ExitUsage, "未知状态: %s", statusFlag)
				}
			}
			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()
			tasks, err := st.ListTasks(status, repoFilter)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\t仓库\t级别\t状态\t修复\t创建时间")
			for _, t := range tasks {
				level := t.Level
				if level == "" {
					level = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", t.ID, t.Repo, level, t.Status, t.FixAttempts, t.CreatedAt)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&statusFlag, "status", "", "按状态过滤")
	cmd.Flags().StringVar(&repoFilter, "repo", "", "按仓库过滤")
	return cmd
}

func taskStatusCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "status <task-id>",
		Short: "查看任务详情与最近事件",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()
			task, err := st.GetTask(args[0])
			if errors.Is(err, store.ErrNotFound) {
				return fail(ExitUsage, "%v", err)
			}
			if err != nil {
				return err
			}
			events, err := st.ListEvents(task.ID)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			level := task.Level
			if level == "" {
				level = "-"
			}
			firstLine := strings.SplitN(task.Requirement, "\n", 2)[0]
			fmt.Fprintf(out, "%s  %s  级别 %s\n", task.ID, task.Repo, level)
			fmt.Fprintf(out, "状态: %s\n", task.Status)
			fmt.Fprintf(out, "需求: %s\n", firstLine)
			fmt.Fprintf(out, "分支: %s   提交: %s\n", orDash(task.Branch), orDash(task.CommitSHA))
			if task.PRURL != "" {
				fmt.Fprintf(out, "PR: %s\n", task.PRURL)
			}
			fmt.Fprintf(out, "修复次数: %d   审查次数: %d   CI 修复次数: %d   花费: $%.4f / 预算 $%.2f\n",
				task.FixAttempts, task.ReviewAttempts, task.CIAttempts, task.SpentUSD, task.BudgetUSD)
			if len(events) > 15 {
				events = events[len(events)-15:]
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "\n时间\t事件\t变化\t详情")
			for _, e := range events {
				change := ""
				if e.ToStatus != "" {
					change = fmt.Sprintf("%s → %s", orDash(e.FromStatus), e.ToStatus)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.TS, e.Type, change, report.EventDetail(e))
			}
			return w.Flush()
		},
	}
}

// taskLogsCmd 输出完整的审计时间线。与 status 只显示最近 15 条不同，
// logs 面向事后复盘：默认输出全部事件，可用 --limit 截取尾部。
func taskLogsCmd(opts *options) *cobra.Command {
	var (
		limit  int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "logs <task-id>",
		Short: "输出任务事件时间线（审计日志）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()
			task, err := st.GetTask(args[0])
			if errors.Is(err, store.ErrNotFound) {
				return fail(ExitUsage, "%v", err)
			}
			if err != nil {
				return err
			}
			events, err := st.ListEvents(task.ID)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(events)
			}
			if limit > 0 && len(events) > limit {
				events = events[len(events)-limit:]
			}
			fmt.Fprintf(out, "%s  %s  状态 %s  共 %d 条事件\n\n", task.ID, task.Repo, task.Status, len(events))
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "时间\t事件\t变化\t详情")
			for _, e := range events {
				change := ""
				if e.ToStatus != "" {
					change = fmt.Sprintf("%s → %s", orDash(e.FromStatus), e.ToStatus)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.TS, e.Type, change, report.EventDetail(e))
			}
			return w.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "只显示最后 N 条（0 表示全部）")
	cmd.Flags().BoolVar(&asJSON, "json", false, "以 JSON 输出事件")
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// loadTask 打开存储并读取任务，统一处理“不存在”的错误码。
func loadTask(cmd *cobra.Command, opts *options) (*config.App, *store.SQLiteStore, store.Task, error) {
	var zero store.Task
	cfg, err := loadConfig(opts)
	if err != nil {
		return nil, nil, zero, err
	}
	st, err := openStore(cfg)
	if err != nil {
		return nil, nil, zero, err
	}
	if len(cmd.Flags().Args()) == 0 {
		st.Close()
		return nil, nil, zero, fail(ExitUsage, "缺少任务 ID")
	}
	task, err := st.GetTask(cmd.Flags().Arg(0))
	if err != nil {
		st.Close()
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, zero, fail(ExitUsage, "%v", err)
		}
		return nil, nil, zero, err
	}
	return cfg, st, task, nil
}

func requireStatus(task store.Task, allowed ...state.Status) error {
	for _, s := range allowed {
		if task.Status == s {
			return nil
		}
	}
	names := make([]string, 0, len(allowed))
	for _, s := range allowed {
		names = append(names, string(s))
	}
	return fail(ExitUsage, "任务 %s 当前状态为 %s，该命令只接受 %s",
		task.ID, task.Status, strings.Join(names, " / "))
}

func taskApproveCmd(opts *options) *cobra.Command {
	var by, planPath string
	cmd := &cobra.Command{
		Use:   "approve <task-id>",
		Short: "审批 M 级任务，允许进入编码（可用 --plan 修改计划）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, st, task, err := loadTask(cmd, opts)
			if err != nil {
				return err
			}
			defer st.Close()
			if err := requireStatus(task, state.AwaitingApproval); err != nil {
				return err
			}

			payload := map[string]any{"approved_by": by}
			fields := map[string]any{}
			if planPath != "" {
				edited, verr := vettedPlanFile(cfg, task, planPath)
				if verr != nil {
					return verr
				}
				fields["plan_json"] = edited
				payload["plan_edited"] = true
			}

			updated, err := st.Transition(task.ID, task.Version, state.Coding, payload, fields)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if planPath != "" {
				fmt.Fprintf(out, "[OK] 已批准 %s（计划已更新），状态: %s\n", task.ID, updated.Status)
			} else {
				fmt.Fprintf(out, "[OK] 已批准 %s，状态: %s\n", task.ID, updated.Status)
			}
			fmt.Fprintln(out, "继续执行: agentdev run --task "+task.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&by, "by", "cli", "审批人标识，写入审计事件")
	cmd.Flags().StringVar(&planPath, "plan", "", "用该 JSON 文件替换计划（可修改验收标准或 allowed_paths）")
	return cmd
}

// vettedPlanFile 读取并校验人工修改后的计划。
//
// 审批可以修改计划，但不能绕过验证约束：验收标准仍需通过 planning.Validate，
// 且 scope 只能收紧或保持不变——人工不能借此扩大 Agent 的写入范围。
func vettedPlanFile(cfg *config.App, task store.Task, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fail(ExitUsage, "读取计划文件失败: %v", err)
	}
	plan, err := planning.Parse(string(data))
	if err != nil {
		return "", fail(ExitUsage, "计划文件不是合法 JSON: %v", err)
	}
	repoCfg, ok := cfg.Repos[task.Repo]
	if !ok {
		return "", fail(ExitUsage, "仓库未配置: %s", task.Repo)
	}
	names := map[string]bool{}
	for _, c := range repoCfg.VerifyCommands {
		names[c.Name] = true
	}
	if err := planning.Validate(plan, names); err != nil {
		return "", fail(ExitUsage, "计划校验失败: %v", err)
	}
	if plan.NeedsInfo() {
		return "", fail(ExitUsage, "审批用的计划不能是 needs_info，请先回复澄清")
	}

	if original, perr := planning.Parse(task.PlanJSON); perr == nil && len(original.Scope.AllowedPaths) > 0 {
		for _, p := range plan.Scope.AllowedPaths {
			if !scopeCovered(p, original.Scope.AllowedPaths) {
				return "", fail(ExitUsage,
					"修改后的写入范围超出原计划（%s）：审批只能收紧范围，不能扩大", p)
			}
		}
	}

	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// scopeCovered 判断单个 glob 是否被允许路径集合覆盖。
// 这里只做保守的字符串/前缀判断：宁可拒绝一次，也不放过扩大范围的情况。
func scopeCovered(glob string, allowed []string) bool {
	for _, a := range allowed {
		if a == glob {
			return true
		}
		prefix := strings.TrimSuffix(a, "/**")
		if prefix != a && (glob == prefix || strings.HasPrefix(glob, prefix+"/")) {
			return true
		}
	}
	return false
}

func taskReplyCmd(opts *options) *cobra.Command {
	var text, file string
	cmd := &cobra.Command{
		Use:   "reply <task-id>",
		Short: "回答任务的澄清问题，恢复规划",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, st, task, err := loadTask(cmd, opts)
			if err != nil {
				return err
			}
			defer st.Close()
			if err := requireStatus(task, state.NeedsInfo); err != nil {
				return err
			}
			if cmd.Flags().Changed("file") {
				data, rerr := os.ReadFile(file)
				if rerr != nil {
					return fail(ExitUsage, "读取回复文件失败: %v", rerr)
				}
				text = string(data)
			}
			text = strings.TrimSpace(text)
			if text == "" {
				return fail(ExitUsage, "回复内容为空（用 --text 或 --file）")
			}
			if err := st.RecordInteraction(task.ID, "reply", text); err != nil {
				return err
			}
			// 回复并入需求文本，规划器在下一轮会看到完整上下文。
			updated, err := st.Transition(task.ID, task.Version, state.Planning,
				map[string]any{"reply": truncateForEvent(text)},
				map[string]any{"requirement": task.Requirement + "\n\n[补充说明]\n" + text})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "[OK] 已记录回复，%s 状态: %s\n", task.ID, updated.Status)
			fmt.Fprintln(cmd.OutOrStdout(), "继续执行: agentdev run --task "+task.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&text, "text", "", "回复内容")
	cmd.Flags().StringVar(&file, "file", "", "从文件读取回复内容")
	return cmd
}

func taskResumeCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "resume <task-id>",
		Short: "把 NEEDS_HUMAN 的任务恢复到挂起前的状态",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, st, task, err := loadTask(cmd, opts)
			if err != nil {
				return err
			}
			defer st.Close()
			if err := requireStatus(task, state.NeedsHuman); err != nil {
				return err
			}
			target := task.PreviousStatus
			if target == "" {
				return fail(ExitUsage, "任务 %s 没有记录挂起前的状态，无法恢复", task.ID)
			}
			if !state.CanTransition(task.Status, target) {
				return fail(ExitUsage, "无法从 %s 恢复到 %s", task.Status, target)
			}
			// 人工恢复意味着再给一次机会：所有有上限的尝试计数都要重置，
			// 否则因审查/CI 达上限挂起的任务恢复后会立刻再次挂起。
			updated, err := st.Transition(task.ID, task.Version, target,
				map[string]any{
					"resumed_to": string(target), "fix_attempts_reset": true,
					"review_attempts_reset": true, "ci_attempts_reset": true,
				},
				map[string]any{"fix_attempts": 0, "review_attempts": 0, "ci_attempts": 0})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "[OK] 已恢复 %s 到 %s（修复次数已重置）\n", task.ID, updated.Status)
			fmt.Fprintln(cmd.OutOrStdout(), "继续执行: agentdev run --task "+task.ID)
			return nil
		},
	}
}

func taskCancelCmd(opts *options) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "cancel <task-id>",
		Short: "取消任务（终态，不可恢复）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, st, task, err := loadTask(cmd, opts)
			if err != nil {
				return err
			}
			defer st.Close()
			if state.Terminal[task.Status] {
				return fail(ExitUsage, "任务 %s 已处于终态 %s", task.ID, task.Status)
			}
			updated, err := st.Transition(task.ID, task.Version, state.Cancelled,
				map[string]any{"reason": orDash(reason)}, nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "[OK] 已取消 %s，状态: %s\n", task.ID, updated.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "取消原因，写入审计事件")
	return cmd
}

func truncateForEvent(s string) string {
	r := []rune(s)
	if len(r) <= 500 {
		return s
	}
	return string(r[:500]) + "..."
}

// levelLabel 在级别尚未确定时给出可读的说明，避免显示 "-"。
func levelLabel(level string) string {
	if level == "" {
		return "自动分级"
	}
	return level + " 级"
}

func costCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cost [task-id]",
		Short: "查看任务花费明细或成本看板",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fail(ExitUsage, "用法: agentdev cost <task-id> 或 agentdev cost report")
			}
			return costTaskRun(cmd, opts, args[0])
		},
	}
	cmd.AddCommand(&cobra.Command{Use: "report", Short: "查看所有任务的成本汇总看板", Args: cobra.NoArgs, RunE: costReportRun(opts)})
	return cmd
}

func costTaskRun(cmd *cobra.Command, opts *options, taskID string) error {
	cfg, st, task, err := loadTask(cmd, opts)
	if err != nil {
		return err
	}
	defer st.Close()
	entries, err := st.ListCosts(task.ID)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "任务 %s  预算 $%.2f  已用 $%.4f\n\n", task.ID, task.BudgetUSD, task.SpentUSD)
	if len(entries) == 0 {
		fmt.Fprintln(out, "无计费记录。")
	} else {
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "时间\t角色\t模型\t输入\t输出\t缓存读\t缓存写\t费用")
		for _, e := range entries {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t$%.6f\n",
				e.TS, e.Role, e.Model, e.InputTokens, e.OutputTokens,
				e.CacheReadTokens, e.CacheWriteTokens, e.CostUSD)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	if _, ok := cfg.PricingFor(); !ok {
		fmt.Fprintf(out, "\n[WARN] models.yaml 未配置模型 %s 的单价，费用按 $0 统计。\n", cfg.Models.Model)
	}
	return nil
}

type costAggregate struct {
	Calls, Input, Output int
	Cost                 float64
}

func costReportRun(opts *options) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig(opts)
		if err != nil {
			return err
		}
		st, err := openStore(cfg)
		if err != nil {
			return err
		}
		defer st.Close()
		entries, err := st.ListAllCosts()
		if err != nil {
			return err
		}
		tasks, err := st.ListTasks("", "")
		if err != nil {
			return err
		}
		byRole, byModel := map[string]*costAggregate{}, map[string]*costAggregate{}
		byTask := map[string]float64{}
		var total float64
		for _, e := range entries {
			total += e.CostUSD
			addCost(byRole, e.Role, e)
			addCost(byModel, e.Model, e)
			byTask[e.TaskID] += e.CostUSD
		}
		var missingPricing []string
		for _, task := range tasks {
			events, err := st.ListEvents(task.ID)
			if err != nil {
				return err
			}
			for _, event := range events {
				if event.Type == "cost_pricing_missing" {
					model, _ := event.Payload["model"].(string)
					missingPricing = append(missingPricing, fmt.Sprintf("%s (%s)", task.ID, model))
				}
			}
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "成本看板：%d 个任务，%d 次模型调用，累计 $%.6f\n\n", len(tasks), len(entries), total)
		if len(missingPricing) > 0 {
			fmt.Fprintf(out, "[WARN] %d 个任务缺少模型单价，$0 计费不代表免费：%s\n\n",
				len(missingPricing), strings.Join(missingPricing, ", "))
		}
		printCostAggregate(out, "按角色", byRole)
		printCostAggregate(out, "按模型", byModel)
		if len(byTask) > 0 {
			fmt.Fprintln(out, "花费最高的任务：")
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "任务 ID\t仓库\t状态\t费用")
			taskByID := make(map[string]store.Task, len(tasks))
			for _, task := range tasks {
				taskByID[task.ID] = task
			}
			type item struct {
				id    string
				spend float64
			}
			var ranked []item
			for id, spend := range byTask {
				ranked = append(ranked, item{id, spend})
			}
			sort.Slice(ranked, func(i, j int) bool { return ranked[i].spend > ranked[j].spend })
			for i, it := range ranked {
				if i == 10 {
					break
				}
				t := taskByID[it.id]
				fmt.Fprintf(w, "%s\t%s\t%s\t$%.6f\n", it.id, t.Repo, t.Status, it.spend)
			}
			if err := w.Flush(); err != nil {
				return err
			}
		}
		if len(entries) == 0 {
			fmt.Fprintln(out, "\n没有模型调用记录；若使用了模型，请检查是否有 cost_pricing_missing 事件。")
		}
		return nil
	}
}

func addCost(groups map[string]*costAggregate, key string, e store.CostEntry) {
	a := groups[key]
	if a == nil {
		a = &costAggregate{}
		groups[key] = a
	}
	a.Calls++
	a.Input += e.InputTokens
	a.Output += e.OutputTokens
	a.Cost += e.CostUSD
}

func printCostAggregate(out io.Writer, title string, groups map[string]*costAggregate) {
	fmt.Fprintf(out, "%s：\n", title)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "名称\t调用\t输入\t输出\t费用")
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		a := groups[key]
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t$%.6f\n", key, a.Calls, a.Input, a.Output, a.Cost)
	}
	_ = w.Flush()
	fmt.Fprintln(out)
}

func runCmd(opts *options) *cobra.Command {
	var backendName, scriptPath, taskID string
	var maxTasks int
	var push bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "推进任务到终态或挂起（含中断后恢复）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			out := cmd.OutOrStdout()

			// 先确认有活可干，再构造后端：没有任务时不应因为缺少 API key 而失败。
			if taskID != "" {
				if _, err := st.GetTask(taskID); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return fail(ExitUsage, "%v", err)
					}
					return err
				}
				orch, err := buildOrchestrator(cfg, st, backendName, scriptPath, push)
				if err != nil {
					return err
				}
				return runOne(cmd, cfg, st, orch, taskID)
			}
			if _, ok, err := st.NextRunnable(); err != nil {
				return err
			} else if !ok {
				fmt.Fprintln(out, "没有可推进的任务")
				return nil
			}
			if maxTasks <= 0 {
				maxTasks = 1
			}
			for i := 0; i < maxTasks; i++ {
				next, ok, err := st.NextRunnable()
				if err != nil {
					return err
				}
				if !ok {
					if i == 0 {
						fmt.Fprintln(out, "没有可推进的任务")
					}
					return nil
				}
				// 每个任务使用独立的后端实例：脚本/状态不应跨任务泄漏。
				orch, err := buildOrchestrator(cfg, st, backendName, scriptPath, push)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "开始执行 %s（%s，%s）\n", next.ID, next.Repo, levelLabel(next.Level))
				result, err := orch.RunTask(next.ID)
				if err != nil {
					return err
				}
				code, stopped := reportResult(out, cfg, result)
				if err := printDegradations(out, st, result.ID); err != nil {
					return err
				}
				if stopped {
					return code
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&backendName, "backend", "anthropic", "anthropic / fake")
	f.StringVar(&scriptPath, "script", "", "fake 后端的脚本 JSON 文件")
	f.StringVar(&taskID, "task", "", "只推进指定的任务（可为挂起后已恢复的任务）")
	f.IntVar(&maxTasks, "max", 1, "最多连续推进几个任务")
	f.BoolVar(&push, "push", false, "本次强制开启推送与创建草稿 PR（覆盖 delivery.enabled）")
	return cmd
}

func runOne(cmd *cobra.Command, cfg *config.App, st store.Repository, orch *orchestrator.Orchestrator, taskID string) error {
	task, err := st.GetTask(taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fail(ExitUsage, "%v", err)
		}
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "开始执行 %s（%s，%s）\n", task.ID, task.Repo, levelLabel(task.Level))
	result, err := orch.RunTask(task.ID)
	if err != nil {
		return err
	}
	code, stopped := reportResult(out, cfg, result)
	if err := printDegradations(out, st, result.ID); err != nil {
		return err
	}
	if stopped {
		return code
	}
	return nil
}

func printDegradations(out io.Writer, st store.Repository, taskID string) error {
	events, err := st.ListEvents(taskID)
	if err != nil {
		return err
	}
	degradations := report.Degradations(events)
	if len(degradations) == 0 {
		return nil
	}
	fmt.Fprintf(out, "[WARN] 本次运行有 %d 处降级或异常：\n", len(degradations))
	for _, d := range degradations {
		fmt.Fprintf(out, "[WARN] - %s\n", d.SummaryLine())
	}
	fmt.Fprintln(out, "[WARN] 请查看运行报告中的“降级与异常”部分后再审阅结果。")
	return nil
}

// reportResult 打印执行结果。第二个返回值表示是否需要以特定退出码结束。
func reportResult(out io.Writer, cfg *config.App, result store.Task) (error, bool) {
	fmt.Fprintf(out, "最终状态: %s\n", result.Status)
	if result.CommitSHA != "" {
		fmt.Fprintf(out, "提交: %s  分支: %s\n", result.CommitSHA, result.Branch)
	}
	if result.PRURL != "" {
		fmt.Fprintf(out, "PR: %s\n", result.PRURL)
	}
	if cfg != nil {
		fmt.Fprintf(out, "花费: $%.4f / 预算 $%.2f\n", result.SpentUSD, result.BudgetUSD)
		fmt.Fprintf(out, "报告: %s\n", filepath.Join(cfg.RunsDir(), result.ID, "report.md"))
	}
	switch result.Status {
	case state.NeedsHuman, state.NeedsInfo, state.AwaitingApproval:
		return &ExitErr{Code: ExitHuman}, true
	case state.Failed, state.Rejected:
		return &ExitErr{Code: ExitError}, true
	}
	return nil, false
}

// benchCmd 运行可复现的基准集，输出成功率、成本与修复轮数。
func benchCmd(opts *options) *cobra.Command {
	var (
		manifestPath string
		backendName  string
		scriptPath   string
		only         string
		workDir      string
		keep         bool
		asJSON       bool
		minRate      float64
		repeat       int
	)
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "运行基准集并测量成功率与成本",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts)
			if err != nil {
				return err
			}
			manifest, err := bench.Load(manifestPath)
			if err != nil {
				return fail(ExitUsage, "%v", err)
			}
			if only != "" {
				wanted := map[string]bool{}
				for _, id := range strings.Split(only, ",") {
					wanted[strings.TrimSpace(id)] = true
				}
				var filtered []bench.Case
				for _, c := range manifest.Cases {
					if wanted[c.ID] {
						filtered = append(filtered, c)
					}
				}
				if len(filtered) == 0 {
					return fail(ExitUsage, "--only 没有匹配任何用例")
				}
				manifest.Cases = filtered
			}

			benchOpts := bench.Options{
				WorkDir: workDir,
				Keep:    keep,
				Repeat:  repeat,
				// 进度写 stderr，stdout 留给表格或 --json，避免机器可读输出被污染。
				Log: cmd.ErrOrStderr(),
				BackendFor: func(bench.Case) (backend.ExecutionBackend, error) {
					return makeBackend(cfg, backendName, scriptPath)
				},
			}
			if backendName != "fake" {
				key := os.Getenv(cfg.Models.APIKeyEnv)
				if key == "" {
					return fail(ExitUsage, "未设置环境变量 %s（见 models.yaml 的 api_key_env）", cfg.Models.APIKeyEnv)
				}
				benchOpts.Model = &bench.ModelConfig{
					Client:    llm.NewClient(cfg.Models.BaseURL, key),
					Model:     cfg.Models.Model,
					MaxTokens: cfg.Models.MaxTokens,
					Pricing:   pricingTable(cfg),
				}
			}

			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			summary, outcomes, err := bench.RunManifest(ctx, filepath.Dir(manifestPath), manifest, benchOpts)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(map[string]any{"summary": summary, "outcomes": outcomes})
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "用例\t轮次\t结果\t级别\t花费\t修复\t审查\tCI\t降级")
			for _, o := range outcomes {
				mark := "FAIL"
				if o.Passed {
					mark = "PASS"
				}
				fmt.Fprintf(w, "%s\t%d\t%s\t%s\t$%.4f\t%d\t%d\t%d\t%d\n",
					o.ID, o.Run, mark, orDash(o.Level), o.SpentUSD,
					o.FixAttempts, o.ReviewAttempts, o.CIAttempts, o.Degradations)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if stats := bench.ByCase(outcomes); len(stats) > 0 && repeat > 1 {
				fmt.Fprintln(out, "\n按用例统计：")
				sw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(sw, "用例\t通过/运行\t通过率")
				for _, st := range stats {
					fmt.Fprintf(sw, "%s\t%d/%d\t%.0f%%\n", st.ID, st.Passed, st.Runs,
						float64(st.Passed)/float64(st.Runs)*100)
				}
				if err := sw.Flush(); err != nil {
					return err
				}
			}
			fmt.Fprintf(out, "\n成功率 %d/%d 次运行 = %.0f%%   用例数 %d   人工介入 %.0f%%   平均修复 %.2f 轮   平均花费 $%.4f   总花费 $%.4f\n",
				summary.Passed, summary.Trials, summary.SuccessRate*100, summary.Total,
				summary.HumanRate*100, summary.MeanFixRounds, summary.MeanCostUSD, summary.TotalCostUSD)
			if minRate > 0 && summary.SuccessRate < minRate {
				return fail(ExitError, "成功率 %.0f%% 低于门槛 %.0f%%", summary.SuccessRate*100, minRate*100)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&manifestPath, "manifest", filepath.Join("bench", "tasks.yaml"), "基准清单路径")
	f.StringVar(&backendName, "backend", "anthropic", "anthropic / fake")
	f.StringVar(&scriptPath, "script", "", "fake 后端的脚本 JSON（每个用例重新读取）")
	f.StringVar(&only, "only", "", "只运行指定用例，逗号分隔")
	f.StringVar(&workDir, "workdir", "", "临时工作目录（默认系统临时目录）")
	f.BoolVar(&keep, "keep", false, "保留临时目录以便排查")
	f.BoolVar(&asJSON, "json", false, "以 JSON 输出")
	f.Float64Var(&minRate, "min-rate", 0, "成功率低于该值（0-1）时以退出码 1 结束")
	f.IntVar(&repeat, "repeat", 1, "每个用例重复运行次数，用于观察波动")
	return cmd
}

func buildOrchestrator(cfg *config.App, st store.Repository, backendName, scriptPath string, push bool) (*orchestrator.Orchestrator, error) {
	be, err := makeBackend(cfg, backendName, scriptPath)
	if err != nil {
		return nil, err
	}
	opts := []orchestrator.Option{}
	if backendName != "fake" {
		key := os.Getenv(cfg.Models.APIKeyEnv)
		if key == "" {
			return nil, fail(ExitUsage, "未设置环境变量 %s（见 models.yaml 的 api_key_env）", cfg.Models.APIKeyEnv)
		}
		opts = append(opts, orchestrator.WithModel(
			llm.NewClient(cfg.Models.BaseURL, key), cfg.Models.Model, cfg.Models.MaxTokens, pricingTable(cfg)))
	}
	if push {
		opts = append(opts, orchestrator.WithDelivery(true))
	}
	return orchestrator.New(st, cfg, be, opts...), nil
}

func pricingTable(cfg *config.App) cost.Table {
	out := cost.Table{}
	for name, p := range cfg.Models.Pricing {
		out[name] = cost.Rates{
			Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite,
		}
	}
	return out
}

func makeBackend(cfg *config.App, name, scriptPath string) (backend.ExecutionBackend, error) {
	switch name {
	case "anthropic":
		key := os.Getenv(cfg.Models.APIKeyEnv)
		if key == "" {
			return nil, fail(ExitUsage, "未设置环境变量 %s（见 models.yaml 的 api_key_env）", cfg.Models.APIKeyEnv)
		}
		return backend.NewAnthropic(
			llm.NewClient(cfg.Models.BaseURL, key), cfg.Models.Model, cfg.Models.MaxTokens), nil
	case "fake":
		if scriptPath == "" {
			return nil, fail(ExitUsage, "fake 后端需要 --script 指定脚本文件")
		}
		data, err := os.ReadFile(scriptPath)
		if err != nil {
			return nil, fail(ExitUsage, "读取脚本失败: %v", err)
		}
		var script [][]backend.ToolCall
		if err := json.Unmarshal(data, &script); err != nil {
			return nil, fail(ExitUsage, "脚本格式错误: %v", err)
		}
		return backend.NewFake(script), nil
	}
	return nil, fail(ExitUsage, "未知后端: %s", name)
}
