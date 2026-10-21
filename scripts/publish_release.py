"""Explicit, qualified publication. No build/rebuild or credential provisioning occurs here."""

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
import zipfile

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from scripts.release import DIGEST, policy, require, run, sha, validate_inventory, write_json
from scripts.release_assurance import publication_ready, attest_verify


def request(url, *, headers=None, data=None, method=None, absent=False):
    try:
        with urllib.request.urlopen(urllib.request.Request(url, data=data, headers=headers or {},
                                                          method=method), timeout=120) as response:
            return response.read(), response.headers
    except urllib.error.HTTPError as error:
        if absent and error.code == 404:
            return None, error.headers
        raise


def preflight(output, confirmation):
    config = policy()
    require(confirmation == "publish-v" + config["version"], "explicit exact-version publication confirmation required")
    require(os.environ.get("GITHUB_REPOSITORY") == config["repository"]
            and os.environ.get("GITHUB_EVENT_NAME") == "workflow_dispatch",
            "publication must run in the repository's explicit manual workflow")
    document = publication_ready(output)
    tag = "v" + config["version"]
    tagged = run(["git", "rev-parse", "refs/tags/" + tag + "^{commit}"]).strip()
    require(tagged == document["candidate"]["checkout"]
            and tagged == os.environ.get("GITHUB_SHA"), "existing tag must identify the exact candidate checkout")
    return document


def sign_maven(output):
    config = policy()
    fingerprint = config["publicationPolicy"]["mavenSigningFingerprint"]
    require(fingerprint and re.fullmatch(r"[0-9A-F]{40}|[0-9A-F]{64}", fingerprint),
            "trusted Maven signing fingerprint required")
    require(os.environ.get("GNUPGHOME"), "an isolated existing-key import is required")
    key = os.environ["MAVEN_GPG_KEY"]
    subprocess.run(["gpg", "--batch", "--import"], input=key.encode(), check=True, stdout=subprocess.DEVNULL)
    keys = run(["gpg", "--batch", "--with-colons", "--list-secret-keys", fingerprint])
    require("fpr:::::::::" + fingerprint + ":" in keys, "imported key does not match trusted release identity")
    base = "brewlet-maven-plugin-" + config["version"]
    destination = output / "signatures"
    destination.mkdir(exist_ok=True)
    for suffix in (".jar", ".pom", "-sources.jar", "-javadoc.jar"):
        path = output / (base + suffix)
        signature = destination / (path.name + ".asc")
        if signature.exists():
            run(["gpg", "--batch", "--verify", signature, path])
            continue
        subprocess.run(["gpg", "--batch", "--no-tty", "--pinentry-mode", "loopback",
                        "--passphrase-fd", "0", "--local-user", fingerprint,
                        "--armor", "--detach-sign", "--output", str(signature), str(path)],
                       input=os.environ["MAVEN_GPG_PASSPHRASE"].encode(), check=True)
        status = run(["gpg", "--batch", "--status-fd", "1", "--verify", signature, path])
        require(any(line.startswith("[GNUPG:] VALIDSIG ") and fingerprint in line.split()
                    for line in status.splitlines()), "Maven signature not made by trusted key")
    payload = {}
    prefix = "sh/brewlet/brewlet-maven-plugin/" + config["version"] + "/"
    for suffix in (".jar", ".pom", "-sources.jar", "-javadoc.jar"):
        path = output / (base + suffix)
        for filename, data in ((path.name, path.read_bytes()),
                               (path.name + ".asc", (destination / (path.name + ".asc")).read_bytes())):
            payload[prefix + filename] = data
            for algorithm in ("md5", "sha1", "sha256", "sha512"):
                payload[prefix + filename + "." + algorithm] = hashlib.new(algorithm, data).hexdigest().encode()
    with zipfile.ZipFile(output / "central-bundle.zip", "w", compression=zipfile.ZIP_DEFLATED) as bundle:
        for name, data in sorted(payload.items()):
            bundle.writestr(name, data)


