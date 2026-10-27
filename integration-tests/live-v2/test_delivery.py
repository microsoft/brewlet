"""Offline tests only: no Docker daemon, cluster, network, or secret exports."""

import base64
import gzip
import io
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import unittest
from unittest.mock import Mock, patch
import uuid

import delivery
from harness import Failure, OWNER


class Work(unittest.TestCase):
    def setUp(self):
        self.work = Path(__file__).resolve().parent / (".delivery-unit-" + uuid.uuid4().hex)
        self.work.mkdir(mode=0o700)
        self.addCleanup(shutil.rmtree, self.work)

    def fixture(self, arch="arm64"):
        layout = self.work / "fixture"
        layer, app_diff = delivery.tar_layer([("app/main.py", b"print('hello')\n", 0o644)])
        app = delivery.blob(layout, layer, delivery.LAYER)
        app["annotations"] = {"brewlet.sh/layer": "app"}
        launch = {"schemaVersion": 1, "process": {"entrypoint": ["python3"], "args": ["/app/main.py"]}}
        layer, launch_diff = delivery.tar_layer([("launch.json", json.dumps(launch).encode(), 0o644)])
        launch_layer = delivery.blob(layout, layer, delivery.LAYER)
        launch_layer["annotations"] = {"brewlet.sh/layer": "launch"}
        config = delivery.json_blob(layout, {"architecture": arch, "os": "linux",
            "config": {"Entrypoint": ["python3"], "Cmd": ["/app/main.py"], "WorkingDir": "/app"},
            "rootfs": {"type": "layers", "diff_ids": [app_diff, launch_diff]}}, delivery.CONFIG)
        target = delivery.json_blob(layout, {"schemaVersion": 2, "mediaType": delivery.MANIFEST,
            "annotations": {"brewlet.sh/artifact-version": "1"}, "config": config,
            "layers": [app, launch_layer]}, delivery.MANIFEST)
        delivery.finish_layout(layout, target)
        return layout, target

    def unpack(self, layout, target):
        manifest = json.loads(delivery.read_blob(layout, target))
        config = json.loads(delivery.read_blob(layout, manifest["config"]))
        return manifest, config

    def test_private_permissions_and_symlink_rejection(self):
        secret = delivery.private_file(self.work / "nested/key", "private")
        self.assertEqual(stat.S_IMODE(secret.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(secret.parent.stat().st_mode), 0o700)
        link = self.work / "link"
        link.symlink_to(secret)
        with self.assertRaises(OSError):
            delivery.private_file(link, "must not replace")
        self.assertEqual(secret.read_text(), "private")

    def test_redaction_covers_raw_password_and_authorization(self):
        password = "a-private-password"
        basic = base64.b64encode((delivery.USERNAME + ":" + password).encode()).decode()
        encoded = base64.b64encode(password.encode()).decode()
        text = delivery.redact(f"{password} Authorization: Basic {basic} {encoded}", password)
        for value in (password, basic, encoded):
            self.assertNotIn(value, text)

    def test_full_reference_identity(self):
        target = delivery.digest(b"manifest bytes")
        self.assertEqual(delivery.canonical("172.18.0.4:5000", "delivery/apps-python", target),
                         "172.18.0.4:5000/delivery/apps-python@" + target)
        for host in ("https://registry:5000", "registry", "user:secret@registry:5000"):
            with self.assertRaises(Failure):
                delivery.canonical(host, "delivery/app", target)
        for target in ("tag", "sha256:" + "F" * 64, "sha256:short"):
            with self.assertRaises(Failure):
                delivery.canonical("registry:5000", "delivery/app", target)

    def test_native_platform_never_inherits_docker_default(self):
        with patch.dict(os.environ, {"DOCKER_DEFAULT_PLATFORM": "linux/amd64"}):
            self.assertEqual(delivery.native_platform("arm64"), "linux/arm64")
        for unsupported in ("aarch64", "amd64/v9", "", "darwin/arm64"):
            with self.assertRaises(Failure):
                delivery.native_platform(unsupported)

    def test_blob_size_and_digest_identity(self):
        layout = self.work / "layout"
        descriptor = delivery.blob(layout, b"verified", delivery.CONFIG)
        self.assertEqual(delivery.read_blob(layout, descriptor), b"verified")
        descriptor["size"] += 1
        with self.assertRaises(Failure):
            delivery.read_blob(layout, descriptor)
        descriptor["size"] -= 1
        path = layout / "blobs/sha256" / descriptor["digest"][7:]
        path.write_bytes(b"tampered")
        with self.assertRaises(Failure):
            delivery.read_blob(layout, descriptor)

    def test_mutation_creates_fresh_digest_without_changing_application_bytes(self):
        layout, original = self.fixture()
        before, _ = self.unpack(layout, original)
        target_layout = self.work / "fresh"
        target = delivery.mutate_application(layout, target_layout, "unique-run")
        manifest, config = self.unpack(target_layout, target)
        self.assertNotEqual(target["digest"], original["digest"])
        self.assertEqual(manifest["layers"][0], before["layers"][0])
        self.assertIn("BREWLET_DELIVERY_RUN=unique-run", config["config"]["Env"])
        launch = gzip.decompress(delivery.read_blob(target_layout, manifest["layers"][-1]))
        self.assertEqual(delivery.digest(launch), config["rootfs"]["diff_ids"][-1])
        import tarfile
        with tarfile.open(fileobj=io.BytesIO(launch)) as archive:
            document = json.load(archive.extractfile("launch.json"))
        self.assertIn({"name": "BREWLET_DELIVERY_RUN", "value": "unique-run"}, document["process"]["env"])

    def test_unsafe_layer_recomputes_digest_and_diff_id(self):
        layout, _ = self.fixture()
        destination = self.work / "unsafe"
        target = delivery.mutate_application(layout, destination, "unsafe", fault="unsafe")
        manifest, config = self.unpack(destination, target)
        self.assertEqual(len(manifest["layers"]), 3)
        raw = gzip.decompress(delivery.read_blob(destination, manifest["layers"][1]))
        self.assertEqual(config["rootfs"]["diff_ids"][1], delivery.digest(raw))
        import tarfile
        with tarfile.open(fileobj=io.BytesIO(raw)) as archive:
            self.assertEqual(archive.getnames(), ["etc/delivery-outside-app"])

    def test_malformed_launch_is_valid_oci_but_invalid_descriptor(self):
        layout, _ = self.fixture()
        destination = self.work / "malformed"
        target = delivery.mutate_application(layout, destination, "malformed", fault="malformed")
        manifest, _ = self.unpack(destination, target)
        raw = gzip.decompress(delivery.read_blob(destination, manifest["layers"][-1]))
        import tarfile
        with tarfile.open(fileobj=io.BytesIO(raw)) as archive:
            self.assertTrue(json.load(archive.extractfile("launch.json"))["unexpectedField"])

    def test_mirror_mismatch_crafted_without_corrupting_identity(self):
        layout, _ = self.fixture()
        destination = self.work / "mirror"
        target = delivery.mutate_application(layout, destination, "mirror", fault="mirror")
        _, config = self.unpack(destination, target)
        self.assertEqual(config["config"]["WorkingDir"], "/must-not-match-launch")

    def test_absent_platform_does_not_fall_back(self):
        layout, _ = self.fixture("arm64")
        with self.assertRaisesRegex(Failure, "platform absent"):
            delivery.mutate_application(layout, self.work / "wrong", "case", platform="amd64")

    def test_static_golden_real_files_and_both_explicit_platforms(self):
        layout = self.work / "static"
        descriptor = delivery.static_runtime(layout)
        index = json.loads(delivery.read_blob(layout, descriptor))
        self.assertEqual({entry["platform"]["architecture"] for entry in index["manifests"]},
                         {"amd64", "arm64"})
        layers = []
        for entry in index["manifests"]:
            manifest, config = self.unpack(layout, entry)
            self.assertEqual(config["config"]["Env"], ["PATH=/usr/bin"])
            self.assertEqual(config["architecture"], entry["platform"]["architecture"])
            raw = gzip.decompress(delivery.read_blob(layout, manifest["layers"][0]))
            self.assertEqual(delivery.digest(raw), config["rootfs"]["diff_ids"][0])
            layers.append(manifest["layers"][0]["digest"])
            import tarfile
            with tarfile.open(fileobj=io.BytesIO(raw)) as archive:
                self.assertIn(b"app:x:65532:65532", archive.extractfile("etc/passwd").read())
                self.assertNotIn("app", archive.getnames())
        self.assertEqual(layers[0], layers[1])

    def test_layer_determinism_and_executable_mode(self):
        first, diff = delivery.tar_layer([("app/native", b"\x7fELF", 0o755)])
        second, second_diff = delivery.tar_layer([("app/native", b"\x7fELF", 0o755)])
        self.assertEqual((first, diff), (second, second_diff))
        import tarfile
        with tarfile.open(fileobj=io.BytesIO(gzip.decompress(first))) as archive:
            self.assertEqual(archive.getmember("app/native").mode, 0o755)


class FakeHarness:
    run_id = "123456abcdef"
    cluster = "brewlet-v2-" + run_id
    namespace = cluster
    arch = "arm64"
    languages = ("java", "python", "node", "dotnet")
    probe_language = "python"
    kubeconfig = Path("/private/invocation/kubeconfig")

    def __init__(self, work):
        self.work = work
        self.calls = []
        self.removed = False
        self.cli_command = ["docker", "run", "--rm", "--platform", "linux/arm64",
            "--network", "none", "--mount", f"type=bind,source={work},target={work}",
            delivery.CLI_USERLAND]

    def pod(self, *args, **kwargs):
        return {"spec": {"nodeSelector": {"kubernetes.io/arch": "arm64"}, "containers": [
            {"image": "registry/app@sha256:" + "2" * 64, "imagePullPolicy": "Never"}]}}

    def run(self, command, **kwargs):
        self.calls.append(command)
        if command[1:3] == ["rm", "--force"]:
            self.removed = True
        if command[1:3] == ["inspect", "registry-id"] and not self.removed:
            return subprocess.CompletedProcess(command, 0, json.dumps([{
                "Id": "registry-id", "Config": {"Labels": {OWNER: self.run_id}}}]), "")
        return subprocess.CompletedProcess(command, 1 if command[1] == "inspect" else 0, "", "")


class CommandTests(unittest.TestCase):
    setUp = Work.setUp

    def state(self):
        h = FakeHarness(self.work)
        state = delivery.Delivery(h)
        state.authority = "172.18.0.9:5000"
        state.untrusted_authority = "owned-registry:5000"
        return h, state

    def test_bootstrap_uses_selected_fixture_for_positive_and_negative_graphs(self):
        h, state = self.state()
        h.languages = ("java",)
        h.probe_language = "java"
        h.app_images = {}
        h.runtime_images = {"java": {}}
        h.result = {}
        h.save = Mock()
        descriptor = {"digest": "sha256:" + "a" * 64}
        state.start_registry = Mock()
        state.configure_node = Mock()
        state.publish = Mock(return_value="registry/fixture@sha256:" + "c" * 64)
        state.fresh = Mock()
        state.golden_layout = Mock(return_value=(self.work, descriptor))
        state.prepare_native = Mock()
        state.registry_info = Mock()
        state.producer_authority = "localhost:5443"
        blob = json.dumps({"layers": [{"digest": "sha256:" + "b" * 64}]}).encode()
        with patch.object(delivery, "mutate_application", return_value=descriptor) as mutate, \
                patch.object(delivery, "read_blob", return_value=blob), \
                patch.object(delivery, "static_runtime", return_value=descriptor):
            state.bootstrap()
        self.assertEqual(mutate.call_count, 7)
        self.assertTrue(all(call.args[0] == self.work / "oci-java"
                            for call in mutate.call_args_list))
        self.assertEqual(set(h.runtime_images["java"]), {"before", "after"})
        self.assertEqual(set(h.app_images), {"java"})
        self.assertEqual([entry.args for entry in state.golden_layout.call_args_list],
                         [("java", "before"), ("java", "after")])

    def test_pod_uses_always_user_secret_and_preserves_arch(self):
        _, state = self.state()
        pod = state.pod("app", "python")
        self.assertEqual(pod["spec"]["containers"][0]["imagePullPolicy"], "Always")
        self.assertEqual(pod["spec"]["imagePullSecrets"], [{"name": state.secret}])
        self.assertEqual(pod["spec"]["nodeSelector"], {"kubernetes.io/arch": "arm64"})

    def test_pull_secret_is_created_in_exact_owned_destination_without_secret_output(self):
        h, state = self.state()
        namespace = h.namespace + "-control"
        h.owned = {("namespace", "", namespace): "namespace-uid"}
        h.get = Mock(return_value={"metadata": {
            "uid": "namespace-uid", "labels": {OWNER: h.run_id}}})
        h.kubectl = Mock(side_effect=[
            subprocess.CompletedProcess([], 0, "secret/" + state.secret, ""),
            subprocess.CompletedProcess([], 0, "secret-uid", "")])
        state.install_pull_secret(namespace)
        create, read = h.kubectl.call_args_list
        self.assertEqual(create.args, ("create", "-f", "-", "-o", "name"))
        obj = json.loads(create.kwargs["input"])
        self.assertEqual(obj["metadata"]["namespace"], namespace)
        self.assertEqual(obj["metadata"]["labels"], {OWNER: h.run_id})
        auth = json.loads(obj["stringData"][".dockerconfigjson"])["auths"][state.authority]["auth"]
        self.assertEqual(base64.b64decode(auth).decode(), delivery.USERNAME + ":" + state.password)
        self.assertEqual(read.args, ("-n", namespace, "get", "secret", state.secret,
                                    "-o", "jsonpath={.metadata.uid}"))
        self.assertEqual(h.owned[("secret", namespace, state.secret)], "secret-uid")

    def test_pull_secret_rejects_unowned_replaced_foreign_or_duplicate_destination(self):
        h, state = self.state()
        for mutation in ("unowned", "replaced", "foreign", "duplicate"):
            with self.subTest(mutation=mutation):
                namespace = h.namespace + "-control"
                h.owned = {("namespace", "", namespace): "namespace-uid"}
                obj = {"metadata": {"uid": "namespace-uid", "labels": {OWNER: h.run_id}}}
                if mutation == "unowned":
                    h.owned.clear()
                elif mutation == "replaced":
                    obj["metadata"]["uid"] = "new-namespace"
                elif mutation == "foreign":
                    obj["metadata"]["labels"][OWNER] = "foreign"
                else:
                    h.owned[("secret", namespace, state.secret)] = "existing-secret"
                h.get = Mock(return_value=obj)
                h.kubectl = Mock()
                with self.assertRaises(Failure):
                    state.install_pull_secret(namespace)
                h.kubectl.assert_not_called()

    def test_registry_certificates_support_strict_x509_and_single_line_password(self):
        h, state = self.state()
        identity = "8" * 64
        created = False
        commands = []
        info = {"Id": identity, "Config": {"Labels": {OWNER: h.run_id}},
                "State": {"Running": True},
                "NetworkSettings": {"Networks": {"kind": {"IPAddress": "172.18.0.9"}},
                                    "Ports": {"5000/tcp": [{"HostPort": "5443"}]}}}
        def run(command, **kwargs):
            nonlocal created
            commands.append(command)
            if command[:2] == ["docker", "create"]:
                created = True
            if command[:2] == ["openssl", "req"] or command[:2] == ["openssl", "x509"]:
                Path(command[command.index("-out") + 1]).write_text("fixture-certificate")
            if command[:2] == ["docker", "inspect"]:
                return subprocess.CompletedProcess(command, 0 if created else 1, json.dumps([info]), "")
            return subprocess.CompletedProcess(command, 0, "", "")
        h.run = run
        h.wait = Mock()
        private = Mock(return_value=subprocess.CompletedProcess([], 0, delivery.USERNAME + ":$2b$test\n", ""))
        with patch.object(state, "private_run", private), patch.object(delivery.shutil, "which", return_value="/htpasswd"), \
                patch.object(delivery.ssl, "create_default_context") as context:
            state.start_registry()
        self.assertEqual(len(private.call_args_list), 2)
        self.assertTrue(all(call.kwargs["input"] == state.password + "\n"
                            for call in private.call_args_list))
        ca = next(command for command in commands if command[:3] == ["openssl", "req", "-x509"])
        self.assertIn("subjectKeyIdentifier=hash", ca)
        self.assertIn("keyUsage=critical,keyCertSign,cRLSign", ca)
        extensions = (state.work / "extensions").read_text()
        self.assertIn("authorityKeyIdentifier=keyid,issuer", extensions)
        self.assertIn("subjectKeyIdentifier=hash", extensions)
        self.assertIn("keyUsage=critical,digitalSignature,keyEncipherment", extensions)
        context.assert_called_once_with(cafile=str(state.work / "ca.crt"))

    def test_producer_ca_and_auth_scoped_to_one_trusted_cli(self):
        h, state = self.state()
        command = state.producer()
        self.assertEqual(h.cli_command[h.cli_command.index("--network") + 1], "none")
        self.assertEqual(command[command.index("--network") + 1], "bridge")
        self.assertIn("--env-file", command)
        self.assertIn(OWNER + "=" + h.run_id, command)
        self.assertNotIn(state.password, " ".join(command))
        self.assertNotIn("--insecure", command)
        environment = Path(command[command.index("--env-file") + 1])
        self.assertEqual(stat.S_IMODE(environment.stat().st_mode), 0o600)
        self.assertIn("SSL_CERT_FILE=", environment.read_text())
        self.assertIn("BREWLET_REGISTRY_PASSWORD=" + state.password, environment.read_text())

    def test_foreign_trust_authority_cannot_be_mutated(self):
        _, state = self.state()
        with self.assertRaisesRegex(Failure, "outside invocation"):
            state.node_private_file("shared.example:5000", "ca.crt", "certificate")
        with self.assertRaisesRegex(Failure, "invalid trusted"):
            state.node_private_file(state.authority, "../../shared", "certificate")

    def test_node_trust_transfer_checks_bytes_without_logging_secret(self):
        h, state = self.state()
        payload = '{"password":"test-only-private-value"}'
        h.node_exec = Mock(return_value=subprocess.CompletedProcess([], 0, "", ""))
        state.node_private_file(state.authority, "credentials.json", payload)
        calls = h.node_exec.call_args_list
        self.assertEqual(calls[-2].kwargs["input"], payload)
        self.assertEqual(calls[-1].args[:3], ("cmp", "-s", "-"))
        self.assertEqual(calls[-1].kwargs, {"input": payload, "check": False})
        self.assertNotIn(payload, str([call.args for call in calls]))
        h.node_exec.return_value = subprocess.CompletedProcess([], 1, "", "")
        with self.assertRaisesRegex(Failure, "readback differs"):
            state.node_private_file(state.authority, "credentials.json", payload)

    def test_terminal_negative_pod_reports_wrong_reason_without_timeout(self):
        h, state = self.state()
        state.cases["malformed"] = "registry.test/negative@sha256:" + "a" * 64
        state.fresh = Mock()
        state.pod = Mock(return_value={
            "metadata": {"name": "delivery-malformed"}, "spec": {"containers": [{}]}})
        obj = {"metadata": {"uid": "owned-pod", "labels": {OWNER: h.run_id}},
               "status": {"phase": "Failed", "containerStatuses": [{"state": {
                   "terminated": {"exitCode": 128, "startedAt": "1970-01-01T00:00:00Z",
                                  "message": "content digest not found"}}}]}}
        h.owned = {("pod", h.namespace, "delivery-malformed"): "owned-pod"}
        h.apply = Mock()
        h.get = Mock(side_effect=[obj, {"items": []}])
        h.wait = lambda description, predicate, **kwargs: predicate()
        with self.assertRaisesRegex(Failure, "unexpected reason.*content digest not found"):
            state.negative("malformed", ["InvalidLaunchConfig"])

    def test_emulated_producer_prefix_is_rejected(self):
        h, state = self.state()
        h.cli_command[h.cli_command.index("--platform") + 1] = "linux/amd64"
        with self.assertRaisesRegex(Failure, "explicit native platform"):
            state.producer()

    def test_private_command_errors_never_disclose_secret(self):
        _, state = self.state()
        with self.assertRaises(Failure) as failure:
            state.private_run([sys.executable, "-c",
                "import os,sys;sys.stderr.write(os.environ['BREWLET_REGISTRY_PASSWORD']);sys.exit(1)"],
                env={"BREWLET_REGISTRY_PASSWORD": state.password})
        self.assertNotIn(state.password, str(failure.exception))
        self.assertIn("<redacted>", str(failure.exception))
        self.assertEqual(list(state.work.glob("*.log")), [])

    def test_producer_lifecycle_records_and_cleans_exact_container_id(self):
        h, state = self.state()
        identity = "5" * 64
        calls = []
        def run(command, **kwargs):
            calls.append(command)
            output = ""
            if command == ["docker", "inspect", state.producer_name]:
                return subprocess.CompletedProcess(command, 1, "", "")
            if command[:2] == ["docker", "create"]:
                output = identity
            if command[:2] == ["docker", "inspect"]:
                output = json.dumps([{"Id": identity, "Config": {
                    "Labels": {OWNER: h.run_id}, "Env": ["PASSWORD=" + state.password]},
                    "State": {"ExitCode": 0}}])
            return subprocess.CompletedProcess(command, 0, output, "")
        with patch.object(state, "private_run", side_effect=run):
            state.run_producer([*state.producer(), "push", "registry.test:5443/app"])
        create = next(call for call in calls if call[:2] == ["docker", "create"])
        self.assertNotIn("--rm", create)
        self.assertIn(["docker", "start", "--attach", identity], calls)
        self.assertIn(["docker", "rm", "--force", "--volumes", identity], calls)
        self.assertIsNone(state.producer_id)
        self.assertEqual(h.calls, [])  # No raw Config.Env through the public logger.

    def test_producer_failure_still_cleans_exact_container_id(self):
        h, state = self.state()
        identity = "6" * 64
        calls = []
        def run(command, **kwargs):
            calls.append(command)
            if command == ["docker", "inspect", state.producer_name]:
                return subprocess.CompletedProcess(command, 1, "", "")
            if command[:2] == ["docker", "start"]:
                raise Failure("controlled failed publication")
            output = identity if command[:2] == ["docker", "create"] else json.dumps([{
                "Id": identity, "Config": {"Labels": {OWNER: h.run_id}}}])
            return subprocess.CompletedProcess(command, 0, output, "")
        with patch.object(state, "private_run", side_effect=run):
            with self.assertRaisesRegex(Failure, "controlled failed"):
                state.run_producer([*state.producer(), "push", "registry.test:5443/app"])
        self.assertIn(["docker", "rm", "--force", "--volumes", identity], calls)

    def test_interrupted_create_recovers_only_owned_exact_identity(self):
        h, state = self.state()
        identity = "7" * 64
        calls, created = [], [False]
        info = json.dumps([{"Id": identity, "Config": {"Labels": {OWNER: h.run_id}}}])
        def run(command, **kwargs):
            calls.append(command)
            if command[:2] == ["docker", "create"]:
                created[0] = True
                raise Failure("bounded create interrupted")
            if command == ["docker", "inspect", state.producer_name] and not created[0]:
                return subprocess.CompletedProcess(command, 1, "", "")
            return subprocess.CompletedProcess(command, 0, info, "")
        with patch.object(state, "private_run", side_effect=run):
            with self.assertRaisesRegex(Failure, "create interrupted"):
                state.run_producer([*state.producer(), "push", "registry.test:5443/app"])
        self.assertIn(["docker", "rm", "--force", "--volumes", identity], calls)

    def test_cleanup_attempts_registry_even_if_producer_cleanup_fails(self):
        h, state = self.state()
        state.registry_id = "registry-id"
        with patch.object(state, "cleanup_producer", side_effect=Failure("producer failed")), \
                patch.object(state, "private_run", return_value=subprocess.CompletedProcess([], 0, "", "")):
            with self.assertRaisesRegex(Failure, "producer failed"):
                state.cleanup()
        self.assertIn(["docker", "rm", "--force", "--volumes", "registry-id"], h.calls)

    def test_diagnostic_failure_cannot_skip_owned_registry_removal(self):
        h, state = self.state()
        state.registry_id = "registry-id"
        with patch.object(state, "private_run", side_effect=Failure("diagnostics failed")):
            with self.assertRaisesRegex(Failure, "diagnostics failed"):
                state.cleanup()
        self.assertIn(["docker", "rm", "--force", "--volumes", "registry-id"], h.calls)

    def test_cleanup_refuses_replaced_registry_identity(self):
        h, state = self.state()
        state.registry_id = "registry-id"
        info = {"Id": "different-id", "Config": {"Labels": {OWNER: h.run_id}}}
        with patch.object(h, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps([info]), "")):
            with self.assertRaisesRegex(Failure, "identity changed"):
                state.cleanup()

    def test_native_pod_keeps_actual_arch_and_no_fake_node_labels(self):
        h, state = self.state()
        state.native["amd64"] = "172.18.0.9:5000/native@sha256:" + "4" * 64
        # Use production-shaped pod, but no Kubernetes operations.
        state.original_pod = lambda *a, **k: {"metadata": {"annotations": {}},
                                            "spec": {"containers": [{}]}}
        pod = state.native_pod("amd64", "native")
        self.assertEqual(pod["spec"]["nodeSelector"],
                         {"kubernetes.io/arch": "amd64", "brewlet.test/pool": h.run_id})
        self.assertEqual(pod["spec"]["containers"][0]["image"], state.native["amd64"])
        self.assertEqual(h.calls, [])

    def test_selected_missing_bootstrap_and_capacity_fail(self):
        h = FakeHarness(self.work)
        with self.assertRaisesRegex(Failure, "bootstrap"):
            delivery.execute(h)
        with self.assertRaisesRegex(Failure, "lacks its real"):
            delivery.finish_native_capacity(h, None)


if __name__ == "__main__":
    unittest.main()
