import re

_EMAIL = re.compile(r"[a-z0-9]+@[a-z]+\.[a-z]+")


def is_valid_email(address: str) -> bool:
    return _EMAIL.fullmatch(address) is not None
