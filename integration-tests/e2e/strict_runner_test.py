# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


E2E = Path(__file__).resolve().parent


class StrictRunnerTests(unittest.TestCase):
    def results(self, body, strict="true"):
        return subprocess.run(
            ["bash", "-c", 'source "$1"; ' + body + '; [[ "$E2E_FAIL" -eq 0 ]]',
             "strict-test", str(E2E / "lib.sh")],
            env=dict(os.environ, E2E_REQUIRE_ALL=strict),
            capture_output=True, text=True)

    def test_passing_tier(self):
        result = self.results('pass example; e2e_require_results 2 0 0')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_skip_fails_even_with_other_successes(self):
        result = self.results('pass example; skip missing prerequisite; e2e_require_results 2 0 0')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("required assertions were skipped", result.stdout)

    def test_empty_tier_cannot_borrow_another_tiers_success(self):
        result = self.results('pass earlier; e2e_require_results 3 1 0')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("no passing assertions", result.stdout)

    def test_local_optional_skips_remain_supported(self):
        result = self.results('skip missing prerequisite; e2e_require_results 2 0 0', "false")
        self.assertEqual(result.returncode, 0)

    def test_failed_assertions_remain_failures(self):
        result = self.results('pass example; fail broken; e2e_require_results 2 0 0')
        self.assertNotEqual(result.returncode, 0)

    def test_invalid_tiers_and_strict_mode_fail_before_execution(self):
        for args, strict in ((["--tier", "14"], "false"), (["--tier"], "true"),
                             (["--tier", "unknown"], "true"), (["--tier", "2"], "typo")):
            with self.subTest(args=args, strict=strict), tempfile.TemporaryDirectory() as work:
                result = subprocess.run(
                    ["bash", str(E2E / "run.sh"), *args],
                    env=dict(os.environ, E2E_WORK=work, E2E_REQUIRE_ALL=strict),
                    capture_output=True, text=True)
                self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                self.assertIn("ERROR:", result.stderr)
                self.assertNotIn("environment", result.stdout)


if __name__ == "__main__":
    unittest.main()
