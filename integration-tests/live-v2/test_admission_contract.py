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
    def hold_harness(self, obj):
        return SimpleNamespace(run_id=RUN, get=Mock(return_value=obj),
            kubectl=Mock(return_value=SimpleNamespace(returncode=0, stderr="")),
            wait=lambda _, predicate: predicate())

    def profile(self):
        return {"metadata": {"name": "owned-profile", "uid": "profile-uid", "resourceVersion": "9",
                             "labels": {OWNER: RUN},
                             "finalizers": ["node.brewlet.sh/cleanup", a.HOLD]}}

    def test_release_only_fixture_hold_with_uid_version_and_value_tests(self):
        h = self.hold_harness(self.profile())
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
            h = self.hold_harness(obj)
            with self.subTest(change=change), self.assertRaises(Failure):
                a.release_observation_hold(h, "owned-profile", "profile-uid")
            h.kubectl.assert_not_called()

    def test_production_finalizer_is_never_removed_when_fixture_hold_is_absent(self):
        obj = self.profile()
        obj["metadata"]["finalizers"] = ["node.brewlet.sh/cleanup"]
        h = self.hold_harness(obj)
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

    def test_fixture_hold_conflict_rereads_uid_and_version_before_retry(self):
        original = self.profile()
        refreshed = copy.deepcopy(original)
        refreshed["metadata"]["resourceVersion"] = "10"
        h = self.hold_harness(original)
        h.get.side_effect = [original, refreshed]
        h.kubectl.side_effect = [SimpleNamespace(returncode=1, stderr="Error (Conflict)"),
                                SimpleNamespace(returncode=0, stderr="")]
        def wait(_, predicate):
            self.assertFalse(predicate())
            self.assertTrue(predicate())
        h.wait = wait
        a.release_observation_hold(h, "owned-profile", "profile-uid")
        versions = [json.loads(call.args[-1])[1]["value"] for call in h.kubectl.call_args_list]
        self.assertEqual(["9", "10"], versions)


