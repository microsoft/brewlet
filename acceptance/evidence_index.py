#!/usr/bin/env python3
"""Materialize checksummed public CI evidence; never execute indexed commands."""

import argparse
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import subprocess
import sys
import urllib.error
import urllib.request
import zipfile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from acceptance.evidence import ROOT, candidate_errors, git, input_hashes, read_json, working_identity
from acceptance.qualify import COMPLETE, SUITES
from acceptance.run_check import CHECKS

REPOSITORY = "microsoft/brewlet"
MAX_BYTES = 32 * 1024 * 1024
SHA256 = re.compile(r"[0-9a-f]{64}")


def required_files():
    return {f"candidate-{arch}-{suite}.json" for arch in ("amd64", "arm64") for suite in SUITES} | {
        name + ("-" + arch if check.get("native") else "") + ".json"
        for name, check in CHECKS.items() for arch in ("amd64", "arm64")}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def positive_id(value):
    require(type(value) is int and value > 0, "positive Actions run/artifact id required")
    return value


def index_document(path):
    document = read_json(path)
    require(isinstance(document, dict) and type(document.get("schemaVersion")) is int
            and document["schemaVersion"] == 1,
            "unsupported evidence index")
    require(set(document) == {"schemaVersion", "history", "candidate"}, "unknown evidence index fields")
    return document


def artifact_files(name, source):
    if name == f"checks-contracts-{source}":
        return ".github/workflows/full-validation.yml", {"go-contracts.json", "maven.json"}
    if name == f"checks-documentation-{source}":
        return ".github/workflows/site-pages.yml", {"documentation.json"}
    for arch in ("amd64", "arm64"):
        for group, checks in (("producer", ("fixtures", "registry")),
                             ("kernel", ("native-contracts", "go-vet"))):
            if name == f"checks-{group}-{arch}-{source}":
                return ".github/workflows/full-validation.yml", {f"{check}-{arch}.json" for check in checks}
        for scenario, suites in (("complete", COMPLETE), ("managed", ("managed",)),
                                 ("managed-wasm", ("managed-wasm",))):
            if name == f"e2e-{arch}-{scenario}-{source}":
                return ".github/workflows/e2e.yml", {f"candidate-{arch}-{suite}.json" for suite in suites}
    raise ValueError("artifact is not a required qualification scope for this source")


def unpack(payload, expected_names):
    require(len(payload) <= MAX_BYTES, "public artifact is too large")
    records = {}
    with zipfile.ZipFile(io.BytesIO(payload)) as archive:
        entries = archive.infolist()
        directories = {str(parent) + "/" for name in expected_names
                       for parent in PurePosixPath(name).parents if str(parent) != "."}
        require(len(entries) <= len(expected_names) + len(directories), "too many artifact entries")
        require(sum(item.file_size for item in entries) <= MAX_BYTES, "expanded public artifact is too large")
        seen = set()
        for item in entries:
            require(item.filename not in seen and item.orig_filename == item.filename
                    and not item.flag_bits & 1, "duplicate, noncanonical or encrypted artifact entry")
            seen.add(item.filename)
            if item.is_dir():
                require(item.filename in directories, "unexpected artifact directory")
                continue
            require(item.filename in expected_names and item.filename not in records,
                    "unexpected or duplicate public artifact path")
            require(stat.S_IFMT(item.external_attr >> 16) in (0, stat.S_IFREG), "nonregular artifact entry")
            records[item.filename] = archive.read(item)
    require(set(records) == expected_names, "artifact is missing required public records")
    return records


class HTTPSRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, file, code, message, headers, new_url):
        require(new_url.startswith("https://"), "artifact redirect must use HTTPS")
        return super().redirect_request(request, file, code, message, headers, new_url)


class Actions:
    def __init__(self, token):
        require(bool(token), "read-only GITHUB_TOKEN is required to retrieve Actions artifacts")
        self.token = token
        self.opener = urllib.request.build_opener(HTTPSRedirect())

    def get(self, path, *, binary=False):
        require(path.startswith("/") and "://" not in path, "fixed GitHub API path required")
        request = urllib.request.Request("https://api.github.com/repos/" + REPOSITORY + path,
                                        headers={"Accept": "application/vnd.github+json",
                                                 "X-GitHub-Api-Version": "2022-11-28"})
        # GitHub's signed artifact-storage redirect must never receive this token.
        request.add_unredirected_header("Authorization", "Bearer " + self.token)
        try:
            with self.opener.open(request, timeout=60) as response:
                content = response.read(MAX_BYTES + 1)
        except urllib.error.HTTPError as error:
            raise ValueError(f"GitHub artifact request failed (HTTP {error.code})") from None
        except urllib.error.URLError:
            raise ValueError("GitHub artifact request failed (transport)") from None
        require(len(content) <= MAX_BYTES, "public artifact response is too large")
        return content if binary else json.loads(content)


