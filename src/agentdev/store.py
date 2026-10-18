from __future__ import annotations

import json
import secrets
import sqlite3
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from agentdev.errors import ConcurrentModification, NotFound
from agentdev.state import Status, assert_transition

SCHEMA_VERSION = 1

SCHEMA = """
CREATE TABLE IF NOT EXISTS tasks (
    id              TEXT PRIMARY KEY,
    repo            TEXT NOT NULL,
    title           TEXT,
    requirement     TEXT NOT NULL,
    source          TEXT NOT NULL DEFAULT 'cli',
    source_ref      TEXT,
    level           TEXT,
    level_override  INTEGER NOT NULL DEFAULT 0,
    status          TEXT NOT NULL,
    previous_status TEXT,
    plan_json       TEXT,
    branch          TEXT,
    worktree_path   TEXT,
    base_sha        TEXT,
    commit_sha      TEXT,
    pr_number       INTEGER,
    pr_url          TEXT,
    fix_attempts    INTEGER NOT NULL DEFAULT 0,
    ci_attempts     INTEGER NOT NULL DEFAULT 0,
    budget_usd      REAL NOT NULL,
    spent_usd       REAL NOT NULL DEFAULT 0,
    version         INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    finished_at     TEXT
);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);

CREATE TABLE IF NOT EXISTS task_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     TEXT NOT NULL REFERENCES tasks(id),
    ts          TEXT NOT NULL,
    type        TEXT NOT NULL,
    from_status TEXT,
    to_status   TEXT,
    payload     TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_task ON task_events(task_id, id);

CREATE TABLE IF NOT EXISTS cost_ledger (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id            TEXT NOT NULL REFERENCES tasks(id),
    ts                 TEXT NOT NULL,
    role               TEXT NOT NULL,
    model              TEXT NOT NULL,
    input_tokens       INTEGER NOT NULL,
    output_tokens      INTEGER NOT NULL,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd           REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS verification_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     TEXT NOT NULL REFERENCES tasks(id),
    attempt     INTEGER NOT NULL,
    passed      INTEGER NOT NULL,
    report_json TEXT NOT NULL,
    ts          TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS policy_violations (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    ts      TEXT NOT NULL,
    kind    TEXT NOT NULL,
    detail  TEXT NOT NULL,
    blocked INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS interactions (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    ts      TEXT NOT NULL,
    kind    TEXT NOT NULL,
    content TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS locks (
    resource     TEXT PRIMARY KEY,
    task_id      TEXT NOT NULL,
    acquired_at  TEXT NOT NULL,
    heartbeat_at TEXT NOT NULL
);
"""

TASK_FIELDS = (
    "title", "source", "source_ref", "level", "level_override", "previous_status",
    "plan_json", "branch", "worktree_path", "base_sha", "commit_sha",
    "pr_number", "pr_url", "fix_attempts", "ci_attempts", "budget_usd", "spent_usd",
)


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def new_task_id() -> str:
    stamp = datetime.now(timezone.utc).strftime("%Y%m%d")
    return f"t_{stamp}_{secrets.token_hex(3)}"


@dataclass
class Task:
    id: str
    repo: str
    requirement: str
    status: Status
    version: int
    created_at: str
    updated_at: str
    title: str | None = None
    source: str = "cli"
    source_ref: str | None = None
    level: str | None = None
    level_override: bool = False
    previous_status: Status | None = None
    plan_json: str | None = None
    branch: str | None = None
    worktree_path: str | None = None
    base_sha: str | None = None
    commit_sha: str | None = None
    pr_number: int | None = None
    pr_url: str | None = None
    fix_attempts: int = 0
    ci_attempts: int = 0
    budget_usd: float = 2.0
    spent_usd: float = 0.0
    finished_at: str | None = None

    @classmethod
    def from_row(cls, row: sqlite3.Row) -> "Task":
        return cls(
            id=row["id"],
            repo=row["repo"],
            requirement=row["requirement"],
            status=Status(row["status"]),
            version=row["version"],
            created_at=row["created_at"],
            updated_at=row["updated_at"],
            title=row["title"],
            source=row["source"],
            source_ref=row["source_ref"],
            level=row["level"],
            level_override=bool(row["level_override"]),
            previous_status=Status(row["previous_status"]) if row["previous_status"] else None,
            plan_json=row["plan_json"],
            branch=row["branch"],
            worktree_path=row["worktree_path"],
            base_sha=row["base_sha"],
            commit_sha=row["commit_sha"],
            pr_number=row["pr_number"],
            pr_url=row["pr_url"],
            fix_attempts=row["fix_attempts"],
            ci_attempts=row["ci_attempts"],
            budget_usd=row["budget_usd"],
            spent_usd=row["spent_usd"],
            finished_at=row["finished_at"],
        )


