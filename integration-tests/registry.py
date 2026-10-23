#!/usr/bin/env python3
"""Exercise real OCI publication against an isolated, digest-pinned registry."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid


ROOT = Path(__file__).resolve().parents[1]
REGISTRY = "docker.io/library/registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e"


def command(args, *, env=None):
    return subprocess.check_output(list(map(str, args)), text=True, env=env).strip()


def fetch(url, media_type=None):
    request = urllib.request.Request(url, headers={"Accept": media_type} if media_type else {})
    with urllib.request.urlopen(request, timeout=5) as response:
        return response.read()


def dependency_reuse(manifest, config, source):
    dependencies = [(index, layer) for index, layer in enumerate(manifest["layers"])
                    if layer.get("annotations", {}).get("brewlet.sh/layer") == "dependency"]
    assert len(dependencies) == len(source["layers"]) and dependencies, "dependency count changed"
    for (index, layer), original in zip(dependencies, source["layers"]):
        assert all(layer[key] == original["descriptor"][key] for key in ("digest", "size", "mediaType")), (
            "compressed dependency descriptor changed")
        assert config["rootfs"]["diff_ids"][index] == original["diffId"], "dependency diff ID changed"
    evidence = json.loads(manifest["annotations"]["brewlet.sh/layerset-evidence"])
    assert len(evidence) == 1
    for key in ("manifestDigest", "configDigest", "lockDigest", "sbomDigest", "attestationDigest", "layers"):
        assert key in source["evidence"] and evidence[0].get(key) == source["evidence"][key], (
            "layer-set provenance binding changed")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--brewlet", required=True, type=Path)
    parser.add_argument("--work-parent", type=Path, default=Path(tempfile.gettempdir()))
    parser.add_argument("--producer-registry-host", default="127.0.0.1",
                        help="host visible to the producer; host.docker.internal for a Docker wrapper")
    parser.add_argument("--registry-platform", default="linux/amd64")
    args = parser.parse_args()
    executable = args.brewlet.resolve()
    invocation = str(uuid.uuid4())
    work = Path(tempfile.mkdtemp(prefix="brewlet-v2-registry-", dir=args.work_parent))
    config = work / "docker-config"
    config.mkdir()
    (config / "config.json").write_text('{"auths":{}}\n')
    environment = dict(os.environ, DOCKER_CONFIG=str(config), BREWLET_TEST_CLI_CWD=str(work))
    environment.pop("BREWLET_REGISTRY_USERNAME", None)
    environment.pop("BREWLET_REGISTRY_PASSWORD", None)
    container = command([
        "docker", "run", "--platform", args.registry_platform, "--detach",
        "--name", f"brewlet-v2-registry-{invocation}",
        "--label", f"brewlet.test.invocation={invocation}",
        "--publish", "127.0.0.1::5000", "--read-only",
        "--tmpfs", "/var/lib/registry", "--tmpfs", "/tmp",
        "--memory", "256m", "--cpus", "1", REGISTRY,
    ])
    try:
        inspection = json.loads(command(["docker", "inspect", container]))[0]
        assert inspection["Config"]["Labels"]["brewlet.test.invocation"] == invocation
        port = inspection["NetworkSettings"]["Ports"]["5000/tcp"][0]["HostPort"]
        authority = f"127.0.0.1:{port}"
        base = f"http://{authority}/v2"
        deadline = time.monotonic() + 60
        while True:
            try:
                fetch(base + "/")
                break
            except (urllib.error.URLError, TimeoutError, ConnectionResetError):
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.25)
        reference = f"{args.producer_registry_host}:{port}/fixtures/python:v2"
        fixture = ROOT / "integration-tests/fixtures/python"
        layout = work / "python-layout"
        command([executable, "build", reference, "--store", layout,
                 "--platform", "linux/amd64", "--platform", "linux/arm64",
                 "--launch", fixture / "launch.json",
                 "--layer", f"dependency={fixture / 'dependencies'}:/app/dependencies",
                 "--layer", f"app={fixture / 'src'}:/app/src"], env=environment)
        push = [executable, "push", reference, "--store", layout, "--insecure"]
        first = command(push, env=environment)
        second = command(push, env=environment)
        assert first == second, "repeated remote publication changed its handoff"
        index_bytes = fetch(base + "/fixtures/python/manifests/v2",
                            "application/vnd.oci.image.index.v1+json")
        index = json.loads(index_bytes)
        assert len(index["manifests"]) == 2
        for descriptor in index["manifests"]:
            content = fetch(base + "/fixtures/python/manifests/" + descriptor["digest"],
                            descriptor["mediaType"])
            assert len(content) == descriptor["size"]
            assert "sha256:" + hashlib.sha256(content).hexdigest() == descriptor["digest"]
            manifest = json.loads(content)
            assert manifest["annotations"]["brewlet.sh/artifact-version"] == "2"
            for blob_descriptor in [manifest["config"], *manifest["layers"]]:
                data = fetch(base + "/fixtures/python/blobs/" + blob_descriptor["digest"])
                assert len(data) == blob_descriptor["size"]
                assert "sha256:" + hashlib.sha256(data).hexdigest() == blob_descriptor["digest"]
        output = command([executable, "inspect", reference, "--insecure", "--platform", "linux/amd64"],
                         env=environment)
        assert "python3" in output and "launch" in output
        private_key = work / "test-signing-key.pem"
        public_key = work / "test-signing-public.pem"
        command(["openssl", "genpkey", "-algorithm", "EC", "-pkeyopt",
                 "ec_paramgen_curve:P-256", "-out", private_key])
        command(["openssl", "pkey", "-in", private_key, "-pubout", "-out", public_key])
        config_file = work / "layer-set-config.json"
        lock_file = work / "layer-set-lock.json"
        config_file.write_text(json.dumps({
            "schemaVersion": 1, "name": "fixture-dependencies", "version": "2.0.0",
            "ecosystem": "python", "layers": [],
        }))
        lock_file.write_text(json.dumps({
            "schemaVersion": 1,
            "entries": [{
                "ecosystem": "python", "name": "greeting", "version": "1",
                "path": "/app/dependencies/greeting.py",
                "sha256": hashlib.sha256((fixture / "dependencies/greeting.py").read_bytes()).hexdigest(),
                "purl": "pkg:pypi/greeting@1",
            }],
        }))
        layer_set_reference = f"{args.producer_registry_host}:{port}/fixtures/dependencies:v2"
        identity = "https://brewlet.test/fixture-builder"
        published = json.loads(command([
            executable, "layer-set", layer_set_reference, "--insecure",
            "--config", config_file, "--lock", lock_file,
            "--layer", f"dependency={fixture / 'dependencies'}:/app/dependencies",
            "--signing-key", private_key, "--builder-identity", identity,
        ], env=environment))
        pinned_set = layer_set_reference.rsplit(":", 1)[0] + "@" + published["digest"]
        trust = ["--trust-key", public_key, "--signer-identity", identity, "--require-provenance"]
        inspected = json.loads(command([
            executable, "inspect", pinned_set, "--insecure", *trust,
        ], env=environment))
        assert inspected["evidence"]["attestationDigest"]
        assert inspected["evidence"]["sbomDigest"]
        wrong_key = work / "untrusted-private.pem"
        wrong_public = work / "untrusted-public.pem"
        command(["openssl", "genpkey", "-algorithm", "EC", "-pkeyopt",
                 "ec_paramgen_curve:P-256", "-out", wrong_key])
        command(["openssl", "pkey", "-in", wrong_key, "-pubout", "-out", wrong_public])
        untrusted = subprocess.run([
            str(executable), "inspect", pinned_set, "--insecure", "--trust-key", str(wrong_public),
            "--signer-identity", identity, "--require-provenance",
        ], env=environment, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        assert untrusted.returncode != 0
        assert any(word in untrusted.stderr.lower() for word in ("provenance", "signature", "trusted"))
        managed_reference = f"{args.producer_registry_host}:{port}/fixtures/managed-python:v2"
        managed_layout = work / "managed-python-layout"
        command([
            executable, "build", managed_reference, "--insecure", "--store", managed_layout,
            "--platform", "linux/amd64", "--platform", "linux/arm64",
            "--launch", fixture / "launch.json",
            "--layer-set", pinned_set, *trust,
            "--layer", f"app={fixture / 'src'}:/app/src",
        ], env=environment)
        command([executable, "push", managed_reference, "--store", managed_layout, "--insecure"],
                env=environment)
        managed_index = json.loads(fetch(base + "/fixtures/managed-python/manifests/v2",
                                         "application/vnd.oci.image.index.v1+json"))
        assert len(managed_index["manifests"]) == 2
        assert {member["platform"]["architecture"] for member in managed_index["manifests"]} == {"amd64", "arm64"}
        for managed_descriptor in managed_index["manifests"]:
            content = fetch(base + "/fixtures/managed-python/manifests/" + managed_descriptor["digest"],
                            managed_descriptor["mediaType"])
            assert len(content) == managed_descriptor["size"]
            assert "sha256:" + hashlib.sha256(content).hexdigest() == managed_descriptor["digest"]
            managed_manifest = json.loads(content)
            config_descriptor = managed_manifest["config"]
            content = fetch(base + "/fixtures/managed-python/blobs/" + config_descriptor["digest"])
            assert len(content) == config_descriptor["size"]
            assert "sha256:" + hashlib.sha256(content).hexdigest() == config_descriptor["digest"]
            config = json.loads(content)
            assert config["os"] == "linux" and config["architecture"] == managed_descriptor["platform"]["architecture"]
            dependency_reuse(managed_manifest, config, inspected)
        result = {
            "scope": "registry-producer-integration",
            "registryImage": REGISTRY,
            "imageDigest": "sha256:" + hashlib.sha256(index_bytes).hexdigest(),
            "outcome": "passed",
            "realNode": False,
            "layerSetDigest": published["digest"],
            "trustedProvenanceVerified": True,
            "untrustedProvenanceRejected": True,
            "dependencyLayersReusedByteForByte": True,
        }
        (work / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        print(json.dumps(result))
        print(f"Retained diagnostics: {work}")
    finally:
        try:
            logs = subprocess.run(["docker", "logs", container], check=True, text=True,
                                  stdout=subprocess.PIPE, stderr=subprocess.STDOUT).stdout
            (work / "registry.log").write_text(logs)
        finally:
            final = json.loads(command(["docker", "inspect", container]))[0]
            if final["Config"]["Labels"].get("brewlet.test.invocation") != invocation:
                raise RuntimeError("Registry identity changed; refusing cleanup")
            subprocess.run(["docker", "rm", "--force", container], check=True,
                           stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
