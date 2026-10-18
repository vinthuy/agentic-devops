from __future__ import annotations

from importlib import resources
from typing import Any

from agentdev.backends.base import StepContext, StepResult, Usage
from agentdev.errors import BackendError, BackendUnavailable


def load_prompt(name: str) -> str:
    return (resources.files("agentdev") / "prompts" / name).read_text(encoding="utf-8")


def _block_to_param(block: Any) -> dict[str, Any]:
    if block.type == "text":
        return {"type": "text", "text": block.text}
    if block.type == "tool_use":
        return {"type": "tool_use", "id": block.id, "name": block.name, "input": block.input}
    if block.type == "thinking":
        return {"type": "thinking", "thinking": block.thinking, "signature": block.signature}
    if block.type == "redacted_thinking":
        return {"type": "redacted_thinking", "data": block.data}
    raise BackendError(f"未支持的内容块类型: {block.type}")


class AnthropicBackend:
    """基于 Anthropic Messages 协议的工具调用循环（SDD 5.3）。

    同一协议可指向 Anthropic 官方或任何兼容端点（如 DeepSeek 的 base_url）。
    """

    name = "anthropic"

    def __init__(
        self,
        model: str,
        max_tokens: int,
        *,
        base_url: str | None = None,
        api_key: str | None = None,
        client: Any | None = None,
    ):
        self.model = model
        self.max_tokens = max_tokens
        if client is None:
            try:
                import anthropic
            except ImportError as e:
                raise BackendUnavailable(
                    "未安装 anthropic SDK，请执行: pip install -e \".[anthropic]\""
                ) from e
            kwargs: dict[str, Any] = {}
            if base_url:
                kwargs["base_url"] = base_url
            if api_key:
                kwargs["api_key"] = api_key
            client = anthropic.Anthropic(**kwargs)
        self.client = client
        self.system_prompt = load_prompt("coder.v1.md")

    def run_step(self, ctx: StepContext) -> StepResult:
        messages: list[dict[str, Any]] = [{"role": "user", "content": self._user_message(ctx)}]
        tools = ctx.tools.schemas()
        usage = Usage()
        final_text = ""
        turns = 0

        for turns in range(1, ctx.max_turns + 1):
            try:
                response = self.client.messages.create(
                    model=self.model,
                    max_tokens=self.max_tokens,
                    system=self.system_prompt,
                    tools=tools,
                    messages=messages,
                )
            except Exception as e:
                raise BackendError(f"模型调用失败: {e}") from e

            usage.add(Usage(response.usage.input_tokens, response.usage.output_tokens))
            messages.append(
                {"role": "assistant", "content": [_block_to_param(b) for b in response.content]}
            )
            final_text = "\n".join(b.text for b in response.content if b.type == "text")

            if response.stop_reason != "tool_use":
                return StepResult(final_text=final_text, turns=turns, usage=usage)

            results = []
            for block in response.content:
                if block.type != "tool_use":
                    continue
                outcome = ctx.tools.execute(block.name, dict(block.input))
                results.append(
                    {
                        "type": "tool_result",
                        "tool_use_id": block.id,
                        "content": outcome.content,
                        "is_error": not outcome.ok,
                    }
                )
            messages.append({"role": "user", "content": results})

        raise BackendError(f"超过最大轮数 {ctx.max_turns}，未完成本步骤")

    def _user_message(self, ctx: StepContext) -> str:
        parts = [
            f"<task_requirement source=\"user\">\n{ctx.requirement}\n</task_requirement>",
            f"<plan>\n{ctx.plan}\n</plan>",
        ]
        if ctx.failure_summary:
            parts.append(
                "<failure_summary trusted=\"false\">\n"
                f"{ctx.failure_summary}\n</failure_summary>"
            )
        return "\n\n".join(parts)
