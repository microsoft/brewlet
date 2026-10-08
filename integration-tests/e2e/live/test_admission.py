#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""CI-discoverable offline safeguards; not a replacement for the live scenario."""

import copy
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch
from types import SimpleNamespace
from urllib.error import HTTPError

from admission_helpers import (
    ARTIFACT, BUILDER, VERIFIER, assert_admission, assert_candidates,
    assert_discoverable, assert_rejection_reasons, extract_result, variant_statement, Registry,
)


CURRENT = "sha256:" + "1" * 64
OBSOLETE = "sha256:" + "2" * 64
IMAGE = "fixture.invalid/app@" + "sha256:" + "3" * 64


def completed(code, message):
    return subprocess.CompletedProcess(["kubectl"], code, "", message)


def report(candidates, allowed):
    return {
        "isSuccess": allowed,
        "verifierReports": [
            {"artifactType": ARTIFACT, "referenceDigest": reference,
             "verifierReports": [
                 {"verifierName": VERIFIER, "isSuccess": success,
                  "message": "verified" if success else "verification failed",
                  "extensions": {"finalImageDigest": IMAGE.split("@")[1]} if success else {},
                  "errorReason": "" if success else "invalid signature"}]}
            for reference, success in candidates.items()],
    }


class AdmissionClassificationTests(unittest.TestCase):
    def test_node_mutations_use_verified_id_and_isolated_runner(self):
        from admission import Admission
        fixture = SimpleNamespace(
            registry="localhost:5000", node="owned-node-name", node_id="captured-node-id",
            registry_id="captured-registry-id", registry_internal="owned-registry:5000",
            registry_ip="172.18.0.2", record=Mock())
        verified = []
        fixture.own_container = Mock(side_effect=lambda value: verified.append(value))

        def mutate(argv, **_kwargs):
            self.assertIn(fixture.node, verified)
            self.assertIn("captured-node-id", argv)
            self.assertNotIn("owned-node-name", argv)
        fixture.run = Mock(side_effect=mutate)
        with patch("admission.native_registry", return_value=("config", "credentials", "storage")):
            Admission(fixture).setup_registry_route()
        self.assertEqual(fixture.run.call_count, 2)

    def test_excluded_namespace_requests_satisfy_restricted_pod_security(self):
        from admission import Admission
        fixture = SimpleNamespace(registry="localhost:5000", namespace="live-e2e")
        pod = Admission(fixture).pod("excluded", IMAGE, namespace="gatekeeper-system")
        self.assertTrue(pod["spec"]["securityContext"]["runAsNonRoot"])
        self.assertEqual(pod["spec"]["securityContext"]["seccompProfile"]["type"], "RuntimeDefault")
        security = pod["spec"]["containers"][0]["securityContext"]
        self.assertFalse(security["allowPrivilegeEscalation"])
        self.assertEqual(security["capabilities"]["drop"], ["ALL"])

    def test_explicit_success(self):
        self.assertTrue(assert_admission(completed(0, "pod created (server dry run)"), True)["allowed"])

    def test_only_the_named_brewlet_policy_denial_counts(self):
        denial = ('Error from server (Forbidden): admission webhook '
                  '"validation.gatekeeper.sh" denied the request: '
                  '[brewlet-managed-dependencies] Brewlet admission: image has no successful response')
        self.assertFalse(assert_admission(completed(1, denial), False)["allowed"])
        for message in (
            "pod is invalid: Forbidden",
            'admission webhook "validation.gatekeeper.sh" denied the request: [another-policy] denial',
            'failed calling webhook "validation.gatekeeper.sh": connection refused',
            'admission webhook "brewlet.sh" denied the request',
            "FailedScheduling: runtimeclass not found",
            denial.replace("Brewlet admission:", "unrelated error:"),
        ):
            with self.subTest(message=message), self.assertRaises(AssertionError):
                assert_admission(completed(1, message), False)
        with self.assertRaises(AssertionError):
            assert_admission(completed(0, denial), False)

    def test_denial_never_counts_as_admission(self):
        with self.assertRaises(AssertionError):
            assert_admission(completed(1, "API failure"), True)


