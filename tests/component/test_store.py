import pytest

from agentdev.errors import ConcurrentModification, InvalidTransition, NotFound
from agentdev.state import Status


def test_create_task_writes_queued_status_and_event(store):
    task = store.create_task(repo="acme/app", requirement="修复登录", budget_usd=2.0, level="S")
    assert task.status == Status.QUEUED
    assert task.version == 0
    events = store.list_events(task.id)
    assert [e["type"] for e in events] == ["task_created"]


def test_transition_updates_status_version_and_fields(store):
    task = store.create_task(repo="acme/app", requirement="x", budget_usd=2.0, level="S")
    task = store.transition(task.id, task.version, Status.TRIAGING)
    task = store.transition(task.id, task.version, Status.PLANNING, branch="agent/t-1")
    assert task.status == Status.PLANNING
    assert task.version == 2
    assert task.branch == "agent/t-1"
    last = store.list_events(task.id)[-1]
    assert (last["from_status"], last["to_status"]) == ("TRIAGING", "PLANNING")


def test_invalid_transition_is_rejected_and_state_unchanged(store):
    task = store.create_task(repo="acme/app", requirement="x", budget_usd=2.0)
    with pytest.raises(InvalidTransition):
        store.transition(task.id, task.version, Status.DONE)
    assert store.get_task(task.id).status == Status.QUEUED


def test_stale_version_raises_concurrent_modification(store):
    task = store.create_task(repo="acme/app", requirement="x", budget_usd=2.0)
    store.transition(task.id, task.version, Status.TRIAGING)
    with pytest.raises(ConcurrentModification):
        store.transition(task.id, task.version, Status.PLANNING)


def test_suspend_records_previous_status(store):
    task = store.create_task(repo="acme/app", requirement="x", budget_usd=2.0)
    task = store.transition(task.id, task.version, Status.TRIAGING)
    task = store.transition(task.id, task.version, Status.PLANNING)
    task = store.transition(task.id, task.version, Status.CODING)
    task = store.transition(task.id, task.version, Status.NEEDS_HUMAN)
    assert task.previous_status == Status.CODING


def test_list_and_next_queued(store):
    a = store.create_task(repo="acme/app", requirement="a", budget_usd=2.0)
    store.create_task(repo="other/x", requirement="b", budget_usd=2.0)
    assert store.next_queued().id == a.id
    assert len(store.list_tasks(repo="other/x")) == 1
    assert len(store.list_tasks(status=Status.DONE)) == 0


def test_get_missing_task(store):
    with pytest.raises(NotFound):
        store.get_task("t_missing")


def test_wal_mode_enabled(store):
    mode = store.conn.execute("PRAGMA journal_mode").fetchone()[0]
    assert mode.lower() == "wal"
