"""Offline native-pool enrollment guards, not scheduling or autoscaler proof."""

import copy
import json
from types import SimpleNamespace
import unittest

from harness import FENCE, OWNER, Failure
from native_capacity import enrollment_patch, observe_startup_fence


class NativeEnrollmentTests(unittest.TestCase):
    def fixture(self):
        h = SimpleNamespace(node_uids={"owned-worker": "original-node"}, arch="arm64",
                            run_id="123456abcdef")
        node = {"metadata": {"name": "owned-worker", "uid": "original-node",
                             "resourceVersion": "37",
                             "labels": {"kubernetes.io/arch": "arm64", "kubernetes.io/os": "linux"}},
                "status": {"nodeInfo": {"architecture": "arm64", "operatingSystem": "linux"}},
                "spec": {"taints": [{"key": FENCE, "value": "provisioning", "effect": "NoSchedule"}]}}
        return h, node

    def test_enrollment_never_changes_hardware_architecture_or_boot_fence(self):
        h, node = self.fixture()
        original = copy.deepcopy(node)
        patch = enrollment_patch(h, node, "native-pool", "native-id")
        self.assertEqual(node, original)
        self.assertEqual(patch["metadata"]["uid"], "original-node")
        self.assertEqual(patch["metadata"]["resourceVersion"], "37")
        labels = patch["metadata"]["labels"]
        self.assertEqual(labels["brewlet.test/pool"], "native-pool")
        self.assertEqual(labels["brewlet.sh/runtime.native-id"], "available")
        self.assertNotIn("kubernetes.io/arch", labels)
        self.assertNotIn("kubernetes.io/os", labels)
        self.assertNotIn("spec", patch)

    def test_recreated_foreign_advertised_unfenced_or_relabelled_worker_is_refused(self):
        for mutation in ("uid", "foreign", "history", "unfenced", "architecture", "label", "os"):
            with self.subTest(mutation=mutation):
                h, node = self.fixture()
                if mutation == "uid":
                    node["metadata"]["uid"] = "replacement"
                elif mutation == "foreign":
                    node["metadata"]["labels"]["brewlet.sh/owner-uid"] = "foreign"
                elif mutation == "history":
                    node["metadata"]["annotations"] = {"brewlet.sh/runtime-images": "{}"}
                elif mutation == "unfenced":
                    node["spec"]["taints"] = []
                elif mutation == "architecture":
                    node["status"]["nodeInfo"]["architecture"] = "amd64"
                elif mutation == "label":
                    node["metadata"]["labels"]["kubernetes.io/arch"] = "amd64"
                else:
                    node["status"]["nodeInfo"]["operatingSystem"] = "windows"
                with self.assertRaises(Failure):
                    enrollment_patch(h, node, "native-pool", "native-id")

    def observation_fixture(self, *, fail=False, paused=False):
        h, node = self.fixture()
        h.namespace = "native-test"
        container = "b" * 64
        h.node_containers = {"owned-worker": container}
        state, calls = {"Running": True, "Paused": paused}, []
        pod = {"metadata": {"uid": "native-pod", "labels": {OWNER: h.run_id}}, "spec": {}}

        def run(command, **kwargs):
            calls.append(command)
            if command[:2] == ["docker", "inspect"]:
                return SimpleNamespace(stdout=json.dumps([{"Id": container,
                    "Config": {"Labels": {"io.x-k8s.kind.cluster": "brewlet-v2-" + h.run_id}},
                    "State": state}]))
            state["Paused"] = command[1] == "pause"
            return SimpleNamespace(stdout="")

        def get(kind, *args, **kwargs):
            if kind == "node":
                return node
            if kind == "pod":
                return pod
            return {"items": [{"involvedObject": {"uid": "native-pod"},
                               "reason": "FailedScheduling",
                               "message": "1 node(s) had untolerated taint(s)"}]}

        def wait(description, predicate, timeout):
            if fail:
                raise Failure("scheduler observation failed")
            return predicate()

        h.run, h.get, h.wait = run, get, wait
        h.kubectl = lambda *args, **kwargs: SimpleNamespace(stdout=json.dumps(node))
        return h, pod, state, calls

    def test_generic_scheduler_taint_message_uses_actual_fence_and_worker_always_resumes(self):
        h, pod, state, calls = self.observation_fixture()
        proof = observe_startup_fence(h, "owned-worker", "native-pool", "native-id", pod)
        self.assertTrue(proof)
        self.assertFalse(state["Paused"])
        self.assertEqual([call[1] for call in calls if call[1] != "inspect"], ["pause", "unpause"])
        h, pod, state, calls = self.observation_fixture(fail=True)
        with self.assertRaisesRegex(Failure, "scheduler observation failed"):
            observe_startup_fence(h, "owned-worker", "native-pool", "native-id", pod)
        self.assertFalse(state["Paused"])
        self.assertEqual([call[1] for call in calls if call[1] != "inspect"], ["pause", "unpause"])

    def test_preexisting_user_pause_is_not_undone(self):
        h, pod, state, calls = self.observation_fixture(paused=True)
        with self.assertRaisesRegex(Failure, "not running normally"):
            observe_startup_fence(h, "owned-worker", "native-pool", "native-id", pod)
        self.assertTrue(state["Paused"])
        self.assertFalse(any(call[1] != "inspect" for call in calls))


if __name__ == "__main__":
    unittest.main()
