"""Offline tests: never contact Docker, kind, Kubernetes, or a registry."""

import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tomllib
from types import SimpleNamespace
import unittest
from unittest import mock
import uuid


HERE = Path(__file__).resolve().parent


def module(name):
    spec = importlib.util.spec_from_file_location("live_v2_" + name, HERE / (name + ".py"))
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


h = module("harness")
interceptor = module("interceptor")
RUN = "123456abcdef"
NODE_UID = "original-node-uid"
CONTAINER = "a" * 64


def owned(uid="resource-uid"):
    return {"metadata": {"uid": uid, "labels": {h.OWNER: RUN}}}


def docker_info(*, kind=False, uid=CONTAINER):
    return {"Id": uid, "Config": {"Labels":
            {"io.x-k8s.kind.cluster": "brewlet-v2-" + RUN} if kind else {h.OWNER: RUN}}}


class ProjectWork(unittest.TestCase):
    def setUp(self):
        self.work = HERE / (".unit-work-" + uuid.uuid4().hex)

    def tearDown(self):
        if self.work.exists():
            shutil.rmtree(self.work)

    def harness(self, **kwargs):
        instance = h.Harness(self.work, **kwargs)
        instance.run_id = RUN
        instance.cluster = "brewlet-v2-" + RUN
        instance.context = "kind-" + instance.cluster
        instance.namespace = instance.cluster
        instance.control_namespace = instance.cluster + "-system"
        instance.profile = instance.cluster
        instance.node = instance.cluster + "-control-plane"
        instance.node_uid = NODE_UID
        instance.arch = "arm64"
        instance.app_images = {lang: f"registry.brewlet.test/{RUN}/{lang}@sha256:" + "b" * 64
                               for lang in instance.languages}
        return instance


