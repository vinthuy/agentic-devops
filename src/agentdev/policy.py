from __future__ import annotations

import re
from functools import lru_cache
from pathlib import Path

from agentdev.config import PolicyConfig, RepoConfig
from agentdev.errors import PolicyViolation

_SHELL_META = re.compile(r"[;&|`<>]|\$\(")


@lru_cache(maxsize=512)
def _glob_to_regex(pattern: str) -> re.Pattern[str]:
    out: list[str] = []
    i = 0
    while i < len(pattern):
        if pattern.startswith("**/", i):
            out.append("(?:.*/)?")
            i += 3
        elif pattern.startswith("**", i):
            out.append(".*")
            i += 2
        elif pattern[i] == "*":
            out.append("[^/]*")
            i += 1
        elif pattern[i] == "?":
            out.append("[^/]")
            i += 1
        else:
            out.append(re.escape(pattern[i]))
            i += 1
    return re.compile("".join(out) + r"\Z")


def glob_match(rel_posix: str, pattern: str) -> bool:
    return _glob_to_regex(pattern).match(rel_posix) is not None


class PolicyEngine:
    """路径与命令的确定性检查。所有工具调用必须先经过这里。"""

    def __init__(
        self,
        policy: PolicyConfig,
        repo: RepoConfig,
        worktree: Path,
        allowed_paths: list[str] | None = None,
    ):
        self.policy = policy
        self.repo = repo
        self.root = Path(worktree).resolve()
        self.allowed_paths = allowed_paths

    def relative(self, raw: str) -> tuple[Path, str]:
        if not raw or "\x00" in raw:
            raise PolicyViolation("path", "路径为空或包含非法字符")
        resolved = (self.root / raw).resolve()
        try:
            rel = resolved.relative_to(self.root)
        except ValueError as e:
            raise PolicyViolation("path", f"路径越出工作区: {raw}") from e
        return resolved, rel.as_posix() if rel.parts else "."

    def is_denied(self, rel_posix: str) -> bool:
        return any(glob_match(rel_posix, pat) for pat in self.policy.deny_paths)

    def check_path(self, raw: str, *, write: bool) -> Path:
        resolved, rel = self.relative(raw)
        if self.is_denied(rel):
            raise PolicyViolation("path", f"敏感路径禁止访问: {rel}")
        if write and self.allowed_paths is not None:
            if not any(glob_match(rel, pat) for pat in self.allowed_paths):
                raise PolicyViolation("path", f"不在计划允许的修改范围内: {rel}")
        return resolved

    def check_command(self, cmd: str) -> None:
        stripped = cmd.strip()
        if not stripped:
            raise PolicyViolation("command", "命令为空")
        for pattern in self.policy.deny_command_patterns:
            if re.search(pattern, stripped):
                raise PolicyViolation("command", f"命令命中黑名单 /{pattern}/: {stripped}")
        if _SHELL_META.search(stripped):
            raise PolicyViolation("command", f"不允许组合命令或重定向: {stripped}")
        allowed = list(self.repo.verify_commands.values()) + list(self.repo.allowed_commands)
        if not any(stripped == a or stripped.startswith(a + " ") for a in allowed):
            raise PolicyViolation("command", f"命令不在白名单中: {stripped}")
