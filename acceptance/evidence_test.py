import copy
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

from acceptance.evidence import (candidate_errors, input_hashes, load_receipt, production_manifest_bytes,
                                 receipt_errors, working_identity)


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.git("init", "-q")
        self.git("config", "user.email", "fixture@example.invalid")
        self.git("config", "user.name", "Synthetic fixture")
        (self.root / "go.mod").write_text("module fixture\n")
        (self.root / "acceptance/results").mkdir(parents=True)
        (self.root / "acceptance/requirements.json").write_text(json.dumps({
            "requirements": [{"id": "RUN-08", "requirement": "immutable obligation",
                              "status": "pending", "evidence": [
                                  {"command": "synthetic", "scope": "contract", "outcome": "pending"}]}]}))
        self.commit()
        self.identity = working_identity(self.root)
        self.result = {**self.identity, "outcome": "passed", "scope": "real-node-execution-only",
                       "architecture": "amd64", "selectedScenario": "complete",
                       "selectedLanguages": ["python"],
                       "cleanSelectedSuitePassed": True, "productionCleanupVerified": True,
                       "fixtureDestruction": {"outcome": "passed"},
                       "assertions": [{"name": "synthetic assertion", "outcome": "passed",
                                       "requirements": ["RUN-08"]}]}

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.root), *args], stderr=subprocess.PIPE)

    def commit(self):
        self.git("add", ".")
        self.git("commit", "-qm", "synthetic fixture")

    def test_valid_candidate_and_scope(self):
        self.assertEqual([], receipt_errors(self.result, ["RUN-08"], live=True, architecture="amd64",
                                           scenario="complete", languages=["python"],
                                           root=self.root))

    def test_production_manifest_string_keys_are_canonical_despite_path_component_order(self):
        paths = [Path("cmd/brewlet/main.go"), Path("cmd/brewlet-node/main_linux.go")]
        manifest = {path.as_posix(): "a" * 64 for path in sorted(paths)}
        self.assertNotEqual(list(manifest), sorted(manifest))
        expected = json.dumps(dict(sorted(manifest.items())), indent=2).encode()
        self.assertEqual(expected, production_manifest_bytes(manifest))
        self.assertNotEqual(json.dumps(manifest, indent=2).encode(), production_manifest_bytes(manifest))
        self.assertEqual(production_manifest_bytes(manifest),
                         production_manifest_bytes(dict(reversed(list(manifest.items())))))

    def test_missing_failed_uncovered_and_wrong_scope_receipts(self):
        changes = [
            {"outcome": "skipped"}, {"outcome": "failed"}, {"outcome": "intervened"},
            {"assertions": []}, {"assertions": [{"name": "missing outcome", "requirements": ["RUN-08"]}]},
            {"assertions": [{"name": "uncovered", "outcome": "passed", "requirements": []}]},
            {"fixtureDestruction": {"outcome": "retained"}}, {"fixtureDestruction": None},
            {"scope": "contract"}, {"cleanSelectedSuitePassed": False},
            {"productionCleanupVerified": False}, {"selectedScenario": "control"},
            {"architecture": "arm64"}, {"selectedLanguages": ["java", "python", "node", "dotnet"]},
            {"selectedLanguages": ["php"]}, {"selectedLanguages": [["python"]]},
            {"productionSourceManifestSHA256": "0" * 64}, {"checkout": "0" * 40},
        ]
        for change in changes:
            with self.subTest(change=change):
                result = {**self.result, **change}
                self.assertTrue(receipt_errors(result, ["RUN-08"], live=True, architecture="amd64",
                                               scenario="complete", languages=["python"],
                                               root=self.root))

    def test_receipt_paths_missing_malformed_duplicate_and_escape(self):
        for relative in ("", "acceptance/results/missing.json", "../outside.json", "go.mod"):
            with self.subTest(path=relative), self.assertRaises(ValueError):
                load_receipt(self.root, relative)
        path = self.root / "acceptance/results/test.json"
        for text in ("not json", "[]", '{"outcome":"passed","outcome":"failed"}'):
            path.write_text(text)
            with self.subTest(text=text), self.assertRaises(ValueError):
                load_receipt(self.root, "acceptance/results/test.json")
        path.unlink()
        path.symlink_to(self.root / "go.mod")
        with self.assertRaises(ValueError):
            load_receipt(self.root, "acceptance/results/test.json")

    def test_evidence_only_commits_keep_identity_but_obligations_do_not(self):
        (self.root / "acceptance/results/fixture.json").write_text('{"synthetic":true}')
        ledger = self.root / "acceptance/requirements.json"
        doc = json.loads(ledger.read_text())
        doc["requirements"][0]["status"] = "verified"
        doc["requirements"][0]["evidence"][0].update(outcome="passed", result="fixture.json")
        ledger.write_text(json.dumps(doc))
        self.commit()
        self.assertEqual([], candidate_errors(self.result, self.root))
        doc["requirements"][0]["requirement"] = "weakened obligation"
        ledger.write_text(json.dumps(doc))
        self.commit()
        self.assertTrue(candidate_errors(self.result, self.root))

    def test_evidence_command_and_scope_mappings_are_qualification_inputs(self):
        path = self.root / "acceptance/requirements.json"
        document = json.loads(path.read_text())
        document["requirements"][0]["evidence"][0]["command"] = "weaker check"
        path.write_text(json.dumps(document))
        self.commit()
        self.assertTrue(candidate_errors(self.result, self.root))

    def test_artifact_index_is_evidence_only_but_its_verifier_is_not(self):
        path = self.root / "acceptance/evidence-index.json"
        path.write_text('{"schemaVersion":1,"candidate":null}')
        self.commit()
        self.assertEqual([], candidate_errors(self.result, self.root))
        path.write_text('{"schemaVersion":1,"candidate":{"different":"artifact pointers"}}')
        self.commit()
        self.assertEqual([], candidate_errors(self.result, self.root))
        (self.root / "acceptance/evidence_index.py").write_text("changed verification policy")
        self.commit()
        self.assertTrue(candidate_errors(self.result, self.root))
    def test_source_tests_workflows_fixtures_and_modes_invalidate_identity(self):
        for name in ("pkg/source.go", "integration-tests/test.py", ".github/workflows/test.yml",
                     "integration-tests/fixtures/pin.json", "acceptance/check.py", "LICENSE.txt"):
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("new qualification input")
            self.commit()
            with self.subTest(path=name):
                self.assertTrue(candidate_errors(self.result, self.root))
            self.result.update(working_identity(self.root))
        path.chmod(0o755)
        self.commit()
        self.assertTrue(candidate_errors(self.result, self.root))

    def test_dirty_and_untracked_candidate_cannot_qualify(self):
        (self.root / "untracked").write_text("uncommitted")
        with self.assertRaises(ValueError):
            working_identity(self.root)
        self.assertTrue(candidate_errors(self.result, self.root))

    def test_assume_unchanged_cannot_hide_modified_inputs(self):
        self.git("update-index", "--assume-unchanged", "go.mod")
        (self.root / "go.mod").write_text("module uncommitted\n")
        self.assertEqual(b"", self.git("status", "--porcelain"))
        with self.assertRaisesRegex(ValueError, "working qualification inputs differ"):
            working_identity(self.root)

    def test_non_live_execution_requires_persistent_assertions_and_exact_scope(self):
        result = {**self.result, "scope": "contract"}
        self.assertEqual([], receipt_errors(result, ["RUN-08"], root=self.root))
        self.assertTrue(receipt_errors(self.result, ["RUN-08"], root=self.root))


if __name__ == "__main__":
    unittest.main()
