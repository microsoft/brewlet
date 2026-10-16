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
        push = [executable, "push", reference, "--insecure", "--platform", "linux/amd64",
                "--platform", "linux/arm64", "--launch", fixture / "launch.json",
                "--layer", f"dependency={fixture / 'dependencies'}:/app/dependencies",
                "--layer", f"app={fixture / 'src'}:/app/src"]
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
        result = {
            "scope": "registry-producer-integration",
            "registryImage": REGISTRY,
            "imageDigest": "sha256:" + hashlib.sha256(index_bytes).hexdigest(),
            "outcome": "passed",
            "realNode": False,
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
