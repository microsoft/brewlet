#!/usr/bin/env python3
"""Run the native Linux materialization contracts without skipped kernel tests."""

import json
from pathlib import Path
import platform
import re
import shlex
import subprocess
import tempfile
import uuid


ROOT = Path(__file__).resolve().parents[1]
OWNER = "brewlet.test.invocation"
SUITES = {
    "artifact": ("./pkg/artifact", "/src/pkg/artifact",
                 ("DAC_OVERRIDE",), ("/src/pkg/artifact:rw,mode=755",)),
    "shim": ("./shim/cmd/containerd-shim-brewlet-v2", "/src/shim/cmd/containerd-shim-brewlet-v2",
             ("DAC_OVERRIDE",), ("/var/lib/brewlet/layers:rw,exec,size=64m",
                                "/src/shim/cmd/containerd-shim-brewlet-v2:rw,mode=755")),
    "node": ("./pkg/node", "/work", ("SETFCAP",), ("/work",)),
    "provisioner": ("./provisioner", "/work", (), ("/work",)),
    "worker": ("./cmd/brewlet-node", "/work", (), ("/work",)),
}


def native_architecture():
    architecture = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    if platform.system() != "Linux" or architecture is None:
        raise ValueError("native Linux amd64/arm64 execution required, not emulation")
    return architecture


def run(command, *, timeout=900):
    print("+ " + shlex.join(map(str, command)), flush=True)
    result = subprocess.run(list(map(str, command)), cwd=ROOT, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
    print(result.stdout, end="", flush=True)
    result.check_returncode()
    return result.stdout


def validate_output(output):
    if re.search(r"--- SKIP:", output) or not re.search(r"--- PASS:", output):
        raise ValueError("native contract suite skipped required tests or executed no passing tests")


def suite_command(name, work, tag, architecture, invocation):
    _, directory, capabilities, mounts = SUITES[name]
    command = ["docker", "run", "--rm", "--cidfile", str(work / (name + ".cid")),
               "--label", OWNER + "=" + invocation, "--platform", "linux/" + architecture,
               "--network", "none", "--user", "0:0", "--cap-drop", "ALL",
               "--cap-add", "SYS_ADMIN", "--cap-add", "MKNOD"]
    for capability in capabilities:
        command += ["--cap-add", capability]
    command += ["--security-opt", "no-new-privileges", "--security-opt", "apparmor=unconfined",
                "--read-only", "--tmpfs", "/tmp"]
    for mount in mounts:
        command += ["--tmpfs", mount]
    command += ["--mount", f"type=bind,source={ROOT},target=/src,readonly",
                "--mount", f"type=bind,source={work},target=/tests,readonly",
                "--workdir", directory, "--entrypoint", "/tests/" + name + ".test", tag, "-test.v"]
    return command


def cleanup_container(path, invocation):
    if not path.exists():
        return
    identifier = path.read_text().strip()
    if not re.fullmatch(r"[0-9a-f]{64}", identifier):
        raise ValueError("invalid recorded native contract container identity")
    present = run(["docker", "ps", "--all", "--quiet", "--no-trunc", "--filter", "id=" + identifier])
    if not present.strip():
        return
    info = json.loads(run(["docker", "inspect", identifier]))[0]
    if info["Id"] != identifier or info["Config"].get("Labels", {}).get(OWNER) != invocation:
        raise ValueError("native contract container ownership changed; refusing cleanup")
    run(["docker", "rm", "--force", identifier])


def main():
    architecture = native_architecture()
    if run(["go", "env", "GOOS", "GOARCH"]).split() != ["linux", architecture]:
        raise ValueError("contract test compilation must target the native Linux runner")
    docker_arch = run(["docker", "info", "--format", "{{.Architecture}}"]).strip()
    if {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64", "amd64": "amd64"}.get(docker_arch) != architecture:
        raise ValueError("Docker daemon must have the runner's native architecture")
    invocation = uuid.uuid4().hex
    tag = "brewlet-overlay-contract:" + invocation
    image = None
    with tempfile.TemporaryDirectory(prefix="brewlet-native-contracts-") as directory:
        work = Path(directory)
        work.chmod(0o755)
        try:
            for name, (package, _, _, _) in SUITES.items():
                run(["go", "test", "-c", "-mod=readonly", "-o", work / (name + ".test"), package])
            run(["docker", "build", "--load", "--platform", "linux/" + architecture,
                 "--label", OWNER + "=" + invocation,
                 "--file", "integration-tests/fixtures/runtimes/python.Dockerfile",
                 "--tag", tag, "integration-tests/fixtures/runtimes"])
            image = json.loads(run(["docker", "image", "inspect", tag]))[0]["Id"]
            for name in SUITES:
                try:
                    validate_output(run(suite_command(name, work, tag, architecture, invocation),
                                        timeout=1800))
                finally:
                    cleanup_container(work / (name + ".cid"), invocation)
        finally:
            if image is not None:
                info = json.loads(run(["docker", "image", "inspect", tag]))[0]
                if info["Id"] != image or info["Config"].get("Labels", {}).get(OWNER) != invocation:
                    raise ValueError("native contract image tag ownership changed; refusing cleanup")
                run(["docker", "image", "rm", tag])
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
