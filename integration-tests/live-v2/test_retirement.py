"""Offline original-identity obligation guards, never physical cleanup evidence."""

import copy
import unittest

from harness import Failure
from retirement import missing_obligation, returning_registration


class ObligationTests(unittest.TestCase):
    def test_returning_registration_requires_real_new_uid_stable_identity_and_no_adoption(self):
        original = {"metadata": {"name": "old", "uid": "original"},
                    "spec": {"providerID": "kind://original-host"},
                    "status": {"nodeInfo": {"systemUUID": "physical-host"}}}
        current = copy.deepcopy(original)
        current["metadata"]["uid"] = "returned"
        profile = {"status": {"detachedRetirements": [
            {"phase": "Missing", "targets": [{"name": "old", "uid": "original"}]}]}}
        self.assertTrue(returning_registration(original, current, profile))
        for field in ("uid", "physical identity", "owner", "target"):
            with self.subTest(field=field):
                node, snapshot = copy.deepcopy(current), copy.deepcopy(profile)
                if field == "uid":
                    node["metadata"]["uid"] = "original"
                elif field == "physical identity":
                    node["spec"]["providerID"] = "different"
                    node["status"]["nodeInfo"]["systemUUID"] = "different"
                elif field == "owner":
                    node["metadata"]["labels"] = {"brewlet.sh/owner-uid": "fresh-claim"}
                else:
                    snapshot["status"]["targets"] = [{"uid": "returned"}]
                with self.assertRaises(Failure):
                    returning_registration(original, node, snapshot)
        for labels in ({"brewlet.sh/runtime": "ready"},
                       {"brewlet.sh/runtime.fixture-python": "ready"}):
            node = copy.deepcopy(current)
            node["metadata"]["labels"] = labels
            self.assertFalse(returning_registration(original, node, profile))
        self.assertFalse(returning_registration(original, current, {}))

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
