"""Offline native-pool enrollment guards, not scheduling or autoscaler proof."""

import copy
from types import SimpleNamespace
import unittest

from harness import FENCE, Failure
from native_capacity import enrollment_patch


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


if __name__ == "__main__":
    unittest.main()