class CandidateEvidenceTests(unittest.TestCase):
    def test_rotation_requires_each_actual_candidate(self):
        assert_candidates(report({CURRENT: True}, True), {CURRENT: True}, True)
        assert_candidates(report({CURRENT: True, OBSOLETE: False}, True),
                          {CURRENT: True, OBSOLETE: False}, True)
        assert_candidates(report({OBSOLETE: False}, False), {OBSOLETE: False}, False)

    def test_cached_current_only_cannot_prove_rotation(self):
        with self.assertRaises(AssertionError):
            assert_candidates(report({CURRENT: True}, True),
                              {CURRENT: True, OBSOLETE: False}, True)

    def test_success_cannot_hide_wrong_or_missing_candidate(self):
        for candidates in ({}, {OBSOLETE: True}, {CURRENT: False}):
            with self.subTest(candidates=candidates), self.assertRaises(AssertionError):
                assert_candidates(report(candidates, True), {CURRENT: True}, True)

    def test_foreign_verifier_cannot_substitute(self):
        result = report({CURRENT: True}, True)
        result["verifierReports"][0]["verifierReports"][0]["verifierName"] = "admission-other"
        with self.assertRaises(AssertionError):
            assert_candidates(result, {CURRENT: True}, True)

    def test_other_verifier_must_really_execute(self):
        result = report({CURRENT: False}, False)
        with self.assertRaises(AssertionError):
            assert_candidates(result, {CURRENT: False}, False, other=True)
        result["verifierReports"][0]["verifierReports"].append(
            {"verifierName": "admission-other", "isSuccess": True})
        assert_candidates(result, {CURRENT: False}, False, other=True)

    def test_ambiguous_duplicate_report_rejected(self):
        result = report({CURRENT: True}, True)
        result["verifierReports"].append(copy.deepcopy(result["verifierReports"][0]))
        with self.assertRaises(AssertionError):
            assert_candidates(result, {CURRENT: True}, True)

    def test_decision_must_be_explicit_boolean(self):
        for decision in (None, 1, "true", False):
            result = report({CURRENT: True}, decision)
            with self.subTest(decision=decision), self.assertRaises(AssertionError):
                assert_candidates(result, {CURRENT: True}, True)

    def test_missing_evidence_requires_empty_reports(self):
        assert_candidates(report({}, False), {}, False)
        with self.assertRaises(AssertionError):
            assert_candidates(report({CURRENT: False}, False), {}, False)

    def test_failure_must_have_real_message(self):
        result = report({CURRENT: False}, False)
        del result["verifierReports"][0]["verifierReports"][0]["message"]
        with self.assertRaises(AssertionError):
            assert_candidates(result, {CURRENT: False}, False)

    def test_success_must_show_bound_digest(self):
        result = report({CURRENT: True}, True)
        result["verifierReports"][0]["verifierReports"][0]["extensions"] = {}
        with self.assertRaises(AssertionError):
            assert_candidates(result, {CURRENT: True}, True)

    def test_success_cannot_bind_a_different_image(self):
        result = report({CURRENT: True}, True)
        with self.assertRaises(AssertionError):
            assert_candidates(result, {CURRENT: True}, True, subject_digest=CURRENT)
        assert_candidates(result, {CURRENT: True}, True, subject_digest=IMAGE.split("@")[1])

    def test_fetch_failure_cannot_masquerade_as_signature_rejection(self):
        result = report({CURRENT: False}, False)
        reports = assert_candidates(result, {CURRENT: False}, False)
        assert_rejection_reasons(reports, {CURRENT: "invalid signature"})
        reports[CURRENT]["verifierReports"][0]["errorReason"] = "fetch DSSE envelope blob: 404"
        with self.assertRaises(AssertionError):
            assert_rejection_reasons(reports, {CURRENT: "invalid signature"})


class ProviderResponseTests(unittest.TestCase):
    def document(self):
        return {"response": {"items": [{"key": IMAGE, "value": report({CURRENT: True}, True)}]}}

    def test_exact_image_response(self):
        self.assertTrue(extract_result(self.document(), IMAGE)["isSuccess"])

    def test_missing_partial_mismatched_or_failed_response_rejected(self):
        for response in [
            {},
            {"response": {"items": []}},
            {"response": {"systemError": "connection refused"}},
            {"response": {"items": [{"key": "different", "value": {"isSuccess": True}}]}},
            {"response": {"items": [{"key": IMAGE, "error": "unauthorized"}]}},
            {"response": {"items": [{"key": IMAGE, "value": {}}]}},
            {"response": {"items": [{"key": IMAGE, "value": {"isSuccess": "true"}}]}},
        ]:
            with self.subTest(response=response), self.assertRaises(AssertionError):
                extract_result(response, IMAGE)

    def test_duplicate_key_response_rejected(self):
        response = self.document()
        response["response"]["items"] *= 2
        with self.assertRaises(AssertionError):
            extract_result(response, IMAGE)


