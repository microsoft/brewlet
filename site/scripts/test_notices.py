"""Offline regression checks for reproducible browser dependency notices."""

import importlib.util
import io
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location("site_notices", Path(__file__).with_name("generate-notice.py"))
NOTICES = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(NOTICES)


class NoticeTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.inputs = self.root / "site/notices"
        self.inputs.mkdir(parents=True)
        (self.root / "site/requirements.txt").write_text("mkdocs-material==9.8.0\n")
        license = self.root / "material.dist-info/licenses/LICENSE"
        license.parent.mkdir(parents=True)
        license.write_text("Material license\n")
        material = SimpleNamespace(version="9.8.0", files=[license.relative_to(self.root)],
                                   locate_file=lambda path: self.root / path)
        mock = patch.object(NOTICES.importlib.metadata, "distribution", return_value=material)
        mock.start()
        self.addCleanup(mock.stop)
        self.dependencies = {"fixture": "1.2.3"}
        self.packages = {"node_modules/fixture": {"version": "1.2.3"}}
        self.add_package("node_modules/fixture", "fixture", "1.2.3")
        self.write_manifests()

    def add_package(self, location, name, version):
        directory = self.inputs / location
        directory.mkdir(parents=True)
        (directory / "package.json").write_text(json.dumps({
            "name": name, "version": version, "repository": {"url": "https://example.invalid/source"},
        }))
        (directory / "LICENSE").write_text(f"license for {name}\n")
        (directory / "NOTICE").write_text(f"attribution for {name}\n")
        return directory

    def write_manifests(self):
        (self.inputs / "package.json").write_text(json.dumps({"dependencies": self.dependencies}))
        (self.inputs / "package-lock.json").write_text(json.dumps({
            "packages": {"": {"dependencies": self.dependencies}, **self.packages},
        }))

    def test_generation_is_deterministic_and_includes_locked_transitive_packages(self):
        location = "node_modules/fixture/node_modules/@scope/transitive"
        self.packages[location] = {"version": "4.5.6"}
        self.add_package(location, "@scope/transitive", "4.5.6")
        self.write_manifests()
        generated = NOTICES.generate(self.root)
        self.assertEqual(generated, NOTICES.generate(self.root))
        for text in ("Material for MkDocs 9.8.0", "fixture 1.2.3", "@scope/transitive 4.5.6",
                     "license for @scope/transitive", "attribution for fixture"):
            self.assertIn(text, generated)
        self.assertNotIn(str(self.root), generated)

    def test_missing_or_empty_license_fails_even_with_attribution(self):
        license = self.inputs / "node_modules/fixture/LICENSE"
        license.unlink()
        with self.assertRaisesRegex(ValueError, "no license text"):
            NOTICES.generate(self.root)
        license.write_text(" \n")
        with self.assertRaisesRegex(ValueError, "empty attribution"):
            NOTICES.generate(self.root)

    def test_missing_transitive_package_fails(self):
        self.packages["node_modules/missing"] = {"version": "1.0.0"}
        self.write_manifests()
        with self.assertRaises(FileNotFoundError):
            NOTICES.generate(self.root)

    def test_manifest_lock_drift_fails(self):
        (self.inputs / "package.json").write_text(json.dumps({"dependencies": {"fixture": "9.0.0"}}))
        with self.assertRaisesRegex(ValueError, "differs from package.json"):
            NOTICES.generate(self.root)

    def test_direct_dependencies_must_have_exact_locked_records(self):
        for packages in ({}, {"node_modules/fixture": {"version": "9.0.0"}}):
            self.packages = packages
            self.write_manifests()
            with self.subTest(packages=packages):
                with self.assertRaisesRegex(ValueError, "missing the exact pinned dependency"):
                    NOTICES.generate(self.root)

    def test_installed_name_and_version_must_match_lock(self):
        metadata = self.inputs / "node_modules/fixture/package.json"
        for name, version in (("fixture", "9.0.0"), ("wrong-package", "1.2.3")):
            metadata.write_text(json.dumps({"name": name, "version": version}))
            with self.subTest(name=name, version=version):
                with self.assertRaisesRegex(ValueError, "differs from lock"):
                    NOTICES.generate(self.root)

    def test_material_version_must_match_pages_requirements(self):
        (self.root / "site/requirements.txt").write_text("mkdocs-material==9.0.0\n")
        with self.assertRaisesRegex(ValueError, "differs from site/requirements.txt"):
            NOTICES.generate(self.root)

    def test_material_upgrade_requires_browser_inventory_review(self):
        (self.root / "site/requirements.txt").write_text("mkdocs-material==9.9.0\n")
        material = NOTICES.importlib.metadata.distribution.return_value
        material.version = "9.9.0"
        with self.assertRaisesRegex(ValueError, "review the browser dependency inventory"):
            NOTICES.generate(self.root)

    def test_known_version_update_requires_attribution_update(self):
        location = "node_modules/focus-visible"
        self.packages[location] = {"version": "9.0.0"}
        self.add_package(location, "focus-visible", "9.0.0")
        self.write_manifests()
        with self.assertRaisesRegex(ValueError, "update source URLs and attribution"):
            NOTICES.generate(self.root)

    def test_checked_in_fallback_license_and_supplemental_notice_are_preserved(self):
        licenses = self.inputs / "licenses"
        licenses.mkdir()
        (licenses / "zenorocha-mit.txt").write_text("Zeno Rocha MIT license\n")
        (licenses / "w3c-software-document-2015.txt").write_text("W3C supplemental license\n")
        delegate = self.add_package("node_modules/delegate", "delegate", "3.2.0")
        (delegate / "LICENSE").unlink()
        self.packages["node_modules/delegate"] = {"version": "3.2.0"}
        self.add_package("node_modules/focus-visible", "focus-visible", "5.2.1")
        self.packages["node_modules/focus-visible"] = {"version": "5.2.1"}
        self.write_manifests()
        generated = NOTICES.generate(self.root)
        self.assertIn("Zeno Rocha MIT license", generated)
        self.assertIn("attribution for delegate", generated)
        self.assertIn("W3C supplemental license", generated)
        self.assertIn("Modification statement: Brewlet does not modify", generated)

    def test_check_is_read_only_and_update_repairs_stale_snapshot(self):
        output = self.root / "site/NOTICE.txt"
        output.write_text("stale snapshot\n")
        with patch.object(NOTICES, "ROOT", self.root), patch("sys.stderr", new_callable=io.StringIO):
            with self.assertRaises(SystemExit) as error:
                NOTICES.main(["--check"])
            self.assertEqual(error.exception.code, 1)
            self.assertEqual(output.read_text(), "stale snapshot\n")
            self.assertEqual(NOTICES.main([]), 0)
            self.assertEqual(NOTICES.main(["--check"]), 0)
            output.unlink()
            with self.assertRaises(SystemExit):
                NOTICES.main(["--check"])
            self.assertFalse(output.exists())


if __name__ == "__main__":
    unittest.main()