class SafetyTests(ProjectWork):
    def inventory(self):
        return {"apiVersion": h.API, "kind": "RuntimeImage",
                "metadata": {"name": "runtime", "uid": "inventory-uid", "generation": 7,
                             "resourceVersion": "old-status-version", "labels": {h.OWNER: RUN},
                             "finalizers": ["controller-owned"],
                             "annotations": {"node.brewlet.sh/inventory-journal": "controller-owned"}},
                "spec": {"source": {"image": "registry.test/runtime@sha256:" + "a" * 64}},
                "status": {"controller": "must-not-replay"}}

    def test_inventory_spec_cas_ignores_status_churn_without_replaying_metadata(self):
        original = self.inventory()
        desired = {"source": {"image": "registry.test/runtime@sha256:" + "b" * 64}}
        patch = h.inventory_spec_patch(original, RUN, "inventory-uid", desired)
        concurrent = copy.deepcopy(original)
        concurrent["metadata"]["resourceVersion"] = "new-status-version"
        concurrent["status"]["controller"] = "new observation"
        self.assertEqual(patch, h.inventory_spec_patch(concurrent, RUN, "inventory-uid", desired))
        self.assertEqual([p["path"] for p in patch],
                         ["/metadata/uid", "/metadata/generation", "/spec"])
        desired["source"]["image"] = "changed-after-building-patch"
        self.assertNotEqual(patch[-1]["value"], desired)
        changed_spec = copy.deepcopy(original)
        changed_spec["metadata"]["generation"] = 8
        self.assertNotEqual(patch[1], h.inventory_spec_patch(changed_spec, RUN, "inventory-uid")[1])

    def test_inventory_spec_cas_rejects_foreign_unknown_and_deleting_identity(self):
        for change in ({"uid": "recreated"}, {"generation": 0}, {"generation": True},
                       {"namespace": "foreign"}, {"deletionTimestamp": "now"}):
            obj = self.inventory()
            obj["metadata"].update(change)
            with self.subTest(change=change), self.assertRaises(h.Failure):
                h.inventory_spec_patch(obj, RUN, "inventory-uid")
        instance = self.harness()
        instance.kubectl = mock.Mock()
        with self.assertRaisesRegex(h.Failure, "unrecorded"):
            instance.replace_inventory_spec(self.inventory())
        instance.kubectl.assert_not_called()
        with self.assertRaisesRegex(h.Failure, "snapshots require"):
            instance.apply(self.inventory())
        instance.kubectl.assert_not_called()

    def test_inventory_spec_update_sends_atomic_tests_and_checks_response_uid(self):
        instance = self.harness()
        obj = self.inventory()
        instance.owned[("runtimeimage", "", "runtime")] = "inventory-uid"
        instance.kubectl = mock.Mock(return_value=subprocess.CompletedProcess([], 0, json.dumps(obj), ""))
        instance.replace_inventory_spec(obj)
        args, kwargs = instance.kubectl.call_args
        self.assertEqual(args[:5], ("patch", "runtimeimage", "runtime", "--type=json", "-p"))
        self.assertEqual(args[-2:], ("-o", "json"))
        self.assertEqual(json.loads(args[5])[1],
                         {"op": "test", "path": "/metadata/generation", "value": 7})
        self.assertEqual(kwargs, {})
        obj["metadata"]["uid"] = "replacement-after-update"
        instance.kubectl.return_value.stdout = json.dumps(obj)
        with self.assertRaises(h.Failure):
            instance.replace_inventory_spec(self.inventory())

    def test_positive_readiness_reports_terminal_start_error_without_timeout(self):
        instance = self.harness()
        instance.owned[("pod", instance.namespace, "positive")] = "pod-uid"
        obj = owned("pod-uid")
        obj["status"] = {"phase": "Failed", "containerStatuses": [
            {"name": "app", "state": {"terminated": {
                "exitCode": 128, "reason": "StartError",
                "message": "content digest sha256:test: not found"}}}]}
        instance.get = mock.Mock(return_value=obj)
        with self.assertRaisesRegex(h.Failure, "StartError exit=128 content digest"):
            instance.ready("positive")
        obj["status"] = {"phase": "Succeeded"}
        with self.assertRaisesRegex(h.Failure, "terminal Succeeded"):
            instance.ready("positive")
        obj["status"] = {"phase": "Running", "containerStatuses": [
            {"lastState": {"terminated": {"reason": "old failure"}}}]}
        self.assertFalse(instance.ready("positive"))

    def test_long_running_fixture_retries_create_without_masking_prior_execution(self):
        instance = self.harness()
        self.assertEqual(instance.pod("positive", "python")["spec"]["restartPolicy"], "Always")
        instance.owned[("pod", instance.namespace, "positive")] = "pod-uid"
        obj = owned("pod-uid")
        obj["status"] = {"phase": "Running", "conditions": [{"type": "Ready", "status": "True"}],
            "containerStatuses": [{"name": "app", "restartCount": 2, "state": {"running": {}},
                "lastState": {"terminated": {"reason": "StartError", "exitCode": 128,
                    "startedAt": "1970-01-01T00:00:00Z",
                    "message": "RuntimeImageInstalling: allowed bounded Create retry"}}}]}
        instance.get = mock.Mock(return_value=obj)
        self.assertEqual(instance.ready("positive"), obj)
        prior = obj["status"]["containerStatuses"][0]["lastState"]["terminated"]
        for reason, code in (("Error", 1), ("OOMKilled", 137), ("Completed", 0), ("StartError", 128)):
            prior.update(reason=reason, exitCode=code, startedAt="2026-10-17T08:00:00Z")
            with self.subTest(reason=reason), self.assertRaisesRegex(h.Failure, "actually exited"):
                instance.ready("positive")

    def test_worker_failure_logs_are_scoped_to_owned_profile_uid(self):
        instance = self.harness()
        instance.kubeconfig.write_text("test-only configuration")
        instance.owned[("nodeprofile", "", instance.profile)] = "owned-profile-uid"
        instance.kubectl = mock.Mock(return_value=subprocess.CompletedProcess([], 0, "bounded logs", ""))
        instance.diagnostics()
        args = instance.kubectl.call_args.args
        self.assertEqual(args[:3], ("-n", instance.control_namespace, "logs"))
        self.assertIn("brewlet.sh/owner-uid=owned-profile-uid", args)
        self.assertIn("--tail=1000", args)
        self.assertEqual((self.work / "diagnostic-workers.log").read_text(), "bounded logs")

    def test_owned_node_exec_forwards_stdin_including_empty_payload(self):
        instance = self.harness()
        instance.node_container = CONTAINER
        for value in ("private input", ""):
            instance.run = mock.Mock(side_effect=[
                subprocess.CompletedProcess([], 0, json.dumps([docker_info(kind=True)]), ""),
                subprocess.CompletedProcess([], 0, "", "")])
            instance.node_exec("cat", input=value)
            call = instance.run.call_args
            self.assertEqual(call.args[0], ["docker", "exec", "-i", CONTAINER, "cat"])
            self.assertEqual(call.kwargs["input"], value)

    def test_owned_node_exec_without_stdin_is_noninteractive(self):
        instance = self.harness()
        instance.node_container = CONTAINER
        instance.run = mock.Mock(side_effect=[
            subprocess.CompletedProcess([], 0, json.dumps([docker_info(kind=True)]), ""),
            subprocess.CompletedProcess([], 0, "", "")])
        instance.node_exec("true")
        self.assertEqual(instance.run.call_args.args[0], ["docker", "exec", CONTAINER, "true"])

    def test_fresh_kind_node_explicitly_enables_cri_trusted_registry_hosts(self):
        instance = self.harness()
        def run(command, **kwargs):
            if command[:3] == ["kind", "create", "cluster"]:
                raise h.Failure("stop before creating a real cluster")
            return subprocess.CompletedProcess(command, 0, "", "")
        instance.run = mock.Mock(side_effect=run)
        with self.assertRaisesRegex(h.Failure, "stop before creating"):
            instance.provision_cluster()
        config = json.loads((self.work / "kind.json").read_text())
        self.assertEqual(config["nodes"], [{"role": "control-plane", "image": h.NODE_IMAGE}])
        patch = tomllib.loads(config["containerdConfigPatches"][0])
        self.assertEqual(patch["plugins"]["io.containerd.grpc.v1.cri"]["registry"]["config_path"],
                         "/etc/containerd/certs.d")
        self.assertNotIn("skip_verify", config["containerdConfigPatches"][0])

    def test_additional_native_worker_starts_with_explicit_startup_fence(self):
        instance = self.harness()
        def run(command, **kwargs):
            if command[:3] == ["kind", "create", "cluster"]:
                raise h.Failure("stop before creating a real cluster")
            return subprocess.CompletedProcess(command, 0, "", "")
        instance.run = mock.Mock(side_effect=run)
        with self.assertRaisesRegex(h.Failure, "stop before creating"):
            instance.provision_cluster(workers=1)
        config = json.loads((self.work / "kind.json").read_text())
        self.assertEqual(len(config["nodes"]), 2)
        worker = config["nodes"][1]
        self.assertEqual(worker["role"], "worker")
        self.assertEqual(worker["image"], h.NODE_IMAGE)
        self.assertIn("key: " + h.FENCE, worker["kubeadmConfigPatches"][0])
        self.assertIn("effect: NoSchedule", worker["kubeadmConfigPatches"][0])

    def test_worker_exec_revalidates_api_uid_and_exact_owned_container(self):
        instance = self.harness()
        name = instance.cluster + "-worker"
        instance.node_containers[name] = CONTAINER
        instance.node_uids[name] = NODE_UID
        instance.get = mock.Mock(return_value={"metadata": {"uid": NODE_UID}})
        info = {**docker_info(kind=True), "Name": "/" + name}
        instance.run = mock.Mock(side_effect=[
            subprocess.CompletedProcess([], 0, json.dumps([info]), ""),
            subprocess.CompletedProcess([], 0, "", "")])
        instance.node_exec_on(name, "cat", input="")
        self.assertEqual(instance.run.call_args.args[0], ["docker", "exec", "-i", CONTAINER, "cat"])
        instance.get.return_value = {"metadata": {"uid": "replacement"}}
        instance.run.reset_mock()
        with self.assertRaisesRegex(h.Failure, "Node UID changed"):
            instance.node_exec_on(name, "true")
        instance.run.assert_not_called()

    def test_multi_node_destroy_requires_every_recorded_id_and_no_extra_container(self):
        instance = self.harness()
        for mutation in ("none", "missing", "extra", "replacement", "foreign", "name"):
            with self.subTest(mutation=mutation):
                first, second = instance.cluster + "-control-plane", instance.cluster + "-worker"
                instance.node_containers = {first: CONTAINER, second: "worker-id"}
                infos = [{**docker_info(kind=True), "Name": "/" + first},
                         {**docker_info(kind=True, uid="worker-id"), "Name": "/" + second}]
                if mutation == "missing":
                    infos.pop()
                elif mutation == "extra":
                    infos.append({**docker_info(kind=True, uid="unexpected"), "Name": "/unexpected"})
                elif mutation == "replacement":
                    infos[1]["Id"] = "replacement"
                elif mutation == "foreign":
                    infos[1]["Config"]["Labels"] = {}
                elif mutation == "name":
                    infos[1]["Name"] = "/different"
                commands, removed = [], []
                def run(command, **kwargs):
                    commands.append(command)
                    if command[:3] == ["docker", "ps", "-aq"]:
                        stdout = "" if removed else "\n".join(i["Id"] for i in infos)
                    elif command[:2] == ["docker", "inspect"]:
                        stdout = json.dumps([next(i for i in infos if i["Id"] == command[-1])])
                    else:
                        if command[:3] == ["kind", "delete", "cluster"]:
                            removed.append(True)
                        stdout = ""
                    return subprocess.CompletedProcess(command, 0, stdout, "")
                instance.run = run
                errors = instance.destroy()
                self.assertEqual(bool(errors), mutation != "none")
                self.assertEqual(bool(removed), mutation == "none")

    def test_lease_uses_recorded_pod_node_and_refuses_foreign_hosts(self):
        instance = self.harness()
        worker = instance.cluster + "-worker"
        instance.node_containers[worker] = "worker-id"
        expected = {"taskID": CONTAINER, "namespace": "k8s.io"}
        response = subprocess.CompletedProcess([], 0, json.dumps(expected), "")
        instance.node_exec = mock.Mock(return_value=response)
        instance.node_exec_on = mock.Mock(return_value=response)
        obj = owned()
        obj.update(spec={"nodeName": worker}, status={"containerStatuses": [
            {"name": "app", "containerID": "containerd://" + CONTAINER}]})
        self.assertEqual(instance.lease(obj), expected)
        instance.node_exec.assert_not_called()
        self.assertEqual(instance.node_exec_on.call_args.args,
                         (worker, "cat", h.STORE + "/.tasks/" + CONTAINER + "/lease.json"))
        instance.node_exec_on.reset_mock()
        obj["spec"]["nodeName"] = "foreign-node"
        with self.assertRaisesRegex(h.Failure, "recorded private Node"):
            instance.lease(obj)
        instance.node_exec_on.assert_not_called()

    def test_named_lease_finds_init_and_ephemeral_without_selecting_main(self):
        instance = self.harness()
        obj = owned()
        obj.update(spec={"nodeName": instance.node}, status={
            "containerStatuses": [{"name": "app", "containerID": "containerd://" + "a" * 64}],
            "initContainerStatuses": [{"name": "native", "containerID": "containerd://" + "b" * 64}],
            "ephemeralContainerStatuses": [{"name": "debug", "containerID": "containerd://" + "c" * 64}]})
        def execute(command, path):
            task = path.split("/")[-2]
            return subprocess.CompletedProcess([], 0, json.dumps({"taskID": task, "namespace": "k8s.io"}), "")
        instance.node_exec = mock.Mock(side_effect=execute)
        self.assertEqual(instance.lease(obj, "native")["taskID"], "b" * 64)
        self.assertEqual(instance.lease(obj, "debug")["taskID"], "c" * 64)
        instance.node_exec.reset_mock()
        obj["status"]["ephemeralContainerStatuses"].append(
            {"name": "native", "containerID": "containerd://" + "d" * 64})
        with self.assertRaisesRegex(h.Failure, "ambiguous"):
            instance.lease(obj, "native")
        instance.node_exec.assert_not_called()

    def test_external_worker_decommission_is_exact_and_preserves_identity_history(self):
        instance = self.harness()
        name = instance.cluster + "-worker"
        instance.node_containers = {instance.node: "primary-id", name: CONTAINER}
        instance.node_uids[name] = "old-worker"
        node = {"metadata": {"name": name, "uid": "old-worker"}}
        instance.get = mock.Mock(return_value=node)
        instance.run = mock.Mock(side_effect=[
            subprocess.CompletedProcess([], 0, json.dumps([
                {**docker_info(kind=True), "Name": "/" + name}]), ""),
            subprocess.CompletedProcess([], 0, CONTAINER, ""),
            subprocess.CompletedProcess([], 1, "", "error: no such object: " + CONTAINER)])
        frozen, container = instance.decommission_worker(name)
        self.assertEqual(frozen, node)
        self.assertEqual(container, CONTAINER)
        self.assertEqual(instance.run.call_args_list[1].args[0],
                         ["docker", "rm", "-f", "-v", CONTAINER])
        self.assertEqual(instance.node_containers, {instance.node: "primary-id"})
        self.assertEqual(instance.retired_node_containers[name], CONTAINER)
        self.assertEqual(instance.node_uids[name], "old-worker")

    def test_acknowledged_removal_keeps_cleanup_inventory_correct_when_inspection_fails(self):
        instance = self.harness()
        name = instance.cluster + "-worker"
        instance.node_containers = {instance.node: "primary-id", name: CONTAINER}
        instance.node_uids[name] = "old-worker"
        instance.get = mock.Mock(return_value={"metadata": {"name": name, "uid": "old-worker"}})
        instance.run = mock.Mock(side_effect=[
            subprocess.CompletedProcess([], 0, json.dumps([
                {**docker_info(kind=True), "Name": "/" + name}]), ""),
            subprocess.CompletedProcess([], 0, CONTAINER, ""),
            subprocess.CompletedProcess([], 1, "", "Docker daemon connection refused")])
        with self.assertRaisesRegex(h.Failure, "not confirmed"):
            instance.decommission_worker(name)
        self.assertNotIn(name, instance.node_containers)
        self.assertEqual(instance.retired_node_containers[name], CONTAINER)
        self.assertEqual(instance.node_uids[name], "old-worker")

    def test_external_decommission_refuses_control_plane_and_recreated_worker(self):
        instance = self.harness()
        instance.run = mock.Mock()
        with self.assertRaisesRegex(h.Failure, "never the control plane"):
            instance.decommission_worker(instance.node)
        name = instance.cluster + "-worker"
        instance.node_containers[name] = CONTAINER
        instance.node_uids[name] = "old-worker"
        instance.get = mock.Mock(return_value={"metadata": {"uid": "replacement"}})
        with self.assertRaisesRegex(h.Failure, "Node UID changed"):
            instance.decommission_worker(name)
        instance.run.assert_not_called()

    def test_inventory_webhook_readiness_retries_only_owned_transport_failures(self):
        namespace = "owned-system"
        prefix = ('Error from server (InternalError): failed calling webhook "runtimeimages.brewlet.sh": '
                  'failed to call webhook: Post "https://brewlet-admission.' + namespace
                  + '.svc:443/validate-runtimeimages?timeout=5s": ')
        for cause in ("connect: connection refused", "context deadline exceeded",
                      "i/o timeout", "tls handshake timeout"):
            self.assertTrue(h.inventory_webhook_transport_pending(prefix + cause, namespace))
        self.assertTrue(h.inventory_webhook_transport_pending(
            'failed calling webhook "runtimeimages.brewlet.sh": failed to call webhook: '
            'no endpoints available for service "brewlet-admission"', namespace))
        for message in (prefix.replace(namespace, "foreign") + "context deadline exceeded",
                        prefix.replace("runtimeimages.brewlet.sh", "other.brewlet.sh") + "i/o timeout",
                        prefix + "x509: certificate signed by unknown authority",
                        prefix + "x509: certificate signed by unknown authority; context deadline exceeded",
                        'admission webhook "runtimeimages.brewlet.sh" denied the request: '
                        + prefix + "context deadline exceeded",
                        "Error from server (Forbidden): denied", "unknown admission failure"):
            self.assertFalse(h.inventory_webhook_transport_pending(message, namespace))

    def test_inventory_webhook_startup_timeout_uses_existing_bound_and_never_retries_denial(self):
        instance = self.harness()
        timeout = ('Error from server (InternalError): failed calling webhook "runtimeimages.brewlet.sh": '
                   'failed to call webhook: Post "https://brewlet-admission.'
                   + instance.control_namespace
                   + '.svc:443/validate-runtimeimages?timeout=5s": context deadline exceeded')
        instance.kubectl = mock.Mock(side_effect=[
            subprocess.CompletedProcess([], 1, "", timeout),
            subprocess.CompletedProcess([], 0, "created (server dry run)", "")])

        def wait(_, probe, *, timeout):
            self.assertEqual(timeout, 60)
            self.assertFalse(probe())
            return probe()

        instance.wait = mock.Mock(side_effect=wait)
        instance.wait_inventory_admission("registry/runtime@sha256:" + "a" * 64)
        self.assertEqual(instance.kubectl.call_count, 2)
        instance.kubectl = mock.Mock(return_value=subprocess.CompletedProcess(
            [], 1, "", 'admission webhook "runtimeimages.brewlet.sh" denied the request: invalid source'))
        with self.assertRaisesRegex(h.Failure, "inventory admission failed"):
            instance.wait_inventory_admission("registry/runtime@sha256:" + "a" * 64)
        self.assertEqual(instance.kubectl.call_count, 1)

    def test_runtime_builds_do_not_import_unused_roots_before_authenticated_publication(self):
        instance = self.harness()
        instance.runtime_vendor_inputs = h.rotation_inputs(json.loads(
            (h.FIXTURES / "runtimes/rotation.json").read_text()))
        instance.build = mock.Mock()
        instance.run = mock.Mock(return_value=subprocess.CompletedProcess(
            [], 0, json.dumps([{"Config": {"Env": ["PATH=/usr/bin:/bin"]}}]), ""))
        instance.load_image = mock.Mock(side_effect=AssertionError("unused runtime imported"))
        instance.ctr = mock.Mock(side_effect=AssertionError("unused runtime alias created"))
        instance.build_runtime_images()
        self.assertEqual(instance.build.call_count, 16)
        self.assertEqual(instance.runtime_images, {language: {} for language in h.LANGUAGES})
        self.assertEqual(set(instance.runtime_paths), set(h.LANGUAGES))
        for language in h.LANGUAGES:
            for revision in ("before", "after"):
                dockerfile = (instance.work / (language + "-" + revision) / "Dockerfile").read_text()
                for field in ('ENTRYPOINT ["/ignored-runtime-entrypoint"]',
                              'CMD ["ignored-runtime-argument"]',
                              "WORKDIR /ignored-runtime-workdir", "USER 0:0",
                              "EXPOSE 65432", 'VOLUME ["/ignored-runtime-volume"]',
                              'HEALTHCHECK CMD ["/ignored-runtime-healthcheck"]'):
                    self.assertIn(field + "\n", dockerfile)
                self.assertIn("ENV BREWLET_RUNTIME_REVISION=" + revision, dockerfile)
        instance.load_image.assert_not_called()
        instance.ctr.assert_not_called()
        instance.kubectl = mock.Mock()
        with self.assertRaisesRegex(h.Failure, "published by authenticated delivery"):
            instance.install()
        instance.kubectl.assert_not_called()

    def test_failed_held_exec_is_reported_before_polling_the_pod(self):
        instance = self.harness()
        process = mock.Mock()
        process.poll.return_value = 1
        instance.exec = mock.Mock()
        instance.wait = mock.Mock(side_effect=lambda _, probe, **kwargs: probe())
        try:
            with mock.patch.object(h.subprocess, "Popen", return_value=process):
                with self.assertRaisesRegex(h.Failure, "python held exec exited.*status 1"):
                    instance.start_held_exec("python")
            instance.exec.assert_not_called()
        finally:
            for _, log in instance.helpers:
                log.close()

    def test_smaller_local_mode_cannot_remove_languages_from_complete_acceptance(self):
        for scenario, pinned in (("complete", True), ("complete", False), ("control", True)):
            with self.subTest(scenario=scenario, pinned=pinned):
                with self.assertRaisesRegex(h.Failure, "requires"):
                    self.harness(scenario=scenario, pinned=pinned, local_language="python")
                self.assertFalse(self.work.exists())
        for scenario, pinned, language in (("control", False, "java"), ("runtime", True, "node"),
                                            ("runtime", False, None), ("runtime", False, "unknown"),
                                            ("maven", False, "python"), ("lifecycle", False, "java"),
                                            ("maven", False, None), ("lifecycle", False, None),
                                            ("roles", False, "java"), ("accounting", False, None),
                                            ("delivery", True, "python")):
            with self.subTest(scenario=scenario, pinned=pinned, language=language):
                with self.assertRaises(h.Failure):
                    self.harness(scenario=scenario, pinned=pinned, local_language=language)
                self.assertFalse(self.work.exists())

    def test_smaller_local_mode_builds_and_offers_only_its_selected_runtime(self):
        instance = self.harness(scenario="control", pinned=False, local_language="python")
        instance.runtime_vendor_inputs = h.rotation_inputs(json.loads(
            (h.FIXTURES / "runtimes/rotation.json").read_text()))
        instance.build = mock.Mock()
        instance.run = mock.Mock(return_value=subprocess.CompletedProcess(
            [], 0, json.dumps([{"Config": {"Env": ["PATH=/usr/bin:/bin"]}}]), ""))
        instance.build_runtime_images()
        self.assertEqual(instance.languages, ("python",))
        self.assertEqual(instance.runtime_images, {"python": {}})
        self.assertEqual(instance.app_images.keys(), {"python"})
        self.assertEqual(instance.runtime_paths, {"python": "/usr/bin:/bin"})
        self.assertEqual(instance.build.call_count, 4)
        self.assertEqual(instance.profile_object()["spec"]["runtimeImages"],
                         [{"id": h.IDS["python"], "install": "OnDemand"}])

    def test_single_runtime_security_and_barrier_probes_use_the_selected_fixture(self):
        instance = self.harness(scenario="runtime", pinned=False, local_language="node")
        instance.initial = {"node": {"metadata": {"generation": 1, "inventoryUID": "epoch"}}}
        instance.bypass_admission = mock.Mock()
        instance.restore_admission = mock.Mock()
        instance.apply = mock.Mock()
        instance.no_start = mock.Mock()
        instance.start_held_exec = mock.Mock(side_effect=RuntimeError("rotation reached"))
        with self.assertRaisesRegex(RuntimeError, "rotation reached"):
            instance.barriers_and_rotation()
        self.assertEqual(instance.apply.call_count, 5)
        for call in instance.apply.call_args_list:
            pod = call.args[0]
            self.assertEqual(pod["metadata"]["annotations"]["brewlet.sh/runtime-image"], h.IDS["node"])
            self.assertEqual(pod["spec"]["containers"][0]["image"], instance.app_images["node"])
        instance.restore_admission.assert_called_once()
        instance.start_held_exec.assert_called_once_with("node")

    def test_child_process_gets_private_kubeconfig_and_checkout_scratch(self):
        instance = self.harness()
        result = instance.run([sys.executable, "-c",
                               "import os,json; print(json.dumps([os.environ['KUBECONFIG'],"
                               "os.environ['TMPDIR']]))"])
        self.assertEqual(json.loads(result.stdout), [str(instance.kubeconfig), str(self.work)])
        self.assertTrue((self.work / "command-0001.log").exists())

    def test_command_timeout_is_bounded_and_retains_partial_evidence(self):
        instance = self.harness()
        with self.assertRaises(h.Failure):
            instance.run(["/bin/sh", "-c", "printf started; sleep 30"], timeout=2)
        self.assertIn("started", (self.work / "command-0001.log").read_text())

    def test_run_uid_grammar(self):
        self.assertEqual(h.validate_run_id(RUN), RUN)
        for value in ("", "../kind", "default", "ABCDEF123456", "123456789ab", RUN + "0"):
            with self.subTest(value=value), self.assertRaises(h.Failure):
                h.validate_run_id(value)

    def test_resource_requires_label_server_uid_and_exact_epoch(self):
        self.assertEqual(h.require_owned(owned(), RUN), "resource-uid")
        for obj, uid in (({"metadata": {"uid": "u"}}, None), (owned(""), None),
                         (owned(), "recreated-uid")):
            with self.subTest(obj=obj), self.assertRaises(h.Failure):
                h.require_owned(obj, RUN, uid)
        foreign = owned()
        foreign["metadata"]["labels"][h.OWNER] = "abcdef123456"
        with self.assertRaises(h.Failure):
            h.require_owned(foreign, RUN)

    def test_docker_identity_and_ownership_are_both_mandatory(self):
        h.docker_owned(docker_info(), RUN, CONTAINER)
        h.docker_owned(docker_info(kind=True), RUN, CONTAINER, kind_node=True)
        with self.assertRaises(h.Failure):
            h.docker_owned(docker_info(uid="replacement"), RUN, CONTAINER)
        foreign = docker_info(kind=True)
        foreign["Config"]["Labels"]["io.x-k8s.kind.cluster"] = "shared"
        with self.assertRaises(h.Failure):
            h.docker_owned(foreign, RUN, CONTAINER, kind_node=True)

    def test_kubectl_always_pins_private_config_and_context(self):
        command = h.kube_command(self.work.resolve() / "kubeconfig", "kind-brewlet-v2-" + RUN,
                                 ["get", "nodes"])
        self.assertEqual(command[:5], ["kubectl", "--kubeconfig", str(self.work / "kubeconfig"),
                                      "--context", "kind-brewlet-v2-" + RUN])
        self.assertIn("--request-timeout=30s", command)
        self.assertIn("--request-timeout=0", h.kube_command(
            self.work.resolve() / "kubeconfig", "kind-brewlet-v2-" + RUN, ["exec"], stream=True))
        for args in (["--context=shared"], ["--kubeconfig", "/shared"], ["--server=https://shared"],
                     ["-s", "https://shared"], ["--token=other"], ["--insecure-skip-tls-verify"]):
            with self.subTest(args=args), self.assertRaises(h.Failure):
                h.kube_command(self.work.resolve() / "kubeconfig", "kind-brewlet-v2-" + RUN, args)
        with self.assertRaises(h.Failure):
            h.kube_command(self.work.resolve() / "kubeconfig", "kind", ["get", "nodes"])

    def test_existing_work_directory_is_refused(self):
        self.work.mkdir()
        with self.assertRaises(h.Failure):
            h.Harness(self.work)

    def test_work_outside_checkout_is_refused_without_creating_it(self):
        with self.assertRaises(h.Failure):
            h.Harness(Path("/not-this-checkout/brewlet-acceptance"))

    def test_save_serializes_uid_ownership_without_tuple_keys(self):
        instance = self.harness()
        instance.owned[("pod", instance.namespace, "java-old")] = "pod-uid"
        instance.save()
        state = json.loads((self.work / "result.json").read_text())
        self.assertEqual(state["ownership"]["objects"][0]["uid"], "pod-uid")
        self.assertFalse(state["fullAcceptance"])

    def test_node_exec_refuses_changed_container_before_exec(self):
        instance = self.harness()
        instance.node_container = CONTAINER
        instance.run = mock.Mock(return_value=subprocess.CompletedProcess(
            [], 0, json.dumps([docker_info(kind=True, uid="new")]), ""))
        with self.assertRaises(h.Failure):
            instance.node_exec("rm", "-rf", "/opt/brewlet")
        self.assertEqual(instance.run.call_count, 1)

    def test_node_snapshot_refuses_recreated_node(self):
        instance = self.harness()
        instance.get = mock.Mock(return_value={"metadata": {"uid": "replacement"}})
        with self.assertRaises(h.Failure):
            instance.node_snapshot()

    def test_delete_uses_atomic_uid_precondition(self):
        instance = self.harness()
        key = ("pod", instance.namespace, "test")
        instance.owned[key] = "pod-uid"
        instance.get = mock.Mock(return_value=owned("pod-uid"))
        instance.kubectl = mock.Mock()
        instance.delete("pod", "test", instance.namespace)
        args, kwargs = instance.kubectl.call_args
        self.assertEqual(args[:2], ("delete", "--raw"))
        self.assertIn("/namespaces/" + instance.namespace + "/pods/test", args[2])
        self.assertEqual(json.loads(kwargs["input"])["preconditions"], {"uid": "pod-uid"})

    def test_delete_foreign_or_replaced_object_does_not_issue_delete(self):
        instance = self.harness()
        instance.owned[("pod", instance.namespace, "test")] = "old"
        instance.get = mock.Mock(return_value=owned("new"))
        instance.kubectl = mock.Mock()
        with self.assertRaises(h.Failure):
            instance.delete("pod", "test", instance.namespace)
        instance.kubectl.assert_not_called()

    def test_destroy_refuses_foreign_node_and_registry(self):
        instance = self.harness()
        instance.node_container = CONTAINER
        instance.registry_id = "registry-id"
        commands = []
        def run(command, **kwargs):
            commands.append(command)
            if command[:3] == ["docker", "ps", "-aq"]:
                return subprocess.CompletedProcess(command, 0, CONTAINER, "")
            return subprocess.CompletedProcess(command, 0, json.dumps([{
                "Id": "foreign-id", "Config": {"Labels": {}}}]), "")
        instance.run = run
        errors = instance.destroy()
        self.assertEqual(len(errors), 2)
        self.assertFalse(any(command[:3] == ["kind", "delete", "cluster"] for command in commands))
        self.assertFalse(any(command[:2] == ["docker", "rm"] for command in commands))

    def test_destroy_removes_only_exact_owned_names_not_image_ids(self):
        instance = self.harness()
        instance.node_container = CONTAINER
        instance.registry_id = "registry-id"
        tag = "registry.brewlet.test/" + RUN + "/test:v2"
        instance.images[tag] = "image-id"
        commands = []
        removed = set()
        def run(command, **kwargs):
            commands.append(command)
            if command[:3] == ["docker", "ps", "-aq"]:
                stdout = "" if "cluster" in removed else CONTAINER
            elif command[:2] == ["docker", "inspect"]:
                if command[-1] in removed:
                    return subprocess.CompletedProcess(command, 1, "", "not found")
                stdout = json.dumps([docker_info(kind=command[-1] == CONTAINER, uid=command[-1])])
            elif command[:3] == ["docker", "image", "inspect"]:
                stdout = json.dumps([docker_info(uid="image-id")])
            else:
                if command[:3] == ["kind", "delete", "cluster"]:
                    removed.add("cluster")
                if command[:2] == ["docker", "rm"]:
                    removed.add(command[-1])
                stdout = ""
            return subprocess.CompletedProcess(command, 0, stdout, "")
        instance.run = run
        self.assertEqual(instance.destroy(), [])
        self.assertIn(["docker", "rm", "-f", "-v", "registry-id"], commands)
        self.assertIn(["docker", "image", "rm", tag], commands)
        self.assertFalse(any(command == ["docker", "image", "rm", "image-id"] for command in commands))

    def test_destroy_refuses_retargeted_image_tag(self):
        instance = self.harness()
        instance.images["registry.brewlet.test/owned/image:v2"] = "original"
        instance.run = mock.Mock(return_value=subprocess.CompletedProcess(
            [], 0, json.dumps([docker_info(uid="replacement")]), ""))
        self.assertEqual(len(instance.destroy()), 1)
        self.assertEqual(instance.run.call_count, 1)


