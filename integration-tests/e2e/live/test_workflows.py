# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import hashlib
from http.client import RemoteDisconnected
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import unittest
from unittest.mock import Mock, call, patch
from urllib.error import URLError

import workflows_helpers as h
from common import Fixture, OWNER_LABEL
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

class AuthRegistryTests(unittest.TestCase):
    def setUp(self):
        self.fixture = CheckoutFixture.__new__(CheckoutFixture)
        self.fixture.name = "owned"
        self.fixture.arch = "amd64"
        self.fixture.auth_registry_id = "registry-id"
        self.fixture.secrets = {"secret-password"}
        self.info = {"Id": "registry-id", "Config": {"Labels": {OWNER_LABEL: "owned"}},
                     "State": {"Status": "running", "Error": "secret-password"},
                     "NetworkSettings": {"Ports": {"5000/tcp": [{"HostPort": "12345"}]}}}
        self.fixture.run = Mock(return_value=subprocess.CompletedProcess(
            [], 0, json.dumps([self.info]), ""))
        self.fixture.save = Mock()

    def test_start_retries_transport_errors_until_auth_challenge(self):
        errors = [ConnectionResetError("reset"), URLError("refused"),
                  TimeoutError("timed out"), RemoteDisconnected("closed")]
        with patch("workflows.h.fetch", side_effect=[
                *errors, (503, {}, b""), (401, {}, b"")]) as fetch, \
                patch("common.time.sleep") as sleep:
            self.assertEqual(self.fixture.start_auth_registry(Path("/private/auth/htpasswd")),
                             "localhost:12345")
        self.assertEqual(fetch.call_count, 6)
        fetch.assert_called_with("localhost:12345", "/v2/")
        self.assertEqual(sleep.call_count, 5)

    def test_start_still_times_out_without_challenge(self):
        for response in (ConnectionResetError("reset"), (200, {}, b""), (503, {}, b"")):
            with self.subTest(response=response), \
                    patch("workflows.h.fetch", side_effect=[response, response]) as fetch, \
                    patch("common.time.monotonic", side_effect=[0, 0, 60]), \
                    patch("common.time.sleep") as sleep:
                with self.assertRaisesRegex(TimeoutError, "after 60s: auth registry challenge"):
                    self.fixture.start_auth_registry(Path("/private/auth/htpasswd"))
                self.assertEqual(fetch.call_count, 2)
                sleep.assert_called_once_with(1)

    def test_start_does_not_retry_unexpected_errors(self):
        with patch("workflows.h.fetch", side_effect=ValueError("invalid response")), \
                patch("common.time.sleep") as sleep:
            with self.assertRaisesRegex(ValueError, "invalid response"):
                self.fixture.start_auth_registry(Path("/private/auth/htpasswd"))
            sleep.assert_not_called()

    def test_fetch_outside_readiness_does_not_swallow_transport_errors(self):
        with patch("workflows_helpers.urlopen", side_effect=ConnectionResetError("reset")):
            with self.assertRaises(ConnectionResetError):
                h.fetch("localhost:12345", "/v2/")

    def test_cleanup_retains_redacted_state_and_logs_before_removal(self):
        self.fixture.run.side_effect = [
            self.fixture.run.return_value,
            subprocess.CompletedProcess([], 0, "stdout secret-password", "stderr secret-password"),
            subprocess.CompletedProcess([], 0, "", ""),
        ]
        self.fixture.remove_owned()
        self.assertEqual(self.fixture.save.call_args_list, [
            call("auth-registry-state.json",
                 json.dumps({"Status": "running", "Error": h.REDACTED})),
            call("auth-registry.log", f"stdout {h.REDACTED}stderr {h.REDACTED}"),
        ])
        self.assertEqual([c.args[0] for c in self.fixture.run.call_args_list], [
            ["docker", "inspect", "registry-id"],
            ["docker", "logs", "--tail", "200", "registry-id"],
            ["docker", "rm", "-f", "--volumes", "registry-id"],
        ])

    def test_cleanup_removes_container_even_if_evidence_write_fails(self):
        self.fixture.save.side_effect = OSError("disk full")
        with self.assertRaisesRegex(OSError, "disk full"):
            self.fixture.remove_owned()
        self.fixture.run.assert_called_with(["docker", "rm", "-f", "--volumes", "registry-id"])

    def test_cleanup_reports_log_collection_failure(self):
        self.fixture.run.side_effect = [
            self.fixture.run.return_value,
            subprocess.CompletedProcess([], 1, "", "logs unavailable"),
            subprocess.CompletedProcess([], 0, "", ""),
        ]
        with self.assertRaisesRegex(RuntimeError, "auth registry logs failed"):
            self.fixture.remove_owned()
        self.fixture.save.assert_called_with("auth-registry.log", "logs unavailable")
        self.fixture.run.assert_called_with(["docker", "rm", "-f", "--volumes", "registry-id"])

    def test_cleanup_does_not_collect_or_remove_foreign_container(self):
        self.info["Config"]["Labels"][OWNER_LABEL] = "foreign"
        self.fixture.run.return_value.stdout = json.dumps([self.info])
        with self.assertRaisesRegex(RuntimeError, "foreign auth registry"):
            self.fixture.remove_owned()
        self.fixture.run.assert_called_once_with(["docker", "inspect", "registry-id"], check=False)
        self.fixture.save.assert_not_called()


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
