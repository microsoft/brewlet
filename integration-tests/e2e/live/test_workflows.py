# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

import workflows_helpers as h
from common import Fixture
from workflows import CheckoutFixture

DIGEST = "sha256:" + "a" * 64


def descriptor(data, media_type, **extra):
    return {"mediaType": media_type, "digest": "sha256:" + hashlib.sha256(data).hexdigest(),
            "size": len(data), **extra}


class FakeRegistry:
    """Minimal read-only distribution API serving one runnable index."""

    def __init__(self, corrupt=False):
        self.blobs, self.manifests = {}, {}
        children = []
        for arch in ("amd64", "arm64"):
            config, layer = f"config-{arch}".encode(), f"layer-{arch}".encode()
            self.blobs[descriptor(config, "c")["digest"]] = config
            self.blobs[descriptor(layer, "l")["digest"]] = layer if not corrupt else b"tampered"
            child = json.dumps({"mediaType": h.MANIFEST, "config": descriptor(config, "c"),
                                "layers": [descriptor(layer, "l")]}).encode()
            self.manifests[descriptor(child, h.MANIFEST)["digest"]] = child
            children.append(descriptor(child, h.MANIFEST, platform={"os": "linux", "architecture": arch}))
        index = json.dumps({"mediaType": h.INDEX, "manifests": children}).encode()
        self.digest = descriptor(index, h.INDEX)["digest"]
        self.manifests[self.digest] = self.manifests["v1"] = index
        registry = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                _prefix, kind, reference = self.path.rsplit("/", 2)
                body = (registry.manifests if kind == "manifests" else registry.blobs).get(reference)
                if self.headers.get("Authorization") != h.basic_auth("user", "pass"):
                    body = None
                self.send_response(200 if body is not None else 404)
                if body is not None and kind == "manifests":
                    self.send_header("Docker-Content-Digest", "sha256:" + hashlib.sha256(body).hexdigest())
                self.end_headers()
                self.wfile.write(body or b"")

            def log_message(self, *_args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.address = f"127.0.0.1:{self.server.server_address[1]}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class RecordingTests(unittest.TestCase):
    def test_secrets_are_redacted_from_argv_env_and_streams(self):
        secrets = {"hunter2", "hunter2-longer"}
        self.assertEqual(h.redact_secrets("a hunter2-longer b hunter2", secrets),
                         "a [REDACTED] b [REDACTED]")
        record = h.sanitized_command(["mvn", "--encrypt-password", "hunter2"],
                                     {"BREWLET_REGISTRY_PASSWORD": "x", "DOCKER_CONFIG": "/p/hunter2"}, secrets)
        self.assertEqual(record["argv"][2], h.REDACTED)
        self.assertEqual(record["env"]["BREWLET_REGISTRY_PASSWORD"], h.REDACTED)
        self.assertEqual(record["env"]["DOCKER_CONFIG"], "/p/[REDACTED]")

    def test_bounded_durations_include_documented_tolerance(self):
        self.assertEqual(h.assert_bounded("op", 24.0, 20, 5)["limitSeconds"], 20)
        with self.assertRaisesRegex(AssertionError, "bound is 20s"):
            h.assert_bounded("op", 25.1, 20, 5)

    def test_maven_timestamps(self):
        output = "12:00:01.500 [INFO] waiting up to 30s\n12:00:31.750 [ERROR] was not Ready after 30s"
        self.assertEqual(h.log_seconds(output, "was not Ready") - h.log_seconds(output, "waiting up to"), 30.25)
        with self.assertRaises(AssertionError):
            h.log_seconds("[INFO] waiting up to 30s", "waiting up to")


class HandoffTests(unittest.TestCase):
    def test_push_stdout_and_handoff(self):
        stdout = f"pushed x\n  index: {DIGEST} (10 bytes)\n  deploy image: r:1/a/b@{DIGEST}\n"
        self.assertEqual(h.push_stdout(stdout), {"digest": DIGEST, "deployImage": f"r:1/a/b@{DIGEST}"})
        with self.assertRaises(AssertionError):
            h.push_stdout("pushed x\n  store: ./oci\n")
        handoff = {"image": "r:1/a/b:v1", "digest": DIGEST, "deployImage": f"r:1/a/b@{DIGEST}", "format": "image"}
        h.validate_handoff(handoff, "r:1", "a/b", "v1", DIGEST)
        for field, value in (("digest", "sha256:" + "b" * 64), ("deployImage", "r:1/a/b:v1"),
                             ("format", "artifact"), ("image", "docker.io/a/b:v1")):
            with self.assertRaises(AssertionError):
                h.validate_handoff(dict(handoff, **{field: value}), "r:1", "a/b", "v1", DIGEST)

    def test_registry_verification_checks_every_child_and_blob(self):
        registry = FakeRegistry()
        try:
            result = h.verify_published(registry.address, "wf/cli", "v1", ("user", "pass"))
            self.assertEqual(result["digest"], registry.digest)
            self.assertEqual(sorted(m["architecture"] for m in result["manifests"]), ["amd64", "arm64"])
            self.assertIsNone(h.manifest(registry.address, "wf/cli", "v1"))
            with self.assertRaises(AssertionError):
                h.verify_published(registry.address, "wf/cli", "missing", ("user", "pass"))
        finally:
            registry.close()
        corrupt = FakeRegistry(corrupt=True)
        try:
            with self.assertRaisesRegex(AssertionError, "does not match its descriptor"):
                h.verify_published(corrupt.address, "wf/cli", "v1", ("user", "pass"))
        finally:
            corrupt.close()

    def test_yaml_top_level(self):
        values = h.yaml_top_level('name: wf-app\nnamespace: "wf-alpha"\nready: true\npods:\n  - name: x\n')
        self.assertEqual(values, {"name": "wf-app", "namespace": "wf-alpha", "ready": "true", "pods": ""})


class CleanupSequenceTests(unittest.TestCase):
    @staticmethod
    def event(kind, *, deleting=True, complete=False, targets=1, uid="u", generation=2):
        conditions = [{"type": "CleanupComplete", "status": "True", "reason": "CleanupSucceeded",
                       "observedGeneration": generation}] if complete else []
        return {"type": kind, "object": {
            "metadata": {"uid": uid, "generation": 2, "resourceVersion": "1",
                         "deletionTimestamp": "t" if deleting else None,
                         "finalizers": ["node.brewlet.sh/cleanup"]},
            "status": {"targets": [{}] * targets, "conditions": conditions}}}

    def test_watch_stream_and_production_order(self):
        events = [self.event("ADDED", deleting=False), self.event("MODIFIED"),
                  self.event("MODIFIED", complete=True), self.event("MODIFIED", complete=True, targets=0),
                  self.event("DELETED", complete=True, targets=0)]
        stream = "\n".join(json.dumps(e, indent=2) for e in events)
        self.assertEqual(len(h.cleanup_sequence(h.parse_json_stream(stream), "u")), 5)

    def test_rejects_missing_or_stale_checkpoint_and_foreign_uid(self):
        cases = {
            "no checkpoint": [self.event("MODIFIED"), self.event("DELETED")],
            "stale generation": [self.event("MODIFIED", complete=True, generation=1), self.event("DELETED")],
            "claims released first": [self.event("MODIFIED", complete=True, targets=0), self.event("DELETED")],
            "not deleted": [self.event("MODIFIED", complete=True)],
            "foreign": [self.event("MODIFIED", complete=True, uid="x"), self.event("DELETED", uid="x")],
        }
        for name, events in cases.items():
            with self.subTest(name), self.assertRaises(AssertionError):
                h.cleanup_sequence(events, "u")


class IsolationTests(unittest.TestCase):
    base = {"clusters": [{"name": "kind", "cluster": {"server": "https://127.0.0.1:6443", "certificate-authority-data": "Q0E="}}],
            "users": [{"name": "kind", "user": {"client-key-data": "S0VZ"}}],
            "contexts": [{"name": "kind", "context": {"cluster": "kind", "user": "kind"}}],
            "current-context": "kind"}

    def test_kubeconfigs_pin_namespace_decoy_and_token(self):
        config = h.derive_kubeconfig(self.base, context="wf", namespace="wf-alpha")
        self.assertEqual(config["current-context"], "wf")
        self.assertEqual(config["contexts"][0]["context"]["namespace"], "wf-alpha")
        decoy = h.derive_kubeconfig(self.base, context="wf", decoy_server="https://127.0.0.1:1")
        self.assertEqual(decoy["current-context"], "decoy")
        self.assertNotIn("namespace", decoy["contexts"][0]["context"])
        restricted = h.derive_kubeconfig(self.base, context="wf", token="t")
        self.assertEqual(restricted["users"][0]["user"], {"token": "t"})
        self.assertNotIn("client-key-data", json.dumps(restricted))

    def test_maven_security_material(self):
        settings = h.maven_settings("localhost:5000", "u<", "{abc=}")
        self.assertIn("<id>localhost:5000</id><username>u&lt;</username><password>{abc=}</password>", settings)
        self.assertIn("<master>{m}</master>", h.settings_security("{m}"))
        self.assertEqual(h.encrypted("noise\n{zr0+/=}\n"), "{zr0+/=}")
        with self.assertRaises(AssertionError):
            h.encrypted("no cipher")
        with tempfile.TemporaryDirectory() as tmp:
            path = h.write_private(Path(tmp) / "settings-security.xml", "x")
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)

    def test_stalled_endpoint_accepts_and_never_answers(self):
        with h.StalledEndpoint() as endpoint, socket.create_connection(("127.0.0.1", endpoint.port), 2) as client:
            client.settimeout(0.3)
            client.sendall(b"GET / HTTP/1.1\r\n\r\n")
            with self.assertRaises(socket.timeout):
                client.recv(1)
        with self.assertRaises(OSError):
            socket.create_connection(("127.0.0.1", h.unused_port()), 1).close()

    def test_stalled_kubectl_stub_and_orphan_detection(self):
        with tempfile.TemporaryDirectory() as tmp:
            state = Path(tmp) / "state with 'quotes'"
            state.mkdir()
            stub = state / "kubectl"
            stub.write_text(h.stalled_kubectl(state))
            stub.chmod(0o755)
            began = time.time()
            process = subprocess.Popen([str(stub), "apply"], stderr=subprocess.DEVNULL)
            child = None
            try:
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline:
                    if (state / "child.pid").exists():
                        child = (state / "child.pid").read_text().strip()
                        if child:
                            break
                    time.sleep(0.05)
                self.assertTrue(child, "stub did not record its child within 10s")
                started = float((state / "started").read_text())
                self.assertGreaterEqual(started, began)
                self.assertLessEqual(started, time.time())
                self.assertEqual(int((state / "kubectl.pid").read_text()), process.pid)
                self.assertTrue(h.process_alive(process.pid, str(stub)))
                self.assertTrue(h.process_alive(child, "sleep"))
                self.assertFalse(h.process_alive(child, "not-the-command"))
            finally:
                try:
                    if child:
                        try:
                            os.kill(int(child), signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                    # Let the shell reap its child before falling back to killing it.
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                finally:
                    process.wait(timeout=10)
            deadline = time.monotonic() + 10
            while h.process_alive(child, "sleep") and time.monotonic() < deadline:
                time.sleep(0.05)
            self.assertFalse(h.process_alive(child, "sleep"))
            self.assertFalse(h.process_alive(process.pid, str(stub)))


class ProcessInspectionTests(unittest.TestCase):
    def test_real_live_and_reaped_process(self):
        process = subprocess.Popen(["sleep", "600"])
        try:
            self.assertTrue(h.process_alive(process.pid, "sleep"))
            self.assertFalse(h.process_alive(process.pid, "not-the-command"))
        finally:
            process.kill()
            process.wait(timeout=10)
        self.assertFalse(h.process_alive(process.pid, "sleep"))

    def test_real_zombie_is_not_alive(self):
        process = subprocess.Popen([sys.executable, "-c", "pass"])
        try:
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                result = subprocess.run(["/bin/ps", "-ww", "-p", str(process.pid), "-o", "stat=", "-o", "args="],
                                        capture_output=True, text=True, check=True, timeout=5)
                if result.stdout.strip().startswith("Z"):
                    break
                time.sleep(0.05)
            self.assertTrue(result.stdout.strip().startswith("Z"), "child did not become a zombie")
            command = result.stdout.strip().split(maxsplit=1)[1]
            self.assertFalse(h.process_alive(process.pid, command))
        finally:
            process.kill()
            process.wait(timeout=10)

    def test_linux_and_macos_states_and_full_command(self):
        for platform in ("linux", "darwin"):
            for state, alive in (("S", True), ("R+", True), ("Ss", True), ("Z+", False)):
                with self.subTest(platform=platform, state=state), patch.object(h.sys, "platform", platform):
                    result = subprocess.CompletedProcess([], 0, f" {state} /bin/sleep 600\n", "")
                    with patch.object(h.subprocess, "run", return_value=result) as run:
                        self.assertEqual(h.process_alive("123", "sleep"), alive)
                        self.assertFalse(h.process_alive("123", "other-command"))
                        self.assertEqual(run.call_args.args[0],
                                         ["/bin/ps", "-ww", "-p", "123", "-o", "stat=", "-o", "args="])
                        self.assertEqual(run.call_args.kwargs["timeout"], 5)

    def test_inspection_errors_fail_closed(self):
        for error in (FileNotFoundError("ps unavailable"), PermissionError("ps denied"),
                      subprocess.TimeoutExpired("ps", 5)):
            with self.subTest(error=error), patch.object(h.subprocess, "run", side_effect=error):
                with self.assertRaisesRegex(RuntimeError, "cannot inspect process"):
                    h.process_alive(123, "sleep")
        for code, stdout, stderr in ((2, "", "ps failed"), (1, "", "access denied"),
                                     (0, "", ""), (0, "S", ""), (0, "? sleep", ""),
                                     (0, "S sleep", "warning")):
            with self.subTest(code=code, stdout=stdout, stderr=stderr):
                with patch.object(h.subprocess, "run",
                                  return_value=subprocess.CompletedProcess([], code, stdout, stderr)):
                    with self.assertRaises(RuntimeError):
                        h.process_alive(123, "sleep")

    def test_no_ps_match_requires_confirmed_absence(self):
        result = subprocess.CompletedProcess([], 1, "", "")
        with patch.object(h.subprocess, "run", return_value=result):
            with patch.object(h.os, "kill", side_effect=ProcessLookupError) as kill:
                self.assertFalse(h.process_alive(123, "sleep"))
                kill.assert_called_once_with(123, 0)
            with patch.object(h.os, "kill", return_value=None):
                with self.assertRaisesRegex(RuntimeError, "existing process"):
                    h.process_alive(123, "sleep")
            with patch.object(h.os, "kill", side_effect=PermissionError):
                with self.assertRaisesRegex(RuntimeError, "cannot confirm"):
                    h.process_alive(123, "sleep")

    def test_unsupported_platform_and_invalid_inputs_fail(self):
        with patch.object(h.sys, "platform", "win32"), patch.object(h.subprocess, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "unsupported"):
                h.process_alive(123, "sleep")
            run.assert_not_called()
        for pid in ("", "0", "-1", "123/../stat", "2147483648"):
            with self.subTest(pid=pid), self.assertRaises(ValueError):
                h.process_alive(pid, "sleep")
        with self.assertRaises(ValueError):
            h.process_alive(123, "")


class FixtureScopeTests(unittest.TestCase):
    def test_workflows_scenario_is_allowed_and_isolated(self):
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {
                "BREWLET_LIVE_OUTPUT": tmp, "BREWLET_REGISTRY_PASSWORD": "leak",
                "BREWLET_REGISTRY_USERNAME": "leak", "MAVEN_OPTS": "-Dleak"}):
            fixture = CheckoutFixture()
            try:
                self.assertEqual(fixture.scenario, "workflows")
                self.assertTrue(fixture.name.startswith("brewlet-live-workflows-"))
                for key in ("BREWLET_REGISTRY_PASSWORD", "BREWLET_REGISTRY_USERNAME", "MAVEN_OPTS"):
                    self.assertNotIn(key, fixture.env)
                self.assertEqual(json.loads(Path(fixture.env["DOCKER_CONFIG"], "config.json").read_text()), {})
                self.assertIn(fixture.remove_owned, fixture.cleanups)
                self.assertFalse(str(fixture.private).startswith(str(fixture.work)))
            finally:
                shutil.rmtree(fixture.private, ignore_errors=True)
        with self.assertRaises(ValueError):
            Fixture("unknown")

    def test_command_recording_fails_mandatory_assertions_and_hides_secrets(self):
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {"BREWLET_LIVE_OUTPUT": tmp}):
            fixture = CheckoutFixture()
            try:
                fixture.secret("s3cret-value")
                result = fixture.cmd("ok", ["sh", "-c", "echo out; echo err >&2"])
                self.assertEqual((result.stdout, result.stderr), ("out\n", "err\n"))
                fixture.cmd("expected-failure", ["sh", "-c", "exit 3"], expect="fail")
                with self.assertRaisesRegex(AssertionError, "exited 3"):
                    fixture.cmd("unexpected-failure", ["sh", "-c", "exit 3"])
                with self.assertRaisesRegex(AssertionError, "unexpectedly succeeded"):
                    fixture.cmd("unexpected-success", ["true"], expect="fail")
                with self.assertRaisesRegex(AssertionError, "disclosed"):
                    fixture.cmd("leak", ["echo", "s3cret-value"])
                commands = json.loads((fixture.work / "commands.json").read_text())
                self.assertEqual([c["exitCode"] for c in commands], [0, 3, 3, 0, 0])
                self.assertEqual(commands[-1]["argv"], ["echo", h.REDACTED])
                self.assertNotIn("s3cret-value", "".join(p.read_text() for p in fixture.work.iterdir()))
                self.assertEqual((fixture.work / "cmd-001-ok.stderr").read_text(), "err\n")
            finally:
                shutil.rmtree(fixture.private, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
