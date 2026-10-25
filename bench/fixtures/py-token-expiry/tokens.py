import time


def is_expired(issued_at, ttl_seconds, now=None):
    """Return True when the token issued at `issued_at` has exceeded its ttl."""
    if now is None:
        now = time.time()
    return now - issued_at > ttl_seconds
