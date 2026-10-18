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
	costs, err := repo.ListCosts(taskID)
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
	content := render(task, events, violations, verifications, costs, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(reportPath, []byte(content), 0o644); err != nil {
		return "", err
	}
	return reportPath, nil
}

func render(
	task store.Task, events []store.Event, violations []store.Violation,
	verifications []store.Verification, costs []store.CostEntry, generatedAt string,
) string {
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
	line("- 来源: %s", reportSource(task))
	line("- 分级: %s", orDash(task.Level))
	line("- 分级来源: %s", triageSource(events, costs))
	line("- 计划来源: %s", planSource(events, costs))
	line("- 最终状态: **%s**", task.Status)
	line("- 分支: %s", orDash(task.Branch))
	line("- 基线提交: %s", orDash(task.BaseSHA))
	line("- 提交: %s", orDash(task.CommitSHA))
	if task.PRURL != "" {
		line("- 草稿 PR: %s (#%d)", task.PRURL, task.PRNumber)
	}
	line("- 修复次数: %d", task.FixAttempts)
	line("- 预算: $%.2f，已用: $%.4f", task.BudgetUSD, task.SpentUSD)
	line("")

	// 降级必须显眼：否则一份降级跑出来的报告会和完全跑通的报告无法区分。
	if degs := Degradations(events); len(degs) > 0 {
		line("## 降级与异常")
		line("")
		line("本次运行有 %d 处偏离设计承诺，结论需人工确认：", len(degs))
		line("")
		for _, d := range degs {
			if d.Detail == "" {
				line("- `%s` %s：%s", d.TS, d.Type, d.Label)
				continue
			}
			line("- `%s` %s：%s —— %s", d.TS, d.Type, d.Label, d.Detail)
		}
		line("")
	}

	var excluded []string
	for _, e := range events {
		if e.Type == "commit_artifacts_excluded" {
			excluded = append(excluded, payloadStrings(e.Payload, "paths")...)
		}
	}
	if len(excluded) > 0 {
		line("## 提交审计")
		line("")
		line("以下工具生成的文件保留在工作区但未进入提交：")
		for _, p := range excluded {
			line("- `%s`", p)
		}
		line("")
	}

	var reviewEvents []store.Event
	for _, e := range events {
		if e.Type == "review_result" {
			reviewEvents = append(reviewEvents, e)
		}
	}
	if len(reviewEvents) > 0 {
		line("## 独立审查")
		line("")
		for _, e := range reviewEvents {
			line("- **%s**：%s", payloadString(e.Payload, "decision"), payloadString(e.Payload, "summary"))
			for _, finding := range payloadItems(e.Payload, "findings") {
				m, _ := finding.(map[string]any)
				where := payloadString(m, "path")
				if n := payloadFloat(m, "line"); n > 0 {
					where = fmt.Sprintf("%s:%d", where, int(n))
				}
				line("  - [%v] `%s`: %v", m["severity"], where, m["comment"])
			}
		}
		line("")
	}

	var ciEvents []store.Event
	for _, e := range events {
		if e.Type == "ci_failed" || (e.Type == "state_changed" && payloadString(e.Payload, "ci") != "") {
			ciEvents = append(ciEvents, e)
		}
	}
	if len(ciEvents) > 0 {
		line("## CI 结果")
		line("")
		for _, e := range ciEvents {
			if e.Type == "ci_failed" {
				line("- CI 失败：%s", payloadString(e.Payload, "summary"))
				continue
			}
			line("- `%s`：%s", payloadString(e.Payload, "ci"), payloadString(e.Payload, "summary"))
		}
		line("")
	}

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
		if acceptance, ok := v.Report["acceptance"].([]any); ok && len(acceptance) > 0 {
			line("")
			line("验收标准：")
			for _, a := range acceptance {
				m, _ := a.(map[string]any)
				flag := "未满足"
				if p, _ := m["passed"].(bool); p {
					flag = "满足"
				}
				line("- [%v] %v：%s", m["id"], m["description"], flag)
			}
		}
		line("")
	}

	line("## 成本")
	line("")
	if len(costs) == 0 {
		line("无计费记录（可能是未调用模型，或模型未配置单价）。")
	} else {
		byRole := map[string]float64{}
		var total float64
		for _, c := range costs {
			byRole[c.Role] += c.CostUSD
			total += c.CostUSD
		}
		line("| 角色 | 模型 | 输入 | 输出 | 缓存读 | 缓存写 | 费用 |")
		line("| --- | --- | --- | --- | --- | --- | --- |")
		for _, c := range costs {
			line("| %s | %s | %d | %d | %d | %d | $%.6f |",
				c.Role, c.Model, c.InputTokens, c.OutputTokens,
				c.CacheReadTokens, c.CacheWriteTokens, c.CostUSD)
		}
		line("")
		line("合计: $%.6f（预算 $%.2f）", total, task.BudgetUSD)
	}
	line("")

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

func reportSource(task store.Task) string {
	if task.Source == "issue" && task.SourceRef != "" {
		return "GitHub Issue " + task.SourceRef
	}
	if task.Source == "" || task.Source == "cli" {
		return "CLI"
	}
	return task.Source
}
