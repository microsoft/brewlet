# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

from admission import Admission
from checkout import CheckoutFixture
from common import OWNER_LABEL, ROOT
from hpa import Scaling


class CheckoutTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        environment = patch.dict(os.environ, {"BREWLET_LIVE_OUTPUT": str(self.root)})
        environment.start()
        self.addCleanup(environment.stop)

    def fixture(self, scenario="hpa"):
        fixture = CheckoutFixture(scenario)
        self.addCleanup(shutil.rmtree, fixture.private)
        return fixture

    def test_every_scenario_uses_checkout_source(self):
        for scenario in ("smoke", "hpa", "admission", "workflows"):
            fixture = self.fixture(scenario)
            self.assertEqual(fixture.scenario, scenario)
            self.assertEqual(fixture.source, ROOT)
            self.assertIn(fixture.remove_checkout_images, fixture.cleanups)

    def test_builds_all_components_and_records_checkout_provenance(self):
        fixture = self.fixture()
        fixture.arch = "amd64"
        source = self.root / "source"
        (source / "maven-plugin").mkdir(parents=True)
        (source / "maven-plugin/pom.xml").write_text(
            '<project xmlns="http://maven.apache.org/POM/4.0.0"><version>9.9.9-test</version></project>')
        (source / "kubernetes/charts/brewlet").mkdir(parents=True)
        (source / "kubernetes/charts/brewlet/Chart.yaml").write_text("version: 9.9.9")

        def execute(argv, **_kwargs):
            if argv[:2] == ["git", "rev-parse"]:
                output = "checkout-revision"
            elif argv[:2] == ["git", "status"]:
                output = ""
            elif argv[:3] == ["docker", "image", "inspect"]:
                output = json.dumps([{"Id": argv[3]}])
            else:
                output = "tool-version"
            return subprocess.CompletedProcess(argv, 0, output, "")

        def build(name, argv, **_kwargs):
            if name == "build-cli":
                fixture.cli.write_bytes(b"checkout-cli")
            elif name == "build-plugin":
                for directory in (fixture.private / "maven-plugin/target",
                                  fixture.private / "m2/sh/brewlet/brewlet-maven-plugin/9.9.9-test"):
                    directory.mkdir(parents=True)
                    (directory / "brewlet-maven-plugin-9.9.9-test.jar").write_bytes(b"checkout-plugin")

        with patch("checkout.ROOT", source), patch.object(fixture, "run", side_effect=execute), \
                patch.object(fixture, "build_command", side_effect=build) as commands:
            fixture.build()
        self.assertEqual(fixture.plugin, "sh.brewlet:brewlet-maven-plugin:9.9.9-test")
        self.assertEqual(set(fixture.built), {"operator", "admission", "provisioner"})
        self.assertEqual([c.args[0] for c in commands.call_args_list], [
            "build-cli", "build-plugin", "build-operator-image", "build-admission-image",
            "build-provisioner-image"])
        for call in commands.call_args_list[2:]:
            self.assertEqual(call.args[1][-1], source)
        versions = json.loads((fixture.work / "versions.json").read_text())
        self.assertEqual(versions["source"], "checkout-revision")
        self.assertFalse(versions["sourceDirty"])
        self.assertEqual(versions["runtime"], "checkout")
        self.assertIn("Chart.yaml", versions["chartHashes"])

    def test_pins_loaded_images_and_records_digests(self):
        fixture = self.fixture()
        fixture.node_id = "owned-node"
        fixture.built = {"operator": "brewlet.local/operator:candidate"}
        fixture.save("versions.json", {"source": "checkout-revision"})
        digest = "sha256:" + "a" * 64
        result = subprocess.CompletedProcess([], 0, f"brewlet.local/operator:candidate type {digest}", "")
        with patch.object(fixture, "run", return_value=result) as execute, \
                patch.object(fixture, "load_image") as load:
            images = fixture.component_images()
        self.assertEqual(images, {"operator": "brewlet.local/operator@" + digest})
        load.assert_called_once_with("brewlet.local/operator:candidate")
        self.assertIn("--force", execute.call_args.args[0])
        self.assertEqual(json.loads((fixture.work / "versions.json").read_text())["components"], images)

    def test_cleanup_refuses_foreign_images(self):
        fixture = self.fixture()
        fixture.image_ids = ["foreign"]
        result = subprocess.CompletedProcess([], 0,
                    json.dumps([{"Config": {"Labels": {OWNER_LABEL: "another-run"}}}]), "")
        with patch.object(fixture, "run", return_value=result) as execute:
            with self.assertRaisesRegex(RuntimeError, "foreign image"):
                fixture.remove_checkout_images()
        execute.assert_called_once_with(["docker", "image", "inspect", "foreign"], check=False)

    def test_hpa_always_checks_retained_bytes(self):
        digest, layer = "sha256:" + "a" * 64, "sha256:" + "b" * 64
        fixture = Mock()
        fixture.node_blob_present.return_value = False
        fixture.run.return_value.stdout = layer[7:] + " retained-file"
        with patch("hpa.runnable_layers", return_value=(digest, [layer])):
            Scaling(fixture).observe_source_gc("image")
            fixture.run.assert_called_once()
            self.assertTrue(fixture.record.call_args.args[1]["retainedBytesVerified"])
            fixture.record.reset_mock()
            fixture.run.return_value.stdout = "c" * 64 + " corrupted-file"
            with self.assertRaisesRegex(AssertionError, "Retained layer"):
                Scaling(fixture).observe_source_gc("image")
            fixture.record.assert_not_called()

    def test_admission_does_not_hide_shipped_manifest_regressions(self):
        for spec_type in (None, "unsupported-regression"):
            fixture = self.fixture("admission")
            fixture.source_revision = "selected-source"
            fixture.registry = "localhost:5000"
            admission = Admission(fixture)
            admission.public = fixture.private / "public.pem"
            admission.public.write_text("PUBLIC")

            def manifest(name):
                spec = {"source": {}, "parameters": {}}
                if name == "20-ratify-verifier.yaml" and spec_type is not None:
                    spec["type"] = spec_type
                return {"name": name, "spec": spec}

            with patch.object(admission, "manifest", side_effect=manifest), \
                    patch.object(fixture, "apply") as apply, patch("admission.wait"):
                admission.policies()
            verifier = apply.call_args_list[1].args[0]
            self.assertNotIn("source", verifier["spec"])
            self.assertEqual(verifier["spec"].get("type"), spec_type)
            evidence = fixture.evidence[-1]["details"]
            self.assertEqual(evidence["source"], "selected-source")
            self.assertFalse(any("correction" in s for s in evidence["substitutions"]))


if __name__ == "__main__":
    unittest.main()
