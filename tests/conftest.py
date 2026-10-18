from __future__ import annotations

import subprocess
import textwrap
from pathlib import Path

import pytest

from agentdev.config import AppConfig, PolicyConfig, RepoConfig
from agentdev.store import SQLiteStore

CHECK_SCRIPT = textwrap.dedent(
    """\
    import pathlib, sys
    p = pathlib.Path("hello.txt")
    ok = p.exists() and p.read_text(encoding="utf-8").strip() == "hello"
    sys.exit(0 if ok else 1)
    """
)


def git(cwd: Path, *args: str) -> str:
    proc = subprocess.run(
        ["git", *args], cwd=cwd, capture_output=True, text=True, check=True, encoding="utf-8"
    )
    return proc.stdout


@pytest.fixture
def repo_path(tmp_path: Path) -> Path:
    path = tmp_path / "upstream"
    path.mkdir()
    git(path, "init", "-b", "main")
    git(path, "config", "user.name", "test")
    git(path, "config", "user.email", "test@example.com")
    (path / "README.md").write_text("# demo\n", encoding="utf-8")
    (path / "check.py").write_text(CHECK_SCRIPT, encoding="utf-8")
    git(path, "add", "-A")
    git(path, "commit", "-m", "init")
    return path


@pytest.fixture
def home(tmp_path: Path) -> Path:
    path = tmp_path / "home"
    path.mkdir()
    return path


@pytest.fixture
def config(home: Path, repo_path: Path) -> AppConfig:
    repo = RepoConfig(
        local_path=repo_path,
        default_branch="main",
        verify_commands={"check": "python check.py"},
        verify_config_files=["check.py"],
        budget_usd=2.0,
    )
    return AppConfig(
        home=home,
        repos={"acme/app": repo},
        policy=PolicyConfig(max_fix_attempts=2),
    )


@pytest.fixture
def store(config: AppConfig):
    s = SQLiteStore(config.db_path)
    yield s
    s.close()
