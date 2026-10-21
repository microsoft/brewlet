"""Digest-bound project SBOMs and real distribution evidence, not product composition evidence."""

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from scripts.release import (SYFT, members, policy, read_json, require, run, sha,
                             validate_inventory, write_json)


def subject_set(document):
    subjects = {name: row["sha256"] for name, row in document["files"].items()}
    for component, image in document["images"].items():
        subjects["image-" + component] = image["digest"][7:]
        for target, child in image["platforms"].items():
            subjects["image-" + component + "-" + target.replace("/", "-")] = child["digest"][7:]
    return subjects


def spdx_subject(document, name, digest, timestamp):
    require(document.get("spdxVersion") == "SPDX-2.3"
            and isinstance(document.get("packages"), list), "invalid SPDX document")
    primary = {"SPDXID": "SPDXRef-BrewletReleaseSubject", "name": name,
               "downloadLocation": "NOASSERTION", "filesAnalyzed": False,
               "checksums": [{"algorithm": "SHA256", "checksumValue": digest}],
               "licenseConcluded": "NOASSERTION", "licenseDeclared": "NOASSERTION",
               "copyrightText": "NOASSERTION"}
    require(not any(p["SPDXID"] == primary["SPDXID"] for p in document["packages"]),
            "scanner used reserved subject SPDX ID")
    # Remove nondeterministic scanner time/namespace, not its discovered inventory.
    document["creationInfo"]["created"] = timestamp
    document["documentNamespace"] = "https://spdx.org/spdxdocs/brewlet-" + digest
    document["name"] = name
    relationships = [r for r in document.get("relationships", [])
                     if r["relationshipType"] != "DESCRIBES"]
    relationships.append({"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES",
                          "relatedSpdxElement": primary["SPDXID"]})
    for package in document["packages"]:
        relationships.append({"spdxElementId": primary["SPDXID"], "relationshipType": "CONTAINS",
                              "relatedSpdxElement": package["SPDXID"]})
    document["packages"].append(primary)
    document["relationships"] = relationships
    return document


