"""Offline cold/native deadline evidence guards, not elapsed real-node proof."""

import copy
import unittest

from cold_install import native_deadline_evidence, verify_default_restart_arguments
from harness import Failure


class NativeDeadlineTests(unittest.TestCase):
    def test_rendered_default_flags_are_exact_and_not_prefix_matches(self):
        verify_default_restart_arguments(
            "    - --restart-max-concurrent=5\n"
            "    - --restart-workload-progress-deadline=10m\n")
        for text in (
            "- --restart-max-concurrent=50\n- --restart-workload-progress-deadline=10m",
            "- --restart-max-concurrent=5\n- --restart-workload-progress-deadline=100m",
            "- --restart-max-concurrent=1\n- --restart-workload-progress-deadline=60s",
        ):
            with self.assertRaises(Failure):
                verify_default_restart_arguments(text)

    def fixture(self):
        first = {"phase": "Failed", "reason": "RestartFailed", "startedAt": "2026-10-16T18:00:00Z"}
        second = {"startedAt": "2026-10-16T18:00:12Z"}
        workload = {"metadata": {"generation": 2}, "status": {"observedGeneration": 2,
            "conditions": [{"type": "Progressing", "status": "False",
                            "reason": "ProgressDeadlineExceeded"}]}}
        return first, second, workload

    def test_requires_real_current_native_failure_and_early_slot_handoff(self):
        proof = native_deadline_evidence(*self.fixture())
        self.assertEqual(proof["slotHandoffSeconds"], 12)
        self.assertEqual(proof["nativeProgressDeadlineSeconds"], 10)
        self.assertEqual(proof["workloadDeadlineSeconds"], 60)

    def test_unstarted_and_unfailed_workloads_are_not_evidence(self):
        first, second, obj = self.fixture()
        self.assertFalse(native_deadline_evidence(None, second, obj))
        first["phase"] = "Rolling"
        self.assertFalse(native_deadline_evidence(first, second, obj))
        first["phase"] = "Failed"
        self.assertFalse(native_deadline_evidence(first, {}, obj))

    def test_fake_condition_stale_status_late_handoff_and_completed_failure_refused(self):
        for change in ("condition", "stale", "late", "early", "complete"):
            with self.subTest(change=change):
                first, second, obj = copy.deepcopy(self.fixture())
                if change == "condition":
                    obj["status"]["conditions"][0]["reason"] = "FixtureSaysFailed"
                elif change == "stale":
                    obj["status"]["observedGeneration"] = 1
                elif change == "late":
                    second["startedAt"] = "2026-10-16T18:01:01Z"
                elif change == "early":
                    second["startedAt"] = "2026-10-16T18:00:01Z"
                else:
                    first["completedAt"] = "2026-10-16T18:00:12Z"
                with self.assertRaises(Failure):
                    native_deadline_evidence(first, second, obj)


if __name__ == "__main__":
    unittest.main()
