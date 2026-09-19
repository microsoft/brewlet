#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Strict evidence checks and native OCI/DSSE fixtures for live admission."""

import base64
import copy
import hashlib
import json
import re
import subprocess
import urllib.error
import urllib.parse
import urllib.request
import uuid


ARTIFACT = "application/vnd.brewlet.attestation.v1+json"
DSSE = "application/vnd.dsse.envelope.v1+json"
MANIFEST = "application/vnd.oci.image.manifest.v1+json"
INDEX = "application/vnd.oci.image.index.v1+json"
VERIFIER = "brewlet-managed-dependencies"
BUILDER = "https://live.brewlet.invalid/application-builder"


def encoded(value):
    return json.dumps(value, separators=(",", ":"), sort_keys=True).encode()


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def assert_admission(result, allowed):
    """A malformed API request or an unrelated webhook error is never a pass."""
    text = result.stdout + result.stderr
    denied = (result.returncode != 0
              and 'admission webhook "validation.gatekeeper.sh" denied the request' in text
              and "[brewlet-managed-dependencies]" in text
              and "Brewlet admission:" in text)
    if allowed:
        if result.returncode:
            raise AssertionError(f"expected admission, got API failure:\n{text}")
    elif not denied:
        raise AssertionError(f"expected identifiable Brewlet Gatekeeper denial:\n{text}")
    return {"allowed": allowed, "returncode": result.returncode, "response": text}


def extract_result(response, image):
    if response.get("response", {}).get("systemError"):
        raise AssertionError(f"Ratify provider error: {response}")
    items = response.get("response", {}).get("items", [])
    matching = [item for item in items if item.get("key") == image]
    if len(matching) != 1 or matching[0].get("error"):
        raise AssertionError(f"expected one real report for {image}: {response}")
    result = matching[0].get("value")
    if not isinstance(result, dict) or not isinstance(result.get("isSuccess"), bool):
        raise AssertionError(f"missing explicit Ratify decision: {response}")
    return result


def assert_candidates(result, expected, allowed, other=None, subject_digest=None):
    """Match every candidate digest to its own named-verifier result."""
    if result.get("isSuccess") is not allowed:
        raise AssertionError(f"wrong Ratify decision: {result}")
    reports = {}
    for report in result.get("verifierReports", []):
        if report.get("artifactType") != ARTIFACT:
            continue
        reference = report.get("referenceDigest")
        if reference in reports:
            raise AssertionError(f"duplicate candidate report: {reference}")
        reports[reference] = report
    if set(reports) != set(expected):
        raise AssertionError(
            f"candidate discovery/report mismatch: expected {expected}, got {reports}")
    for reference, success in expected.items():
        candidates = [vr for vr in reports[reference].get("verifierReports", [])
                      if vr.get("verifierName") == VERIFIER]
        if len(candidates) != 1 or candidates[0].get("isSuccess", False) is not success:
            raise AssertionError(f"wrong Brewlet candidate result: {reports[reference]}")
        if success:
            bound = candidates[0].get("extensions", {}).get("finalImageDigest")
            if not bound or (subject_digest is not None and bound != subject_digest):
                raise AssertionError(f"success lacks bound predicate: {candidates[0]}")
        elif not candidates[0].get("message"):
            raise AssertionError(f"failure lacks verifier message: {candidates[0]}")
        if other is not None:
            unrelated = [vr for vr in reports[reference].get("verifierReports", [])
                         if vr.get("verifierName") == "admission-other"]
            if len(unrelated) != 1 or unrelated[0].get("isSuccess", False) is not other:
                raise AssertionError(f"other verifier not actually run: {reports[reference]}")
    return reports


def assert_discoverable(index, expected):
    candidates = [entry["digest"] for entry in index.get("manifests", [])
                  if entry.get("artifactType") == ARTIFACT]
    if len(candidates) != len(set(candidates)) or set(candidates) != set(expected):
        raise AssertionError(f"native OCI referrers mismatch: {candidates} != {expected}")


def assert_rejection_reasons(reports, reasons):
    for reference, expected in reasons.items():
        report = reports[reference]
        actual = [item.get("errorReason", "") for item in report["verifierReports"]
                  if item.get("verifierName") == VERIFIER]
        if len(actual) != 1 or expected not in actual[0]:
            raise AssertionError(
                f"candidate {reference} failed for the wrong reason: {actual}; expected {expected}")


