"""Pinned release inputs for explicit Wasm examples; never executes at import."""

import hashlib
import io
import json
from pathlib import Path
import shutil
import struct
import tarfile
import tomllib
import urllib.request

import delivery
from harness import ROOT, require

EXAMPLE = ROOT / "examples/wasm"


def host_elf(data, arch):
    require(len(data) >= 64 and data[:6] == b"\x7fELF\x02\x01", "64-bit little-endian ELF required")
    require(struct.unpack_from("<H", data, 18)[0] == {"amd64": 62, "arm64": 183}[arch],
            "release binary architecture differs from the native host")
    offset = struct.unpack_from("<Q", data, 32)[0]
    size, count = struct.unpack_from("<HH", data, 54)
    require(size == 56 and count > 0 and offset + count * size <= len(data), "invalid ELF program headers")
    for i in range(count):
        header = offset + i * size
        if struct.unpack_from("<I", data, header)[0] == 3:
            start = struct.unpack_from("<Q", data, header + 8)[0]
            length = struct.unpack_from("<Q", data, header + 32)[0]
            require(length > 1 and start + length <= len(data), "invalid ELF interpreter range")
            raw = data[start:start + length]
            require(raw[-1:] == b"\0" and b"\0" not in raw[:-1], "invalid ELF interpreter string")
            interpreter = raw[:-1].decode()
            expected = {"amd64": "/lib64/ld-linux-x86-64.so.2", "arm64": "/lib/ld-linux-aarch64.so.1"}[arch]
            require(interpreter == expected, "host requires a loader outside the pinned distroless base contract")
            return interpreter
    return None


def release_binary(pin, name, arch):
    require(pin["url"].startswith(("https://github.com/bytecodealliance/wasmtime/releases/download/",
                                    "https://github.com/spinframework/spin/releases/download/")),
            "unexpected release source")
    with urllib.request.urlopen(pin["url"], timeout=120) as response:
        data = response.read(200 * 1024 * 1024 + 1)
    require(len(data) <= 200 * 1024 * 1024, "release archive exceeds fixture limit")
    require(hashlib.sha256(data).hexdigest() == pin["sha256"], "release archive checksum mismatch")
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:*") as archive:
        candidates = [member for member in archive if member.isfile()
                      and Path(member.name).name == name]
        require(len(candidates) == 1 and candidates[0].size <= 300 * 1024 * 1024,
                "release must contain exactly one bounded host executable")
        binary = archive.extractfile(candidates[0]).read()
    host_elf(binary, arch)
    return binary


def runtime_layout(layout, name, arch, revision, binary, base_layout, base):
    shutil.copytree(base_layout, layout)
    manifest = json.loads(delivery.read_blob(base_layout, base["descriptor"]))
    entries = [(path, b"", 0o755) for path in ("usr/", "usr/local/", "usr/local/bin/", "etc/")]
    entries += [("tmp/", b"", 0o1777), ("usr/local/bin/" + name, binary, 0o755),
                ("etc/runtime-sentinel", b"host-only-runtime-data", 0o644)]
    data, diff = delivery.tar_layer(entries)
    layer = delivery.blob(layout, data, delivery.LAYER)
    config = delivery.json_blob(layout, {"architecture": arch, "os": "linux",
        "config": {"Env": ["WASM_RUNTIME_REVISION=" + revision], "User": "0:0",
                   "Entrypoint": ["/ignored-runtime-entrypoint"]},
        "rootfs": {"type": "layers", "diff_ids": base["config"]["rootfs"]["diff_ids"] + [diff]}}, delivery.CONFIG)
    target = delivery.json_blob(layout, {"schemaVersion": 2, "mediaType": delivery.MANIFEST,
        "config": config, "layers": manifest["layers"] + [layer]}, delivery.MANIFEST)
    return delivery.finish_layout(layout, target)


def build(h, revisions=("before", "after")):
    require(bool(revisions) and len(set(revisions)) == len(revisions)
            and set(revisions) <= {"before", "after"}, "explicit unique Wasm host revisions required")
    pins = json.loads((EXAMPLE / "runtimes.json").read_text())
    base_layout = h.work / "runtime-base"
    importer = h.work / "runtimeimage"
    h.run(["go", "build", "-mod=readonly", "-o", importer, "./integration-tests/live-v2/runtimeimage"], timeout=180)
    base = json.loads(h.run([importer, "--source", pins["base"][h.arch], "--output", base_layout],
                            timeout=600).stdout)
    require(base["config"]["architecture"] == h.arch, "base userland architecture mismatch")
    h.run(["go", "build", "-mod=readonly", "-trimpath", "-o", h.work / "guest.wasm",
           "./examples/wasm/guest"], env={"GOOS": "wasip1", "GOARCH": "wasm", "CGO_ENABLED": "0"}, timeout=300)
    binary = (h.work / "guest.wasm").read_bytes()
    require(binary[:8] == b"\0asm\x01\0\0\0", "guest is not a WebAssembly core module")
    component = EXAMPLE / "spin-component"
    toolchain = tomllib.loads((component / "rust-toolchain.toml").read_text())["toolchain"]["channel"]
    target = h.work / "spin-target"
    h.run(["cargo", "+" + toolchain, "build", "--locked", "--release",
           "--manifest-path", component / "Cargo.toml", "--target", "wasm32-wasip2",
           "--target-dir", target], timeout=900)
    data = (target / "wasm32-wasip2/release/brewlet_spin_example.wasm").read_bytes()
    require(data[:8] == b"\0asm\x0d\0\x01\0", "Spin guest is not a WebAssembly component")
    (h.work / "spin-guest.wasm").write_bytes(data)
    h.result["wasmGuestInputs"] = {
        "command": {"kind": "WASIp1 core module", "sha256": hashlib.sha256(binary).hexdigest(),
                    "toolchain": h.run(["go", "version"]).stdout.strip()},
        "http": {"kind": "Spin SDK 7 HTTP component", "target": "wasm32-wasip2",
                 "sha256": hashlib.sha256(data).hexdigest(),
                 "cargoLockSHA256": hashlib.sha256((component / "Cargo.lock").read_bytes()).hexdigest(),
                 "toolchain": h.run(["rustc", "+" + toolchain, "--version"]).stdout.strip()},
    }
    records = {}
    for name in ("wasmtime", "spin"):
        engine = pins[name]
        records[name] = {}
        for revision in revisions:
            source = engine[revision][h.arch]
            executable = release_binary(source, name, h.arch)
            path = h.work / (name + "-" + revision)
            path.write_bytes(executable)
            path.chmod(0o755)
            layout = h.work / (name + "-" + revision + "-oci")
            descriptor = runtime_layout(layout, name, h.arch, revision, executable, base_layout, base)
            records[name][revision] = {"releaseVersion": engine[revision]["version"],
                "archive": source, "binarySHA256": hashlib.sha256(executable).hexdigest(),
                "descriptor": descriptor, "layout": str(layout), "binary": str(path),
                "base": pins["base"][h.arch], "interpreter": host_elf(executable, h.arch),
                "sourceKind": "custom-platform-fixture-pinned-distroless-cc-and-unchanged-release-binary"}
    h.result["wasmRuntimes"] = records
    return records
