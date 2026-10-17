#!/usr/bin/env python3
"""Offline safeguards only: no Docker, kubectl, Helm, network, or cluster."""

import copy
from datetime import datetime, timezone
import importlib
import json
from pathlib import Path
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import control_edges as ce


RUN = "012345abcdef"
UID = "owned-workload-uid"
EPOCH = "current-inventory-uid"
RID = "control-" + RUN + "-a"
NS = "brewlet-v2-" + RUN + "-control"
NOW = datetime(2026, 10, 16, 18, 0, 0, tzinfo=timezone.utc).timestamp()


def meta(name, uid, namespace=None):
    value = {"name": name, "uid": uid, "resourceVersion": "37", "generation": 2,
             "labels": {ce.OWNER: RUN}}
    if namespace:
        value["namespace"] = namespace
    return value


def image():
    return {"apiVersion": ce.API, "kind": "RuntimeImage", "metadata": meta(RID, EPOCH),
            "status": {"generation": 2, "rotationCompletedAt": "2026-10-16T17:59:00Z",
                       "restarts": {"workloads": []}}}


def workload(kind="Deployment"):
    value = {"apiVersion": "apps/v1", "kind": kind, "metadata": meta("work", UID, NS),
             "spec": {"replicas": 1, "selector": {"matchLabels": {ce.CASE: "work"}},
                      "template": {"metadata": {"annotations": {
                          ce.BARRIER: json.dumps(ce.barrier_for(image()))}}}},
             "status": {"observedGeneration": 2}}
    if kind == "Deployment":
        value["status"].update(replicas=1, updatedReplicas=1, readyReplicas=1, availableReplicas=1)
    elif kind == "StatefulSet":
        value["status"].update(updatedReplicas=1, readyReplicas=1,
                               currentRevision="current", updateRevision="current")
    else:
        value["status"].update(desiredNumberScheduled=1, updatedNumberScheduled=1,
                               numberAvailable=1, numberReady=1, numberUnavailable=0)
    return value


def pod(uid="new-pod", kind="ReplicaSet", owner="rs-owned", *, ready=True):
    value = {"kind": "Pod", "metadata": meta("pod", uid, NS),
             "spec": {"nodeName": "real-owned-node"},
             "status": {"phase": "Running",
                        "conditions": [{"type": "Ready", "status": "True" if ready else "False"}],
                        "containerStatuses": [{"containerID": "containerd://" + uid,
                                               "restartCount": 0, "state": {"running": {}}}]}}
    value["metadata"]["annotations"] = {ce.BARRIER: json.dumps(ce.barrier_for(image()))}
    value["metadata"]["ownerReferences"] = [{"kind": kind, "uid": owner, "controller": True}]
    return value


def row(phase="Complete", reason=None, **kwargs):
    value = {"kind": "Deployment", "namespace": NS, "name": "work", "uid": UID,
             "inventoryUID": EPOCH, "generation": 2, "phase": phase,
             "reason": reason or "Restart" + phase, "patched": True,
             "startedAt": "2026-10-16T17:59:50Z"}
    value.update(kwargs)
    return value


def fake_harness():
    return SimpleNamespace(
        cluster="brewlet-v2-" + RUN, run_id=RUN, namespace="base-workloads",
        control_namespace="brewlet-v2-" + RUN + "-system", node="real-owned-node",
        node_uid="real-host-uid", kubeconfig=Path("/private/kubeconfig"),
        context="kind-brewlet-v2-" + RUN, owned={}, run=Mock(), get=Mock(), kubectl=Mock(),
        save=Mock(), apply=Mock(), work=Path(__file__).parent / ("run-offline-" + RUN),
        runtime_images={"python": {"before": "registry/runtime@sha256:" + "a" * 64,
                                   "after": "registry/runtime@sha256:" + "b" * 64}},
        app_images={"python": "registry/app@sha256:" + "c" * 64})


