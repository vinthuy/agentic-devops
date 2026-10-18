from __future__ import annotations

import os
import subprocess
from pathlib import Path
from typing import Any

import yaml
from pydantic import BaseModel, Field, ValidationError, field_validator

from agentdev.errors import ConfigError

DEFAULT_DENY_PATHS = [".env*", "**/.env*", "**/*.pem", "**/*.key", ".git/**", "**/secrets/**"]

DEFAULT_DENY_COMMANDS = [
    r"rm\s+-rf",
    r"curl\s+.*\|\s*(sh|bash)",
    r"git\s+(push|config|remote)",
    r"npm\s+publish",
    r"\bsudo\b",
]


class RepoConfig(BaseModel):
    local_path: Path
    default_branch: str = "main"
    verify_commands: dict[str, str]
    allowed_commands: list[str] = Field(default_factory=list)
    sensitive_paths: list[str] = Field(default_factory=list)
    verify_config_files: list[str] = Field(default_factory=list)
    budget_usd: float = 2.0
    pr_draft: bool = True

    @field_validator("verify_commands")
    @classmethod
    def _non_empty(cls, v: dict[str, str]) -> dict[str, str]:
        if not v:
            raise ValueError("verify_commands 不能为空")
        return v

    @field_validator("budget_usd")
    @classmethod
    def _positive(cls, v: float) -> float:
        if v <= 0:
            raise ValueError("budget_usd 必须大于 0")
        return v


class PolicyConfig(BaseModel):
    deny_paths: list[str] = Field(default_factory=lambda: list(DEFAULT_DENY_PATHS))
    deny_command_patterns: list[str] = Field(default_factory=lambda: list(DEFAULT_DENY_COMMANDS))
    max_fix_attempts: int = 3
    max_turns_per_step: int = 30
    tool_output_max_chars: int = 8000
    command_timeout_s: int = 300
    task_timeout_minutes: int = 45


class ModelsConfig(BaseModel):
    provider: str = "anthropic"
    model: str = "claude-sonnet-5-5"
    max_tokens: int = 8192
    base_url: str | None = None
    api_key_env: str = "ANTHROPIC_API_KEY"


class DeliveryConfig(BaseModel):
    enabled: bool = False
    branch_prefix: str = "agent/"


class AppConfig(BaseModel):
    home: Path
    repos: dict[str, RepoConfig] = Field(default_factory=dict)
    policy: PolicyConfig = Field(default_factory=PolicyConfig)
    models: ModelsConfig = Field(default_factory=ModelsConfig)
    delivery: DeliveryConfig = Field(default_factory=DeliveryConfig)
    git_author_name: str = "agentdev"
    git_author_email: str = "agentdev@localhost"

    @property
    def db_path(self) -> Path:
        return self.home / "agentdev.db"

    @property
    def workspaces_dir(self) -> Path:
        return self.home / "workspaces"

    @property
    def runs_dir(self) -> Path:
        return self.home / "runs"


def resolve_home(override: Path | None = None) -> Path:
    if override is not None:
        return Path(override)
    env = os.environ.get("AGENTDEV_HOME")
    if env:
        return Path(env)
    return Path.home() / ".agentdev"


def _read_yaml(path: Path) -> dict[str, Any]:
    if not path.exists():
        return {}
    try:
        data = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
    except yaml.YAMLError as e:
        raise ConfigError(f"{path}: YAML 解析失败: {e}") from e
    if not isinstance(data, dict):
        raise ConfigError(f"{path}: 顶层必须是映射")
    return data


def load_config(home: Path | None = None) -> AppConfig:
    home = resolve_home(home)
    raw_repos = _read_yaml(home / "repos.yaml").get("repos", {})
    raw_policy = _read_yaml(home / "policy.yaml")
    raw_models = _read_yaml(home / "models.yaml")
    raw_config = _read_yaml(home / "config.yaml")

    try:
        return AppConfig(
            home=home,
            repos={name: RepoConfig(**cfg) for name, cfg in raw_repos.items()},
            policy=PolicyConfig(**raw_policy),
            models=ModelsConfig(**raw_models),
            delivery=DeliveryConfig(**raw_config.get("delivery", {})),
            git_author_name=raw_config.get("git_author_name", "agentdev"),
            git_author_email=raw_config.get("git_author_email", "agentdev@localhost"),
        )
    except ValidationError as e:
        raise ConfigError(_format_validation_error(e)) from e


def _format_validation_error(e: ValidationError) -> str:
    lines = []
    for err in e.errors():
        loc = ".".join(str(p) for p in err["loc"])
        lines.append(f"- {loc}: {err['msg']}")
    return "配置校验失败:\n" + "\n".join(lines)


def check_repo_paths(config: AppConfig) -> list[str]:
    """返回仓库路径相关的问题列表，空列表表示通过。"""
    problems: list[str] = []
    for name, repo in config.repos.items():
        path = repo.local_path
        if not path.exists():
            problems.append(f"{name}: local_path 不存在: {path}")
            continue
        result = subprocess.run(
            ["git", "-C", str(path), "rev-parse", "--is-inside-work-tree"],
            capture_output=True,
            text=True,
        )
        if result.returncode != 0:
            problems.append(f"{name}: local_path 不是 git 仓库: {path}")
    return problems
