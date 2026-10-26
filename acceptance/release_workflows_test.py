"""Fail-closed release routing contracts, without GitHub calls."""

import itertools
from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[1]


class RoutingTests(unittest.TestCase):
    def test_publisher_runs_only_from_a_version_tag_after_release_gate_tests_and_candidate(self):
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
        self.assertIn("uses: ./.github/workflows/release-gate-tests.yml", gate)
        self.assertIn("expected_sha: ${{ github.sha }}", gate)
        self.assertIn("security-events: write", gate)
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
        import importlib.util
        import sys
        sys.path.insert(0, str(ROOT / "integration-tests/live-v2"))
        spec = importlib.util.spec_from_file_location("harness", ROOT / "integration-tests/live-v2/harness.py")
        harness = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(harness)
        self.assertIn("python", harness.LANGUAGES)
        full = (ROOT / ".github/workflows/release-gate-tests.yml").read_text()
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
        self.assertIn("  workflow_call:", builder)

    def test_exact_success_logic_excludes_skips_missing_and_cancellation(self):
        for states in itertools.product(("success", "failure", "skipped", "cancelled", ""), repeat=3):
            program = '\n'.join('test "$' + key + '" = success' for key in ("QUALIFICATION", "ORDINARY", "PREPARE"))
            result = subprocess.run(["bash", "-eu", "-c", program], capture_output=True,
                                    env=dict(zip(("QUALIFICATION", "ORDINARY", "PREPARE"), states)))
            self.assertEqual(all(value == "success" for value in states), result.returncode == 0, states)

    def test_no_live_or_publication_work_in_candidate_builder(self):
        text = (ROOT / ".github/workflows/release-build.yml").read_text()
        for forbidden in ("packages: write", "contents: write", "publish_release.py",
                          "qualify.py --run", "workflow run"):
            self.assertNotIn(forbidden, text)
        for target in ("linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"):
            self.assertIn("target: " + target, text)
        self.assertIn('smoke-cli --target "$TARGET"', text)


if __name__ == "__main__":
    unittest.main()
