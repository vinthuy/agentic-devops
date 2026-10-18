def format_duration(seconds: int) -> str:
    """把秒数格式化为 mm:ss。"""
    minutes = seconds // 60
    rest = seconds % 60
    return "{}:{}".format(minutes, rest)