class EmptyFixtureDeletionTests(unittest.TestCase):
    def setUp(self):
        self.spec = {"nodePool": {"key": "fixture.test/pool", "names": ["absent"]},
                     "runtimeImages": [{"id": "python", "install": "OnDemand"}],
                     "registry": {}, "rollout": {}}
        self.obj = {"kind": "NodeProfile", "metadata": {"name": "fixture", "uid": "fixture-uid",
            "resourceVersion": "1", "generation": 2, "labels": {OWNER: RUN},
            "finalizers": ["node.brewlet.sh/cleanup"]}, "spec": copy.deepcopy(self.spec)}

    def ready(self, version="2"):
        obj = copy.deepcopy(self.obj)
        obj["metadata"]["resourceVersion"] = version
        obj["status"] = {"ownershipInitialized": True, "provisioningGeneration": 2,
                         "provisioningSpec": copy.deepcopy(self.spec), "targets": []}
        return obj

    def harness(self, snapshots):
        iterator = iter(snapshots)
        events = []
        def get(kind, *args, **kwargs):
            if kind == "nodes":
                return {"items": []}
            events.append("read-profile")
            return copy.deepcopy(next(iterator))
        def replace(obj, spec):
            events.append("restore-spec")
            self.assertEqual("fixture-uid", obj["metadata"]["uid"])
            self.assertEqual("1", obj["metadata"]["resourceVersion"])
            restored = copy.deepcopy(self.obj)
            restored["spec"] = copy.deepcopy(spec)
            return restored
        def kubectl(*args, **kwargs):
            events.append("request-delete")
            self.assertEqual(("delete", "--raw", "/apis/" + a.API + "/nodeprofiles/fixture", "-f", "-"), args)
            request = json.loads(kwargs["input"])
            self.assertEqual("fixture-uid", request["preconditions"]["uid"])
            self.assertEqual("Foreground", request["propagationPolicy"])
            return SimpleNamespace(returncode=0, stderr="")
        def wait(description, predicate):
            for _ in range(8):
                result = predicate()
                if result:
                    return result
            self.fail("bounded unit fixture did not finish: " + description)
        h = SimpleNamespace(run_id=RUN, owned={("nodeprofile", "", "fixture"): "fixture-uid"},
            get=get, replace_inventory_spec=Mock(side_effect=replace),
            kubectl=Mock(side_effect=kubectl), wait=wait)
        return h, events

    def test_restore_wait_current_initialized_ledger_then_uid_version_delete_and_normal_absence(self):
        uninitialized = copy.deepcopy(self.obj)
        stale = self.ready("3")
        stale["status"]["provisioningGeneration"] = 1
        stale_spec = self.ready("4")
        stale_spec["status"]["provisioningSpec"]["runtimeImages"] = []
        ready = self.ready("5")
        deleting = copy.deepcopy(ready)
        deleting["metadata"]["deletionTimestamp"] = "2026-10-22T00:00:00Z"
        h, events = self.harness([self.obj, uninitialized, stale, stale_spec, ready, deleting, None])
        a.request_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        self.assertEqual(1, h.kubectl.call_count)
        self.assertEqual({"uid": "fixture-uid", "resourceVersion": "5"},
                         json.loads(h.kubectl.call_args.kwargs["input"])["preconditions"])
        self.assertEqual(["read-profile", "restore-spec", "read-profile", "read-profile",
                          "read-profile", "read-profile", "request-delete"], events)
        h.wait("normal fixture deletion", lambda: h.get("nodeprofile", "fixture", absent=True) is None)
        self.assertEqual("read-profile", events[-1])
        self.assertTrue(all(call.args[0] == "delete" for call in h.kubectl.call_args_list))

    def test_delete_conflict_requires_fresh_fully_revalidated_snapshot(self):
        h, _ = self.harness([self.obj, self.ready("5"), self.ready("6")])
        h.kubectl.side_effect = [SimpleNamespace(returncode=1, stderr="Error (Conflict)"),
                                SimpleNamespace(returncode=0, stderr="")]
        a.request_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        self.assertEqual(["5", "6"], [
            json.loads(call.kwargs["input"])["preconditions"]["resourceVersion"]
            for call in h.kubectl.call_args_list])

    def test_conflicting_invalid_spec_is_corrected_to_disjoint_valid_inputs(self):
        initial = copy.deepcopy(self.obj)
        initial["spec"]["nodePool"]["names"] = ["overlapping"]
        initial["spec"]["registry"]["mirrors"] = {"registry.test": "forbidden.invalid"}
        before = copy.deepcopy(initial)
        h, _ = self.harness([initial, self.ready()])
        a.request_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        self.assertEqual(self.spec, h.replace_inventory_spec.call_args.args[1])
        self.assertEqual(before, initial)
        self.assertEqual(1, h.kubectl.call_count)

    def test_restore_conflict_rereads_resource_version_before_any_deletion(self):
        refreshed = copy.deepcopy(self.obj)
        refreshed["metadata"]["resourceVersion"] = "2"
        h, _ = self.harness([self.obj, refreshed, self.ready("5")])
        h.replace_inventory_spec.side_effect = [Failure("Error (Conflict)"), copy.deepcopy(self.obj)]
        a.request_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        self.assertEqual(["1", "2"], [call.args[0]["metadata"]["resourceVersion"]
                                    for call in h.replace_inventory_spec.call_args_list])
        self.assertEqual(1, h.kubectl.call_count)

    def test_restore_retry_cannot_adopt_replacement_and_does_not_retry_other_errors(self):
        replacement = copy.deepcopy(self.obj)
        replacement["metadata"]["uid"] = "replacement"
        h, _ = self.harness([self.obj, replacement])
        h.replace_inventory_spec.side_effect = Failure("Error (Conflict)")
        with self.assertRaises(Failure):
            a.request_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        self.assertEqual(1, h.replace_inventory_spec.call_count)
        h.kubectl.assert_not_called()
        h, _ = self.harness([self.obj])
        h.replace_inventory_spec.side_effect = Failure("transport unavailable")
        with self.assertRaisesRegex(Failure, "transport unavailable"):
            a.request_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        self.assertEqual(1, h.replace_inventory_spec.call_count)
        h.kubectl.assert_not_called()

    def test_empty_value_structs_are_explicit_spec_inputs_not_status_normalization(self):
        proposed = copy.deepcopy(self.spec)
        del proposed["registry"], proposed["rollout"]
        h, _ = self.harness([self.obj, self.ready()])
        a.request_empty_profile_deletion(h, "fixture", "fixture-uid", proposed)
        self.assertEqual(self.spec, h.replace_inventory_spec.call_args.args[1])
        self.assertNotIn("registry", proposed)
        self.assertNotIn("rollout", proposed)

    def test_already_deleting_uninitialized_fixture_cannot_be_repaired_by_forcing_authority(self):
        obj = copy.deepcopy(self.obj)
        obj["metadata"]["deletionTimestamp"] = "2026-10-22T00:00:00Z"
        h, _ = self.harness([obj])
        with self.assertRaisesRegex(Failure, "no current initialized ownership"):
            a.request_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        h.replace_inventory_spec.assert_not_called()
        h.kubectl.assert_not_called()

    def test_replaced_uid_or_changed_generation_is_rejected_before_deletion(self):
        for change in ("uid", "generation", "spec"):
            obj = self.ready()
            if change == "uid":
                obj["metadata"]["uid"] = "replacement"
            elif change == "generation":
                obj["metadata"]["generation"] = 3
            else:
                obj["spec"]["runtimeImages"] = []
            h, _ = self.harness([self.obj, obj])
            with self.subTest(change=change), self.assertRaises(Failure):
                a.request_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
            h.kubectl.assert_not_called()

    def test_target_or_retirement_obligations_prevent_empty_fixture_deletion(self):
        for change in ("targets", "retirement", "detachedRetirements"):
            obj = self.ready()
            obj["status"][change] = [{"uid": "node"}]
            h, _ = self.harness([self.obj, obj])
            with self.subTest(change=change), self.assertRaisesRegex(Failure, "obligations"):
                a.request_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
            h.kubectl.assert_not_called()

    def test_matching_pool_or_unrecorded_node_claim_prevents_restore_and_delete(self):
        for metadata in ({"labels": {"fixture.test/pool": "absent"}},
                         {"annotations": {"brewlet.sh/owner-name": "fixture"}},
                         {"labels": {"brewlet.sh/owner-uid": "fixture-uid"}}):
            h, _ = self.harness([self.obj])
            get = h.get
            h.get = lambda kind, *args, **kwargs: ({"items": [{"metadata": metadata}]} if kind == "nodes"
                                                   else get(kind, *args, **kwargs))
            with self.subTest(metadata=metadata), self.assertRaises(Failure):
                a.request_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
            h.replace_inventory_spec.assert_not_called()
            h.kubectl.assert_not_called()


if __name__ == "__main__":
    unittest.main()
