import json
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import Mock
import interceptor

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


class RealStartEvidenceTests(unittest.TestCase):
    def test_assigned_container_id_with_epoch_start_error_is_not_execution(self):
        self.assertFalse(harness.application_started({
            "containerID": "containerd://allocated-before-start",
            "state": {"waiting": {"reason": "CrashLoopBackOff"}},
            "lastState": {"terminated": {"reason": "StartError",
                                        "startedAt": "1970-01-01T00:00:00Z"}},
        }))

    def test_running_or_real_start_time_cannot_pass_negative_start_test(self):
        for status in (
            {"state": {"running": {}}},
            {"started": True},
            {"lastState": {"terminated": {"startedAt": "2026-10-16T16:00:00Z"}}},
        ):
            self.assertTrue(harness.application_started(status))


class ProviderRefusalWireTests(unittest.TestCase):
    def test_actual_webhook_denial_has_provider_forbidden_api_status(self):
        response = interceptor.review_response("request-uid", False, "provider-pinned label refusal")["response"]
        self.assertEqual("request-uid", response["uid"])
        self.assertFalse(response["allowed"])
        self.assertEqual(403, response["status"]["code"])
        self.assertEqual("Forbidden", response["status"]["reason"])


class WebhookDiscoveryTests(unittest.TestCase):
    def setUp(self):
        self.instance = harness.Harness.__new__(harness.Harness)
        self.instance.cluster = "brewlet-v2-0123456789ab"
        self.instance.control_namespace = self.instance.cluster + "-system"
        self.instance.namespace = self.instance.cluster
        self.webhook = {
            "metadata": {"name": "brewlet-pods", "uid": "owned-webhook-uid", "resourceVersion": "1",
                         "annotations": {"meta.helm.sh/release-name": self.instance.cluster,
                                         "meta.helm.sh/release-namespace": self.instance.control_namespace},
                         "labels": {"app.kubernetes.io/managed-by": "Helm"}},
            "webhooks": [{"clientConfig": {"service": {
                "namespace": self.instance.control_namespace, "path": "/mutate-pods"}},
                "namespaceSelector": {"matchLabels": {"existing": "preserved"}}}],
        }
        self.instance.get = Mock(return_value={"items": [self.webhook]})
        self.instance.kubectl = Mock()

    def test_actual_chart_name_is_discovered_and_existing_selector_preserved(self):
        self.instance.bypass_admission()
        value = json.loads(self.instance.kubectl.call_args.kwargs["input"])
        self.assertEqual("brewlet-pods", value["metadata"]["name"])
        self.assertEqual({"existing": "preserved"}, value["webhooks"][0]["namespaceSelector"]["matchLabels"])
        self.assertEqual([self.instance.namespace],
                         value["webhooks"][0]["namespaceSelector"]["matchExpressions"][-1]["values"])

    def test_foreign_release_or_replaced_webhook_cannot_be_patched(self):
        self.webhook["metadata"]["annotations"]["meta.helm.sh/release-namespace"] = "foreign"
        with self.assertRaises(harness.Failure):
            self.instance.bypass_admission()
        self.instance.kubectl.assert_not_called()

    def test_restore_refuses_recreated_name(self):
        self.instance.bypass_admission()
        self.instance.get = Mock(return_value={
            "metadata": {"name": "brewlet-pods", "uid": "replacement"},
        })
        self.instance.kubectl.reset_mock()
        with self.assertRaises(harness.Failure):
            self.instance.restore_admission()
        self.instance.kubectl.assert_not_called()

if __name__ == "__main__":
    unittest.main()
