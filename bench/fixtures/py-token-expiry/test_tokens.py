import unittest

from tokens import is_expired


class TokenExpiryTests(unittest.TestCase):
    def test_not_yet_expired(self):
        self.assertFalse(is_expired(1000.0, 60.0, now=1030.0))

    def test_expired_past_ttl(self):
        self.assertTrue(is_expired(1000.0, 60.0, now=1061.0))

    def test_expired_exactly_at_ttl(self):
        # 恰好到 ttl 时视为过期：继续放行会多给一个请求窗口。
        self.assertTrue(is_expired(1000.0, 60.0, now=1060.0))

    def test_zero_ttl_is_immediately_expired(self):
        self.assertTrue(is_expired(1000.0, 0.0, now=1000.0))


if __name__ == "__main__":
    unittest.main()
