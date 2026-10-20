"""Offline shape/provenance guards; not Wasm execution qualification."""

import json
from pathlib import Path
import struct
import tempfile
import tomllib
import unittest

from harness import Failure
import delivery
from wasm import WasmHarness, application_manifest
from wasm_inputs import EXAMPLE, host_elf


def elf(machine=183, interpreter=None):
    data = bytearray(128)
    data[:6] = b"\x7fELF\x02\x01"
    struct.pack_into("<H", data, 18, machine)
    struct.pack_into("<Q", data, 32, 64)
    struct.pack_into("<HH", data, 54, 56, 1)
    struct.pack_into("<I", data, 64, 3 if interpreter else 1)
    if interpreter:
        encoded = interpreter.encode() + b"\0"
        struct.pack_into("<Q", data, 72, 128)
        struct.pack_into("<Q", data, 96, len(encoded))
        data.extend(encoded)
    return bytes(data)


class WasmFixtureTests(unittest.TestCase):
    def test_single_platform_manifest_and_explicit_index_are_both_valid_application_roots(self):
        for indexed in (False, True):
            with tempfile.TemporaryDirectory() as temp:
                layout = Path(temp)
                manifest = {"mediaType": delivery.MANIFEST, "layers": [{"digest": "example"}]}
                descriptor = delivery.json_blob(layout, manifest, delivery.MANIFEST)
                if indexed:
                    descriptor = delivery.json_blob(layout, {"manifests": [descriptor]}, delivery.INDEX)
                delivery.finish_layout(layout, descriptor)
                self.assertEqual(application_manifest(layout), manifest)

    def test_host_architecture_and_real_loader_contract_not_a_static_filename_assumption(self):
        self.assertIsNone(host_elf(elf(), "arm64"))
        path = "/lib/ld-linux-aarch64.so.1"
        self.assertEqual(host_elf(elf(interpreter=path), "arm64"), path)
        for data in (elf(machine=62), elf(interpreter="/unprovided/loader"), b"not ELF"):
            with self.assertRaises(Failure):
                host_elf(data, "arm64")

    def test_release_archives_and_base_are_pinned_for_both_native_architectures(self):
        pins = json.loads((EXAMPLE / "runtimes.json").read_text())
        for arch in ("amd64", "arm64"):
            self.assertRegex(pins["base"][arch], r"@sha256:[0-9a-f]{64}$")
            for name in ("wasmtime", "spin"):
                for revision in ("before", "after"):
                    value = pins[name][revision]
                    source = value[arch]
                    self.assertIn("/releases/download/v" + value["version"] + "/", source["url"])
                    self.assertRegex(source["sha256"], r"^[0-9a-f]{64}$")
                self.assertNotEqual(pins[name]["before"][arch]["sha256"], pins[name]["after"][arch]["sha256"])

    def test_launches_are_explicit_flat_allowlists_with_no_guest_build_or_remote_resolution(self):
        for name, rid in (("wasmtime", "wasmtime-wasip1"), ("spin", "spin-http-sdk7")):
            launch = json.loads((EXAMPLE / (name + "-launch.json")).read_text())
            self.assertEqual(launch["runtimeImages"], [rid])
            self.assertNotIn("requires", launch)
            self.assertEqual(launch["process"]["entrypoint"], ["/usr/local/bin/" + name])
            self.assertNotIn("--build", launch["process"]["args"])
            self.assertIn({"name": "XDG_CACHE_HOME", "value": "/cache/compiled"}, launch["process"]["env"])
            self.assertNotIn("/cache", launch["process"]["args"])
        manifest = tomllib.loads((EXAMPLE / "spin.toml").read_text())
        self.assertNotIn("executor", manifest["trigger"]["http"][0])
        self.assertEqual(manifest["component"]["guest"]["source"], "guest.wasm")
        self.assertEqual(manifest["component"]["guest"]["allowed_outbound_hosts"], [])
        self.assertNotIn("HOST_ONLY", manifest["component"]["guest"]["environment"])

    def test_fixture_uses_existing_runc_class_explicit_platform_and_workload_local_state(self):
        h = object.__new__(WasmHarness)
        h.namespace, h.run_id, h.arch = "owned", "0123456789ab", "arm64"
        h.app_images = {"wasmtime": "example.invalid/app@sha256:" + "a" * 64}
        pod = h.pod("test", "wasmtime")
        self.assertEqual(pod["spec"]["runtimeClassName"], "brewlet")
        self.assertEqual(pod["spec"]["nodeSelector"]["kubernetes.io/arch"], "arm64")
        self.assertEqual(pod["spec"]["volumes"], [
            {"name": "work", "emptyDir": {}}, {"name": "cache", "emptyDir": {}}])
        self.assertTrue(pod["spec"]["containers"][0]["securityContext"]["readOnlyRootFilesystem"])
        self.assertNotIn("hostPath", json.dumps(pod))


if __name__ == "__main__":
    unittest.main()
