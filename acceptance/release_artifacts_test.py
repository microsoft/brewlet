"""Synthetic OCI fixtures exercise artifact rejection, never stand in for native evidence."""

import gzip
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

from scripts import release as r


def blob(layout, value, media, *, raw=False):
    data = value if raw else json.dumps(value, sort_keys=True).encode()
    digest = "sha256:" + r.sha(data)
    path = layout / "blobs/sha256" / digest[7:]
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    return {"mediaType": media, "size": len(data), "digest": digest}


def layer(layout, files):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w") as archive:
        for name, data in files.items():
            info = tarfile.TarInfo(name)
            info.size, info.mode = len(data), 0o755 if name == "admission" else 0o644
            archive.addfile(info, io.BytesIO(data))
    return blob(layout, gzip.compress(output.getvalue(), mtime=0),
                "application/vnd.oci.image.layer.v1.tar+gzip", raw=True)


def image(layout, config, *, fault=None):
    descriptors = []
    for arch in ("amd64", "arm64"):
        target = "linux/" + arch
        files = {"LICENSE.txt": (r.ROOT / "LICENSE.txt").read_bytes(), "admission": b"fake-binary",
                 "NOTICE.txt": (f"Brewlet third-party notices\nTarget: {target}\n\nGo runtime go1.26.0\n"
                                "fixture.module v1.0.0\n--- LICENSE ---\nfixture text\n").encode()}
        if fault == "license":
            files["LICENSE.txt"] = b"wrong license"
        if fault == "notice":
            files["NOTICE.txt"] = b"wrong notice target"
        if fault == "module":
            files["NOTICE.txt"] = files["NOTICE.txt"].replace(b"fixture.module", b"missing.module")
        if fault == "binary":
            del files["admission"]
        layers = [layer(layout, files)]
        if fault == "whiteout":
            layers.append(layer(layout, {".wh.NOTICE.txt": b""}))
        if fault == "opaque":
            layers.append(layer(layout, {".wh..wh..opq": b""}))
        image_config = {"os": "linux", "architecture": arch, "config": {
            "Entrypoint": ["/wrong" if fault == "entrypoint" else "/admission"],
            "Labels": {"org.opencontainers.image.source": "https://github.com/microsoft/brewlet",
                       "org.opencontainers.image.version": config["version"],
                       "org.opencontainers.image.revision": "a" * 40}}}
        manifest = {"schemaVersion": 2, "config": blob(
            layout, image_config, "application/vnd.oci.image.config.v1+json"), "layers": layers}
        descriptor = blob(layout, manifest, "application/vnd.oci.image.manifest.v1+json")
        descriptor["platform"] = {"os": "linux", "architecture": arch}
        descriptors.append(descriptor)
    if fault == "duplicate-platform":
        descriptors[1] = descriptors[0]
    index = blob(layout, {"schemaVersion": 2, "manifests": descriptors},
                 "application/vnd.oci.image.index.v1+json")
    (layout / "index.json").write_text(json.dumps({"schemaVersion": 2, "manifests": [index]}))


class ImageArtifactTests(unittest.TestCase):
    def test_actual_descriptor_graph_and_final_files_are_checked(self):
        config = r.policy()
        for fault in (None, "license", "notice", "module", "binary", "whiteout", "opaque",
                      "entrypoint", "duplicate-platform", "blob"):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as temp:
                layout = Path(temp)
                image(layout, config, fault=fault)
                if fault == "blob":
                    descriptor = r.read_json(layout / "index.json")["manifests"][0]
                    (layout / "blobs/sha256" / descriptor["digest"][7:]).write_bytes(b"tampered")
                def metadata(args, **kwargs):
                    path = args[-1]
                    # Image verification must inspect both native platform binaries.
                    arch = "amd64" if command.call_count == 1 else "arm64"
                    return f"{path}: go1.26.0\n\tdep\tfixture.module\tv1.0.0\n\tbuild\tGOOS=linux\n\tbuild\tGOARCH={arch}\n"
                with patch.object(r, "run", side_effect=metadata) as command:
                    if fault:
                        with self.assertRaises(ValueError):
                            r.image_identity(layout, "admission", config)
                    else:
                        value = r.image_identity(layout, "admission", config)
                        self.assertEqual({"linux/amd64", "linux/arm64"}, set(value["platforms"]))


if __name__ == "__main__":
    unittest.main()
