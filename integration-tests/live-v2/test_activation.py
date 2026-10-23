"""Offline fixture-transform safeguards, not production activation proof."""

import unittest
import tomllib
import json
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
from unittest.mock import Mock

import delivery
from activation import (failed_attempt_times, invalid_path_runtime, protected_cache_roots,
                        runc_lifecycle, shim_kill_command, unsafe_retention_config)
from harness import Failure


class ActivationFixtureTests(unittest.TestCase):
    def test_exec_lifecycle_proof_requires_exact_nonzero_code_and_output(self):
        for code, output in ((0, "runc-exec-output\n"), (37, "wrong"), (42, "runc-exec-output\n")):
            h = SimpleNamespace(exec=Mock(return_value=SimpleNamespace(returncode=code, stdout=output)),
                                record=Mock())
            with self.subTest(code=code, output=output), self.assertRaisesRegex(Failure, "nonzero exit code"):
                runc_lifecycle(h, "owned-survivor")
            h.record.assert_not_called()

    def test_invalid_runtime_changes_only_explicit_env_not_root_layers_or_original(self):
        with TemporaryDirectory() as work:
            source, destination = Path(work) / "source", Path(work) / "invalid"
            config = {"config": {"Env": ["PATH=/bin", "OTHER=kept"]},
                      "rootfs": {"type": "layers", "diff_ids": []}}
            manifest = {"schemaVersion": 2, "mediaType": delivery.MANIFEST, "layers": [],
                        "config": delivery.json_blob(source, config, delivery.CONFIG)}
            target = delivery.finish_layout(source, delivery.json_blob(source, manifest, delivery.MANIFEST))
            changed = invalid_path_runtime(source, destination)
            actual = json.loads(delivery.read_blob(destination, changed))
            new_config = json.loads(delivery.read_blob(destination, actual["config"]))
            self.assertEqual(new_config["config"]["Env"], ["PATH=/app", "OTHER=kept"])
            self.assertEqual(new_config["rootfs"], config["rootfs"])
            self.assertEqual(actual["layers"], manifest["layers"])
            self.assertEqual(json.loads(delivery.read_blob(source, target)), manifest)
            self.assertEqual(json.loads(delivery.read_blob(source, manifest["config"])), config)

    def test_retry_timestamps_require_exact_install_failure_and_root_reason(self):
        lines = [
            "2026-06-01T12:00:00.123456789Z provision-error: runtime install failed: unsafe runtime PATH component",
            "2026-06-01T12:00:01Z provision-error: runtime install failed: unsafe runtime PATH component",
            "2026-06-01T12:00:02Z unrelated unsafe runtime PATH component",
            "2026-06-01T12:00:03Z provision-error: another failure",
        ]
        times = failed_attempt_times("\n".join(lines))
        self.assertEqual(len(times), 2)
        self.assertAlmostEqual((times[1] - times[0]).total_seconds(), 0.876544)

    def test_task_server_kill_requires_exact_start_and_installed_brewlet_binary(self):
        command = shim_kill_command(42, "123", "/usr/local/bin/containerd-shim-brewlet-v2")
        self.assertIn("readlink /proc/$1/exe", command[2])
        self.assertIn("kill -KILL", command[2])
        self.assertEqual(command[-3:], ["42", "123", "/usr/local/bin/containerd-shim-brewlet-v2"])
        for args in ((1, "123", "/usr/local/bin/containerd-shim-brewlet-v2"),
                     (42, "invalid", "/usr/local/bin/containerd-shim-brewlet-v2"),
                     (42, "123", "/usr/bin/containerd"),
                     (42, "123", "/usr/bin/kubelet")):
            with self.subTest(args=args), self.assertRaises(Failure):
                shim_kill_command(*args)

    def test_cache_provenance_requires_actual_canonical_digest_policy_roots(self):
        root = "/var/lib/brewlet/layers/v1-3-" + "a" * 64 + "/rootfs"
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
