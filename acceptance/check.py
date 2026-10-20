#!/usr/bin/env python3
"""Validate the acceptance ledger; --require-complete is the release gate."""

import argparse
import json
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from acceptance.qualification import load_matrix


REAL_NODE_REQUIRED = {
    "PROF-03", "PROF-05", "LEASE-02", "RUN-07", "RUN-08", "RUN-09", "RUN-10",
    "PROC-03", "E2E-02", "E2E-03", "E2E-05", "E2E-06", "E2E-07",
    "E2E-08", "LANG-02",
    "UP-05",
    "WASM-01", "WASM-02", "WASM-03",
    "FS-03", "FS-04",
}


def validate(document):
    requirements = document["requirements"] + document.get("amendments", [])
    seen = set()
    errors = []
    for item in requirements:
        identifier = item["id"]
        if identifier in seen:
            errors.append(f"duplicate requirement {identifier}")
        seen.add(identifier)
        if not item["section"] or not item["requirement"]:
            errors.append(f"{identifier}: missing contract reference")
        if item["status"] not in ("pending", "partial", "verified", "blocked"):
            errors.append(f"{identifier}: invalid status")
        if item["status"] == "verified" and not item["evidence"]:
            errors.append(f"{identifier}: verified requires command/result evidence")
        for evidence in item["evidence"]:
            if not all(evidence.get(key) for key in ("command", "outcome", "scope")):
                errors.append(f"{identifier}: incomplete evidence")
            if evidence.get("scope") not in ("unit", "contract", "integration", "real-node", "provider", "inspection"):
                errors.append(f"{identifier}: invalid evidence scope")
            if item["status"] == "verified" and evidence.get("outcome") != "passed":
                errors.append(f"{identifier}: failed/unexecuted evidence cannot verify a requirement")
        if item["status"] == "verified" and identifier in REAL_NODE_REQUIRED:
            if not any(evidence.get("scope") in ("real-node", "provider") for evidence in item["evidence"]):
                errors.append(f"{identifier}: real-node/provider evidence is mandatory")
    required_scenarios = {f"E2E-{number:02}" for number in range(1, 10)}
    if not required_scenarios <= seen:
        errors.append("missing Proposal 0009 section 13.1 acceptance scenarios")
    if document.get("contractRevision") != "2026-10-20-immutable-runtime-view":
        errors.append("missing current immutable-runtime-view contract revision")
    if not {f"UP-{number:02}" for number in range(1, 7)} <= seen:
        errors.append("missing approved upstream-runtime amendment obligations")
    if not {"SCHEMA-01", "WASM-01", "WASM-02", "WASM-03"} <= seen:
        errors.append("missing flat schema or Wasm qualification obligations")
    if not {f"FS-{number:02}" for number in range(1, 5)} <= seen:
        errors.append("missing immutable runtime filesystem obligations")
    for item in requirements:
        if not item["id"].startswith("FS-") or item["status"] != "verified":
            continue
        qualified = False
        for evidence in item["evidence"]:
            relative = evidence.get("result", "")
            if evidence.get("scope") != "real-node" or not relative.startswith("acceptance/results/"):
                continue
            path = Path(__file__).resolve().parents[1] / relative
            try:
                result = json.loads(path.read_text())
            except (OSError, ValueError) as error:
                errors.append(f'{item["id"]}: filesystem receipt unavailable: {error}')
                continue
            covered = {identifier for row in result.get("assertions", []) if row.get("outcome") == "passed"
                       for identifier in row.get("requirements", [])}
            needed = {"FS-01", "FS-02", "FS-03"}
            if item["id"] == "FS-04":
                needed.add("RUN-08")
            if (result.get("outcome") == "passed" and result.get("cleanSelectedSuitePassed") is True
                    and result.get("fixtureDestruction", {}).get("outcome") == "passed"
                    and result.get("architecture") in ("arm64", "amd64")
                    and result.get("selectedScenario") in ("complete", "accounting")
                    and "python" in result.get("selectedLanguages", []) and needed <= covered):
                qualified = True
        if not qualified:
            errors.append(f'{item["id"]}: clean real-node immutable-filesystem receipt required')
    return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--require-complete", action="store_true")
    args = parser.parse_args()
    load_matrix()
    document = json.loads(Path(__file__).with_name("requirements.json").read_text())
    errors = validate(document)
    requirements = document["requirements"] + document.get("amendments", [])
    incomplete = [item for item in requirements if item["status"] != "verified"]
    print(f'{len(requirements)} requirements; {len(incomplete)} not verified')
    if args.require_complete:
        errors.extend(f'{item["id"]} [{item["status"]}] {item["requirement"]}' for item in incomplete)
    for error in errors:
        print(error)
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(main())
