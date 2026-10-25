"""Execute the actual workflow guards offline; never execute live jobs."""

import json
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

from acceptance.native_contracts import SUITES, validate_output
from acceptance.run_check import CHECKS

ROOT = Path(__file__).resolve().parents[1]


def block(text, marker):
    tail = text.split(marker, 1)[1].split("        run: |\n", 1)[1]
    return "\n".join(line[10:] for line in re.match(r"(?:          [^\n]*\n)+", tail).group().splitlines()) + "\n"


class WorkflowTests(unittest.TestCase):
    def test_routine_and_qualification_share_fail_closed_native_contract_runner(self):
        text = (ROOT / ".github/workflows/v2-ci.yml").read_text()
        website = (ROOT / ".github/workflows/site-pages.yml").read_text()
        self.assertIn("python3 acceptance/run_check.py native-contracts", text)
        self.assertEqual(["python3", "acceptance/native_contracts.py"], CHECKS["native-contracts"]["command"])
        for name in CHECKS:
            self.assertEqual(1, (text + website).count("python3 acceptance/run_check.py " + name), name)
        self.assertNotIn("run_check.py all", text)
        self.assertNotIn("uses: ./.github/workflows/site-pages.yml", text)
        self.assertNotIn("  documentation:", text)
        self.assertNotIn("documentation_required", text)
        self.assertNotIn("PR_NUMBER", text)
        self.assertIn("  workflow_call:", website)
        self.assertNotIn("  pull_request:", website)
        self.assertNotIn("  push:", website)
        self.assertEqual({"artifact", "shim", "node", "provisioner", "worker"}, set(SUITES))
        validate_output("--- PASS: required test\nPASS\n")
        for output in ("--- SKIP: required test\n--- PASS: unrelated\nPASS\n", "", "PASS\n"):
            with self.assertRaises(ValueError):
                validate_output(output)

    def test_automatic_validation_requires_actual_e2e_not_promotion_evidence(self):
        text = (ROOT / ".github/workflows/v2-ci.yml").read_text()
        self.assertIn("branches: [main]", text)
        self.assertIn("  pull_request:", text)
        self.assertNotIn("paths:", text)
        self.assertNotIn("pull_request_target", text)
        aggregate = text.split("  validation:", 1)[1].split("\n  e2e:", 1)[0]
        self.assertIn("needs: [context, contracts, producer-integration, linux-overlay-contract, archive-history, e2e]", aggregate)
        program = block(aggregate, "Require every prerequisite")
        dependencies = ("CONTEXT", "CONTRACTS", "PRODUCER", "OVERLAY", "HISTORY", "E2E")
        for required in ("true", "false", ""):
            for dependency in dependencies:
                for value in ("success", "failure", "skipped", "cancelled", ""):
                    environment = dict.fromkeys(dependencies, "success")
                    environment["E2E_REQUIRED"] = required
                    if required != "true":
                        environment["E2E"] = "skipped"
                    environment[dependency] = value
                    passing = value == "success" if dependency != "E2E" or required == "true" else value == "skipped"
                    passing = passing and required in ("true", "false")
                    result = subprocess.run(["bash", "-eu", "-c", program], env=environment, capture_output=True)
                    self.assertEqual(passing, result.returncode == 0, (required, dependency, value))
        e2e = text.split("\n  e2e:", 1)[1].split("\n  producer-integration:", 1)[0]
        self.assertIn("    needs: context\n", e2e)
        self.assertIn("needs.context.outputs.e2e == 'true'", e2e)
        self.assertIn("uses: ./.github/workflows/e2e.yml", e2e)
        self.assertIn("scenario: pr", e2e)
        self.assertIn("architecture: amd64", e2e)
        # Only pull requests narrow to Python; main pushes gating releases keep all four fixtures.
        self.assertIn("languages: ${{ github.event_name == 'pull_request' && 'python' || 'all' }}", e2e)
        self.assertNotIn("continue-on-error", text)
        self.assertNotIn("make acceptance", text)

    def test_e2e_scope_skips_only_exact_documentation_and_workflow_pull_requests(self):
        text = (ROOT / ".github/workflows/v2-ci.yml").read_text()
        listing = text.split("List exact pull request changes for E2E scope", 1)[1].split("      - name:", 1)[0]
        self.assertIn("if: github.event_name == 'pull_request'", listing)
        self.assertIn('test "$head" = "$SOURCE"', listing)
        self.assertIn("previous_filename", listing)
        program = block(text, "Classify E2E scope")
        for event, listed, files, required in (
                ("pull_request", "true", ["docs/index.md", "README.md", "site/index.html"], "false"),
                ("pull_request", "true", [".github/dependabot.yml", ".github/workflows/site-pages.yml"], "false"),
                ("pull_request", "true", ["release-notes/v2.md", "kubernetes/README.md"], "false"),
                ("pull_request", "true", ["docs/index.md", "pkg/runtime/x.go"], "true"),
                ("pull_request", "true", [".github/workflows/e2e.yml"], "true"),
                ("pull_request", "true", [".github/workflows/v2-ci.yml"], "true"),
                ("pull_request", "true", ["integration-tests/live-v2/harness.py"], "true"),
                ("pull_request", "true", ["docs/../go.mod.md", "go.sum"], "true"),
                ("pull_request", "true", [], "true"),
                ("pull_request", "false", ["docs/index.md"], "true"),
                ("pull_request", "", ["docs/index.md"], "true"),
                ("push", "true", ["docs/index.md"], "true"),
                ("workflow_dispatch", "true", ["docs/index.md"], "true"),
                ("workflow_call", "", [], "true")):
            with tempfile.TemporaryDirectory() as temporary:
                listing = Path(temporary) / "files"
                listing.write_text("".join(name + "\n" for name in files))
                output = Path(temporary) / "output"
                environment = {"EVENT": event, "LISTED": listed, "FILES": str(listing),
                    "GITHUB_OUTPUT": str(output)}
                subprocess.run(["bash", "-eu", "-c", program], env=environment, check=True, capture_output=True)
                self.assertEqual("e2e=" + required + "\n", output.read_text(), (event, listed, files))

    def test_pull_requests_run_native_checks_on_amd64_only_and_main_keeps_both(self):
        text = (ROOT / ".github/workflows/v2-ci.yml").read_text()
        producer = text.split("\n  producer-integration:", 1)[1].split("\n  linux-overlay-contract:", 1)[0]
        overlay = text.split("\n  linux-overlay-contract:", 1)[1].split("\n  archive-history:", 1)[0]
        for job in (producer, overlay):
            expression = job.split("${{ fromJSON(", 1)[1].split(") }}", 1)[0]
            self.assertTrue(expression.startswith("github.event_name == 'pull_request'"), expression)
            pr, other = (json.loads(part.strip().strip("&|").strip().strip("'"))
                         for part in expression.split("github.event_name == 'pull_request'", 1)[1].split("||"))
            arch = lambda entries: [e if isinstance(e, str) else e["architecture"] for e in entries]
            self.assertEqual(arch(pr), ["amd64"])
            self.assertEqual(arch(other), ["amd64", "arm64"])

    def test_evidence_only_jobs_cannot_emit_a_green_required_pr_check(self):
        text = (ROOT / ".github/workflows/v2-ci.yml").read_text()
        aggregate = text.split("  validation:", 1)[1].split("\n  e2e:", 1)[0]
        self.assertIn("needs.context.outputs.purpose == 'validation' || needs.context.result != 'success'", aggregate)
        self.assertIn("== 'archive-history' && 'Archive-only status' || 'Brewlet validation'", aggregate)
        self.assertIn("export-history --if-needed", text)

    def test_ci_source_and_purpose_guards_are_executed_before_tests(self):
        text = (ROOT / ".github/workflows/v2-ci.yml").read_text()
        program = block(text, "Validate immutable source")
        for event, purpose, expected, success in (
                ("pull_request", "validation", "", True),
                ("pull_request", "validation", "b" * 40, False),
                ("push", "validation", "", True),
                ("pull_request", "qualification-non-live", "a" * 40, False),
                ("workflow_dispatch", "validation", "a" * 40, True),
                ("workflow_dispatch", "qualification-non-live", "a" * 40, False),
                ("workflow_dispatch", "archive-history", "a" * 40, True),
                ("workflow_dispatch", "validation", "b" * 40, False),
                ("workflow_dispatch", "publish", "a" * 40, False)):
            with tempfile.TemporaryDirectory() as temporary:
                environment = {"EVENT": event, "PURPOSE": purpose, "EXPECTED": expected,
                    "SOURCE": "a" * 40, "REPOSITORY": "microsoft/brewlet", "REF": "refs/heads/candidate",
                    "GITHUB_OUTPUT": str(Path(temporary) / "output")}
                result = subprocess.run(["bash", "-eu", "-c", program], env=environment, capture_output=True)
                self.assertEqual(success, result.returncode == 0, (event, purpose, expected))

    def test_promotion_is_manual_reusable_and_fetches_verified_artifacts_before_strict_gate(self):
        text = (ROOT / ".github/workflows/promotion-qualification.yml").read_text()
        triggers = text.split("\non:\n", 1)[1].split("\npermissions:", 1)[0]
        self.assertEqual(["  workflow_dispatch:", "  workflow_call:"],
                         [line for line in triggers.splitlines() if line.startswith("  ") and not line.startswith("    ")])
        self.assertIn("actions: read", text)
        self.assertIn("fetch-depth: 0", text)
        self.assertIn("persist-credentials: false", text)
        self.assertIn("python3 acceptance/evidence_index.py fetch", text)
        self.assertIn("make acceptance", text)
        self.assertLess(text.index("evidence_index.py fetch"), text.index("make acceptance"))
        self.assertLess(text.index('test "$SOURCE" = "$EXPECTED_SOURCE"'), text.index("- uses: actions/checkout"))
        for bypass in ("continue-on-error", "||", "--structure-only", "qualify.py --run", "workflow run"):
            self.assertNotIn(bypass, text)

    def test_e2e_is_reusable_real_execution_only_and_pr_guard_rejects_forks(self):
        text = (ROOT / ".github/workflows/e2e.yml").read_text()
        self.assertIn("  workflow_call:", text)
        self.assertNotIn("  pull_request:", text)
        self.assertNotIn("pull_request_target", text)
        self.assertNotIn("secrets.", text)
        self.assertNotIn("QUALIFICATION_MODE", text)
        self.assertNotIn("run_check.py", text)
        self.assertNotIn("make acceptance", text)
        self.assertNotIn("unittest discover", text)
        self.assertIn('harness.py --scenario pr "${selection[@]}"', text)
        self.assertIn("selection=(--pr-language python)", text)
        self.assertIn('--verify-pr "acceptance-export/pr-e2e-${ARCHITECTURE}.json"', text)
        self.assertIn('--languages "$LANGUAGES"', text)
        export = text.split("      - name: Export only public PR outcomes", 1)[1].split(
            "      - name:", 1)[0]
        self.assertIn("steps.guard.outcome == 'success'", export)
        self.assertNotIn("${{ inputs.", export.split("        run: |", 1)[1])
        self.assertIn("persist-credentials: false", text)
        for name in ("Restore PR E2E host Go caches", "Restore PR E2E image Go build cache",
                     "Seed BuildKit Go build cache mount", "Extract BuildKit Go build cache mount"):
            step = text.split("      - name: " + name, 1)[1].split("      - ", 1)[0]
            self.assertIn("inputs.scenario == 'pr'", step, name)
        self.assertIn("cache: ${{ inputs.scenario != 'pr' }}", text)
        self.assertLess(text.index("Seed BuildKit"), text.index("harness.py --scenario pr"))
        self.assertLess(text.index("harness.py --scenario pr"), text.index("Extract BuildKit"))
        program = block(text, "Refuse unsupported execution contexts")
        for event, ref, head, scenario, languages, success in (
                ("pull_request", "refs/pull/1/merge", "microsoft/brewlet", "pr", "all", True),
                ("pull_request", "refs/pull/1/merge", "microsoft/brewlet", "pr", "python", True),
                ("pull_request", "refs/pull/1/merge", "microsoft/brewlet", "pr", "java", False),
                ("pull_request", "refs/pull/1/merge", "fork/brewlet", "pr", "python", False),
                ("pull_request", "refs/pull/1/merge", "microsoft/brewlet", "complete", "all", False),
                ("push", "refs/heads/main", "microsoft/brewlet", "pr", "all", True),
                ("push", "refs/heads/main", "microsoft/brewlet", "pr", "python", False),
                ("push", "refs/heads/candidate", "microsoft/brewlet", "pr", "all", False),
                ("push", "refs/heads/main", "microsoft/brewlet", "complete", "all", False),
                ("workflow_dispatch", "refs/heads/candidate", "microsoft/brewlet", "complete", "all", True),
                ("workflow_dispatch", "refs/heads/candidate", "microsoft/brewlet", "complete", "python", False),
                ("workflow_dispatch", "refs/heads/candidate", "microsoft/brewlet", "pr", "all", True),
                ("workflow_dispatch", "refs/heads/candidate", "microsoft/brewlet", "pr", "python", True),
                ("workflow_dispatch", "refs/heads/candidate", "microsoft/brewlet", "pr", "", False),
                ("workflow_dispatch", "refs/tags/untrusted", "microsoft/brewlet", "pr", "all", False)):
            environment = {"EVENT": event, "REF": ref, "HEAD_REPOSITORY": head, "SCENARIO": scenario,
                "REPOSITORY": "microsoft/brewlet", "SOURCE": "a" * 40, "EXPECTED_SOURCE": "a" * 40,
                "ARCHITECTURE": "amd64", "LANGUAGES": languages}
            result = subprocess.run(["bash", "-eu", "-c", program], env=environment, capture_output=True)
            self.assertEqual(success, result.returncode == 0, (event, ref, head, scenario, languages))
        self.assertLess(text.index('test "$SOURCE" = "$EXPECTED_SOURCE"'), text.index("- uses: actions/checkout"))

    def test_dispatch_moved_head_bad_architecture_and_unknown_scope_fail_closed(self):
        text = (ROOT / ".github/workflows/e2e.yml").read_text()
        program = block(text, "Refuse unsupported execution contexts")
        for field, values in {
                "EXPECTED_SOURCE": ("b" * 40, "", "main", "A" * 40, "a" * 39),
                "ARCHITECTURE": ("emulated", "", "AMD64"),
                "SCENARIO": ("publish", "verify-only", "non-live", "")}.items():
            for value in values:
                environment = {"EVENT": "workflow_dispatch", "REF": "refs/heads/candidate",
                    "HEAD_REPOSITORY": "microsoft/brewlet", "REPOSITORY": "microsoft/brewlet",
                    "SOURCE": "a" * 40, "EXPECTED_SOURCE": "a" * 40, "SCENARIO": "pr",
                    "ARCHITECTURE": "amd64", "LANGUAGES": "all"}
                environment[field] = value
                result = subprocess.run(["bash", "-eu", "-c", program], env=environment, capture_output=True)
                self.assertNotEqual(0, result.returncode, (field, value))


if __name__ == "__main__":
    unittest.main()
