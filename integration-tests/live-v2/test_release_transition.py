"""Offline refusal tests; these never install, drain or remove a real node."""

from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import release_transition as transition


class TransitionTests(unittest.TestCase):
    def test_non_native_platform_stops_before_any_cluster_action(self):
        harness = SimpleNamespace(preflight=Mock(), provision_cluster=Mock(), phase="")
        with patch.object(transition.sys, "platform", "darwin"), self.assertRaisesRegex(
                Exception, "native Linux"):
            transition.execute(harness)
        harness.provision_cluster.assert_not_called()

    def test_failed_execution_or_cleanup_never_creates_success_receipt(self):
        for failure in ("execution", "cleanup"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as temp:
                directory = Path(temp)
                value = SimpleNamespace(
                    work=directory, result={"outcome": "failed"}, phase="test", arch="arm64",
                    stop_helpers=Mock(), diagnostics=Mock(), save=Mock())
                value.destroy = Mock(return_value=["cleanup failed"] if failure == "cleanup" else [])
                action = Mock(side_effect=RuntimeError("execution failed")) if failure == "execution" else Mock(
                    return_value=[{"name": "synthetic", "outcome": "passed"}])
                with patch.object(transition, "DistributionHarness", return_value=value), \
                        patch.object(transition, "execute", action), \
                        patch.object(transition.delivery, "cleanup"), \
                        patch.object(transition, "receipt") as receipt:
                    self.assertEqual(1, transition.run(directory, directory / "work"))
                    receipt.assert_not_called()
                self.assertFalse((directory / "evidence").exists())

    def test_host_reversal_failure_stops_before_reinstallation(self):
        harness = SimpleNamespace(run=Mock(), get=Mock(return_value={"items": []}),
                                  cluster="private", kubeconfig="private", context="private",
                                  control_namespace="private", node_snapshot=Mock(return_value={"metadata": {}}),
                                  node_exec=Mock(side_effect=RuntimeError("owned shim remains")))
        with self.assertRaisesRegex(RuntimeError, "owned shim remains"):
            transition.uninstall(harness)
        self.assertEqual(1, harness.node_exec.call_count)


if __name__ == "__main__":
    unittest.main()
