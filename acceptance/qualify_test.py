import copy
import json
from pathlib import Path
import tempfile
import subprocess
import unittest
from unittest.mock import patch

from acceptance import qualify, run_check
import evidence_test


class SuiteTests(unittest.TestCase):
    def setUp(self):
        self.fixture = evidence_test.EvidenceTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.root = self.fixture.root
        self.directory = self.root / "acceptance/results"
        self.identity = self.fixture.identity
        for architecture in ("amd64", "arm64"):
            for suite, (_, scenario, languages) in qualify.SUITES.items():
                result = {**copy.deepcopy(self.fixture.result), "architecture": architecture,
                          "selectedScenario": scenario, "selectedLanguages": list(languages)}
                if suite == "tutorial":
                    result["scope"] = "integration"
                if suite == "core":
                    result.update(scope="real-node-execution-and-remote-delivery",
                                  kubeletPullVerified=True, tlsTransportVerified=True)
                (self.directory / f"candidate-{architecture}-{suite}.json").write_text(json.dumps(result))
        self.fixture.commit()

    def test_every_expected_suite_and_both_architectures_can_qualify(self):
        self.assertEqual([], qualify.validate_suites(self.directory, self.root))
        self.assertEqual(9, len(qualify.COMPLETE))
        self.assertEqual({"managed", "managed-wasm"}, set(qualify.SUITES) - set(qualify.COMPLETE))

    def test_status_inventory_distinguishes_missing_failed_and_qualified_receipts(self):
        (self.directory / "candidate-amd64-cold.json").unlink()
        failed = self.directory / "candidate-arm64-managed.json"
        result = json.loads(failed.read_text())
        result["outcome"] = "failed"
        failed.write_text(json.dumps(result))
        self.fixture.commit()
        statuses = qualify.suite_statuses(self.directory, self.root)
        self.assertEqual(22, len(statuses))
        self.assertEqual(22, len({row["file"] for row in statuses}))
        self.assertEqual(1, sum(row["status"] == "missing" for row in statuses))
        self.assertEqual(1, sum(row["status"] == "failed" for row in statuses))
        self.assertEqual(20, sum(row["status"] == "qualified" for row in statuses))
        for row in statuses:
            if row["status"] != "missing":
                self.assertEqual(self.identity, row["identity"])

    def test_tutorial_execution_failure_leaves_failed_public_receipt(self):
        output = self.root / "acceptance-export"
        with patch.object(qualify, "ROOT", self.root), \
                patch.object(qualify, "COMPLETE", ("tutorial",)), \
                patch.object(qualify, "working_identity", return_value=self.identity), \
                patch.object(qualify.platform, "system", return_value="Linux"), \
                patch.object(qualify.platform, "machine", return_value="x86_64"), \
                patch.object(qualify.subprocess, "run",
                             side_effect=subprocess.CalledProcessError(1, ["tutorial"])):
            with self.assertRaises(subprocess.CalledProcessError):
                qualify.run("complete", "amd64", output)
        result = json.loads((output / "candidate-amd64-tutorial.json").read_text())
        self.assertEqual("failed", result["outcome"])
        self.assertEqual(self.identity["checkout"], result["checkout"])
        self.assertNotIn("productionCleanupVerified", result)
        self.assertTrue(all(row["outcome"] == "failed" for row in result["assertions"]))

    def test_input_change_after_tutorial_cannot_export_success(self):
        output = self.root / "acceptance-export"
        with patch.object(qualify, "ROOT", self.root), \
                patch.object(qualify, "COMPLETE", ("tutorial",)), \
                patch.object(qualify, "working_identity",
                             side_effect=[self.identity, ValueError("dirty checkout")]), \
                patch.object(qualify, "suite_errors", return_value=[]), \
                patch.object(qualify.platform, "system", return_value="Linux"), \
                patch.object(qualify.platform, "machine", return_value="x86_64"), \
                patch.object(qualify.subprocess, "run"):
            with self.assertRaisesRegex(ValueError, "dirty checkout"):
                qualify.run("complete", "amd64", output)
        result = json.loads((output / "candidate-amd64-tutorial.json").read_text())
        self.assertEqual("failed", result["outcome"])
        self.assertEqual("failed", result["assertions"][-1]["outcome"])

    def test_omitted_failed_partial_wrong_architecture_and_stale_suites_fail(self):
        path = self.directory / "candidate-arm64-core.json"
        original = path.read_text()
        for change in ({"outcome": "failed"}, {"outcome": "skipped"}, {"selectedScenario": "control"},
                       {"architecture": "amd64"}, {"qualificationInputsSHA256": "0" * 64},
                       {"productionCleanupVerified": False}, {"tlsTransportVerified": False},
                       {"scope": "real-node-execution-only"}):
            with self.subTest(change=change):
                path.write_text(json.dumps({**json.loads(original), **change}))
                self.fixture.commit()
                self.assertTrue(qualify.validate_suites(self.directory, self.root))
        path.unlink()
        self.fixture.commit()
        self.assertTrue(qualify.validate_suites(self.directory, self.root))

    def test_cross_architecture_and_non_linux_execution_rejected_before_commands(self):
        with patch.object(qualify.platform, "system", return_value="Linux"), \
                patch.object(qualify.platform, "machine", return_value="x86_64"), \
                patch.object(qualify.subprocess, "run") as command:
            with self.assertRaisesRegex(ValueError, "native Linux"):
                qualify.run("complete", "arm64", self.directory)
            command.assert_not_called()

    def test_recorded_command_failure_never_exports_success(self):
        output = self.root / "receipt.json"
        with patch.object(run_check, "working_identity", return_value=self.identity), \
                patch.object(run_check.subprocess, "run") as command:
            command.return_value.returncode = 7
            self.assertEqual(7, run_check.execute("maven", output))
            result = json.loads(output.read_text())
            self.assertEqual("failed", result["outcome"])
            self.assertEqual("failed", result["assertions"][0]["outcome"])
            self.assertEqual(run_check.CHECKS["maven"]["command"], command.call_args.args[0])

    def test_dirty_inputs_after_execution_preserve_failed_receipt(self):
        output = self.root / "receipt.json"
        with patch.object(run_check, "working_identity",
                          side_effect=[self.identity, ValueError("dirty checkout")]), \
                patch.object(run_check.subprocess, "run") as command:
            command.return_value.returncode = 0
            self.assertEqual(1, run_check.execute("maven", output))
        result = json.loads(output.read_text())
        self.assertEqual("failed", result["outcome"])
        self.assertEqual("failed", result["assertions"][-1]["outcome"])
        self.assertEqual(self.identity["checkout"], result["checkout"])

    def test_missing_executable_preserves_failed_receipt(self):
        output = self.root / "receipt.json"
        with patch.object(run_check, "working_identity", return_value=self.identity), \
                patch.object(run_check.subprocess, "run", side_effect=FileNotFoundError("missing tool")):
            self.assertEqual(1, run_check.execute("maven", output))
        result = json.loads(output.read_text())
        self.assertEqual("failed", result["outcome"])
        self.assertEqual("failed", result["assertions"][0]["outcome"])

    def test_all_fixed_checks_run_even_after_a_failed_command(self):
        with patch("sys.argv", ["run_check.py", "all"]), \
                patch.object(run_check, "execute", side_effect=[1, 0, 0]) as execute:
            self.assertEqual(1, run_check.main())
        self.assertEqual(list(run_check.CHECKS), [call.args[0] for call in execute.call_args_list])


if __name__ == "__main__":
    unittest.main()
