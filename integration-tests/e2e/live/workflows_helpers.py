#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Pure, offline-testable helpers for the live workflow scenario (workflows.py)."""

import base64
import hashlib
import json
import os
from pathlib import Path
import re
import socket
import threading
from urllib.error import HTTPError
from urllib.request import Request, urlopen
from xml.sax.saxutils import escape

DIGEST = re.compile(r"sha256:[a-f0-9]{64}")
INDEX = "application/vnd.oci.image.index.v1+json"
MANIFEST = "application/vnd.oci.image.manifest.v1+json"
ACCEPT = ", ".join((INDEX, MANIFEST, "application/vnd.docker.distribution.manifest.list.v2+json",
                    "application/vnd.docker.distribution.manifest.v2+json"))
REDACTED = "[REDACTED]"
SECRET_ENV = re.compile(r"(?i)(password|token|secret|auth)")


def redact_secrets(text, secrets):
    """Replace every known secret (longest first) so prefixes never leak."""
    text = str(text)
    for secret in sorted((s for s in secrets if s), key=len, reverse=True):
        text = text.replace(secret, REDACTED)
    return text


def sanitized_command(argv, env_overrides, secrets):
    """Command record: argv with secrets removed and only env names for secret-like keys."""
    env = {}
    for key, value in sorted((env_overrides or {}).items()):
        env[key] = REDACTED if SECRET_ENV.search(key) else redact_secrets(value, secrets)
    return {"argv": [redact_secrets(arg, secrets) for arg in argv], "env": env}


def assert_bounded(description, elapsed, limit, tolerance):
    """Fail unless a bounded operation finished within its limit plus documented tolerance."""
    if elapsed > limit + tolerance:
        raise AssertionError(f"{description} took {elapsed:.1f}s; bound is {limit}s + {tolerance}s tolerance")
    return {"elapsedSeconds": round(elapsed, 3), "limitSeconds": limit, "toleranceSeconds": tolerance}


def log_seconds(output, needle):
    """Seconds since midnight of the first HH:mm:ss.SSS-stamped line containing needle."""
    for line in output.splitlines():
        match = re.match(r"(\d{2}):(\d{2}):(\d{2})\.(\d{3}) ", line)
        if match and needle in line:
            hours, minutes, seconds, millis = (int(v) for v in match.groups())
            return hours * 3600 + minutes * 60 + seconds + millis / 1000
    raise AssertionError(f"no timestamped log line contains {needle!r}")


def push_stdout(stdout):
    """Digest and deploy image reported by `brewlet push` on stdout."""
    index = re.search(r"^\s*index: (sha256:[a-f0-9]{64})\b", stdout, re.M)
    deploy = re.search(r"^\s*deploy image: (\S+)$", stdout, re.M)
    if not index:
        raise AssertionError("brewlet push stdout has no index digest")
    return {"digest": index[1], "deployImage": deploy[1] if deploy else None}


def validate_handoff(handoff, registry, repository, tag, digest):
    """A push.json handoff must name the requested tag and pin the published digest."""
    expected = {"image": f"{registry}/{repository}:{tag}", "digest": digest,
                "deployImage": f"{registry}/{repository}@{digest}", "format": "image"}
    if not DIGEST.fullmatch(str(handoff.get("digest", ""))):
        raise AssertionError(f"push handoff has no valid digest: {handoff}")
    if handoff != expected:
        raise AssertionError(f"push handoff {handoff} does not match {expected}")
    return expected


def yaml_top_level(text):
    """Top-level scalar keys of a simple YAML document (enough for report checks)."""
    values = {}
    for line in text.splitlines():
        match = re.fullmatch(r"([A-Za-z][A-Za-z0-9]*):(?: (.*))?", line)
        if match:
            values[match[1]] = (match[2] or "").strip().strip('"')
    return values


def parse_json_stream(text):
    """Decode concatenated JSON documents such as `kubectl get --watch -o json` output."""
    decoder = json.JSONDecoder()
    position, documents = 0, []
    while True:
        while position < len(text) and text[position].isspace():
            position += 1
        if position >= len(text):
            return documents
        document, position = decoder.raw_decode(text, position)
        documents.append(document)


def condition(obj, kind):
    return next((c for c in obj.get("status", {}).get("conditions", []) if c.get("type") == kind), None)


