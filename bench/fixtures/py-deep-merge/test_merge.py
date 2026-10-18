import unittest

from merge import deep_merge


class MergeTests(unittest.TestCase):
    def test_nested_dicts_are_merged(self):
        base = {"server": {"host": "localhost", "port": 80}}
        override = {"server": {"port": 8080}}
        self.assertEqual(deep_merge(base, override), {"server": {"host": "localhost", "port": 8080}})

    def test_scalars_are_replaced(self):
        self.assertEqual(deep_merge({"a": 1}, {"a": 2}), {"a": 2})

    def test_inputs_are_not_mutated(self):
        base = {"a": {"b": 1}}
        deep_merge(base, {"a": {"c": 2}})
        self.assertEqual(base, {"a": {"b": 1}})


if __name__ == "__main__":
    unittest.main()
