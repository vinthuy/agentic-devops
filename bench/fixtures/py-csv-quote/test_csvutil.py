import unittest

from csvutil import quote_field


class CsvTests(unittest.TestCase):
    def test_plain_field(self):
        self.assertEqual(quote_field("abc"), "abc")

    def test_comma_is_quoted(self):
        self.assertEqual(quote_field("a,b"), '"a,b"')

    def test_inner_quotes_are_doubled(self):
        self.assertEqual(quote_field('say "hi"'), '"say ""hi"""')


if __name__ == "__main__":
    unittest.main()
