import json
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import Mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import harness


class NativeAdmissionTests(unittest.TestCase):
    def result(self, terms):
        return subprocess.CompletedProcess([], 0, json.dumps({
            "spec": {"affinity": {"nodeAffinity": {
                "requiredDuringSchedulingIgnoredDuringExecution": {
                    "nodeSelectorTerms": terms
                }
            }}}
        }), "")

    def test_preserved_empty_selector(self):
        self.assertEqual("preserved-unsatisfiable",
                         harness.empty_selector_outcome(self.result([])))

    def test_native_rejection_is_not_claimed_admission_success(self):
        result = subprocess.CompletedProcess([], 1, "",
            "spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution."
            "nodeSelectorTerms: Required value: must have at least one node selector term")
        self.assertEqual("preserved-and-rejected-by-native-api-validation",
                         harness.empty_selector_outcome(result))

    def test_unexpected_denial_cannot_pass(self):
        with self.assertRaises(harness.Failure):
            harness.empty_selector_outcome(subprocess.CompletedProcess([], 1, "", "Forbidden"))

    def test_added_or_alternative_cannot_pass(self):
        with self.assertRaises(harness.Failure):
            harness.empty_selector_outcome(self.result([{"matchExpressions": []}]))


class StartupSchedulingTests(unittest.TestCase):
    def setUp(self):
        self.node = {"spec": {"taints": [
            {"key": harness.FENCE, "value": "provisioning", "effect": "NoSchedule"}
        ]}}
        self.pod = {"metadata": {"uid": "owned-pod"}, "spec": {}}
        self.events = [{"reason": "FailedScheduling", "involvedObject": {"uid": "owned-pod"},
                        "message": "0/1 nodes are available: 1 node(s) had untolerated taint(s)."}]

    def test_native_summary_without_taint_key_is_validated_against_actual_node(self):
        self.assertEqual([self.events[0]["message"]],
                         harness.startup_taint_evidence(self.node, self.pod, self.events))

    def test_other_no_schedule_taint_cannot_establish_startup_proof(self):
        self.node["spec"]["taints"].append({"key": "other", "effect": "NoSchedule"})
        with self.assertRaises(harness.Failure):
            harness.startup_taint_evidence(self.node, self.pod, self.events)

    def test_foreign_pod_event_cannot_establish_startup_proof(self):
        self.events[0]["involvedObject"]["uid"] = "foreign-pod"
        self.assertEqual([], harness.startup_taint_evidence(self.node, self.pod, self.events))

    def test_resource_failure_cannot_establish_startup_proof(self):
        self.events[0]["message"] = "Insufficient cpu"
        self.assertEqual([], harness.startup_taint_evidence(self.node, self.pod, self.events))

    def test_scheduled_pod_cannot_establish_startup_proof(self):
        self.pod["spec"]["nodeName"] = "node"
        with self.assertRaises(harness.Failure):
            harness.startup_taint_evidence(self.node, self.pod, self.events)


class WebhookReadinessTests(unittest.TestCase):
    def harness(self, results):
        instance = harness.Harness.__new__(harness.Harness)
        instance.run_id = "0123456789ab"
        instance.kubectl = Mock(side_effect=results)
        instance.wait = lambda description, predicate, timeout: next(
            result for _ in range(3) if (result := predicate()))
        return instance

    def test_dry_run_waits_for_service_transport_then_real_success(self):
        instance = self.harness([
            subprocess.CompletedProcess([], 1, "", "connect: connection refused"),
            subprocess.CompletedProcess([], 0, "created (server dry run)", ""),
        ])
        instance.wait_inventory_admission("registry.test/runtime@sha256:" + "a" * 64)
        self.assertEqual(2, instance.kubectl.call_count)
        self.assertIn("--dry-run=server", instance.kubectl.call_args.args)

    def test_validation_or_tls_identity_failure_is_not_retried_as_success(self):
        for message in ("Forbidden", "x509: certificate signed by unknown authority",
                        "RuntimeImage source is invalid"):
            instance = self.harness([subprocess.CompletedProcess([], 1, "", message)])
            with self.assertRaises(harness.Failure):
                instance.wait_inventory_admission("registry.test/runtime@sha256:" + "a" * 64)
            self.assertEqual(1, instance.kubectl.call_count)

if __name__ == "__main__":
    unittest.main()