class ContractTests(ProjectWork):
    def test_all_language_pods_keep_ro_root_missing_volume_target_and_real_exec_probe(self):
        instance = self.harness()
        for language in h.LANGUAGES:
            pod = instance.pod("contract", language, overrides=True)
            container = pod["spec"]["containers"][0]
            self.assertTrue(container["securityContext"]["readOnlyRootFilesystem"])
            self.assertEqual(container["volumeMounts"][0]["mountPath"], "/app/new-volume-target")
            self.assertEqual(container["imagePullPolicy"], "Never")
            self.assertIn("--probe", container["readinessProbe"]["exec"]["command"])
            self.assertEqual(container["workingDir"], "/home/app")
            self.assertEqual(container["args"][-1], "two words")
            self.assertEqual(pod["spec"]["nodeSelector"], {"kubernetes.io/arch": "arm64"})
            self.assertNotIn("nodeName", pod["spec"])
            self.assertNotIn("tolerations", pod["spec"])

    def test_bound_negative_has_explicit_nonroot_independent_of_admission(self):
        instance = self.harness()
        barrier = {"id": h.IDS["python"], "inventoryUID": "epoch", "generation": 3}
        pod = instance.pod("negative", "python", barrier=barrier, bound=True)
        self.assertEqual(pod["spec"]["nodeName"], instance.node)
        security = pod["spec"]["containers"][0]["securityContext"]
        self.assertEqual((security["runAsUser"], security["runAsGroup"]), (65532, 65532))
        self.assertFalse(security["allowPrivilegeEscalation"])
        self.assertEqual(security["capabilities"]["drop"], ["ALL"])
        self.assertEqual(json.loads(pod["metadata"]["annotations"][
            "brewlet.sh/rotated-to-generation"]), barrier)

    def test_profile_has_explicit_controlplane_pool_and_exact_toleration(self):
        profile = self.harness().profile_object()
        self.assertTrue(profile["spec"]["nodePool"]["includeControlPlane"])
        self.assertEqual(profile["spec"]["nodePool"]["key"], "brewlet.test/pool")
        self.assertEqual(profile["spec"]["tolerations"], [{"key": h.FENCE, "operator": "Equal",
                                                         "value": "provisioning", "effect": "NoSchedule"}])
        self.assertTrue(all(e["install"] == "OnDemand" for e in profile["spec"]["runtimeImages"]))

    def test_evidence_never_equates_import_to_pull_tls_or_full_acceptance(self):
        result = h.classify_evidence([{"outcome": "passed"}], ["missing real-node scenario"])
        self.assertTrue(result["selectedAssertionsPassed"])
        for key in ("fullAcceptance", "kubeletPullVerified", "tlsTransportVerified",
                    "providerScaleFromZeroVerified"):
            self.assertFalse(result[key])
        self.assertFalse(h.classify_evidence([], [])["fullAcceptance"])
        self.assertFalse(h.classify_evidence([{"outcome": "passed"}], [])["fullAcceptance"])
        self.assertFalse(h.classify_evidence([{"outcome": "failed"}], [])["selectedAssertionsPassed"])

    def test_failure_class_keeps_environment_errors_separate_from_contract_failures(self):
        self.assertEqual(h.failure_class("prerequisites", "tool missing"), "infrastructure")
        self.assertEqual(h.failure_class("build", "failed to resolve registry: no such host"), "infrastructure")
        self.assertEqual(h.failure_class("execution", "runtime PATH mismatch"), "acceptance")

    def test_no_start_requires_exact_reason_and_scopes_events_to_pod_uid(self):
        instance = self.harness()
        instance.owned[("pod", instance.namespace, "negative")] = "pod-uid"
        pod = owned("pod-uid")
        pod["status"] = {"containerStatuses": [{"state": {"waiting": {
            "reason": "CreateContainerError",
            "message": "context deadline exceeded; private-fixture-detail"}}}]}
        instance.get = mock.Mock(side_effect=lambda kind, *args, **kwargs:
                                {"items": [{"involvedObject": {"uid": "foreign"},
                                             "message": "RuntimeImageRevoked"}]} if kind == "events" else pod)
        instance.wait = lambda description, predicate, **kwargs: h.require(
            predicate(), "expected failure was not observed")
        with self.assertRaises(h.Failure):
            instance.no_start("negative", "RuntimeImageRevoked")
        diagnostic = instance.result["startRejectionDiagnostics"]["negative"]
        self.assertEqual(diagnostic["podUID"], "pod-uid")
        self.assertEqual(diagnostic["runtimeReasons"], [])
        self.assertEqual(diagnostic["eventReasons"], [])
        self.assertEqual(diagnostic["containerReasons"], ["CreateContainerError"])
        self.assertTrue(diagnostic["requestDeadlineExceeded"])
        self.assertNotIn("private-fixture-detail", json.dumps(instance.result))

    def test_no_start_cannot_accept_started_container_with_failure_event(self):
        instance = self.harness()
        instance.owned[("pod", instance.namespace, "negative")] = "pod-uid"
        pod = owned("pod-uid")
        pod["status"] = {"containerStatuses": [{"containerID": "containerd://running",
                                               "state": {"running": {}}}]}
        instance.get = mock.Mock(return_value=pod)
        instance.wait = lambda description, predicate, **kwargs: predicate()
        with self.assertRaises(h.Failure):
            instance.no_start("negative", "RuntimeImageRevoked")

    def test_inner_application_index_not_outer_layout_index(self):
        layout = self.work
        blobs = layout / "blobs/sha256"
        blobs.mkdir(parents=True)
        def put(value):
            data = json.dumps(value, sort_keys=True).encode()
            digest = hashlib.sha256(data).hexdigest()
            (blobs / digest).write_bytes(data)
            return {"digest": "sha256:" + digest, "size": len(data)}
        config = put({"config": {}})
        manifest = put({"annotations": {"brewlet.sh/artifact-version": "2"}, "config": config,
                        "layers": [{"annotations": {"brewlet.sh/layer": role}}
                                   for role in ("dependency", "app", "launch")]})
        inner = put({"manifests": [manifest]})
        (layout / "index.json").write_text(json.dumps({"manifests": [inner]}))
        self.assertEqual(h.artifact_target(layout)[0], inner["digest"])
        (blobs / inner["digest"][7:]).write_bytes(b"corrupted")
        with self.assertRaises(h.Failure):
            h.artifact_target(layout)