class IsolationTests(unittest.TestCase):
    def test_import_has_no_external_side_effect(self):
        with patch("subprocess.Popen", side_effect=AssertionError("external process")), \
                patch("subprocess.run", side_effect=AssertionError("external process")), \
                patch("pathlib.Path.mkdir", side_effect=AssertionError("file mutation")):
            importlib.reload(ce)

    def test_dedicated_ids_and_namespace(self):
        h = fake_harness()
        control = ce.ControlEdges(h)
        self.assertEqual(control.namespace, NS)
        self.assertEqual(len(set(control.ids)), 2)
        self.assertFalse(set(control.ids).intersection(ce.IDS.values()))
        self.assertNotIn(control.namespace, (h.namespace, h.control_namespace))
        h.run.assert_not_called()
        h.kubectl.assert_not_called()

    def test_reject_namespace_collision(self):
        h = fake_harness()
        h.namespace = NS
        with self.assertRaises(ce.Failure):
            ce.ControlEdges(h)

    def test_apply_rejects_base_namespace(self):
        h = fake_harness()
        obj = workload()
        obj["metadata"]["namespace"] = h.namespace
        with self.assertRaises(ce.Failure):
            ce.ControlEdges(h).apply_workload(obj)
        h.apply.assert_not_called()

    def test_patch_uses_live_uid_and_resource_version(self):
        h = fake_harness()
        h.kubectl.return_value = subprocess.CompletedProcess([], 0, "patched", "")
        obj = workload()
        h.get.return_value = obj
        c = ce.ControlEdges(h)
        c.workloads["work"] = obj
        original = {"spec": {"paused": True}}
        c.patch_workload("work", original)
        args = h.kubectl.call_args.args
        body = json.loads(args[-1])
        self.assertEqual(body["metadata"], {"uid": UID, "resourceVersion": "37"})
        self.assertEqual(original, {"spec": {"paused": True}})

    def test_patch_retries_only_status_churn_with_fresh_resource_version(self):
        h = fake_harness()
        original, fresh = workload(), workload()
        fresh["metadata"]["resourceVersion"] = "38"
        fresh["status"]["observedGeneration"] = 3
        h.get.side_effect = [original, fresh]
        h.kubectl.side_effect = [
            subprocess.CompletedProcess([], 1, "", "Error from server (Conflict): modified"),
            subprocess.CompletedProcess([], 0, "patched", "")]
        control = ce.ControlEdges(h)
        control.workloads["work"] = original
        with patch.object(ce.time, "sleep"):
            control.patch_workload("work", {"spec": {"paused": True}})
        bodies = [json.loads(call.args[-1]) for call in h.kubectl.call_args_list]
        self.assertEqual([body["metadata"]["resourceVersion"] for body in bodies], ["37", "38"])
        self.assertTrue(all(body["metadata"]["uid"] == UID for body in bodies))
        self.assertTrue(all("status" not in body for body in bodies))

    def test_patch_conflict_cannot_adopt_recreated_or_changed_workload(self):
        for change in ("uid", "generation", "spec", "annotation"):
            with self.subTest(change=change):
                h = fake_harness()
                original, fresh = workload(), workload()
                fresh["metadata"]["resourceVersion"] = "38"
                if change in ("uid", "generation"):
                    fresh["metadata"][change] = "replacement" if change == "uid" else 9
                elif change == "spec":
                    fresh["spec"]["paused"] = True
                else:
                    fresh["metadata"].setdefault("annotations", {})[ce.OPT_IN] = "changed"
                h.get.side_effect = [original, fresh]
                h.kubectl.return_value = subprocess.CompletedProcess(
                    [], 1, "", "Error from server (Conflict): modified")
                control = ce.ControlEdges(h)
                control.workloads["work"] = original
                with patch.object(ce.time, "sleep"), self.assertRaises(ce.Failure):
                    control.patch_workload("work", {"metadata": {"annotations": {ce.OPT_IN: "false"}}})
                self.assertEqual(h.kubectl.call_count, 1)

    def test_patch_does_not_retry_other_failures_or_conflict_forever(self):
        for message, expected in (("Error from server (Forbidden): denied", 1),
                                  ("Error from server (Conflict): modified", 6)):
            with self.subTest(message=message):
                h = fake_harness()
                original = workload()
                h.get.return_value = original
                h.kubectl.return_value = subprocess.CompletedProcess([], 1, "", message)
                control = ce.ControlEdges(h)
                control.workloads["work"] = original
                with patch.object(ce.time, "sleep"), self.assertRaises(ce.Failure):
                    control.patch_workload("work", {"spec": {"paused": True}})
                self.assertEqual(h.kubectl.call_count, expected)

    def test_refuse_patch_recreated_workload(self):
        h = fake_harness()
        original, replacement = workload(), workload()
        replacement["metadata"]["uid"] = "replacement"
        h.get.return_value = replacement
        c = ce.ControlEdges(h)
        c.workloads["work"] = original
        with self.assertRaises(ce.Failure):
            c.patch_workload("work", {"spec": {"paused": True}})
        h.kubectl.assert_not_called()

    def test_refuse_status_mutation(self):
        h = fake_harness()
        obj = workload()
        h.get.return_value = obj
        c = ce.ControlEdges(h)
        c.workloads["work"] = obj
        with self.assertRaises(ce.Failure):
            c.patch_workload("work", {"status": {"phase": "Complete"}})
        h.kubectl.assert_not_called()

    def test_delete_requires_recorded_ownership(self):
        h = fake_harness()
        with self.assertRaises(ce.Failure):
            ce.ControlEdges(h).delete_object("Deployment", "work", NS)
        h.kubectl.assert_not_called()

    def test_delete_uid_and_rv_preconditions(self):
        h = fake_harness()
        h.owned[("deployment", NS, "work")] = UID
        h.get.return_value = workload()
        h.kubectl.return_value = SimpleNamespace(returncode=0, stderr="")
        h.wait = lambda description, predicate, timeout: predicate()
        ce.ControlEdges(h).delete_object("Deployment", "work", NS)
        args, kwargs = h.kubectl.call_args
        self.assertEqual(args[2], "/apis/apps/v1/namespaces/" + NS + "/deployments/work")
        options = json.loads(kwargs["input"])
        self.assertEqual(options["preconditions"], {"uid": UID, "resourceVersion": "37"})
        self.assertEqual(options["propagationPolicy"], "Foreground")

    def test_delete_rejects_foreign_namespace(self):
        h = fake_harness()
        obj = workload()
        obj["metadata"]["namespace"] = h.namespace
        h.get.return_value = obj
        h.owned[("deployment", h.namespace, "work")] = UID
        with self.assertRaises(ce.Failure):
            ce.ControlEdges(h).delete_object("Deployment", "work", h.namespace)
        h.kubectl.assert_not_called()

    def test_callback_rejects_missing_base_execution_without_running_commands(self):
        h = fake_harness()
        h.initial = {}
        with self.assertRaises(ce.Failure):
            ce.execute(h)
        h.run.assert_not_called()

    def test_inventory_patch_only_sends_owned_spec_and_cas_metadata(self):
        h = fake_harness()
        c = ce.ControlEdges(h)
        im = image()
        h.owned[("runtimeimage", "", RID)] = EPOCH
        im["metadata"]["finalizers"] = ["must-not-replay"]
        c.patch_inventory_spec(im, {"description": "test"})
        body = json.loads(h.kubectl.call_args.args[-1])
        self.assertEqual(body, [
            {"op": "test", "path": "/metadata/uid", "value": EPOCH},
            {"op": "test", "path": "/metadata/generation", "value": 2},
            {"op": "replace", "path": "/spec", "value": {"description": "test"}}])
        self.assertIn("--type=json", h.kubectl.call_args.args)

    def test_inventory_patch_refuses_base_runtime(self):
        h = fake_harness()
        c = ce.ControlEdges(h)
        im = image()
        im["metadata"]["name"] = ce.IDS["python"]
        h.owned[("runtimeimage", "", ce.IDS["python"])] = EPOCH
        with self.assertRaises(ce.Failure):
            c.patch_inventory_spec(im, {})
        h.kubectl.assert_not_called()


class ControllerReadinessTests(unittest.TestCase):
    def test_incomplete_rotation_retains_bounded_controller_status_without_raw_objects(self):
        control = object.__new__(ce.ControlEdges)
        original = {"metadata": {"uid": EPOCH, "generation": 3},
                    "status": {"generation": 2, "digest": "sha256:target",
                               "rotation": {"generation": 2, "participants": []},
                               "conditions": [{"type": "RotationComplete", "status": "False",
                                               "reason": "RotationProgressing"}],
                               "unrelatedPrivateField": "not-exported"}}
        control.image = lambda _: original
        control.h = SimpleNamespace(result={"controlAcceptance": {}},
                                    wait=lambda _description, predicate: predicate())
        self.assertFalse(control.complete_rotation(RID))
        diagnostic = control.h.result["controlAcceptance"]["rotationDiagnostics"][RID]
        self.assertEqual(diagnostic["inventoryUID"], EPOCH)
        self.assertEqual(diagnostic["objectGeneration"], 3)
        self.assertEqual(diagnostic["status"]["conditions"], original["status"]["conditions"])
        self.assertNotIn("unrelatedPrivateField", diagnostic["status"])
        original["status"]["conditions"][0]["reason"] = "changed-later"
        self.assertEqual(diagnostic["status"]["conditions"][0]["reason"], "RotationProgressing")

    def test_terminal_history_does_not_count_as_an_active_replica(self):
        h = fake_harness()
        h.wait = lambda description, predicate: predicate()
        control = ce.ControlEdges(h)
        obj = workload()
        ready = pod()
        failed = pod("kubelet-rejected")
        failed["status"] = {"phase": "Failed", "reason": "NodeAffinity"}
        completed = pod("completed")
        completed["status"] = {"phase": "Succeeded"}
        control.get_workload = Mock(return_value=obj)
        control.pods = Mock(return_value=[failed, completed, ready])
        self.assertEqual(control.wait_ready("work"), [ready])

    def test_pending_terminating_unready_or_incomplete_rollout_still_blocks(self):
        for state in ("pending", "terminating", "unready", "incomplete"):
            with self.subTest(state=state):
                h = fake_harness()
                h.wait = lambda description, predicate: predicate()
                control = ce.ControlEdges(h)
                obj, ready = workload(), pod()
                pods = [ready]
                if state == "pending":
                    extra = pod("pending")
                    extra["status"] = {"phase": "Pending"}
                    pods.append(extra)
                elif state == "terminating":
                    ready["metadata"]["deletionTimestamp"] = "2026-10-16T18:00:00Z"
                elif state == "unready":
                    ready["status"]["conditions"][0]["status"] = "False"
                else:
                    obj["status"]["updatedReplicas"] = 0
                control.get_workload = Mock(return_value=obj)
                control.pods = Mock(return_value=pods)
                self.assertFalse(control.wait_ready("work"))


