#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""SBOM-driven CVE sweep over running Brewlet workloads (tier 18).

``sweep`` answers "which Brewlet applications running in the cluster ship a
vulnerable third-party library?" without trusting image tags or application
metadata:

  running Pod (runtimeClassName=brewlet)
    -> digest-pinned image
    -> ``brewlet inspect`` managed-dependency evidence (bundle + SBOM digests)
    -> the bundle's single CycloneDX SBOM referrer, digest-verified
    -> components matching the advisory's Maven coordinates and version range

``compare`` reports which layers two runnable images share, proving a
remediation recomposition reused the application layer and replaced only the
managed classpath layer.

Both read a local OCI layout (the tier's registry stand-in). The logic is the
same against a registry: resolve the digest, read the evidence annotation, and
fetch the bundle's CycloneDX referrer.
"""

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys

REF_NAME = "org.opencontainers.image.ref.name"
REFERRER_SUBJECT = "brewlet.sh/referrer-subject"
CYCLONEDX = "application/vnd.cyclonedx+json"
EVIDENCE_HEADER = "== managed dependency evidence (unsigned) =="
TERMINAL_PHASES = {"Succeeded", "Failed"}


def version_key(version):
    """Order Maven-style versions: 2.0-beta9 < 2.0 < 2.14.1 < 2.17.1."""
    release, _, qualifier = version.partition("-")
    numbers = []
    for part in release.split("."):
        match = re.match(r"\d+", part)
        numbers.append(int(match.group()) if match else 0)
    numbers += [0] * (4 - len(numbers))
    if not qualifier:
        return tuple(numbers), (1,)
    match = re.match(r"([A-Za-z]*)(\d*)", qualifier)
    label, number = match.group(1).lower(), int(match.group(2) or 0)
    return tuple(numbers), (0, label, number)


def is_affected(version, introduced, fixed):
    key = version_key(version)
    return version_key(introduced) <= key < version_key(fixed)


def image_digest(image):
    if "@sha256:" in image:
        return "sha256:" + image.rsplit("@sha256:", 1)[1]
    if image.startswith("sha256:"):
        return image
    return ""


def workload_of(pod):
    meta = pod.get("metadata", {})
    owners = meta.get("ownerReferences") or []
    if not owners:
        return "Pod", meta.get("name", "")
    owner = owners[0]
    template_hash = meta.get("labels", {}).get("pod-template-hash", "")
    if owner.get("kind") == "ReplicaSet" and template_hash and \
            owner.get("name", "").endswith("-" + template_hash):
        return "Deployment", owner["name"][: -len(template_hash) - 1]
    return owner.get("kind", ""), owner.get("name", "")


class Layout:
    def __init__(self, root):
        self.root = root
        with open(os.path.join(root, "index.json"), encoding="utf-8") as stream:
            self.index = json.load(stream).get("manifests", [])

    def blob(self, digest):
        algorithm, _, value = digest.partition(":")
        with open(os.path.join(self.root, "blobs", algorithm, value), "rb") as stream:
            raw = stream.read()
        if "sha256:" + hashlib.sha256(raw).hexdigest() != digest:
            raise ValueError(f"blob {digest} failed digest verification")
        return raw

    def ref_for_digest(self, digest):
        for desc in self.index:
            ref = desc.get("annotations", {}).get(REF_NAME)
            if desc.get("digest") == digest and ref:
                return ref
        return ""

    def descriptor_for_ref(self, ref):
        for desc in self.index:
            if desc.get("annotations", {}).get(REF_NAME) == ref:
                return desc
        raise KeyError(f"ref {ref!r} not found in {self.root}")

    def platform_manifest(self, ref, arch):
        desc = self.descriptor_for_ref(ref)
        document = json.loads(self.blob(desc["digest"]))
        if "manifests" in document:
            for entry in document["manifests"]:
                platform = entry.get("platform", {})
                if platform.get("os") == "linux" and platform.get("architecture") == arch:
                    return json.loads(self.blob(entry["digest"]))
            raise KeyError(f"{ref} has no linux/{arch} manifest")
        return document

    def bundle_sbom(self, bundle_digest, sbom_digest):
        referrers = [
            desc for desc in self.index
            if desc.get("artifactType") == CYCLONEDX
            and desc.get("annotations", {}).get(REFERRER_SUBJECT) == bundle_digest
        ]
        if len(referrers) != 1:
            raise ValueError(
                f"bundle {bundle_digest} has {len(referrers)} CycloneDX referrers, want exactly 1")
        manifest = json.loads(self.blob(referrers[0]["digest"]))
        if (manifest.get("subject") or {}).get("digest") != bundle_digest:
            raise ValueError(f"SBOM referrer subject does not bind bundle {bundle_digest}")
        layers = manifest.get("layers") or []
        if len(layers) != 1 or layers[0].get("digest") != sbom_digest:
            raise ValueError(f"SBOM referrer does not carry evidence SBOM {sbom_digest}")
        return json.loads(self.blob(sbom_digest))


def inspect_evidence(brewlet, store, ref):
    out = subprocess.run(
        [brewlet, "inspect", ref, "--store", store],
        check=True, capture_output=True, text=True).stdout
    if EVIDENCE_HEADER not in out:
        return None
    return json.JSONDecoder().raw_decode(out.split(EVIDENCE_HEADER, 1)[1].lstrip())[0]


def scan_image(layout, brewlet, store, digest, group, artifact, introduced, fixed):
    ref = layout.ref_for_digest(digest)
    if not ref:
        raise LookupError(f"image {digest} is not in the artifact store")
    evidence = inspect_evidence(brewlet, store, ref)
    if evidence is None:
        raise LookupError(f"image {ref} carries no managed dependency evidence")
    sbom = layout.bundle_sbom(evidence["dependencyBundleDigest"], evidence["sbomDigest"])
    findings, components = [], []
    for component in sbom.get("components", []):
        if component.get("group") != group or component.get("name") != artifact:
            continue
        version = component.get("version", "")
        components.append(version)
        if is_affected(version, introduced, fixed):
            findings.append({"purl": component.get("purl", ""), "version": version})
    return {
        "ref": ref,
        "digest": digest,
        "applicationJarDigest": evidence["applicationJarDigest"],
        "dependencyBundleDigest": evidence["dependencyBundleDigest"],
        "sbomDigest": evidence["sbomDigest"],
        "sourceBom": evidence.get("sourceBom", ""),
        "componentVersions": components,
        "findings": findings,
    }


def sweep(args):
    group, _, artifact = args.component.partition(":")
    with open(args.pods, encoding="utf-8") as stream:
        pods = json.load(stream).get("items", [])
    layout = Layout(args.store)
    cache, workloads, unscannable, brewlet_pods = {}, {}, [], 0
    for pod in pods:
        meta, spec = pod.get("metadata", {}), pod.get("spec", {})
        if spec.get("runtimeClassName") != "brewlet":
            continue
        if pod.get("status", {}).get("phase") in TERMINAL_PHASES:
            continue
        namespace = meta.get("namespace", "")
        if args.namespace and namespace not in args.namespace:
            continue
        brewlet_pods += 1
        kind, name = workload_of(pod)
        entry = workloads.setdefault((namespace, kind, name), {
            "namespace": namespace, "kind": kind, "name": name,
            "pods": [], "images": {}, "vulnerable": False,
        })
        entry["pods"].append(meta.get("name", ""))
        statuses = {s.get("name"): s for s in pod.get("status", {}).get("containerStatuses") or []}
        for container in spec.get("containers", []):
            image = container.get("image", "")
            digest = image_digest(image) or image_digest(
                statuses.get(container.get("name"), {}).get("imageID", ""))
            if not digest:
                unscannable.append({"namespace": namespace, "pod": meta.get("name", ""),
                                    "image": image, "reason": "image is not digest-pinned"})
                continue
            if digest not in cache:
                try:
                    cache[digest] = scan_image(layout, args.brewlet, args.store, digest,
                                               group, artifact, args.introduced, args.fixed)
                except (LookupError, ValueError, OSError, subprocess.CalledProcessError) as error:
                    cache[digest] = {"error": str(error)}
            result = cache[digest]
            if "error" in result:
                unscannable.append({"namespace": namespace, "pod": meta.get("name", ""),
                                    "image": image, "reason": result["error"]})
                continue
            entry["images"][digest] = result
            entry["vulnerable"] = entry["vulnerable"] or bool(result["findings"])
    report = {
        "advisory": {"component": args.component, "introduced": args.introduced,
                     "fixed": args.fixed},
        "brewletPods": brewlet_pods,
        "workloads": [],
        "unscannable": unscannable,
    }
    for key in sorted(workloads):
        entry = workloads[key]
        entry["pods"].sort()
        entry["images"] = [entry["images"][d] for d in sorted(entry["images"])]
        report["workloads"].append(entry)
    report["vulnerable"] = sorted(
        f"{w['namespace']}/{w['name']}" for w in report["workloads"] if w["vulnerable"])
    json.dump(report, sys.stdout, indent=2)
    print()
    if args.fail_on_findings and (report["vulnerable"] or unscannable):
        return 3
    return 0


def compare(args):
    layout = Layout(args.store)
    layers = []
    for ref in (args.ref_a, args.ref_b):
        manifest = layout.platform_manifest(ref, args.arch)
        layers.append({
            layer["digest"]: layer.get("annotations", {}).get("brewlet.sh/layer", "")
            for layer in manifest.get("layers", [])
        })
    a, b = layers
    json.dump({
        "shared": sorted(f"{a[d]}={d}" for d in a if d in b),
        "onlyA": sorted(f"{a[d]}={d}" for d in a if d not in b),
        "onlyB": sorted(f"{b[d]}={d}" for d in b if d not in a),
    }, sys.stdout, indent=2)
    print()
    return 0


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    commands = parser.add_subparsers(dest="command", required=True)
    s = commands.add_parser("sweep", help="find running Brewlet workloads affected by an advisory")
    s.add_argument("--store", required=True, help="OCI layout holding the deployed images")
    s.add_argument("--brewlet", required=True, help="brewlet CLI binary")
    s.add_argument("--pods", required=True, help="output of `kubectl get pods -A -o json`")
    s.add_argument("--component", required=True, help="Maven groupId:artifactId")
    s.add_argument("--introduced", required=True, help="first affected version (inclusive)")
    s.add_argument("--fixed", required=True, help="first fixed version (exclusive bound)")
    s.add_argument("--namespace", action="append", default=[],
                   help="limit the sweep to this namespace (repeatable)")
    s.add_argument("--fail-on-findings", action="store_true",
                   help="exit 3 when a workload is affected or cannot be scanned")
    s.set_defaults(func=sweep)
    c = commands.add_parser("compare", help="diff the layers of two runnable images")
    c.add_argument("--store", required=True)
    c.add_argument("--arch", required=True)
    c.add_argument("ref_a")
    c.add_argument("ref_b")
    c.set_defaults(func=compare)
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
