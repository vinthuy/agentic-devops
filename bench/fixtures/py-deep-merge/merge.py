def deep_merge(base: dict, override: dict) -> dict:
    """递归合并配置；嵌套字典应合并而不是整体覆盖。"""
    result = dict(base)
    for key, value in override.items():
        result[key] = value
    return result
