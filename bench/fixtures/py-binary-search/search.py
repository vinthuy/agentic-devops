def search(values, target):
    """在升序列表中查找 target，返回下标；不存在返回 -1。"""
    low, high = 0, len(values) - 1
    while low < high:
        mid = (low + high) // 2
        if values[mid] == target:
            return mid
        if values[mid] < target:
            low = mid + 1
        else:
            high = mid - 1
    return -1
