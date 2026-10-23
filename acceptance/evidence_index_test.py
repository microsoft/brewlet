import copy
import hashlib
import io
import json
from pathlib import Path
import shlex
import stat
import tempfile
import unittest
from unittest.mock import patch
import urllib.request
import urllib.error
import zipfile

import evidence_test
from acceptance import check, evidence_index as index, qualify, run_check, verify_evidence
from acceptance.evidence import ROOT, read_json, working_identity


def zipped(records):
    stream = io.BytesIO()
    with zipfile.ZipFile(stream, "w") as archive:
        for name, payload in records.items():
            archive.writestr(name, payload)
    return stream.getvalue()


class ArtifactTests(unittest.TestCase):
    def test_missing_access_deleted_expired_or_unavailable_bytes_are_errors_not_index_fallbacks(self):
        with self.assertRaisesRegex(ValueError, "GITHUB_TOKEN"):
            index.Actions(None)
        actions = index.Actions("synthetic-read-only-token")
        for code in (401, 403, 404, 410, 503):
            error = urllib.error.HTTPError("https://storage.invalid/signed-secret", code, "failure", {}, None)
            with self.subTest(code=code), patch.object(actions.opener, "open", side_effect=error), \
                    self.assertRaisesRegex(ValueError, f"HTTP {code}") as caught:
                actions.get("/actions/artifacts/22/zip", binary=True)
            self.assertNotIn("signed-secret", str(caught.exception))
            self.assertNotIn("synthetic-read-only-token", str(caught.exception))

    def fixture(self):
        source = "a" * 40
        name = "checks-contracts-" + source
        files = {"go-contracts.json": b"{}", "maven.json": b"{}"}
        record = {"runId": 11, "artifactId": 22, "name": name}
        responses = {
            "/actions/runs/11": {"event": "pull_request", "head_sha": source,
                "path": ".github/workflows/v2-ci.yml", "status": "completed", "conclusion": "success"},
            "/actions/artifacts/22": {"name": name, "expired": False,
                "workflow_run": {"id": 11, "head_sha": source}},
            "/actions/artifacts/22/zip": zipped(files)}
        return record, files, responses, {"checkout": source}

    def test_successful_ci_artifact_is_bound_to_workflow_run_source_and_scope(self):
        record, files, responses, identity = self.fixture()
        actions = SimpleActions(responses)
        self.assertEqual(files, index.download(actions, record, identity))
        for target, key, value in (
                ("/actions/runs/11", "conclusion", "failure"),
                ("/actions/runs/11", "status", "in_progress"),
                ("/actions/runs/11", "head_sha", "b" * 40),
                ("/actions/runs/11", "path", ".github/workflows/unrelated.yml"),
                ("/actions/artifacts/22", "expired", True),
                ("/actions/artifacts/22", "name", "unrelated")):
            bad = copy.deepcopy(responses)
            bad[target][key] = value
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                index.download(SimpleActions(bad), record, identity)
        bad = copy.deepcopy(responses)
        bad["/actions/artifacts/22"]["workflow_run"]["id"] = 99
        with self.assertRaises(ValueError):
            index.download(SimpleActions(bad), record, identity)

    def test_debug_and_pr_artifacts_cannot_be_indexed_as_full_qualification(self):
        for name in ("e2e-amd64-pr-", "e2e-amd64-control-", "checks-unrelated-"):
            with self.assertRaises(ValueError):
                index.artifact_files(name + "a" * 40, "a" * 40)
        self.assertEqual(33, len(index.required_files()))
        self.assertNotIn("pr-e2e-amd64.json", index.required_files())

    def test_documentation_receipts_come_from_the_standalone_website_workflow(self):
        source = "a" * 40
        name = "checks-documentation-" + source
        files = {"documentation.json": b"{}"}
        self.assertEqual((".github/workflows/site-pages.yml", set(files)),
                         index.artifact_files(name, source))
        record = {"runId": 11, "artifactId": 22, "name": name}
        responses = {
            "/actions/runs/11": {"event": "workflow_dispatch", "head_sha": source,
                "path": ".github/workflows/site-pages.yml", "status": "completed", "conclusion": "success"},
            "/actions/artifacts/22": {"name": name, "expired": False,
                "workflow_run": {"id": 11, "head_sha": source}},
            "/actions/artifacts/22/zip": zipped(files)}
        self.assertEqual(files, index.download(SimpleActions(responses), record, {"checkout": source}))
        for key, value in (("path", ".github/workflows/v2-ci.yml"),
                           ("event", "pull_request"), ("event", "push")):
            bad = copy.deepcopy(responses)
            bad["/actions/runs/11"][key] = value
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                index.download(SimpleActions(bad), record, {"checkout": source})

    def test_zip_missing_extra_traversal_symlink_and_expansion_fail_closed(self):
        for files in ({}, {"../good.json": b"{}"}, {"good.json": b"{}", "private.key": b"secret"}):
            with self.subTest(files=files), self.assertRaises(ValueError):
                index.unpack(zipped(files), {"good.json"})
        stream = io.BytesIO()
        with zipfile.ZipFile(stream, "w") as archive:
            info = zipfile.ZipInfo("good.json")
            info.external_attr = (stat.S_IFLNK | 0o777) << 16
            archive.writestr(info, "../private")
        with self.assertRaises(ValueError):
            index.unpack(stream.getvalue(), {"good.json"})
        with patch.object(index, "MAX_BYTES", 4), self.assertRaises(ValueError):
            index.unpack(zipped({"good.json": b"large"}), {"good.json"})

    def test_redirect_never_forwards_token_and_refuses_non_https(self):
        request = urllib.request.Request("https://api.github.com/repos/microsoft/brewlet/actions/artifacts/1/zip")
        request.add_unredirected_header("Authorization", "Bearer synthetic-secret")
        redirect = index.HTTPSRedirect()
        result = redirect.redirect_request(request, None, 302, "Found", {},
                                           "https://artifact-storage.example.invalid/signed")
        self.assertNotIn("Authorization", result.headers)
        self.assertNotIn("Authorization", result.unredirected_hdrs)
        with self.assertRaises(ValueError):
            redirect.redirect_request(request, None, 302, "Found", {}, "http://unsafe.invalid/file")

    def test_materialization_never_overwrites_old_or_follows_symlink(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "acceptance/results").mkdir(parents=True)
            (root / "acceptance/results/go-contracts.json").write_text("original")
            with self.assertRaises(ValueError):
                index.materialize(root, {"go-contracts.json": b"replacement"})
            self.assertEqual("original", (root / "acceptance/results/go-contracts.json").read_text())
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "acceptance").mkdir()
            (root / "elsewhere").mkdir()
            (root / "acceptance/results").symlink_to(root / "elsewhere", target_is_directory=True)
            with self.assertRaises(ValueError):
                index.materialize(root, {"go-contracts.json": b"replacement"})

    def test_history_export_checks_original_bytes_and_recording_archive_is_not_qualification(self):
        fixture = evidence_test.EvidenceTests()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        relative = "acceptance/results/original.json"
        original = b'{"outcome":"failed","original":true}\n'
        (fixture.root / relative).write_bytes(original)
        fixture.commit()
        source = working_identity(fixture.root)["checkout"]
        history = {"commit": source, "files": [{"path": relative, "size": len(original),
            "sha256": hashlib.sha256(original).hexdigest()}], "artifactRunId": None}
        output = fixture.root / "archive"
        self.assertEqual(1, index.export_history(fixture.root, history, output))
        self.assertEqual(original, (output / relative).read_bytes())
        with self.assertRaises(FileExistsError):
            index.export_history(fixture.root, history, output)
        bad = copy.deepcopy(history)
        bad["files"][0]["sha256"] = "0" * 64
        with self.assertRaises(ValueError):
            index.export_history(fixture.root, bad, fixture.root / "bad")
        document = {"schemaVersion": 1, "history": history, "candidate": None}
        actions = SimpleActions({
            "/actions/runs/11": {"path": ".github/workflows/v2-ci.yml", "head_sha": "a" * 40},
            "/actions/runs/11/artifacts?per_page=100": {"artifacts": [{
                "id": 22, "name": "historical-acceptance-receipts-" + "a" * 40, "expired": False}]},
            "/actions/artifacts/22/zip": zipped({relative: original,
                "index.json": json.dumps(history).encode()})})
        recorded = index.record_history(actions, document, 11)
        self.assertEqual(11, recorded["history"]["artifactRunId"])
        self.assertIsNone(recorded["candidate"])
        self.assertIsNone(document["history"]["artifactRunId"])
        self.assertTrue(verify_evidence.validate(fixture.root, recorded, expected=fixture.identity))

    def test_recorded_history_does_not_require_original_git_objects_on_later_ci(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "acceptance").mkdir()
            document = {"schemaVersion": 1, "candidate": None,
                        "history": {"artifactRunId": 11, "artifactId": 22, "commit": "a" * 40, "files": []}}
            (root / "acceptance/evidence-index.json").write_text(json.dumps(document))
            output = root / "step-output"
            with patch.object(index, "ROOT", root), patch.dict("os.environ", {"GITHUB_OUTPUT": str(output)}), \
                    patch("sys.argv", ["evidence_index.py", "export-history", "--if-needed",
                                       "--output", str(root / "not-created")]):
                self.assertEqual(0, index.main())
            self.assertEqual("written=false\n", output.read_text())
            self.assertFalse((root / "not-created").exists())


