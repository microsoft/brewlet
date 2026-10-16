"""Offline safety/helper tests only; these never establish RUN-08 conformance."""

import copy
import json
from types import SimpleNamespace
import unittest
from unittest.mock import Mock

import accounting as a
from harness import Failure, Harness, OWNER


RUN = "0123456789ab"
CID = "a" * 64
NODE = "brewlet-v2-" + RUN + "-control-plane"


def pod():
    return {"metadata": {"name": "accounting-" + RUN, "namespace": "private",
                         "uid": "pod-uid", "labels": {OWNER: RUN}},
            "spec": {"nodeName": NODE},
            "status": {"phase": "Running", "containerStatuses": [
                {"name": a.CONTAINER, "containerID": "containerd://" + CID,
                 "restartCount": 0, "ready": True, "state": {"running": {}}}]}}


def node():
    return {"metadata": {"name": NODE, "uid": "node-uid"},
            "status": {"conditions": [{"type": "Ready", "status": "True"},
                                       {"type": "DiskPressure", "status": "False"}]}}


def stats():
    return {"stats": [{"attributes": {"id": CID, "metadata": {"name": a.CONTAINER},
                                      "labels": {"io.kubernetes.pod.uid": "pod-uid",
                                                 "io.kubernetes.pod.namespace": "private",
                                                 "io.kubernetes.pod.name": "accounting-" + RUN}},
                       "writableLayer": {"usedBytes": {"value": 4096}, "timestamp": "123",
                                         "fsId": {"mountpoint": "/var/lib/containerd"}}}]}


def probe():
    return {"uid": 65532, "euid": 65532, "tmpMode": 0o1777,
            "tmpRealPath": "/tmp", "rootDev": 123, "tmpDev": 123,
            "status": {"NoNewPrivs": "1", "CapEff": "0", "CapPrm": "0", "CapBnd": "0",
                       "CapInh": "0", "CapAmb": "0"},
            "mountinfo":
                "1 0 0:1 / / rw - overlay overlay rw,lowerdir=/runtime,"
                "upperdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/"
                "snapshots/123/fs,workdir=/var/lib/containerd/work\n"
                "2 1 0:2 / /app ro - overlay overlay ro,lowerdir=/app-layers\n"}


