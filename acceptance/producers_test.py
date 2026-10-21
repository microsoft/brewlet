import copy
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from acceptance import native_contracts, producer_check, run_check


ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("registry_check", ROOT / "integration-tests/registry.py")
registry = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(registry)


class ProducerTests(unittest.TestCase):
    def test_registry_uses_actual_native_architecture_and_checkout_cli(self):
        for architecture in ("amd64", "arm64"):
            command = producer_check.command("registry", architecture)
            self.assertEqual(["--registry-platform", "linux/" + architecture], command[-2:])
            self.assertEqual(["--brewlet", "./bin/brewlet"], command[2:4])
        self.assertEqual(["python3", "integration-tests/fixtures/check.py", "--brewlet", "./bin/brewlet"],
                         producer_check.command("fixtures", "arm64"))
        for name, architecture in (("unknown", "amd64"), ("registry", "unknown")):
            with self.assertRaises(ValueError):
                producer_check.command(name, architecture)

    def test_cross_built_producer_is_rejected_before_execution(self):
        with patch("sys.argv", ["producer_check.py", "registry"]), \
                patch.object(producer_check, "native_architecture", return_value="arm64"), \
                patch.object(producer_check.subprocess, "run",
                             return_value=SimpleNamespace(stdout="build GOOS=linux\nbuild GOARCH=amd64")) as command, \
                self.assertRaisesRegex(ValueError, "does not match"):
            producer_check.main()
        self.assertEqual(1, command.call_count)

    def test_native_fixed_commands_force_native_go_analysis_target(self):
        import tempfile
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(run_check, "working_identity", return_value={"checkout": "synthetic"}), \
                patch.object(run_check, "native_architecture", return_value="arm64"), \
                patch.object(run_check.subprocess, "run", return_value=SimpleNamespace(returncode=0)) as command:
            path = Path(directory) / "receipt.json"
            self.assertEqual(0, run_check.execute("go-vet", path))
            self.assertEqual("linux", command.call_args.kwargs["env"]["GOOS"])
            self.assertEqual("arm64", command.call_args.kwargs["env"]["GOARCH"])
            self.assertEqual("arm64", json.loads(path.read_text())["architecture"])

    def test_native_contract_security_and_isolation_match_each_suite(self):
        for name in native_contracts.SUITES:
            command = native_contracts.suite_command(name, Path("/owned"), "owned:test", "arm64", "owner")
            for value in ("--rm", "--read-only", "ALL", "SYS_ADMIN", "MKNOD",
                          "no-new-privileges", "apparmor=unconfined", "none", "linux/arm64",
                          "type=bind,source=/owned,target=/tests,readonly"):
                self.assertIn(value, command)
            self.assertEqual(command[-3:], ["/tests/" + name + ".test", "owned:test", "-test.v"])
            self.assertEqual(name in ("artifact", "shim"), "DAC_OVERRIDE" in command)
            self.assertEqual(name == "node", "SETFCAP" in command)

    def test_native_checks_reject_non_linux_and_unknown_architecture(self):
        for system, machine in (("Darwin", "arm64"), ("Linux", "unknown")):
            with patch.object(native_contracts.platform, "system", return_value=system), \
                    patch.object(native_contracts.platform, "machine", return_value=machine), \
                    self.assertRaisesRegex(ValueError, "native Linux"):
                native_contracts.native_architecture()

    def test_replaced_or_foreign_container_is_never_removed(self):
        for info in ({"Id": "b" * 64, "Config": {"Labels": {native_contracts.OWNER: "owner"}}},
                     {"Id": "a" * 64, "Config": {"Labels": {native_contracts.OWNER: "foreign"}}}):
            path = SimpleNamespace(exists=lambda: True, read_text=lambda: "a" * 64)
            with patch.object(native_contracts, "run", side_effect=["a" * 64, json.dumps([info])]) as run, \
                    self.assertRaisesRegex(ValueError, "ownership changed"):
                native_contracts.cleanup_container(path, "owner")
            self.assertTrue(all(call.args[0][:2] != ["docker", "rm"] for call in run.call_args_list))

    def test_full_descriptor_diff_id_and_provenance_are_preserved(self):
        descriptor = {"digest": "sha256:compressed", "size": 123, "mediaType": "layer",
                      "annotations": {"brewlet.sh/layer": "dependency"}}
        bindings = {"manifestDigest": "sha256:source", "configDigest": "sha256:config",
                    "lockDigest": "sha256:lock", "sbomDigest": "sha256:sbom",
                    "attestationDigest": "sha256:provenance",
                    "layers": [{"digest": "sha256:compressed", "diffId": "sha256:uncompressed"}]}
        source = {"layers": [{"descriptor": descriptor, "diffId": "sha256:uncompressed"}],
                  "evidence": bindings}
        manifest = {"layers": [descriptor], "annotations": {"brewlet.sh/layerset-evidence": json.dumps([bindings])}}
        config = {"rootfs": {"diff_ids": ["sha256:uncompressed"]}}
        registry.dependency_reuse(manifest, config, source)
        for key in ("digest", "size", "mediaType"):
            changed = copy.deepcopy(manifest)
            changed["layers"][0][key] = "changed"
            with self.subTest(key=key), self.assertRaises(AssertionError):
                registry.dependency_reuse(changed, config, source)
        with self.assertRaisesRegex(AssertionError, "diff ID"):
            registry.dependency_reuse(manifest, {"rootfs": {"diff_ids": ["changed"]}}, source)
        for key in bindings:
            changed = copy.deepcopy(manifest)
            changed["annotations"]["brewlet.sh/layerset-evidence"] = json.dumps([{**bindings, key: "changed"}])
            with self.subTest(binding=key), self.assertRaisesRegex(AssertionError, "provenance"):
                registry.dependency_reuse(changed, config, source)


if __name__ == "__main__":
    unittest.main()
