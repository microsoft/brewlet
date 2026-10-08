# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from copy import deepcopy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, call, patch

from common import Fixture, kind_config
from retirement import evidence_document, main, require_blocked, retire_worker


TARGET = {"name": "owned-worker", "uid": "original-node-uid", "claimed": True,
          "providerID": "kind://docker/owned/owned-worker", "systemUUID": "original-host"}
PROFILE = {"metadata": {"uid": "profile-uid"},
           "status": {"targets": [TARGET], "retirement": {"targets": [TARGET]},
                      "conditions": [{"type": "Ready", "status": "False", "reason": "CleanupBlocked"}]}}


class RetirementTests(unittest.TestCase):
    def fixture(self):
        f = Mock()
        f.worker_names = ["owned-worker", "owned-worker2"]
        f.worker_ids = {"owned-worker": "original-container", "owned-worker2": "replacement-container"}
        f.cluster_uid = "cluster-uid"
        node = {"metadata": {"uid": TARGET["uid"]}, "spec": {"providerID": TARGET["providerID"]}}
        f.get.side_effect = [{"metadata": {"uid": f.cluster_uid}}, node]
        f.own_container.return_value = {
            "Id": "original-container", "Mounts": [{"Type": "volume", "Name": "owned-volume"}]}
        f.run.return_value = Mock(stdout="", returncode=0)
        return f

    def test_destroys_only_owned_host_before_removing_node_registration(self):
        f = self.fixture()
        receipt = retire_worker(f, TARGET["name"], TARGET, PROFILE)
        f.own_container.assert_called_once_with("owned-worker")
        self.assertEqual(f.run.call_args_list, [
            call(["docker", "rm", "-f", "--volumes", "original-container"]),
            call(["docker", "container", "ls", "-a", "--no-trunc", "--format", "{{.ID}}"]),
            call(["docker", "volume", "ls", "--format", "{{.Name}}"]),
        ])
        calls = [c[0] for c in f.mock_calls]
        self.assertGreater(calls.index("kube"), max(i for i, name in enumerate(calls) if name == "run"))
        self.assertEqual(f.worker_ids, {"owned-worker2": "replacement-container"})
        self.assertTrue(receipt["containerAbsent"] and receipt["volumesAbsent"])
        self.assertEqual(receipt["target"], TARGET)

    def test_rejects_survivor_replacement_foreign_or_changed_cluster_before_destruction(self):
        for name in ("owned-control-plane", "owned-worker2", "foreign-worker"):
            f = self.fixture()
            with self.subTest(name=name), self.assertRaisesRegex(RuntimeError, "original worker"):
                retire_worker(f, name, TARGET, PROFILE)
            f.run.assert_not_called()
        f = self.fixture()
        f.get.side_effect = [{"metadata": {"uid": "foreign-cluster"}}]
        with self.assertRaisesRegex(RuntimeError, "cluster identity"):
            retire_worker(f, TARGET["name"], TARGET, PROFILE)
        f.run.assert_not_called()

    def test_changed_node_provider_or_container_identity_blocks_destruction(self):
        for field, value in (("uid", "replacement"), ("providerID", "foreign")):
            f = self.fixture()
            node = {"metadata": {"uid": TARGET["uid"]}, "spec": {"providerID": TARGET["providerID"]}}
            node["metadata" if field == "uid" else "spec"][field] = value
            f.get.side_effect = [{"metadata": {"uid": f.cluster_uid}}, node]
            with self.subTest(field=field), self.assertRaises(RuntimeError):
                retire_worker(f, TARGET["name"], TARGET, PROFILE)
            f.run.assert_not_called()
        f = self.fixture()
        f.own_container.side_effect = RuntimeError("foreign/replaced container")
        with self.assertRaisesRegex(RuntimeError, "foreign/replaced"):
            retire_worker(f, TARGET["name"], TARGET, PROFILE)
        f.run.assert_not_called()

    def test_surviving_container_or_volume_is_not_evidence(self):
        for containers, volumes in (("original-container", ""), ("", "owned-volume")):
            f = self.fixture()
            f.run.side_effect = [Mock(stdout=""), Mock(stdout=containers), Mock(stdout=volumes)]
            with self.subTest(containers=containers, volumes=volumes), self.assertRaisesRegex(
                    AssertionError, "survived retirement"):
                retire_worker(f, TARGET["name"], TARGET, PROFILE)
            f.kube.assert_not_called()
            self.assertNotIn("retirement-host.json", [c.args[0] for c in f.save.call_args_list])

    def test_docker_inspection_error_is_not_treated_as_host_absence(self):
        f = self.fixture()
        f.run.side_effect = [Mock(stdout=""), RuntimeError("Docker disconnected")]
        with self.assertRaisesRegex(RuntimeError, "Docker disconnected"):
            retire_worker(f, TARGET["name"], TARGET, PROFILE)
        f.kube.assert_not_called()

    def test_submission_is_bound_to_saved_proof_and_original_identities(self):
        f = self.fixture()
        receipt = retire_worker(f, TARGET["name"], TARGET, PROFILE)
        with tempfile.TemporaryDirectory() as directory:
            f.work = Path(directory)
            (f.work / "retirement-host.json").write_text(json.dumps(receipt))
            spec = evidence_document(f, receipt, PROFILE)["spec"]
            self.assertEqual(spec["profileUID"], "profile-uid")
            self.assertEqual(spec["nodeUID"], "original-node-uid")
            self.assertEqual(spec["instanceID"], "original-container")
            self.assertEqual(spec["systemUUID"], "original-host")
            self.assertRegex(spec["evidenceRef"], r"^urn:sha256:[a-f0-9]{64}$")
            self.assertTrue(spec["permanentlyDecommissioned"])
            receipt["target"] = dict(TARGET)
            del receipt["target"]["systemUUID"]
            self.assertNotIn("systemUUID", evidence_document(f, receipt, PROFILE)["spec"])

    def test_negative_case_requires_block_and_preserved_obligation_and_no_new_claim(self):
        require_blocked(PROFILE, TARGET, "replacement")
        for mutation in ("unblocked", "lost-obligation", "claimed-replacement"):
            p = deepcopy(PROFILE)
            if mutation == "unblocked":
                p["status"]["conditions"] = []
            elif mutation == "lost-obligation":
                p["status"]["retirement"]["targets"] = []
            else:
                p["status"]["targets"].append({"name": "replacement", "uid": "new", "claimed": True})
            with self.subTest(mutation=mutation), self.assertRaises(AssertionError):
                require_blocked(p, TARGET, "replacement")

    def test_multi_node_config_keeps_all_nodes_invocation_owned(self):
        config = kind_config("owned", 2)
        self.assertEqual(config.count("- role: worker"), 2)
        self.assertEqual(config.count("sh.brewlet/live-owner: owned"), 3)
        self.assertNotIn("- role: worker", kind_config("owned"))

    def test_worker_identity_check_requires_captured_id_and_private_cluster_label(self):
        f = Fixture.__new__(Fixture)
        f.name, f.node = "owned", "owned-control-plane"
        f.worker_ids = {"owned-worker": "captured-id"}
        for identifier, cluster in (("changed-id", "owned"), ("captured-id", "foreign")):
            result = Mock(stdout=json.dumps([{
                "Id": identifier, "Config": {"Labels": {"io.x-k8s.kind.cluster": cluster}}}]))
            with patch.object(f, "run", return_value=result), self.assertRaisesRegex(
                    RuntimeError, "foreign/replaced"):
                f.own_container("owned-worker")

    def test_entry_point_owns_two_workers_and_runs_mandatory_scenario(self):
        with patch("retirement.CheckoutFixture") as factory, patch("retirement.exercise") as exercise:
            main()
            factory.assert_called_once_with("retirement", workers=2)
            exercise.assert_called_once_with(factory.return_value.__enter__.return_value)

    def test_cleanup_removes_surviving_worker_not_already_retired_host(self):
        f = Fixture.__new__(Fixture)
        f.node, f.node_id, f.registry_id, f.network_id = "control-plane", None, None, None
        f.worker_ids = {"owned-worker2": "replacement-container"}
        f.children, f.cleanups, f.old_signals, f.evidence = [], [], {}, []
        f.scenario = "retirement"
        with tempfile.TemporaryDirectory() as directory:
            f.private = Path(directory) / "private"
            f.private.mkdir()
            with patch.object(f, "diagnostics"), patch.object(f, "own_container") as owned, \
                    patch.object(f, "save"), patch.object(f, "run") as execute:
                f.finish(False)
                owned.assert_called_once_with("owned-worker2")
                execute.assert_called_once_with(
                    ["docker", "rm", "-f", "--volumes", "replacement-container"])


if __name__ == "__main__":
    unittest.main()
