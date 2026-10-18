import unittest

from calc import add


class EdgeTests(unittest.TestCase):
    """只在 CI 里跑的边界约束：local verify 刻意不包含本文件。"""

    def test_none_is_rejected_with_value_error(self):
        with self.assertRaises(ValueError):
            add(None, 1)

    def test_bool_is_rejected(self):
        # bool 是 int 的子类，容易被忽略；这里要求显式拒绝。
        with self.assertRaises(ValueError):
            add(True, 1)


if __name__ == "__main__":
    unittest.main()