def download(actions, record, identity):
    run_id, artifact_id = positive_id(record["runId"]), positive_id(record["artifactId"])
    workflow, filenames = artifact_files(record["name"], identity["checkout"])
    run = actions.get(f"/actions/runs/{run_id}")
    events = ("schedule", "workflow_dispatch") if workflow.endswith("full-validation.yml") else ("workflow_dispatch",)
    require(run.get("event") in events and run.get("head_sha") == identity["checkout"]
            and run.get("path") == workflow and run.get("status") == "completed"
            and run.get("conclusion") == "success", "run is not a successful immutable qualification invocation")
    artifact = actions.get(f"/actions/artifacts/{artifact_id}")
    require(artifact.get("name") == record["name"] and artifact.get("expired") is False
            and artifact.get("workflow_run", {}).get("id") == run_id
            and artifact.get("workflow_run", {}).get("head_sha") == identity["checkout"],
            "artifact is expired or does not belong to the indexed source/run")
    return unpack(actions.get(f"/actions/artifacts/{artifact_id}/zip", binary=True), filenames)


def merge_records(records, incoming):
    for name, payload in incoming.items():
        require(name not in records or records[name] == payload, "conflicting public receipt: " + name)
        records[name] = payload


def materialize(root, records):
    destination = Path(root) / "acceptance/results"
    require(not destination.is_symlink(), "receipt directory must not be a symlink")
    destination.mkdir(exist_ok=True)
    require(not any(destination.iterdir()), "refusing to replace existing public receipts")
    for name, payload in records.items():
        require(name in required_files(), "unknown current receipt filename")
        with (destination / name).open("xb") as output:
            output.write(payload)


def candidate_records(actions, candidate, expected):
    require(isinstance(candidate, dict) and isinstance(candidate.get("identity"), dict)
            and isinstance(candidate.get("artifacts"), list), "current evidence index is incomplete")
    errors = candidate_errors(candidate["identity"], ROOT, expected)
    require(not errors, "; ".join(errors))
    records = {}
    for record in candidate["artifacts"]:
        require(isinstance(record, dict) and set(record) == {"runId", "artifactId", "name", "files"},
                "invalid indexed artifact")
        require(isinstance(record["files"], dict), "indexed checksums required")
        incoming = download(actions, record, candidate["identity"])
        require(set(incoming) == set(record["files"]), "indexed artifact file set differs")
        for name, payload in incoming.items():
            digest = record["files"][name]
            require(isinstance(digest, str) and SHA256.fullmatch(digest)
                    and hashlib.sha256(payload).hexdigest() == digest, "receipt checksum mismatch: " + name)
        merge_records(records, incoming)
    require(set(records) == required_files(), "all 34 current qualification receipts are required")
    return records


def export_history(root, history, output):
    require(isinstance(history, dict) and isinstance(history.get("files"), list),
            "historical evidence manifest required")
    revision = history.get("commit", "")
    require(isinstance(revision, str) and re.fullmatch(r"[0-9a-f]{40}", revision), "immutable archive commit required")
    subprocess.run(["git", "-C", str(root), "merge-base", "--is-ancestor", revision, "HEAD"],
                   check=True, capture_output=True)
    payloads = {}
    for record in history["files"]:
        path = record["path"]
        require(isinstance(path, str), "historical path must be a string")
        parsed = PurePosixPath(path)
        require(path.startswith("acceptance/results/")
                and path.endswith(".json") and ".." not in parsed.parts and "\\" not in path
                and str(parsed) == path and path not in payloads, "invalid historical public path")
        payload = git(root, "show", revision + ":" + path)
        require(len(payload) == record["size"] and hashlib.sha256(payload).hexdigest() == record["sha256"],
                "historical public bytes differ: " + path)
        payloads[path] = payload
    tracked = git(root, "ls-tree", "-r", "--name-only", revision, "--", "acceptance/results").decode().splitlines()
    require(set(payloads) == {path for path in tracked if path.endswith(".json")},
            "historical index must preserve every public receipt at its pinned revision")
    output = Path(output)
    output.mkdir(parents=True, exist_ok=False)
    for path, payload in payloads.items():
        target = output / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(payload)
    (output / "index.json").write_text(json.dumps(history, indent=2) + "\n")
    return len(payloads)


