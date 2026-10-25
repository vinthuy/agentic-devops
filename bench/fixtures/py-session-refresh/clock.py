import time

_override = None


def now():
    """当前时间。测试通过 set_now 注入，避免依赖真实时钟。"""
    if _override is not None:
        return _override
    return time.time()


def set_now(value):
    global _override
    _override = value


def is_expired(issued_at, ttl, current):
    """判断签发于 issued_at、有效期为 ttl 的对象在 current 时刻是否已过期。"""
    return current - issued_at > ttl
