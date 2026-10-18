from __future__ import annotations

import json
import os
import shutil
from pathlib import Path
from typing import Optional

import typer
from rich.console import Console
from rich.table import Table

from agentdev import __version__
from agentdev.config import AppConfig, check_repo_paths, load_config, resolve_home
from agentdev.errors import AgentDevError, ConfigError, NotFound
from agentdev.state import Status
from agentdev.repository import Repository, open_repository

EXIT_OK = 0
EXIT_ERROR = 1
EXIT_USAGE = 2
EXIT_HUMAN = 3

console = Console()
err_console = Console(stderr=True)

app = typer.Typer(help="agentdev：需求到 PR 的自动化工程流水线", no_args_is_help=True)
config_app = typer.Typer(help="配置相关命令", no_args_is_help=True)
task_app = typer.Typer(help="任务相关命令", no_args_is_help=True)
app.add_typer(config_app, name="config")
app.add_typer(task_app, name="task")

HomeOption = Optional[Path]
home_option = typer.Option(None, "--home", help="数据目录，默认读取 AGENTDEV_HOME 或 ~/.agentdev")


def _fail(message: str, code: int = EXIT_ERROR) -> None:
    err_console.print(f"[red]错误[/red] {message}")
    raise typer.Exit(code)


def _load(home: HomeOption) -> AppConfig:
    try:
        return load_config(resolve_home(home))
    except ConfigError as e:
        _fail(str(e), EXIT_USAGE)
    raise AssertionError("unreachable")


def _open_store(config: AppConfig) -> Repository:
    return open_repository(config)


@app.command()
def version() -> None:
    """显示版本号。"""
    console.print(__version__)


@config_app.command("validate")
def config_validate(home: HomeOption = home_option) -> None:
    """加载并校验配置文件。"""
    config = _load(home)
    problems = check_repo_paths(config)
    console.print(f"数据目录: {config.home}")
    console.print(f"已配置仓库: {len(config.repos)}")
    if problems:
        for p in problems:
            console.print(f"[red][FAIL][/red] {p}")
        raise typer.Exit(EXIT_ERROR)
    console.print("[green][OK][/green] 配置校验通过")


@app.command()
def doctor(home: HomeOption = home_option) -> None:
    """检查运行环境。"""
    config = _load(home)
    checks: list[tuple[bool, bool, str]] = []  # (通过, 是否必需, 说明)

    checks.append((shutil.which("git") is not None, True, "git 可用"))
    checks.append((shutil.which("gh") is not None, False, "gh CLI 可用（推送功能需要）"))
    checks.append((bool(os.environ.get("ANTHROPIC_API_KEY")), False, "ANTHROPIC_API_KEY 已设置（真实模型需要）"))

    try:
        config.home.mkdir(parents=True, exist_ok=True)
        probe = config.home / ".write-probe"
        probe.write_text("ok", encoding="utf-8")
        probe.unlink()
        checks.append((True, True, f"数据目录可写: {config.home}"))
    except OSError as e:
        checks.append((False, True, f"数据目录不可写: {e}"))

    if not config.repos:
        checks.append((False, True, "repos.yaml 中没有配置任何仓库"))
    for problem in check_repo_paths(config):
        checks.append((False, True, problem))
    for name in config.repos:
        checks.append((True, True, f"仓库已配置: {name}"))

    failed = False
    for ok, required, message in checks:
        mark = "[green][OK][/green]" if ok else "[red][FAIL][/red]"
        console.print(f"{mark} {message}")
        if required and not ok:
            failed = True
    if failed:
        raise typer.Exit(EXIT_ERROR)


@task_app.command("create")
def task_create(
    repo: str = typer.Option(..., "--repo", help="owner/name，需已在 repos.yaml 中配置"),
    text: Optional[str] = typer.Option(None, "--text", help="需求文本"),
    file: Optional[Path] = typer.Option(None, "--file", help="从文件读取需求"),
    level: str = typer.Option("S", "--level", help="S / M / L"),
    budget: Optional[float] = typer.Option(None, "--budget", help="预算上限（美元）"),
    title: Optional[str] = typer.Option(None, "--title", help="任务标题，用于分支名"),
    issue: Optional[str] = typer.Option(None, "--issue", help="关联的 Issue 编号"),
    home: HomeOption = home_option,
) -> None:
    """创建一个任务（状态为 QUEUED）。"""
    config = _load(home)
    if repo not in config.repos:
        _fail(f"仓库未配置: {repo}（请检查 repos.yaml）", EXIT_USAGE)
    if (text is None) == (file is None):
        _fail("--text 与 --file 必须二选一", EXIT_USAGE)
    requirement = text if text is not None else file.read_text(encoding="utf-8")
    if not requirement.strip():
        _fail("需求内容为空", EXIT_USAGE)
    level = level.upper()
    if level not in {"S", "M", "L"}:
        _fail("--level 只能是 S / M / L", EXIT_USAGE)
    repo_cfg = config.repos[repo]
    budget_usd = budget if budget is not None else repo_cfg.budget_usd
    if budget_usd <= 0:
        _fail("--budget 必须大于 0", EXIT_USAGE)

    store = _open_store(config)
    try:
        task = store.create_task(
            repo=repo,
            requirement=requirement.strip(),
            budget_usd=budget_usd,
            level=level,
            title=title,
            source="issue" if issue else "cli",
            source_ref=issue,
        )
    finally:
        store.close()
    console.print(f"[green][OK][/green] 已创建任务 [bold]{task.id}[/bold]（{repo}，{level} 级）")