class SimpleActions:
    def __init__(self, responses):
        self.responses = responses

    def get(self, path, **_kwargs):
        return self.responses[path]


class IndexedAcceptanceTests(unittest.TestCase):
    def setUp(self):
        self.fixture = evidence_test.EvidenceTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.root = self.fixture.root
        for filename in ("requirements.json", "composition-requirements.json"):
            (self.root / "acceptance" / filename).write_bytes((ROOT / "acceptance" / filename).read_bytes())
        (self.root / ".gitignore").write_text("/acceptance/results/\n")
        self.fixture.commit()
        self.identity = working_identity(self.root)
        obligations = sorted(check.REQUIRED_IDS | {
            row["id"] for row in read_json(ROOT / "acceptance/composition-requirements.json")["requirements"]})
        assertion = [{"name": "synthetic isolated Git fixture, never public evidence",
                      "outcome": "passed", "requirements": obligations}]
        base = {**self.fixture.result, **self.identity, "assertions": assertion, "fullAcceptance": False}
        records = {}
        for name, definition in run_check.CHECKS.items():
            for architecture in (("amd64", "arm64") if definition.get("native") else ("amd64",)):
                filename = name + ("-" + architecture if definition.get("native") else "") + ".json"
                receipt = {**copy.deepcopy(base), "scope": definition["scope"],
                           "command": shlex.join(definition["command"])}
                if definition.get("native"):
                    receipt.update(architecture=architecture, nativeExecution=True)
                records[filename] = receipt
        for architecture in ("amd64", "arm64"):
            for suite, (_, scenario, languages) in qualify.SUITES.items():
                receipt = {**copy.deepcopy(base), "architecture": architecture,
                           "selectedScenario": scenario, "selectedLanguages": list(languages)}
                if suite == "tutorial":
                    receipt["scope"] = "integration"
                if suite == "core":
                    receipt.update(scope="real-node-execution-and-remote-delivery",
                                   kubeletPullVerified=True, tlsTransportVerified=True)
                if suite == "managed-wasm":
                    provenance = read_json(ROOT / "acceptance/testdata/wasm-receipt.json")
                    receipt.update({key: provenance[key] for key in ("wasmGuestInputs", "wasmRuntimes")})
                records[f"candidate-{architecture}-{suite}.json"] = receipt
        for name, receipt in records.items():
            (self.root / "acceptance/results" / name).write_text(json.dumps(receipt))
        names = ["checks-contracts-", "checks-documentation-",
                 *[f"checks-{group}-{architecture}-" for group in ("producer", "kernel")
                   for architecture in ("amd64", "arm64")],
                 *[f"e2e-{architecture}-{scenario}-" for architecture in ("amd64", "arm64")
                   for scenario in ("complete", "managed", "managed-wasm")]]
        artifacts = []
        for number, prefix in enumerate(names, 1):
            name = prefix + self.identity["checkout"]
            _, files = index.artifact_files(name, self.identity["checkout"])
            artifacts.append({"runId": number, "artifactId": number + 100, "name": name,
                "files": {filename: hashlib.sha256((self.root / "acceptance/results" / filename).read_bytes()).hexdigest()
                          for filename in files}})
        self.index = {"schemaVersion": 1, "history": {}, "candidate": {
            "identity": self.identity, "artifacts": artifacts}}
        (self.root / "acceptance/evidence-index.json").write_text(json.dumps(self.index))
        self.fixture.commit()

    def update(self, name, change):
        path = self.root / "acceptance/results" / name
        receipt = read_json(path)
        change(receipt)
        path.write_text(json.dumps(receipt))
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        for artifact in self.index["candidate"]["artifacts"]:
            if name in artifact["files"]:
                artifact["files"][name] = digest

    def actions(self):
        responses = {}
        for artifact in self.index["candidate"]["artifacts"]:
            run_id, artifact_id, name = (artifact[key] for key in ("runId", "artifactId", "name"))
            workflow, files = index.artifact_files(name, self.identity["checkout"])
            responses[f"/actions/runs/{run_id}"] = {
                "event": "pull_request" if workflow.endswith("v2-ci.yml") else "workflow_dispatch",
                "head_sha": self.identity["checkout"], "path": workflow,
                "status": "completed", "conclusion": "success"}
            responses[f"/actions/runs/{run_id}/artifacts?per_page=100"] = {
                "artifacts": [{"id": artifact_id, "name": name}]}
            responses[f"/actions/artifacts/{artifact_id}"] = {"name": name, "expired": False,
                "workflow_run": {"id": run_id, "head_sha": self.identity["checkout"]}}
            responses[f"/actions/artifacts/{artifact_id}/zip"] = zipped({
                filename: (self.root / "acceptance/results" / filename).read_bytes() for filename in files})
        return SimpleActions(responses)

    def test_all_actual_bindings_can_pass_without_rewriting_source_declarations(self):
        original = (self.root / "acceptance/requirements.json").read_bytes()
        composition = (self.root / "acceptance/composition-requirements.json").read_bytes()
        self.assertEqual([], verify_evidence.validate(self.root, self.index))
        self.assertEqual(original, (self.root / "acceptance/requirements.json").read_bytes())
        self.assertEqual(composition, (self.root / "acceptance/composition-requirements.json").read_bytes())
        self.assertTrue(all(row["status"] == "pending"
                            for row in read_json(self.root / "acceptance/requirements.json")["requirements"]))

    def test_bad_payload_is_rejected_even_if_index_checksum_is_updated(self):
        cases = (
            ("candidate-amd64-core.json", lambda r: r.update(selectedScenario="pr")),
            ("candidate-arm64-managed.json", lambda r: r.update(productionCleanupVerified=False)),
            ("candidate-arm64-managed-wasm.json", lambda r: r.update(selectedLanguages=["wasmtime"])),
            ("go-contracts.json", lambda r: r.update(assertions=[])),
            ("native-contracts-arm64.json", lambda r: r.update(nativeExecution=False)),
            ("documentation.json", lambda r: r.update(command="weaker command")),
        )
        for name, change in cases:
            original = (self.root / "acceptance/results" / name).read_bytes()
            original_index = copy.deepcopy(self.index)
            self.update(name, change)
            with self.subTest(name=name):
                self.assertTrue(verify_evidence.validate(self.root, self.index))
            (self.root / "acceptance/results" / name).write_bytes(original)
            self.index = original_index

    def test_missing_changed_stale_or_unknown_evidence_never_qualifies(self):
        self.assertTrue(verify_evidence.validate(self.root, dict(self.index, candidate=None)))
        bad = copy.deepcopy(self.index)
        bad["candidate"]["identity"]["qualificationInputsSHA256"] = "0" * 64
        self.assertTrue(verify_evidence.validate(self.root, bad))
        bad = copy.deepcopy(self.index)
        bad["candidate"]["artifacts"].pop()
        self.assertTrue(verify_evidence.validate(self.root, bad))
        path = self.root / "acceptance/results/go-contracts.json"
        path.write_text('{"outcome":"passed"}')
        self.assertTrue(verify_evidence.validate(self.root, self.index))

    def test_an_index_and_digests_without_actual_assertion_bytes_never_qualify(self):
        for path in (self.root / "acceptance/results").iterdir():
            path.unlink()
        errors = verify_evidence.validate(self.root, self.index)
        self.assertTrue(errors)
        self.assertTrue(any("unavailable public receipt" in error for error in errors))

    def test_fetch_requires_all_checksums_and_the_same_candidate_inputs(self):
        actions = self.actions()
        with patch.object(index, "ROOT", self.root):
            records = index.candidate_records(actions, self.index["candidate"], working_identity(self.root))
            self.assertEqual(index.required_files(), set(records))
            bad = copy.deepcopy(self.index["candidate"])
            bad["artifacts"][0]["files"]["maven.json"] = "0" * 64
            with self.assertRaisesRegex(ValueError, "checksum mismatch"):
                index.candidate_records(actions, bad, working_identity(self.root))
            bad = copy.deepcopy(self.index["candidate"])
            bad["identity"]["qualificationInputsSHA256"] = "0" * 64
            with self.assertRaisesRegex(ValueError, "identity mismatch"):
                index.candidate_records(actions, bad, working_identity(self.root))

    def test_recording_reviewed_runs_verifies_payloads_without_rewriting_any_ledger_or_index(self):
        actions = self.actions()
        original_index = (self.root / "acceptance/evidence-index.json").read_bytes()
        for path in (self.root / "acceptance/results").iterdir():
            path.unlink()
        expected = working_identity(self.root)
        runs = [row["runId"] for row in self.index["candidate"]["artifacts"]]
        result = index.record_candidate(actions, self.root, dict(self.index, candidate=None), runs, expected)
        self.assertEqual(self.identity, result["candidate"]["identity"])
        self.assertEqual([], verify_evidence.validate(self.root, result))
        self.assertEqual(expected, working_identity(self.root))
        self.assertEqual(original_index, (self.root / "acceptance/evidence-index.json").read_bytes())

if __name__ == "__main__":
    unittest.main()