class RegistryDiscoveryTests(unittest.TestCase):
    def test_registry_without_native_api_is_actionable_failure(self):
        registry = Registry(SimpleNamespace(registry="localhost:5000"))
        with patch.object(registry, "request", side_effect=HTTPError(
                "http://localhost:5000", 404, "Not Found", {}, None)):
            with self.assertRaisesRegex(RuntimeError, "requires the native OCI 1.1 Referrers API"):
                registry.referrers("fixture", CURRENT)

    def test_native_negative_candidates_must_remain_visible(self):
        index = {"manifests": [{"digest": CURRENT, "artifactType": ARTIFACT}]}
        assert_discoverable(index, {CURRENT: False})
        for document in ({}, {"manifests": []},
                         {"manifests": [{"digest": CURRENT, "artifactType": "other"}]}):
            with self.subTest(document=document), self.assertRaises(AssertionError):
                assert_discoverable(document, {CURRENT: False})

    def test_discovery_alone_cannot_prove_wrong_candidate(self):
        index = {"manifests": [{"digest": OBSOLETE, "artifactType": ARTIFACT}]}
        with self.assertRaises(AssertionError):
            assert_discoverable(index, {CURRENT: False})

    def test_duplicate_candidates_rejected(self):
        index = {"manifests": [{"digest": CURRENT, "artifactType": ARTIFACT}] * 2}
        with self.assertRaises(AssertionError):
            assert_discoverable(index, {CURRENT: False})


class DiagnosticCaptureTests(unittest.TestCase):
    def fixture(self):
        return SimpleNamespace(
            registry="localhost:5000", registry_id="owned-registry",
            source_revision="checkout-commit", plugin_version="9.9.9-test",
            record=Mock(), save=Mock(), own_container=Mock(),
            run=Mock(return_value=completed(0, "")),
            kube=Mock(return_value=completed(0, "")))

    def test_capture_continues_and_reports_every_failure(self):
        from admission import Admission
        fixture = self.fixture()
        fixture.kube.side_effect = [
            RuntimeError("Ratify logs unreachable"), completed(3, "Gatekeeper logs unavailable"),
            *[completed(0, "") for _ in range(5)]]
        fixture.own_container.side_effect = RuntimeError("registry identity changed")
        with patch("sys.stderr"):
            errors = Admission(fixture).diagnostics()
        self.assertEqual(fixture.kube.call_count, 7)
        self.assertEqual({error["surface"] for error in errors}, {"ratify", "gatekeeper", "registry"})
        fixture.run.assert_not_called()
        fixture.save.assert_any_call("admission-diagnostics-result.json",
                                     {"complete": False, "errors": errors})

    def stub_execution(self):
        from admission import Admission
        admission = Admission(self.fixture())
        for phase in ("setup_registry_route", "keys", "publish", "build_ratify",
                      "install_gatekeeper", "install_ratify", "policies",
                      "assert_enforcement_ready", "positive", "negatives", "rotation",
                      "competing_verifier", "api_paths", "fetch_failure",
                      "authentication_failure", "unavailable_provider"):
            setattr(admission, phase, Mock())
        admission.diagnostics = Mock(return_value=[{"surface": "events", "error": "unavailable"}])
        return admission

    def test_diagnostics_failure_cannot_make_green_run(self):
        admission = self.stub_execution()
        with self.assertRaisesRegex(RuntimeError, "Required admission diagnostics/cleanup failed"):
            admission.execute()

    def test_diagnostics_failure_preserves_original_failure(self):
        admission = self.stub_execution()
        admission.positive.side_effect = ValueError("original admission failure")
        with self.assertRaisesRegex(ValueError, "original admission failure"):
            admission.execute()
        admission.diagnostics.assert_called_once()

