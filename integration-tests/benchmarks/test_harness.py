#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline lifecycle/schema tests. All Docker/kind/Kubernetes commands are mocked."""
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import export as exporter
import measure
import resources
import setup
import summarize

HERE = Path(__file__).resolve().parent


class HarnessTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="work-test-", dir=HERE)
        self.root = Path(self.temp.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        for name in ["docker", "kind", "kubectl", "helm", "git"]:
            (self.bin / name).symlink_to(HERE / "mock_cli.py")
        self.mock = self.root / "mock.json"
        self.write_mock({"images": {}, "containers": {}, "calls": []})
        self.env = patch.dict(os.environ, {
            "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
            "BENCHMARK_MOCK_STATE": str(self.mock),
            **{key: "" for key in resources.FIXTURE_OVERRIDES}})
        self.env.start()
        self.work = self.root / "run"
        resources.initialize(self.work)
        self.ledger = resources.Ledger(self.work)
        state = self.read_mock()
        state["run_id"] = self.ledger.state["run_id"]
        self.write_mock(state)

    def tearDown(self):
        self.env.stop()
        self.temp.cleanup()

    def read_mock(self):
        return json.loads(self.mock.read_text())

    def write_mock(self, data):
        self.mock.write_text(json.dumps(data))

    def create_runtime_resources(self):
        self.ledger.create_node(self.bin / "kind", "pinned-node")
        self.ledger.create_registry()
        self.ledger.state["helm_attempted"] = True
        self.ledger.state["profile_attempted"] = True
        self.ledger.save()

    def test_unique_names_and_no_existing_workdir_overwrite(self):
        other = resources.initialize(self.root / "other")
        self.assertNotEqual(other["run_id"], self.ledger.state["run_id"])
        with self.assertRaisesRegex(ValueError, "must be new"):
            resources.initialize(self.work)

    def test_override_rejected_before_side_effects(self):
        for key in resources.FIXTURE_OVERRIDES:
            with self.subTest(key=key), patch.dict(os.environ, {key: "override"}):
                target = self.root / ("rejected-" + key)
                before = len(self.read_mock()["calls"])
                with self.assertRaisesRegex(ValueError, key):
                    resources.initialize(target)
                self.assertFalse(target.exists())
                self.assertEqual(before, len(self.read_mock()["calls"]))

    def test_dirty_production_rejected_but_docs_allowed(self):
        state = self.read_mock()
        state["git_status"] = " M core/go.mod\n M docs/evaluation.md\n M Makefile"
        self.write_mock(state)
        with self.assertRaisesRegex(ValueError, "must be clean"):
            resources.initialize(self.root / "dirty")
        self.assertFalse((self.root / "dirty").exists())
        state["git_status"] = " M docs/evaluation.md\n M Makefile\n?? integration-tests/benchmarks/test_harness.py"
        self.write_mock(state)
        self.assertEqual(resources.preflight(), "actual-source-head")
        self.assertEqual(self.ledger.state["source_commit"], "actual-source-head")

    def test_untracked_production_source_is_rejected(self):
        state = self.read_mock()
        state["git_status"] = "?? core/internal/runtime/untracked.go"
        self.write_mock(state)
        with self.assertRaisesRegex(ValueError, "must be clean"):
            resources.initialize(self.root / "untracked")
        self.assertFalse((self.root / "untracked").exists())
        self.assertTrue(any("--untracked-files=all" in call for call in self.read_mock()["calls"]))

    def test_driver_rejects_override_without_creating_work(self):
        target = self.root / "driver-rejected"
        env = dict(os.environ, JAVA_HOME=str(self.root), PETCLINIC_JAR="different.jar")
        before = len(self.read_mock()["calls"])
        outcome = subprocess.run(["bash", str(HERE / "run.sh"), str(target)], env=env,
                                 text=True, capture_output=True)
        self.assertNotEqual(outcome.returncode, 0)
        self.assertIn("PETCLINIC_JAR", outcome.stderr)
        self.assertFalse(target.exists())
        self.assertEqual(before, len(self.read_mock()["calls"]))

    def test_provenance_reads_actual_fixture_head_and_rejects_head_changes(self):
        state = self.read_mock()
        state["fixture_head"] = "actual-fixture-revision-not-a-hardcoded-label"
        self.write_mock(state)
        observed = setup.source_provenance(self.work, self.ledger)
        self.assertEqual(observed["workload_commit"], state["fixture_head"])
        self.assertEqual(observed["source_commit"], "actual-source-head")
        state["git_head"] = "changed-source-head"
        self.write_mock(state)
        with self.assertRaisesRegex(ValueError, "HEAD changed"):
            setup.source_provenance(self.work, self.ledger)

    def test_preexisting_resources_never_adopted_or_deleted(self):
        names = self.ledger.state["names"]
        ref = names["prefix"] + "/operator:baseline"
        state = self.read_mock()
        foreign = {"id": "foreign-id", "labels": {"brewlet.benchmark": "prior-run"}}
        state["images"][ref] = foreign
        state["containers"][names["node"]] = foreign
        state["containers"][names["registry"]] = foreign
        self.write_mock(state)
        with self.assertRaisesRegex(ValueError, "preexisting image"):
            self.ledger.build(ref, ["."])
        with self.assertRaisesRegex(ValueError, "preexisting benchmark node"):
            self.ledger.create_node(self.bin / "kind", "node")
        with self.assertRaisesRegex(ValueError, "preexisting registry"):
            self.ledger.create_registry()
        result = self.ledger.cleanup()
        self.assertFalse(result["failures"])
        self.assertEqual(self.read_mock()["images"][ref], foreign)
        self.assertEqual(self.read_mock()["containers"][names["node"]], foreign)
        self.assertEqual(self.read_mock()["containers"][names["registry"]], foreign)
        self.assertFalse(self.ledger.state["images"])
        self.assertFalse(self.ledger.state["containers"])

    def test_exact_images_failure_reported_remaining_cleanup_attempted(self):
        self.create_runtime_resources()
        prefix = self.ledger.state["names"]["prefix"]
        good, bad = prefix + "/good:one", prefix + "/bad:one"
        self.ledger.build(good, ["."])
        self.ledger.build(bad, ["."])
        state = self.read_mock()
        old = "localhost/phase10-78c1a5d3/operator:retained"
        state["images"][old] = {"id": "old-id", "labels": {"brewlet.benchmark": "78c1a5d3"}}
        state["images"]["eclipse-temurin:21"] = {"id": "shared-base", "labels": {}}
        state["fail_remove"] = [bad]
        state["fail_kind_delete"] = True
        self.write_mock(state)
        outcome = subprocess.run([sys.executable, str(HERE / "resources.py"), "--work",
                                  str(self.work), "cleanup"], capture_output=True, text=True)
        self.assertNotEqual(outcome.returncode, 0)
        result = json.loads((self.work / "cleanup.json").read_text())
        self.assertTrue(result["failures"])
        self.assertTrue(result["registry_removed"])
        self.assertFalse(result["node_removed"])
        self.assertEqual(result["remaining_owned_image_references"], [bad])
        state = self.read_mock()
        self.assertNotIn(good, state["images"])
        self.assertIn(old, state["images"])
        self.assertIn("eclipse-temurin:21", state["images"])
        self.assertFalse(any("prune" in call or "ls" in call for call in state["calls"]))
        self.assertTrue(any(call[:4] == ["docker", "rm", "-f", "-v"] for call in state["calls"]))

    def test_replaced_owned_image_and_node_are_preserved(self):
        self.create_runtime_resources()
        ref = self.ledger.state["names"]["prefix"] + "/operator:one"
        self.ledger.build(ref, ["."])
        state = self.read_mock()
        state["images"][ref]["id"] = "foreign-replacement"
        state["containers"][self.ledger.state["names"]["node"]]["id"] = "foreign-node"
        self.write_mock(state)
        result = self.ledger.cleanup()
        self.assertTrue(result["failures"])
        self.assertIn(ref, self.read_mock()["images"])
        self.assertIn(self.ledger.state["names"]["node"], self.read_mock()["containers"])
        self.assertTrue(result["registry_removed"])
        self.assertFalse(any(call[0] in ["kind", "kubectl", "helm"] and
                             ("delete" in call or "uninstall" in call) for call in self.read_mock()["calls"]))

    def test_invocation_tag_reuse_and_foreign_alias_refusal(self):
        source = "source@sha256:base"
        state = self.read_mock()
        state["images"][source] = {"id": "base-id", "labels": {}}
        self.write_mock(state)
        ref = self.ledger.state["names"]["prefix"] + "/jdk:old"
        self.ledger.tag(source, ref)
        self.ledger.tag(source, ref)
        self.assertEqual(len(self.ledger.state["images"]), 1)
        self.ledger.cleanup()
        self.assertIn(source, self.read_mock()["images"])

    def test_disappeared_node_is_not_proof_of_normal_cleanup(self):
        self.create_runtime_resources()
        state = self.read_mock()
        del state["containers"][self.ledger.state["names"]["node"]]
        self.write_mock(state)
        result = self.ledger.cleanup()
        self.assertTrue(result["node_removed"])
        self.assertTrue(result["registry_removed"])
        self.assertTrue(result["failures"])
        self.assertNotEqual(result["exit_code"], 0)
        self.assertIsNone(result["profile_finalization_succeeded"])

    def test_fresh_raw_cleanup_export_analyze_with_retained_measurements(self):
        self.create_runtime_resources()
        historic_path = HERE / "results/2026-09-09-linux-arm64.json"
        original_bytes = historic_path.read_bytes()
        historic = json.loads(original_bytes)
        runtime = copy.deepcopy(historic["runtime"])
        contract = runtime.pop("verified_launch_contract")
        detailed = runtime.pop("control_plane_idle_detailed")
        runtime.pop("control_plane_idle", None)
        for trial in runtime["trials"] + runtime["cohorts"]:
            for container in trial["memory"]["containers"]:
                container["cmdline"] = contract["command"]
                container["jdk_release"] = contract["jdk_release"]
                container["runtime_environment"] = runtime["verified_runtime_environment"]
                container["cds_mappings"] = ["retained recorded mapping"] if container.pop("default_cds_mapped") else []
        pod_listing = {"items": [{"status": {"containerStatuses": [
            {"containerID": "containerd://" + c["container_id"], "state": {"running": {}}}
            for c in detailed["containers"]]}}]}
        outputs = [subprocess.CompletedProcess([], 0, json.dumps(pod_listing), ""),
                   subprocess.CompletedProcess([], 0, json.dumps(detailed), "")]
        with patch.object(measure, "run", side_effect=outputs) as collector:
            runtime["control_plane_idle_detailed"] = measure.collect_idle(
                self.work, "mock-node", ["kubectl", "--kubeconfig", "mock"])
            self.assertIn("--descendants", collector.call_args.args[0])
        (self.work / "runtime-raw.json").write_text(json.dumps(runtime))
        (self.work / "storage-raw.json").write_text(json.dumps(historic["storage"]))
        (self.work / "matched").mkdir()
        (self.work / "matched/launch.json").write_text(json.dumps({
            "command": contract["command"], "files": contract["payload_files"]}))
        cleanup = self.ledger.cleanup()
        self.assertEqual(cleanup["failures"], [])
        exporter.export_results(self.work, self.work / "results.json")
        exported = json.loads((self.work / "results.json").read_text())
        self.assertNotIn(str(Path.home()), (self.work / "results.json").read_text())
        analysis = summarize.analyze(exported)
        self.assertTrue(analysis["cleanup_verified"])
        self.assertEqual(analysis["startup_seconds"]["conventional"]["spring_startup_seconds"]["median"], 6.606)
        cli = subprocess.run([sys.executable, str(HERE / "summarize.py"), str(self.work / "results.json")],
                             text=True, capture_output=True)
        self.assertEqual(cli.returncode, 0, cli.stderr)
        self.assertEqual(historic_path.read_bytes(), original_bytes)
        runtime.pop("control_plane_idle_detailed")
        (self.work / "runtime-raw.json").write_text(json.dumps(runtime))
        exporter.export_results(self.work, self.work / "incomplete.json")
        missing = json.loads((self.work / "incomplete.json").read_text())
        self.assertIsNone(missing["runtime"]["control_plane_idle_detailed"])
        self.assertTrue(missing["runtime"]["failures"])


if __name__ == "__main__":
    unittest.main()
