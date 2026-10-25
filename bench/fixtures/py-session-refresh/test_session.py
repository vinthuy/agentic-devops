import unittest

import clock
from session import SessionStore


class SessionStoreTests(unittest.TestCase):
    def setUp(self):
        clock.set_now(1000.0)
        self.store = SessionStore(ttl=60)

    def tearDown(self):
        clock.set_now(None)

    def test_fresh_session_is_returned(self):
        self.store.issue("a")
        clock.set_now(1030.0)
        self.assertIsNotNone(self.store.get("a"))

    def test_expired_session_is_dropped(self):
        # 满 ttl 即失效：继续放行会多给一个请求窗口。
        self.store.issue("a")
        clock.set_now(1060.0)
        self.assertIsNone(self.store.get("a"))

    def test_refresh_extends_lifetime(self):
        self.store.issue("a")
        clock.set_now(1059.0)
        self.assertIsNotNone(self.store.refresh("a"))
        clock.set_now(1110.0)
        self.assertIsNotNone(self.store.get("a"))

    def test_refresh_of_expired_session_returns_none(self):
        self.store.issue("a")
        clock.set_now(1070.0)
        self.assertIsNone(self.store.refresh("a"))


if __name__ == "__main__":
    unittest.main()
