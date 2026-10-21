"""Offline routing checks execute the actual small shell guards, never live jobs."""

from pathlib import Path
import re
import subprocess
import tempfile
import unittest

from acceptance.native_contracts import SUITES, validate_output

ROOT = Path(__file__).resolve().parents[1]


def block(text, marker):
    tail = text.split(marker, 1)[1].split("        run: |\n", 1)[1]
    lines = re.match(r"(?:          [^\n]*\n)+", tail).group().splitlines()
    return "\n".join(line[10:] for line in lines) + "\n"


class WorkflowTests(unittest.TestCase):
    def test_routine_and_qualification_share_fail_closed_native_contract_runner(self):
        text = (ROOT / ".github/workflows/v2-ci.yml").read_text()
        self.assertIn("run: python3 acceptance/native_contracts.py", text)
        self.assertEqual({"artifact", "shim", "node", "provisioner", "worker"}, set(SUITES))
        validate_output("--- PASS: required test\nPASS\n")
        for output in ("--- SKIP: required test\n--- PASS: unrelated\nPASS\n", "", "PASS\n"):
            with self.subTest(output=output), self.assertRaises(ValueError):
                validate_output(output)

    def test_routine_validation_is_main_compatible_without_promotion_qualification(self):
        text = (ROOT / ".github/workflows/v2-ci.yml").read_text()
        self.assertIn("branches: [main, brewlet-2.0]", text)
        self.assertIn("  pull_request:", text)
        self.assertNotIn("paths:", text)
        self.assertNotIn("pull_request_target", text)
        self.assertNotIn("release_gate", text)
        self.assertNotIn("candidate-acceptance", text)
        self.assertNotIn("make acceptance", text)
        self.assertNotIn("promotion-qualification", text)
        aggregate = text.split("  validation:", 1)[1].split("\n  producer-integration:", 1)[0]
        self.assertIn("if: always()", aggregate)
        self.assertIn("needs: [contracts, producer-integration, linux-overlay-contract]", aggregate)
        program = block(aggregate, "Require every prerequisite")
        for value in ("success", "failure", "skipped", "cancelled", ""):
            for dependency in ("CONTRACTS", "PRODUCER", "OVERLAY"):
                environment = dict.fromkeys(("CONTRACTS", "PRODUCER", "OVERLAY"), "success")
                environment[dependency] = value
                result = subprocess.run(["bash", "-eu", "-c", program], env=environment, capture_output=True)
                self.assertEqual(value == "success", result.returncode == 0, (dependency, value))

    def test_promotion_qualification_is_manual_reusable_and_always_strict(self):
        text = (ROOT / ".github/workflows/promotion-qualification.yml").read_text()
        triggers = text.split("\non:\n", 1)[1].split("\npermissions:", 1)[0]
        self.assertEqual(["  workflow_dispatch:", "  workflow_call:"],
                         [line for line in triggers.splitlines() if line.strip()])
        self.assertIn("name: Brewlet promotion qualification", text)
        self.assertIn("contents: read", text)
        self.assertIn("ref: ${{ github.sha }}", text)
        self.assertIn("fetch-depth: 0", text)
        self.assertIn("persist-credentials: false", text)
        self.assertIn("run: make acceptance", text)
        for bypass in ("if:", "continue-on-error", "||", "inputs.", "secrets.",
                       "--structure-only", "qualify.py --run", "e2e.yml", "workflow run"):
            self.assertNotIn(bypass, text)
        self.assertNotIn("event_name", text, "reusable calls retain the caller's original event")

    def test_live_work_is_manual_native_and_same_repository_branch_only(self):
        text = (ROOT / ".github/workflows/e2e.yml").read_text()
        self.assertNotIn("  pull_request:", text)
        self.assertNotIn("  push:", text)
        self.assertNotIn("pull_request_target", text)
        self.assertNotIn("secrets.", text)
        self.assertIn("contents: read", text)
        self.assertIn("options: [amd64, arm64]", text)
        self.assertIn("'ubuntu-24.04-arm' || 'ubuntu-24.04'", text)
        self.assertIn("ref: ${{ github.sha }}", text)
        self.assertIn("persist-credentials: false", text)
        program = block(text, "Refuse unsupported execution contexts")
        for event, repository, ref, success in (
                ("workflow_dispatch", "microsoft/brewlet", "refs/heads/main", True),
                ("workflow_dispatch", "microsoft/brewlet", "refs/heads/brewlet-2.0", True),
                ("workflow_dispatch", "microsoft/brewlet", "refs/heads/candidate", True),
                ("pull_request", "microsoft/brewlet", "refs/pull/1/merge", False),
                ("workflow_dispatch", "fork/brewlet", "refs/heads/candidate", False),
                ("workflow_dispatch", "microsoft/brewlet", "refs/tags/unreviewed", False)):
            result = subprocess.run(["bash", "-eu", "-c", program],
                                    env={"EVENT": event, "REPOSITORY": repository, "REF": ref,
                                         "QUALIFICATION_MODE": "native",
                                         "SOURCE": "a" * 40, "EXPECTED_SOURCE": "a" * 40},
                                    capture_output=True)
            self.assertEqual(success, result.returncode == 0, (event, repository, ref))

    def test_registered_e2e_modes_do_not_route_qualification_through_publication(self):
        text = (ROOT / ".github/workflows/e2e.yml").read_text()
        self.assertIn("options: [native, non-live, verify-only]", text)
        self.assertIn("QUALIFICATION_MODE: ${{ inputs.mode || 'native' }}", text)
        self.assertIn("python3 acceptance/run_check.py all", text)
        self.assertIn("if: env.QUALIFICATION_MODE == 'native'\n", text)
        strict = text.split("      - name: Require strict evidence acceptance", 1)[1].split(
            "      - name:", 1)[0]
        self.assertIn("if: env.QUALIFICATION_MODE == 'verify-only'", strict)
        self.assertIn("run: make acceptance", strict)
        self.assertNotIn("continue-on-error", text)
        self.assertNotIn("publish", text.lower())
        program = block(text, "Refuse unsupported execution contexts")
        for mode in ("native", "non-live", "verify-only", "publish", ""):
            result = subprocess.run(["bash", "-eu", "-c", program], capture_output=True,
                env={"EVENT": "workflow_dispatch", "REPOSITORY": "microsoft/brewlet",
                     "REF": "refs/heads/candidate", "QUALIFICATION_MODE": mode,
                     "SOURCE": "a" * 40, "EXPECTED_SOURCE": "a" * 40})
            self.assertEqual(mode in ("native", "non-live", "verify-only"), result.returncode == 0)

    def test_dispatch_ref_movement_is_rejected_before_checkout_or_execution(self):
        text = (ROOT / ".github/workflows/e2e.yml").read_text()
        self.assertIn("expected_sha:", text)
        program = block(text, "Refuse unsupported execution contexts")
        for expected in ("a" * 40, "b" * 40, "", "main", "A" * 40, "a" * 39):
            result = subprocess.run(["bash", "-eu", "-c", program], capture_output=True,
                env={"EVENT": "workflow_dispatch", "REPOSITORY": "microsoft/brewlet",
                     "REF": "refs/heads/candidate", "QUALIFICATION_MODE": "native",
                     "SOURCE": "a" * 40, "EXPECTED_SOURCE": expected})
            self.assertEqual(expected == "a" * 40, result.returncode == 0, expected)
        self.assertLess(text.index('test "$SOURCE" = "$EXPECTED_SOURCE"'),
                        text.index("- uses: actions/checkout"))


if __name__ == "__main__":
    unittest.main()
