import copy
import importlib.util
import json
import shlex
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import evidence_test
from acceptance.evidence import working_identity
from acceptance.run_check import CHECKS

ROOT = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("composition_check", ROOT / "composition_check.py")
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class CompositionLedgerTests(unittest.TestCase):
    def test_wasm_provenance_and_actual_companion_scope_cannot_be_replaced_by_labels(self):
        original = json.loads((ROOT / "testdata/wasm-receipt.json").read_text())
        for case in ("valid", "missing-spin", "old-runtime-suite", "false-upstream-root", "missing-guest-hash"):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                result = copy.deepcopy(original)
                if case == "missing-spin":
                    result["selectedLanguages"] = ["wasmtime"]
                elif case == "old-runtime-suite":
                    result["selectedScenario"] = "wasm-runc-examples"
                elif case == "false-upstream-root":
                    for revisions in result["wasmRuntimes"].values():
                        for record in revisions.values():
                            record["sourceKind"] = "unchanged-upstream-engine-image"
                elif case == "missing-guest-hash":
                    result["wasmGuestInputs"]["command"].pop("sha256")
                path = Path(directory) / "result.json"
                path.write_text(json.dumps(result))
                document = copy.deepcopy(self.document)
                for name in document["checks"]:
                    document["checks"][name]["outcome"] = "pending"
                document["checks"]["wasm-arm64"].update(outcome="passed", result="result.json")
                (Path(directory) / "acceptance/results").mkdir(parents=True)
                path.rename(Path(directory) / "acceptance/results/result.json")
                document["checks"]["wasm-arm64"]["result"] = "results/result.json"
                with patch.object(CHECK, "receipt_errors", return_value=[]):
                    errors, _ = CHECK.validate(document, Path(directory) / "acceptance",
                                               expected={"checkout": "0" * 40})
                if case == "valid":
                    self.assertFalse(errors)
                else:
                    self.assertTrue(errors)

    def setUp(self):
        self.document = json.loads((ROOT / "composition-requirements.json").read_text())

    def test_shape_and_incomplete_gate(self):
        errors, incomplete = CHECK.validate(self.document, ROOT, structure_only=True)
        self.assertFalse(errors)
        if incomplete:
            errors, _ = CHECK.validate(self.document, ROOT, True)
            self.assertTrue(errors)

    def test_current_commands_match_fixed_receipt_producers_without_relabeling_history(self):
        for name, producer in (("contracts", "go-contracts"), ("maven", "maven"),
                               ("documentation", "documentation")):
            check = self.document["checks"][name]
            self.assertEqual(shlex.join(CHECKS[producer]["command"]), check["command"])
            self.assertEqual(CHECKS[producer]["scope"], check["scope"])
            self.assertEqual("results/" + producer + ".json", check["result"])
            obligations = {row["id"] for row in self.document["requirements"] if name in row["checks"]}
            self.assertLessEqual(obligations, set(CHECKS[producer]["requirements"]))
        self.assertTrue(all(check["outcome"] == "pending" for check in self.document["checks"].values()))
        self.assertTrue(all(check["outcome"] == "passed" for check in self.document["historicalChecks"].values()))
        for name, (architecture, _, _, _) in CHECK.NATIVE_SCOPES.items():
            suffix = "managed-wasm" if name.startswith("wasm-") else "managed"
            self.assertEqual(f"results/candidate-{architecture}-{suffix}.json",
                             self.document["checks"][name]["result"])

    def test_synthetic_complete_candidate_and_native_scope_cannot_be_downgraded(self):
        fixture = evidence_test.EvidenceTests()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        document = copy.deepcopy(self.document)
        for name, check in document["checks"].items():
            check["outcome"] = "passed"
            check["result"] = f"results/{name}.json"
        (fixture.root / "acceptance/composition-requirements.json").write_text(json.dumps(document))
        fixture.commit()
        identity = working_identity(fixture.root)
        for name, check in document["checks"].items():
            obligations = [row["id"] for row in document["requirements"] if name in row["checks"]]
            result = {**fixture.result, **identity, "command": check["command"],
                      "scope": check["scope"], "assertions": [{
                          "name": "synthetic fixture, not public evidence", "outcome": "passed",
                          "requirements": obligations}]}
            if name in CHECK.NATIVE_SCOPES:
                architecture, scenario, languages, _ = CHECK.NATIVE_SCOPES[name]
                result.update(scope="real-node-execution-only", architecture=architecture,
                              selectedScenario=scenario, selectedLanguages=sorted(languages))
                if name.startswith("wasm"):
                    provenance = json.loads((ROOT / "testdata/wasm-receipt.json").read_text())
                    result.update({key: provenance[key] for key in ("wasmGuestInputs", "wasmRuntimes")})
            (fixture.root / "acceptance" / check["result"]).write_text(json.dumps(result))
        fixture.commit()
        self.assertEqual(([], []), CHECK.validate(document, fixture.root / "acceptance", True))
        document["checks"]["arm64"]["scope"] = "contract"
        self.assertTrue(CHECK.validate(document, fixture.root / "acceptance", True)[0])

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

    def test_four_language_pass_cannot_qualify_missing_wasm_hosts_or_architectures(self):
        document = copy.deepcopy(self.document)
        document["checks"].pop("wasm-amd64")
        self.assertTrue(CHECK.validate(document, ROOT)[0])
        document = copy.deepcopy(self.document)
        document["requirements"] = [row for row in document["requirements"]
                                    if not row["id"].startswith("COMP-WASM-")]
        self.assertTrue(CHECK.validate(document, ROOT)[0])
        for name in ("wasm-arm64", "wasm-amd64"):
            document = copy.deepcopy(self.document)
            document["checks"][name]["outcome"] = "passed"
            document["checks"][name]["result"] = "results/managed-composition-" + document["checks"][name]["architecture"] + ".json"
            self.assertTrue(CHECK.validate(document, ROOT)[0], "native-language receipt qualified Wasm")
        document = copy.deepcopy(self.document)
        document["requirements"][0]["checks"] = ["unknown"]
        self.assertTrue(CHECK.validate(document, ROOT)[0])
