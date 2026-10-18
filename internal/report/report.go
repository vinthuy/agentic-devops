package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentdev/internal/config"
	"agentdev/internal/store"
)

// WriteRunArtifacts 写入 events.jsonl 与 report.md，返回报告路径。
func WriteRunArtifacts(repo store.Repository, cfg *config.App, taskID string) (string, error) {
	task, err := repo.GetTask(taskID)
	if err != nil {
		return "", err
	}
	events, err := repo.ListEvents(taskID)
	if err != nil {
		return "", err
	}
	violations, err := repo.ListViolations(taskID)
	if err != nil {
		return "", err
	}
	verifications, err := repo.ListVerifications(taskID)
	if err != nil {
		return "", err
	}

	dir := filepath.Join(cfg.RunsDir(), taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	var jsonl strings.Builder
	for _, e := range events {
		line, err := json.Marshal(e)
		if err != nil {
			return "", err
		}
		jsonl.Write(line)
		jsonl.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(jsonl.String()), 0o644); err != nil {
		return "", err
	}

	reportPath := filepath.Join(dir, "report.md")
	content := render(task, events, violations, verifications, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(reportPath, []byte(content), 0o644); err != nil {
		return "", err
	}
	return reportPath, nil
}

func render(task store.Task, events []store.Event, violations []store.Violation, verifications []store.Verification, generatedAt string) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	orDash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}

	line("# 任务报告 %s", task.ID)
	line("")
	line("- 生成时间: %s", generatedAt)
	line("- 仓库: %s", task.Repo)
	line("- 分级: %s", orDash(task.Level))
	line("- 最终状态: **%s**", task.Status)
	line("- 分支: %s", orDash(task.Branch))
	line("- 基线提交: %s", orDash(task.BaseSHA))
	line("- 提交: %s", orDash(task.CommitSHA))
	line("- 修复次数: %d", task.FixAttempts)
	line("- 预算: $%.2f（计费尚未实现，未统计实际花费）", task.BudgetUSD)
	line("")
	line("## 需求")
	line("")
	line("> %s", strings.ReplaceAll(task.Requirement, "\n", "\n> "))
	line("")
	line("## 验证记录")
	line("")
	if len(verifications) == 0 {
		line("无。")
	}
	for _, v := range verifications {
		mark := "失败"
		if v.Passed {
			mark = "通过"
		}
		line("### 第 %d 次: %s", v.Attempt, mark)
		if outcomes, ok := v.Report["outcomes"].([]any); ok {
			for _, o := range outcomes {
				m, _ := o.(map[string]any)
				line("- `%v` (%v): 退出码 %v，耗时 %vs，日志 `%v`",
					m["command"], m["name"], m["exit_code"], m["duration_s"], m["log_path"])
			}
		}
		line("")
	}

	line("## 策略违规")
	line("")
	if len(violations) == 0 {
		line("无。")
	}
	for _, v := range violations {
		flag := "记录"
		if v.Blocked {
			flag = "已拦截"
		}
		line("- %s [%s] %s: %s", v.TS, v.Kind, flag, v.Detail)
	}
	line("")

	line("## 事件时间线")
	line("")
	line("| 时间 | 事件 | 状态变化 |")
	line("| --- | --- | --- |")
	for _, e := range events {
		change := ""
		if e.ToStatus != "" {
			change = fmt.Sprintf("%s → %s", orDash(e.FromStatus), e.ToStatus)
		}
		line("| %s | %s | %s |", e.TS, e.Type, change)
	}
	line("")
	return b.String()
}
