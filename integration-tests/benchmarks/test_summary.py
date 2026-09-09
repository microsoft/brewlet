# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import copy
import json
from pathlib import Path
import unittest

from summarize import analyze, distribution, image_objects


class SummaryTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.data = json.loads((Path(__file__).parent / "results/2026-09-09-linux-arm64.json").read_text())

    def test_measured_results_are_consistent(self):
        result = analyze(self.data)
        self.assertEqual(result["warmups"], 4)
        self.assertEqual(result["startup_seconds"]["brewlet"]["spring_startup_seconds"]["n"], 12)
        self.assertAlmostEqual(result["startup_seconds"]["conventional"]["spring_startup_seconds"]["median"], 6.606)
        self.assertAlmostEqual(result["startup_seconds"]["brewlet"]["spring_startup_seconds"]["median"], 6.613)
        self.assertEqual(result["transfer_payload"]["resource_revision"]["brewlet"]["bytes"], 445430)
        self.assertTrue(result["cleanup_verified"])

    def test_quantiles_include_all_samples(self):
        self.assertEqual(distribution([1, 2, 3, 100]), {
            "n": 4, "median": 2.5, "q1": 1.75, "q3": 27.25, "min": 1, "max": 100,
        })

    def test_missing_or_nonfinite_values_are_not_silently_filtered(self):
        for value in (None, float("nan"), float("inf")):
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, "finite"):
                distribution([1, value])

    def test_unbalanced_pairs_are_rejected(self):
        data = copy.deepcopy(self.data)
        data["runtime"]["trials"].pop()
        with self.assertRaisesRegex(ValueError, "Unbalanced"):
            analyze(data)

    def test_failed_trials_are_not_silently_dropped(self):
        data = copy.deepcopy(self.data)
        data["runtime"]["trials"][0]["status"] = "failed"
        with self.assertRaisesRegex(ValueError, "unsuccessful"):
            analyze(data)

    def test_resource_mismatch_is_rejected(self):
        data = copy.deepcopy(self.data)
        data["runtime"]["trials"][0]["memory"]["containers"][0]["cgroup_values"]["cpu.max"] = "200000 100000"
        with self.assertRaisesRegex(ValueError, "CPU limit"):
            analyze(data)

    def test_missing_pss_does_not_become_zero(self):
        data = copy.deepcopy(self.data)
        data["runtime"]["trials"][0]["memory"]["shims"][0]["smaps_available"] = False
        with self.assertRaisesRegex(ValueError, "PSS is unavailable"):
            analyze(data)

    def test_changed_storage_summary_is_detected(self):
        data = copy.deepcopy(self.data)
        data["storage"]["scenarios"]["resource_revision"]["brewlet"]["bytes"] -= 1
        with self.assertRaisesRegex(ValueError, "Byte/count mismatch"):
            analyze(data)

    def test_identical_digest_is_only_counted_once(self):
        images = self.data["storage"]["images"]
        self.assertEqual(image_objects(images, "jdk"), image_objects(images, "jdk", "jdk"))

    def test_unverified_cleanup_does_not_report_success(self):
        data = copy.deepcopy(self.data)
        data["cleanup"]["profile_finalization_succeeded"] = None
        with self.assertRaisesRegex(ValueError, "cleanup was not verified"):
            analyze(data)


if __name__ == "__main__":
    unittest.main()
