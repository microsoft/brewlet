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
        self.assertFalse(result["cleanSelectedSuitePassed"])
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
                       {"selectedLanguages": [["java"], "python", "node", "dotnet"]},
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

    def test_pr_requires_actual_each_language_start_rotation_identity_and_cleanup_but_is_not_core(self):
        result = {**self.fixture.result, **self.identity, "selectedScenario": "pr", "fullAcceptance": False}
        names = [language + suffix for language in qualify.NATIVE for suffix in (
            " real startup, HTTP, dependency, exec/probe, identity and readonly mounts",
            " source rotation, unchanged app, retained task/exec lease, new barrier",
            " actual protected CRI identity resolves verified native descriptors and config")]
        result["assertions"] = [{"name": name, "outcome": "passed", "requirements": []} for name in names]
        self.assertEqual([], qualify.pr_errors(result, "amd64", self.root, self.identity))
        self.assertTrue(qualify.suite_errors(result, "core", "amd64", self.root, self.identity))
        for change in ("missing-language", "missing-identity", "cleanup", "destruction", "full-claim", "failed"):
            value = copy.deepcopy(result)
            if change == "missing-language":
                value["selectedLanguages"] = ["python"]
            elif change == "missing-identity":
                value["assertions"].pop()
            elif change == "cleanup":
                value["productionCleanupVerified"] = False
            elif change == "destruction":
                value["fixtureDestruction"]["outcome"] = "failed"
            elif change == "full-claim":
                value["fullAcceptance"] = True
            else:
                value["outcome"] = "failed"
            with self.subTest(change=change):
                self.assertTrue(qualify.pr_errors(value, "amd64", self.root, self.identity))
        for value in ([], {}, {"assertions": None}, {"assertions": [{"name": [], "outcome": "passed"}]}):
            self.assertTrue(qualify.pr_errors(value, "amd64", self.root, self.identity))
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
                patch.object(run_check, "native_architecture", return_value="amd64"), \
                patch.object(run_check, "execute", side_effect=[1] + [0] * (len(run_check.CHECKS) - 1)) as execute:
            self.assertEqual(1, run_check.main())
        self.assertEqual(list(run_check.CHECKS), [call.args[0] for call in execute.call_args_list])

    def test_existing_execution_receipt_is_preserved_without_rerunning_command(self):
        path = self.root / "previous.json"
        path.write_text('{"outcome":"failed","original":true}')
        with patch.object(run_check.subprocess, "run") as command, \
                self.assertRaisesRegex(ValueError, "refusing to overwrite"):
            run_check.execute("maven", path)
        command.assert_not_called()
        self.assertEqual('{"outcome":"failed","original":true}', path.read_text())

    def test_existing_suite_receipt_is_preserved_before_any_new_live_command(self):
        output = self.root / "acceptance-export"
        output.mkdir()
        path = output / "candidate-amd64-core.json"
        path.write_text('{"outcome":"failed","original":true}')
        with patch.object(qualify, "ROOT", self.root), \
                patch.object(qualify, "working_identity", return_value=self.identity), \
                patch.object(qualify.platform, "system", return_value="Linux"), \
                patch.object(qualify.platform, "machine", return_value="x86_64"), \
                patch.object(qualify.subprocess, "run") as command, \
                self.assertRaisesRegex(ValueError, "refusing to overwrite"):
            qualify.run("complete", "amd64", output)
        command.assert_not_called()
        self.assertEqual('{"outcome":"failed","original":true}', path.read_text())

    def test_receipt_created_concurrently_is_not_overwritten(self):
        output = self.root / "concurrent.json"
        def command(*args, **kwargs):
            output.write_text('{"outcome":"failed","original":true}')
            return type("Result", (), {"returncode": 0})()
        with patch.object(run_check, "working_identity", return_value=self.identity), \
                patch.object(run_check.subprocess, "run", side_effect=command), \
                self.assertRaises(FileExistsError):
            run_check.execute("maven", output)
        self.assertEqual('{"outcome":"failed","original":true}', output.read_text())

    def test_malformed_native_result_retains_failed_public_receipt(self):
        output = self.root / "acceptance-export"
        def command(arguments, **kwargs):
            work = Path(arguments[arguments.index("--work") + 1])
            work.mkdir(parents=True)
            (work / "result.json").write_text("[]")
        with patch.object(qualify, "ROOT", self.root), \
                patch.object(qualify, "COMPLETE", ("core",)), \
                patch.object(qualify, "working_identity", return_value=self.identity), \
                patch.object(qualify.platform, "system", return_value="Linux"), \
                patch.object(qualify.platform, "machine", return_value="x86_64"), \
                patch.object(qualify.subprocess, "run", side_effect=command), \
                self.assertRaisesRegex(ValueError, "suite result"):
            qualify.run("complete", "amd64", output)
        result = json.loads((output / "candidate-amd64-core.json").read_text())
        self.assertEqual("failed", result["outcome"])
        self.assertEqual(self.identity["checkout"], result["checkout"])
        self.assertEqual("failed", result["assertions"][-1]["outcome"])


if __name__ == "__main__":
    unittest.main()
