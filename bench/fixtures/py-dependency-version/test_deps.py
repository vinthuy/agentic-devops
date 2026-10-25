import unittest

from deps import satisfies


class SatisfiesTests(unittest.TestCase):
    def test_plain_versions(self):
        self.assertTrue(satisfies("1.3.0", ">=1.2.0"))
        self.assertFalse(satisfies("1.1.9", ">=1.2.0"))

    def test_less_than(self):
        self.assertTrue(satisfies("1.9.0", "<2.0.0"))
        self.assertFalse(satisfies("2.0.0", "<2.0.0"))

    def test_exact(self):
        self.assertTrue(satisfies("1.2.3", "1.2.3"))
        self.assertFalse(satisfies("1.2.4", "1.2.3"))

    def test_leading_v_prefix(self):
        # 依赖清单里常见 "v1.2.3" 写法，应与 "1.2.3" 等价。
        self.assertTrue(satisfies("v1.2.3", ">=1.2.0"))
        self.assertTrue(satisfies("v1.2.3", "1.2.3"))
        self.assertFalse(satisfies("v1.1.0", ">=1.2.0"))


if __name__ == "__main__":
    unittest.main()
