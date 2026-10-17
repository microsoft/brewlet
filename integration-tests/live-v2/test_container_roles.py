"""Offline role/identity safeguards only; not real-node execution evidence."""

import copy
import json
from types import SimpleNamespace
import unittest
from unittest.mock import Mock

import container_roles as roles
from harness import Failure, OWNER


RUN = "123456abcdef"


class RoleTests(unittest.TestCase):
    def base(self):
        return {"name": "app", "image": "registry.test/app@sha256:" + "a" * 64,
                "imagePullPolicy": "Always", "env": [{"name": "BREWLET_TEST_PORT", "value": "8000"}],
                "securityContext": {"runAsUser": 65532, "runAsGroup": 65532,
                                    "runAsNonRoot": True, "allowPrivilegeEscalation": False,
                                    "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}},
                "resources": {"requests": {"memory": "32Mi"}},
                "readinessProbe": {"exec": {"command": ["python3", "probe"]}},
                "volumeMounts": [{"name": "data", "mountPath": "/app/new-volume-target"}]}

    def test_native_sidecar_is_explicit_and_preserves_security_and_distinct_port(self):
        base = self.base()
        original = copy.deepcopy(base)
        native = roles.sidecar(base, "native", 8001, native=True)
        self.assertEqual(base, original)
        self.assertEqual(native["restartPolicy"], "Always")
        self.assertEqual(native["startupProbe"], base["readinessProbe"])
        self.assertNotIn("readinessProbe", native)
        self.assertEqual(native["securityContext"], base["securityContext"])
        self.assertEqual(native["env"], [{"name": "BREWLET_TEST_PORT", "value": "8001"}])

    def test_ephemeral_drops_unsupported_fields_without_dropping_execution_security(self):
        base = self.base()
        base["lifecycle"] = {"postStart": {"exec": {"command": ["test"]}}}
        ephemeral = roles.ephemeral(base, "debug")
        for field in ("resources", "readinessProbe", "lifecycle", "restartPolicy"):
            self.assertNotIn(field, ephemeral)
        self.assertEqual(ephemeral["securityContext"], base["securityContext"])
        self.assertEqual(ephemeral["volumeMounts"], base["volumeMounts"])
        self.assertEqual(ephemeral["targetContainerName"], "app")
        self.assertEqual(ephemeral["imagePullPolicy"], "Always")
        self.assertEqual(base["name"], "app")

    def fixture(self, state):
        pod = {"metadata": {"name": "role", "uid": "owned-pod", "labels": {OWNER: RUN}},
               "status": {"ephemeralContainerStatuses": [{"name": "rejected", **state}]}}
        h = SimpleNamespace(run_id=RUN, namespace="owned",
                            owned={("pod", "owned", "role"): "owned-pod"},
                            get=lambda *args: pod)
        return h, pod

    def test_failed_create_id_and_zero_timestamp_are_not_execution(self):
        h, pod = self.fixture({"containerID": "containerd://" + "b" * 64,
            "state": {"waiting": {"reason": "CrashLoopBackOff"}},
            "lastState": {"terminated": {"reason": "StartError",
                "startedAt": "1970-01-01T00:00:00Z", "message": "InvalidLaunchConfig"}}})
        self.assertEqual(roles.denied(h, "role", "rejected"), pod)

    def test_ephemeral_update_tests_uid_and_spec_and_preserves_existing_entries(self):
        h, pod = self.fixture({})
        pod["spec"] = {"containers": [self.base()],
                       "ephemeralContainers": [{"name": "existing", "image": "existing"}]}
        h.kubectl = Mock()
        roles.add_ephemeral(h, "role", roles.ephemeral(self.base(), "debug"))
        patch = json.loads(h.kubectl.call_args.args[-1])
        self.assertEqual(patch[0], {"op": "test", "path": "/metadata/uid", "value": "owned-pod"})
        self.assertEqual(patch[1], {"op": "test", "path": "/spec", "value": pod["spec"]})
        self.assertEqual([c["name"] for c in patch[2]["value"]], ["existing", "debug"])
        h.kubectl.reset_mock()
        with self.assertRaisesRegex(Failure, "name already exists"):
            roles.add_ephemeral(h, "role", roles.ephemeral(self.base(), "existing"))
        h.kubectl.assert_not_called()

    def test_actual_execution_or_unrelated_failure_cannot_count_as_rejection(self):
        for state in (
            {"state": {"running": {"startedAt": "2026-10-16T18:00:00Z"}}},
            {"state": {"terminated": {"reason": "Error", "startedAt": "2026-10-16T18:00:00Z",
                                     "message": "InvalidLaunchConfig"}}},
            {"state": {"terminated": {"reason": "StartError", "startedAt": "1970-01-01T00:00:00Z",
                                     "message": "RootNotSupported"}}},
        ):
            with self.subTest(state=state):
                h, _ = self.fixture(state)
                with self.assertRaises(Failure):
                    roles.denied(h, "role", "rejected")


if __name__ == "__main__":
    unittest.main()