def registry_digest(repository, tag):
    scope = "repository:" + repository.removeprefix("ghcr.io/") + ":pull"
    headers = {}
    if os.environ.get("GH_TOKEN"):
        headers["Authorization"] = "Basic " + base64.b64encode(
            (os.environ["GITHUB_ACTOR"] + ":" + os.environ["GH_TOKEN"]).encode()).decode()
    token, _ = request("https://ghcr.io/token?" + urllib.parse.urlencode({"service": "ghcr.io", "scope": scope}),
                       headers=headers)
    token = json.loads(token)["token"]
    body, headers = request("https://ghcr.io/v2/" + repository.removeprefix("ghcr.io/") + "/manifests/" + tag,
                            headers={"Authorization": "Bearer " + token,
                                     "Accept": "application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json"},
                            absent=True)
    if body is None:
        return None
    digest = headers["Docker-Content-Digest"]
    require(DIGEST.fullmatch(digest), "registry returned no usable digest")
    return digest


def publish_images(output, document):
    config = policy()
    subprocess.run(["oras", "login", "ghcr.io", "--username", os.environ["GITHUB_ACTOR"], "--password-stdin"],
                   input=os.environ["GH_TOKEN"].encode(), check=True, stdout=subprocess.DEVNULL)
    for component, repository in config["images"].items():
        expected = document["images"][component]["digest"]
        existing = registry_digest(repository, config["version"])
        require(existing is None or existing == expected, "refusing to overwrite different published image")
        if existing is None:
            run(["oras", "cp", "--recursive", "--from-oci-layout",
                 str(output / ("image-" + component)) + "@" + expected,
                 repository + ":" + config["version"]])
        require(registry_digest(repository, config["version"]) == expected, "pushed image identity mismatch")
    chart = output / f"brewlet-{config['version']}.tgz"
    existing = registry_digest(config["chart"], config["version"])
    if existing is None:
        subprocess.run(["helm", "registry", "login", "ghcr.io", "--username", os.environ["GITHUB_ACTOR"],
                        "--password-stdin"], input=os.environ["GH_TOKEN"].encode(), check=True,
                       stdout=subprocess.DEVNULL)
        run(["helm", "push", chart, "oci://ghcr.io/microsoft/charts"])
    with tempfile.TemporaryDirectory() as temp:
        run(["helm", "pull", "oci://" + config["chart"], "--version", config["version"], "--destination", temp])
        require((Path(temp) / chart.name).read_bytes() == chart.read_bytes(),
                "existing/published Helm artifact differs; refusing overwrite")
    digest = registry_digest(config["chart"], config["version"])
    require(digest and DIGEST.fullmatch(digest), "missing chart OCI identity")
    write_json(output / "published.json", {
        "candidateSHA256": sha((output / "candidate.json").read_bytes()),
        "chartDigest": digest, "images": document["images"]})
    if os.environ.get("GITHUB_OUTPUT"):
        with Path(os.environ["GITHUB_OUTPUT"]).open("a") as stream:
            stream.write("chart_digest=" + digest + "\n")
            for component, image in document["images"].items():
                stream.write(component + "_digest=" + image["digest"] + "\n")


def central_payloads(output):
    config = policy()
    prefix = "https://repo.maven.apache.org/maven2/sh/brewlet/brewlet-maven-plugin/" + config["version"] + "/"
    paths = [output / name for name in validate_inventory(output, config, candidate=False)["files"]
             if name.startswith("brewlet-maven-plugin-")]
    existing = []
    for path in paths:
        data, _ = request(prefix + path.name, absent=True)
        if data is not None:
            require(data == path.read_bytes(), "Central version already contains different immutable bytes")
        existing.append(data is not None)
    require(not any(existing) or all(existing), "partial Central version requires owner reconciliation")
    return all(existing)


