"""Offline release tests use synthetic artifacts only, never release receipts."""

import copy
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

from scripts import release as r
from scripts import release_assurance as a
from scripts import publish_release as p
from scripts import release_tools as tools


class IdentityTests(unittest.TestCase):
    def test_current_coordinates_and_metadata_agree(self):
        config = r.policy()
        self.assertEqual("2.0.0-alpha.1", config["version"])
        fixture = r.ET.parse(r.ROOT / "integration-tests/fixtures/java/app/pom.xml").getroot()
        plugin = next(item for item in fixture.findall("m:build/m:plugins/m:plugin", r.NS)
                      if item.findtext("m:artifactId", namespaces=r.NS) == "brewlet-maven-plugin")
        self.assertEqual(config["version"], plugin.findtext("m:version", namespaces=r.NS))
        self.assertIsNone(config["publicationPolicy"]["legalApproval"])
        self.assertIsNone(config["publicationPolicy"]["mavenSigningFingerprint"])

    def test_only_explicit_prerelease_version_policy_is_accepted(self):
        for value in ("2.0.0", "v2.0.0-alpha.1", "2.0", "2.0.0-SNAPSHOT",
                      "2.0.0-alpha.01", "2.0.0-alpha.1+mutable", "../../file", "2.0.0-rc.1\n"):
            self.assertIsNone(r.VERSION.fullmatch(value), value)
        for value in ("2.0.0-alpha.1", "2.0.0-beta.12", "2.0.0-rc.0"):
            self.assertIsNotNone(r.VERSION.fullmatch(value))

    def test_mismatched_pom_or_chart_fails(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "maven-plugin").mkdir()
            (root / "kubernetes/charts/brewlet").mkdir(parents=True)
            for name in ("release.json", "maven-plugin/pom.xml", "kubernetes/charts/brewlet/Chart.yaml",
                         "kubernetes/charts/brewlet/values.yaml"):
                (root / name).write_bytes((r.ROOT / name).read_bytes())
            for name in ("maven-plugin/pom.xml", "kubernetes/charts/brewlet/Chart.yaml"):
                path = root / name
                original = path.read_text()
                path.write_text(original.replace("2.0.0-alpha.1", "2.0.0-SNAPSHOT"))
                with self.assertRaisesRegex(ValueError, "identity drift"):
                    r.policy(root)
                path.write_text(original)


