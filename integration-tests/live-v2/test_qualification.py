import ast
from pathlib import Path
import unittest

from acceptance.qualification import fixture_names
import harness
import wasm
import accounting
from acceptance import composition_check


class QualificationSelectionTests(unittest.TestCase):
    def test_actual_accounting_assertion_records_both_storage_obligations(self):
        tree = ast.parse(Path(accounting.__file__).read_text())
        calls = [node for node in ast.walk(tree) if isinstance(node, ast.Call)
                 and isinstance(node.func, ast.Attribute) and node.func.attr == "record"
                 and len(node.args) == 3]
        self.assertEqual(1, len(calls))
        self.assertEqual(["RUN-08", "FS-04"], ast.literal_eval(calls[0].args[2]))

    def test_native_and_wasm_harness_selection_uses_the_gate_matrix(self):
        matrix = harness.QUALIFICATION_MATRIX
        self.assertEqual(harness.LANGUAGES, fixture_names(matrix, "native"))
        self.assertEqual(tuple(harness.IDS), fixture_names(matrix, "native"))
        self.assertEqual(harness.WASM_HOSTS, fixture_names(matrix, "wasm"))
        self.assertEqual(tuple(wasm.IDS), fixture_names(matrix, "wasm"))
        for name, declaration in matrix["compositionScopes"].items():
            architecture, scenario, workloads, obligations = composition_check.NATIVE_SCOPES[name]
            self.assertEqual(architecture, declaration["architecture"])
            self.assertEqual(scenario, declaration["scenario"])
            self.assertEqual(workloads, set(fixture_names(matrix, declaration["group"])))
            self.assertEqual(len(obligations), 8 if declaration["group"] == "native" else 4)

    def test_existing_fixture_ids_and_receipt_selection_order_are_preserved(self):
        self.assertEqual(tuple(harness.IDS), ("java", "python", "node", "dotnet"))
        self.assertEqual(wasm.IDS, {"wasmtime": "wasmtime-wasip1", "spin": "spin-http-sdk7"})
