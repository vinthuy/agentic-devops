import unittest

from paging import page


class PagingTests(unittest.TestCase):
    def test_first_page_starts_at_zero(self):
        self.assertEqual(page(list(range(10)), 1, 3), [0, 1, 2])

    def test_second_page(self):
        self.assertEqual(page(list(range(10)), 2, 3), [3, 4, 5])

    def test_last_page_is_partial(self):
        self.assertEqual(page(list(range(10)), 4, 3), [9])


if __name__ == "__main__":
    unittest.main()