class ManifestTests(unittest.TestCase):
    def setUp(self):
        self.h = fake_harness()
        self.h.pod = Mock(return_value={
            "metadata": {"namespace": self.h.namespace, "annotations": {ce.RUNTIME: ce.IDS["python"]}},
            "spec": {"runtimeClassName": "brewlet", "restartPolicy": "Never",
                     "containers": [{"name": "app", "image": self.h.app_images["python"],
                                     "imagePullPolicy": "Never", "readinessProbe": {
                                         "exec": {"command": ["python3", "probe"]}}}]}})
        self.control = ce.ControlEdges(self.h)

    def test_native_matrix_manifests_have_dedicated_selection_and_real_controllers(self):
        for kind in ("Deployment", "StatefulSet", "DaemonSet"):
            with self.subTest(kind=kind):
                obj = self.control.workload("matrix", kind, RID)
                self.assertEqual(obj["kind"], kind)
                self.assertEqual(obj["apiVersion"], "apps/v1")
                self.assertEqual(obj["metadata"]["namespace"], NS)
                self.assertEqual(obj["metadata"]["annotations"][ce.OPT_IN], "true")
                self.assertEqual(obj["spec"]["template"]["metadata"]["annotations"][ce.RUNTIME], RID)
                self.assertNotIn(ce.BARRIER, obj["spec"]["template"]["metadata"]["annotations"])
                self.assertEqual(obj["spec"]["template"]["spec"]["restartPolicy"], "Always")
                self.assertEqual(obj["spec"]["template"]["spec"]["containers"][0]["image"],
                                 self.h.app_images["python"])
                self.assertEqual(obj["spec"]["template"]["spec"]["containers"][0]["imagePullPolicy"],
                                 "Never")
        self.h.apply.assert_not_called()
        self.h.kubectl.assert_not_called()

    def test_controller_preserves_authenticated_delivery_policy_and_secret_reference(self):
        template = self.h.pod.return_value
        template["spec"]["containers"][0]["imagePullPolicy"] = "Always"
        template["spec"]["imagePullSecrets"] = [{"name": "owned-pull-secret"}]
        obj = self.control.workload("remote", "Deployment", RID)
        spec = obj["spec"]["template"]["spec"]
        self.assertEqual(spec["containers"][0]["imagePullPolicy"], "Always")
        self.assertEqual(spec["imagePullSecrets"], [{"name": "owned-pull-secret"}])

    def test_object_optout_is_distinct_from_template_optin(self):
        obj = self.control.workload("noopt", "Deployment", RID, opted=False)
        self.assertEqual(obj["metadata"]["annotations"][ce.OPT_IN], "false")
        self.assertEqual(obj["spec"]["template"]["metadata"]["annotations"][ce.OPT_IN], "true")

    def test_gated_rollout_is_actual_probe_failure_not_fake_status(self):
        obj = self.control.workload("gated", "Deployment", RID, gated=True)
        self.assertNotIn("status", obj)
        template = obj["spec"]["template"]
        self.assertNotIn("status", template)
        probe = template["spec"]["containers"][0]["readinessProbe"]["exec"]["command"]
        self.assertEqual(probe[:2], ["python3", "-c"])
        self.assertIn("BREWLET_RUNTIME_REVISION", probe[2])
        self.assertEqual(obj["spec"]["strategy"]["rollingUpdate"],
                         {"maxSurge": 1, "maxUnavailable": 0})
        self.assertGreater(obj["spec"]["progressDeadlineSeconds"], ce.DEADLINE)

    def test_ondelete_and_partition_are_native_strategy_fields(self):
        for kind in ("StatefulSet", "DaemonSet"):
            obj = self.control.workload("manual", kind, RID, strategy={"type": "OnDelete"})
            self.assertEqual(obj["spec"]["updateStrategy"], {"type": "OnDelete"})
        strategy = {"type": "RollingUpdate", "rollingUpdate": {"partition": 1}}
        obj = self.control.workload("part", "StatefulSet", RID, strategy=strategy)
        self.assertEqual(obj["spec"]["updateStrategy"], strategy)
        self.assertNotIn("delete", json.dumps(obj).lower())


class ReleaseTests(unittest.TestCase):
    def test_helm_always_carries_private_identity(self):
        h = fake_harness()
        ce.ControlEdges(h).helm(["get", "values", h.cluster])
        command = h.run.call_args.args[0]
        self.assertEqual(command[:5], ["helm", "--kubeconfig", h.kubeconfig,
                                      "--kube-context", h.context])

    def test_actual_chart_upgrade_sets_short_real_deadlines(self):
        h = fake_harness()
        h.kubectl.return_value = subprocess.CompletedProcess([], 0, "yes\n", "")
        c = ce.ControlEdges(h)
        c.helm = Mock()
        c.release_values = Mock(return_value={"operator": {"rotationRestarts": {
            "enabled": True, "maxConcurrent": 1, "nodeProgressDeadline": "10m",
            "workloadProgressDeadline": "30s"}}})
        c.enable()
        upgrade = c.helm.call_args.args[0]
        self.assertIn("operator.rotationRestarts.enabled=true", upgrade)
        self.assertIn("operator.rotationRestarts.workloadProgressDeadline=30s", upgrade)
        self.assertIn("operator.rotationRestarts.nodeProgressDeadline=10m", upgrade)
        self.assertIn("operator.rotationRestarts.maxConcurrent=1", upgrade)
        self.assertTrue(c.enabled)
        self.assertEqual(h.kubectl.call_count, 12)

    def test_default_rbac_denies_workload_access_except_required_daemonset_reads(self):
        h = fake_harness()
        # kubectl arguments are auth/can-i/VERB/RESOURCE/-n/NAMESPACE/--as.
        def answer(*args, **kwargs):
            allowed = args[3] == "daemonsets.apps" and args[2] != "patch"
            return subprocess.CompletedProcess([], 0 if allowed else 1, "yes\n" if allowed else "no\n", "")
        h.kubectl.side_effect = answer
        ce.ControlEdges(h).check_rbac(False)
        self.assertEqual(h.kubectl.call_count, 12)
        self.assertTrue(all(call.args[5] == NS for call in h.kubectl.call_args_list))