def record_history(actions, document, run_id):
    run_id = positive_id(run_id)
    run = actions.get(f"/actions/runs/{run_id}")
    source = run.get("head_sha", "")
    require(run.get("path") == ".github/workflows/full-validation.yml"
            and isinstance(source, str) and re.fullmatch(r"[0-9a-f]{40}", source),
            "historical archive must belong to the full validation workflow")
    listing = actions.get(f"/actions/runs/{run_id}/artifacts?per_page=100")
    matches = [item for item in listing["artifacts"]
               if item["name"] == "historical-acceptance-receipts-" + source and item["expired"] is False]
    require(len(matches) == 1, "one unexpired historical archive is required")
    artifact_id = positive_id(matches[0]["id"])
    history = document["history"]
    expected = {row["path"] for row in history["files"]}
    records = unpack(actions.get(f"/actions/artifacts/{artifact_id}/zip", binary=True), expected | {"index.json"})
    original = json.loads(records.pop("index.json"))
    require(original.get("commit") == history["commit"] and original.get("files") == history["files"],
            "historical archive manifest differs")
    for row in history["files"]:
        payload = records[row["path"]]
        require(len(payload) == row["size"] and hashlib.sha256(payload).hexdigest() == row["sha256"],
                "historical archive checksum mismatch: " + row["path"])
    return dict(document, history=dict(history, artifactRunId=run_id, artifactId=artifact_id))


def record_candidate(actions, root, document, runs, expected):
    records, artifacts = {}, []
    identity = None
    for run_id in runs:
        run = actions.get(f"/actions/runs/{positive_id(run_id)}")
        source = run.get("head_sha", "")
        require(isinstance(source, str) and re.fullmatch(r"[0-9a-f]{40}", source), "immutable run source required")
        observed = {"checkout": source, **input_hashes(root, source)}
        errors = candidate_errors(observed, root, expected)
        require(not errors, "; ".join(errors))
        require(identity is None or identity == observed, "reviewed runs must use one frozen input identity")
        identity = observed
        listing = actions.get(f"/actions/runs/{positive_id(run_id)}/artifacts?per_page=100")
        for artifact in listing["artifacts"]:
            try:
                artifact_files(artifact["name"], identity["checkout"])
            except ValueError:
                continue
            record = {"runId": run_id, "artifactId": artifact["id"], "name": artifact["name"]}
            incoming = download(actions, record, identity)
            record["files"] = {name: hashlib.sha256(payload).hexdigest() for name, payload in incoming.items()}
            artifacts.append(record)
            merge_records(records, incoming)
    require(set(records) == required_files(), "reviewed runs do not supply all 34 qualification receipts")
    require(working_identity(root) == expected, "source changed while retrieving evidence")
    materialize(root, records)
    candidate = {"identity": identity, "artifacts": artifacts}
    updated = dict(document, candidate=candidate)
    from acceptance.verify_evidence import validate
    errors = validate(root, updated, expected=expected)
    require(not errors, "\n".join(errors))
    return updated


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("fetch")
    record = commands.add_parser("record-candidate", help="only after reviewing successful workflow runs")
    record.add_argument("--run", type=int, action="append", required=True)
    history = commands.add_parser("export-history")
    history.add_argument("--output", type=Path, required=True)
    history.add_argument("--if-needed", action="store_true")
    record_archive = commands.add_parser("record-history", help="record only after downloading and verifying every original byte")
    record_archive.add_argument("--run", type=int, required=True)
    args = parser.parse_args()
    try:
        path = ROOT / "acceptance/evidence-index.json"
        document = index_document(path)
        if args.command == "export-history":
            needed = not (args.if_needed and document["history"].get("artifactRunId") is not None)
            if needed:
                print(f"Archived {export_history(ROOT, document['history'], args.output)} byte-verified original records")
            else:
                positive_id(document["history"]["artifactRunId"])
                positive_id(document["history"].get("artifactId"))
                print("Historical records already indexed as CI artifacts; no repeated archive")
            if os.environ.get("GITHUB_OUTPUT"):
                with Path(os.environ["GITHUB_OUTPUT"]).open("a") as output:
                    output.write("written=" + ("true" if needed else "false") + "\n")
            return 0
        expected = working_identity()
        actions = Actions(os.environ.get("GITHUB_TOKEN"))
        if args.command == "fetch":
            materialize(ROOT, candidate_records(actions, document["candidate"], expected))
            print("Retrieved all 34 indexed current receipts; strict acceptance must still pass")
        elif args.command == "record-candidate":
            updated = record_candidate(actions, ROOT, document, args.run, expected)
            path.write_text(json.dumps(updated, indent=2) + "\n")
            print("Recorded reviewed evidence index; commit this evidence-only change before strict acceptance")
        else:
            path.write_text(json.dumps(record_history(actions, document, args.run), indent=2) + "\n")
            print("Recorded byte-verified historical archive; commit this evidence-only index change")
        return 0
    except (OSError, ValueError, KeyError, TypeError, zipfile.BadZipFile, subprocess.CalledProcessError) as error:
        print("evidence operation failed: " + str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
