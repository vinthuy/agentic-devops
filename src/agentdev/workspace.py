from __future__ import annotations

import re
import subprocess
from dataclasses import dataclass
from pathlib import Path

from agentdev.config import AppConfig, RepoConfig
from agentdev.errors import WorkspaceError


@dataclass
class WorkspaceInfo:
    path: Path
    branch: str
    base_sha: str


def git(cwd: Path, *args: str, check: bool = True) -> str:
    proc = subprocess.run(
        ["git", *args],
        cwd=cwd,
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
    )
    if check and proc.returncode != 0:
        raise WorkspaceError(f"git {' '.join(args)} 失败: {proc.stderr.strip()}")
    return proc.stdout


def slugify(text: str, limit: int = 40) -> str:
    slug = re.sub(r"[^a-zA-Z0-9]+", "-", text.lower()).strip("-")
    return (slug[:limit].rstrip("-")) or "task"


def repo_dir_name(repo_name: str) -> str:
    return repo_name.replace("/", "__")


class Workspace:
    """管理每个任务的 git worktree 及其 diff / 提交。"""

    def __init__(self, config: AppConfig):
        self.config = config

    def create(
        self, repo_name: str, repo: RepoConfig, task_id: str, slug: str
    ) -> WorkspaceInfo:
        repo_path = repo.local_path
        base_sha = git(repo_path, "rev-parse", repo.default_branch).strip()
        branch = f"{self.config.delivery.branch_prefix}{task_id}-{slugify(slug)}"
        path = self.config.workspaces_dir / repo_dir_name(repo_name) / task_id
        path.parent.mkdir(parents=True, exist_ok=True)
        git(repo_path, "worktree", "add", "-b", branch, str(path), base_sha)
        return WorkspaceInfo(path=path, branch=branch, base_sha=base_sha)

    def stage_all(self, info: WorkspaceInfo) -> None:
        git(info.path, "add", "-A")

    def changed_files(self, info: WorkspaceInfo) -> list[tuple[str, str]]:
        """返回 [(状态, 路径)]，状态为 A/M/D/R 等，相对于基线提交。"""
        self.stage_all(info)
        out = git(info.path, "diff", "--cached", "--name-status", info.base_sha)
        result: list[tuple[str, str]] = []
        for line in out.splitlines():
            parts = line.split("\t")
            if len(parts) >= 2:
                result.append((parts[0][0], parts[-1]))
        return result

    def diff(self, info: WorkspaceInfo) -> str:
        self.stage_all(info)
        return git(info.path, "diff", "--cached", "--no-color", info.base_sha)

    def diff_stat(self, info: WorkspaceInfo) -> str:
        self.stage_all(info)
        return git(info.path, "diff", "--cached", "--shortstat", info.base_sha).strip()

    def commit(self, info: WorkspaceInfo, message: str, author: tuple[str, str]) -> str | None:
        self.stage_all(info)
        staged = git(info.path, "diff", "--cached", "--name-only").strip()
        if not staged:
            return None
        name, email = author
        git(
            info.path,
            "-c", f"user.name={name}",
            "-c", f"user.email={email}",
            "commit", "-m", message,
        )
        return git(info.path, "rev-parse", "HEAD").strip()

    def remove(self, repo_name: str, repo: RepoConfig, info: WorkspaceInfo, keep_branch: bool = True) -> None:
        git(repo.local_path, "worktree", "remove", "--force", str(info.path), check=False)
        if not keep_branch:
            git(repo.local_path, "branch", "-D", info.branch, check=False)
