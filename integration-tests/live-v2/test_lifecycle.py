"""Offline safeguards only; these do not establish lifecycle acceptance."""

import ast
import copy
from datetime import datetime, timedelta, timezone
import inspect
import json
from pathlib import Path
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from harness import Failure, OWNER
import lifecycle as l


RUN = "0123456789ab"
CONTAINER = "a" * 64
RUNTIME = l.fixture_id(RUN, "epoch")
NOW = datetime(2026, 10, 16, 18, tzinfo=timezone.utc)


def identity():
    return {"id": RUNTIME, "inventoryUID": "inventory-old", "generation": 9,
            "sourceDigest": "sha256:" + "b" * 64}


def lease():
    return {"taskID": CONTAINER, "namespace": "k8s.io", "runtime": identity()}


def authorization():
    return {**identity(), "profileUID": "profile-uid", "profileGeneration": 3,
            "expiresAt": (NOW - timedelta(seconds=1)).isoformat(), "revoked": False}


def profile():
    return {"apiVersion": "node.brewlet.sh/v2alpha1", "kind": "NodeProfile",
            "metadata": {"name": "private", "uid": "profile-uid",
                         "resourceVersion": "92", "labels": {OWNER: RUN}},
            "spec": {"runtimeImages": [{"id": "fixture-python", "install": "OnDemand"}]},
            "status": {"provisioningGeneration": 3}}


class WorkerRestartTests(unittest.TestCase):
    def test_recovery_observes_real_stderr_logs_and_requires_fresh_marker(self):
        report = {"complete": True, "retainedTasks": [CONTAINER], "removedTasks": []}
        message = "2026/10/16 runtime-lease-recovery: " + json.dumps(report) + "\n"
        worker = l.Lifecycle.__new__(l.Lifecycle)
        worker.inspect = Mock()
        current = {"cid": "b" * 64, "podUID": "worker-pod"}
        for stream in ("stdout", "stderr"):
            with self.subTest(stream=stream):
                output = {"stdout": "", "stderr": "", stream: message}
                worker.h = SimpleNamespace(node_exec=Mock(
                    return_value=subprocess.CompletedProcess([], 0, **output)))
                self.assertTrue(worker.recovery_log(current, lease()))
                self.assertFalse(worker.recovery_log(current, lease(), after_prefix=message))
        worker.inspect.assert_called_with(current["cid"], pod_uid="worker-pod")

    def fixture(self):
        worker = l.Lifecycle.__new__(l.Lifecycle)
        worker.profile_uid = "profile-uid"
        worker.guard = Mock()
        current = profile()
        current["metadata"]["generation"] = 3
        container = {"name": "provisioner", "env": [
            {"name": "BREWLET_PROFILE_UID", "value": "profile-uid"},
            {"name": "BREWLET_PROFILE_GENERATION", "value": "3"},
            {"name": "BREWLET_MODE", "value": "provisioning"}]}
        pod = {"metadata": {"name": "worker", "uid": "worker-pod", "ownerReferences": [
            {"kind": "DaemonSet", "name": "worker-ds", "uid": "ds-uid", "controller": True}]},
               "spec": {"nodeName": "private-node", "containers": [container]},
               "status": {"containerStatuses": [{"name": "provisioner",
                   "containerID": "containerd://" + CONTAINER, "state": {"running": {}}}]}}
        ds = {"metadata": {"uid": "ds-uid", "ownerReferences": [
            {"kind": "NodeProfile", "uid": "profile-uid", "controller": True}]}}
        worker.h = SimpleNamespace(run_id=RUN, profile="private", node="private-node",
                                   control_namespace="private-system")
        worker.h.get = Mock(side_effect=lambda kind, *args, **kwargs:
                            current if kind == "nodeprofile" else
                            {"items": [pod]} if kind == "pods" else ds)
        worker.inspect = Mock(return_value={"info": {"pid": 1234}})
        def node_exec(*args):
            output = ("/brewlet-node\n" if args[0] == "readlink" else
                      "1234 (brewlet-node) " + " ".join(["S"] + ["0"] * 18 + ["98765"])
                      if args[0] == "cat" else "")
            return subprocess.CompletedProcess(args, 0, output, "")
        worker.h.node_exec = Mock(side_effect=node_exec)
        return worker, pod, ds

    def test_successor_wait_retries_stopped_or_previous_container(self):
        worker, pod, _ = self.fixture()
        self.assertFalse(worker.worker_if_running(CONTAINER))
        worker.inspect.assert_not_called()
        pod["status"]["containerStatuses"][0]["state"] = {"terminated": {"exitCode": 1}}
        self.assertFalse(worker.worker_if_running(CONTAINER))
        worker.inspect.assert_not_called()
        with self.assertRaisesRegex(Failure, "not actually running"):
            worker.worker()

    def test_successor_selection_uses_one_pod_snapshot_and_checks_owner_chain(self):
        worker, pod, ds = self.fixture()
        successor = "b" * 64
        pod["status"]["containerStatuses"][0]["containerID"] = "containerd://" + successor
        result = worker.worker_if_running(CONTAINER)
        self.assertEqual(result["cid"], successor)
        self.assertEqual(result["podUID"], "worker-pod")
        self.assertEqual(result["start"], "98765")
        self.assertEqual(sum(call.args[0] == "pods" for call in worker.h.get.call_args_list), 1)
        ds["metadata"]["uid"] = "foreign-ds"
        with self.assertRaisesRegex(Failure, "ownership chain changed"):
            worker.worker_if_running(CONTAINER)


