#!/usr/bin/env python3
"""Verify indexed CI artifacts against every immutable acceptance binding."""

import argparse
import copy
import hashlib
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from acceptance import check, composition_check, qualify
from acceptance.evidence import ROOT, candidate_errors, read_json, working_identity


def validate(root, index, *, expected=None, composition_only=False, baseline_only=False):
    root = Path(root)
    expected = expected or working_identity(root)
    if (not isinstance(index, dict) or type(index.get("schemaVersion")) is not int or index["schemaVersion"] != 1
            or set(index) != {"schemaVersion", "history", "candidate"}):
        return ["unsupported evidence index"]
    candidate = index.get("candidate")
    if (not isinstance(candidate, dict) or not isinstance(candidate.get("identity"), dict)
            or not isinstance(candidate.get("artifacts"), list)):
        return ["no current candidate evidence is indexed"]
    errors = candidate_errors(candidate.get("identity", {}), root, expected)
    from acceptance.evidence_index import artifact_files, positive_id, required_files
    files = {}
    seen = set()
    for artifact in candidate.get("artifacts", []):
        if (not isinstance(artifact, dict) or set(artifact) != {"runId", "artifactId", "name", "files"}
                or not isinstance(artifact["files"], dict)):
            errors.append("invalid indexed artifact")
            continue
        try:
            positive_id(artifact["runId"])
            artifact_id = positive_id(artifact["artifactId"])
            _, names = artifact_files(artifact["name"], candidate["identity"].get("checkout", ""))
        except (ValueError, TypeError):
            errors.append("invalid indexed artifact provenance/scope")
            continue
        if artifact_id in seen or set(artifact["files"]) != names:
            errors.append("duplicate artifact or wrong indexed scope")
        seen.add(artifact_id)
        for name, digest in artifact.get("files", {}).items():
            if not isinstance(name, str) or Path(name).name != name or "\\" in name or not name.endswith(".json"):
                errors.append("invalid public receipt filename")
                continue
            if name in files and files[name] != digest:
                errors.append("conflicting indexed receipt: " + name)
            files[name] = digest
    composition = copy.deepcopy(read_json(root / "acceptance/composition-requirements.json"))
    baseline = copy.deepcopy(read_json(root / "acceptance/requirements.json"))
    if composition_only:
        required = {binding["result"].removeprefix("results/") for binding in composition["checks"].values()}
    elif baseline_only:
        required = {binding["result"].removeprefix("acceptance/results/")
                    for row in baseline["requirements"] + baseline.get("amendments", [])
                    for binding in row["evidence"]}
    else:
        required = required_files()
    if not required <= set(files) or not set(files) <= required_files():
        errors.append("index is missing required current receipt filenames or contains unknown receipts")
    for name, digest in files.items():
        path = root / "acceptance/results" / name
        if not path.is_file() or path.is_symlink():
            errors.append("unavailable public receipt: " + name)
        elif hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            errors.append("receipt checksum mismatch: " + name)
    if errors:
        return errors
    # These are verification requests, not source declarations or rewritten receipts.
    for row in baseline["requirements"] + baseline.get("amendments", []):
        row["status"] = "verified"
        for evidence in row["evidence"]:
            evidence["outcome"] = "passed"
    for binding in composition["checks"].values():
        binding["outcome"] = "passed"
    if not composition_only:
        errors.extend(check.validate(baseline, root, expected=expected))
    if not baseline_only and not composition_only:
        errors.extend(qualify.validate_suites(root / "acceptance/results", root, expected))
    if not baseline_only:
        errors.extend(composition_check.validate(composition, root / "acceptance",
                                                 require_complete=True, expected=expected)[0])
    return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--composition-only", action="store_true")
    args = parser.parse_args()
    try:
        errors = validate(ROOT, read_json(ROOT / "acceptance/evidence-index.json"),
                          composition_only=args.composition_only)
    except (OSError, ValueError) as error:
        errors = [str(error)]
    for error in errors:
        print(error, file=sys.stderr)
    if not errors:
        print("Composition obligations verified from actual indexed receipts" if args.composition_only else
              "All baseline/composition obligations and 22 native suites verified from actual indexed receipts")
    return int(bool(errors))


if __name__ == "__main__":
    raise SystemExit(main())