class PackagingTests(unittest.TestCase):
    def test_archive_bytes_and_metadata_are_deterministic(self):
        with tempfile.TemporaryDirectory() as temp:
            first, second = Path(temp) / "first.tgz", Path(temp) / "second.tgz"
            entries = {"NOTICE.txt": (b"notice", 0o644), "brewlet": (b"binary", 0o755)}
            r.archive(first, entries, 100)
            r.archive(second, dict(reversed(list(entries.items()))), 100)
            self.assertEqual(first.read_bytes(), second.read_bytes())
            self.assertEqual(entries, r.members(first))
            with tarfile.open(first) as archive:
                self.assertEqual(sorted(entries), archive.getnames())
                self.assertTrue(all(x.uid == 0 and x.gid == 0 and x.mtime == 100
                                    and not x.uname and not x.gname for x in archive))

    def test_unsafe_duplicate_and_link_members_fail(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "bad.tar"
            for names, link in ((["../escape"], False), (["/absolute"], False),
                                (["same", "same"], False), (["link"], True)):
                with tarfile.open(path, "w") as archive:
                    for name in names:
                        info = tarfile.TarInfo(name)
                        if link:
                            info.type, info.linkname = tarfile.SYMTYPE, "../escape"
                            archive.addfile(info)
                        else:
                            info.size = 1
                            archive.addfile(info, io.BytesIO(b"x"))
                with self.subTest(names=names, link=link), self.assertRaises(ValueError):
                    r.members(path)

    def test_exact_payload_set_includes_every_distribution(self):
        names = r.expected_files(r.policy())
        self.assertEqual(9, len(names))
        self.assertIn("brewlet-2.0.0-alpha.1.tgz", names)
        self.assertIn("brewlet-maven-plugin-2.0.0-alpha.1-javadoc.jar", names)


class AssuranceTests(unittest.TestCase):
    def test_spdx_primary_subject_has_digest_and_contains_scanned_packages(self):
        raw = {"spdxVersion": "SPDX-2.3", "creationInfo": {}, "packages": [
            {"SPDXID": "SPDXRef-runtime", "name": "actual-runtime"}], "relationships": []}
        value = a.spdx_subject(raw, "fixture", "a" * 64, "2026-01-01T00:00:00Z")
        self.assertEqual("a" * 64, value["packages"][-1]["checksums"][0]["checksumValue"])
        self.assertIn({"spdxElementId": "SPDXRef-BrewletReleaseSubject", "relationshipType": "CONTAINS",
                       "relatedSpdxElement": "SPDXRef-runtime"}, value["relationships"])

    def test_verification_pins_repository_workflow_and_immutable_source(self):
        with patch.object(a, "run") as command:
            a.attest_verify(Path("fixture.json"), "release-build.yml", "a" * 40)
        args = list(map(str, command.call_args.args[0]))
        for flag, expected in (("--repo", "microsoft/brewlet"),
                               ("--signer-workflow", "microsoft/brewlet/.github/workflows/release-build.yml"),
                               ("--source-digest", "a" * 40), ("--signer-digest", "a" * 40)):
            self.assertEqual(expected, args[args.index(flag) + 1])
        self.assertIn("--deny-self-hosted-runners", args)

    def test_synthetic_receipts_cannot_omit_or_relabel_bound_evidence(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "candidate.json").write_text("synthetic fixture")
            document = {"candidate": {"checkout": "a" * 40}}
            value = {"schemaVersion": 1, "candidate": document["candidate"],
                     "candidateSHA256": r.sha((root / "candidate.json").read_bytes()),
                     "scenario": "cli", "platform": "linux/arm64", "outcome": "passed",
                     "assertions": [{"name": name, "outcome": "passed"} for name in
                                    ("native-cli-identity", "native-cli-conformance")]}
            self.assertEqual([], a.evidence_errors(value, root, document, "cli", "linux/arm64"))
            for key, mutation in (("candidate", {}), ("candidateSHA256", "b" * 64),
                                  ("scenario", "transition"), ("platform", "linux/amd64"),
                                  ("outcome", "skipped"), ("assertions", []),
                                  ("interventions", ["manual repair"])):
                bad = {**value, key: mutation}
                with self.subTest(key=key):
                    self.assertTrue(a.evidence_errors(bad, root, document, "cli", "linux/arm64"))

    def test_missing_policy_blocks_before_any_publishing_surface(self):
        config = r.policy()
        with patch.object(a, "validate_inventory", return_value={"candidate": {"checkout": "a" * 40}}), \
                patch.object(a, "verify_sboms"), patch.object(a, "attest_verify"), \
                tempfile.TemporaryDirectory() as temp:
            path = Path(temp)
            (path / "candidate.json").write_text("{}")
            (path / "sboms").mkdir()
            (path / "sboms/index.json").write_text("{}")
            with self.assertRaisesRegex(ValueError, "legal/container policy"):
                a.publication_ready(path)


class PublicationTests(unittest.TestCase):
    def test_exact_existing_tag_manual_context_and_confirmation_are_required(self):
        import os
        config = r.policy()
        environment = {"GITHUB_REPOSITORY": "microsoft/brewlet",
                       "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_SHA": "a" * 40}
        with patch.dict(os.environ, environment), \
                patch.object(p, "publication_ready", return_value={"candidate": {"checkout": "a" * 40}}), \
                patch.object(p, "run", return_value="a" * 40):
            p.preflight(Path("synthetic"), "publish-v" + config["version"])
            with self.assertRaisesRegex(ValueError, "confirmation"):
                p.preflight(Path("synthetic"), "yes")
            with patch.dict(os.environ, {"GITHUB_REPOSITORY": "fork/brewlet"}), self.assertRaises(ValueError):
                p.preflight(Path("synthetic"), "publish-v" + config["version"])
            with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "push"}), self.assertRaises(ValueError):
                p.preflight(Path("synthetic"), "publish-v" + config["version"])
            with patch.object(p, "run", return_value="b" * 40), self.assertRaisesRegex(ValueError, "exact candidate"):
                p.preflight(Path("synthetic"), "publish-v" + config["version"])

    def test_a_conflicting_image_version_is_never_overwritten(self):
        import os
        document = {"images": {name: {"digest": "sha256:" + "a" * 64} for name in r.policy()["images"]}}
        with patch.dict(os.environ, {"GITHUB_ACTOR": "fixture", "GH_TOKEN": "synthetic-not-a-credential"}), \
                patch.object(p.subprocess, "run"), \
                patch.object(p, "registry_digest", return_value="sha256:" + "b" * 64), \
                patch.object(p, "run") as transfer, \
                self.assertRaisesRegex(ValueError, "overwrite different"):
            p.publish_images(Path("synthetic"), document)
        transfer.assert_not_called()

    def test_tool_integrity_failure_does_not_install_or_execute(self):
        with tempfile.TemporaryDirectory() as temp, patch.object(tools.platform, "system", return_value="Linux"), \
                patch.object(tools.platform, "machine", return_value="x86_64"), \
                patch.object(tools.urllib.request, "urlopen", return_value=io.BytesIO(b"tampered-tool")), \
                self.assertRaisesRegex(ValueError, "checksum mismatch"):
            destination = Path(temp) / "tools"
            tools.install("syft", destination)
        self.assertFalse(destination.exists())


if __name__ == "__main__":
    unittest.main()
