from __future__ import annotations

import os
import re
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

from agentdev.errors import PolicyViolation
from agentdev.policy import PolicyEngine

MAX_READ_BYTES = 2_000_000
MAX_SEARCH_RESULTS = 100
SKIP_DIRS = {".git", "node_modules", "__pycache__", ".venv", "venv", "dist", "build"}

ViolationHook = Callable[[str, str, bool], None]


@dataclass
class ToolResult:
    ok: bool
    content: str


def _walk_files(base: Path):
    if base.is_file():
        yield base
        return
    for dirpath, dirnames, filenames in os.walk(base):
        dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS)
        for name in sorted(filenames):
            yield Path(dirpath) / name


def truncate(text: str, limit: int) -> str:
    if len(text) <= limit:
        return text
    head = int(limit * 0.6)
    tail = limit - head
    omitted = len(text) - limit
    return f"{text[:head]}\n...[已截断 {omitted} 字符]...\n{text[-tail:]}"


_SCHEMAS: list[dict[str, Any]] = [
    {
        "name": "read_file",
        "description": "读取工作区内的文本文件，可指定起止行号（从 1 开始，含端点）。",
        "input_schema": {
            "type": "object",
            "properties": {
                "path": {"type": "string"},
                "start": {"type": "integer"},
                "end": {"type": "integer"},
            },
            "required": ["path"],
        },
    },
    {
        "name": "write_file",
        "description": "整文件写入工作区内的文件（不存在则创建）。",
        "input_schema": {
            "type": "object",
            "properties": {"path": {"type": "string"}, "content": {"type": "string"}},
            "required": ["path", "content"],
        },
    },
    {
        "name": "edit_file",
        "description": "把文件中唯一出现的 old 文本替换为 new。old 必须在文件中恰好出现一次。",
        "input_schema": {
            "type": "object",
            "properties": {
                "path": {"type": "string"},
                "old": {"type": "string"},
                "new": {"type": "string"},
            },
            "required": ["path", "old", "new"],
        },
    },
    {
        "name": "list_dir",
        "description": "列出目录内容，目录名以 / 结尾。",
        "input_schema": {
            "type": "object",
            "properties": {"path": {"type": "string"}},
        },
    },
    {
        "name": "search",
        "description": "在工作区内用正则搜索文本，返回 相对路径:行号: 内容。",
        "input_schema": {
            "type": "object",
            "properties": {
                "pattern": {"type": "string"},
                "path": {"type": "string"},
            },
            "required": ["pattern"],
        },
    },
    {
        "name": "run_command",
        "description": "在工作区根目录执行白名单中的命令。不支持管道、重定向和组合命令。",
        "input_schema": {
            "type": "object",
            "properties": {"command": {"type": "string"}},
            "required": ["command"],
        },
    },
]