def sboms(output):
    config = policy()
    document = validate_inventory(output, config)
    version = json.loads(run(["syft", "version", "-o", "json"]))
    require(version["version"].removeprefix("v") == SYFT, "unapproved Syft version")
    destination = output / "sboms"
    destination.mkdir()
    timestamp = datetime.fromtimestamp(document["sourceTimestamp"], timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    subjects = subject_set(document)
    mapping = {}
    for name, digest in subjects.items():
        with tempfile.TemporaryDirectory() as temp:
            if name in document["files"]:
                path = output / name
                if name.startswith("brewlet_"):
                    binary = Path(temp) / "brewlet"
                    binary.write_bytes(members(path)["brewlet"][0])
                    source = "file:" + str(binary)
                elif name.endswith(".jar") and not name.endswith(("-sources.jar", "-javadoc.jar")):
                    # Maven runtime dependencies are not shaded into the plugin JAR.
                    import shutil
                    shutil.copyfile(path, Path(temp) / name)
                    classpath = (ROOT / "maven-plugin/target/runtime-classpath.txt").read_text().strip()
                    for dependency in classpath.split(os.pathsep):
                        item = Path(dependency)
                        require(item.is_file(), "resolved Maven SBOM dependency missing")
                        shutil.copyfile(item, Path(temp) / item.name)
                    source = "dir:" + temp
                else:
                    source = "file:" + str(path)
                args = []
            else:
                component = name.removeprefix("image-")
                if component in document["images"]:
                    # An index is a set of manifests, not a single-platform filesystem.
                    children = document["images"][component]["platforms"]
                    packages = [{"SPDXID": "SPDXRef-" + target.replace("/", "-"), "name": target,
                                 "downloadLocation": "NOASSERTION", "filesAnalyzed": False,
                                 "licenseConcluded": "NOASSERTION", "licenseDeclared": "NOASSERTION",
                                 "copyrightText": "NOASSERTION",
                                 "checksums": [{"algorithm": "SHA256", "checksumValue": child["digest"][7:]}]}
                                for target, child in children.items()]
                    scanned = {"spdxVersion": "SPDX-2.3", "dataLicense": "CC0-1.0",
                               "SPDXID": "SPDXRef-DOCUMENT", "creationInfo": {
                                   "created": timestamp, "creators": ["Tool: Brewlet-release-index"]},
                               "packages": packages, "relationships": []}
                    result = spdx_subject(scanned, name, digest, timestamp)
                    path = destination / (name + ".spdx.json")
                    write_json(path, result)
                    mapping[name] = {"subjectSHA256": digest, "path": "sboms/" + path.name,
                                     "sha256": sha(path.read_bytes())}
                    continue
                component, system, arch = component.rsplit("-", 2)
                source = "oci-dir:" + str(output / ("image-" + component))
                args = ["--platform", system + "/" + arch]
            scanned = json.loads(run(["syft", "scan", source, *args, "-o", "spdx-json"]))
            result = spdx_subject(scanned, name, digest, timestamp)
            path = destination / (name + ".spdx.json")
            write_json(path, result)
            mapping[name] = {"subjectSHA256": digest, "path": "sboms/" + path.name, "sha256": sha(path.read_bytes())}
    write_json(output / "sboms/index.json", {"schemaVersion": 1,
               "candidateSHA256": sha((output / "candidate.json").read_bytes()), "subjects": mapping})


def verify_sboms(output, document):
    index = read_json(output / "sboms/index.json")
    require(index["schemaVersion"] == 1
            and index["candidateSHA256"] == sha((output / "candidate.json").read_bytes()),
            "SBOMs belong to another candidate")
    subjects = subject_set(document)
    require(set(index["subjects"]) == set(subjects), "incomplete SBOM coverage")
    for name, digest in subjects.items():
        row = index["subjects"][name]
        require(row["path"] == "sboms/" + name + ".spdx.json" and row["subjectSHA256"] == digest,
                "wrong SBOM subject")
        path = output / row["path"]
        require(not path.is_symlink() and sha(path.read_bytes()) == row["sha256"], "tampered SBOM")
        value = read_json(path)
        described = [p for p in value["packages"] if p["SPDXID"] == "SPDXRef-BrewletReleaseSubject"]
        require(len(described) == 1 and described[0]["checksums"] ==
                [{"algorithm": "SHA256", "checksumValue": digest}], "SPDX subject digest mismatch")
        require({"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES",
                 "relatedSpdxElement": "SPDXRef-BrewletReleaseSubject"} in value["relationships"],
                "missing SPDX subject relationship")


def attest_verify(path, workflow, source, repository="microsoft/brewlet"):
    run(["ghx", "attestation", "verify", path, "--repo", repository,
         "--signer-repo", repository, "--signer-workflow", repository + "/.github/workflows/" + workflow,
         "--signer-digest", source, "--source-digest", source, "--deny-self-hosted-runners"])


def receipt(output, scenario, assertions):
    document = validate_inventory(output, policy())
    architecture = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64"}.get(platform.machine())
    system = {"Linux": "linux", "Darwin": "darwin"}.get(platform.system())
    require(system and architecture, "unsupported native receipt platform")
    require(assertions and all(a["outcome"] == "passed" for a in assertions), "failed assertion")
    return {"schemaVersion": 1, "candidate": document["candidate"],
            "candidateSHA256": sha((output / "candidate.json").read_bytes()),
            "scenario": scenario, "platform": system + "/" + architecture,
            "outcome": "passed", "assertions": assertions}


def evidence_errors(value, output, document, scenario, target):
    errors = []
    for valid, message in (
        (value.get("schemaVersion") == 1, "receipt schema"),
        (value.get("candidate") == document["candidate"], "candidate source"),
        (value.get("candidateSHA256") == sha((output / "candidate.json").read_bytes()), "artifact manifest"),
        (value.get("scenario") == scenario and value.get("platform") == target, "native scope"),
        (value.get("outcome") == "passed", "receipt outcome"),
        (value.get("interventions", []) == [], "manual interventions"),
    ):
        if not valid:
            errors.append(message)
    required = {"native-cli-identity", "native-cli-conformance"} if scenario == "cli" else {
        "artifact-workload-execution", "production-drain", "production-uninstall",
        "host-reversal-and-cri", "target-reinstall", "target-workload-execution"}
    assertions = value.get("assertions")
    if (not isinstance(assertions, list) or not assertions
            or not all(isinstance(a, dict) and a.get("outcome") == "passed" for a in assertions)
            or not required <= {a.get("name") for a in assertions}):
        errors.append("missing successful assertion coverage")
    if scenario == "transition":
        if value.get("productionCleanupVerified") is not True or value.get("fixtureDestruction") != {"outcome": "passed"}:
            errors.append("production cleanup and fixture destruction")
        pair = value.get("transition")
        manifest = sha((output / "candidate.json").read_bytes())
        if (not isinstance(pair, dict) or pair.get("targetCandidateSHA256") != manifest
                or pair.get("sourceCandidateSHA256") != manifest
                or pair.get("sourceVersion") != document.get("version")
                or pair.get("targetVersion") != document.get("version")
                or pair.get("method") != "drain-teardown-reinstall"):
            errors.append("explicit source/target transition")
    return errors


def publication_ready(output):
    config = policy()
    document = validate_inventory(output, config)
    verify_sboms(output, document)
    source = document["candidate"]["checkout"]
    for path in (output / "candidate.json", output / "sboms/index.json"):
        attest_verify(path, "release-build.yml", source)
    policy_value = config["publicationPolicy"]
    legal = policy_value["legalApproval"]
    require(isinstance(legal, dict) and legal.get("approved") is True and legal.get("reference")
            and legal.get("containerPolicy") == "no-additional-text-required",
            "release-owner legal/container policy confirmation required")
    require(re.fullmatch(r"[0-9A-F]{40}|[0-9A-F]{64}", policy_value["mavenSigningFingerprint"] or ""),
            "release-owner Maven signing fingerprint required")
    for scenario, targets in (("cli", config["cliTargets"]), ("transition", config["imagePlatforms"])):
        for target in targets:
            path = output / "evidence" / f"{scenario}-{target.replace('/', '-')}.json"
            value = read_json(path)
            require(not evidence_errors(value, output, document, scenario, target), "invalid distribution receipt: " + str(path))
            workflow = "release-build.yml" if scenario == "cli" else "release-transition.yml"
            attest_verify(path, workflow, source)
            if scenario == "transition":
                approved = {key: value["transition"].get(key) for key in
                            ("sourceVersion", "targetVersion", "method")}
                require(approved in policy_value["supportedTransitions"],
                        "transition not approved by release support policy")
    return document


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("sboms", "verify", "ready", "smoke-cli"))
    parser.add_argument("--output", type=Path, default=ROOT / "dist/candidate")
    parser.add_argument("--target")
    args = parser.parse_args()
    output = args.output.resolve()
    if args.command == "sboms":
        sboms(output)
    elif args.command == "verify":
        verify_sboms(output, validate_inventory(output, policy()))
    elif args.command == "ready":
        publication_ready(output)
    else:
        from scripts.release import smoke_cli
        smoke_cli(output, args.target, policy())
        value = receipt(output, "cli", [
            {"name": "native-cli-identity", "outcome": "passed"},
            {"name": "native-cli-conformance", "outcome": "passed"}])
        destination = output / "evidence"
        destination.mkdir(exist_ok=True)
        write_json(destination / ("cli-" + args.target.replace("/", "-") + ".json"), value)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, KeyError, subprocess.CalledProcessError) as error:
        print("release assurance failed:", error, file=sys.stderr)
        raise SystemExit(1)