def cleanup_sequence(events, uid):
    """Prove production cleanup order from watch events of one NodeProfile UID.

    The operator records CleanupComplete=True/CleanupSucceeded for the current
    generation while the cleanup finalizer and node claims (status.targets) are
    still present, and only then lets the object disappear.
    """
    checkpoint, deleted, timeline = None, None, []
    for index, event in enumerate(events):
        obj = event.get("object", {})
        meta = obj.get("metadata", {})
        if meta.get("uid") != uid:
            continue
        done = condition(obj, "CleanupComplete")
        ready = condition(obj, "Ready") or {}
        timeline.append({"type": event.get("type"), "resourceVersion": meta.get("resourceVersion"),
                         "deleting": bool(meta.get("deletionTimestamp")),
                         "finalizers": meta.get("finalizers", []),
                         "targets": len(obj.get("status", {}).get("targets", [])),
                         "ready": ready.get("reason"),
                         "cleanupComplete": done and done.get("reason")})
        if (checkpoint is None and done and done.get("status") == "True" and
                done.get("reason") == "CleanupSucceeded" and
                done.get("observedGeneration") == meta.get("generation") and
                meta.get("deletionTimestamp") and
                "node.brewlet.sh/cleanup" in meta.get("finalizers", []) and
                obj.get("status", {}).get("targets")):
            checkpoint = index
        if event.get("type") == "DELETED":
            deleted = index
    if checkpoint is None:
        raise AssertionError("no CleanupComplete=True/CleanupSucceeded checkpoint with retained claims")
    if deleted is None or deleted < checkpoint:
        raise AssertionError("profile was not deleted after its cleanup checkpoint")
    return timeline


def basic_auth(username, password):
    return "Basic " + base64.b64encode(f"{username}:{password}".encode()).decode()


def fetch(registry, path, *, accept=None, auth=None, method="GET"):
    """Fetch from a loopback registry; returns (status, headers, body)."""
    headers = {}
    if accept:
        headers["Accept"] = accept
    if auth:
        headers["Authorization"] = basic_auth(*auth)
    request = Request(f"http://{registry}{path}", headers=headers, method=method)
    try:
        with urlopen(request, timeout=30) as response:
            return response.status, dict(response.headers), response.read()
    except HTTPError as error:
        return error.code, dict(error.headers), error.read()


def manifest(registry, repository, reference, auth=None):
    status, headers, body = fetch(registry, f"/v2/{repository}/manifests/{reference}",
                                  accept=ACCEPT, auth=auth)
    if status != 200:
        return None
    digest = "sha256:" + hashlib.sha256(body).hexdigest()
    if DIGEST.fullmatch(reference) and digest != reference:
        raise AssertionError(f"registry returned mismatched manifest for {reference}")
    if headers.get("Docker-Content-Digest", digest) != digest:
        raise AssertionError("registry digest header does not match manifest bytes")
    return digest, json.loads(body), len(body)


def verify_blob(registry, repository, descriptor, auth=None):
    status, _headers, body = fetch(registry, f"/v2/{repository}/blobs/{descriptor['digest']}", auth=auth)
    if status != 200:
        raise AssertionError(f"blob {descriptor['digest']} missing ({status})")
    if ("sha256:" + hashlib.sha256(body).hexdigest() != descriptor["digest"] or
            len(body) != descriptor["size"]):
        raise AssertionError(f"blob {descriptor['digest']} does not match its descriptor")


def verify_published(registry, repository, reference, auth=None):
    """Fetch an index/manifest and prove every child manifest and blob matches its descriptor."""
    found = manifest(registry, repository, reference, auth)
    if not found:
        raise AssertionError(f"{registry}/{repository}:{reference} is not published")
    digest, root, _size = found
    children = []
    descriptors = root.get("manifests") if root.get("mediaType") == INDEX else None
    for descriptor in descriptors or [{"digest": digest, "platform": {}}]:
        child_digest, child, size = (manifest(registry, repository, descriptor["digest"], auth)
                                     or (None, None, None))
        if child_digest != descriptor["digest"] or descriptor.get("size", size) != size:
            raise AssertionError(f"child manifest {descriptor['digest']} missing or mis-sized")
        for blob in [child["config"], *child["layers"]]:
            verify_blob(registry, repository, blob, auth)
        platform = descriptor.get("platform", {})
        children.append({"digest": child_digest, "os": platform.get("os"),
                         "architecture": platform.get("architecture"),
                         "blobs": 1 + len(child["layers"])})
    return {"digest": digest, "mediaType": root.get("mediaType"), "manifests": children}


