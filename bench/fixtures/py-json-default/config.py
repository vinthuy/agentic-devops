DEFAULT_RETRIES = 3


def with_defaults(config: dict, defaults: dict) -> dict:
    """返回合并后的配置；调用方传入的 defaults 不应被修改。"""
    defaults.update(config)
    return defaults
