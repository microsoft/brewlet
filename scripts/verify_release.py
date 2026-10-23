"""Read-only verification of published prerelease bytes, attestations and Central signatures."""

import argparse
import json
from pathlib import Path, PurePosixPath
import re
import sys
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from scripts.release import policy, read_json, require, run, validate_inventory, validate_payloads
from scripts.release_assurance import attest_verify, publication_ready, verify_sboms
from scripts.publish_release import central_payloads, registry_digest


def unpack(path, destination):
    with zipfile.ZipFile(path) as archive:
        names = set()
        for item in archive.infolist():
            name = PurePosixPath(item.filename)
            require(not name.is_absolute() and ".." not in name.parts
                    and not item.is_dir() and item.filename not in names
                    and (item.external_attr >> 16) & 0o170000 != 0o120000,
                    "unsafe verification archive")
            names.add(item.filename)
            target = destination / item.filename
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(archive.read(item))


def verify(source):
    config = policy()
    require(re.fullmatch(r"[0-9a-f]{40}", source), "expected immutable source SHA required")
    with tempfile.TemporaryDirectory() as temp:
        output = Path(temp)
        run(["ghx", "release", "download", "v" + config["version"], "--repo", config["repository"],
             "--dir", output])
        for directory in ("sboms", "evidence", "signatures", "attestations"):
            path = output / (directory + ".zip")
            unpack(path, output / directory)
            path.unlink()
        attest_verify(output / "candidate.json", "release-build.yml", source)
        document = read_json(output / "candidate.json")
        require(document["candidate"]["checkout"] == source, "release source mismatch")
        attest_verify(output / "sboms/index.json", "release-build.yml", source)
        attest_verify(output / "published.json", "release.yml", source)
        published = read_json(output / "published.json")
        require(published["images"] == document["images"], "published image set mismatch")
        for component, repository in config["images"].items():
            digest = document["images"][component]["digest"]
            require(registry_digest(repository, config["version"]) == digest, "published image tag drift")
            attest_verify("oci://" + repository + "@" + digest, "release.yml", source)
            layout = output / ("image-" + component)
            run(["oras", "cp", "--to-oci-layout", repository + "@" + digest, str(layout) + ":verified"])
            # oras also lists child platform manifests; keep only the pinned top-level index.
            index = read_json(layout / "index.json")
            index["manifests"] = [m for m in index["manifests"] if m["digest"] == digest]
            (layout / "index.json").write_text(json.dumps(index))
        attest_verify("oci://" + config["chart"] + "@" + published["chartDigest"], "release.yml", source)
        with tempfile.TemporaryDirectory() as chart_temp:
            run(["helm", "pull", "oci://" + config["chart"], "--version", config["version"],
                 "--destination", chart_temp])
            name = "brewlet-" + config["version"] + ".tgz"
            require((Path(chart_temp) / name).read_bytes() == (output / name).read_bytes(), "chart coordinate drift")
        validate_inventory(output, config, candidate=False)
        validate_payloads(output, config)
        verify_sboms(output, document)
        publication_ready(output)
        for name in document["files"]:
            attest_verify(output / name, "release-build.yml", source)
        require(central_payloads(output), "Maven coordinates not published")
        fingerprint = config["publicationPolicy"]["mavenSigningFingerprint"]
        require(fingerprint and re.fullmatch(r"[0-9A-F]{40}|[0-9A-F]{64}", fingerprint),
                "trusted Maven public key fingerprint required")
        signatures = list((output / "signatures").glob("*.asc"))
        require({p.name for p in signatures} == {name + ".asc" for name in document["files"]
                                                if name.startswith("brewlet-maven-plugin-")},
                "incomplete Maven signing coverage")
        for path in signatures:
            payload = output / path.name.removesuffix(".asc")
            status = run(["gpg", "--batch", "--status-fd", "1", "--verify", path, payload])
            require(any(line.startswith("[GNUPG:] VALIDSIG ") and fingerprint in line.split()
                        for line in status.splitlines()), "untrusted Maven signature")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True)
    args = parser.parse_args()
    verify(args.source)
