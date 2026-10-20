import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("composition_check", ROOT / "composition_check.py")
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class CompositionLedgerTests(unittest.TestCase):
    def setUp(self):
        self.document = json.loads((ROOT / "composition-requirements.json").read_text())

    def test_shape_and_incomplete_gate(self):
        errors, incomplete = CHECK.validate(self.document, ROOT)
        self.assertFalse(errors)
        if incomplete:
            errors, _ = CHECK.validate(self.document, ROOT, True)
            self.assertTrue(errors)

    def test_failed_skipped_or_missing_proofs_cannot_qualify(self):
        for outcome in ("failed", "skipped", "pending"):
            document = copy.deepcopy(self.document)
            document["checks"]["contracts"]["outcome"] = outcome
            errors, incomplete = CHECK.validate(document, ROOT, True)
            self.assertTrue(errors)
            self.assertIn("COMP-09", incomplete)
        document = copy.deepcopy(self.document)
        document["checks"]["arm64"]["outcome"] = "passed"
        with tempfile.TemporaryDirectory() as directory:
            errors, _ = CHECK.validate(document, Path(directory))
            self.assertTrue(errors)

    def test_obligations_and_evidence_references_cannot_disappear(self):
        document = copy.deepcopy(self.document)
        document["requirements"].pop()
        self.assertTrue(CHECK.validate(document, ROOT)[0])
        document = copy.deepcopy(self.document)
        document["requirements"][0]["checks"] = ["unknown"]
        self.assertTrue(CHECK.validate(document, ROOT)[0])
