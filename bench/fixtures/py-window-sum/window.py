def max_window_sum(values, size):
    """返回长度固定为 size 的连续子数组最大和。"""
    if size <= 0 or size > len(values):
        raise ValueError("size 非法")
    current = sum(values[:size - 1])
    best = current
    for i in range(size, len(values)):
        current += values[i] - values[i - size]
        if current > best:
            best = current
    return best