class PodOwnershipTests(unittest.TestCase):
    def test_deployment_requires_replicaset_uid_hop(self):
        obj = workload()
        rs = {"metadata": meta("rs", "rs-owned", NS)}
        rs["metadata"]["ownerReferences"] = [{"kind": "Deployment", "uid": UID,
                                              "controller": True}]
        actual = pod()
        wrong = pod("wrong", owner="other-rs")
        self.assertEqual(ce.owned_pods(obj, [actual, wrong], [rs], RUN), [actual])

    def test_labels_do_not_establish_ownership(self):
        obj = workload()
        p = pod()
        p["metadata"]["labels"][ce.CASE] = "work"
        self.assertEqual(ce.owned_pods(obj, [p], [], RUN), [])

    def test_stateful_and_daemon_direct_owners(self):
        for kind in ("StatefulSet", "DaemonSet"):
            with self.subTest(kind=kind):
                obj, p = workload(kind), pod(kind=kind, owner=UID)
                self.assertEqual(ce.owned_pods(obj, [p], [], RUN), [p])
                p["metadata"]["ownerReferences"][0]["uid"] = "foreign"
                self.assertEqual(ce.owned_pods(obj, [p], [], RUN), [])

    def test_foreign_pod_label_refused_even_with_correct_uid_hop(self):
        p = pod(kind="StatefulSet", owner=UID)
        p["metadata"]["labels"][ce.OWNER] = "foreign"
        with self.assertRaises(ce.Failure):
            ce.owned_pods(workload("StatefulSet"), [p], [], RUN)

    def test_namespace_boundary_is_enforced(self):
        p = pod(kind="StatefulSet", owner=UID)
        p["metadata"]["namespace"] = "foreign"
        self.assertEqual(ce.owned_pods(workload("StatefulSet"), [p], [], RUN), [])


class RolloutTests(unittest.TestCase):
    def test_complete_requires_native_controller_and_new_ready_pods(self):
        for kind in ("Deployment", "StatefulSet", "DaemonSet"):
            with self.subTest(kind=kind):
                ce.verify_restart(image(), workload(kind), [pod("old")], [pod()],
                                  row(), expected="Complete")

    def test_api_patch_is_not_native_rollout_completion(self):
        obj = workload()
        obj["status"]["updatedReplicas"] = 0
        with self.assertRaises(ce.Failure):
            ce.verify_restart(image(), obj, [pod("old")], [pod()], row(), expected="Complete")

    def test_old_pod_remaining_refuses_completion(self):
        p = pod()
        with self.assertRaises(ce.Failure):
            ce.verify_restart(image(), workload(), [p], [p], row(), expected="Complete")

    def test_new_unready_pod_refuses_completion(self):
        with self.assertRaises(ce.Failure):
            ce.verify_restart(image(), workload(), [pod("old")], [pod(ready=False)], row(),
                              expected="Complete")

    def test_missing_pod_barrier_refuses_completion(self):
        p = pod()
        del p["metadata"]["annotations"][ce.BARRIER]
        with self.assertRaises(ce.Failure):
            ce.verify_restart(image(), workload(), [pod("old")], [p], row(), expected="Complete")

    def test_controller_observed_generation_required(self):
        for kind in ("Deployment", "StatefulSet", "DaemonSet"):
            obj = workload(kind)
            obj["status"]["observedGeneration"] = 1
            self.assertFalse(ce.native_complete(obj))

    def test_zero_node_daemonset_is_not_execution_success(self):
        obj = workload("DaemonSet")
        obj["status"].update(desiredNumberScheduled=0, updatedNumberScheduled=0,
                             numberAvailable=0, numberReady=0)
        self.assertFalse(ce.native_complete(obj))

    def test_ondelete_and_deferred_preserve_actual_container_identity(self):
        p = pod("old")
        for phase in ("NotApplicable", "Deferred"):
            ce.verify_restart(image(), workload(), [p], [copy.deepcopy(p)],
                              row(phase, nonActionable=True), expected=phase)

    def test_ondelete_container_restart_is_not_accepted_as_retention(self):
        before, after = pod("old"), pod("old")
        after["status"]["containerStatuses"][0]["restartCount"] = 1
        with self.assertRaises(ce.Failure):
            ce.verify_restart(image(), workload(), [before], [after],
                              row("NotApplicable", nonActionable=True), expected="NotApplicable")

    def test_deferred_must_not_hold_slot(self):
        p = pod("old")
        with self.assertRaises(ce.Failure):
            ce.verify_restart(image(), workload(), [p], [p], row("Deferred"), expected="Deferred")

    def test_wrong_uid_epoch_and_target_are_refused(self):
        for field, value in (("uid", "recreated"), ("inventoryUID", "prior-epoch"),
                             ("generation", 1)):
            with self.subTest(field=field):
                with self.assertRaises(ce.Failure):
                    ce.verify_restart(image(), workload(), [pod("old")], [pod()],
                                      row(**{field: value}), expected="Complete")

    def test_previous_epoch_error_must_be_explicit(self):
        ce.verify_restart(image(), workload(), [], [],
                          row("EpochMismatch", "RuntimeImageEpochMismatch"), expected="EpochMismatch")
        with self.assertRaises(ce.Failure):
            ce.verify_restart(image(), workload(), [], [], row("EpochMismatch", "RestartFailed"),
                              expected="EpochMismatch")

    def test_status_selection_uses_uid_epoch_and_target_not_name(self):
        im = image()
        foreign = row(uid="foreign")
        prior = row(inventoryUID="prior")
        current = row()
        im["status"]["restarts"]["workloads"] = [foreign, prior, current]
        self.assertEqual(ce.restart_record(im, workload()), current)
        im["status"]["restarts"]["workloads"].append(copy.deepcopy(current))
        with self.assertRaises(ce.Failure):
            ce.restart_record(im, workload())