class ToolRuntime:
    def __init__(
        self,
        policy: PolicyEngine,
        *,
        max_output_chars: int,
        command_timeout_s: int,
        on_violation: ViolationHook | None = None,
    ):
        self.policy = policy
        self.max_output_chars = max_output_chars
        self.command_timeout_s = command_timeout_s
        self.on_violation = on_violation
        self._handlers: dict[str, Callable[[dict[str, Any]], ToolResult]] = {
            "read_file": self._read_file,
            "write_file": self._write_file,
            "edit_file": self._edit_file,
            "list_dir": self._list_dir,
            "search": self._search,
            "run_command": self._run_command,
        }

    def schemas(self) -> list[dict[str, Any]]:
        return [dict(s) for s in _SCHEMAS]

    def execute(self, name: str, args: dict[str, Any]) -> ToolResult:
        handler = self._handlers.get(name)
        if handler is None:
            return ToolResult(False, f"未知工具: {name}")
        try:
            return handler(args or {})
        except PolicyViolation as e:
            if self.on_violation:
                self.on_violation(e.kind, e.detail, True)
            return ToolResult(False, f"策略拒绝 [{e.kind}]: {e.detail}")
        except KeyError as e:
            return ToolResult(False, f"缺少参数: {e}")
        except (OSError, UnicodeError) as e:
            return ToolResult(False, f"执行失败: {e}")

    def _read_file(self, args: dict[str, Any]) -> ToolResult:
        path = self.policy.check_path(args["path"], write=False)
        if not path.is_file():
            return ToolResult(False, f"不是文件: {args['path']}")
        if path.stat().st_size > MAX_READ_BYTES:
            return ToolResult(False, "文件过大，拒绝读取")
        lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
        start = max(int(args.get("start") or 1), 1)
        end = int(args.get("end") or len(lines))
        numbered = [f"{i}: {lines[i - 1]}" for i in range(start, min(end, len(lines)) + 1)]
        return ToolResult(True, truncate("\n".join(numbered), self.max_output_chars))

    def _write_file(self, args: dict[str, Any]) -> ToolResult:
        path = self.policy.check_path(args["path"], write=True)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(args["content"], encoding="utf-8")
        return ToolResult(True, f"已写入 {args['path']} ({len(args['content'])} 字符)")

    def _edit_file(self, args: dict[str, Any]) -> ToolResult:
        path = self.policy.check_path(args["path"], write=True)
        if not path.is_file():
            return ToolResult(False, f"文件不存在: {args['path']}")
        text = path.read_text(encoding="utf-8")
        old = args["old"]
        if not old:
            return ToolResult(False, "old 不能为空")
        count = text.count(old)
        if count != 1:
            return ToolResult(False, f"old 必须恰好出现一次，实际出现 {count} 次")
        path.write_text(text.replace(old, args["new"], 1), encoding="utf-8")
        return ToolResult(True, f"已编辑 {args['path']}")

    def _list_dir(self, args: dict[str, Any]) -> ToolResult:
        path = self.policy.check_path(args.get("path") or ".", write=False)
        if not path.is_dir():
            return ToolResult(False, f"不是目录: {args.get('path')}")
        entries = []
        for child in sorted(path.iterdir(), key=lambda p: (not p.is_dir(), p.name)):
            if child.name in SKIP_DIRS:
                continue
            _, rel = self.policy.relative(child.relative_to(self.policy.root).as_posix())
            if self.policy.is_denied(rel):
                continue
            entries.append(child.name + ("/" if child.is_dir() else ""))
        return ToolResult(True, "\n".join(entries) or "(空目录)")

    def _search(self, args: dict[str, Any]) -> ToolResult:
        base = self.policy.check_path(args.get("path") or ".", write=False)
        try:
            regex = re.compile(args["pattern"])
        except re.error as e:
            return ToolResult(False, f"正则无效: {e}")
        results: list[str] = []
        for file in _walk_files(base):
            rel = file.relative_to(self.policy.root).as_posix()
            if self.policy.is_denied(rel) or file.stat().st_size > MAX_READ_BYTES:
                continue
            try:
                lines = file.read_text(encoding="utf-8").splitlines()
            except UnicodeDecodeError:
                continue
            for n, line in enumerate(lines, 1):
                if regex.search(line):
                    results.append(f"{rel}:{n}: {line.strip()}")
                    if len(results) >= MAX_SEARCH_RESULTS:
                        results.append(f"...结果已达上限 {MAX_SEARCH_RESULTS} 条")
                        return ToolResult(True, truncate("\n".join(results), self.max_output_chars))
        return ToolResult(True, truncate("\n".join(results) or "(无匹配)", self.max_output_chars))

    def _run_command(self, args: dict[str, Any]) -> ToolResult:
        command = args["command"]
        self.policy.check_command(command)
        try:
            proc = subprocess.run(
                command,
                shell=True,
                cwd=self.policy.root,
                capture_output=True,
                text=True,
                encoding="utf-8",
                errors="replace",
                timeout=self.command_timeout_s,
            )
        except subprocess.TimeoutExpired:
            return ToolResult(False, f"命令超时（>{self.command_timeout_s}s）: {command}")
        output = (proc.stdout or "") + (proc.stderr or "")
        body = truncate(output, self.max_output_chars) or "(无输出)"
        return ToolResult(proc.returncode == 0, f"exit={proc.returncode}\n{body}")
