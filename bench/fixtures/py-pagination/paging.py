def page(items, page, size):
    """返回第 page 页（从 1 开始）的切片。"""
    start = page * size
    return items[start:start + size]