class PacingTests(unittest.TestCase):
    def with_rows(self, *rows, name=None):
        obj = image()
        if name:
            obj["metadata"]["name"] = name
            obj["metadata"]["uid"] = name + "-epoch"
            for r in rows:
                r["inventoryUID"] = obj["metadata"]["uid"]
        obj["status"]["restarts"]["workloads"] = list(rows)
        return obj

    def test_global_limit_counts_across_inventory_ids(self):
        first = self.with_rows(row("Rolling"))
        second = self.with_rows(row("Patching", uid="other"), name="second")
        with self.assertRaises(ce.Failure):
            ce.verify_pacing([first, second], 1, NOW, 30)

    def test_pending_does_not_take_slot(self):
        obj = self.with_rows(row("Rolling"), row("Pending", uid="waiting"))
        self.assertEqual(len(ce.verify_pacing([obj], 1, NOW, 30)), 1)

    def test_nonactionable_deferred_and_ondelete_do_not_take_slots(self):
        obj = self.with_rows(row("Rolling"), row("Patching", uid="ondelete", nonActionable=True),
                             row("Deferred", uid="paused"), row("NotApplicable", uid="manual"))
        self.assertEqual(len(ce.verify_pacing([obj], 1, NOW, 30)), 1)

    def test_deadline_releases_slot_without_claiming_completion(self):
        expired = row("Rolling", startedAt="2026-10-16T17:59:00Z")
        active = row("Rolling", uid="new", startedAt="2026-10-16T18:00:00Z")
        obj = self.with_rows(expired, active)
        self.assertEqual(ce.verify_pacing([obj], 1, NOW, 30), [active])
        self.assertEqual(expired["phase"], "Rolling")

    def test_superseded_targets_and_other_epochs_not_counted(self):
        obj = self.with_rows(row("Rolling"), row("Rolling", generation=1, uid="prior"),
                             row("Rolling", inventoryUID="old-epoch", uid="old"))
        self.assertEqual(len(ce.verify_pacing([obj], 1, NOW, 30)), 1)

    def test_missing_or_future_start_is_protocol_failure(self):
        for start in (None, "2026-10-16T19:00:00Z"):
            with self.subTest(start=start):
                obj = self.with_rows(row("Rolling", startedAt=start))
                with self.assertRaises(ce.Failure):
                    ce.verify_pacing([obj], 1, NOW, 30)


    def test_both_targets_finish_installing_before_restart_slots_open(self):
        control = object.__new__(ce.ControlEdges)
        calls = []
        source = "registry.test/runtime@sha256:" + "a" * 64
        control.ids = ["first", "second"]
        control.image = lambda _: {"spec": {"source": {"image": source}}, "status": {"generation": 2}}
        control.workload = lambda name, *_args, **_kwargs: name
        control.apply_workload = lambda name: calls.append("apply " + name)
        control.wait_ready = lambda name: calls.append("ready " + name)
        control.restore_release = lambda: calls.append("disable")
        control.rotate = lambda rid, _: calls.append("rotate " + rid)
        control.complete_rotation = lambda rid: calls.append("installed " + rid)
        control.enable = lambda **kwargs: calls.append("enable " + str(kwargs["workload_deadline"]))

        def sample(*_):
            calls.append("sample")
            raise ce.Failure("sampling reached")

        control.h = SimpleNamespace(runtime_images={"python": {"before": source}},
                                    timeout=300, get=sample, advertised=lambda *_: True,
                                    node_snapshot=lambda: {"spec": {}},
                                    wait=lambda description, predicate:
                                        calls.append("capacity " + description.split()[-1]) or predicate())
        with self.assertRaisesRegex(ce.Failure, "sampling reached"):
            control.pacing()
        self.assertEqual(calls, [
            "disable", "capacity first", "apply pace-0", "ready pace-0",
            "capacity second", "apply pace-1", "ready pace-1",
            "rotate first", "rotate second", "installed first", "installed second",
            "enable 60", "sample"])


class BlockedPodTests(unittest.TestCase):
    def blocked(self, reason="RuntimeImageInstalling"):
        return {"name": "pod", "namespace": NS, "uid": "new-pod", "node": "real-owned-node",
                "minimumGeneration": 2, "reason": reason}

    def waiting_pod(self):
        p = pod(ready=False)
        p["status"]["phase"] = "Pending"
        p["status"]["containerStatuses"][0]["state"] = {
            "waiting": {"reason": "CreateContainerError", "message": "RuntimeImageInstalling"}}
        return p

    def test_actual_installation_wait_has_matching_uid_node_and_generation(self):
        blocked = self.blocked()
        record = row("Deferred", blockedPods=[blocked])
        self.assertEqual(ce.blocked_pod_evidence(record, [self.waiting_pod()], 2, []), blocked)

    def test_never_started_create_failure_remains_proof_during_backoff_for_every_role(self):
        for role in ("containerStatuses", "initContainerStatuses", "ephemeralContainerStatuses"):
            with self.subTest(role=role):
                p = self.waiting_pod()
                p["status"]["containerStatuses"] = []
                p["status"][role] = [{"name": "app",
                    "state": {"waiting": {"reason": "CrashLoopBackOff"}},
                    "lastState": {"terminated": {"reason": "StartError",
                        "startedAt": "1970-01-01T00:00:00Z",
                        "message": "failed to create task: RuntimeImageInstalling"}}}]
                blocked = self.blocked()
                self.assertEqual(ce.blocked_pod_evidence(
                    row("Deferred", blockedPods=[blocked]), [p], 2, []), blocked)

    def test_running_or_actual_previous_execution_cannot_reuse_stale_failure_text(self):
        for state in ("running", "previously-started", "application-exit"):
            with self.subTest(state=state):
                p = self.waiting_pod()
                status = {"state": {"waiting": {"reason": "CrashLoopBackOff"}},
                          "lastState": {"terminated": {"reason": "StartError",
                              "startedAt": "1970-01-01T00:00:00Z",
                              "message": "RuntimeImageInstalling"}}}
                if state == "running":
                    status["state"] = {"running": {}}
                elif state == "previously-started":
                    status["lastState"]["terminated"]["startedAt"] = "2026-10-16T18:00:00Z"
                else:
                    status["lastState"]["terminated"]["reason"] = "Error"
                p["status"]["containerStatuses"] = [status]
                with self.assertRaises(ce.Failure):
                    ce.blocked_pod_evidence(row("Deferred", blockedPods=[self.blocked()]), [p], 2, [])

    def test_annotation_or_fixture_label_alone_is_not_waiting_proof(self):
        p = pod(ready=False)
        p["metadata"]["annotations"]["fixture/reason"] = "RuntimeImageInstalling"
        record = row("Deferred", blockedPods=[self.blocked()])
        with self.assertRaises(ce.Failure):
            ce.blocked_pod_evidence(record, [p], 2, [])

    def test_wrong_uid_name_node_and_minimum_refused(self):
        for field, value in (("uid", "other"), ("name", "other"), ("node", "other"),
                             ("namespace", "other"), ("minimumGeneration", 1)):
            with self.subTest(field=field):
                blocked = self.blocked()
                blocked[field] = value
                with self.assertRaises(ce.Failure):
                    ce.blocked_pod_evidence(row("Deferred", blockedPods=[blocked]),
                                            [self.waiting_pod()], 2, [])

    def test_excluded_requires_actual_nonrunning_bound_pod_and_uid_scoped_exclusion(self):
        p = self.waiting_pod()
        p["status"]["containerStatuses"] = []
        blocked = self.blocked("NodeExcluded")
        excluded = [{"name": "real-owned-node", "uid": "real-host-uid"}]
        self.assertEqual(ce.blocked_pod_evidence(row("Deferred", blockedPods=[blocked]),
                                                [p], 2, excluded), blocked)
        with self.assertRaises(ce.Failure):
            ce.blocked_pod_evidence(row("Deferred", blockedPods=[blocked]), [p], 2, [])

    def test_failed_or_complete_is_not_deferred(self):
        for phase in ("Failed", "Complete", "Rolling"):
            with self.assertRaises(ce.Failure):
                ce.blocked_pod_evidence(row(phase, blockedPods=[self.blocked()]),
                                        [self.waiting_pod()], 2, [])


