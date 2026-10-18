import unittest

from slug import slugify


class SlugTests(unittest.TestCase):
    def test_simple_words(self):
        self.assertEqual(slugify("Hello World"), "hello-world")

    def test_collapses_punctuation_runs(self):
        self.assertEqual(slugify("Hello,  World!"), "hello-world")

    def test_strips_edges(self):
        self.assertEqual(slugify("  --Hello--  "), "hello")


if __name__ == "__main__":
    unittest.main()
