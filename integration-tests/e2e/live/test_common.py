# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

from common import GC_STARTUP_DELAY, Fixture, kind_config, owned_container, redact, wait
from hpa import assert_cold_start_retention, cpu_millicores, hpa_cpu_utilization


class FixtureSafetyTests(unittest.TestCase):
    def test_container_requires_both_identity_and_owner(self):
        info = {"Id": "ours", "Config": {"Labels": {"owner": "invocation"}}}
        self.assertTrue(owned_container(info, "ours", "owner", "invocation"))
        self.assertFalse(owned_container(info, "replaced", "owner", "invocation"))
        self.assertFalse(owned_container(info, "ours", "owner", "foreign"))

    def test_target_is_explicit_and_cannot_be_overridden(self):
        fixture = Fixture.__new__(Fixture)
        fixture.kubeconfig = Path("/owned/private/kubeconfig")
        fixture.context = "kind-owned"
        with patch.object(fixture, "run") as execute:
            fixture.kube("get", "nodes")
            command = execute.call_args.args[0]
            self.assertEqual(command[1:5], ["--kubeconfig", fixture.kubeconfig,
                                           "--context", "kind-owned"])
            for arg in ("--context=shared", "--kubeconfig", "--server=x", "--cluster=x"):
                with self.assertRaises(ValueError):
                    fixture.kube("get", "nodes", arg)

    def test_wait_is_bounded_and_does_not_swallow_failures(self):
        with self.assertRaises(TimeoutError):
            wait("mandatory", lambda: False, timeout=0)
        with self.assertRaisesRegex(RuntimeError, "API unavailable"):
            wait("mandatory", lambda: (_ for _ in ()).throw(RuntimeError("API unavailable")))

    def test_private_material_is_redacted(self):
        secret = "-----BEGIN EC PRIVATE KEY-----\nSECRET\n-----END EC PRIVATE KEY-----"
        self.assertNotIn("SECRET", redact(secret))
        self.assertNotIn("credential", redact("Authorization: Bearer credential"))

    def test_cpu_quantities(self):
        for value, expected in [("50000000n", 50), ("50000u", 50), ("50m", 50), ("0.5", 500)]:
            self.assertEqual(cpu_millicores(value), expected)
        with self.assertRaises(ValueError):
            cpu_millicores("synthetic")

    def test_preferred_hpa_api_cpu_utilization(self):
        self.assertEqual(hpa_cpu_utilization({"currentCPUUtilizationPercentage": 70}), 70)
        self.assertEqual(hpa_cpu_utilization({"currentMetrics": [
            {"resource": {"name": "cpu", "current": {"averageUtilization": 70}}}]}), 70)
        self.assertEqual(hpa_cpu_utilization({"currentMetrics": [{"type": ""}]}), 0)

    def test_kind_config_defers_containerd_gc_and_keeps_registry_patch(self):
        config = kind_config("owned")
        self.assertIn('[plugins."io.containerd.gc.v1.scheduler"]', config)
        self.assertIn(f'startup_delay = "{GC_STARTUP_DELAY}"', config)
        self.assertIn("deletion_threshold = 0", config)
        self.assertIn('config_path = "/etc/containerd/certs.d"', config)
        self.assertIn("sh.brewlet/live-owner: owned", config)
        self.assertIn('horizontal-pod-autoscaler-sync-period: "10s"', config)

    def test_gc_request_uses_containerd_collector_on_owned_node(self):
        fixture = Fixture.__new__(Fixture)
        fixture.node, fixture.node_id = "owned-node", "node-id"
        with patch.object(fixture, "own_container") as owned, \
                patch.object(fixture, "run") as execute:
            lease = fixture.request_containerd_gc()
            owned.assert_called_once_with("owned-node")
        prefix = ["docker", "exec", "node-id", "ctr", "-n", "k8s.io", "leases"]
        self.assertEqual([c.args[0] for c in execute.call_args_list], [
            [*prefix, "create", "--id", lease],
            [*prefix, "delete", "--sync", lease],
        ])
        self.assertTrue(lease.startswith("brewlet-live-gc-"))

    def test_cold_start_retention_fails_when_layers_were_collected(self):
        fixture = Mock()
        fixture.node_blob_present.side_effect = lambda d: d == "sha256:" + "a" * 64
        layers = ["sha256:" + "a" * 64, "sha256:" + "b" * 64]
        with patch("hpa.runnable_layers", return_value=("sha256:" + "c" * 64, layers)):
            with self.assertRaisesRegex(AssertionError, "b" * 64):
                assert_cold_start_retention(fixture, "registry:5000/apps/demo@sha256:x")
            fixture.record.assert_not_called()
            fixture.node_blob_present.side_effect = None
            fixture.node_blob_present.return_value = True
            assert_cold_start_retention(fixture, "registry:5000/apps/demo@sha256:x")
            fixture.record.assert_called_once()

    def test_cleanup_refuses_replaced_container(self):
        fixture = Fixture.__new__(Fixture)
        fixture.node = "owned-node"
        fixture.node_id = "original"
        result = subprocess.CompletedProcess([], 0, json.dumps([
            {"Id": "replacement", "Config": {"Labels": {"io.x-k8s.kind.cluster": "owned"}}}
        ]), "")
        fixture.name = "owned"
        with patch.object(fixture, "run", return_value=result):
            with self.assertRaisesRegex(RuntimeError, "foreign/replaced"):
                fixture.own_container("owned-node")

    def test_failure_log_error_still_cleans_up(self):
        fixture = Fixture.__new__(Fixture)
        with patch.object(fixture, "save", side_effect=OSError("disk full")), \
                patch.object(fixture, "finish") as finish:
            with self.assertRaises(OSError):
                fixture.__exit__(RuntimeError, RuntimeError("failed"), None)
            finish.assert_called_once_with(False)

    def test_finish_removes_owned_volumes_after_child_race(self):
        fixture = Fixture.__new__(Fixture)
        fixture.node, fixture.node_id = "owned-node", "node-id"
        fixture.registry_name, fixture.registry_id = "owned-registry", "registry-id"
        fixture.network_id = None
        fixture.old_signals = {}
        fixture.scenario = "hpa"
        fixture.evidence = []
        fixture.private = Path(tempfile.mkdtemp())
        child = Mock()
        child.poll.return_value = None
        child.terminate.side_effect = ProcessLookupError("already exited")
        fixture.children = [child]
        with patch.object(fixture, "diagnostics"), patch.object(fixture, "own_container"), \
                patch.object(fixture, "save"), patch.object(fixture, "run") as execute:
            fixture.finish(False)
            self.assertEqual([c.args[0] for c in execute.call_args_list], [
                ["docker", "rm", "-f", "--volumes", "node-id"],
                ["docker", "rm", "-f", "--volumes", "registry-id"],
            ])
        self.assertFalse(fixture.private.exists())


if __name__ == "__main__":
    unittest.main()
