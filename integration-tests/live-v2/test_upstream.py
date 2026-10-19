"""Offline provenance/fixture guards, not proof of upstream node execution."""

import json
from pathlib import Path
import tempfile
import unittest

import delivery
from harness import Failure
from upstream import EXECUTABLES, PINS, baseline_audit


class UpstreamFixtureTests(unittest.TestCase):
    def test_inputs_pin_unchanged_platform_digests_and_explicit_executables(self):
        pins = json.loads(PINS.read_text())
        self.assertEqual(set(pins), {"java", "python", "node", "static"})
        for name, pin in pins.items():
            for selected in (pin, *([pin["previous"]] if "previous" in pin else [])):
                for arch in ("arm64", "amd64"):
                    self.assertRegex(selected[arch], r"^gcr\.io/distroless/[a-z0-9-]+@sha256:[0-9a-f]{64}$")
                self.assertNotEqual(selected["arm64"], selected["amd64"])
            if name in EXECUTABLES:
                self.assertTrue(EXECUTABLES[name].startswith("/"))
                self.assertNotEqual(pin["previous"]["arm64"], pin["arm64"])

    def fixture(self, root, extra=()):
        compressed, diff = delivery.tar_layer([
            ("etc/", b"", 0o755),
            ("etc/passwd", b"nonroot:x:65532:65532::/home/nonroot:/sbin/nologin\n", 0o644), *extra])
        layer = delivery.blob(root, compressed, delivery.LAYER)
        config = {"config": {"Env": ["PATH=/missing:/usr/bin"]},
                  "rootfs": {"diff_ids": [diff]}}
        selected = delivery.json_blob(root, {"layers": [layer], "config": {
            "digest": delivery.digest(b"config")}}, delivery.MANIFEST)
        return {"descriptor": selected, "config": config, "source": "test@sha256:" + "a" * 64}

    def test_baseline_audit_records_real_account_mismatch_without_mutation(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            metadata = self.fixture(root)
            before = {str(p): p.read_bytes() for p in root.rglob("*") if p.is_file()}
            report = baseline_audit(root, metadata)
            self.assertEqual(report["accountNames"], ["nonroot"])
            self.assertTrue(report["shellAbsent"])
            self.assertEqual(report["oldContractBlockers"], ["pre-amendment named app account requirement"])
            self.assertEqual(before, {str(p): p.read_bytes() for p in root.rglob("*") if p.is_file()})

    def test_audit_refuses_shell_or_setid_instead_of_sanitizing_upstream(self):
        for extra in ([("bin/sh", b"not a real shell", 0o755)], [("usr/bin/tool", b"setid", 0o4755)]):
            with tempfile.TemporaryDirectory() as temporary, self.assertRaises(Failure):
                root = Path(temporary)
                baseline_audit(root, self.fixture(root, extra))


if __name__ == "__main__":
    unittest.main()
