"""Check the reader tutorial observer offline."""

import importlib.util
import json
from pathlib import Path
import unittest
from unittest.mock import patch
from datetime import datetime, timedelta, timezone

ROOT = Path(__file__).resolve().parents[2]


class TutorialObservationTests(unittest.TestCase):
    def setUp(self):
        spec = importlib.util.spec_from_file_location("reader_observe", ROOT / "examples/local/observe.py")
        self.module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.module)
        self.environment = patch.dict("os.environ", {
            "NODE": "private-node", "NODE_UID": "node-uid", "RID": "python-test",
            "RUNTIME_UID": "inventory-uid", "POD": "actual-pod", "APP_NAMESPACE": "private-app"})
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.binding = {"podUID": "pod-uid", "id": "python-test", "inventoryUID": "inventory-uid",
                        "digest": "sha256:" + "a" * 64, "state": "running", "generation": 2}
        self.observation = {"complete": True, "nodeUID": "node-uid",
                            "observedAt": datetime.now(timezone.utc).isoformat(),
                            "bindings": [self.binding]}
        self.image = {"metadata": {"uid": "inventory-uid", "generation": 3},
                      "status": {"digest": self.binding["digest"], "generation": 2,
                                 "rotation": {"participants": [{"uid": "node-uid"}]},
                                 "conditions": [{"type": "RotationComplete", "status": "True",
                                                 "observedGeneration": 3}]}}

    def observed(self, mode="binding"):
        node = {"metadata": {"uid": "node-uid", "annotations": {
            "brewlet.sh/runtime-bindings": json.dumps(self.observation)}}}
        with patch.object(self.module, "get", side_effect=[
                node, self.image, *([{"metadata": {"uid": "pod-uid"}}] if mode == "binding" else [])]):
            return self.module.observe(mode, "example.invalid/runtime@" + self.binding["digest"])

    def test_actual_identity_and_fresh_complete_binding_required(self):
        self.assertEqual(self.observed(), self.binding)
        self.observation["complete"] = False
        self.assertIsNone(self.observed())

    def test_stale_observation_is_not_a_positive_binding(self):
        self.observation["observedAt"] = (datetime.now(timezone.utc) - timedelta(minutes=5)).isoformat()
        self.assertIsNone(self.observed())

    def test_inventory_replacement_fails_instead_of_adopting_new_epoch(self):
        self.image["metadata"]["uid"] = "replacement"
        with self.assertRaisesRegex(RuntimeError, "inventory UID changed"):
            self.observed()

    def test_rotation_requires_current_condition_generation_and_actual_node_participation(self):
        self.assertEqual(self.observed("rotation")["generation"], 2)
        self.image["status"]["conditions"][0]["observedGeneration"] = 2
        self.assertIsNone(self.observed("rotation"))
        self.image["status"]["conditions"][0]["observedGeneration"] = 3
        self.image["status"]["rotation"]["participants"] = []
        self.assertIsNone(self.observed("rotation"))

    def test_webhook_retry_is_only_for_this_service_transport_not_denials_or_tls(self):
        error = 'failed calling webhook "runtimeimages.brewlet.sh": https://brewlet-admission.brewlet.svc: connect: connection refused'
        self.assertTrue(self.module.webhook_pending(error))
        self.assertFalse(self.module.webhook_pending(error.replace("brewlet-admission.brewlet.svc", "foreign.svc")))
        self.assertFalse(self.module.webhook_pending(error + " x509 certificate error"))
        self.assertFalse(self.module.webhook_pending('runtimeimages.brewlet.sh denied the request'))


if __name__ == "__main__":
    unittest.main()