class AccountingTests(unittest.TestCase):
    def lookup(self, value):
        return a.usage(value, uid="pod-uid", namespace="private",
                       pod_name="accounting-" + RUN, cid=CID)

    def test_exact_cri_container_lookup(self):
        value = stats()
        other = copy.deepcopy(value["stats"][0])
        other["attributes"]["id"] = "b" * 64
        other["writableLayer"]["usedBytes"]["value"] = a.TOTAL_BYTES
        value["stats"].insert(0, other)
        self.assertEqual(self.lookup(value)["usedBytes"], 4096)

    def test_foreign_pod_container_name_namespace_or_id_rejected(self):
        for location, key, bad in (
            ("attributes", "id", "b" * 64),
            ("metadata", "name", "other"),
            ("labels", "io.kubernetes.pod.uid", "replacement"),
            ("labels", "io.kubernetes.pod.namespace", "foreign"),
            ("labels", "io.kubernetes.pod.name", "other")):
            with self.subTest(location=location, key=key):
                value = stats()
                attrs = value["stats"][0]["attributes"]
                target = attrs if location == "attributes" else attrs[location]
                target[key] = bad
                with self.assertRaises(Failure):
                    self.lookup(value)

    def test_missing_duplicate_fake_and_volume_only_stats_rejected(self):
        for value in (None, [], {}, {"stats": [None]}, {"stats": []},
                      {"pods": [{"rootfs": {"usedBytes": a.TOTAL_BYTES}}]},
                      {"stats": stats()["stats"] * 2},
                      {"stats": [{"volume": {"usedBytes": a.TOTAL_BYTES}}]}):
            with self.subTest(value=value), self.assertRaises(Failure):
                self.lookup(value)
        for good in (0, "0", 16777216, "16777216"):
            value = stats()
            value["stats"][0]["writableLayer"]["usedBytes"]["value"] = good
            self.assertEqual(self.lookup(value)["usedBytes"], int(good))
        for bad in (None, -1, True, "-1", "1.5", "", 2**64, "0" * 2):
            value = stats()
            value["stats"][0]["writableLayer"]["usedBytes"]["value"] = bad
            with self.subTest(bad=bad), self.assertRaises(Failure):
                self.lookup(value)
        for bad in (None, 0, "not-time"):
            value = stats()
            value["stats"][0]["writableLayer"]["timestamp"] = bad
            with self.subTest(timestamp=bad), self.assertRaises(Failure):
                self.lookup(value)

    def test_delta_uses_baseline_not_absolute_usage_and_fresh_timestamp(self):
        baseline = {"usedBytes": 100 * a.MIB, "timestampNS": 10}
        self.assertFalse(a.increased(baseline, {"usedBytes": 110 * a.MIB, "timestampNS": 11}))
        self.assertTrue(a.increased(baseline, {"usedBytes": 116 * a.MIB, "timestampNS": 11}))
        self.assertFalse(a.increased(baseline, {"usedBytes": 116 * a.MIB, "timestampNS": 10}))
        self.assertFalse(a.increased(baseline, {"usedBytes": 90 * a.MIB, "timestampNS": 11}))
        with self.assertRaises(Failure):
            a.increased(baseline, {"usedBytes": 116 * a.MIB, "timestampNS": 9})

    def test_only_real_limit_eviction_passes(self):
        status = {"phase": "Failed", "reason": "Evicted",
                  "message": "Container accounting exceeded its local ephemeral-storage limit 48Mi"}
        self.assertTrue(a.ephemeral_eviction({"status": status}))
        for actual in ('Container accounting exceeded its local ephemeral storage limit "48Mi".',
                       "Pod ephemeral local storage usage exceeds the total limit of containers 48Mi."):
            self.assertTrue(a.ephemeral_eviction({"status": dict(status, message=actual)}))
        for patch in ({"phase": "Running"}, {"phase": "Succeeded"}, {"reason": "Error"},
                      {"reason": "OOMKilled"}, {"message": "DiskPressure"},
                      {"message": "ephemeral-storage report is unavailable"},
                      {"message": "The node was low on resource: ephemeral-storage; exceeded limit"},
                      {"message": "Memory usage exceeds limit"}):
            with self.subTest(patch=patch):
                self.assertFalse(a.ephemeral_eviction({"status": dict(status, **patch)}))

    def test_node_identity_and_disk_pressure_guards(self):
        a.guard_node(node(), NODE, "node-uid", RUN)
        for obj, name, uid in ((node(), "foreign-node", "node-uid"),
                               (node(), NODE, "replacement")):
            with self.assertRaises(Failure):
                a.guard_node(obj, name, uid, RUN)
        for condition in ("True", "Unknown", None):
            obj = node()
            obj["status"]["conditions"][1]["status"] = condition
            with self.assertRaises(Failure):
                a.guard_node(obj, NODE, "node-uid", RUN)
        obj = node()
        obj["metadata"]["deletionTimestamp"] = "now"
        with self.assertRaises(Failure):
            a.guard_node(obj, NODE, "node-uid", RUN)

    def test_pod_and_container_replacement_refused(self):
        h = SimpleNamespace(run_id=RUN, node=NODE, namespace="private")
        a.guard_pod(pod(), h, "pod-uid", CID)
        for key, value in (("uid", "replacement"), ("namespace", "foreign"),
                           ("labels", {OWNER: "foreign"})):
            obj = pod()
            obj["metadata"][key] = value
            with self.assertRaises(Failure):
                a.guard_pod(obj, h, "pod-uid", CID)
        obj = pod()
        obj["status"]["containerStatuses"][0]["containerID"] = "containerd://" + "b" * 64
        with self.assertRaises(Failure):
            a.guard_pod(obj, h, "pod-uid", CID)
        obj = pod()
        obj["status"]["containerStatuses"][0]["restartCount"] = 1
        with self.assertRaises(Failure):
            a.guard_pod(obj, h, "pod-uid", CID)

    def test_mount_proof_requires_containerd_upper_readonly_app_and_security(self):
        proof = a.mount_proof(probe(), "/tmp/brewlet-accounting-" + RUN)
        self.assertTrue(proof["upperdir"].endswith("/snapshots/123/fs"))
        for old, new in (("upperdir=/var/lib/containerd/", "upperdir=/var/lib/brewlet/"),
                         ("/app ro", "/app rw"), ("/ rw", "/ ro")):
            value = probe()
            value["mountinfo"] = value["mountinfo"].replace(old, new)
            with self.subTest(old=old), self.assertRaises(Failure):
                a.mount_proof(value, "/tmp/brewlet-accounting-" + RUN)
        for key, bad in (("uid", 0), ("euid", 0), ("tmpMode", 0o755),
                         ("tmpRealPath", "/mounted-volume"), ("tmpDev", 999)):
            value = probe()
            value[key] = bad
            with self.assertRaises(Failure):
                a.mount_proof(value, "/tmp/brewlet-accounting-" + RUN)
        value = probe()
        value["status"]["NoNewPrivs"] = "0"
        with self.assertRaises(Failure):
            a.mount_proof(value, "/tmp/brewlet-accounting-" + RUN)
        for capability in ("CapEff", "CapPrm", "CapBnd", "CapInh", "CapAmb"):
            value = probe()
            value["status"][capability] = "1"
            with self.subTest(capability=capability), self.assertRaises(Failure):
                a.mount_proof(value, "/tmp/brewlet-accounting-" + RUN)

    def test_emptydir_tmpfs_bind_or_file_mount_cannot_proxy_usage(self):
        path = "/tmp/brewlet-accounting-" + RUN
        for target in ("/tmp", path, "/tmp/subdir"):
            for filesystem in ("tmpfs", "ext4", "overlay"):
                value = probe()
                value["mountinfo"] += f"3 1 0:3 / {target} rw - {filesystem} volume rw\n"
                with self.subTest(target=target, filesystem=filesystem), self.assertRaises(Failure):
                    a.mount_proof(value, path)

    def test_unrelated_workloads_cannot_disappear_restart_or_become_unready(self):
        original = pod()
        before = a.workload_snapshot([original])
        a.no_collateral(before, [copy.deepcopy(original)])
        for change in ("gone", "uid", "phase", "restart", "ready", "id"):
            obj = copy.deepcopy(original)
            if change == "uid":
                obj["metadata"]["uid"] = "replacement"
            elif change == "phase":
                obj["status"]["phase"] = "Failed"
            elif change in ("restart", "ready", "id"):
                key = {"restart": "restartCount", "ready": "ready", "id": "containerID"}[change]
                obj["status"]["containerStatuses"][0][key] = {"restart": 1, "ready": False,
                                                            "id": "containerd://other"}[change]
            with self.subTest(change=change), self.assertRaises(Failure):
                a.no_collateral(before, [] if change == "gone" else [obj])

    def test_manifest_security_no_volume_and_two_bounded_write_stages(self):
        h = SimpleNamespace(run_id=RUN, node=NODE, namespace="private",
                            app_images={"python": "fixture@sha256:" + CID})
        h.pod = Mock(return_value={"spec": {"containers": [
            {"image": h.app_images["python"], "imagePullPolicy": "Never"}]}})
        obj = a.manifest(h, "accounting-" + RUN)
        spec = obj["spec"]
        self.assertNotIn("volumes", spec)
        self.assertFalse(spec["automountServiceAccountToken"])
        self.assertEqual(obj["metadata"]["annotations"]["brewlet.sh/runtime-image"], "fixture-python")
        container = spec["containers"][0]
        self.assertNotIn("volumeMounts", container)
        self.assertFalse(container["securityContext"]["readOnlyRootFilesystem"])
        self.assertFalse(container["securityContext"]["allowPrivilegeEscalation"])
        self.assertEqual(container["securityContext"]["capabilities"]["drop"], ["ALL"])
        self.assertEqual(container["resources"]["limits"]["ephemeral-storage"], "48Mi")
        self.assertLess(a.FIRST_BYTES, a.LIMIT_BYTES)
        self.assertGreater(a.TOTAL_BYTES, a.LIMIT_BYTES)

    def test_accounting_uses_selected_remote_delivery_without_inheriting_volumes(self):
        image = "registry.test/python@sha256:" + CID
        base = {"spec": {"containers": [{"image": image, "imagePullPolicy": "Always"}],
                         "imagePullSecrets": [{"name": "owned-pull-secret"}],
                         "volumes": [{"name": "must-not-be-inherited"}]}}
        h = SimpleNamespace(run_id=RUN, node=NODE, namespace="private",
                            app_images={"python": image}, pod=Mock(return_value=base))
        result = a.manifest(h, "accounting-" + RUN)
        self.assertEqual(result["spec"]["containers"][0]["imagePullPolicy"], "Always")
        self.assertEqual(result["spec"]["imagePullSecrets"], [{"name": "owned-pull-secret"}])
        self.assertNotIn("volumes", result["spec"])
        self.assertNotIn("volumeMounts", result["spec"]["containers"][0])
        result["spec"]["imagePullSecrets"][0]["name"] = "changed"
        self.assertEqual(base["spec"]["imagePullSecrets"][0]["name"], "owned-pull-secret")
        self.assertEqual(a.TOTAL_BYTES, 80 * a.MIB)
        self.assertIn("os.O_NOFOLLOW", a.WRITE)
        self.assertIn("os.O_EXCL", a.WRITE)
        self.assertIn("os.urandom", a.WRITE)
        self.assertNotIn("truncate", a.WRITE)
        compile(a.WRITE, "accounting-write.py", "exec")
        compile(a.PROBE, "accounting-probe.py", "exec")

    def test_cleanup_refuses_replacement_or_untracked_uid(self):
        for replacement in (True, False):
            h = SimpleNamespace(run_id=RUN, node=NODE, node_uid="node-uid",
                                namespace="private", owned={}, delete=Mock(), wait=Mock())
            obj = pod()
            if replacement:
                obj["metadata"]["uid"] = "replacement"
                h.owned[("pod", "private", obj["metadata"]["name"])] = "pod-uid"
            h.get = Mock(side_effect=lambda kind, *args, **kwargs: node() if kind == "node" else obj)
            with self.assertRaises(Failure):
                a.cleanup(h, obj["metadata"]["name"], "pod-uid")
            h.delete.assert_not_called()

    def test_cleanup_harness_delete_uses_atomic_uid_precondition(self):
        h = Harness.__new__(Harness)
        h.run_id, h.namespace, h.node, h.node_uid = RUN, "private", NODE, "node-uid"
        name = "accounting-" + RUN
        h.owned = {("pod", h.namespace, name): "pod-uid"}
        h.get = Mock(side_effect=[pod(), node(), pod(), pod()])
        h.kubectl = Mock()
        h.wait = Mock()
        a.cleanup(h, name, "pod-uid")
        args, kwargs = h.kubectl.call_args
        self.assertEqual(args, ("delete", "--raw", "/api/v1/namespaces/private/pods/" + name,
                                "-f", "-"))
        self.assertEqual(json.loads(kwargs["input"])["preconditions"], {"uid": "pod-uid"})
        h.wait.assert_called_once()

    def test_cleanup_refuses_replaced_node_even_for_owned_pod(self):
        h = SimpleNamespace(run_id=RUN, node=NODE, node_uid="node-uid",
                            namespace="private",
                            owned={("pod", "private", "accounting-" + RUN): "pod-uid"},
                            delete=Mock(), wait=Mock())
        changed = node()
        changed["metadata"]["uid"] = "replacement-node"
        h.get = Mock(side_effect=[pod(), changed])
        with self.assertRaises(Failure):
            a.cleanup(h, "accounting-" + RUN, "pod-uid")
        h.delete.assert_not_called()

    def test_cleanup_of_owned_pod_is_still_possible_after_disk_pressure_failure(self):
        h = SimpleNamespace(run_id=RUN, node=NODE, node_uid="node-uid",
                            namespace="private",
                            owned={("pod", "private", "accounting-" + RUN): "pod-uid"},
                            delete=Mock(), wait=Mock())
        pressured = node()
        pressured["status"]["conditions"][1]["status"] = "True"
        h.get = Mock(side_effect=[pod(), pressured])
        a.cleanup(h, "accounting-" + RUN, "pod-uid")
        h.delete.assert_called_once_with("pod", "accounting-" + RUN, "private")

    def test_cleanup_already_absent_is_idempotent(self):
        h = SimpleNamespace(namespace="private", get=Mock(return_value=None), delete=Mock())
        a.cleanup(h, "accounting-" + RUN, "pod-uid")
        h.delete.assert_not_called()