class Registry:
    """HTTP is restricted to the invocation-owned loopback registry."""

    def __init__(self, fixture):
        if not re.fullmatch(r"localhost:\d+", fixture.registry):
            raise ValueError("fixture registry must be a loopback endpoint")
        self.fixture = fixture
        self.base = "http://" + fixture.registry

    def request(self, method, path, body=None, content_type=None):
        url = urllib.parse.urljoin(self.base, path)
        if urllib.parse.urlsplit(url).netloc != self.fixture.registry:
            raise ValueError(f"registry redirect escaped fixture: {url}")
        headers = {"Accept": ", ".join([INDEX, MANIFEST,
                   "application/vnd.docker.distribution.manifest.list.v2+json",
                   "application/vnd.docker.distribution.manifest.v2+json"])}
        if content_type:
            headers["Content-Type"] = content_type
        with urllib.request.urlopen(urllib.request.Request(
                url, data=body, headers=headers, method=method), timeout=30) as response:
            return response.read(), response.headers

    def manifest(self, repository, reference):
        raw, _ = self.request("GET", f"/v2/{repository}/manifests/{reference}")
        return raw, json.loads(raw)

    def blob(self, repository, reference):
        return self.request("GET", f"/v2/{repository}/blobs/{reference}")[0]

    def put_blob(self, repository, raw):
        reference = digest(raw)
        _, headers = self.request("POST", f"/v2/{repository}/blobs/uploads/", b"")
        location = headers["Location"]
        parts = urllib.parse.urlsplit(location)
        # Distribution uses its container-facing name in some Location headers.
        # Only the path/query are used; never follow a registry-supplied host.
        path = urllib.parse.urlunsplit(("", "", parts.path, parts.query, ""))
        path += ("&" if parts.query else "?") + urllib.parse.urlencode({"digest": reference})
        self.request("PUT", path, raw, "application/octet-stream")
        return reference

    def put_manifest(self, repository, tag, document):
        raw = encoded(document)
        self.request("PUT", f"/v2/{repository}/manifests/{tag}", raw,
                     document.get("mediaType", MANIFEST))
        return {"mediaType": document.get("mediaType", MANIFEST),
                "digest": digest(raw), "size": len(raw)}

    def referrers(self, repository, subject):
        try:
            raw, _ = self.request("GET", f"/v2/{repository}/referrers/{subject}")
        except urllib.error.HTTPError as error:
            if error.code == 404:
                raise RuntimeError(
                    "Scenario A requires the native OCI 1.1 Referrers API; registry "
                    f"/v2/{repository}/referrers/{subject} returned 404. Distribution "
                    "3.0.0 does not provide this API. Brewlet fallback tags cannot "
                    "substitute for native Ratify discovery.") from error
            raise
        return json.loads(raw)

    def clone_subject(self, repository, reference, label):
        """Keep runnable bytes intact, give every failure a new image digest."""
        _, manifest = self.manifest(repository, reference)
        manifest.setdefault("annotations", {})["live.brewlet.invalid/case"] = (
            label + "-" + uuid.uuid4().hex)
        return self.put_manifest(repository, label, manifest)

    def publish(self, repository, subject, envelope, label):
        blob = self.put_blob(repository, envelope)
        config = b"{}"
        config_digest = self.put_blob(repository, config)
        return self.put_manifest(repository, "evidence-" + label, {
            "schemaVersion": 2, "mediaType": MANIFEST, "artifactType": ARTIFACT,
            "subject": subject,
            "config": {"mediaType": "application/vnd.oci.empty.v1+json",
                       "digest": config_digest, "size": len(config)},
            "layers": [{"mediaType": DSSE, "digest": blob, "size": len(envelope)}],
        })["digest"]

    def delete_candidate(self, repository, reference):
        self.request("DELETE", f"/v2/{repository}/manifests/{reference}")


def signed_envelope(statement, private_key):
    payload_type = b"application/vnd.in-toto+json"
    payload = encoded(statement)
    pae = (b"DSSEv1 " + str(len(payload_type)).encode() + b" " + payload_type
           + b" " + str(len(payload)).encode() + b" " + payload)
    signature = subprocess.run(
        ["openssl", "dgst", "-sha256", "-sign", str(private_key)],
        input=pae, capture_output=True, check=True, timeout=30).stdout
    public = subprocess.run(
        ["openssl", "pkey", "-in", str(private_key), "-pubout", "-outform", "DER"],
        capture_output=True, check=True, timeout=30).stdout
    return encoded({"payloadType": payload_type.decode(),
                    "payload": base64.b64encode(payload).decode(),
                    "signatures": [{"keyid": digest(public),
                                    "sig": base64.b64encode(signature).decode()}]})


def variant_statement(original, subject, kind):
    statement = copy.deepcopy(original)
    statement["subject"][0]["digest"]["sha256"] = subject["digest"].split(":")[1]
    statement["predicate"]["finalImageDigest"] = subject["digest"]
    if kind == "wrong-builder":
        statement["predicate"]["builderIdentity"] = BUILDER + "/untrusted"
    elif kind == "wrong-subject":
        statement["subject"][0]["digest"]["sha256"] = "0" * 64
        statement["predicate"]["finalImageDigest"] = "sha256:" + "0" * 64
    elif kind == "incomplete":
        del statement["predicate"]["applicationJarDigest"]
    return statement
