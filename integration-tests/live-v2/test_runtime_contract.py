"""Offline guards only; these fixtures do not qualify a runtime or native node."""

import copy
from datetime import datetime, timezone
from types import SimpleNamespace
import unittest
from unittest.mock import Mock

import authority_checks
import cold_rpc
import runtime_contract as r
from harness import FENCE, Failure, STORE


class RuntimeContractTests(unittest.TestCase):
    def publication(self):
        digest = "sha256:" + "a" * 64
        metadata = {"id": "python", "inventoryUID": "epoch", "generation": 2, "sourceDigest": digest,
                    "manifestDigest": "sha256:" + "b" * 64, "uid": 65532, "gid": 65532,
                    "platform": {"os": "linux", "architecture": "amd64"},
                    "validatedAt": "2026-10-21T12:00:00Z", "env": ["PATH=/usr/bin", "HOME=/home/app"]}
        inventory = {"metadata": {"name": "python", "uid": "epoch"}, "status": {"generation": 2, "digest": digest},
                     "spec": {"source": {"image": "registry.test/runtime@" + digest},
                              "user": {"uid": 65532, "gid": 65532}}}
        return metadata, inventory, "amd64", STORE + "/python/epoch/2", ["0 0 700", "0 0 600"]

    def test_publication_requires_observed_epoch_digest_native_platform_and_protected_metadata(self):
        self.assertEqual(STORE + "/python/epoch/2", r.validate_publication(*self.publication()))
        for change in ("epoch", "source", "platform", "identity", "time", "env", "owner", "permissions", "path"):
            values = list(copy.deepcopy(self.publication()))
            if change == "epoch":
                values[0]["inventoryUID"] = "foreign"
            elif change == "source":
                values[0]["sourceDigest"] = "sha256:" + "c" * 64
            elif change == "platform":
                values[0]["platform"]["architecture"] = "arm64"
            elif change == "identity":
                values[0]["uid"] = 0
            elif change == "time":
                values[0]["validatedAt"] = "0001-01-01T00:00:00Z"
            elif change == "env":
                values[0]["env"] += ["PATH=/app"]
            elif change == "owner":
                values[4][0] = "1000 0 700"
            elif change == "permissions":
                values[4][1] = "0 0 644"
            else:
                values[3] = STORE + "/python/foreign/2"
            with self.subTest(change=change), self.assertRaises((Failure, ValueError)):
                r.validate_publication(*values)

    def test_actual_generation_transition_cannot_reuse_a_number_or_change_epoch(self):
        before = {"metadata": {"uid": "epoch"}, "status": {"generation": 2}}
        after = {"metadata": {"uid": "epoch"}, "status": {"generation": 3, "digest": "sha256:new"}}
        r.generation_transition(before, after, "registry.test/runtime@sha256:new")
        for change in ("reuse", "epoch", "digest"):
            value = copy.deepcopy(after)
            if change == "reuse":
                value["status"]["generation"] = 2
            elif change == "epoch":
                value["metadata"]["uid"] = "foreign"
            else:
                value["status"]["digest"] = "sha256:other"
            with self.subTest(change=change), self.assertRaises(Failure):
                r.generation_transition(before, value, "registry.test/runtime@sha256:new")

    def quarantine(self):
        participant = {"assignedAt": "2026-10-21T12:00:00Z"}
        excluded = {"name": "owned", "uid": "node-uid", "reason": "RotationStalled", "observedGeneration": 1}
        node = {"metadata": {"name": "owned", "uid": "node-uid", "labels": {}}}
        lease = {"runtime": {"id": "python", "generation": 1}}
        conditions = [{"type": "RotationComplete", "status": "True"}]
        now = datetime(2026, 10, 21, 12, 10, tzinfo=timezone.utc).timestamp()
        return participant, excluded, node, lease, conditions, now

    def test_quarantine_requires_real_deadline_uid_generation_and_confirmed_withdrawal(self):
        self.assertEqual(600, r.validate_quarantine(*self.quarantine())["elapsedSeconds"])
        for change in ("early", "uid", "generation", "ready", "available", "unconfirmed", "incomplete"):
            values = list(copy.deepcopy(self.quarantine()))
            if change == "early":
                values[-1] -= 1
            elif change == "uid":
                values[1]["uid"] = "foreign"
            elif change == "generation":
                values[1]["observedGeneration"] = 0
            elif change in ("ready", "available"):
                values[2]["metadata"]["labels"]["brewlet.sh/runtime.python"] = change
            elif change == "unconfirmed":
                values[1]["reason"] = "RotationStalled: UnconfirmedFence"
            else:
                values[4] = []
            with self.subTest(change=change), self.assertRaises(Failure):
                r.validate_quarantine(*values)
        values = list(self.quarantine())
        values[2]["metadata"]["labels"]["brewlet.sh/runtime.python"] = "ready"
        values[2]["spec"] = {"taints": [{"key": FENCE, "value": "provisioning", "effect": "NoSchedule"}]}
        self.assertEqual(600, r.validate_quarantine(*values)["elapsedSeconds"])

    def test_real_cold_rpc_wait_thresholds_and_no_started_process(self):
        response = SimpleNamespace(returncode=1, stdout="", stderr="RuntimeImageInstalling")
        status = {"startedAt": 0, "state": "CONTAINER_CREATED"}
        cold_rpc.validate_wait(30, 120, response, status)
        for elapsed in (0, 28.9, 40, 120):
            with self.subTest(elapsed=elapsed), self.assertRaises(Failure):
                cold_rpc.validate_wait(elapsed, 120, response, status)
        for bad in ({"startedAt": 1, "state": "CONTAINER_EXITED"},
                    {"startedAt": 0, "state": "CONTAINER_RUNNING"}, {"state": "CONTAINER_CREATED"}):
            with self.subTest(status=bad), self.assertRaises(Failure):
                cold_rpc.validate_wait(30, 120, response, bad)
        with self.assertRaises(Failure):
            cold_rpc.validate_wait(30, 30, response, status)

    def test_cold_rpc_copy_keeps_actual_mounts_security_and_does_not_modify_kubelet_config(self):
        original = {"metadata": {"name": "app", "attempt": 3}, "image": {"image": "old"},
                    "log_path": "app/3.log", "mounts": [{"host_path": "/owned/pod/volume"}],
                    "linux": {"security_context": {"readonly_rootfs": True}}}
        before = copy.deepcopy(original)
        config = cold_rpc.container_config(original, "owned-probe", "registry.test/app@sha256:image")
        self.assertEqual(before, original)
        self.assertEqual(original["linux"], config["linux"])
        self.assertEqual(original["mounts"], config["mounts"])
        self.assertEqual({"name": "owned-probe", "attempt": 0}, config["metadata"])
        self.assertEqual("owned-probe.log", config["log_path"])

    def test_initial_authority_probe_has_real_writable_host_access_but_no_valid_authority(self):
        h = SimpleNamespace(run_id="0123456789ab", node="owned-node", profile="owned-profile",
                            control_namespace="owned-system", result={"workerImage": "owned-worker"})
        pod = authority_checks.initial_probe(h)
        container = pod["spec"]["containers"][0]
        self.assertTrue(container["securityContext"]["privileged"])
        self.assertEqual({"name": "host", "mountPath": "/host"}, container["volumeMounts"][0])
        self.assertEqual("/", pod["spec"]["volumes"][0]["hostPath"]["path"])
        self.assertEqual("[]", next(env["value"] for env in container["env"] if env["name"] == "BREWLET_TARGET_UIDS"))
        self.assertNotIn("runtimeClassName", pod["spec"])

    def test_worker_selection_requires_controller_uids_not_profile_labels(self):
        pod = {"metadata": {"namespace": "owned-system", "ownerReferences": [
            {"controller": True, "kind": "DaemonSet", "name": "worker", "uid": "daemonset-uid"}]}}
        daemonset = {"metadata": {"uid": "daemonset-uid", "ownerReferences": [
            {"controller": True, "kind": "NodeProfile", "name": "owned-profile", "uid": "profile-uid"}]}}
        h = SimpleNamespace(control_namespace="owned-system", profile="owned-profile",
                            get=Mock(return_value=daemonset))
        authority_checks.require_worker_owner(h, pod, "profile-uid")
        for change in ("namespace", "missing-controller", "daemonset-uid", "profile-uid"):
            candidate, owner = copy.deepcopy(pod), copy.deepcopy(daemonset)
            if change == "namespace":
                candidate["metadata"]["namespace"] = "foreign"
            elif change == "missing-controller":
                candidate["metadata"]["ownerReferences"][0]["controller"] = False
            elif change == "daemonset-uid":
                owner["metadata"]["uid"] = "replaced"
            else:
                owner["metadata"]["ownerReferences"][0]["uid"] = "foreign"
            h.get.return_value = owner
            with self.subTest(change=change), self.assertRaises(Failure):
                authority_checks.require_worker_owner(h, candidate, "profile-uid")

    def test_cold_rpc_restores_owned_kubelet_after_probe_or_stop_failure(self):
        for fail_stop in (False, True):
            calls = []
            state = "active"
            def execute(worker, *args, **kwargs):
                nonlocal state
                calls.append(args)
                if args[:2] == ("systemctl", "stop"):
                    state = "inactive"
                    if fail_stop:
                        raise Failure("stop command interrupted")
                elif args[:2] == ("systemctl", "start"):
                    state = "active"
                return SimpleNamespace(stdout=state + "\n")
            def wait(description, predicate):
                self.assertTrue(predicate(), description)
            h = SimpleNamespace(node_exec_on=execute, wait=wait,
                get=lambda *args: {"status": {"conditions": [{"type": "Ready", "status": "True"}]}})
            with self.subTest(fail_stop=fail_stop), self.assertRaises(Failure):
                with cold_rpc.paused_kubelet(h, "owned"):
                    self.assertEqual("inactive", state)
                    raise Failure("probe failed")
            self.assertEqual("active", state)
            self.assertIn(("systemctl", "start", "kubelet"), calls)

    def test_cold_rpc_does_not_start_an_initially_inactive_kubelet(self):
        h = SimpleNamespace(node_exec_on=Mock(return_value=SimpleNamespace(stdout="inactive\n")))
        with self.assertRaises(Failure):
            with cold_rpc.paused_kubelet(h, "owned"):
                self.fail("inactive kubelet must not enter the probe")
        self.assertEqual(1, h.node_exec_on.call_count)


if __name__ == "__main__":
    unittest.main()
