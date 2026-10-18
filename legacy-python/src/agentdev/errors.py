class AgentDevError(Exception):
    """所有可预期错误的基类。"""


class ConfigError(AgentDevError):
    pass


class InvalidTransition(AgentDevError):
    pass


class ConcurrentModification(AgentDevError):
    pass


class NotFound(AgentDevError):
    pass


class PolicyViolation(AgentDevError):
    def __init__(self, kind: str, detail: str):
        super().__init__(detail)
        self.kind = kind
        self.detail = detail


class WorkspaceError(AgentDevError):
    pass


class BackendError(AgentDevError):
    """可恢复的后端错误（如限流、超时），任务进入 NEEDS_HUMAN。"""


class BackendUnavailable(AgentDevError):
    """后端依赖缺失，例如未安装 anthropic SDK。"""
