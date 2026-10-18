import unittest

from validators import is_valid_email


class EmailTests(unittest.TestCase):
    def test_simple_address(self):
        self.assertTrue(is_valid_email("alice@example.com"))

    def test_plus_tag_is_valid(self):
        self.assertTrue(is_valid_email("alice+news@example.com"))

    def test_missing_at_is_invalid(self):
        self.assertFalse(is_valid_email("alice.example.com"))


if __name__ == "__main__":
    unittest.main()
