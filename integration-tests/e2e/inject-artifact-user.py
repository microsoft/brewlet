#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Clone an OCI artifact manifest and inject a prohibited root user block."""

import argparse
import copy
import hashlib
import json
from pathlib import Path


def blob_path(root, digest):
    hex_digest = digest[len("sha256:") :] if digest.startswith("sha256:") else digest
    return root / "blobs" / "sha256" / hex_digest


def encode_json(value):
    return json.dumps(value, separators=(",", ":"), sort_keys=True).encode()


def write_blob(root, value):
    data = encode_json(value)
    digest = "sha256:" + hashlib.sha256(data).hexdigest()
    path = blob_path(root, digest)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    return digest, len(data)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("layout")
    parser.add_argument("source_ref")
    parser.add_argument("target_ref")
    args = parser.parse_args()

    root = Path(args.layout)
    index_path = root / "index.json"
    index = json.loads(index_path.read_text())
    source = next(
        descriptor
        for descriptor in index["manifests"]
        if descriptor.get("annotations", {}).get(
            "org.opencontainers.image.ref.name"
        )
        == args.source_ref
    )

    manifest = json.loads(blob_path(root, source["digest"]).read_text())
    config = json.loads(blob_path(root, manifest["config"]["digest"]).read_text())
    config["user"] = {"uid": 0, "gid": 0}

    config_digest, config_size = write_blob(root, config)
    manifest["config"]["digest"] = config_digest
    manifest["config"]["size"] = config_size
    manifest_digest, manifest_size = write_blob(root, manifest)

    target = copy.deepcopy(source)
    target["digest"] = manifest_digest
    target["size"] = manifest_size
    target.setdefault("annotations", {})[
        "org.opencontainers.image.ref.name"
    ] = args.target_ref
    index["manifests"] = [
        descriptor
        for descriptor in index["manifests"]
        if descriptor.get("annotations", {}).get(
            "org.opencontainers.image.ref.name"
        )
        != args.target_ref
    ]
    index["manifests"].append(target)
    index_path.write_text(json.dumps(index, indent=2) + "\n")
    print(manifest_digest)


if __name__ == "__main__":
    main()
