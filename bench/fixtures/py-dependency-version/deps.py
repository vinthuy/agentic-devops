def parse_version(version):
    """把 "1.2.3" 拆成可比较的整数元组。"""
    return tuple(int(part) for part in version.split("."))


def satisfies(version, constraint):
    """判断 version 是否满足形如 ">=1.2.0" / "<2.0.0" / "1.2.3" 的约束。"""
    if constraint.startswith(">="):
        return parse_version(version) >= parse_version(constraint[2:])
    if constraint.startswith("<"):
        return parse_version(version) < parse_version(constraint[1:])
    return parse_version(version) == parse_version(constraint)