class StatementFixtureTests(unittest.TestCase):
    def test_mutations_preserve_original_and_bind_new_subject(self):
        original = {"subject": [{"digest": {"sha256": "0" * 64}}],
                    "predicate": {"finalImageDigest": "sha256:" + "0" * 64,
                                  "builderIdentity": BUILDER, "applicationJarDigest": CURRENT}}
        before = copy.deepcopy(original)
        subject = {"digest": OBSOLETE}
        valid = variant_statement(original, subject, "valid")
        self.assertEqual(valid["predicate"]["finalImageDigest"], OBSOLETE)
        self.assertEqual(valid["subject"][0]["digest"]["sha256"], "2" * 64)
        self.assertNotIn("applicationJarDigest",
                         variant_statement(original, subject, "incomplete")["predicate"])
        self.assertNotEqual(BUILDER,
                            variant_statement(original, subject, "wrong-builder")["predicate"]["builderIdentity"])
        self.assertEqual(original, before)


class GatekeeperReadinessTests(unittest.TestCase):
    def admission(self):
        from admission import Admission
        f = Mock(namespace="live-e2e")
        f.registry = "localhost:5000"
        return Admission(f)

    def test_success_requires_server_side_dry_run_without_persisting_probe(self):
        a = self.admission()
        a.f.kube.return_value = completed(0, "")
        self.assertTrue(a.gatekeeper_admission_ready())
        self.assertEqual(a.f.kube.call_args.args, ("create", "--dry-run=server", "-f", "-"))
        probe = json.loads(a.f.kube.call_args.kwargs["input"])
        self.assertEqual(probe["kind"], "ConfigMap")
        self.assertEqual(probe["metadata"]["namespace"], "live-e2e")
        a.f.save.assert_called_once()

    def test_only_named_gatekeeper_startup_transport_failures_are_retried(self):
        a = self.admission()
        for name in ("validation.gatekeeper.sh", "mutation.gatekeeper.sh"):
            for error in ("connection refused",
                          'no endpoints available for service "gatekeeper-webhook-service"'):
                a.f.kube.return_value = completed(1, f'failed calling webhook "{name}": {error}')
                with self.subTest(name=name, error=error):
                    self.assertFalse(a.gatekeeper_admission_ready())
        for message in (
            'admission webhook "validation.gatekeeper.sh" denied the request',
            'failed calling webhook "unrelated.sh": connection refused',
            'failed calling webhook "validation.gatekeeper.sh": x509: unknown authority',
            'failed calling webhook "validation.gatekeeper.sh": context deadline exceeded',
            "Forbidden: cannot create configmaps",
            "connection refused",
        ):
            a.f.kube.return_value = completed(1, message)
            with self.subTest(message=message), self.assertRaisesRegex(RuntimeError, "readiness failed"):
                a.gatekeeper_admission_ready()

    def test_startup_gate_preserves_health_probes_and_fail_closed_policy(self):
        a = self.admission()
        with tempfile.TemporaryDirectory() as directory:
            a.f.private = Path(directory)
            (a.f.private / "admission-gatekeeper.yaml").write_text("replicas: 3\n")
            a.f.get.return_value = {"webhooks": [
                {"name": "validation.gatekeeper.sh", "failurePolicy": "Ignore"},
                {"name": "another-webhook", "failurePolicy": "Ignore"},
            ]}
            with patch("admission.download", return_value="checksum"), \
                    patch("admission.wait") as ready:
                a.install_gatekeeper()
        calls = a.f.kube.call_args_list
        self.assertEqual(calls[1].args[:-1], (
            "patch", "deployment", "gatekeeper-controller-manager",
            "-n", "gatekeeper-system", "--type=strategic", "-p"))
        container = json.loads(calls[1].args[-1])["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(container, {
            "name": "manager", "startupProbe": {
                "tcpSocket": {"port": "webhook-server"}, "periodSeconds": 1, "failureThreshold": 120},
        })
        self.assertIn("rollout", calls[2].args)
        self.assertEqual(a.f.apply.call_args.args[0]["webhooks"], [
            {"name": "validation.gatekeeper.sh", "failurePolicy": "Fail", "timeoutSeconds": 30},
            {"name": "another-webhook", "failurePolicy": "Ignore"},
        ])
        ready.assert_called_once_with(
            "Gatekeeper admits through the API server", a.gatekeeper_admission_ready,
            timeout=120, interval=1)
        self.assertLess([c[0] for c in a.f.mock_calls].index("apply"),
                        [c[0] for c in a.f.mock_calls].index("record"))

    def test_readiness_failure_never_records_success(self):
        a = self.admission()
        with tempfile.TemporaryDirectory() as directory:
            a.f.private = Path(directory)
            (a.f.private / "admission-gatekeeper.yaml").write_text("replicas: 3\n")
            a.f.get.return_value = {"webhooks": [{"name": "validation.gatekeeper.sh"}]}
            with patch("admission.download"), \
                    patch("admission.wait", side_effect=TimeoutError("listener never served")), \
                    self.assertRaisesRegex(TimeoutError, "never served"):
                a.install_gatekeeper()
        a.f.record.assert_not_called()


