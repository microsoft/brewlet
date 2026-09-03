#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Clone a Brewlet OCI target and inject a prohibited root user block."""

import argparse
import copy
import hashlib
import json
from pathlib import Path

JVM_CONFIG_ANNOTATION = "brewlet.sh/jvm-config"


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


def inject_user(root, descriptor):
    document = json.loads(blob_path(root, descriptor["digest"]).read_text())
    injected = 0

    if "manifests" in document:
        manifests = []
        for child in document["manifests"]:
            rewritten, child_injected = inject_user(root, child)
            manifests.append(rewritten)
            injected += child_injected
        document["manifests"] = manifests
    elif JVM_CONFIG_ANNOTATION in document.get("annotations", {}):
        launch_config = json.loads(document["annotations"][JVM_CONFIG_ANNOTATION])
        launch_config["user"] = {"uid": 0, "gid": 0}
        document["annotations"][JVM_CONFIG_ANNOTATION] = json.dumps(
            launch_config, separators=(",", ":"), sort_keys=True
        )
        injected = 1
    else:
        config_descriptor = document["config"]
        config = json.loads(blob_path(root, config_descriptor["digest"]).read_text())
        config["user"] = {"uid": 0, "gid": 0}

        config_digest, config_size = write_blob(root, config)
        document["config"]["digest"] = config_digest
        document["config"]["size"] = config_size
        injected = 1

    digest, size = write_blob(root, document)
    rewritten = copy.deepcopy(descriptor)
    rewritten["digest"] = digest
    rewritten["size"] = size
    return rewritten, injected


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

    target, injected = inject_user(root, source)
    if injected == 0:
        raise ValueError("source target contains no Brewlet launch config")
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
    print(target["digest"])


if __name__ == "__main__":
    main()