class DiskHeadroomTests(unittest.TestCase):
    def config(self, **changes):
        result = {
            "evictionHard": {"nodefs.available": "0%", "imagefs.available": "0%"},
            "evictionSoft": None, "evictionMinimumReclaim": None,
            "mergeDefaultEvictionSettings": False, "imageGCHighThresholdPercent": 100,
        }
        result.update(changes)
        return result

    def test_pinned_kind_uses_real_disabled_global_thresholds_not_arbitrary_twenty_percent(self):
        self.assertEqual(a.DISK_RESERVE + a.TOTAL_BYTES,
                         a.required_headroom(141407907840, self.config()))

    def test_configured_eviction_and_gc_thresholds_remain_protected(self):
        capacity = 100 * 1024**3
        configuration = self.config(
            evictionHard={"nodefs.available": "10%", "imagefs.available": "15%"},
            imageGCHighThresholdPercent=85)
        self.assertEqual(15 * 1024**3 + a.DISK_RESERVE + a.TOTAL_BYTES,
                         a.required_headroom(capacity, configuration))

    def test_soft_threshold_and_minimum_reclaim_are_reserved(self):
        configuration = self.config(
            evictionSoft={"imagefs.available": "20%"},
            evictionMinimumReclaim={"imagefs.available": "1Gi"})
        self.assertEqual(21 * 1024**3 + a.DISK_RESERVE + a.TOTAL_BYTES,
                         a.required_headroom(100 * 1024**3, configuration))

    def test_unknown_or_merged_policy_fails_closed(self):
        for configuration in (
            self.config(mergeDefaultEvictionSettings=True),
            self.config(evictionHard={"imagefs.available": "invalid"}),
            self.config(imageGCHighThresholdPercent=None),
        ):
            with self.assertRaises(Failure):
                a.required_headroom(100 * 1024**3, configuration)

    def test_fractional_threshold_rounds_up(self):
        self.assertEqual(2, a.disk_quantity("15%", 11))
        self.assertEqual(1536, a.disk_quantity("1.5Ki", 1))
        for invalid in ("NaN%", "-1%", "101%", "unknown"):
            with self.assertRaises(Failure):
                a.disk_quantity(invalid, 100)


class InitialCollectionTests(unittest.TestCase):
    def test_zero_timestamp_is_pending_not_verified_zero_usage(self):
        raw = {"stats": [{
            "attributes": {"id": CID, "metadata": {"name": a.CONTAINER},
                           "labels": {"io.kubernetes.pod.uid": "pod-uid",
                                      "io.kubernetes.pod.namespace": "private",
                                      "io.kubernetes.pod.name": "accounting-" + RUN}},
            "writableLayer": {"timestamp": "0", "usedBytes": {"value": "0"}},
        }]}
        with self.assertRaises(a.StatsPending):
            a.usage(raw, uid="pod-uid", namespace="private",
                    pod_name="accounting-" + RUN, cid=CID)


if __name__ == "__main__":
    unittest.main()
