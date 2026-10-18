import pytest

from agentdev.errors import InvalidTransition
from agentdev.state import TERMINAL, Status, assert_transition, can_transition


@pytest.mark.parametrize(
    "src, dst",
    [
        (Status.QUEUED, Status.TRIAGING),
        (Status.TRIAGING, Status.PLANNING),
        (Status.TRIAGING, Status.REJECTED),
        (Status.PLANNING, Status.CODING),
        (Status.PLANNING, Status.AWAITING_APPROVAL),
        (Status.CODING, Status.VERIFYING),
        (Status.VERIFYING, Status.CODING),
        (Status.VERIFYING, Status.COMMITTING),
        (Status.COMMITTING, Status.DONE),
        (Status.COMMITTING, Status.PUSHED),
        (Status.NEEDS_HUMAN, Status.CODING),
        (Status.CODING, Status.NEEDS_HUMAN),
        (Status.CODING, Status.FAILED),
        (Status.QUEUED, Status.CANCELLED),
    ],
)
def test_allowed_transitions(src, dst):
    assert can_transition(src, dst)
    assert_transition(src, dst)


@pytest.mark.parametrize(
    "src, dst",
    [
        (Status.QUEUED, Status.DONE),
        (Status.QUEUED, Status.CODING),
        (Status.CODING, Status.DONE),
        (Status.DONE, Status.CODING),
        (Status.REJECTED, Status.TRIAGING),
        (Status.NEEDS_HUMAN, Status.NEEDS_HUMAN),
        (Status.AWAITING_APPROVAL, Status.DONE),
    ],
)
def test_forbidden_transitions(src, dst):
    assert not can_transition(src, dst)
    with pytest.raises(InvalidTransition):
        assert_transition(src, dst)


def test_terminal_states_have_no_outgoing_transitions():
    from agentdev.state import TRANSITIONS

    for status in TERMINAL:
        assert TRANSITIONS[status] == frozenset(), status
