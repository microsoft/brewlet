import hashlib
from pathlib import Path
import unittest

from harness import Failure
import managed_wasm
from wasm import guest


class ManagedWasmTests(unittest.TestCase):
    def test_specification_lists_wasm_without_language_detection_or_runtime_host_mislabeling(self):
        proposal = (managed_wasm.EXAMPLE.parents[1] / "specs/SPECIFICATION.md").read_text()
        for name in ("Java", "Python", "Node.js", ".NET", "WebAssembly"):
            self.assertIn("| " + name + " |", proposal)
        self.assertIn("Linux host", proposal)
        self.assertIn("Guest compilation", proposal)
        self.assertIn("13.2 Language-neutral workload acceptance", proposal)
    def test_asset_lock_has_complete_exact_identity_and_no_linked_library_claim(self):
        before = managed_wasm.dependency_lock(b"original\n", "1")
        after = managed_wasm.dependency_lock(b"updated\n", "2")
        self.assertEqual(before["entries"][0]["path"], "/app/dependencies/message.txt")
        self.assertEqual(before["entries"][0]["sha256"], hashlib.sha256(b"original\n").hexdigest())
        self.assertEqual(after["entries"][0]["purl"], "pkg:generic/brewlet/wasm-message@2")
        self.assertNotEqual(before["entries"][0]["sha256"], after["entries"][0]["sha256"])
        self.assertEqual(before["entries"][0]["ecosystem"], "generic")

    def test_guest_checks_accept_only_expected_external_update_and_preserve_boundaries(self):
        report = {"message": "Hello from Wasm", "dependency": managed_wasm.MESSAGES["after"] + "\n",
                  "visible": "explicit", "hostOnly": "", "runtimeFileHidden": True,
                  "engineCacheHidden": True, "dependencyWriteDenied": True}
        guest(report, managed_wasm.MESSAGES["after"])
        with self.assertRaises(Failure):
            guest(report)
        for name in ("runtimeFileHidden", "engineCacheHidden", "dependencyWriteDenied"):
            with self.subTest(field=name), self.assertRaises(Failure):
                guest({**report, name: False}, managed_wasm.MESSAGES["after"])

    def test_both_hosts_keep_explicit_runtime_class_launch_and_separate_caches(self):
        h = object.__new__(managed_wasm.ManagedWasmHarness)
        h.namespace, h.run_id, h.arch = "owned", "0123456789ab", "arm64"
        h.app_images = {engine: "example.invalid/" + engine + "@sha256:" + "a" * 64
                        for engine in managed_wasm.IDS}
        h.delivery = type("Delivery", (), {"secret": "owned-registry"})()
        for engine in managed_wasm.IDS:
            pod = h.pod("test", engine)
            self.assertEqual(pod["spec"]["runtimeClassName"], "brewlet")
            self.assertEqual(pod["spec"]["imagePullSecrets"], [{"name": "owned-registry"}])
            self.assertEqual(pod["spec"]["containers"][0]["imagePullPolicy"], "Always")
            self.assertEqual([v["name"] for v in pod["spec"]["volumes"]], ["work", "cache"])
            self.assertEqual(pod["metadata"]["annotations"]["brewlet.sh/runtime-image"],
                             managed_wasm.IDS[engine])

    def test_qualification_checks_real_initial_guest_hashes_owner_export_and_cleanup(self):
        source = Path(managed_wasm.__file__).read_text()
        for value in ('"sha256sum"', '["wasmGuestInputs"]', 'inspect_wasm(',
                      'h.delivery.publish_revision("after")', '"ManagedPathConflict"',
                      '"COM', 'managed.inventory_snapshot', 'h.delete("nodeprofile"',
                      '"inventoryUID", "generation", "sourceDigest"'):
            self.assertIn(value, source)
        self.assertIn('if engine == "spin":', source)
        self.assertNotIn("runwasi", source)
        self.assertNotIn("trivy", source)
        self.assertNotIn("grype", source)
