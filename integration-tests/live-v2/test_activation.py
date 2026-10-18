"""Offline fixture-transform safeguards, not production activation proof."""

import unittest
import tomllib

from activation import unsafe_retention_config
from harness import Failure


class ActivationFixtureTests(unittest.TestCase):
    def test_only_exact_effective_retention_field_changes(self):
        for version, section in (
                (2, "plugins.'io.containerd.grpc.v1.cri'.containerd"),
                (3, "plugins.'io.containerd.cri.v1.images'")):
            text = f"version = {version}\n[{section}]\n  discard_unpacked_layers = false\n"
            changed = unsafe_retention_config(text)
            self.assertIn("exact pre-Brewlet fixture bytes", changed)
            self.assertIn("discard_unpacked_layers = true", changed)
            expected = tomllib.loads(text.replace("= false", "= true"))
            self.assertEqual(tomllib.loads(changed), expected)

    def test_missing_duplicate_or_wrong_retention_cannot_fake_unsafe_fixture(self):
        for text in (
                "version=3\n",
                "version=1\ndiscard_unpacked_layers=false\n",
                "version=3\n[other]\ndiscard_unpacked_layers=false\n",
                "version=3\n[plugins.'io.containerd.cri.v1.images']\n"
                "discard_unpacked_layers=false\n[other]\ndiscard_unpacked_layers=false\n"):
            with self.assertRaises((Failure, KeyError)):
                unsafe_retention_config(text)


if __name__ == "__main__":
    unittest.main()
