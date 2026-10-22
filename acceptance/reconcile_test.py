import copy
import json
from pathlib import Path
import unittest
from unittest.mock import patch

import evidence_test
from acceptance import check, qualify, reconcile
from acceptance.evidence import working_identity


class BindingPlanTests(unittest.TestCase):
    def setUp(self):
        self.document = json.loads(Path(__file__).with_name("requirements.json").read_text())

    def test_prepare_preserves_obligations_and_original_history_without_executing_anything(self):
        with patch("subprocess.run") as command:
            prepared = reconcile.prepare(self.document)
        command.assert_not_called()
        original = {row["id"]: row for row in self.document["requirements"] + self.document["amendments"]}
        for row in prepared["requirements"] + prepared["amendments"]:
            before = original[row["id"]]
            self.assertEqual((before["requirement"], before["section"]), (row["requirement"], row["section"]))
            self.assertEqual(before["historicalEvidence"], row["historicalEvidence"])
            self.assertEqual("pending", row["status"])
            self.assertTrue(all(item["outcome"] == "pending" for item in row["evidence"]))
        self.assertEqual(check.REQUIRED_IDS, set(original))
        self.assertEqual(prepared, reconcile.prepare(prepared))

    def test_every_mandatory_live_binding_requires_both_architectures_and_selected_suite(self):
        bindings = reconcile.bindings()
        for identifier in check.REAL_NODE_REQUIRED:
            live = [row for row in bindings[identifier] if row["scope"] == "real-node"]
            self.assertEqual({"amd64", "arm64"}, {row["architecture"] for row in live})
            self.assertTrue(all(row["suite"] in qualify.SUITES for row in live))
        for identifier in reconcile.CHECKS["native-contracts"]["requirements"]:
            native = [row for row in bindings[identifier] if "native-contracts-" in row["result"]]
            self.assertEqual({"amd64", "arm64"}, {row["architecture"] for row in native})

    def test_active_ledger_commands_and_scopes_match_the_reviewed_plan(self):
        bindings = reconcile.bindings()
        def immutable(evidence):
            return [{key: value for key, value in row.items() if key not in ("outcome", "result")}
                    for row in evidence]
        for row in self.document["requirements"] + self.document["amendments"]:
            with self.subTest(identifier=row["id"]):
                self.assertEqual(immutable(bindings[row["id"]]), immutable(row["evidence"]))

    def test_unmapped_live_obligations_or_unknown_suites_are_source_errors(self):
        suites = copy.deepcopy(reconcile.LIVE_SUITES)
        suites["upstream"].remove("UP-05")
        with patch.object(reconcile, "LIVE_SUITES", suites), self.assertRaisesRegex(ValueError, "UP-05"):
            reconcile.bindings()
        with patch.object(reconcile, "LIVE_SUITES", {"invented": ["UP-05"]}), \
                self.assertRaisesRegex(ValueError, "unknown bound suite"):
            reconcile.bindings()


class BoundReceiptTests(unittest.TestCase):
    def setUp(self):
        self.fixture = evidence_test.EvidenceTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.document = reconcile.prepare(json.loads(Path(__file__).with_name("requirements.json").read_text()))
        for row in self.document["requirements"] + self.document["amendments"]:
            row["status"] = "verified"
            for item in row["evidence"]:
                item["outcome"] = "passed"
        (self.fixture.root / "acceptance/requirements.json").write_text(json.dumps(self.document))
        self.fixture.commit()
        identity = working_identity(self.fixture.root)
        receipts = {}
        for row in self.document["requirements"] + self.document["amendments"]:
            for item in row["evidence"]:
                result = {**self.fixture.result, **identity, "command": item["command"], "scope": item["scope"],
                          "architecture": item.get("architecture", "amd64"), "nativeExecution": True,
                          "assertions": [{"name": "synthetic unit fixture, NOT execution evidence",
                                          "outcome": "passed", "requirements": sorted(check.REQUIRED_IDS)}]}
                if "suite" in item:
                    _, scenario, languages = qualify.SUITES[item["suite"]]
                    result.update(scope="real-node-execution-only", selectedScenario=scenario,
                                  selectedLanguages=list(languages))
                    if item["suite"] == "core":
                        result.update(scope="real-node-execution-and-remote-delivery",
                                      kubeletPullVerified=True, tlsTransportVerified=True)
                receipts[item["result"]] = result
        for relative, result in receipts.items():
            (self.fixture.root / relative).write_text(json.dumps(result))
        self.fixture.commit()

    def test_complete_synthetic_bindings_validate_without_weakening_scope(self):
        self.assertEqual([], check.validate(self.document, self.fixture.root))

    def test_native_contract_receipt_cannot_substitute_the_other_architecture(self):
        path = self.fixture.root / "acceptance/results/native-contracts-arm64.json"
        result = json.loads(path.read_text())
        result["architecture"] = "amd64"
        path.write_text(json.dumps(result))
        self.fixture.commit()
        self.assertTrue(any("native architecture" in error for error in check.validate(self.document, self.fixture.root)))

    def test_partial_control_cannot_substitute_complete_core_bindings(self):
        path = self.fixture.root / "acceptance/results/candidate-amd64-core.json"
        result = json.loads(path.read_text())
        result["selectedScenario"] = "control"
        path.write_text(json.dumps(result))
        self.fixture.commit()
        self.assertTrue(any("scenario" in error for error in check.validate(self.document, self.fixture.root)))

    def test_historical_receipt_metadata_cannot_be_rewritten_in_evidence_only_descendant(self):
        document = copy.deepcopy(self.document)
        document["requirements"][0]["historicalEvidence"]["evidence"][0]["outcome"] = "failed"
        (self.fixture.root / "acceptance/requirements.json").write_text(json.dumps(document))
        self.fixture.commit()
        self.assertTrue(any("candidate identity mismatch" in error for error in check.validate(document, self.fixture.root)))


if __name__ == "__main__":
    unittest.main()