class APIRequestTests(unittest.TestCase):
    def node(self):
        return {"apiVersion": "v1", "kind": "Node",
                "metadata": meta("control-cas-" + RUN + "-node", "test-node"),
                "spec": {"unschedulable": True}}

    def test_real_put_path_preserves_uid_and_resource_version(self):
        obj = self.node()
        path, body = ce.cas_request(obj, RUN, "test-node", "real-owned-node", "changed")
        self.assertEqual(path, "/api/v1/nodes/control-cas-" + RUN + "-node")
        self.assertEqual(body["metadata"]["uid"], "test-node")
        self.assertEqual(body["metadata"]["resourceVersion"], "37")
        self.assertEqual(body["metadata"]["annotations"][ce.PROBE], "changed")
        self.assertNotIn("annotations", obj["metadata"])
        self.assertNotIn("status", body)

    def test_replacement_uid_rejected_before_network(self):
        obj = self.node()
        with self.assertRaises(ce.Failure):
            ce.cas_request(obj, RUN, "prior-uid", "real-owned-node", "changed")

    def test_foreign_ownership_and_node_names_rejected(self):
        for mutation in ("owner", "name", "live"):
            obj = self.node()
            if mutation == "owner":
                obj["metadata"]["labels"][ce.OWNER] = "foreign"
            elif mutation == "name":
                obj["metadata"]["name"] = "arbitrary-node"
            live = obj["metadata"]["name"] if mutation == "live" else "real-owned-node"
            with self.assertRaises(ce.Failure):
                ce.cas_request(obj, RUN, "test-node", live, "changed")

    def test_real_host_identity_is_never_accepted_for_synthetic_probe(self):
        for key in ("providerID", "systemUUID"):
            obj = self.node()
            if key == "providerID":
                obj["spec"][key] = "real-provider-host"
            else:
                obj["status"] = {"nodeInfo": {key: "real-host"}}
            with self.assertRaises(ce.Failure):
                ce.cas_request(obj, RUN, "test-node", "real-owned-node", "changed")

    def test_no_claim_or_runtime_state_mutation(self):
        for area in ("labels", "annotations"):
            for key in ("brewlet.sh/owner-uid", "brewlet.sh/runtime.test",
                        "brewlet.sh/runtime-images", "brewlet.sh/cleanup-complete"):
                with self.subTest(area=area, key=key):
                    obj = self.node()
                    obj["metadata"].setdefault(area, {})[key] = "state"
                    with self.assertRaises(ce.Failure):
                        ce.cas_request(obj, RUN, "test-node", "real-owned-node", "changed")

    def test_missing_real_resource_version_refused(self):
        obj = self.node()
        del obj["metadata"]["resourceVersion"]
        with self.assertRaises(ce.Failure):
            ce.cas_request(obj, RUN, "test-node", "real-owned-node", "changed")


class CASSetupTests(unittest.TestCase):
    def test_setup_retries_only_conflicts_with_fresh_validated_node(self):
        first = {"kind": "Node", "metadata": meta("control-cas-" + RUN + "-node", UID),
                 "spec": {"unschedulable": True}}
        first["metadata"]["annotations"] = {ce.PROBE: "original"}
        fresh = copy.deepcopy(first)
        fresh["metadata"]["resourceVersion"] = "38"
        h = SimpleNamespace(cluster="fixture", namespace="base", control_namespace="system",
                            run_id=RUN, node="real-owned-node",
                            apply=Mock(return_value=first),
                            get=Mock(side_effect=[first, fresh]),
                            kubectl=Mock(side_effect=[
                                SimpleNamespace(returncode=1, stderr="Error (Conflict): modified"),
                                SimpleNamespace(returncode=0, stderr="")]))

        class SetupObserved(Exception):
            pass

        def wait(description, predicate, timeout):
            self.assertEqual(timeout, 30)
            self.assertFalse(predicate())
            _, stale = predicate()
            self.assertEqual(stale["metadata"]["resourceVersion"], "38")
            self.assertEqual(stale["metadata"]["annotations"][ce.PROBE], "stale")
            raise SetupObserved()

        h.wait = wait
        with self.assertRaises(SetupObserved):
            ce.ControlEdges(h).api_cas()
        bodies = [json.loads(c.kwargs["input"]) for c in h.kubectl.call_args_list]
        self.assertEqual([b["metadata"]["resourceVersion"] for b in bodies], ["37", "38"])

    def test_setup_refuses_unrelated_errors_and_replaced_nodes(self):
        for refusal in ("Forbidden", "replacement"):
            with self.subTest(refusal=refusal):
                obj = {"kind": "Node",
                       "metadata": meta("control-cas-" + RUN + "-node", UID),
                       "spec": {"unschedulable": True}}
                obj["metadata"]["annotations"] = {ce.PROBE: "original"}
                current = copy.deepcopy(obj)
                if refusal == "replacement":
                    current["metadata"]["uid"] = "new-uid"
                h = SimpleNamespace(cluster="fixture", namespace="base",
                                    control_namespace="system", run_id=RUN, node="real-owned-node",
                                    apply=Mock(return_value=obj), get=Mock(return_value=current),
                                    kubectl=Mock(return_value=SimpleNamespace(
                                        returncode=1, stderr="Forbidden")))
                h.wait = lambda description, predicate, timeout: predicate()
                with self.assertRaises(ce.Failure):
                    ce.ControlEdges(h).api_cas()
                if refusal == "replacement":
                    h.kubectl.assert_not_called()


class ControlDeletionTests(unittest.TestCase):
    def harness(self):
        name = "control-cas-" + RUN + "-node"
        obj = {"kind": "Node", "metadata": meta(name, UID)}
        h = SimpleNamespace(cluster="fixture", namespace="base", control_namespace="system",
                            run_id=RUN, owned={("node", "", name): UID},
                            get=Mock(return_value=obj), kubectl=Mock())
        h.wait = lambda description, predicate, timeout: predicate()
        return h, name, obj

    def test_conflict_refreshes_version_without_weakening_uid_precondition(self):
        h, name, obj = self.harness()
        fresh = copy.deepcopy(obj)
        fresh["metadata"]["resourceVersion"] = "38"
        h.get.side_effect = [obj, fresh]
        h.kubectl.side_effect = [
            SimpleNamespace(returncode=1, stderr="Error (Conflict): modified"),
            SimpleNamespace(returncode=0, stderr="")]

        def wait(description, predicate, timeout):
            self.assertEqual(timeout, 30)
            self.assertFalse(predicate())
            self.assertTrue(predicate())

        h.wait = wait
        ce.ControlEdges(h).delete_object("Node", name)
        preconditions = [json.loads(c.kwargs["input"])["preconditions"]
                         for c in h.kubectl.call_args_list]
        self.assertEqual(preconditions, [{"uid": UID, "resourceVersion": "37"},
                                         {"uid": UID, "resourceVersion": "38"}])

    def test_replacement_or_foreign_owner_is_not_deleted_after_conflict(self):
        for changed in ("uid", "owner"):
            with self.subTest(changed=changed):
                h, name, obj = self.harness()
                fresh = copy.deepcopy(obj)
                if changed == "uid":
                    fresh["metadata"]["uid"] = "replacement"
                else:
                    fresh["metadata"]["labels"][ce.OWNER] = "foreign"
                h.get.side_effect = [obj, fresh]
                h.kubectl.return_value = SimpleNamespace(returncode=1, stderr="Conflict")

                def wait(description, predicate, timeout):
                    self.assertFalse(predicate())
                    return predicate()

                h.wait = wait
                with self.assertRaises(ce.Failure):
                    ce.ControlEdges(h).delete_object("Node", name)
                h.kubectl.assert_called_once()

    def test_absent_is_complete_and_unrelated_failure_is_not_retried(self):
        h, name, _ = self.harness()
        h.get.return_value = None
        ce.ControlEdges(h).delete_object("Node", name)
        h.kubectl.assert_not_called()
        h, name, _ = self.harness()
        h.kubectl.return_value = SimpleNamespace(returncode=1, stderr="Forbidden")
        with self.assertRaises(ce.Failure):
            ce.ControlEdges(h).delete_object("Node", name)
        h.kubectl.assert_called_once()