class CallbackOrderTests(ProjectWork):
    def test_vendor_update_proof_checks_distinct_old_and_replacement_pods(self):
        instance = self.harness()
        instance.runtime_vendor_inputs = {
            "java": {"probe": ["java", "-version"],
                     "before": {"version": "21.0.7"}, "after": {"version": "21.0.8"}}}
        instance.exec = mock.Mock(side_effect=[
            subprocess.CompletedProcess([], 0, "", 'openjdk version "21.0.7"'),
            subprocess.CompletedProcess([], 0, "", 'openjdk version "21.0.8"')])
        result = instance.rotated_vendor_versions("java", "java-new")
        self.assertIn("21.0.7", result["before"])
        self.assertIn("21.0.8", result["after"])
        self.assertEqual([call.args[0] for call in instance.exec.call_args_list], ["java-old", "java-new"])
        instance.exec.reset_mock()
        with self.assertRaisesRegex(h.Failure, "distinct replacement"):
            instance.rotated_vendor_versions("java", "java-old")
        instance.exec.assert_not_called()

    def test_rotation_pins_select_distinct_real_vendor_versions(self):
        inputs = json.loads((h.FIXTURES / "runtimes/rotation.json").read_text())
        self.assertEqual(h.rotation_inputs(inputs), inputs)
        for language in h.LANGUAGES:
            invalid = copy.deepcopy(inputs)
            invalid[language]["after"] = invalid[language]["before"]
            with self.assertRaises(h.Failure):
                h.rotation_inputs(invalid)
        invalid = copy.deepcopy(inputs)
        invalid["python"]["before"]["image"] = "python:latest"
        with self.assertRaises(h.Failure):
            h.rotation_inputs(invalid)
        invalid["python"] = None
        with self.assertRaises(h.Failure):
            h.rotation_inputs(invalid)

    def test_vendor_probe_matches_complete_versions_not_prefixes(self):
        self.assertTrue(h.runtime_version_matches('openjdk version "21.0.7" 2025-04-15', "21.0.7"))
        self.assertTrue(h.runtime_version_matches("Microsoft.NETCore.App 10.0.1 [/usr/share/dotnet]",
                                                 "Microsoft.NETCore.App 10.0.1 "))
        self.assertFalse(h.runtime_version_matches("Python 3.13.50", "Python 3.13.5"))
        self.assertFalse(h.runtime_version_matches("v22.18.01", "v22.18.0"))

    def test_remote_evidence_requires_completed_pull_and_tls_proofs(self):
        receipt = {"bootstrapOnly": False, "kubeletPullVerified": True,
                   "runtimeSourcePullVerified": True, "tlsTransportVerified": True}
        for key in receipt:
            partial = dict(receipt)
            partial.pop(key)
            self.assertFalse(h.classify_evidence([{"outcome": "passed"}], [], partial)[
                "kubeletPullVerified"])
        result = h.classify_evidence([{"outcome": "passed"}], ["provider capacity"], receipt)
        self.assertTrue(result["kubeletPullVerified"])
        self.assertTrue(result["tlsTransportVerified"])
        self.assertFalse(result["fullAcceptance"])
        self.assertFalse(result["providerScaleFromZeroVerified"])

    def test_clean_pass_requires_completed_suite_and_fixture_destruction(self):
        instance = self.harness()
        instance.records = [{"outcome": "passed"}]
        instance.save()
        self.assertFalse(instance.result["cleanSelectedSuitePassed"])
        instance.result["outcome"] = "passed"
        instance.save()
        self.assertFalse(instance.result["cleanSelectedSuitePassed"])
        instance.result["fixtureDestruction"] = {"outcome": "passed"}
        instance.save()
        self.assertTrue(instance.result["cleanSelectedSuitePassed"])

    def configure(self, *, bootstrap_failure=False, cleanup_failure=False, observer_failure=False,
                  scenario="complete", retain_on_failure=False, local_language=None):
        instance = self.harness(scenario=scenario, retain_on_failure=retain_on_failure,
                                local_language=local_language, pinned=local_language is None)
        calls = []
        for name in ("preflight", "provision_cluster", "prepare_images", "install",
                     "startup", "application_execution", "barriers_and_rotation",
                     "pinned_revocation", "production_cleanup", "stop_helpers",
                     "diagnostics", "save"):
            setattr(instance, name, mock.Mock(side_effect=lambda name=name: calls.append(name)))
        instance.destroy = mock.Mock(side_effect=lambda: calls.append("destroy") or [])
        def callback(name, fail=False):
            def invoke(_):
                calls.append(name)
                if fail:
                    raise h.Failure(name + " failed")
            return invoke
        class Observer:
            def __init__(self, _):
                pass

            def __enter__(self):
                calls.append("observer_start")
                return self

            def verify(self):
                callback("observability", observer_failure)(instance)

            def __exit__(self, *_):
                calls.append("observer_stop")

        modules = {
            "observability": SimpleNamespace(Observer=Observer),
            "delivery": SimpleNamespace(
                bootstrap=callback("bootstrap_delivery", bootstrap_failure),
                execute=callback("delivery"),
                cleanup=callback("cleanup_delivery", cleanup_failure)),
            "accounting": SimpleNamespace(execute=callback("accounting")),
            "maven_node": SimpleNamespace(execute=callback("maven_node")),
            "container_roles": SimpleNamespace(execute=callback("container_roles")),
            "lifecycle": SimpleNamespace(execute=callback("lifecycle")),
            "control_edges": SimpleNamespace(
                execute=callback("control_edges"),
                before_cleanup=callback("before_cleanup"),
                after_cleanup=callback("after_cleanup")),
        }
        return instance, calls, modules

    def test_selected_callbacks_are_mandatory_and_ordered(self):
        instance, calls, modules = self.configure()
        with mock.patch.dict(sys.modules, modules):
            self.assertEqual(instance.execute(), 0)
        self.assertEqual(calls, [
            "preflight", "provision_cluster", "prepare_images", "bootstrap_delivery",
            "install", "startup", "observer_start", "application_execution", "barriers_and_rotation",
            "observability", "observer_stop",
            "accounting", "delivery", "maven_node", "container_roles", "lifecycle",
            "control_edges", "pinned_revocation",
            "before_cleanup", "production_cleanup", "after_cleanup",
            "stop_helpers", "diagnostics", "cleanup_delivery", "destroy", "save"])
        self.assertEqual(instance.result["outcome"], "passed")

    def test_observability_failure_cannot_be_reported_as_a_pass_or_skip_cleanup(self):
        instance, calls, modules = self.configure(observer_failure=True)
        with mock.patch.dict(sys.modules, modules):
            self.assertEqual(instance.execute(), 1)
        self.assertEqual(instance.result["failurePhase"], "observability")
        self.assertIn("observer_stop", calls)
        self.assertNotIn("accounting", calls)
        self.assertEqual(calls[-5:], [
            "stop_helpers", "diagnostics", "cleanup_delivery", "destroy", "save"])

    def test_explicit_local_retention_keeps_failed_resources_but_stops_helpers(self):
        instance, calls, modules = self.configure(observer_failure=True, retain_on_failure=True)
        with mock.patch.dict(sys.modules, modules):
            self.assertEqual(instance.execute(), 1)
        self.assertEqual(instance.result["outcome"], "failed")
        self.assertEqual(instance.result["fixtureDestruction"]["outcome"], "retained")
        self.assertFalse(instance.result["fullAcceptance"])
        self.assertIn("stop_helpers", calls)
        self.assertIn("diagnostics", calls)
        self.assertNotIn("cleanup_delivery", calls)
        self.assertNotIn("destroy", calls)
        self.assertTrue(any("owner-checked cleanup required" in gap
                            for gap in instance.result["gaps"]))

    def test_local_retention_option_never_skips_cleanup_after_success(self):
        instance, calls, modules = self.configure(retain_on_failure=True)
        with mock.patch.dict(sys.modules, modules):
            self.assertEqual(instance.execute(), 0)
        self.assertIn("cleanup_delivery", calls)
        self.assertIn("destroy", calls)
        self.assertNotEqual(instance.result.get("fixtureDestruction", {}).get("outcome"), "retained")

    def test_control_only_scope_keeps_real_bootstrap_fencing_and_cleanup_but_records_omissions(self):
        instance, calls, modules = self.configure(scenario="control")
        with mock.patch.dict(sys.modules, modules):
            self.assertEqual(instance.execute(), 0)
        self.assertEqual(instance.result["selectedScenario"], "control")
        self.assertTrue(any("Focused control execution omits" in gap
                            for gap in instance.result["gaps"]))
        self.assertFalse(instance.result["fullAcceptance"])
        for required in ("bootstrap_delivery", "startup", "observability", "control_edges",
                         "pinned_revocation", "before_cleanup", "production_cleanup",
                         "after_cleanup", "cleanup_delivery", "destroy"):
            self.assertIn(required, calls)
        for omitted in ("accounting", "delivery", "maven_node", "container_roles", "lifecycle"):
            self.assertNotIn(omitted, calls)

    def test_smaller_local_scope_preserves_real_control_and_marks_language_omissions(self):
        instance, calls, modules = self.configure(scenario="control", local_language="python")
        with mock.patch.dict(sys.modules, modules):
            self.assertEqual(instance.execute(), 0)
        self.assertEqual(instance.result["selectedLanguages"], ["python"])
        self.assertFalse(instance.result["fullAcceptance"])
        self.assertIn("control_edges", calls)
        self.assertIn("production_cleanup", calls)
        self.assertNotIn("pinned_revocation", calls)
        self.assertTrue(any("omits java, node, dotnet" in gap
                            for gap in instance.result["gaps"]))

    def test_runtime_only_scope_keeps_execution_and_production_cleanup_without_control_hooks(self):
        instance, calls, modules = self.configure(scenario="runtime", local_language="java")
        with mock.patch.dict(sys.modules, modules):
            self.assertEqual(instance.execute(), 0)
        self.assertEqual(instance.result["selectedLanguages"], ["java"])
        self.assertFalse(instance.result["fullAcceptance"])
        for required in ("bootstrap_delivery", "startup", "application_execution",
                         "barriers_and_rotation", "observability", "production_cleanup", "destroy"):
            self.assertIn(required, calls)
        for omitted in ("control_edges", "before_cleanup", "after_cleanup", "pinned_revocation",
                        "accounting", "delivery", "maven_node", "container_roles", "lifecycle"):
            self.assertNotIn(omitted, calls)
        self.assertTrue(any("Local runtime-only execution omits" in gap
                            for gap in instance.result["gaps"]))

    def test_local_maven_scope_runs_producer_and_cleanup_without_unselected_callbacks(self):
        instance, calls, modules = self.configure(scenario="maven", local_language="java")
        with mock.patch.dict(sys.modules, modules):
            self.assertEqual(instance.execute(), 0)
        self.assertEqual(instance.result["selectedLanguages"], ["java"])
        self.assertFalse(instance.result["fullAcceptance"])
        self.assertIn("maven_node", calls)
        self.assertIn("production_cleanup", calls)
        for omitted in ("control_edges", "before_cleanup", "after_cleanup", "lifecycle",
                        "container_roles", "accounting", "delivery", "pinned_revocation"):
            self.assertNotIn(omitted, calls)

    def test_local_lifecycle_scope_runs_real_callback_and_cleanup_without_other_suites(self):
        instance, calls, modules = self.configure(scenario="lifecycle", local_language="python")
        with mock.patch.dict(sys.modules, modules):
            self.assertEqual(instance.execute(), 0)
        self.assertEqual(instance.result["selectedLanguages"], ["python"])
        self.assertFalse(instance.result["fullAcceptance"])
        self.assertIn("lifecycle", calls)
        self.assertIn("production_cleanup", calls)
        for omitted in ("control_edges", "before_cleanup", "after_cleanup", "maven_node",
                        "container_roles", "accounting", "delivery", "pinned_revocation"):
            self.assertNotIn(omitted, calls)

    def test_selected_local_maven_failure_is_not_skipped_or_reported_as_success(self):
        instance, calls, modules = self.configure(scenario="maven", local_language="java")
        modules["maven_node"].execute = mock.Mock(side_effect=h.Failure("producer failed"))
        with mock.patch.dict(sys.modules, modules):
            self.assertEqual(instance.execute(), 1)
        self.assertEqual(instance.result["failurePhase"], "maven-producer-node")
        self.assertEqual(instance.result["outcome"], "failed")
        self.assertIn("destroy", calls)

    def test_local_role_accounting_and_delivery_callbacks_are_selected_explicitly(self):
        original = self.work
        try:
            for scenario, selected in (("roles", "container_roles"), ("accounting", "accounting"),
                                       ("delivery", "delivery")):
                self.work = original / scenario
                instance, calls, modules = self.configure(scenario=scenario, local_language="python")
                with self.subTest(scenario=scenario), mock.patch.dict(sys.modules, modules):
                    self.assertEqual(instance.execute(), 0)
                self.assertEqual(instance.result["selectedLanguages"], ["python"])
                self.assertFalse(instance.result["fullAcceptance"])
                self.assertIn(selected, calls)
                self.assertIn("production_cleanup", calls)
                self.assertTrue(any("Local " + scenario + " execution omits callbacks" in gap
                                    for gap in instance.result["gaps"]))
                for other in ("maven_node", "lifecycle", "control_edges", "before_cleanup", "after_cleanup",
                              "container_roles", "accounting", "delivery", "pinned_revocation"):
                    if other != selected:
                        self.assertNotIn(other, calls)
        finally:
            self.work = original

    def test_partial_bootstrap_still_cleans_registry_and_cluster(self):
        instance, calls, modules = self.configure(bootstrap_failure=True)
        with mock.patch.dict(sys.modules, modules):
            self.assertEqual(instance.execute(), 1)
        self.assertNotIn("install", calls)
        self.assertEqual(calls[-5:], [
            "stop_helpers", "diagnostics", "cleanup_delivery", "destroy", "save"])
        self.assertEqual(instance.result["failurePhase"], "remote-delivery-bootstrap")

    def test_registry_cleanup_error_cannot_skip_cluster_destruction(self):
        instance, calls, modules = self.configure(cleanup_failure=True)
        with mock.patch.dict(sys.modules, modules):
            self.assertEqual(instance.execute(), 1)
        self.assertIn("destroy", calls)
        self.assertEqual(instance.result["outcome"], "failed")