class EpochTests(unittest.TestCase):
    def test_fixture_ids_do_not_overlap_base_or_other_invocations(self):
        for purpose in l.PREFIXES:
            value = l.fixture_id(RUN, purpose)
            self.assertLessEqual(len(value), 55)
            self.assertNotIn(value, ("fixture-python", "fixture-java", "fixture-node", "fixture-dotnet"))
            self.assertNotEqual(value, l.fixture_id("ffffffffffff", purpose))
        for run in ("", "../../outside", "A" * 12, "a" * 64):
            with self.subTest(run=run), self.assertRaises(Failure):
                l.fixture_id(run, "epoch")

    def test_epoch_identity_rejects_unsafe_paths_and_fake_generations(self):
        for key, values in (
            ("id", ("fixture-python", "../escape", RUNTIME + "/root", None)),
            ("inventoryUID", ("../escape", "", "a/b", "a b", None)),
            ("generation", (0, -1, True, "1", 1.0, None))):
            for value in values:
                with self.subTest(key=key, value=value), self.assertRaises(Failure):
                    l.root_path(dict(identity(), **{key: value}))
        self.assertEqual(l.root_path(identity()),
                         f"/opt/brewlet/runtimes/{RUNTIME}/inventory-old/9")

    def test_recreation_compares_uids_not_cross_epoch_generations(self):
        old = identity()
        new = dict(old, inventoryUID="inventory-new", generation=1)
        self.assertEqual(l.recreated(old, new),
                         {"id": RUNTIME, "inventoryUID": "inventory-old", "generation": 9})
        for bad in (dict(new, inventoryUID="inventory-old"), dict(new, generation=2),
                    dict(new, id=l.fixture_id(RUN, "cold"))):
            with self.subTest(bad=bad), self.assertRaises(Failure):
                l.recreated(old, bad)