@task_app.command("list")
def task_list(
    status: Optional[str] = typer.Option(None, "--status", help="按状态过滤"),
    repo: Optional[str] = typer.Option(None, "--repo", help="按仓库过滤"),
    home: HomeOption = home_option,
) -> None:
    """列出任务。"""
    config = _load(home)
    status_filter = _parse_status(status) if status else None
    store = _open_store(config)
    try:
        tasks = store.list_tasks(status=status_filter, repo=repo)
    finally:
        store.close()
    table = Table(show_lines=False)
    for col in ("ID", "仓库", "级别", "状态", "修复", "创建时间"):
        table.add_column(col)
    for t in tasks:
        table.add_row(t.id, t.repo, t.level or "-", t.status.value, str(t.fix_attempts), t.created_at)
    console.print(table)


@task_app.command("status")
def task_status(task_id: str, home: HomeOption = home_option) -> None:
    """查看任务详情与最近事件。"""
    config = _load(home)
    store = _open_store(config)
    try:
        task = store.get_task(task_id)
        events = store.list_events(task_id)[-15:]
    except NotFound as e:
        _fail(str(e), EXIT_USAGE)
    finally:
        store.close()
    console.print(f"[bold]{task.id}[/bold]  {task.repo}  级别 {task.level or '-'}")
    console.print(f"状态: [bold]{task.status.value}[/bold]")
    console.print(f"需求: {task.requirement.splitlines()[0] if task.requirement else ''}")
    console.print(f"分支: {task.branch or '-'}   提交: {task.commit_sha or '-'}")
    console.print(f"修复次数: {task.fix_attempts}   预算: ${task.budget_usd:.2f}")
    table = Table(title="最近事件")
    for col in ("时间", "事件", "变化"):
        table.add_column(col)
    for e in events:
        change = f"{e['from_status'] or '-'} → {e['to_status']}" if e["to_status"] else ""
        table.add_row(e["ts"], e["type"], change)
    console.print(table)


@app.command()
def run(
    backend: str = typer.Option("anthropic", "--backend", help="anthropic / fake"),
    script: Optional[Path] = typer.Option(None, "--script", help="fake 后端的脚本 JSON 文件"),
    home: HomeOption = home_option,
) -> None:
    """取出一个 QUEUED 任务并执行到终态或挂起。"""
    from agentdev.orchestrator import Orchestrator

    config = _load(home)
    store = _open_store(config)
    try:
        task = store.next_queued()
        if task is None:
            console.print("没有排队中的任务")
            return
        exec_backend = _make_backend(config, backend, script)
        console.print(f"开始执行 {task.id}（{task.repo}，{task.level or '-'} 级）")
        result = Orchestrator(store, config, exec_backend).run_task(task.id)
    except AgentDevError as e:
        _fail(str(e))
    finally:
        store.close()

    report = config.runs_dir / result.id / "report.md"
    console.print(f"最终状态: [bold]{result.status.value}[/bold]")
    if result.commit_sha:
        console.print(f"提交: {result.commit_sha}  分支: {result.branch}")
    console.print(f"报告: {report}")
    if result.status == Status.NEEDS_HUMAN:
        raise typer.Exit(EXIT_HUMAN)
    if result.status in {Status.FAILED, Status.REJECTED}:
        raise typer.Exit(EXIT_ERROR)


def _make_backend(config: AppConfig, name: str, script: Optional[Path]):
    from agentdev.errors import BackendUnavailable

    if name == "anthropic":
        from agentdev.backends.anthropic import AnthropicBackend

        try:
            return AnthropicBackend(config.models.model, config.models.max_tokens)
        except BackendUnavailable as e:
            _fail(str(e), EXIT_USAGE)
    if name == "fake":
        from agentdev.backends.fake import FakeBackend

        if script is None:
            _fail("fake 后端需要 --script 指定脚本文件", EXIT_USAGE)
        steps = json.loads(script.read_text(encoding="utf-8"))
        return FakeBackend([[(s["tool"], s.get("args", {})) for s in step] for step in steps])
    _fail(f"未知后端: {name}", EXIT_USAGE)
    raise AssertionError("unreachable")


def _parse_status(value: str) -> Status:
    try:
        return Status(value.upper())
    except ValueError:
        _fail(f"未知状态: {value}", EXIT_USAGE)
    raise AssertionError("unreachable")


if __name__ == "__main__":
    app()
