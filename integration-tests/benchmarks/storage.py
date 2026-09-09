#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Measure actual compressed OCI descriptors in an anonymous loopback registry."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import urllib.parse
import urllib.request
from resources import Ledger


ACCEPT = ", ".join(["application/vnd.oci.image.index.v1+json",
                    "application/vnd.oci.image.manifest.v1+json",
                    "application/vnd.docker.distribution.manifest.v2+json",
                    "application/vnd.docker.distribution.manifest.list.v2+json"])


def request(url, method="GET", data=None, headers=None):
    with urllib.request.urlopen(urllib.request.Request(url, data=data, method=method,
                                headers=headers or {}), timeout=120) as response:
        return response.read(), dict(response.headers)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", required=True, type=Path)
    parser.add_argument("--registry")
    parser.add_argument("--include-runtime-update", action="store_true")
    args = parser.parse_args()
    ledger = Ledger(args.work)
    registry = args.registry or ledger.state["registry_endpoint"]
    if registry != ledger.state["registry_endpoint"] or not registry.startswith("127.0.0.1:"):
        raise SystemExit("Refusing to publish benchmark images outside loopback")
    owned = next(x for x in ledger.state["containers"] if x["role"] == "registry")
    current = ledger.inspect("container", owned["name"])
    if current is None or current["id"] != owned["id"] or \
            current["labels"].get("brewlet.benchmark") != ledger.state["run_id"]:
        raise SystemExit("Refusing an unowned/replaced registry")
    work = args.work
    prefix = ledger.state["names"]["prefix"]
    repository = ledger.state["names"]["registry_repository"]
    base = "http://" + registry
    results = {"schema_version": 1,
               "metric": "compressed content-addressed transfer payload, NOT observed network traffic",
               "units": "bytes", "images": {},
               "not_measured": ["HTTP headers", "TLS", "retries", "transfer duration",
                                "billing", "CVE remediation verification"]}

    def inspect(name, ref):
        repo = ref.split("/", 1)[1].rsplit(":", 1)[0]
        tag = ref.rsplit(":", 1)[1]
        content, headers = request(base + f"/v2/{repo}/manifests/{tag}", headers={"Accept": ACCEPT})
        manifest = json.loads(content)
        index = None
        if "manifests" in manifest:
            index = {"digest": "sha256:" + hashlib.sha256(content).hexdigest(), "size": len(content)}
            child = next(x for x in manifest["manifests"] if x.get("platform", {}).get("architecture") == "arm64")
            content, headers = request(base + f"/v2/{repo}/manifests/{child['digest']}", headers={"Accept": ACCEPT})
            manifest = json.loads(content)
        blobs = [manifest["config"], *manifest["layers"]]
        for desc in blobs:
            _, head = request(base + f"/v2/{repo}/blobs/{desc['digest']}", method="HEAD")
            if int(head["Content-Length"]) != desc["size"]:
                raise RuntimeError("Registry stored blob length differs from descriptor")
        record = {"manifest_digest": "sha256:" + hashlib.sha256(content).hexdigest(),
                  "manifest_bytes": len(content), "index": index,
                  "config": manifest["config"], "layers": manifest["layers"]}
        (work / (name + "-registry-manifest.json")).write_bytes(content)
        results["images"][name] = record

    sources = {
        "jdk": "eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b",
        "conventional": prefix + "/conventional:baseline",
        "conventional-revision": prefix + "/conventional:revision",
        "fat": prefix + "/fat:baseline",
        "fat-revision": prefix + "/fat:revision",
        "operator": prefix + "/operator:baseline",
        "admission": prefix + "/admission:baseline",
        "provisioner": prefix + "/provisioner:baseline",
    }
    if args.include_runtime_update:
        sources.update({"jdk-old": prefix + "/jdk:old",
                        "conventional-old-jdk": prefix + "/conventional:old-jdk",
                        "conventional-linked": prefix + "/conventional-linked:baseline",
                        "conventional-linked-old-jdk": prefix + "/conventional-linked:old-jdk"})
    else:
        results["not_measured"].append("real alternate-JDK delta")
    for name, source in sources.items():
        dest = f"{registry}/{repository}/{name}:measured"
        ledger.tag(source, dest)
        with (work / (name + "-push.log")).open("w") as log:
            subprocess.run(["docker", "push", "--platform", "linux/arm64", dest],
                           check=True, stdout=log, stderr=subprocess.STDOUT)
        inspect(name, dest)
    for name, layout in [("brewlet", work / "brewlet-oci"), ("brewlet-revision", work / "brewlet-revision-oci")]:
        repo = repository + "/" + name
        descriptor = json.loads((layout / "index.json").read_text())["manifests"][0]
        readblob = lambda digest: (layout / "blobs" / digest.replace(":", "/")).read_bytes()
        index = json.loads(readblob(descriptor["digest"]))
        child = next(x for x in index["manifests"] if x["platform"]["architecture"] == "arm64")
        raw_manifest = readblob(child["digest"])
        manifest = json.loads(raw_manifest)
        for desc in [manifest["config"], *manifest["layers"]]:
            data = readblob(desc["digest"])
            assert len(data) == desc["size"]
            assert "sha256:" + hashlib.sha256(data).hexdigest() == desc["digest"]
            _, headers = request(base + f"/v2/{repo}/blobs/uploads/", "POST", b"")
            url = urllib.parse.urljoin(base, headers["Location"])
            if urllib.parse.urlsplit(url).netloc != registry:
                raise RuntimeError("Registry upload redirected outside the task-owned loopback endpoint")
            url += ("&" if "?" in url else "?") + urllib.parse.urlencode({"digest": desc["digest"]})
            request(url, "PUT", data, {"Content-Type": "application/octet-stream"})
        request(base + f"/v2/{repo}/manifests/measured", "PUT", raw_manifest,
                {"Content-Type": manifest["mediaType"]})
        inspect(name, f"{registry}/{repo}:measured")
        results["images"][name]["artifact_index"] = descriptor

    def objects(*names):
        data = {}
        for name in names:
            image = results["images"][name]
            for blob in [image["config"], *image["layers"]]:
                data[blob["digest"]] = blob["size"]
            data[image["manifest_digest"]] = image["manifest_bytes"]
        return data

    def delta(names, cache=()):
        needed, cached = objects(*names), objects(*cache)
        missing = {k: v for k, v in needed.items() if k not in cached}
        return {"bytes": sum(missing.values()), "unique_objects": len(missing),
                "digests": list(missing)}

    results["scenarios"] = {
        "empty_cache": {"conventional": delta(["conventional"]), "fat": delta(["fat"]),
                        "brewlet_app_plus_runtime": delta(["brewlet", "jdk"])},
        "runtime_cached": {"conventional": delta(["conventional"], ["jdk"]),
                           "fat": delta(["fat"], ["jdk"]),
                           "brewlet": delta(["brewlet"], ["jdk"])},
        "second_identical_app_or_replica": {
            "conventional": delta(["conventional"], ["conventional"]),
            "brewlet": delta(["brewlet", "jdk"], ["brewlet", "jdk"]),
            "fat": delta(["fat"], ["fat"])},
        "resource_revision": {"conventional": delta(["conventional-revision"], ["conventional"]),
                              "brewlet": delta(["brewlet-revision"], ["brewlet", "jdk"]),
                              "fat": delta(["fat-revision"], ["fat"])},
        "brewlet_components_additional": delta(["operator", "admission", "provisioner"], ["jdk"]),
    }
    results["scope"] = "Selected linux/arm64 manifest, config and layer objects; index retrieval excluded equally."
    if args.include_runtime_update:
        results["scenarios"]["runtime_build_update"] = {
            "conventional_linked_rebase": delta(["conventional-linked"], ["conventional-linked-old-jdk"]),
            "conventional_standard_copy_rebuild": delta(["conventional"], ["conventional-old-jdk"]),
            "brewlet_runtime_plus_unchanged_app": delta(["jdk", "brewlet"], ["jdk-old", "brewlet"]),
            "brewlet_app_only": delta(["brewlet"], ["brewlet"]),
        }
        results["runtime_build_update"] = {
            "direction": "Temurin 21.0.11+10 source image to Temurin 21.0.12+8 source image",
            "scope": "Includes any userland/source-image layer changes; not a JDK-binary-only or CVE-fix measurement.",
            "optimized_control": "Additional storage-only COPY --link images preserve application/dependency layers across base changes. Primary runtime comparison uses the original split-layer conventional image.",
            "conventional_manifest_changed": (results["images"]["conventional-old-jdk"]["manifest_digest"] !=
                                               results["images"]["conventional"]["manifest_digest"]),
        }
    (work / "storage-raw.json").write_text(json.dumps(results, indent=2) + "\n")
    print(json.dumps(results["scenarios"], indent=2))


if __name__ == "__main__":
    main()
