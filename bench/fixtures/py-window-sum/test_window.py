import unittest

from window import max_window_sum


class WindowTests(unittest.TestCase):
    def test_simple_peak(self):
        self.assertEqual(max_window_sum([1, 4, 2, 10, 2, 3, 1, 0, 20], 4), 24)

    def test_window_equals_length(self):
        self.assertEqual(max_window_sum([1, 2, 3], 3), 6)

    def test_all_negative(self):
        self.assertEqual(max_window_sum([-5, -2, -9, -1], 2), -7)

    def test_invalid_size(self):
        with self.assertRaises(ValueError):
            max_window_sum([1], 0)


if __name__ == "__main__":
    unittest.main()
