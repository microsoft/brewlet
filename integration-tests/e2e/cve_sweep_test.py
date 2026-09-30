#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline checks for cve_sweep.py; no cluster or brewlet build required."""

import contextlib
import hashlib
import io
import json
import os
import stat
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import cve_sweep  # noqa: E402

GROUP, ARTIFACT = "org.apache.logging.log4j", "log4j-core"


class Store:
    def __init__(self, root):
        self.root = root
        self.index = []
        os.makedirs(os.path.join(root, "blobs", "sha256"))

    def blob(self, document):
        raw = document if isinstance(document, bytes) else json.dumps(document).encode()
        digest = "sha256:" + hashlib.sha256(raw).hexdigest()
        with open(os.path.join(self.root, "blobs", "sha256", digest[7:]), "wb") as stream:
            stream.write(raw)
        return digest

    def bundle(self, name, version):
        bundle = self.blob({"bundle": name, "version": version})
        sbom = self.blob({"bomFormat": "CycloneDX", "components": [{
            "group": GROUP, "name": ARTIFACT, "version": version,
            "purl": f"pkg:maven/{GROUP}/{ARTIFACT}@{version}"}]})
        referrer = self.blob({"subject": {"digest": bundle}, "layers": [{"digest": sbom}]})
        self.index.append({"digest": bundle, "annotations": {cve_sweep.REF_NAME: name}})
        self.index.append({"digest": referrer, "artifactType": cve_sweep.CYCLONEDX,
                           "annotations": {cve_sweep.REFERRER_SUBJECT: bundle}})
        return bundle, sbom

    def image(self, ref, bundle, sbom):
        digest = self.blob({"image": ref})
        self.index.append({"digest": digest, "annotations": {cve_sweep.REF_NAME: ref}})
        return digest, {"schemaVersion": 1, "thinJar": True,
                        "applicationJarDigest": "sha256:app",
                        "dependencyBundleDigest": bundle, "sbomDigest": sbom,
                        "sourceBom": "com.example:bom:1"}

    def save(self):
        with open(os.path.join(self.root, "index.json"), "w", encoding="utf-8") as stream:
            json.dump({"manifests": self.index}, stream)


def pod(name, namespace, image, owner=None, runtime="brewlet", phase="Running"):
    metadata = {"name": name, "namespace": namespace}
    if owner:
        metadata["labels"] = {"pod-template-hash": "abc12"}
        metadata["ownerReferences"] = [{"kind": "ReplicaSet", "name": f"{owner}-abc12"}]
    return {"metadata": metadata,
            "spec": {"runtimeClassName": runtime,
                     "containers": [{"name": "app", "image": image}]},
            "status": {"phase": phase}}


class VersionTest(unittest.TestCase):
    def test_log4shell_range(self):
        affected = lambda v: cve_sweep.is_affected(v, "2.0-beta9", "2.17.1")
        for version in ("2.0-beta9", "2.0", "2.14.1", "2.15.0", "2.17.0"):
            self.assertTrue(affected(version), version)
        for version in ("1.2.17", "2.0-beta8", "2.17.1", "2.24.3"):
            self.assertFalse(affected(version), version)


class SweepTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = self.tmp.name
        self.store = Store(os.path.join(root, "oci"))
        bad_bundle, bad_sbom = self.store.bundle("platform/logging:1", "2.14.1")
        good_bundle, good_sbom = self.store.bundle("platform/logging:2", "2.17.1")
        self.vulnerable, bad_evidence = self.store.image("apps/orders:1", bad_bundle, bad_sbom)
        self.patched, good_evidence = self.store.image("apps/orders:2", good_bundle, good_sbom)
        self.plain = self.store.image("apps/plain:1", "", "")[0]
        self.store.save()
        evidence = {"apps/orders:1": bad_evidence, "apps/orders:2": good_evidence}
        self.brewlet = os.path.join(root, "brewlet")
        with open(self.brewlet, "w", encoding="utf-8") as stream:
            stream.write("#!/usr/bin/env python3\nimport json, sys\n"
                         f"evidence = {json.dumps(evidence)!r}\n"
                         "print('== kind ==\\nrunnable OCI image\\n')\n"
                         "ref = sys.argv[2]\n"
                         "if ref in json.loads(evidence):\n"
                         f"    print({cve_sweep.EVIDENCE_HEADER!r})\n"
                         "    print(json.dumps(json.loads(evidence)[ref], indent=2))\n")
        os.chmod(self.brewlet, os.stat(self.brewlet).st_mode | stat.S_IEXEC)
        self.pods = os.path.join(root, "pods.json")

    def tearDown(self):
        self.tmp.cleanup()

    def run_sweep(self, pods, *extra):
        with open(self.pods, "w", encoding="utf-8") as stream:
            json.dump({"items": pods}, stream)
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = cve_sweep.main([
                "sweep", "--store", self.store.root, "--brewlet", self.brewlet,
                "--pods", self.pods, "--component", f"{GROUP}:{ARTIFACT}",
                "--introduced", "2.0-beta9", "--fixed", "2.17.1", *extra])
        return code, json.loads(out.getvalue())

    def test_identifies_only_vulnerable_brewlet_workloads_in_scope(self):
        code, report = self.run_sweep([
            pod("orders-abc12-x", "prod", f"reg/apps/orders@{self.vulnerable}", owner="orders"),
            pod("orders-abc12-y", "prod", f"reg/apps/orders@{self.vulnerable}", owner="orders"),
            pod("billing-abc12-x", "prod", f"reg/apps/billing@{self.patched}", owner="billing"),
            pod("orders-abc12-z", "staging", f"reg/apps/orders@{self.vulnerable}", owner="orders"),
            pod("web", "prod", f"reg/apps/orders@{self.vulnerable}", runtime="runc"),
            pod("done", "prod", f"reg/apps/orders@{self.vulnerable}", phase="Succeeded"),
        ], "--namespace", "prod", "--fail-on-findings")
        self.assertEqual(code, 3)
        self.assertEqual(report["vulnerable"], ["prod/orders"])
        self.assertEqual(report["brewletPods"], 3)
        orders = next(w for w in report["workloads"] if w["name"] == "orders")
        self.assertEqual(orders["kind"], "Deployment")
        self.assertEqual(orders["pods"], ["orders-abc12-x", "orders-abc12-y"])
        self.assertEqual(orders["images"][0]["findings"][0]["version"], "2.14.1")
        self.assertEqual(report["unscannable"], [])

    def test_clean_fleet_passes(self):
        code, report = self.run_sweep(
            [pod("orders-abc12-x", "prod", f"reg/apps/orders@{self.patched}", owner="orders")],
            "--fail-on-findings")
        self.assertEqual(code, 0)
        self.assertEqual(report["vulnerable"], [])
        self.assertEqual(report["workloads"][0]["images"][0]["componentVersions"], ["2.17.1"])

    def test_unverifiable_images_fail_closed(self):
        code, report = self.run_sweep([
            pod("tagged", "prod", "reg/apps/orders:1"),
            pod("unknown", "prod", "reg/apps/x@sha256:" + "0" * 64),
            pod("plain", "prod", f"reg/apps/plain@{self.plain}"),
        ], "--fail-on-findings")
        self.assertEqual(code, 3)
        reasons = sorted(entry["reason"] for entry in report["unscannable"])
        self.assertIn("image is not digest-pinned", reasons)
        self.assertTrue(any("not in the artifact store" in r for r in reasons))
        self.assertTrue(any("no managed dependency evidence" in r for r in reasons))

    def test_conflicting_sbom_referrers_fail_closed(self):
        bundle = self.store.index[0]["digest"]
        extra = self.store.blob({"subject": {"digest": bundle}, "layers": [{"digest": "x"}]})
        self.store.index.append({"digest": extra, "artifactType": cve_sweep.CYCLONEDX,
                                 "annotations": {cve_sweep.REFERRER_SUBJECT: bundle}})
        self.store.save()
        _, report = self.run_sweep(
            [pod("orders", "prod", f"reg/apps/orders@{self.vulnerable}")])
        self.assertIn("2 CycloneDX referrers", report["unscannable"][0]["reason"])


if __name__ == "__main__":
    unittest.main()
