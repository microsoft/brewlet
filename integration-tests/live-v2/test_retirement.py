"""Offline original-identity obligation guards, never physical cleanup evidence."""

import copy
import unittest

from harness import Failure
from retirement import missing_obligation


class ObligationTests(unittest.TestCase):
    def test_only_exact_original_uid_missing_obligation_is_accepted(self):
        row = {"phase": "Missing", "targets": [{"name": "old", "uid": "original"}]}
        profile = {"status": {"detachedRetirements": [row]}}
        self.assertEqual(missing_obligation(profile, "old", "original"), row)
        self.assertFalse(missing_obligation(profile, "old", "replacement"))
        self.assertFalse(missing_obligation(profile, "different", "original"))
        self.assertFalse(missing_obligation({}, "old", "original"))
        duplicate = copy.deepcopy(profile)
        duplicate["status"]["detachedRetirements"].append(row)
        with self.assertRaisesRegex(Failure, "duplicate"):
            missing_obligation(duplicate, "old", "original")


if __name__ == "__main__":
    unittest.main()