def derive_kubeconfig(base, *, context, namespace=None, token=None, decoy_server=None):
    """A private kubeconfig derived from a minified flattened base (JSON form).

    The real cluster becomes context `context`; with decoy_server the
    current-context points at an unusable decoy so only an explicit context can
    reach the cluster. A token replaces the user's client credentials.
    """
    cluster = base["clusters"][0]["cluster"]
    user = {"token": token} if token else base["users"][0]["user"]
    target = {"cluster": "wf-cluster", "user": "wf-user"}
    if namespace:
        target["namespace"] = namespace
    config = {"apiVersion": "v1", "kind": "Config", "preferences": {},
              "clusters": [{"name": "wf-cluster", "cluster": cluster}],
              "users": [{"name": "wf-user", "user": user}],
              "contexts": [{"name": context, "context": target}],
              "current-context": context}
    if decoy_server:
        config["clusters"].append({"name": "decoy", "cluster": {
            "server": decoy_server, "insecure-skip-tls-verify": True}})
        config["users"].append({"name": "decoy", "user": {"token": "decoy"}})
        config["contexts"].append({"name": "decoy", "context": {
            "cluster": "decoy", "user": "decoy", "namespace": "decoy"}})
        config["current-context"] = "decoy"
    return config


def maven_settings(server_id, username, password):
    return ("<settings><servers><server>"
            f"<id>{escape(server_id)}</id><username>{escape(username)}</username>"
            f"<password>{escape(password)}</password>"
            "</server></servers></settings>\n")


def settings_security(master):
    return f"<settingsSecurity><master>{escape(master)}</master></settingsSecurity>\n"


def encrypted(value):
    """Maven's decorated cipher text, e.g. {base64}."""
    match = re.search(r"\{[A-Za-z0-9+/=]+\}", value)
    if not match:
        raise AssertionError("Maven did not print an encrypted value")
    return match[0]


def stalled_kubectl(state):
    """A test-only kubectl that never exits and spawns a descendant."""
    return f"""#!/bin/sh
# Injected process failure for the brewlet:deploy kubectl deadline. Not a live rollout.
date +%s.%N > '{state}/started'
echo "$$" > '{state}/kubectl.pid'
sleep 600 &
echo "$!" > '{state}/child.pid'
echo "stalled test kubectl invoked" >&2
wait
"""


def process_alive(pid, expected):
    """True if pid is a live (non-zombie) process whose cmdline contains expected."""
    proc = Path(f"/proc/{pid}")
    try:
        state = (proc / "stat").read_text().rsplit(")", 1)[1].split()[0]
        cmdline = (proc / "cmdline").read_bytes().replace(b"\0", b" ").decode(errors="replace")
    except (FileNotFoundError, ProcessLookupError, IndexError):
        return False
    return state != "Z" and expected in cmdline


class StalledEndpoint:
    """A loopback TCP endpoint that accepts connections and never answers."""

    def __init__(self):
        self.server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.server.bind(("127.0.0.1", 0))
        self.server.listen(16)
        self.port = self.server.getsockname()[1]
        self.connections = []
        self.closed = threading.Event()
        self.thread = threading.Thread(target=self._accept, daemon=True)
        self.thread.start()

    def _accept(self):
        while not self.closed.is_set():
            try:
                connection, _ = self.server.accept()
            except OSError:
                return
            self.connections.append(connection)

    def close(self):
        self.closed.set()
        for connection in [self.server, *self.connections]:
            try:
                connection.close()
            except OSError:
                pass
        self.thread.join(timeout=5)

    def __enter__(self):
        return self

    def __exit__(self, *_exc):
        self.close()
        return False


def unused_port():
    """A loopback port with no listener (closed immediately after binding)."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
        probe.bind(("127.0.0.1", 0))
        return probe.getsockname()[1]


def write_private(path, text):
    """Write private material with owner-only permissions."""
    path = Path(path)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "w") as handle:
        handle.write(text)
    return path
