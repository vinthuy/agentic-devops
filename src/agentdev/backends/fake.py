from __future__ import annotations

from typing import Any

from agentdev.backends.base import StepContext, StepResult


class FakeBackend:
    """按脚本依次执行工具调用，用于测试与离线演示。

    script 是一个列表，每个元素对应一次 run_step 调用，内容为 (工具名, 参数) 的序列。
    脚本耗尽后，后续调用不做任何修改。
    """

    name = "fake"

    def __init__(self, script: list[list[tuple[str, dict[str, Any]]]]):
        self._script = list(script)
        self.calls: list[str] = []

    def run_step(self, ctx: StepContext) -> StepResult:
        self.calls.append(ctx.task_id)
        actions = self._script.pop(0) if self._script else []
        texts: list[str] = []
        for name, args in actions:
            result = ctx.tools.execute(name, args)
            texts.append(f"{name}: {'ok' if result.ok else 'error'} {result.content[:200]}")
        return StepResult(final_text="\n".join(texts), turns=len(actions))