class ProfileSafetyTests(unittest.TestCase):
    def test_profile_replacement_preserves_base_uid_and_resource_version(self):
        original = profile()
        value = l.profile_update(original, RUN, "profile-uid", {RUNTIME: None},
                                 {RUNTIME: {"id": RUNTIME, "install": "OnDemand"}})
        self.assertEqual(original, profile(), "builder mutated its input")
        self.assertEqual(value["metadata"], original["metadata"])
        self.assertEqual(value["status"], original["status"])
        self.assertEqual(value["spec"]["runtimeImages"][0], original["spec"]["runtimeImages"][0])
        self.assertEqual(value["metadata"]["resourceVersion"], "92")
        entry = value["spec"]["runtimeImages"][1]
        removed = l.profile_update(value, RUN, "profile-uid", {RUNTIME: entry}, {RUNTIME: None})
        self.assertEqual(removed, original)

    def test_replaced_foreign_deleting_and_unversioned_profiles_rejected(self):
        for mutate in (
            lambda p: p["metadata"].update(uid="replacement"),
            lambda p: p["metadata"].update(resourceVersion=""),
            lambda p: p["metadata"].update(deletionTimestamp="now"),
            lambda p: p["metadata"]["labels"].update({OWNER: "ffffffffffff"})):
            value = profile()
            mutate(value)
            with self.assertRaises(Failure):
                l.profile_update(value, RUN, "profile-uid", {RUNTIME: None},
                                 {RUNTIME: {"id": RUNTIME}})

    def test_never_overwrites_base_foreign_or_concurrently_edited_entries(self):
        for rid in ("fixture-python", l.fixture_id("ffffffffffff", "epoch")):
            with self.subTest(rid=rid), self.assertRaises(Failure):
                l.profile_update(profile(), RUN, "profile-uid", {rid: None}, {rid: None})
        value = profile()
        value["spec"]["runtimeImages"].append({"id": RUNTIME, "install": "Eager"})
        with self.assertRaises(Failure):
            l.profile_update(value, RUN, "profile-uid",
                             {RUNTIME: {"id": RUNTIME, "install": "OnDemand"}}, {RUNTIME: None})
        with self.assertRaises(Failure):
            l.profile_update(profile(), RUN, "profile-uid", {}, {RUNTIME: None})

    def test_uid_scoped_finalization_refuses_another_epoch_before_read_or_mutation(self):
        h = SimpleNamespace(run_id=RUN, profile="private",
                            owned={("nodeprofile", "", "private"): "profile-uid"},
                            wait=Mock(), get=Mock(), node_exec=Mock())
        fixture = l.Lifecycle(h)
        fixture.inventories[RUNTIME] = {"uid": "inventory-new", "deleted": False}
        with self.assertRaises(Failure):
            fixture.finalize(RUNTIME, identity())
        h.wait.assert_not_called()
        h.get.assert_not_called()
        h.node_exec.assert_not_called()

    def test_failed_profile_replace_never_updates_local_ownership_tracking(self):
        h = SimpleNamespace(run_id=RUN, profile="private",
                            owned={("nodeprofile", "", "private"): "profile-uid"},
                            get=Mock(return_value=profile()),
                            kubectl=Mock(side_effect=Failure("409 conflict")), wait=Mock())
        fixture = l.Lifecycle(h)
        fixture.guard = Mock()
        with self.assertRaises(Failure):
            fixture.edit_profile({RUNTIME: {"id": RUNTIME, "install": "OnDemand"}})
        self.assertEqual(fixture.entries, {})
        args, kwargs = h.kubectl.call_args
        self.assertEqual(args, ("replace", "-f", "-", "-o", "json"))
        self.assertEqual(json.loads(kwargs["input"])["metadata"]["resourceVersion"], "92")
        h.wait.assert_not_called()

    def test_resource_version_conflicts_reread_and_preserve_new_uid_fences(self):
        h = SimpleNamespace(run_id=RUN, profile="private",
                            owned={("nodeprofile", "", "private"): "profile-uid"},
                            get=Mock(), kubectl=Mock())
        fixture = l.Lifecycle(h)
        fixture.guard = Mock()
        first, newer = profile(), profile()
        newer["metadata"]["resourceVersion"] = "93"
        h.get.side_effect = [first, newer]
        h.kubectl.side_effect = [
            SimpleNamespace(returncode=1, stderr="Error from server (Conflict): changed"),
            SimpleNamespace(returncode=0, stdout=json.dumps(newer))]
        with patch.object(l.time, "sleep"):
            result = fixture.replace_owned("nodeprofile", "private", "profile-uid",
                                            lambda current: current)
        self.assertEqual(result["metadata"]["resourceVersion"], "93")
        sent = [json.loads(call.kwargs["input"])["metadata"]["resourceVersion"]
                for call in h.kubectl.call_args_list]
        self.assertEqual(sent, ["92", "93"])
        replaced = profile()
        replaced["metadata"]["uid"] = "foreign-recreated"
        h.get.side_effect = [first, replaced]
        h.kubectl.reset_mock()
        h.kubectl.side_effect = [
            SimpleNamespace(returncode=1, stderr="Error from server (Conflict): changed")]
        with patch.object(l.time, "sleep"), self.assertRaises(Failure):
            fixture.replace_owned("nodeprofile", "private", "profile-uid", lambda value: value)
        self.assertEqual(h.kubectl.call_count, 1)


