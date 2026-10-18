"""Offline fixture-transform safeguards, not production activation proof."""

import unittest
import tomllib

from activation import protected_cache_roots, unsafe_retention_config
from harness import Failure


class ActivationFixtureTests(unittest.TestCase):
    def test_cache_provenance_requires_actual_canonical_digest_policy_roots(self):
        root = "/var/lib/brewlet/layers/v2-3-" + "a" * 64 + "/rootfs"
        self.assertEqual(protected_cache_roots({"layerRoots": [root]}), [root])
        for roots in (None, [], ["/other/rootfs"], [root + "/../other"], [root, None],
                      root, [root.replace("a" * 64, "bad")]):
            with self.subTest(roots=roots), self.assertRaises(Failure):
                protected_cache_roots({"layerRoots": roots})

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
