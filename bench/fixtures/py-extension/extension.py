def split_ext(name: str):
    """把文件名拆成 (stem, ext)；没有扩展名时 ext 为空串，隐藏文件不算扩展名。"""
    i = name.rfind(".")
    return name[:i], name[i:]
