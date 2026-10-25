import unittest

from calc import add


class CoreTests(unittest.TestCase):
    """本地验证只跑这个文件。"""

    def test_positive(self):
        self.assertEqual(add(2, 3), 5)

    def test_negative(self):
        self.assertEqual(add(-2, -3), -5)


if __name__ == "__main__":
    unittest.main()
