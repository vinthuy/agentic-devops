from __future__ import annotations

from enum import Enum

from agentdev.errors import InvalidTransition


class Status(str, Enum):
    QUEUED = "QUEUED"
    TRIAGING = "TRIAGING"
    REJECTED = "REJECTED"
    PLANNING = "PLANNING"
    NEEDS_INFO = "NEEDS_INFO"
    AWAITING_APPROVAL = "AWAITING_APPROVAL"
    CODING = "CODING"
    VERIFYING = "VERIFYING"
    REVIEWING = "REVIEWING"
    COMMITTING = "COMMITTING"
    PUSHED = "PUSHED"
    PR_OPENED = "PR_OPENED"
    CI_WATCHING = "CI_WATCHING"
    DONE = "DONE"
    NEEDS_HUMAN = "NEEDS_HUMAN"
    FAILED = "FAILED"
    CANCELLED = "CANCELLED"


TERMINAL: frozenset[Status] = frozenset(
    {Status.REJECTED, Status.DONE, Status.FAILED, Status.CANCELLED}
)

SUSPENDED: frozenset[Status] = frozenset(
    {Status.NEEDS_INFO, Status.AWAITING_APPROVAL, Status.NEEDS_HUMAN}
)

_ACTIVE: frozenset[Status] = frozenset(Status) - TERMINAL

_RESUMABLE: frozenset[Status] = _ACTIVE - SUSPENDED

_BASE: dict[Status, frozenset[Status]] = {
    Status.QUEUED: frozenset({Status.TRIAGING}),
    Status.TRIAGING: frozenset({Status.REJECTED, Status.PLANNING}),
    Status.REJECTED: frozenset(),
    Status.PLANNING: frozenset({Status.NEEDS_INFO, Status.AWAITING_APPROVAL, Status.CODING}),
    Status.NEEDS_INFO: frozenset({Status.PLANNING}),
    Status.AWAITING_APPROVAL: frozenset({Status.CODING}),
    Status.CODING: frozenset({Status.VERIFYING}),
    Status.VERIFYING: frozenset({Status.CODING, Status.REVIEWING, Status.COMMITTING}),
    Status.REVIEWING: frozenset({Status.CODING, Status.COMMITTING}),
    Status.COMMITTING: frozenset({Status.PUSHED, Status.DONE}),
    Status.PUSHED: frozenset({Status.PR_OPENED}),
    Status.PR_OPENED: frozenset({Status.DONE, Status.CI_WATCHING}),
    Status.CI_WATCHING: frozenset({Status.CODING, Status.DONE}),
    Status.DONE: frozenset(),
    Status.NEEDS_HUMAN: _RESUMABLE,
    Status.FAILED: frozenset(),
    Status.CANCELLED: frozenset(),
}

def _build_transitions() -> dict[Status, frozenset[Status]]:
    table: dict[Status, frozenset[Status]] = {}
    for src in Status:
        dsts = set(_BASE[src])
        if src not in TERMINAL:
            dsts |= {Status.FAILED, Status.CANCELLED}
        if src in _ACTIVE and src != Status.NEEDS_HUMAN:
            dsts.add(Status.NEEDS_HUMAN)
        table[src] = frozenset(dsts)
    return table


TRANSITIONS: dict[Status, frozenset[Status]] = _build_transitions()


def can_transition(src: Status, dst: Status) -> bool:
    return dst in TRANSITIONS.get(src, frozenset())


def assert_transition(src: Status, dst: Status) -> None:
    if not can_transition(src, dst):
        raise InvalidTransition(f"非法状态转换: {src.value} -> {dst.value}")
