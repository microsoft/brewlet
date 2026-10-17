"""Offline gate safeguards; real TLS and cold-node execution are separate."""

import io
import json
from pathlib import Path
import shutil
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
import uuid

import delivery
import transfer_gate as gate


class GateTests(unittest.TestCase):
    def setUp(self):
        self.work = Path(__file__).parent / (".transfer-unit-" + uuid.uuid4().hex)
        self.work.mkdir(mode=0o700)
        self.addCleanup(shutil.rmtree, self.work)

    def test_gate_accepts_only_an_explicit_blob_path(self):
        digest = "sha256:" + "a" * 64
        self.assertEqual(gate.gated_path("delivery/cold-runtime", digest),
                         "/v2/delivery/cold-runtime/blobs/" + digest)
        for repository, value in (("../foreign", digest), ("repo?x", digest),
                                  ("repo", "tag"), ("repo", digest + "/extra")):
            with self.assertRaises(ValueError):
                gate.gated_path(repository, value)

    def test_runtime_append_preserves_existing_bytes_and_adds_a_fresh_layer(self):
        root = self.work / "delivery"
        source = root / "python-after-golden"
        old = delivery.blob(source, b"old-compressed-runtime", delivery.LAYER)
        config = {"architecture": "arm64", "os": "linux",
                  "config": {"Env": ["PATH=/usr/bin", "BREWLET_RUNTIME_REVISION=after"]},
                  "rootfs": {"type": "layers", "diff_ids": ["sha256:" + "b" * 64]}}
        config_descriptor = delivery.json_blob(source, config, delivery.CONFIG)
        manifest = {"schemaVersion": 2, "mediaType": delivery.MANIFEST,
                    "config": config_descriptor, "layers": [old]}
        selected = delivery.finish_layout(source, delivery.json_blob(source, manifest, delivery.MANIFEST))
        producer = SimpleNamespace(work=root, authority="172.18.0.8:5000")
        producer.publish = Mock(side_effect=lambda layout, descriptor, repository:
                                producer.authority + "/" + repository + "@" + descriptor["digest"])
        h = SimpleNamespace(work=self.work, delivery=producer, run_id="123456abcdef")
        state = gate.TransferGate(h)
        reference = state.publish_runtime()
        self.assertTrue(reference.startswith("172.18.0.8:5001/delivery/cold-runtime@sha256:"))
        layout, target, _ = producer.publish.call_args.args
        new_manifest = json.loads(delivery.read_blob(layout, target))
        new_config = json.loads(delivery.read_blob(layout, new_manifest["config"]))
        self.assertEqual(new_manifest["layers"][:-1], [old])
        self.assertEqual(delivery.read_blob(layout, old), b"old-compressed-runtime")
        self.assertEqual(new_config["config"], config["config"])
        self.assertEqual(new_config["rootfs"]["diff_ids"][:-1], config["rootfs"]["diff_ids"])
        self.assertNotEqual(target["digest"], selected["digest"])
        self.assertEqual((state.work / "path").read_text(),
                         gate.gated_path("delivery/cold-runtime", state.layer["digest"]))

    def test_authentication_and_head_requests_are_never_held(self):
        directory, certs = self.work / "gate", self.work / "certificates"
        directory.mkdir()
        (directory / "path").write_text("/v2/repo/blobs/sha256:" + "a" * 64)
        (directory / "release").touch()
        for method, status, path, held in (
            ("GET", 200, (directory / "path").read_text(), True),
            ("HEAD", 200, (directory / "path").read_text(), False),
            ("GET", 401, (directory / "path").read_text(), False),
            ("GET", 200, "/v2/", False),
        ):
            with self.subTest(method=method, status=status, path=path):
                started = directory / "started"
                started.unlink(missing_ok=True)
                response = io.BytesIO(b"payload")
                response.status = status
                response.headers = {"Content-Length": "7", "WWW-Authenticate": "Basic realm=test"}
                opener = Mock()
                opener.open.return_value = response
                sent, output = [], io.BytesIO()

                class Server:
                    def __init__(self, address, handler):
                        self.socket = object()
                        self.handler = handler

                    def serve_forever(self):
                        instance = object.__new__(self.handler)
                        instance.command, instance.path = method, path
                        instance.headers, instance.wfile = {}, output
                        instance.send_response = lambda value: sent.append(value)
                        instance.send_header = Mock()
                        instance.end_headers = Mock()
                        instance.forward()

                with patch("transfer_gate.http.server.ThreadingHTTPServer", Server), \
                        patch("transfer_gate.ssl.create_default_context"), \
                        patch("transfer_gate.ssl.SSLContext"), \
                        patch("transfer_gate.urllib.request.build_opener", return_value=opener):
                    gate.serve("172.18.0.8:5000", "172.18.0.8:5001", directory, certs)
                self.assertEqual(sent, [status])
                self.assertEqual(started.exists(), held)
                self.assertEqual(output.getvalue(), b"" if method == "HEAD" else b"payload")
                request = opener.open.call_args.args[0]
                self.assertEqual(request.full_url, "https://172.18.0.8:5000" + path)

    def test_unexpected_authority_is_refused_before_host_trust_write(self):
        producer = SimpleNamespace(work=self.work, authority="172.18.0.8:5000")
        h = SimpleNamespace(work=self.work, delivery=producer, node_exec_on=Mock())
        state = gate.TransferGate(h)
        state.authority = "foreign.example:5001"
        with self.assertRaises(delivery.Failure):
            state.trust_on("worker")
        h.node_exec_on.assert_not_called()


if __name__ == "__main__":
    unittest.main()
