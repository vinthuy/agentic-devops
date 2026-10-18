import pytest

from agentdev.backends.base import StepContext, StepResult
from agentdev.backends.fake import FakeBackend
from agentdev.errors import AgentDevError, BackendError
from agentdev.orchestrator import Orchestrator
from agentdev.state import Status
from tests.conftest import git

GOOD = [("write_file", {"path": "hello.txt", "content": "hello\n"})]
BAD = [("write_file", {"path": "hello.txt", "content": "bye\n"})]


def _task(store, requirement="在仓库根目录创建 hello.txt，内容为 hello", level="S"):
    return store.create_task(repo="acme/app", requirement=requirement, budget_usd=2.0, level=level)


def _run(store, config, backend, task_id):
    return Orchestrator(store, config, backend).run_task(task_id)


def test_happy_path_commits_and_writes_report(store, config):
    task = _task(store)
    done = _run(store, config, FakeBackend([[*GOOD]]), task.id)

    assert done.status == Status.DONE
    assert done.commit_sha and len(done.commit_sha) == 40
    assert done.fix_attempts == 0
    assert done.branch.startswith("agent/" + task.id)

    repo = config.repos["acme/app"].local_path
    assert task.id in git(repo, "log", "--all", "--format=%s")

    report = (config.runs_dir / task.id / "report.md").read_text(encoding="utf-8")
    assert "DONE" in report
    assert "`python check.py`" in report
    events = [e["type"] for e in store.list_events(task.id)]
    assert "step_finished" in events


def test_fix_loop_recovers_after_failed_verification(store, config):
    task = _task(store)
    done = _run(store, config, FakeBackend([BAD, GOOD]), task.id)

    assert done.status == Status.DONE
    assert done.fix_attempts == 1
    verifications = store.list_verifications(task.id)
    assert [v["passed"] for v in verifications] == [False, True]


def test_fix_limit_leads_to_needs_human(store, config):
    task = _task(store)
    done = _run(store, config, FakeBackend([BAD, BAD, BAD]), task.id)

    assert done.status == Status.NEEDS_HUMAN
    assert done.fix_attempts == config.policy.max_fix_attempts
    reason = store.list_events(task.id)[-1]["payload"]["reason"]
    assert "修复次数" in reason


def test_tampering_with_verify_config_needs_human(store, config):
    task = _task(store)
    tamper = [("write_file", {"path": "check.py", "content": "import sys; sys.exit(0)\n"})]
    done = _run(store, config, FakeBackend([tamper]), task.id)

    assert done.status == Status.NEEDS_HUMAN
    last = store.list_events(task.id)[-1]
    assert last["payload"]["reason"] == "防作弊检查命中"
    assert any("验证配置" in f for f in last["payload"]["findings"])


def test_l_level_is_rejected_without_running_backend(store, config):
    task = _task(store, level="L")
    backend = FakeBackend([])
    done = _run(store, config, backend, task.id)

    assert done.status == Status.REJECTED
    assert backend.calls == []


def test_m_level_parks_for_approval_not_yet_supported(store, config):
    task = _task(store, level="M")
    done = _run(store, config, FakeBackend([]), task.id)
    assert done.status == Status.NEEDS_HUMAN


def test_policy_violations_are_blocked_then_task_completes(store, config):
    task = _task(store)
    script = [
        [
            ("write_file", {"path": ".env", "content": "TOKEN=1\n"}),
            ("run_command", {"command": "rm -rf ."}),
        ],
        [*GOOD],
    ]
    done = _run(store, config, FakeBackend(script), task.id)

    assert done.status == Status.DONE
    violations = store.list_violations(task.id)
    kinds = [v["kind"] for v in violations]
    assert kinds.count("path") >= 1
    assert kinds.count("command") >= 1
    assert all(v["blocked"] for v in violations)


class _ExplodingBackend:
    name = "exploding"

    def run_step(self, ctx: StepContext) -> StepResult:
        raise BackendError("模型限流")


def test_backend_error_leads_to_needs_human(store, config):
    task = _task(store)
    done = _run(store, config, _ExplodingBackend(), task.id)
    assert done.status == Status.NEEDS_HUMAN
    assert "限流" in store.list_events(task.id)[-1]["payload"]["reason"]


class _CrashingBackend:
    name = "crashing"

    def run_step(self, ctx: StepContext) -> StepResult:
        raise RuntimeError("bug in backend")


def test_unexpected_error_marks_task_failed(store, config):
    task = _task(store)
    done = _run(store, config, _CrashingBackend(), task.id)
    assert done.status == Status.FAILED
    assert done.finished_at is not None


def test_running_non_queued_task_raises(store, config):
    task = _task(store)
    _run(store, config, FakeBackend([[*GOOD]]), task.id)
    with pytest.raises(AgentDevError):
        _run(store, config, FakeBackend([]), task.id)


def test_unknown_repo_fails_task(store, config):
    task = store.create_task(repo="ghost/x", requirement="x", budget_usd=1.0, level="S")
    done = _run(store, config, FakeBackend([]), task.id)
    assert done.status == Status.FAILED
