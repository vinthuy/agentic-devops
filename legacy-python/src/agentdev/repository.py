from __future__ import annotations

from typing import Any, Protocol

from agentdev.config import AppConfig
from agentdev.state import Status
from agentdev.store import Task


class Repository(Protocol):
    """任务持久化的抽象接口。实现可以是 SQLite（当前）或 PostgreSQL（未来）。"""

    def create_task(
        self,
        *,
        repo: str,
        requirement: str,
        budget_usd: float,
        level: str | None = None,
        title: str | None = None,
        source: str = "cli",
        source_ref: str | None = None,
    ) -> Task: ...

    def get_task(self, task_id: str) -> Task: ...

    def list_tasks(self, status: Status | None = None, repo: str | None = None) -> list[Task]: ...

    def next_queued(self) -> Task | None: ...

    def transition(
        self,
        task_id: str,
        expected_version: int,
        to: Status,
        *,
        event_payload: dict[str, Any] | None = None,
        **fields: Any,
    ) -> Task: ...

    def update(
        self,
        task_id: str,
        expected_version: int,
        *,
        event_type: str = "task_updated",
        event_payload: dict[str, Any] | None = None,
        **fields: Any,
    ) -> Task: ...

    def append_event(
        self, task_id: str, event_type: str, payload: dict[str, Any] | None = None
    ) -> None: ...

    def list_events(self, task_id: str) -> list[dict[str, Any]]: ...

    def record_violation(self, task_id: str, kind: str, detail: str, blocked: bool) -> None: ...

    def list_violations(self, task_id: str) -> list[dict[str, Any]]: ...

    def record_verification(
        self, task_id: str, attempt: int, passed: bool, report: dict
    ) -> None: ...

    def list_verifications(self, task_id: str) -> list[dict[str, Any]]: ...

    def close(self) -> None: ...


def open_repository(config: AppConfig) -> Repository:
    from agentdev.store import SQLiteStore

    return SQLiteStore(config.db_path)