class InterceptorTests(unittest.TestCase):
    def request(self):
        node = {"metadata": {"name": "test-node", "uid": NODE_UID,
                             "labels": {"brewlet.sh/runtime.python-3.12": "ready"}},
                "spec": {"taints": []}}
        return {"resource": {"resource": "nodes"}, "oldObject": node, "object": copy.deepcopy(node)}

    def config(self, **extra):
        return {"node": "test-node", "nodeUID": NODE_UID, "revision": 1, **extra}

    def test_real_admission_decision_refuses_label_removal_and_value_change(self):
        for value in (None, "available"):
            request = self.request()
            labels = request["object"]["metadata"]["labels"]
            key = "brewlet.sh/runtime.python-3.12"
            if value is None:
                labels.pop(key)
            else:
                labels[key] = value
            allowed, reason = interceptor.decision(request, self.config(pinnedLabels=[key]))
            self.assertFalse(allowed)
            self.assertIn("provider-pinned label refusal", reason)

    def test_pool_owner_can_initialize_once_but_neither_owner_nor_worker_can_mutate_pin(self):
        key = "brewlet.sh/runtime.stale"
        config = self.config(pinnedLabels=[key], initialPinnedLabels={key: "ready"},
                             poolOwner="fixture-pool-owner")
        request = self.request()
        request["userInfo"] = {"username": "fixture-pool-owner"}
        request["object"]["metadata"]["labels"][key] = "ready"
        self.assertTrue(interceptor.decision(request, config)[0])
        for mutation in ("worker-initialization", "wrong-value", "remove", "change"):
            with self.subTest(mutation=mutation):
                attempt = copy.deepcopy(request)
                if mutation == "worker-initialization":
                    attempt["userInfo"]["username"] = "system:serviceaccount:system:worker"
                elif mutation == "wrong-value":
                    attempt["object"]["metadata"]["labels"][key] = "available"
                else:
                    attempt["oldObject"]["metadata"]["labels"][key] = "ready"
                    if mutation == "remove":
                        attempt["object"]["metadata"]["labels"].pop(key)
                    else:
                        attempt["object"]["metadata"]["labels"][key] = "available"
                self.assertFalse(interceptor.decision(attempt, config)[0])

    def test_interceptor_cannot_pin_foreign_node_and_refuses_same_name_replacement(self):
        request = self.request()
        request["object"]["metadata"]["name"] = "foreign"
        self.assertTrue(interceptor.decision(request, self.config(denyFence=True))[0])
        request = self.request()
        request["object"]["metadata"]["uid"] = "recreated"
        self.assertFalse(interceptor.decision(request, self.config())[0])

    def test_fallback_taint_can_be_refused_independently(self):
        request = self.request()
        request["object"]["spec"]["taints"].append(
            {"key": h.FENCE, "value": "provisioning", "effect": "NoSchedule"})
        self.assertFalse(interceptor.decision(request, self.config(denyFence=True))[0])
        self.assertTrue(interceptor.decision(request, self.config(denyFence=False))[0])
        self.assertTrue(interceptor.decision(self.request(), self.config(denyFence=True))[0])


if __name__ == "__main__":
    unittest.main()
