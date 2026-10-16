#!/usr/bin/env python3
"""Validate the acceptance ledger; --require-complete is the release gate."""

import argparse
import json
from pathlib import Path


REAL_NODE_REQUIRED = {
    "PROF-03", "PROF-05", "LEASE-02", "RUN-07", "RUN-08", "RUN-09",
    "PROC-03", "E2E-02", "E2E-03", "E2E-05", "E2E-06", "E2E-07",
    "E2E-08", "LANG-02",
}


def validate(document):
    requirements = document["requirements"]
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
    return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--require-complete", action="store_true")
    args = parser.parse_args()
    document = json.loads(Path(__file__).with_name("requirements.json").read_text())
    errors = validate(document)
    incomplete = [item for item in document["requirements"] if item["status"] != "verified"]
    print(f'{len(document["requirements"])} requirements; {len(incomplete)} not verified')
    if args.require_complete:
        errors.extend(f'{item["id"]} [{item["status"]}] {item["requirement"]}' for item in incomplete)
    for error in errors:
        print(error)
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(main())
