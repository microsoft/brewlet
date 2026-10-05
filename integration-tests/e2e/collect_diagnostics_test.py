# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
from pathlib import Path
import tempfile
import unittest

from collect_diagnostics import collect, redact


class DiagnosticsTests(unittest.TestCase):
    def test_only_selected_logs_are_exported_and_redacted(self):
        with tempfile.TemporaryDirectory() as tmp:
            work, output = Path(tmp) / "work", Path(tmp) / "evidence"
            work.mkdir()
            for name in ("kubeconfig", "tls.key", "values.yaml", "settings.xml", "unselected.log"):
                (work / name).write_text("DO-NOT-UPLOAD")
            (work / "runner.log").write_text("107 passed, 1 failed\nAuthorization: Bearer CREDENTIAL")
            (work / "t13-fixture-teardown.log").write_text("claims retained\n")
            (work / "diag-profile.log").write_text(
                "-----BEGIN PRIVATE KEY-----\nKEY-MATERIAL\n-----END PRIVATE KEY-----\n"
                '  "client-key-data": "ENCODED-KEY"\npassword=PASSWORD\n'
                "https://user:URL-PASSWORD@example.test/path\n")
            collect(work, output)
            manifest = json.loads((output / "manifest.json").read_text())
            self.assertEqual(manifest["logs"],
                             ["diag-profile.log", "runner.log", "t13-fixture-teardown.log"])
            text = "\n".join(p.read_text() for p in output.iterdir())
            for secret in ("DO-NOT-UPLOAD", "KEY-MATERIAL", "ENCODED-KEY", "URL-PASSWORD", "=PASSWORD"):
                self.assertNotIn(secret, text)
            self.assertIn("107 passed, 1 failed", text)
            self.assertIn("claims retained", text)

    def test_refuses_symlink_and_output_inside_work(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp) / "work"
            work.mkdir()
            (work / "kubeconfig").write_text("PRIVATE")
            (work / "diag-link.log").symlink_to(work / "kubeconfig")
            with self.assertRaisesRegex(ValueError, "non-regular"):
                collect(work, Path(tmp) / "evidence")
            with self.assertRaisesRegex(ValueError, "outside"):
                collect(work, work / "evidence")

    def test_missing_work_is_reported_without_fabricating_logs(self):
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / "evidence"
            collect(Path(tmp) / "missing", output)
            self.assertEqual(json.loads((output / "manifest.json").read_text()),
                             {"logs": [], "workPresent": False})

    def test_structured_and_inline_credentials(self):
        for line in ('"auth": "BASE64"', '<password>VALUE</password>',
                     'TOKEN: VALUE', 'client-certificate-data: VALUE'):
            self.assertEqual(redact(line), "[REDACTED CREDENTIAL LINE]")
        for text in ("password: |\n  MULTILINE-VALUE\nnext: visible",
                     "<password>\nMULTILINE-VALUE\n</password>"):
            self.assertNotIn("MULTILINE-VALUE", redact(text))


if __name__ == "__main__":
    unittest.main()
