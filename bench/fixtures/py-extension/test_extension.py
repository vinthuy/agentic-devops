import unittest

from extension import split_ext


class ExtensionTests(unittest.TestCase):
    def test_simple(self):
        self.assertEqual(split_ext("a.txt"), ("a", ".txt"))

    def test_multiple_dots_keep_last(self):
        self.assertEqual(split_ext("a.tar.gz"), ("a.tar", ".gz"))

    def test_no_extension(self):
        self.assertEqual(split_ext("README"), ("README", ""))

    def test_hidden_file_has_no_extension(self):
        self.assertEqual(split_ext(".env"), (".env", ""))


if __name__ == "__main__":
    unittest.main()
