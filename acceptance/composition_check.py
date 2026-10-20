#!/usr/bin/env python3
"""Proposal 0010 qualification is separate from the Proposal 0009 baseline."""

import argparse
import json
from pathlib import Path
import re
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from acceptance.qualification import fixture_names, load_matrix


MATRIX = load_matrix()
NATIVE_SCOPES = {
    name: (scope["architecture"], scope["scenario"], set(fixture_names(MATRIX, scope["group"])),
           {f"COMP-{number:02}" for number in range(1, 9)} if scope["group"] == "native" else
           {f"COMP-WASM-{number:02}" for number in range(1, 5)})
    for name, scope in MATRIX["compositionScopes"].items()
}


def validate(document, root, require_complete=False):
    errors, incomplete = [], []
    if document.get("schemaVersion") != 1 or document.get("contract") != "Proposal 0010":
        errors.append("wrong composition ledger contract/version")
    required = {f"COMP-{number:02}" for number in range(1, 26)}
    required |= {f"COMP-WASM-{number:02}" for number in range(1, 5)}
    identifiers = [row.get("id") for row in document.get("requirements", [])]
    if set(identifiers) != required or len(identifiers) != len(required):
        errors.append("missing or duplicate composition obligation")
    checks = document.get("checks", {})
    if not NATIVE_SCOPES.keys() <= checks.keys():
        errors.append("native language and Wasm architecture checks are all mandatory")
    successful = set()
    for name, check in checks.items():
        if not check.get("command") or check.get("scope") not in ("contract", "integration", "inspection", "real-node"):
            errors.append(f"{name}: incomplete evidence command/scope")
        if check.get("outcome") != "passed":
            continue
        if check.get("scope") == "real-node":
            if name not in NATIVE_SCOPES:
                errors.append(f"{name}: unknown native qualification scope")
                continue
            expected_architecture, scenario, workloads, obligations = NATIVE_SCOPES[name]
            path = root / check.get("result", "")
            if not path.is_file():
                errors.append(f"{name}: passed requires persistent public result")
                continue
            result = json.loads(path.read_text())
            architecture = result.get("architecture")
            covered = {identifier for row in result.get("assertions", []) if row.get("outcome") == "passed"
                       for identifier in row.get("requirements", [])}
            if (result.get("outcome") != "passed" or not result.get("cleanSelectedSuitePassed")
                    or result.get("fixtureDestruction", {}).get("outcome") != "passed"
                    or result.get("selectedScenario") != scenario
                    or set(result.get("selectedLanguages", [])) != workloads
                    or architecture != expected_architecture or architecture != check.get("architecture")
                    or not obligations <= covered):
                errors.append(f"{name}: incomplete or wrong native qualification scope")
                continue
            if name.startswith("wasm-"):
                guests = result.get("wasmGuestInputs", {})
                runtimes = result.get("wasmRuntimes", {})
                if (set(guests) != {"command", "http"} or set(runtimes) != workloads
                        or not all(re.fullmatch(r"[0-9a-f]{64}", value.get("sha256", "")) for value in guests.values())
                        or not all(record.get("sourceKind") ==
                            "custom-platform-fixture-pinned-distroless-cc-and-unchanged-release-binary"
                            and record.get("binarySHA256") and record.get("base")
                            for revisions in runtimes.values() for record in revisions.values())
                        or not all(runtimes.values())):
                    errors.append(f"{name}: missing actual guest/host provenance or false unchanged-root scope")
                    continue
        successful.add(name)
    for row in document.get("requirements", []):
        if not row.get("sections") or not row.get("requirement") or not row.get("checks"):
            errors.append(f'{row.get("id")}: incomplete contract/test mapping')
        if any(name not in checks for name in row.get("checks", [])):
            errors.append(f'{row.get("id")}: unknown evidence check')
        if not set(row.get("checks", [])) <= successful:
            incomplete.append(row.get("id"))
    if require_complete:
        errors.extend(f"{identifier}: not qualified" for identifier in incomplete)
    return errors, incomplete


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--require-complete", action="store_true")
    args = parser.parse_args()
    root = Path(__file__).resolve().parent
    document = json.loads((root / "composition-requirements.json").read_text())
    errors, incomplete = validate(document, root, args.require_complete)
    print(f'{len(document["requirements"])} composition obligations; {len(incomplete)} not qualified')
    for error in errors:
        print(error)
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(main())
