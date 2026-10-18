package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"agentdev/internal/backend"
	"agentdev/internal/config"
	"agentdev/internal/orchestrator"
	"agentdev/internal/state"
	"agentdev/internal/store"
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
	home string
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

	root.AddCommand(versionCmd(), configCmd(opts), doctorCmd(opts), taskCmd(opts), runCmd(opts))
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
	cmd.AddCommand(taskCreateCmd(opts), taskListCmd(opts), taskStatusCmd(opts))
	return cmd
}

func taskCreateCmd(opts *options) *cobra.Command {
	var (
		repoName string
		text     string
		file     string
		level    string
		budget   float64
		title    string
		issue    string
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
			if textSet == fileSet {
				return fail(ExitUsage, "--text 与 --file 必须二选一")
			}
			requirement := text
			if fileSet {
				data, err := os.ReadFile(file)
				if err != nil {
					return fail(ExitUsage, "读取需求文件失败: %v", err)
				}
				requirement = string(data)
			}
			requirement = strings.TrimSpace(requirement)
			if requirement == "" {
				return fail(ExitUsage, "需求内容为空")
			}
			level = strings.ToUpper(level)
			if level != "S" && level != "M" && level != "L" {
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
				Repo:        repoName,
				Requirement: requirement,
				BudgetUSD:   budget,
				Level:       level,
				Title:       title,
				Source:      sourceOf(issue),
				SourceRef:   issue,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "[OK] 已创建任务 %s（%s，%s 级）\n", task.ID, repoName, level)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&repoName, "repo", "", "owner/name，需已在 repos.yaml 中配置")
	f.StringVar(&text, "text", "", "需求文本")
	f.StringVar(&file, "file", "", "从文件读取需求")
	f.StringVar(&level, "level", "S", "S / M / L")
	f.Float64Var(&budget, "budget", 0, "预算上限（美元），默认取仓库配置")
	f.StringVar(&title, "title", "", "任务标题，用于分支名")
	f.StringVar(&issue, "issue", "", "关联的 Issue 编号")
	_ = cmd.MarkFlagRequired("repo")
	return cmd
}

func sourceOf(issue string) string {
	if issue != "" {
		return "issue"
	}
	return "cli"
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
			fmt.Fprintf(out, "修复次数: %d   预算: $%.2f\n", task.FixAttempts, task.BudgetUSD)
			if len(events) > 15 {
				events = events[len(events)-15:]
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "\n时间\t事件\t变化")
			for _, e := range events {
				change := ""
				if e.ToStatus != "" {
					change = fmt.Sprintf("%s → %s", orDash(e.FromStatus), e.ToStatus)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", e.TS, e.Type, change)
			}
			return w.Flush()
		},
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func runCmd(opts *options) *cobra.Command {
	var backendName, scriptPath string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "取出一个 QUEUED 任务并执行到终态或挂起",
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

			next, ok, err := st.NextQueued()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !ok {
				fmt.Fprintln(out, "没有排队中的任务")
				return nil
			}
			be, err := makeBackend(cfg, backendName, scriptPath)
			if err != nil {
				return err
			}
			level := next.Level
			if level == "" {
				level = "-"
			}
			fmt.Fprintf(out, "开始执行 %s（%s，%s 级）\n", next.ID, next.Repo, level)
			result, err := orchestrator.New(st, cfg, be).RunTask(next.ID)
			if err != nil {
				return err
			}

			fmt.Fprintf(out, "最终状态: %s\n", result.Status)
			if result.CommitSHA != "" {
				fmt.Fprintf(out, "提交: %s  分支: %s\n", result.CommitSHA, result.Branch)
			}
			fmt.Fprintf(out, "报告: %s\n", filepath.Join(cfg.RunsDir(), result.ID, "report.md"))
			switch result.Status {
			case state.NeedsHuman:
				return &ExitErr{Code: ExitHuman}
			case state.Failed, state.Rejected:
				return &ExitErr{Code: ExitError}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&backendName, "backend", "anthropic", "anthropic / fake")
	cmd.Flags().StringVar(&scriptPath, "script", "", "fake 后端的脚本 JSON 文件")
	return cmd
}

func makeBackend(cfg *config.App, name, scriptPath string) (backend.ExecutionBackend, error) {
	switch name {
	case "anthropic":
		key := os.Getenv(cfg.Models.APIKeyEnv)
		if key == "" {
			return nil, fail(ExitUsage, "未设置环境变量 %s（见 models.yaml 的 api_key_env）", cfg.Models.APIKeyEnv)
		}
		return backend.NewAnthropic(cfg.Models.Model, cfg.Models.MaxTokens, cfg.Models.BaseURL, key), nil
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
