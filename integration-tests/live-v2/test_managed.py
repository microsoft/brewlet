import json
from pathlib import Path
import tempfile
import unittest

import managed
from harness import Failure


class ManagedFixtureTests(unittest.TestCase):
    def test_every_payload_file_gets_exact_hash_path_and_ecosystem_purl(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "nested").mkdir()
            (root / "one").write_bytes(b"one")
            (root / "nested/two").write_bytes(b"two")
            for ecosystem, prefix in (("java", "pkg:maven/"), ("python", "pkg:pypi/"),
                                      ("node", "pkg:npm/"), ("dotnet", "pkg:nuget/")):
                lock = managed.complete_lock(root, "/app/dependencies", ecosystem, "2.0.1")
                self.assertEqual(2, len(lock["entries"]))
                for entry in lock["entries"]:
                    self.assertTrue(entry["purl"].startswith(prefix))
                    relative = entry["path"].removeprefix("/app/dependencies/")
                    self.assertEqual(managed.hashlib.sha256((root / relative).read_bytes()).hexdigest(),
                                     entry["sha256"])
            (root / "alias").symlink_to("one")
            with self.assertRaises(Failure):
                managed.complete_lock(root, "/app/dependencies", "python", "2.0.1")

    def test_observable_dependency_update_is_explicit_and_not_an_app_rebuild(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "greeting.py"
            source.write_text('MESSAGE = "shared-contract"\n')
            managed.replace_fixture_message(root, ".py")
            self.assertIn("shared-contract-patched", source.read_text())
            with self.assertRaises(Failure):
                managed.replace_fixture_message(root, ".java")

    def test_native_scenario_keeps_external_scanning_and_controlled_availability(self):
        source = Path(managed.__file__).read_text()
        self.assertIn('"preStop": {"sleep": {"seconds": 5}}', source)
        self.assertIn('"maxUnavailable": 0', source)
        self.assertIn('after["applicationRoot"] == before["applicationRoot"]', source)
        self.assertIn("actual old image cohorts drained", source)
        self.assertIn('row["workload"]["kind"] == "Deployment"', source)
        self.assertNotIn("trivy", source.lower())
        self.assertNotIn("grype", source.lower())

    def test_dotnet_input_is_pinned_for_both_native_architectures(self):
        pins = json.loads((managed.FIXTURES / "runtimes/managed-dotnet.json").read_text())
        for architecture in ("arm64", "amd64"):
            self.assertRegex(pins[architecture], r"^registry-1\.docker\.io/ubuntu/dotnet-runtime@sha256:[0-9a-f]{64}$")
