from __future__ import annotations

import json
from pathlib import Path

from agentdev.config import AppConfig
from agentdev.repository import Repository
from agentdev.store import Task, now_iso


def run_dir(config: AppConfig, task_id: str) -> Path:
    path = config.runs_dir / task_id
    path.mkdir(parents=True, exist_ok=True)
    return path


def write_run_artifacts(store: Repository, config: AppConfig, task_id: str) -> Path:
    """写入 events.jsonl 与 report.md，返回报告路径。"""
    task = store.get_task(task_id)
    events = store.list_events(task_id)
    violations = store.list_violations(task_id)
    verifications = store.list_verifications(task_id)

    directory = run_dir(config, task_id)
    with (directory / "events.jsonl").open("w", encoding="utf-8") as f:
        for event in events:
            f.write(json.dumps(event, ensure_ascii=False) + "\n")

    report_path = directory / "report.md"
    report_path.write_text(
        _render(task, events, violations, verifications, generated_at=now_iso()),
        encoding="utf-8",
    )
    return report_path


def _render(
    task: Task,
    events: list[dict],
    violations: list[dict],
    verifications: list[dict],
    *,
    generated_at: str,
) -> str:
    lines: list[str] = [
        f"# 任务报告 {task.id}",
        "",
        f"- 生成时间: {generated_at}",
        f"- 仓库: {task.repo}",
        f"- 分级: {task.level or '-'}",
        f"- 最终状态: **{task.status.value}**",
        f"- 分支: {task.branch or '-'}",
        f"- 基线提交: {task.base_sha or '-'}",
        f"- 提交: {task.commit_sha or '-'}",
        f"- 修复次数: {task.fix_attempts}",
        f"- 预算: ${task.budget_usd:.2f}（已用 ${task.spent_usd:.4f}）",
        "",
        "## 需求",
        "",
        "> " + task.requirement.replace("\n", "\n> "),
        "",
        "## 验证记录",
        "",
    ]
    if not verifications:
        lines.append("无。")
    for v in verifications:
        mark = "通过" if v["passed"] else "失败"
        lines.append(f"### 第 {v['attempt']} 次: {mark}")
        for o in v["report"].get("outcomes", []):
            lines.append(
                f"- `{o['command']}` ({o['name']}): 退出码 {o['exit_code']}，"
                f"耗时 {o['duration_s']}s，日志 `{o['log_path']}`"
            )
        lines.append("")

    lines += ["## 策略违规", ""]
    if not violations:
        lines.append("无。")
    for v in violations:
        flag = "已拦截" if v["blocked"] else "记录"
        lines.append(f"- {v['ts']} [{v['kind']}] {flag}: {v['detail']}")
    lines.append("")

    lines += ["## 事件时间线", "", "| 时间 | 事件 | 状态变化 |", "| --- | --- | --- |"]
    for e in events:
        change = (
            f"{e['from_status'] or '-'} → {e['to_status']}" if e["to_status"] else ""
        )
        lines.append(f"| {e['ts']} | {e['type']} | {change} |")
    lines.append("")
    return "\n".join(lines)