class ReleaseCASTests(unittest.TestCase):
    def setUp(self):
        self.config = {"runID": RUN, "node": "real-owned-node", "nodeUID": "real-node-uid",
                   "profileUID": "real-profile-uid", "profile": "real-profile", "generation": 5,
                   "operatorUsername": "system:serviceaccount:owned-system:brewlet-operator"}
        self.before = {"kind": "Node", "metadata": meta(self.config["node"], "real-node-uid")}
        self.before["metadata"]["labels"].update({
            "brewlet.sh/owner-uid": "real-profile-uid", "brewlet.sh/owner-node-uid": "real-node-uid"})
        self.before["metadata"]["annotations"] = {
            "brewlet.sh/owner-name": "real-profile",
            "brewlet.sh/cleanup-complete": "real-profile-uid/5"}
        self.after = copy.deepcopy(self.before)
        for k in ("brewlet.sh/owner-uid", "brewlet.sh/owner-node-uid"):
            del self.after["metadata"]["labels"][k]
        del self.after["metadata"]["annotations"]["brewlet.sh/owner-name"]
        self.request = {"uid": "first-api-request", "operation": "UPDATE",
                    "resource": {"resource": "nodes"}, "oldObject": self.before,
                    "object": self.after,
                    "userInfo": {"username": self.config["operatorUsername"]}}
        self.profile = {
            "metadata": {"uid": "real-profile-uid", "deletionTimestamp": "2026-10-16T18:00:00Z"},
            "status": {"retirement": {"phase": "Teardown", "generation": 5,
                                   "targets": [{"name": "real-owned-node", "uid": "real-node-uid"}]},
                   "conditions": []}}
        self.current = copy.deepcopy(self.before)
        self.events, self.calls = [], []

    def api(self, method, path, data=None):
        self.calls.append((method, path, copy.deepcopy(data)))
        if method == "GET" and "/nodeprofiles/" in path:
            return copy.deepcopy(self.profile)
        if method == "GET":
            return copy.deepcopy(self.current)
        self.assertEqual(method, "PATCH")
        self.assertEqual(data[-1]["path"], "/metadata/annotations/" + ce.pointer(ce.RELEASE_MARKER))
        self.current["metadata"]["annotations"][ce.RELEASE_MARKER] = data[-1]["value"]
        self.current["metadata"]["resourceVersion"] = "38"
        return copy.deepcopy(self.current)

    def engine(self):
        return ce.ReleaseAdmission(self.api, self.events.append)

    def retry(self):
        self.profile["status"]["conditions"] = [{
            "type": "Ready", "status": "False", "reason": "CleanupBlocked",
            "message": 'Operation cannot be fulfilled on nodes "real-owned-node": '
                   "the object has been modified; please apply your changes to the latest version"}]
        request = copy.deepcopy(self.request)
        request["uid"] = "fresh-api-request"
        request["oldObject"] = copy.deepcopy(self.current)
        request["object"]["metadata"]["resourceVersion"] = "38"
        request["object"]["metadata"]["annotations"][ce.RELEASE_MARKER] = (
            self.current["metadata"]["annotations"][ce.RELEASE_MARKER])
        return request

    def test_trigger_is_exact_node_uid_profile_and_release_transition(self):
        self.assertTrue(ce.release_trigger(self.request, self.config))
        for area, key, value in (("oldObject", "uid", "recreated"), ("object", "uid", "recreated"),
                             ("oldObject", "name", "foreign"), ("object", "name", "foreign")):
            req = copy.deepcopy(self.request)
            req[area]["metadata"][key] = value
            self.assertFalse(ce.release_trigger(req, self.config))
        req = copy.deepcopy(self.request)
        req["oldObject"]["metadata"]["labels"]["brewlet.sh/owner-uid"] = "foreign"
        self.assertFalse(ce.release_trigger(req, self.config))
        req = copy.deepcopy(self.request)
        req["object"]["metadata"]["labels"]["brewlet.sh/owner-uid"] = "real-profile-uid"
        self.assertFalse(ce.release_trigger(req, self.config))

    def test_patch_contains_atomic_uid_rv_and_claim_tests_only_one_harmless_write(self):
        obj = copy.deepcopy(self.before)
        operations = ce.release_patch(obj, self.config, "first-api-request")
        self.assertEqual(obj, self.before)
        writes = [v for v in operations if v["op"] != "test"]
        self.assertEqual(len(writes), 1)
        self.assertEqual(writes[0]["op"], "add")
        self.assertEqual(writes[0]["path"], "/metadata/annotations/" + ce.pointer(ce.RELEASE_MARKER))
        self.assertIn({"op": "test", "path": "/metadata/uid", "value": "real-node-uid"}, operations)
        self.assertIn({"op": "test", "path": "/metadata/resourceVersion", "value": "37"}, operations)
        for key in ("brewlet.sh/owner-uid", "brewlet.sh/owner-node-uid"):
            self.assertTrue(any(p["op"] == "test" and p["path"] == "/metadata/labels/" + ce.pointer(key)
                            for p in operations))

    def test_json_pointer_escaping(self):
        self.assertEqual(ce.pointer("domain.test/a~b"), "domain.test~1a~0b")

    def test_admission_container_uses_importable_layout_and_valid_json_entrypoint(self):
        dockerfile = ce.release_dockerfile(RUN)
        self.assertIn("COPY harness.py interceptor.py control_edges.py /acceptance/live-v2/", dockerfile)
        line = [v for v in dockerfile.splitlines() if v.startswith("ENTRYPOINT ")][0]
        command = json.loads(line[len("ENTRYPOINT "):])
        self.assertEqual(command[:2], ["python3", "-c"])
        self.assertIn("'/acceptance/live-v2'", command[2])
        self.assertIn("control_edges._serve_release_admission()", command[2])
        self.assertNotIn("production_cleanup()", command[2])

    def test_real_release_interceptor_mutates_once_then_requires_conflict_and_fresh_rv(self):
        engine = self.engine()
        allowed, _ = engine.decision(self.request, self.config)
        self.assertTrue(allowed)
        self.assertEqual(self.events[0]["event"], "release-cas-injected")
        self.assertEqual(self.events[0]["requestResourceVersion"], "37")
        self.assertEqual(self.events[0]["patchedResourceVersion"], "38")
        allowed, _ = engine.decision(self.retry(), self.config)
        self.assertTrue(allowed)
        self.assertEqual(self.events[-1]["event"], "release-retry-validated")
        self.assertEqual(len([c for c in self.calls if c[0] == "PATCH"]), 1)
        self.assertEqual(self.before["metadata"]["labels"]["brewlet.sh/owner-uid"], "real-profile-uid")

    def test_same_trigger_after_interceptor_restart_still_does_not_reinject(self):
        self.engine().decision(self.request, self.config)
        self.engine().decision(self.retry(), self.config)
        self.assertEqual(len([c for c in self.calls if c[0] == "PATCH"]), 1)

    def test_dryrun_is_not_real_api_evidence(self):
        self.request["dryRun"] = True
        with self.assertRaises(ce.Failure):
            self.engine().decision(self.request, self.config)
        self.assertFalse(self.calls)

    def test_user_claim_edit_is_not_a_production_operator_release(self):
        self.request["userInfo"]["username"] = "test-admin"
        with self.assertRaises(ce.Failure):
            self.engine().decision(self.request, self.config)
        self.assertFalse(self.calls)

    def test_no_teardown_or_fake_cleanup_receipt_is_never_completed(self):
        for mutation in ("phase", "receipt", "inventory", "capability", "target"):
            with self.subTest(mutation=mutation):
                before, profile = copy.deepcopy(self.before), copy.deepcopy(self.profile)
                if mutation == "phase":
                    profile["status"]["retirement"]["phase"] = "Cleaning"
                elif mutation == "receipt":
                    del before["metadata"]["annotations"]["brewlet.sh/cleanup-complete"]
                elif mutation == "inventory":
                    before["metadata"]["annotations"]["brewlet.sh/runtime-images"] = '{"x":{}}'
                elif mutation == "capability":
                    before["metadata"]["labels"]["brewlet.sh/runtime.test"] = "ready"
                else:
                    profile["status"]["retirement"]["targets"][0]["uid"] = "foreign"
                with self.assertRaises(ce.Failure):
                    ce.release_teardown(profile, before, self.after, self.config)

    def test_frozen_retirement_generation_not_current_spec_is_cleanup_authority(self):
        config = dict(self.config, generation=99)
        self.assertEqual(ce.release_teardown(self.profile, self.before, self.after, config), 5)
        engine = ce.ReleaseAdmission(self.api, self.events.append)
        allowed, _ = engine.decision(self.request, config)
        self.assertTrue(allowed)
        self.assertEqual(self.events[0]["marker"]["retirementGeneration"], 5)

    def test_preexisting_concurrent_rv_change_does_not_count_as_injection(self):
        self.current["metadata"]["resourceVersion"] = "39"
        allowed, message = self.engine().decision(self.request, self.config)
        self.assertTrue(allowed)
        self.assertIn("no injection", message)
        self.assertFalse(self.events)
        self.assertFalse(any(c[0] == "PATCH" for c in self.calls))

    def test_attempted_mutation_without_storage_commit_is_not_success(self):
        def no_commit(method, path, data=None):
            if method == "PATCH":
                return copy.deepcopy(self.before)
            return self.api(method, path, data)
        engine = ce.ReleaseAdmission(no_commit, self.events.append)
        with self.assertRaises(ce.Failure):
            engine.decision(self.request, self.config)
        self.assertFalse(self.events)

    def test_retry_without_real_conflict_checkpoint_is_not_proof(self):
        engine = self.engine()
        engine.decision(self.request, self.config)
        request = self.retry()
        self.profile["status"]["conditions"] = []
        with self.assertRaises(ce.Failure):
            engine.decision(request, self.config)

    def test_retry_with_stale_request_version_is_rejected(self):
        engine = self.engine()
        engine.decision(self.request, self.config)
        req = self.retry()
        req["object"]["metadata"]["resourceVersion"] = "37"
        with self.assertRaises(ce.Failure):
            engine.decision(req, self.config)
        self.assertEqual(len([c for c in self.calls if c[0] == "PATCH"]), 1)

    def test_actual_cas_evidence_requires_conflict_retry_and_eventual_claim_release(self):
        engine = self.engine()
        engine.decision(self.request, self.config)
        req = self.retry()
        engine.decision(req, self.config)
        released = copy.deepcopy(req["object"])
        proof = ce.verify_release_audit(self.events, released, self.config)
        self.assertEqual(proof["firstRealConflict"]["reason"], "CleanupBlocked")
        for mutation in ("no-retry", "two-injections", "claim-remains", "node-replaced",
                         "no-conflict", "wrong-marker"):
            with self.subTest(mutation=mutation):
                rows, node = copy.deepcopy(self.events), copy.deepcopy(released)
                if mutation == "no-retry":
                    rows.pop()
                elif mutation == "two-injections":
                    rows.append(copy.deepcopy(rows[0]))
                elif mutation == "claim-remains":
                    node["metadata"]["labels"]["brewlet.sh/owner-uid"] = "real-profile-uid"
                elif mutation == "node-replaced":
                    node["metadata"]["uid"] = "replacement"
                elif mutation == "no-conflict":
                    rows[1]["conflictCheckpoint"] = []
                else:
                    node["metadata"]["annotations"][ce.RELEASE_MARKER] = "{}"
                with self.assertRaises(ce.Failure):
                    ce.verify_release_audit(rows, node, self.config)

    def test_nonrelease_nested_annotation_patch_never_recurses(self):
        request = copy.deepcopy(self.request)
        request["object"] = copy.deepcopy(self.before)
        request["object"]["metadata"]["annotations"][ce.RELEASE_MARKER] = "instrumentation"
        allowed, _ = self.engine().decision(request, self.config)
        self.assertTrue(allowed)
        self.assertFalse(self.calls)


