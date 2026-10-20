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
    def test_wasm_provenance_and_actual_companion_scope_cannot_be_replaced_by_labels(self):
        original = json.loads((ROOT / "results/managed-wasm-composition-arm64.json").read_text())
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
                for name in CHECK.NATIVE_SCOPES:
                    document["checks"][name]["outcome"] = "pending"
                document["checks"]["wasm-arm64"].update(outcome="passed", result="result.json")
                errors, _ = CHECK.validate(document, Path(directory))
                if case == "valid":
                    self.assertFalse(errors)
                else:
                    self.assertTrue(errors)

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
