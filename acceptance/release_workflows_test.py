"""Fail-closed release routing and immutable-run contracts, without GitHub calls."""

import copy
import itertools
from pathlib import Path
import subprocess
import unittest

from scripts.release_run import validate
from acceptance.workflows_test import block

ROOT = Path(__file__).resolve().parents[1]


class RoutingTests(unittest.TestCase):
    def test_publisher_runs_only_from_a_version_tag_after_full_validation_and_candidate(self):
        text = (ROOT / ".github/workflows/release.yml").read_text()
        triggers = text.split("\non:\n", 1)[1].split("\npermissions:", 1)[0]
        self.assertIn('  push:\n    tags: ["v*"]', triggers)
        self.assertIn("workflow_dispatch:", triggers)
        self.assertNotIn("branches", triggers)
        self.assertNotIn("pull_request", triggers)
        self.assertIn('test "$REF" = "refs/tags/v$version"', text)
        gate = text.split("\n  validation:\n", 1)[1].split("\n\n  candidate:", 1)[0]
        self.assertIn("needs: metadata", gate)
        self.assertIn("if: ${{ github.event_name == 'push' }}", gate)
        self.assertIn("uses: ./.github/workflows/full-validation.yml", gate)
        self.assertIn("expected_sha: ${{ github.sha }}", gate)
        self.assertIn("security-events: write", gate)
        self.assertNotIn("ci-gate", text)
        self.assertNotIn("v2-ci", text)
        publish = text.split("\n  publish:\n", 1)[1]
        self.assertIn("needs: [metadata, validation, candidate]", publish)
        self.assertIn("github.event_name == 'push'", publish)
        for prerequisite in ("metadata", "validation", "candidate"):
            self.assertIn("needs." + prerequisite + ".result == 'success'", publish)
        self.assertIn("environment: maven-central", publish)
        for unsafe in ("continue-on-error", "always() &&", "secrets: inherit", "git tag", "push --tags", "--confirm"):
            self.assertNotIn(unsafe, text)
        operations = ("preflight", "sign-maven", "images", "central", "release")
        positions = [publish.index("publish_release.py " + operation) for operation in operations]
        self.assertEqual(sorted(positions), positions)
        self.assertLess(publish.index("publish_release.py preflight"), publish.index("secrets."))
        self.assertNotIn("packages: write", text.split("\n  publish:\n", 1)[0])
        self.assertNotIn("contents: write", text.split("\n  publish:\n", 1)[0])
        self.assertNotIn("secrets.", text.split("\n  publish:\n", 1)[0])

    def test_every_release_runs_the_python_real_node_e2e_on_both_architectures(self):
        from acceptance.qualification import fixture_names, load_matrix
        self.assertIn("python", fixture_names(load_matrix(), "native"))
        full = (ROOT / ".github/workflows/full-validation.yml").read_text()
        e2e = full.split("\n  e2e:\n", 1)[1].split("\n\n", 1)[0]
        self.assertIn("uses: ./.github/workflows/e2e.yml", e2e)
        self.assertIn("scenario: pr", e2e)
        self.assertIn("architecture: [amd64, arm64]", e2e)
        self.assertNotIn("if:", e2e)
        self.assertIn('test "$E2E" = success', full)
        workflow = (ROOT / ".github/workflows/e2e.yml").read_text()
        self.assertIn("harness.py --scenario pr", workflow)
        guard = workflow.split("            push)", 1)[1].split(";;", 1)[0]
        self.assertIn('test "$SCENARIO" = pr', guard)
        self.assertIn("refs/tags/v*", guard)
        self.assertFalse((ROOT / ".github/workflows/promotion-qualification.yml").exists())

    def test_dispatch_is_a_build_only_dry_run_of_the_same_candidate_builder(self):
        text = (ROOT / ".github/workflows/release.yml").read_text()
        candidate = text.split("\n  candidate:\n", 1)[1].split("\n\n", 1)[0]
        self.assertIn("needs: metadata", candidate)
        self.assertNotIn("if:", candidate)
        self.assertIn("uses: ./.github/workflows/release-build.yml", candidate)
        self.assertNotIn("@main", text)
        self.assertNotIn("ref: main", text)
        builder = (ROOT / ".github/workflows/release-build.yml").read_text()
        self.assertIn("workflow_dispatch:*|push:refs/tags/v*) ;;", builder)
        for name in ("release-build.yml", "release-transition.yml"):
            self.assertIn("  workflow_call:", (ROOT / ".github/workflows" / name).read_text())

    def test_exact_success_logic_excludes_skips_missing_and_cancellation(self):
        for states in itertools.product(("success", "failure", "skipped", "cancelled", ""), repeat=3):
            program = '\n'.join('test "$' + key + '" = success' for key in ("QUALIFICATION", "ORDINARY", "PREPARE"))
            result = subprocess.run(["bash", "-eu", "-c", program], capture_output=True,
                                    env=dict(zip(("QUALIFICATION", "ORDINARY", "PREPARE"), states)))
            self.assertEqual(all(value == "success" for value in states), result.returncode == 0, states)

    def test_no_live_or_publication_work_in_candidate_builder(self):
        text = (ROOT / ".github/workflows/release-build.yml").read_text()
        for forbidden in ("packages: write", "contents: write", "publish_release.py",
                          "release_transition.py", "qualify.py --run", "workflow run"):
            self.assertNotIn(forbidden, text)
        for target in ("linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"):
            self.assertIn("target: " + target, text)
        self.assertIn('smoke-cli --target "$TARGET"', text)

    def test_transition_needs_explicit_private_scope_authorization(self):
        text = (ROOT / ".github/workflows/release-transition.yml").read_text()
        self.assertIn('test "$AUTHORIZATION" = authorize-disposable-drain-reinstall', text)
        self.assertNotIn("push:", text)
        self.assertNotIn("pull_request", text)
        self.assertIn("--authorize-disposable-transition", text)
        self.assertIn("--workflow release-build.yml", text)
        self.assertIn("--source-digest", text)
        program = block(text, "Require explicit disposable-scope authorization")
        good = {"AUTHORIZATION": "authorize-disposable-drain-reinstall", "EVENT": "workflow_dispatch",
                "REPOSITORY": "microsoft/brewlet", "ARCHITECTURE": "amd64"}
        for key, values in (("AUTHORIZATION", (good["AUTHORIZATION"], "", "yes")),
                            ("EVENT", ("workflow_dispatch", "push")),
                            ("REPOSITORY", ("microsoft/brewlet", "fork/brewlet")),
                            ("ARCHITECTURE", ("amd64", "arm64", "", "emulated"))):
            for value in values:
                result = subprocess.run(["bash", "-eu", "-c", program], capture_output=True,
                                        env={**good, key: value})
                expected = value in ("amd64", "arm64") if key == "ARCHITECTURE" else value == good[key]
                self.assertEqual(expected, result.returncode == 0, (key, value))


class ImmutableRunTests(unittest.TestCase):
    def test_wrong_source_fork_workflow_event_and_non_success_are_rejected(self):
        good = {"repository": {"full_name": "microsoft/brewlet"},
                "head_repository": {"full_name": "microsoft/brewlet"},
                "event": "workflow_dispatch", "path": ".github/workflows/release-build.yml",
                "head_sha": "a" * 40, "status": "completed", "conclusion": "success"}
        validate(good, "release-build.yml", "a" * 40)
        validate({**good, "path": ".github/workflows/release.yml"}, "release-build.yml", "a" * 40)
        for key, value in (("repository", {"full_name": "fork/brewlet"}),
                           ("head_repository", {"full_name": "fork/brewlet"}),
                           ("event", "pull_request"), ("path", ".github/workflows/untrusted.yml"),
                           ("head_sha", "b" * 40), ("status", "in_progress"),
                           ("conclusion", "failure"), ("conclusion", "skipped"),
                           ("conclusion", "cancelled"), ("conclusion", None)):
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                validate({**good, key: value}, "release-build.yml", "a" * 40)


if __name__ == "__main__":
    unittest.main()