class ProviderProofTests(unittest.TestCase):
    def test_fixture_labels_without_actual_tls_refusal_do_not_prove_pin(self):
        h = fake_harness()
        h.kubectl.return_value = subprocess.CompletedProcess([], 0, "ordinary worker log", "")
        provider = ce.ProviderPins(ce.ControlEdges(h))
        provider.revision = 4
        self.assertFalse(provider.refused("brewlet.sh/runtime.test"))

    def test_refusal_audit_requires_exact_key_revision_and_request_uid(self):
        h = fake_harness()
        key = "brewlet.sh/runtime.test"
        valid = {"requestUID": "real-review-uid", "revision": 4, "allowed": False,
                 "message": "provider-pinned label refusal: " + key}
        provider = ce.ProviderPins(ce.ControlEdges(h))
        provider.revision = 4
        for mutation in ({"revision": 3}, {"requestUID": ""}, {"allowed": True},
                         {"message": "provider-pinned label refusal: other"}):
            wrong = dict(valid, **mutation)
            h.kubectl.return_value = subprocess.CompletedProcess([], 0, json.dumps(wrong), "")
            self.assertFalse(provider.refused(key))
        h.kubectl.return_value = subprocess.CompletedProcess([], 0, json.dumps(valid), "")
        self.assertEqual(provider.refused(key), valid)
        self.assertEqual(provider.audit, [valid])

    def test_gaps_are_explicit_not_full_acceptance(self):
        self.assertTrue(any("not production claim-release" in s for s in ce.GAPS))
        self.assertTrue(any("RuntimeImageInstalling" in s for s in ce.GAPS))
        self.assertTrue(any("uninstall" in s for s in ce.GAPS))


if __name__ == "__main__":
    unittest.main()
