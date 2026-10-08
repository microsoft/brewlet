#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import copy
import importlib.util
import json
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    "fixtures", Path(__file__).with_name("nodeprofile-fixtures.py"))
fixtures = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixtures)


class OperatorAPIsTest(unittest.TestCase):
    def test_standalone_manager_tiers_install_every_watched_crd(self):
        for filename in ("tier4-k8s.sh", "tier7-petclinic.sh", "tier13-nodeprofile.sh",
                         "tier18-jdk-patch.sh"):
            source = Path(__file__).with_name(filename).read_text()
            with self.subTest(tier=filename):
                for crd, resource in (
                    ("nodeprofile", "nodeprofiles.node.brewlet.sh"),
                    ("javaapplication", "javaapplications.apps.brewlet.sh"),
                    ("noderetirementevidence", "noderetirementevidence.node.brewlet.sh"),
                ):
                    self.assertIn(f'/deploy/{crd}-crd.yaml"', source)
                    self.assertIn(f"crd/{resource}", source)


class PlacementTest(unittest.TestCase):
    def setUp(self):
        self.node = {"metadata": {"name": "worker", "uid": "node-uid",
                     "labels": {fixtures.OWNER: "owner", fixtures.IDENTITY: "node-uid",
                                "agentpool": "batch"},
                     "annotations": {fixtures.OWNER_NAME: "batch"}}}
        self.profile = {
            "metadata": {"uid": "owner"},
            "spec": {"nodePool": {"names": ["batch"], "key": "agentpool"}},
            "status": {"targets": [{"name": "worker", "uid": "node-uid", "claimed": True}]},
        }
        self.term = {
            "matchFields": [{"key": "metadata.name", "operator": "In", "values": ["worker"]}],
            "matchExpressions": [
                {"key": "agentpool", "operator": "In", "values": ["batch"]},
                {"key": fixtures.IDENTITY, "operator": "In", "values": ["node-uid"]},
                {"key": fixtures.OWNER, "operator": "In", "values": ["owner"]},
                *({"key": role, "operator": "DoesNotExist"} for role in fixtures.ROLES),
            ],
        }
        self.ds = {
            "metadata": {"ownerReferences": [{"uid": "owner", "controller": True}]},
            "spec": {"template": {"spec": {
                "containers": [{"name": "provisioner", "env": [
                    {"name": "BREWLET_PROFILE_UID", "value": "owner"},
                    {"name": "BREWLET_PROFILE_NAME", "value": "batch"},
                ]}],
                "affinity": {"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution":
                             {"nodeSelectorTerms": [self.term]}}},
            }}},
        }

    def get(self, kind, *args):
        return {
            "nodeprofile": self.profile, "nodeprofiles": {"items": [self.profile]},
            "daemonset": self.ds, "nodes": {"items": [self.node]}, "daemonsets,pods": {"items": []},
        }[kind]

    def validate(self, count=1):
        with patch.object(fixtures, "get", self.get):
            fixtures.placement("brewlet", "batch", count)

    def test_expression_order_is_irrelevant(self):
        self.validate()
        self.term["matchExpressions"].reverse()
        self.validate()

    def test_empty_ledger_matches_nothing(self):
        self.profile["status"]["targets"] = []
        self.term.clear()
        self.node["metadata"]["labels"]["agentpool"] = "another-pool"
        self.validate(0)

    def test_empty_ledger_rejects_permissive_term(self):
        self.profile["status"]["targets"] = []
        with self.assertRaises(AssertionError):
            self.validate(0)

    def test_each_identity_pool_role_requirement_is_enforced(self):
        for requirement in list(self.term["matchExpressions"]):
            with self.subTest(key=requirement["key"]):
                original = copy.deepcopy(self.term["matchExpressions"])
                self.term["matchExpressions"].remove(requirement)
                with self.assertRaises(AssertionError):
                    self.validate()
                self.term["matchExpressions"] = original
        self.term["matchFields"] = []
        with self.assertRaises(AssertionError):
            self.validate()

    def test_broad_or_term_is_rejected(self):
        terms = self.ds["spec"]["template"]["spec"]["affinity"]["nodeAffinity"][
            "requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
        terms.append({"matchExpressions": [{"key": "agentpool", "operator": "Exists"}]})
        with self.assertRaises(AssertionError):
            self.validate()

    def test_cleanup_keeps_identity_without_obsolete_pool_or_role_filters(self):
        self.term["matchExpressions"] = [r for r in self.term["matchExpressions"]
                                        if r["key"] in (fixtures.OWNER, fixtures.IDENTITY)]
        self.node["metadata"]["labels"]["agentpool"] = "changed-pool"
        self.node["metadata"]["labels"][fixtures.ROLES[0]] = ""
        self.ds["spec"]["template"]["spec"]["containers"][0]["env"].append(
            {"name": "BREWLET_MODE", "value": "cleanup"})
        with patch.object(fixtures, "get", self.get):
            fixtures.placement("brewlet", "batch", 1, "cleanup")

    def test_catchall_membership_excludes_named_pool_independently(self):
        named = copy.deepcopy(self.profile)
        self.profile["spec"]["nodePool"] = {}
        self.term["matchExpressions"] = [r for r in self.term["matchExpressions"]
                                        if r["key"] != "agentpool"]

        def get(kind, *args):
            if kind == "nodeprofiles":
                return {"items": [self.profile, named]}
            return self.get(kind, *args)

        with patch.object(fixtures, "get", get), self.assertRaises(AssertionError):
            fixtures.placement("brewlet", "batch", 1)


class TeardownTest(unittest.TestCase):
    image = "invalid.brewlet-e2e.invalid/provisioner:unique"

    def worker(self, kind="DaemonSet"):
        obj = {
            "kind": kind,
            "metadata": {"namespace": "brewlet", "uid": "ds", "ownerReferences": [
                {"uid": "owner" if kind == "DaemonSet" else "ds", "controller": True}]},
            "spec": {"containers": [{"name": "provisioner", "image": self.image}]},
        }
        if kind == "DaemonSet":
            obj["spec"] = {"template": {"spec": obj["spec"]}}
        return obj

    def assert_refused(self, workers, image=None, uid="owner"):
        profile = '{"metadata":{"uid":"owner"},"status":{}}'
        with patch.object(fixtures, "kubectl", return_value=profile) as command, \
             patch.object(fixtures, "get", return_value={"items": workers}):
            with self.assertRaises(AssertionError):
                fixtures.teardown("brewlet", "batch", uid, image or self.image)
            self.assertTrue(all(c.args[0] == "get" for c in command.call_args_list))

    def test_dirty_workers_cannot_release_claims(self):
        for status in (
            {"phase": "Running"},
            {"containerStatuses": [{"containerID": "containerd://ran"}]},
            {"containerStatuses": [{"imageID": "sha256:downloaded"}]},
            {"containerStatuses": [{"restartCount": 1}]},
            {"containerStatuses": [{"lastState": {"terminated": {"exitCode": 0}}}]},
            {"initContainerStatuses": [{"state": {"running": {}}}]},
        ):
            with self.subTest(status=status):
                pod = self.worker("Pod")
                pod["status"] = status
                self.assert_refused([self.worker(), pod])

    def test_foreign_and_executable_workers_are_preserved(self):
        for field, value in (("namespace", "foreign"), ("ownerReferences", [])):
            worker = self.worker()
            worker["metadata"][field] = value
            self.assert_refused([worker])
        worker = self.worker()
        worker["spec"]["template"]["spec"]["containers"][0]["image"] = "real-provisioner:latest"
        self.assert_refused([worker])
        self.assert_refused([], image="real-provisioner:latest")
        self.assert_refused([], uid="replacement")

    def test_unstarted_teardown_uses_identity_cas_and_preserves_other_finalizers(self):
        profile = {"metadata": {"uid": "owner", "resourceVersion": "profile-version",
                    "finalizers": ["node.brewlet.sh/cleanup", "other/finalizer"]},
                   "status": {"targets": [{"name": "worker", "uid": "node-uid", "claimed": True}]}}
        node = {"metadata": {
            "name": "worker", "uid": "node-uid", "resourceVersion": "node-version",
            "labels": {fixtures.OWNER: "owner", fixtures.IDENTITY: "node-uid"},
            "annotations": {fixtures.OWNER_NAME: "batch", "brewlet.sh/provision-state": "Provisioning"},
        }}
        listings = iter([[self.worker()], []])

        def get(kind, *args):
            if kind == "daemonsets,pods":
                return {"items": next(listings)}
            return {"nodes": {"items": [node]}, "nodeprofile": profile}[kind]

        with patch.object(fixtures, "get", get), \
             patch.object(fixtures, "kubectl", return_value=json.dumps(profile)) as command, \
             patch("builtins.print"):
            fixtures.teardown("brewlet", "batch", "owner", self.image)
        patches = [c.args for c in command.call_args_list if c.args[0] == "patch"]
        node_patch, profile_patch = [json.loads(args[-1]) for args in patches]
        self.assertEqual(node_patch[:2], [
            {"op": "test", "path": "/metadata/uid", "value": "node-uid"},
            {"op": "test", "path": "/metadata/resourceVersion", "value": "node-version"},
        ])
        self.assertIn({"op": "remove", "path": "/metadata/annotations/brewlet.sh~1provision-state"},
                      node_patch)
        self.assertEqual(profile_patch[0], {"op": "test", "path": "/metadata/uid", "value": "owner"})
        self.assertEqual(profile_patch[-1]["value"], ["other/finalizer"])


def failed(stderr, args=("kubectl",)):
    return fixtures.subprocess.CalledProcessError(1, list(args), output="", stderr=stderr)


class TransientRetryTest(unittest.TestCase):
    RESET = "Unable to connect to the server: read tcp 10.0.0.1:5->1.2.3.4:443: read: connection reset by peer"

    def setUp(self):
        env = patch.dict(fixtures.os.environ, {"E2E_RETRIES": "4", "E2E_RETRY_BACKOFF": "0"})
        env.start()
        self.addCleanup(env.stop)
        sleep = patch.object(fixtures.time, "sleep")
        self.sleep = sleep.start()
        self.addCleanup(sleep.stop)

    def test_only_transport_errors_are_transient(self):
        for stderr in (self.RESET, "error: unexpected EOF", "error: EOF",
                       "Unable to connect to the server: net/http: TLS handshake timeout",
                       "dial tcp: socket is not connected"):
            with self.subTest(stderr=stderr):
                self.assertTrue(fixtures.transient(failed(stderr)))
        for stderr in ('Error from server (NotFound): nodeprofiles "batch" not found',
                       "Error from server (Conflict): the object has been modified",
                       "The request is invalid: test operation failed", "HEOFX", ""):
            with self.subTest(stderr=stderr):
                self.assertFalse(fixtures.transient(failed(stderr)))
        self.assertFalse(fixtures.transient(AssertionError("connection reset by peer")))

    def test_idempotent_kubectl_retries_transient_errors(self):
        calls = iter([failed(self.RESET), failed("error: EOF"), "ok"])

        def once(*args):
            value = next(calls)
            if isinstance(value, Exception):
                raise value
            return value

        with patch.object(fixtures, "_kubectl_once", side_effect=once) as command, \
             patch("builtins.print"):
            self.assertEqual(fixtures.kubectl("delete", "nodeprofile", "batch", "--ignore-not-found"), "ok")
        self.assertEqual(command.call_count, 3)

    def test_non_idempotent_and_real_errors_are_not_retried(self):
        with patch.object(fixtures, "_kubectl_once", side_effect=failed(self.RESET)) as command:
            with self.assertRaises(fixtures.subprocess.CalledProcessError):
                fixtures.kubectl("patch", "node", "worker", "--type=json", "-p", "[]")
            with self.assertRaises(fixtures.subprocess.CalledProcessError):
                fixtures.kubectl("delete", "nodeprofile", "batch")
        self.assertEqual(command.call_count, 2)
        with patch.object(fixtures, "_kubectl_once",
                          side_effect=failed("Error from server (Forbidden)")) as command:
            with self.assertRaises(fixtures.subprocess.CalledProcessError):
                fixtures.kubectl("get", "nodes")
        self.assertEqual(command.call_count, 1)

    def test_retries_are_bounded(self):
        with patch.object(fixtures, "_kubectl_once", side_effect=failed(self.RESET)) as command, \
             patch("builtins.print"):
            with self.assertRaises(fixtures.subprocess.CalledProcessError):
                fixtures.kubectl("get", "nodes")
        self.assertEqual(command.call_count, 4)
        self.assertEqual(self.sleep.call_count, 3)

    def test_teardown_reruns_after_reset_and_rechecks_state(self):
        # The delete was applied server-side but its response was lost; the
        # re-run finds the profile gone and verifies no claims remain.
        passes = []

        def once(namespace, name, uid, image):
            passes.append(name)
            if len(passes) == 1:
                raise failed(self.RESET)
            fixtures.released(namespace, name, uid)

        nodes = {"items": [{"metadata": {"name": "worker", "labels": {}}}]}
        with patch.object(fixtures, "_teardown_once", side_effect=once), \
             patch.object(fixtures, "kubectl", return_value=""), \
             patch.object(fixtures, "get", side_effect=lambda kind, *a:
                          nodes if kind == "nodes" else {"items": []}), \
             patch("builtins.print"):
            fixtures.teardown("brewlet", "batch", "owner", TeardownTest.image)
        self.assertEqual(len(passes), 2)

    def test_released_detects_retained_claims_and_profiles(self):
        claimed = {"items": [{"metadata": {"name": "worker", "labels": {fixtures.OWNER: "owner"}}}]}
        with patch.object(fixtures, "kubectl", return_value=""), \
             patch.object(fixtures, "get", side_effect=lambda kind, *a:
                          claimed if kind == "nodes" else {"items": []}):
            with self.assertRaisesRegex(AssertionError, "retains the claim"):
                fixtures.released("brewlet", "batch", "owner")
        with patch.object(fixtures, "kubectl", return_value='{"metadata":{"uid":"owner"}}'), \
             patch.object(fixtures, "get", return_value={"items": []}):
            with self.assertRaisesRegex(AssertionError, "still exists"):
                fixtures.released("brewlet", "batch", "owner")
        owned = {"items": [{"metadata": {"ownerReferences": [{"uid": "owner"}]}}]}
        with patch.object(fixtures, "kubectl", return_value=""), \
             patch.object(fixtures, "get", side_effect=lambda kind, *a:
                          owned if kind == "daemonsets,pods" else {"items": []}):
            with self.assertRaisesRegex(AssertionError, "worker owned"):
                fixtures.released("brewlet", "batch", "owner")
        with patch.object(fixtures, "kubectl", return_value='{"metadata":{"uid":"replacement"}}'), \
             patch.object(fixtures, "get", return_value={"items": []}):
            fixtures.released("brewlet", "batch", "owner")


if __name__ == "__main__":
    unittest.main()
