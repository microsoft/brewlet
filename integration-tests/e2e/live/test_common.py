# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

from common import Fixture, owned_container, redact, wait
from hpa import cpu_millicores, hpa_cpu_utilization


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
        fixture.candidate_image_id = None
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
