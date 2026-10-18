from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol

from agentdev.tools import ToolRuntime


@dataclass
class Usage:
    input_tokens: int = 0
    output_tokens: int = 0

    def add(self, other: "Usage") -> None:
        self.input_tokens += other.input_tokens
        self.output_tokens += other.output_tokens


@dataclass
class StepContext:
    task_id: str
    requirement: str
    plan: str
    worktree: Path
    tools: ToolRuntime
    max_turns: int
    failure_summary: str | None = None


@dataclass
class StepResult:
    final_text: str
    turns: int = 0
    usage: Usage = field(default_factory=Usage)


class ExecutionBackend(Protocol):
    name: str

    def run_step(self, ctx: StepContext) -> StepResult: ...
