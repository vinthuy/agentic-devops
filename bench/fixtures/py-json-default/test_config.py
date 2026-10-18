import unittest

from config import DEFAULT_RETRIES, with_defaults


class ConfigTests(unittest.TestCase):
    def test_override_wins(self):
        merged = with_defaults({"retries": 5}, {"retries": DEFAULT_RETRIES, "timeout": 10})
        self.assertEqual(merged["retries"], 5)
        self.assertEqual(merged["timeout"], 10)

    def test_defaults_are_not_mutated(self):
        defaults = {"retries": DEFAULT_RETRIES}
        with_defaults({"timeout": 10}, defaults)
        self.assertEqual(defaults, {"retries": DEFAULT_RETRIES})


if __name__ == "__main__":
    unittest.main()
