import re


def slugify(text: str) -> str:
    # 连续的空白与标点应折叠成一个连字符。
    return re.sub(r"[^a-z0-9]", "-", text.lower()).strip("-")
