def quote_field(value: str) -> str:
    """按 CSV 规则引用字段：含逗号、引号或换行时需要加引号。"""
    if "," in value or '"' in value or "\n" in value:
        return '"' + value + '"'
    return value