def publish_central(output):
    if central_payloads(output):
        return
    require((output / "central-bundle.zip").is_file(), "signed prebuilt Central bundle missing")
    credentials = base64.b64encode((os.environ["MAVEN_CENTRAL_USERNAME"] + ":" +
                                   os.environ["MAVEN_CENTRAL_PASSWORD"]).encode()).decode()
    headers = {"Authorization": "Bearer " + credentials}
    boundary = "brewlet-" + uuid.uuid4().hex
    data = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"bundle\"; filename=\"bundle.zip\"\r\n"
            "Content-Type: application/zip\r\n\r\n").encode()
    data += (output / "central-bundle.zip").read_bytes() + f"\r\n--{boundary}--\r\n".encode()
    uploaded, _ = request(
        "https://central.sonatype.com/api/v1/publisher/upload?publishingType=AUTOMATIC",
        headers={**headers, "Content-Type": "multipart/form-data; boundary=" + boundary}, data=data)
    deployment = uploaded.decode().strip()
    require(re.fullmatch(r"[0-9a-f-]{36}", deployment), "invalid Central deployment identity")
    write_json(output / "central-deployment.json", {"deploymentId": deployment})
    for _ in range(120):
        status, _ = request("https://central.sonatype.com/api/v1/publisher/status?" +
                            urllib.parse.urlencode({"id": deployment}), headers=headers, data=b"", method="POST")
        state = json.loads(status)["deploymentState"]
        require(state != "FAILED", "Central rejected the signed bundle; inspect deployment " + deployment)
        if state == "PUBLISHED":
            break
        require(state in ("PENDING", "VALIDATING", "VALIDATED", "PUBLISHING"), "unknown Central state")
        time.sleep(15)
    else:
        raise ValueError("Central publication deadline; reconcile deployment " + deployment + " before retry")
    require(central_payloads(output), "Central reported publication without exact distributable bytes")


def publish_release(output):
    config = policy()
    tag = "v" + config["version"]
    release_url = "https://api.github.com/repos/" + config["repository"] + "/releases/tags/" + tag
    body, _ = request(release_url, headers={"Authorization": "Bearer " + os.environ["GH_TOKEN"]}, absent=True)
    if body is None:
        run(["ghx", "release", "create", tag, "--repo", config["repository"], "--verify-tag",
             "--draft", "--prerelease", "--latest=false", "--title", "Brewlet " + config["version"],
             "--notes", "Qualified prerelease. Verify candidate.json, checksums and attestations before installation. "
             "See the source-pinned supported-release runbook; no general in-place upgrade is supported."])
    else:
        require(json.loads(body)["prerelease"] is True, "refusing to modify a non-prerelease")
    with tempfile.TemporaryDirectory() as temp:
        destination = Path(temp)
        document = validate_inventory(output, config)
        paths = [output / name for name in document["files"]]
        paths += [output / name for name in ("candidate.json", "checksums.txt", "published.json")]
        for directory in ("sboms", "evidence", "signatures", "attestations"):
            archive = destination / (directory + ".zip")
            with zipfile.ZipFile(archive, "w") as bundle:
                for path in sorted((output / directory).rglob("*")):
                    if path.is_file():
                        bundle.write(path, path.relative_to(output / directory))
            paths.append(archive)
        current = json.loads(run(["ghx", "release", "view", tag, "--repo", config["repository"], "--json", "assets"]))
        existing = {row["name"] for row in current["assets"]}
        require(existing <= {p.name for p in paths}, "unexpected assets on candidate release")
        for path in paths:
            if path.name in existing:
                with tempfile.TemporaryDirectory() as downloaded:
                    run(["ghx", "release", "download", tag, "--repo", config["repository"],
                         "--pattern", path.name, "--dir", downloaded])
                    require((Path(downloaded) / path.name).read_bytes() == path.read_bytes(),
                            "refusing to overwrite different release asset: " + path.name)
            else:
                run(["ghx", "release", "upload", tag, path, "--repo", config["repository"]])
        run(["ghx", "release", "edit", tag, "--repo", config["repository"],
             "--draft=false", "--prerelease", "--latest=false"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("preflight", "sign-maven", "images", "central", "release"))
    parser.add_argument("--output", type=Path, default=ROOT / "dist/candidate")
    parser.add_argument("--confirm", required=True)
    args = parser.parse_args()
    output = args.output.resolve()
    document = preflight(output, args.confirm)
    if args.command == "sign-maven":
        sign_maven(output)
    elif args.command == "images":
        publish_images(output, document)
    elif args.command == "central":
        publish_central(output)
    elif args.command == "release":
        publish_release(output)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, KeyError, subprocess.CalledProcessError) as error:
        print("publication failed:", error, file=sys.stderr)
        raise SystemExit(1)
