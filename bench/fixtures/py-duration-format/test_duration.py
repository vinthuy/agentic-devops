import unittest

from duration import format_duration


class DurationTests(unittest.TestCase):
    def test_single_digit_seconds_are_padded(self):
        self.assertEqual(format_duration(65), "1:05")

    def test_zero(self):
        self.assertEqual(format_duration(0), "0:00")

    def test_exact_minute(self):
        self.assertEqual(format_duration(600), "10:00")


if __name__ == "__main__":
    unittest.main()
