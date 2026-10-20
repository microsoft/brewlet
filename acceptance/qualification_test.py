import copy
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

from acceptance import qualification


ROOT = Path(__file__).resolve().parents[1]


class QualificationMatrixTests(unittest.TestCase):
    def setUp(self):
        self.document = qualification.load_matrix()

    def test_all_five_categories_six_fixtures_and_both_architectures_are_explicit(self):
        self.assertEqual(qualification.fixture_names(self.document, "native"),
                         ("java", "python", "node", "dotnet"))
        self.assertEqual(qualification.fixture_names(self.document, "wasm"), ("wasmtime", "spin"))
        categories = {row["category"] for rows in self.document["fixtureGroups"].values() for row in rows}
        self.assertEqual(categories, {"java", "python", "node", "dotnet", "webassembly"})
        self.assertEqual(set(self.document["architectures"]), {"arm64", "amd64"})
        self.assertEqual(set(self.document["compositionScopes"]),
                         {"arm64", "amd64", "wasm-arm64", "wasm-amd64"})

    def test_matrix_cannot_omit_duplicate_or_reclassify_required_fixtures(self):
        for group, rows in self.document["fixtureGroups"].items():
            for index in range(len(rows)):
                for change in ("missing", "duplicate", "unknown-id", "wrong-category", "malformed"):
                    with self.subTest(group=group, fixture=rows[index]["id"], change=change):
                        document = copy.deepcopy(self.document)
                        selected = document["fixtureGroups"][group]
                        if change == "missing":
                            selected.pop(index)
                        elif change == "duplicate":
                            selected.append(copy.deepcopy(selected[index]))
                        elif change == "unknown-id":
                            selected[index]["id"] = "unknown"
                        elif change == "wrong-category":
                            selected[index]["category"] = "unknown"
                        else:
                            selected[index] = {"id": []}
                        self.assertTrue(qualification.validate_matrix(document))
        for group in self.document["fixtureGroups"]:
            document = copy.deepcopy(self.document)
            del document["fixtureGroups"][group]
            self.assertTrue(qualification.validate_matrix(document))
        document = copy.deepcopy(self.document)
        document["fixtureGroups"]["extra"] = []
        self.assertTrue(qualification.validate_matrix(document))

    def test_scope_coverage_and_references_are_independent_contract_requirements(self):
        for architecture in ("arm64", "amd64"):
            document = copy.deepcopy(self.document)
            document["architectures"].remove(architecture)
            self.assertTrue(qualification.validate_matrix(document))
        document = copy.deepcopy(self.document)
        document["architectures"] = ["arm64", "arm64"]
        self.assertTrue(qualification.validate_matrix(document))
        for name in self.document["compositionScopes"]:
            document = copy.deepcopy(self.document)
            del document["compositionScopes"][name]
            self.assertTrue(qualification.validate_matrix(document))
            for key, value in (("architecture", "unknown"), ("group", "unknown"),
                               ("group", "wasm" if not name.startswith("wasm-") else "native"),
                               ("scenario", "wasm-runc-examples"), ("extra", "unsupported")):
                with self.subTest(scope=name, key=key, value=value):
                    document = copy.deepcopy(self.document)
                    document["compositionScopes"][name][key] = value
                    self.assertTrue(qualification.validate_matrix(document))

    def test_malformed_declarations_fail_without_type_errors_or_defaults(self):
        for value in (None, [], {}, {**self.document, "schemaVersion": True},
                      {**self.document, "architectures": [[]]},
                      {**self.document, "architectures": ["arm64", "unknown"]},
                      {**self.document, "fixtureGroups": []},
                      {**self.document, "compositionScopes": []},
                      {**self.document, "extra": "unsupported"}):
            with self.subTest(value=value):
                self.assertTrue(qualification.validate_matrix(value))
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "matrix.json"
            with self.assertRaises(FileNotFoundError):
                qualification.load_matrix(path)
            path.write_text("{")
            with self.assertRaises(json.JSONDecodeError):
                qualification.load_matrix(path)
            path.write_text("{}")
            with self.assertRaisesRegex(ValueError, "invalid qualification matrix"):
                qualification.load_matrix(path)
            path.write_text('{"schemaVersion": 1, "schemaVersion": 1}')
            with self.assertRaisesRegex(ValueError, "duplicate field schemaVersion"):
                qualification.load_matrix(path)

    def test_acceptance_entry_points_and_harness_refuse_a_partial_matrix(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            scripts = ("acceptance/check.py", "acceptance/composition_check.py",
                       "integration-tests/live-v2/harness.py")
            for relative in (*scripts, "acceptance/__init__.py", "acceptance/qualification.py"):
                destination = root / relative
                destination.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(ROOT / relative, destination)
            document = copy.deepcopy(self.document)
            document["fixtureGroups"]["wasm"].pop()
            (root / "acceptance/qualification-matrix.json").write_text(json.dumps(document))
            for script in scripts:
                with self.subTest(script=script):
                    result = subprocess.run([sys.executable, str(root / script)], cwd=root,
                                            capture_output=True, text=True, timeout=20)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("invalid qualification matrix", result.stderr)
