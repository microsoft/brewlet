"""Offline ownership and finalizer guards; not real API admission evidence."""

import copy
import json
from types import SimpleNamespace
import unittest
from unittest.mock import Mock

import admission_contract as a
from harness import Failure, OWNER

RUN = "0123456789ab"


class AdmissionContractTests(unittest.TestCase):
    def profile(self):
        return {"metadata": {"name": "owned-profile", "uid": "profile-uid", "resourceVersion": "9",
                             "labels": {OWNER: RUN},
                             "finalizers": ["node.brewlet.sh/cleanup", a.HOLD]}}

    def test_release_only_fixture_hold_with_uid_version_and_value_tests(self):
        h = SimpleNamespace(run_id=RUN, get=Mock(return_value=self.profile()), kubectl=Mock())
        a.release_observation_hold(h, "owned-profile", "profile-uid")
        patch = json.loads(h.kubectl.call_args.args[-1])
        self.assertEqual(patch[:2], [
            {"op": "test", "path": "/metadata/uid", "value": "profile-uid"},
            {"op": "test", "path": "/metadata/resourceVersion", "value": "9"}])
        self.assertEqual(patch[2:], [
            {"op": "test", "path": "/metadata/finalizers/1", "value": a.HOLD},
            {"op": "remove", "path": "/metadata/finalizers/1"}])

    def test_foreign_or_replaced_profile_cannot_release_any_finalizer(self):
        for change in ("uid", "owner"):
            obj = self.profile()
            if change == "uid":
                obj["metadata"]["uid"] = "replacement"
            else:
                obj["metadata"]["labels"][OWNER] = "foreign"
            h = SimpleNamespace(run_id=RUN, get=Mock(return_value=obj), kubectl=Mock())
            with self.subTest(change=change), self.assertRaises(Failure):
                a.release_observation_hold(h, "owned-profile", "profile-uid")
            h.kubectl.assert_not_called()

    def test_production_finalizer_is_never_removed_when_fixture_hold_is_absent(self):
        obj = self.profile()
        obj["metadata"]["finalizers"] = ["node.brewlet.sh/cleanup"]
        h = SimpleNamespace(run_id=RUN, get=Mock(return_value=obj), kubectl=Mock())
        a.release_observation_hold(h, "owned-profile", "profile-uid")
        h.kubectl.assert_not_called()

    def test_webhook_selection_requires_exact_private_helm_release_and_service(self):
        obj = {"metadata": {"name": "private-hook", "uid": "webhook-uid",
                           "annotations": {"meta.helm.sh/release-name": "owned",
                                           "meta.helm.sh/release-namespace": "owned-system"},
                           "labels": {"app.kubernetes.io/managed-by": "Helm"}},
               "webhooks": [{"clientConfig": {"service": {"namespace": "owned-system",
                                                         "path": "/validate-nodeprofiles"}}}]}
        h = SimpleNamespace(cluster="owned", control_namespace="owned-system",
                            get=Mock(return_value={"items": [obj]}))
        self.assertEqual(obj, a.profile_webhook(h))
        for change in ("release", "namespace", "path", "duplicate"):
            altered = copy.deepcopy(obj)
            if change == "release":
                altered["metadata"]["annotations"]["meta.helm.sh/release-name"] = "foreign"
            elif change == "namespace":
                altered["webhooks"][0]["clientConfig"]["service"]["namespace"] = "foreign"
            elif change == "path":
                altered["webhooks"][0]["clientConfig"]["service"]["path"] = "/validate-runtimeimages"
            h.get.return_value = {"items": [altered] * (2 if change == "duplicate" else 1)}
            with self.subTest(change=change), self.assertRaises(Failure):
                a.profile_webhook(h)

    def test_foreign_existing_profile_prevents_mutation(self):
        h = SimpleNamespace(get=Mock(return_value={"items": [self.profile()]}), apply=Mock())
        with self.assertRaisesRegex(Failure, "empty profile inventory"):
            a.exercise(h)
        h.apply.assert_not_called()


if __name__ == "__main__":
    unittest.main()
