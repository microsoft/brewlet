"""Offline ownership and finalizer guards; not real API admission evidence."""

import copy
import json
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import admission_contract as a
from harness import Failure, OWNER

RUN = "0123456789ab"


class AdmissionContractTests(unittest.TestCase):
    def hold_harness(self, obj):
        return SimpleNamespace(run_id=RUN, result={}, get=Mock(return_value=obj),
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
        del self.obj["spec"]["registry"]

    def ready(self, version="2"):
        obj = copy.deepcopy(self.obj)
        obj["spec"] = copy.deepcopy(self.spec)
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
        h = SimpleNamespace(run_id=RUN, result={}, owned={("nodeprofile", "", "fixture"): "fixture-uid"},
            get=get, replace_inventory_spec=Mock(side_effect=replace),
            kubectl=Mock(side_effect=kubectl), wait=wait)
        return h, events

    def test_restore_wait_current_initialized_ledger_then_uid_version_delete_and_normal_absence(self):
        uninitialized = self.ready()
        del uninitialized["status"]
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
        initial["spec"]["registry"] = {"mirrors": {"registry.test": "forbidden.invalid"}}
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
        h.replace_inventory_spec.side_effect = [Failure("Error (Conflict)"), self.ready()]
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
        obj["spec"] = copy.deepcopy(self.spec)
        obj["metadata"]["deletionTimestamp"] = "2026-10-22T00:00:00Z"
        h, _ = self.harness([obj])
        with self.assertRaisesRegex(Failure, "no current initialized ownership"):
            a.request_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        h.replace_inventory_spec.assert_not_called()
        h.kubectl.assert_not_called()

    def test_unchanged_valid_spec_is_not_patched_on_repeat_cleanup(self):
        h, _ = self.harness([self.ready(), self.ready("5")])
        a.request_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        h.replace_inventory_spec.assert_not_called()
        self.assertEqual(1, h.kubectl.call_count)

    def test_repeat_cleanup_after_authoritative_absence_never_patches_or_deletes(self):
        h, _ = self.harness([None] * 6)
        for _ in range(2):
            a.finish_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        h.replace_inventory_spec.assert_not_called()
        h.kubectl.assert_not_called()
        self.assertFalse(h.result["fixtureLifecycle"]["observations"]["empty-profile"]["absence"]["present"])

    def test_repeat_cleanup_of_initialized_deleting_fixture_then_absence(self):
        deleting = self.ready()
        deleting["metadata"]["deletionTimestamp"] = "2026-10-22T00:00:00Z"
        h, _ = self.harness([deleting, deleting, None, None, None, None])
        for _ in range(2):
            a.finish_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        h.replace_inventory_spec.assert_not_called()
        h.kubectl.assert_not_called()

    def test_normal_held_cleanup_then_repeat_waits_absence_and_removes_only_test_hold(self):
        ready = self.ready("5")
        deleting = self.ready("6")
        deleting["metadata"].update(deletionTimestamp="2026-10-22T00:00:00Z",
                                    finalizers=["node.brewlet.sh/cleanup", a.HOLD])
        finalizing = copy.deepcopy(deleting)
        finalizing["metadata"]["finalizers"] = ["node.brewlet.sh/cleanup"]
        h, _ = self.harness([ready, ready, deleting, finalizing, None, None, None, None])
        h.kubectl.side_effect = None
        h.kubectl.return_value = SimpleNamespace(returncode=0, stderr="")
        a.finish_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        a.finish_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        self.assertEqual(["delete", "patch"], [call.args[0] for call in h.kubectl.call_args_list])
        patch = json.loads(h.kubectl.call_args.args[-1])
        self.assertEqual([
            {"op": "test", "path": "/metadata/uid", "value": "fixture-uid"},
            {"op": "test", "path": "/metadata/resourceVersion", "value": "6"},
            {"op": "test", "path": "/metadata/finalizers/1", "value": a.HOLD},
            {"op": "remove", "path": "/metadata/finalizers/1"}], patch)
        h.replace_inventory_spec.assert_not_called()

    def test_absence_after_restoration_wait_can_finish_without_fake_ownership(self):
        h, _ = self.harness([self.obj, None, None, None])
        a.finish_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        h.kubectl.assert_not_called()

    def test_existing_deleting_stale_or_uninitialized_state_is_not_success(self):
        for field, value in (("ownershipInitialized", False), ("provisioningGeneration", 1),
                             ("provisioningSpec", {})):
            obj = self.ready()
            obj["metadata"]["deletionTimestamp"] = "2026-10-22T00:00:00Z"
            obj["status"][field] = value
            h, _ = self.harness([obj])
            with self.subTest(field=field), self.assertRaisesRegex(Failure, "no current initialized ownership"):
                a.finish_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
            h.replace_inventory_spec.assert_not_called()
            h.kubectl.assert_not_called()
            diagnostic = h.result["fixtureLifecycle"]["observations"]["empty-profile"]["deletion"]
            self.assertTrue(diagnostic["present"])
            self.assertTrue(diagnostic["deleting"])
            self.assertFalse(diagnostic[{"ownershipInitialized": "ownershipInitialized",
                                        "provisioningGeneration": "provisioningGenerationMatches",
                                        "provisioningSpec": "provisioningSpecMatches"}[field]])

    def test_new_stale_state_cannot_release_hold_after_guarded_deletion_request(self):
        stale = self.ready("6")
        stale["metadata"].update(deletionTimestamp="2026-10-22T00:00:00Z", finalizers=[a.HOLD])
        stale["status"]["provisioningGeneration"] = 1
        h, _ = self.harness([self.obj, self.ready("5"), stale])
        with self.assertRaisesRegex(Failure, "hold release requires current initialized ownership"):
            a.finish_empty_profile_deletion(h, "fixture", "fixture-uid", self.spec)
        self.assertEqual(1, h.kubectl.call_count)
        self.assertEqual("delete", h.kubectl.call_args.args[0])

    def test_uid_generation_and_obligation_diagnostics_contain_only_predicates(self):
        obj = self.ready()
        obj["metadata"].update(uid="PRIVATE-UID", generation=3, annotations={"claims": "SECRET"})
        obj["spec"]["registry"] = {"credentials": "SECRET"}
        obj["status"].update(provisioningGeneration=1, retirement={"private": "SECRET"},
                         targets=[{"private": "SECRET"}], detachedRetirements=[{"private": "SECRET"}])
        h, _ = self.harness([])
        a.observe_ownership(h, "admission-held", "deletion", obj, "fixture-uid", 2, self.spec)
        row = h.result["fixtureLifecycle"]["observations"]["admission-held"]["deletion"]
        self.assertFalse(row["uidMatches"])
        self.assertFalse(row["generationMatches"])
        self.assertFalse(row["specMatches"])
        self.assertFalse(row["provisioningGenerationMatches"])
        self.assertFalse(row["targetsEmpty"])
        self.assertTrue(row["retirementPresent"])
        self.assertTrue(row["detachedRetirementsPresent"])
        self.assertNotIn("SECRET", json.dumps(row))
        self.assertNotIn("PRIVATE-UID", json.dumps(row))

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


class FailureReportingTests(unittest.TestCase):
    def test_primary_predicate_snapshot_is_not_overwritten_by_later_cleanup(self):
        h = SimpleNamespace(result={"fixtureLifecycle": {"observations": {
            "admission-held": {"deletion": {"uidMatches": False}}}}})
        def cleanup():
            h.result["fixtureLifecycle"]["observations"]["admission-held"]["deletion"]["uidMatches"] = True
            raise Failure("secondary ownership error")
        with self.assertRaisesRegex(Failure, "original assertion"):
            with a.preserve_failures(h, (("held-cleanup", cleanup),)):
                raise Failure("original assertion")
        primary, secondary = h.result["fixtureLifecycle"]["errors"]
        self.assertFalse(primary["observations"]["admission-held"]["deletion"]["uidMatches"])
        self.assertTrue(secondary["observations"]["admission-held"]["deletion"]["uidMatches"])

    def test_primary_exception_survives_multiple_independent_cleanup_failures(self):
        h = SimpleNamespace(result={})
        original = Failure("original assertion")
        first = Mock(side_effect=Failure("cleanup ownership error"))
        second = Mock(side_effect=Failure("private webhook failure"))
        last = Mock()
        with self.assertRaises(Failure) as caught:
            with a.preserve_failures(h, (("conflict-cleanup", first), ("held-cleanup", second),
                                         ("webhook-restore", last))):
                raise original
        self.assertIs(original, caught.exception)
        for callback in (first, second, last):
            callback.assert_called_once_with()
        records = h.result["fixtureLifecycle"]["errors"]
        self.assertEqual(["primary", "secondary", "secondary"], [row["role"] for row in records])
        self.assertEqual(["original assertion", "cleanup ownership error", "private webhook failure"],
                         [row["privateMessage"] for row in records])
        self.assertEqual(2, len(original.__notes__))

    def test_cleanup_only_failures_raise_first_and_record_later_errors(self):
        h = SimpleNamespace(result={})
        original = Failure("first cleanup")
        with self.assertRaises(Failure) as caught:
            with a.preserve_failures(h, (("held-cleanup", Mock(side_effect=original)),
                                         ("webhook-restore", Mock(side_effect=RuntimeError("second cleanup"))))):
                pass
        self.assertIs(original, caught.exception)
        self.assertEqual(["primary", "secondary"],
                         [row["role"] for row in h.result["fixtureLifecycle"]["errors"]])

    def test_successful_cleanup_does_not_hide_primary_or_create_errors_on_success(self):
        for fails in (False, True):
            h = SimpleNamespace(result={})
            cleanup = Mock()
            def exercise():
                with a.preserve_failures(h, (("held-cleanup", cleanup),)):
                    if fails:
                        raise Failure("original failure")
            if fails:
                with self.assertRaisesRegex(Failure, "original failure"):
                    exercise()
            else:
                exercise()
                self.assertNotIn("fixtureLifecycle", h.result)
            cleanup.assert_called_once_with()

    def test_complete_exercise_keeps_assertion_and_both_cleanup_failures(self):
        obj = {"metadata": {"uid": "held-uid", "name": "fixture-admission"},
               "spec": {"nodePool": {"names": ["unused"]}, "runtimeImages": []}}
        def get(kind, *args, **kwargs):
            if kind == "nodeprofiles":
                return {"items": []}
            if kind == "runtimeimage":
                return {"metadata": {"uid": "runtime-uid"}}
            return copy.deepcopy(obj)
        selections = iter([
            SimpleNamespace(returncode=1, stderr="NoCompatibleRuntime"),
            SimpleNamespace(returncode=0, stderr="fixture-admission"),
            SimpleNamespace(returncode=0, stderr="unexpected authorization")])
        def kubectl(*args, **kwargs):
            return next(selections) if args[0] == "create" else SimpleNamespace(returncode=0)
        h = SimpleNamespace(profile="fixture", run_id=RUN, result={}, probe_language="python",
            profile_object=lambda: copy.deepcopy(obj), get=get,
            apply=Mock(side_effect=[copy.deepcopy(obj), {"metadata": {"uid": "conflict-uid", "name": "conflict"}}]),
            run=Mock(), kubeconfig="private", runtime_images={"python": {"before": "private"}},
            app_images={"python": "private"}, record=Mock(), kubectl=Mock(side_effect=kubectl),
            replace_inventory_spec=Mock(), pod=Mock(return_value={"kind": "Pod"}))
        hook = {"metadata": {"name": "hook", "uid": "hook-uid"}, "webhooks": []}
        original_get = h.get
        h.get = lambda kind, *args, **kwargs: (hook if kind == "validatingwebhookconfiguration"
                                              else original_get(kind, *args, **kwargs))
        with patch.object(a, "profile_webhook", return_value=hook), \
                patch.object(a, "finish_empty_profile_deletion", side_effect=[
                    Failure("conflict cleanup ownership error"), Failure("held cleanup error")]) as cleanup:
            with self.assertRaisesRegex(Failure, "ineligible profile authorized placement"):
                a.exercise(h)
        self.assertEqual(2, cleanup.call_count)
        self.assertEqual(["assertions", "conflict-cleanup", "held-cleanup"],
                         [row["stage"] for row in h.result["fixtureLifecycle"]["errors"]])
        self.assertEqual(2, sum(call.args[0] == "replace" for call in h.kubectl.call_args_list))

if __name__ == "__main__":
    unittest.main()