class ExpiryTests(unittest.TestCase):
    def test_gc_timing_uses_positive_production_configuration_without_zero_grace(self):
        for value, expected in (("20s", 20), ("2s", 2), ("1h", 3600),
                                ("1m", 60), ("500ms", 0.5), ("0.5s", 0.5)):
            with self.subTest(value=value):
                self.assertEqual(l.duration_seconds(value), expected)
        for value in ("0s", "-1s", "inf", "", "1", None, "nan", "1d"):
            with self.subTest(value=value), self.assertRaises(Failure):
                l.duration_seconds(value)
        h = SimpleNamespace(run_id=RUN, profile="private", timeout=600,
                            owned={("nodeprofile", "", "private"): "profile-uid"})
        fixture = l.Lifecycle(h)
        worker = {"cid": CONTAINER, "gcSettings": {
            "BREWLET_GC_GRACE": "20s", "BREWLET_GC_SWEEP_INTERVAL": "2s"}}
        self.assertEqual(fixture.gc_timing(worker)["graceSeconds"], 20)
        with self.assertRaises(Failure):
            fixture.gc_timing({"cid": CONTAINER, "gcSettings": {}})
        worker["gcSettings"]["BREWLET_GC_GRACE"] = "0s"
        with self.assertRaises(Failure):
            fixture.gc_timing(worker)

    def test_expiry_requires_unchanged_real_record_stopped_renewal_and_elapsed_time(self):
        record = authorization()
        proof = l.expiry_proof(record, copy.deepcopy(record), NOW, elapsed=301.2, stopped=True)
        self.assertEqual(proof["realElapsedSeconds"], 301.2)
        for kwargs in (
            {"stopped": False}, {"elapsed": 0},
            {"observed_at": NOW - timedelta(seconds=5)},
            {"observed_at": NOW.replace(tzinfo=None)},
            {"after": dict(record, expiresAt=(NOW + timedelta(minutes=5)).isoformat())}):
            options = dict(before=record, after=copy.deepcopy(record), observed_at=NOW,
                           elapsed=301, stopped=True)
            options.update(kwargs)
            with self.subTest(kwargs=kwargs), self.assertRaises(Failure):
                l.expiry_proof(**options)
        revoked = dict(record, revoked=True)
        with self.assertRaises(Failure):
            l.expiry_proof(revoked, revoked, NOW, elapsed=301, stopped=True)

    def test_missing_timezone_or_malformed_expiry_rejected(self):
        for value in (None, "", "tomorrow", "2026-10-16T18:00:00", 42):
            with self.subTest(value=value), self.assertRaises(Failure):
                l.timestamp(value)

    def test_live_path_uses_real_time_and_no_protected_record_writes(self):
        source = Path(l.__file__).read_text()
        tree = ast.parse(source)
        calls = [node for node in ast.walk(tree) if isinstance(node, ast.Call)]
        forbidden = {"clock_settime", "utime", "settimeofday", "freeze_time", "mock", "patch"}
        for call in calls:
            name = call.func.attr if isinstance(call.func, ast.Attribute) else (
                call.func.id if isinstance(call.func, ast.Name) else "")
            self.assertNotIn(name, forbidden)
        self.assertNotIn("write_text", source)
        self.assertNotIn("write_bytes", source)
        self.assertNotIn("finalizers] =", source)
        expiry = inspect.getsource(l.Lifecycle.expiry_and_recovery)
        self.assertIn('signal_command(worker["pid"], worker["start"], "/brewlet-node", "STOP")', expiry)
        self.assertIn('signal_command(worker["pid"], worker["start"], "/brewlet-node", "CONT")', expiry)
        self.assertIn("datetime.now(timezone.utc)", expiry)
        self.assertIn("initial == observed", expiry)
        self.assertIn('self.rotate(rid, "after")', expiry)
        self.assertIn("finally:", expiry)


