from __future__ import annotations

import json
import traceback

from agentdev.backends.base import ExecutionBackend, StepContext
from agentdev.config import AppConfig, RepoConfig
from agentdev.errors import AgentDevError, BackendError, ConfigError, WorkspaceError
from agentdev.policy import PolicyEngine
from agentdev.report import write_run_artifacts
from agentdev.state import TERMINAL, Status
from agentdev.repository import Repository
from agentdev.store import Task, now_iso
from agentdev.tools import ToolRuntime
from agentdev.verify import Verifier, check_anticheat
from agentdev.workspace import Workspace


class Orchestrator:
    """驱动单个任务从 QUEUED 走到终态或挂起状态（SDD 4 / 5）。

    M0 范围：需求即计划、无审批、本地提交不推送。
    """

    def __init__(self, store: Repository, config: AppConfig, backend: ExecutionBackend):
        if config.delivery.enabled:
            raise ConfigError("delivery.enabled=true 尚未实现，请保持为 false")
        self.store = store
        self.config = config
        self.backend = backend
        self.workspace = Workspace(config)

    def run_task(self, task_id: str) -> Task:
        task = self.store.get_task(task_id)
        if task.status != Status.QUEUED:
            raise AgentDevError(f"任务 {task_id} 当前状态为 {task.status.value}，只能执行 QUEUED 任务")
        repo = self.config.repos.get(task.repo)
        if repo is None:
            return self._fail(task, f"仓库未在 repos.yaml 中配置: {task.repo}")

        try:
            task = self._execute(task, repo)
        except (WorkspaceError, ConfigError) as e:
            task = self._fail(self.store.get_task(task_id), str(e))
        except Exception:
            task = self._fail(self.store.get_task(task_id), traceback.format_exc())
        finally:
            write_run_artifacts(self.store, self.config, task_id)
        return task

    def _execute(self, task: Task, repo: RepoConfig) -> Task:
        policy_cfg = self.config.policy
        task = self._move(task, Status.TRIAGING)

        level = task.level or "S"
        if level == "L":
            return self._move(
                task,
                Status.REJECTED,
                event_payload={"reason": "L 级任务不自动执行，请拆分为 S/M 级子任务"},
                finished_at=now_iso(),
            )
        if level == "M":
            return self._move(
                task,
                Status.NEEDS_HUMAN,
                event_payload={"reason": "M 级需要审批，审批流程尚未实现"},
            )

        task = self._move(
            task,
            Status.PLANNING,
            plan_json=json.dumps({"mode": "requirement-as-plan"}, ensure_ascii=False),
        )
        info = self.workspace.create(task.repo, repo, task.id, _slug_source(task))
        task = self._move(
            task,
            Status.CODING,
            branch=info.branch,
            base_sha=info.base_sha,
            worktree_path=info.path.as_posix(),
        )

        policy = PolicyEngine(policy_cfg, repo, info.path)
        tools = ToolRuntime(
            policy,
            max_output_chars=policy_cfg.tool_output_max_chars,
            command_timeout_s=policy_cfg.command_timeout_s,
            on_violation=_violation_recorder(self.store, task.id),
        )
        verifier = Verifier(
            timeout_s=policy_cfg.command_timeout_s,
            artifacts_dir=self.config.runs_dir / task.id / "artifacts",
        )

        failure: str | None = None
        while True:
            ctx = StepContext(
                task_id=task.id,
                requirement=task.requirement,
                plan=task.requirement,
                worktree=info.path,
                tools=tools,
                max_turns=policy_cfg.max_turns_per_step,
                failure_summary=failure,
            )
            try:
                result = self.backend.run_step(ctx)
            except BackendError as e:
                return self._move(
                    task, Status.NEEDS_HUMAN, event_payload={"reason": str(e)}
                )
            self.store.append_event(
                task.id,
                "step_finished",
                {
                    "turns": result.turns,
                    "summary": result.final_text[:2000],
                    "input_tokens": result.usage.input_tokens,
                    "output_tokens": result.usage.output_tokens,
                },
            )

            task = self._move(task, Status.VERIFYING)
            changed = self.workspace.changed_files(info)
            diff = self.workspace.diff(info)

            findings = check_anticheat(changed, diff, repo.verify_config_files)
            if findings:
                self.store.append_event(task.id, "anticheat", {"findings": findings})
                return self._move(
                    task,
                    Status.NEEDS_HUMAN,
                    event_payload={"reason": "防作弊检查命中", "findings": findings},
                )

            if not changed:
                failure = "本步骤未产生任何文件改动，请根据需求完成实际修改。"
                self.store.record_verification(
                    task.id, task.fix_attempts, False, {"passed": False, "reason": failure}
                )
            else:
                report = verifier.run(info.path, repo.verify_commands, attempt=task.fix_attempts)
                self.store.record_verification(
                    task.id, task.fix_attempts, report.passed, report.to_dict()
                )
                if report.passed:
                    break
                failure = report.failure_summary

            if task.fix_attempts >= policy_cfg.max_fix_attempts:
                return self._move(
                    task,
                    Status.NEEDS_HUMAN,
                    event_payload={"reason": "修复次数已达上限", "last_failure": failure},
                )
            task = self._move(task, Status.CODING, fix_attempts=task.fix_attempts + 1)

        task = self._move(task, Status.COMMITTING)
        commit_sha = self.workspace.commit(
            info,
            _commit_message(task),
            (self.config.git_author_name, self.config.git_author_email),
        )
        if commit_sha is None:
            return self._move(
                task, Status.NEEDS_HUMAN, event_payload={"reason": "没有可提交的改动"}
            )
        return self._move(
            task, Status.DONE, commit_sha=commit_sha, finished_at=now_iso()
        )

    def _move(self, task: Task, to: Status, event_payload: dict | None = None, **fields) -> Task:
        return self.store.transition(
            task.id, task.version, to, event_payload=event_payload, **fields
        )

    def _fail(self, task: Task, reason: str) -> Task:
        if task.status in TERMINAL:
            return task
        return self.store.transition(
            task.id,
            task.version,
            Status.FAILED,
            event_payload={"reason": reason},
            finished_at=now_iso(),
        )


def _slug_source(task: Task) -> str:
    return task.title or task.requirement[:60]


def _commit_message(task: Task) -> str:
    first = task.requirement.strip().splitlines()[0] if task.requirement.strip() else task.id
    return f"agent({task.id}): {first[:72]}\n\nTask: {task.id}"


def _violation_recorder(store: Repository, task_id: str):
    def record(kind: str, detail: str, blocked: bool) -> None:
        store.record_violation(task_id, kind, detail, blocked)

    return record