class RatifyInstallationTests(unittest.TestCase):
    def setUp(self):
        from admission import Admission
        private = tempfile.TemporaryDirectory()
        self.addCleanup(private.cleanup)
        self.fixture = SimpleNamespace(
            registry="localhost:5000", registry_internal="owned-registry:5000",
            private=Path(private.name), kubeconfig=Path(private.name) / "kubeconfig",
            context="kind-owned", kube=Mock(), record=Mock(),
            get=Mock(side_effect=AssertionError("must not fetch a Deployment snapshot")),
            apply=Mock(side_effect=AssertionError("must not reapply a Deployment snapshot")))
        self.admission = Admission(self.fixture)
        self.admission.baked_repository = "brewlet.local/ratify"
        self.admission.baked_digest = CURRENT
        self.admission.baked_image = "brewlet.local/ratify@" + CURRENT
        self.admission.registry_ip = "172.18.0.2"
        self.admission.certificates = Mock(return_value={})
        self.admission.command = Mock()
        for target, kwargs in [
            ("admission.download", {"return_value": "archive-checksum"}),
            ("admission.tarfile.open", {}),
            ("admission.wait", {}),
        ]:
            patcher = patch(target, **kwargs)
            mocked = patcher.start()
            self.addCleanup(patcher.stop)
            if target == "admission.wait":
                self.wait = mocked

    def test_install_patches_only_intended_spec_fields_then_waits(self):
        self.admission.install_ratify()
        calls = self.fixture.kube.call_args_list
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0].args[:-1], (
            "patch", "deployment", "ratify", "-n", "ratify-service",
            "--type=strategic", "-p"))
        self.assertEqual(json.loads(calls[0].args[-1]), {
            "spec": {
                "strategy": {"type": "Recreate", "rollingUpdate": None},
                "template": {"spec": {
                    "containers": [{"name": "ratify", "image": self.admission.baked_image}],
                    "hostAliases": [{"ip": "172.18.0.2", "hostnames": ["owned-registry"]}],
                }},
            },
        })
        self.assertEqual(calls[1].args, (
            "-n", "ratify-service", "rollout", "status", "deployment/ratify",
            "--timeout=240s"))
        self.wait.assert_called_once_with(
            "Ratify current Pod and Service endpoints agree",
            self.admission.ratify_endpoints_ready, timeout=120, interval=1)
        self.fixture.record.assert_called_once()
        self.fixture.get.assert_not_called()
        self.fixture.apply.assert_not_called()

    def test_patch_failure_aborts_before_readiness_or_success(self):
        self.fixture.kube.side_effect = RuntimeError("deployment patch failed")
        with self.assertRaisesRegex(RuntimeError, "deployment patch failed"):
            self.admission.install_ratify()
        self.fixture.kube.assert_called_once()
        self.wait.assert_not_called()
        self.fixture.record.assert_not_called()


class RatifyReadinessTests(unittest.TestCase):
    def pod(self, deleting=False):
        return {"metadata": {"deletionTimestamp": "now"} if deleting else {},
                "status": {"podIP": "10.0.0.2",
                           "conditions": [{"type": "Ready", "status": "True"}]}}

    def ready(self, pods, addresses):
        from admission import Admission
        fixture = SimpleNamespace(registry="localhost:5000", get=Mock(side_effect=[
            {"items": pods}, {"subsets": [{"addresses": [{"ip": ip} for ip in addresses]}]}]))
        return Admission(fixture).ratify_endpoints_ready()

    def test_terminating_rollout_pod_cannot_count_as_ready(self):
        self.assertFalse(self.ready([self.pod(deleting=True)], ["10.0.0.2"]))
        self.assertFalse(self.ready([self.pod(), self.pod(deleting=True)], ["10.0.0.2"]))

    def test_service_endpoints_must_match_current_ready_pod(self):
        self.assertFalse(self.ready([self.pod()], []))
        self.assertFalse(self.ready([self.pod()], ["10.0.0.1"]))
        self.assertTrue(self.ready([self.pod()], ["10.0.0.2"]))


if __name__ == "__main__":
    unittest.main()
