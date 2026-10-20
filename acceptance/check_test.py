import copy
import importlib.util
import json
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location("check", Path(__file__).with_name("check.py"))
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class LedgerTests(unittest.TestCase):
    def setUp(self):
        self.document = json.loads(Path(__file__).with_name("requirements.json").read_text())

    def test_ledger_shape(self):
        self.assertEqual([], CHECK.validate(self.document))

    def test_verified_without_evidence_rejected(self):
        document = copy.deepcopy(self.document)
        document["requirements"][0].update(status="verified", evidence=[])
        self.assertTrue(CHECK.validate(document))

    def test_failed_or_skipped_evidence_cannot_verify(self):
        for outcome in ("failed", "skipped", "unexecuted"):
            document = copy.deepcopy(self.document)
            document["requirements"][0].update(status="verified", evidence=[
                {"command": "test", "outcome": outcome, "scope": "real-node"}
            ])
            self.assertTrue(CHECK.validate(document))

    def test_all_acceptance_scenarios_required(self):
        document = copy.deepcopy(self.document)
        document["requirements"] = [
            item for item in document["requirements"] if item["id"] != "E2E-09"
        ]
        self.assertTrue(CHECK.validate(document))

    def test_unit_proof_does_not_replace_real_node_requirement(self):
        for identifier in CHECK.REAL_NODE_REQUIRED:
            with self.subTest(requirement=identifier):
                document = copy.deepcopy(self.document)
                item = next(item for item in document["requirements"] + document.get("amendments", [])
                            if item["id"] == identifier)
                item.update(status="verified", evidence=[
                    {"command": "go test ./...", "outcome": "passed", "scope": "unit"}
                ])
                self.assertTrue(CHECK.validate(document))

    def test_amendment_obligations_cannot_be_omitted(self):
        for identifier in ([f"UP-{number:02}" for number in range(1, 7)] +
                           [f"FS-{number:02}" for number in range(1, 5)]):
            document = copy.deepcopy(self.document)
            document["amendments"] = [
                item for item in document["amendments"] if item["id"] != identifier
            ]
            self.assertTrue(CHECK.validate(document))

    def test_old_revision_cannot_bypass_amendment_gate(self):
        document = copy.deepcopy(self.document)
        document.pop("contractRevision")
        document.pop("amendments")
        self.assertTrue(CHECK.validate(document))

    def test_historical_writable_root_receipt_cannot_qualify_immutable_filesystem(self):
        document = copy.deepcopy(self.document)
        for item in document["amendments"]:
            if item["id"].startswith("FS-"):
                item.update(status="verified", evidence=[{
                    "command": "historic accounting", "outcome": "passed", "scope": "real-node",
                    "result": "acceptance/results/cri-accounting.json"}])
        self.assertTrue(CHECK.validate(document))


if __name__ == "__main__":
    unittest.main()
