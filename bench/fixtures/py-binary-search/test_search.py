import unittest

from search import search


class SearchTests(unittest.TestCase):
    def test_finds_middle(self):
        self.assertEqual(search([1, 3, 5, 7, 9], 5), 2)

    def test_finds_last_element(self):
        self.assertEqual(search([1, 3, 5, 7, 9], 9), 4)

    def test_finds_first_element(self):
        self.assertEqual(search([1, 3, 5, 7, 9], 1), 0)

    def test_missing_returns_minus_one(self):
        self.assertEqual(search([1, 3, 5], 4), -1)

    def test_empty(self):
        self.assertEqual(search([], 1), -1)


if __name__ == "__main__":
    unittest.main()
