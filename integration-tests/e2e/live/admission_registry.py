#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Invocation-owned native OCI 1.1 registry for admission (not fallback tags)."""

import json
import os

from common import OWNER_LABEL, wait


ZOT_IMAGE = ("ghcr.io/project-zot/zot:v2.1.8@sha256:"
             "cd2aea942f428630bcb4190542be6abd35e14177aab84fc7ccad0dca8ecb363d")


def native_registry(fixture):
    """Replace only the empty, identity-checked fixture registry before publishing."""
    old = fixture.own_container(fixture.registry_name)
    config = fixture.private / "admission-zot.json"
    credentials = fixture.private / "admission-htpasswd"
    storage = fixture.private / "admission-zot-data"
    storage.mkdir(mode=0o700)
    credentials.write_text("")
    credentials.chmod(0o600)
    document = {
        "distSpecVersion": "1.1.1",
        "storage": {"rootDirectory": "/var/lib/zot", "gc": False, "dedupe": False},
        "http": {"address": "0.0.0.0", "port": "5000", "realm": "brewlet-live-fixture"},
        "log": {"level": "debug"},
    }
    config.write_text(json.dumps(document))
    config.chmod(0o600)
    port = fixture.registry.split(":")[1]
    fixture.run(["docker", "rm", "-f", "--volumes", old["Id"]])
    fixture.registry_id = None
    try:
        fixture.run([
            "docker", "create", "--platform", "linux/" + fixture.arch,
            "--user", f"{os.getuid()}:{os.getgid()}",
            "--name", fixture.registry_name, "--label", f"{OWNER_LABEL}={fixture.name}",
            "--network", fixture.name, "--cpus", "0.5", "--memory", "256m",
            "-p", f"127.0.0.1:{port}:5000",
            "--mount", f"type=bind,src={config},dst=/etc/zot/config.json,readonly",
            "--mount", f"type=bind,src={credentials},dst=/etc/zot/htpasswd,readonly",
            "--mount", f"type=bind,src={storage},dst=/var/lib/zot",
            ZOT_IMAGE, "serve", "/etc/zot/config.json"], timeout=300)
    finally:
        result = fixture.run(["docker", "inspect", fixture.registry_name], check=False)
        if result.returncode == 0:
            info = json.loads(result.stdout)[0]
            if info["Config"].get("Labels", {}).get(OWNER_LABEL) != fixture.name:
                raise RuntimeError("Refusing to adopt a foreign native registry")
            fixture.registry_id = info["Id"]
    fixture.run(["docker", "start", fixture.registry_id])
    info = fixture.own_container(fixture.registry_name)
    fixture.registry_ip = info["NetworkSettings"]["Networks"][fixture.name]["IPAddress"]
    wait("native-referrers registry /v2/", lambda: fixture.run(
        ["curl", "-fsS", "--max-time", "3", f"http://{fixture.registry}/v2/"],
        check=False).returncode == 0, timeout=60, interval=1)
    fixture.save("registry-identity.json", {
        "id": fixture.registry_id, "name": fixture.registry_name, "mounts": info["Mounts"],
        "image": ZOT_IMAGE, "replacedEmptyDistribution": old["Id"],
    })
    fixture.record("admission-native-registry", {
        "image": ZOT_IMAGE, "nativeOCIReferrers": True,
        "containerUser": f"{os.getuid()}:{os.getgid()}",
        "reason": "Distribution 3.0.0 returns 404 for native referrers; no fallback substitution",
        "host": fixture.registry, "containerHost": fixture.registry_internal,
        "plainHTTP": "only this invocation-owned registry", "garbageCollection": False,
    })
    return config, credentials, storage
