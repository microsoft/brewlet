# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import io
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from contextlib import redirect_stdout
from unittest.mock import patch

from ci_plan import JOBS, changed_paths, check_results, main, plan


class ImpactTests(unittest.TestCase):
    def test_docs_only(self):
        selection = plan(["docs/installation.md", "core/README.md"])
        self.assertEqual([j for j in JOBS if selection[j]], ["docs"])

    def test_core_selects_consumers_but_not_docs_or_integrity(self):
        selection = plan(["core/internal/runtime/launch.go"])
        for job in ("core", "kubernetes", "admission", "containers", "notices", "host", "live"):
            self.assertTrue(selection[job], job)
        self.assertFalse(selection["docs"])
        self.assertFalse(selection["image_integrity"])
        self.assertEqual(selection["host_matrix"], [2, 3])
        self.assertTrue(selection["live"])

    def test_shared_registry_fixture_selects_both_languages(self):
        selection = plan(["core/internal/registry/testdata/conformance.json"])
        self.assertTrue(selection["core"])
        self.assertTrue(selection["maven"])
        self.assertFalse(selection["maven_release"])
        self.assertTrue(selection["live"])

    def test_maven_selects_real_goals_and_deployment_smoke(self):
        selection = plan(["maven-plugin/pom.xml"])
        self.assertTrue(selection["maven"])
        self.assertTrue(selection["maven_release"])
        self.assertEqual(selection["host_matrix"], [2])
        self.assertTrue(selection["live"])
        self.assertFalse(selection["core"])

    def test_provisioner_integrity_and_smoke(self):
        selection = plan(["provisioner/checksums/amd64.sha256"])
        self.assertTrue(selection["containers"])
        self.assertTrue(selection["image_integrity"])
        self.assertTrue(selection["live"])
        self.assertFalse(plan(["provisioner/entrypoint.sh"])["image_integrity"])

    def test_high_risk_and_dependency_paths_keep_pr_smoke_not_exhaustive_suites(self):
        for path in (
            "core/internal/runtime/cds_train.go", "core/shim/stage_gc.go",
            "kubernetes/internal/controller/nodeprofile_controller.go",
            "core/cmd/brewlet-metrics-exporter/main.go", "core/pkg/attest/verify.go",
            "core/go.mod", "kubernetes/go.sum",
        ):
            with self.subTest(path=path):
                selection = plan([path])
                self.assertTrue(selection["live"])
                self.assertNotIn("tiers", selection)
                self.assertNotIn("tier_matrix", selection)
                self.assertNotIn("live_matrix", selection)

    def test_api_raw_and_chart_schemas_select_maven_and_deployment(self):
        for path in ("kubernetes/api/v1alpha1/javaapplication_types.go",
                     "kubernetes/deploy/javaapplication-crd.yaml",
                     "kubernetes/charts/brewlet/crds/javaapplication-crd.yaml"):
            with self.subTest(path=path):
                selection = plan([path])
                for job in ("maven", "live", "kubernetes", "helm"):
                    self.assertTrue(selection[job], job)

    def test_ratify_retains_component_tests_without_unrelated_smoke(self):
        selection = plan(["admission/ratify-verifier/main.go"])
        self.assertTrue(selection["admission"])
        self.assertFalse(selection["live"])

    def test_unknown_empty_and_shared_inputs_fail_open_to_full_coverage(self):
        for paths in ([], ["new-component/config.json"], [".github/workflows/ci.yml"],
                      ["scripts/ci_plan.py"], ["Makefile"], ["specs/SPECIFICATION.md"],
                      ["integration-tests/fixtures/demo-app/build.sh"]):
            with self.subTest(paths=paths):
                selection = plan(paths)
                self.assertTrue(all(selection[j] for j in JOBS))
                self.assertEqual(selection["host_matrix"], [2, 3])
                self.assertNotIn("tiers", selection)
                self.assertNotIn("live_matrix", selection)

    def test_non_pr_runs_ignore_path_filtering(self):
        for event in ("push", "schedule", "workflow_dispatch"):
            with self.subTest(event=event), tempfile.TemporaryDirectory() as directory:
                output = Path(directory) / "output"
                with patch.dict(os.environ, {"GITHUB_EVENT_NAME": event,
                                             "GITHUB_OUTPUT": str(output)}), \
                        patch("sys.argv", ["ci_plan.py", "plan"]), redirect_stdout(io.StringIO()):
                    main()
                for job in JOBS:
                    self.assertIn(f"{job}=true\n", output.read_text())
                self.assertNotIn("tiers=", output.read_text())
                self.assertNotIn("live_matrix=", output.read_text())

    def test_git_diff_uses_merge_base_and_both_sides_of_renames(self):
        result = subprocess.CompletedProcess([], 0, b"core/old.go\0docs/new.md\0")
        with patch("ci_plan.subprocess.run", return_value=result) as execute:
            self.assertEqual(changed_paths("a" * 40, "b" * 40), ["core/old.go", "docs/new.md"])
        self.assertEqual(execute.call_args.args[0],
                         ["git", "diff", "--no-renames", "--name-only", "-z",
                          "a" * 40 + "..." + "b" * 40])
        with self.assertRaises(ValueError):
            changed_paths("--unsafe", "b" * 40)

    def test_failed_diff_selects_full_coverage(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "output"
            with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "pull_request",
                                         "PR_BASE_SHA": "a" * 40, "PR_HEAD_SHA": "b" * 40,
                                         "GITHUB_OUTPUT": str(output)}), \
                    patch("ci_plan.changed_paths", side_effect=subprocess.CalledProcessError(1, "git")), \
                    patch("sys.argv", ["ci_plan.py", "plan"]), redirect_stdout(io.StringIO()):
                main()
            self.assertIn("live=true", output.read_text())

    def test_real_git_rename_selects_both_owners(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            execute = subprocess.run

            def git(*args):
                return execute(["git", *args], cwd=root, check=True,
                               capture_output=True, text=True).stdout.strip()

            def commit():
                git("-c", "user.name=CI fixture", "-c", "user.email=ci@example.invalid",
                    "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
                    "commit", "-qm", "CI fixture\n\nCo-authored-by: Copilot App "
                    "<223556219+Copilot@users.noreply.github.com>")
                return git("rev-parse", "HEAD")

            git("init", "-q")
            (root / "core").mkdir()
            (root / "docs").mkdir()
            (root / "core/old.go").write_text("fixture\n")
            git("add", ".")
            base = commit()
            git("mv", "core/old.go", "docs/new.md")
            head = commit()
            with patch("ci_plan.subprocess.run",
                       side_effect=lambda *args, **kwargs: execute(*args, cwd=root, **kwargs)):
                paths = changed_paths(base, head)
            self.assertEqual(paths, ["core/old.go", "docs/new.md"])
            selected = plan(paths)
            self.assertTrue(selected["core"])
            self.assertTrue(selected["docs"])


class GateTests(unittest.TestCase):
    def needs(self):
        selected = plan(["docs/installation.md"])
        return {
            "changes": {"result": "success", "outputs": {j: json.dumps(selected[j]) for j in JOBS}},
            "safeguards": {"result": "success"},
            **{j: {"result": "success" if selected[j] else "skipped"} for j in JOBS},
        }

    def test_intentional_skips_pass(self):
        check_results(self.needs())

    def test_failure_cancellation_and_unexpected_skip_fail(self):
        for job in ("changes", "safeguards", "docs"):
            for result in ("failure", "cancelled", "skipped"):
                with self.subTest(job=job, result=result), self.assertRaises(ValueError):
                    needs = self.needs()
                    needs[job]["result"] = result
                    check_results(needs)

    def test_unselected_failures_and_missing_outputs_fail(self):
        for result in ("failure", "cancelled", "success"):
            needs = self.needs()
            needs["core"]["result"] = result
            with self.assertRaises(ValueError):
                check_results(needs)
        needs = self.needs()
        del needs["changes"]["outputs"]["core"]
        with self.assertRaises(ValueError):
            check_results(needs)

    def test_missing_dependency_fails(self):
        needs = self.needs()
        del needs["core"]
        with self.assertRaises(ValueError):
            check_results(needs)


if __name__ == "__main__":
    unittest.main()
