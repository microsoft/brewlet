import copy
import importlib.util
import json
from pathlib import Path
import unittest
import evidence_test
from acceptance.evidence import working_identity

SPEC = importlib.util.spec_from_file_location("check", Path(__file__).with_name("check.py"))
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class LedgerTests(unittest.TestCase):
    def setUp(self):
        self.document = json.loads(Path(__file__).with_name("requirements.json").read_text())

    def test_ledger_shape(self):
        self.assertEqual([], CHECK.validate(self.document, structure_only=True))

    def test_historical_declarations_do_not_qualify_current_candidate(self):
        document = copy.deepcopy(self.document)
        for row in document["requirements"] + document.get("amendments", []):
            row.update(row["historicalEvidence"])
        self.assertTrue(CHECK.validate(document, expected={"checkout": "0" * 40}))

    def test_synthetic_complete_candidate_and_assertion_coverage(self):
        fixture = evidence_test.EvidenceTests()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        document = copy.deepcopy(self.document)
        for row in document["requirements"] + document.get("amendments", []):
            row["status"] = "verified"
            live = row["id"] in CHECK.REAL_NODE_REQUIRED
            row["evidence"] = [{"scope": "real-node" if live else "contract",
                                "command": "synthetic assertion fixture", "outcome": "passed",
                                "result": "acceptance/results/live.json" if live
                                else "acceptance/results/contracts.json"}]
        ledger = fixture.root / "acceptance/requirements.json"
        ledger.write_text(json.dumps(document))
        fixture.commit()
        identity = working_identity(fixture.root)
        result = {**fixture.result, **identity,
                  "command": "synthetic assertion fixture",
                  "assertions": [{"name": "synthetic fixture, not public evidence", "outcome": "passed",
                                  "requirements": sorted(CHECK.REQUIRED_IDS)}]}
        for name, scope in (("live", "real-node-execution-only"), ("contracts", "contract")):
            (fixture.root / f"acceptance/results/{name}.json").write_text(json.dumps({**result, "scope": scope}))
        fixture.commit()
        self.assertEqual([], CHECK.validate(document, fixture.root))
        result["assertions"][0]["requirements"].remove("LEASE-02")
        (fixture.root / "acceptance/results/live.json").write_text(json.dumps(result))
        fixture.commit()
        self.assertTrue(any("LEASE-02" in error for error in CHECK.validate(document, fixture.root)))

    def test_no_baseline_obligation_or_real_node_scope_can_be_removed(self):
        document = copy.deepcopy(self.document)
        document["requirements"] = [row for row in document["requirements"] if row["id"] != "ARCH-01"]
        self.assertTrue(CHECK.validate(document, structure_only=True))
        document = copy.deepcopy(self.document)
        for row in document["requirements"]:
            if row["id"] == "ARCH-01":
                row["status"] = "verified"
                row["evidence"] = [{"command": "contract only", "scope": "contract", "outcome": "passed"}]
        self.assertTrue(CHECK.validate(document, structure_only=True))

    def test_non_filesystem_missing_receipts_fail_closed(self):
        document = copy.deepcopy(self.document)
        for item in document["requirements"] + document.get("amendments", []):
            if not item["id"].startswith("FS-"):
                item["status"] = "verified"
                for evidence in item["evidence"]:
                    evidence["outcome"] = "passed"
                    if evidence.get("result"):
                        evidence["result"] = "acceptance/results/nonexistent-audit-receipt.json"
        errors = CHECK.validate(document, expected={"checkout": "0" * 40})
        self.assertTrue(any("existing file beneath acceptance/results" in error for error in errors))

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