class SQLiteStore:
    """Repository 的 SQLite 实现，见 agentdev.repository.Repository。"""

    def __init__(self, path: Path):
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.conn = sqlite3.connect(self.path, isolation_level=None, check_same_thread=False)
        self.conn.row_factory = sqlite3.Row
        self.conn.execute("PRAGMA journal_mode=WAL")
        self.conn.execute("PRAGMA foreign_keys=ON")
        self.conn.executescript(SCHEMA)
        self.conn.execute(f"PRAGMA user_version={SCHEMA_VERSION}")

    def close(self) -> None:
        self.conn.close()

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
    ) -> Task:
        task_id = new_task_id()
        ts = now_iso()
        self.conn.execute("BEGIN IMMEDIATE")
        try:
            self.conn.execute(
                """INSERT INTO tasks (id, repo, title, requirement, source, source_ref, level,
                   status, budget_usd, version, created_at, updated_at)
                   VALUES (?,?,?,?,?,?,?,?,?,0,?,?)""",
                (task_id, repo, title, requirement, source, source_ref, level,
                 Status.QUEUED.value, budget_usd, ts, ts),
            )
            self._insert_event(task_id, "task_created", None, Status.QUEUED, {"repo": repo}, ts)
            self.conn.execute("COMMIT")
        except Exception:
            self.conn.execute("ROLLBACK")
            raise
        return self.get_task(task_id)

    def get_task(self, task_id: str) -> Task:
        row = self.conn.execute("SELECT * FROM tasks WHERE id = ?", (task_id,)).fetchone()
        if row is None:
            raise NotFound(f"任务不存在: {task_id}")
        return Task.from_row(row)

    def list_tasks(self, status: Status | None = None, repo: str | None = None) -> list[Task]:
        sql = "SELECT * FROM tasks WHERE 1=1"
        params: list[Any] = []
        if status is not None:
            sql += " AND status = ?"
            params.append(status.value)
        if repo is not None:
            sql += " AND repo = ?"
            params.append(repo)
        sql += " ORDER BY rowid"
        return [Task.from_row(r) for r in self.conn.execute(sql, params).fetchall()]

    def next_queued(self) -> Task | None:
        row = self.conn.execute(
            "SELECT * FROM tasks WHERE status = ? ORDER BY rowid LIMIT 1",
            (Status.QUEUED.value,),
        ).fetchone()
        return Task.from_row(row) if row else None

    def transition(
        self,
        task_id: str,
        expected_version: int,
        to: Status,
        *,
        event_payload: dict[str, Any] | None = None,
        **fields: Any,
    ) -> Task:
        """状态转换与字段更新在同一事务内完成，并校验版本号。"""
        return self._update(task_id, expected_version, to, event_payload, fields)

    def update(
        self,
        task_id: str,
        expected_version: int,
        *,
        event_type: str = "task_updated",
        event_payload: dict[str, Any] | None = None,
        **fields: Any,
    ) -> Task:
        return self._update(task_id, expected_version, None, event_payload, fields, event_type)

    def append_event(
        self, task_id: str, event_type: str, payload: dict[str, Any] | None = None
    ) -> None:
        self.conn.execute("BEGIN IMMEDIATE")
        try:
            self._insert_event(task_id, event_type, None, None, payload, now_iso())
            self.conn.execute("COMMIT")
        except Exception:
            self.conn.execute("ROLLBACK")
            raise

    def list_events(self, task_id: str) -> list[dict[str, Any]]:
        rows = self.conn.execute(
            "SELECT * FROM task_events WHERE task_id = ? ORDER BY id", (task_id,)
        ).fetchall()
        return [
            {
                "id": r["id"],
                "ts": r["ts"],
                "type": r["type"],
                "from_status": r["from_status"],
                "to_status": r["to_status"],
                "payload": json.loads(r["payload"]) if r["payload"] else None,
            }
            for r in rows
        ]

    def record_violation(self, task_id: str, kind: str, detail: str, blocked: bool) -> None:
        self.conn.execute(
            "INSERT INTO policy_violations (task_id, ts, kind, detail, blocked) VALUES (?,?,?,?,?)",
            (task_id, now_iso(), kind, detail, int(blocked)),
        )

    def list_violations(self, task_id: str) -> list[dict[str, Any]]:
        rows = self.conn.execute(
            "SELECT ts, kind, detail, blocked FROM policy_violations WHERE task_id = ? ORDER BY id",
            (task_id,),
        ).fetchall()
        return [dict(r) for r in rows]

    def record_verification(self, task_id: str, attempt: int, passed: bool, report: dict) -> None:
        self.conn.execute(
            "INSERT INTO verification_runs (task_id, attempt, passed, report_json, ts)"
            " VALUES (?,?,?,?,?)",
            (task_id, attempt, int(passed), json.dumps(report, ensure_ascii=False), now_iso()),
        )

    def list_verifications(self, task_id: str) -> list[dict[str, Any]]:
        rows = self.conn.execute(
            "SELECT attempt, passed, report_json, ts FROM verification_runs"
            " WHERE task_id = ? ORDER BY id",
            (task_id,),
        ).fetchall()
        return [
            {
                "attempt": r["attempt"],
                "passed": bool(r["passed"]),
                "report": json.loads(r["report_json"]),
                "ts": r["ts"],
            }
            for r in rows
        ]

    def _update(
        self,
        task_id: str,
        expected_version: int,
        to: Status | None,
        event_payload: dict[str, Any] | None,
        fields: dict[str, Any],
        event_type: str = "state_changed",
    ) -> Task:
        unknown = set(fields) - set(TASK_FIELDS) - {"finished_at", "previous_status"}
        if unknown:
            raise ValueError(f"不允许更新的字段: {sorted(unknown)}")
        ts = now_iso()
        self.conn.execute("BEGIN IMMEDIATE")
        try:
            row = self.conn.execute(
                "SELECT status, version FROM tasks WHERE id = ?", (task_id,)
            ).fetchone()
            if row is None:
                raise NotFound(f"任务不存在: {task_id}")
            if row["version"] != expected_version:
                raise ConcurrentModification(
                    f"任务 {task_id} 版本冲突: 期望 {expected_version}，实际 {row['version']}"
                )
            current = Status(row["status"])
            assignments = {k: _db_value(v) for k, v in fields.items()}
            if to is not None:
                assert_transition(current, to)
                assignments["status"] = to.value
                if to in _SUSPEND_TARGETS:
                    assignments["previous_status"] = current.value
            assignments["version"] = expected_version + 1
            assignments["updated_at"] = ts
            set_sql = ", ".join(f"{k} = ?" for k in assignments)
            self.conn.execute(
                f"UPDATE tasks SET {set_sql} WHERE id = ? AND version = ?",
                (*assignments.values(), task_id, expected_version),
            )
            self._insert_event(
                task_id,
                event_type if to is None else "state_changed",
                current if to is not None else None,
                to,
                event_payload,
                ts,
            )
            self.conn.execute("COMMIT")
        except Exception:
            self.conn.execute("ROLLBACK")
            raise
        return self.get_task(task_id)

    def _insert_event(
        self,
        task_id: str,
        event_type: str,
        from_status: Status | None,
        to_status: Status | None,
        payload: dict[str, Any] | None,
        ts: str,
    ) -> None:
        self.conn.execute(
            "INSERT INTO task_events (task_id, ts, type, from_status, to_status, payload)"
            " VALUES (?,?,?,?,?,?)",
            (
                task_id,
                ts,
                event_type,
                from_status.value if from_status else None,
                to_status.value if to_status else None,
                json.dumps(payload, ensure_ascii=False) if payload is not None else None,
            ),
        )


_SUSPEND_TARGETS = frozenset({Status.NEEDS_INFO, Status.AWAITING_APPROVAL, Status.NEEDS_HUMAN})


def _db_value(v: Any) -> Any:
    if isinstance(v, Status):
        return v.value
    if isinstance(v, bool):
        return int(v)
    if isinstance(v, Path):
        return v.as_posix()
    return v