class ProcessAndReferenceTests(unittest.TestCase):
    def test_only_exact_owned_cid_and_exec_are_signal_targets(self):
        self.assertEqual(l.task_command("terminate-main", CONTAINER),
                         ["ctr", "-n", "k8s.io", "tasks", "kill", "--signal", "SIGTERM", CONTAINER])
        command = l.task_command("terminate-exec", CONTAINER, exec_id="life-exec-" + RUN)
        self.assertEqual(command[-1], CONTAINER)
        self.assertIn("--exec-id", command)
        self.assertNotIn("--all", command)
        for value in ("app", CONTAINER[:12], CONTAINER + ";kill 1", "", None):
            with self.subTest(value=value), self.assertRaises(Failure):
                l.task_command("terminate-main", value)
        for value in ("foreign", "life-exec-../../", "", None):
            with self.subTest(value=value), self.assertRaises(Failure):
                l.task_command("terminate-exec", CONTAINER, exec_id=value)

    def test_pause_checks_pid_starttime_and_exact_executable(self):
        command = l.signal_command(4321, "123456", "/brewlet-node", "STOP")
        self.assertEqual(command[-4:], ["4321", "123456", "/brewlet-node", "STOP"])
        self.assertIn("readlink /proc/$1/exe", command[2])
        self.assertIn('test "$(printf', command[2])
        self.assertIn('kill -"$4" "$1"', command[2])
        for args in ((1, "123", "/brewlet-node", "STOP"),
                     (True, "123", "/brewlet-node", "STOP"),
                     (4321, "1;kill", "/brewlet-node", "STOP"),
                     (4321, "123", "/usr/bin/python", "STOP"),
                     (4321, "123", "/brewlet-node", "KILL")):
            with self.subTest(args=args), self.assertRaises(Failure):
                l.signal_command(*args)

    def test_detached_exec_uses_real_shim_rpc_and_explicit_nonroot_identity(self):
        command = l.exec_command(CONTAINER, "life-exec-" + RUN)
        self.assertEqual(command[:6], ["ctr", "-n", "k8s.io", "tasks", "exec", "--exec-id"])
        self.assertEqual(command[command.index("--user") + 1], "65532:65532")
        self.assertIn("--detach", command)
        self.assertEqual(command[-4:], [CONTAINER, "python3", "-c", l.MAIN])
        with self.assertRaises(Failure):
            l.exec_command("app-name", "life-exec-" + RUN)
        with self.assertRaises(Failure):
            l.exec_command(CONTAINER, "../foreign-exec")

    def test_daemon_restart_requires_service_pid_start_identity_and_containerd_binary(self):
        command = l.daemon_restart_command(4321, "123456", "/usr/local/bin/containerd")
        self.assertEqual(command[-3:], ["4321", "123456", "/usr/local/bin/containerd"])
        self.assertIn('systemctl show containerd --property=MainPID --value', command[2])
        self.assertIn('readlink /proc/$1/exe', command[2])
        self.assertIn('systemctl restart containerd', command[2])
        for args in ((1, "123", "/usr/bin/containerd"),
                     (4321, "1;kill", "/usr/bin/containerd"),
                     (4321, "123", "/usr/bin/kubelet")):
            with self.subTest(args=args), self.assertRaises(Failure):
                l.daemon_restart_command(*args)

    def test_shim_signal_is_guarded_by_exact_binary_starttime_namespace_and_owned_sandbox(self):
        command = l.shim_signal_command(4321, "123456", CONTAINER, "STOP")
        self.assertEqual(command[-4:], ["4321", "123456", CONTAINER, "STOP"])
        self.assertIn("/usr/local/bin/containerd-shim-brewlet-v2", command[2])
        self.assertIn('prev=="-id"&&$0==id', command[2])
        self.assertIn('prev=="-namespace"&&$0=="k8s.io"', command[2])
        for args in ((1, "123", CONTAINER, "STOP"),
                     (4321, "1;kill", CONTAINER, "STOP"),
                     (4321, "123", "sandbox-name", "STOP"),
                     (4321, "123", CONTAINER, "KILL")):
            with self.subTest(args=args), self.assertRaises(Failure):
                l.shim_signal_command(*args)

    def test_signal_guard_rejects_reused_or_foreign_pid_without_sending_signal(self):
        # Read-only failure before kill: this PID is this test's shell child,
        # never /brewlet-node. No cluster, Docker, or host service is accessed.
        command = l.signal_command(2147483647, "123", "/brewlet-node", "STOP")
        result = subprocess.run(command, text=True, capture_output=True, check=False)
        self.assertNotEqual(result.returncode, 0)

    def test_lease_requires_exact_task_namespace_epoch_generation_and_digest(self):
        self.assertEqual(l.validate_lease(lease(), identity(), CONTAINER), lease())
        mutations = (
            lambda v: v.update(taskID="b" * 64),
            lambda v: v.update(namespace="foreign"),
            lambda v: v["runtime"].update(inventoryUID="new-epoch"),
            lambda v: v["runtime"].update(generation=1),
            lambda v: v["runtime"].update(sourceDigest="sha256:" + "c" * 64))
        for mutation in mutations:
            value = lease()
            mutation(value)
            with self.assertRaises(Failure):
                l.validate_lease(value, identity(), CONTAINER)

    def test_orphan_proof_requires_live_exec_main_exit_and_exact_root_mount(self):
        mount = "lowerdir=" + l.root_path(identity()) + "/rootfs,/app-cache"
        good = dict(main_exited=True, exec_pids=[5432], mountinfo=[mount])
        self.assertTrue(l.retained_refs(lease(), identity(), CONTAINER, **good)["mountReferenced"])
        for patch in ({"main_exited": False}, {"exec_pids": []}, {"exec_pids": [1]},
                      {"mountinfo": []}, {"mountinfo": [mount.replace("/9/", "/8/")]},
                      {"mountinfo": [mount.replace("/rootfs", "/rootfs-other")]}):
            with self.subTest(patch=patch), self.assertRaises(Failure):
                l.retained_refs(lease(), identity(), CONTAINER, **dict(good, **patch))

    def test_fresh_recovery_proof_requires_complete_actual_task_identity_sets(self):
        report = {"retainedTasks": [CONTAINER], "removedTasks": [], "complete": True}
        proof = l.recovery_proof(report, identity(), CONTAINER, retained=True)
        self.assertTrue(proof["retained"])
        self.assertEqual(proof["observation"], report)
        for value in (None, {}, {"error": "incomplete task evidence", **report},
                      dict(report, complete=False),
                      dict(report, retainedTasks=[]),
                      dict(report, retainedTasks=[CONTAINER, CONTAINER]),
                      dict(report, retainedTasks=["foreign-pid"]),
                      dict(report, removedTasks=[CONTAINER])):
            with self.subTest(value=value), self.assertRaises(Failure):
                l.recovery_proof(value, identity(), CONTAINER, retained=True)
        self.assertFalse(l.recovery_proof({"retainedTasks": [], "removedTasks": [CONTAINER],
                                          "complete": True},
                                         identity(), CONTAINER, retained=False)["retained"])
        self.assertTrue(l.recovery_proof(dict(report, removedTasks=None),
                                         identity(), CONTAINER, retained=True)["retained"])
        with self.assertRaises(Failure):
            l.recovery_proof(report, identity(), CONTAINER, retained=False)

    def test_recovery_marker_must_come_from_exact_fresh_worker_container_logs(self):
        h = SimpleNamespace(run_id=RUN, profile="private",
                            owned={("nodeprofile", "", "private"): "profile-uid"},
                            node_exec=Mock())
        fixture = l.Lifecycle(h)
        fixture.inspect = Mock()
        worker = {"cid": "c" * 64, "podUID": "real-worker-pod"}
        report = {"retainedTasks": [CONTAINER], "removedTasks": [], "complete": True}
        h.node_exec.return_value = SimpleNamespace(
            stdout="2026/10/16 12:00:00 runtime-lease-recovery: " + json.dumps(report), stderr="")
        self.assertTrue(fixture.recovery_log(worker, lease())["retained"])
        fixture.inspect.assert_called_once_with("c" * 64, pod_uid="real-worker-pod")
        h.node_exec.assert_called_once_with("crictl", "logs", "c" * 64)
        for output in ("", "unrelated error", "runtime-lease-recovery: " + json.dumps(
                dict(report, retainedTasks=["b" * 64])),
                "runtime-lease-recovery: " + json.dumps(dict(report, complete=False))):
            h.node_exec.return_value = SimpleNamespace(stdout=output, stderr="")
            self.assertFalse(fixture.recovery_log(worker, lease()))
        h.node_exec.return_value = SimpleNamespace(
            stdout="runtime-lease-recovery: " + json.dumps(dict(report, removedTasks=[CONTAINER])), stderr="")
        with self.assertRaises(Failure):
            fixture.recovery_log(worker, lease())
        prefix = "old runtime-lease-recovery: " + json.dumps(report) + "\n"
        h.node_exec.return_value = SimpleNamespace(stdout=prefix, stderr="")
        self.assertFalse(fixture.recovery_log(worker, lease(), after_prefix=prefix))
        h.node_exec.return_value = SimpleNamespace(
            stdout=prefix + "new runtime-lease-recovery: " + json.dumps(report), stderr="")
        self.assertTrue(fixture.recovery_log(worker, lease(), after_prefix=prefix)["retained"])
        h.node_exec.return_value = SimpleNamespace(stdout="rotated-log\n", stderr="")
        with self.assertRaises(Failure):
            fixture.recovery_log(worker, lease(), after_prefix=prefix)

    def test_uncertainty_injection_restores_only_owned_shim_and_requires_new_recovery_marker(self):
        source = inspect.getsource(l.Lifecycle.uncertain_shim_evidence)
        self.assertIn('group_id in (sandbox, lease["taskID"])', source)
        self.assertIn('require_owned(obj, self.h.run_id, self.pods[name])', source)
        self.assertIn('shim_signal_command(shim_pid, start, group_id, "STOP")', source)
        self.assertIn('finally:', source)
        self.assertIn('shim_signal_command(shim_pid, start, group_id, "CONT")', source)
        self.assertIn('after_prefix=blocked_prefix', source)
        self.assertIn('"runtime collection blocked:"', source)
        self.assertNotIn("SIGKILL", source)
        self.assertNotIn("killall", source)
        self.assertNotIn("pkill", source)

    def test_callbacks_do_not_execute_at_import_or_claim_shim_crash_reconstruction(self):
        tree = ast.parse(Path(l.__file__).read_text())
        for node in tree.body:
            if isinstance(node, ast.Expr) and isinstance(node.value, ast.Call):
                self.fail("import performs a top-level operation")
        callback = inspect.getsource(l.Lifecycle.execute)
        self.assertIn('"shimCrashReconstructionVerified": False', callback)
        self.assertIn('"fullAcceptance": False', callback)
        self.assertNotIn("skip", callback)


if __name__ == "__main__":
    unittest.main()
