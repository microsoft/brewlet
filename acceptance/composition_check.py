#!/usr/bin/env python3
"""Specification §16 (composition) qualification is separate from the Part I baseline."""

import argparse
import json
from pathlib import Path
import re
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from acceptance.qualification import fixture_names, load_matrix
from acceptance.evidence import load_receipt, receipt_errors, working_identity


MATRIX = load_matrix()
NATIVE_SCOPES = {
    name: (scope["architecture"], scope["scenario"], set(fixture_names(MATRIX, scope["group"])),
           {f"COMP-{number:02}" for number in range(1, 9)} if scope["group"] == "native" else
           {f"COMP-WASM-{number:02}" for number in range(1, 5)})
    for name, scope in MATRIX["compositionScopes"].items()
}


def wasm_provenance(result, workloads):
    guests = result.get("wasmGuestInputs")
    runtimes = result.get("wasmRuntimes")
    return (isinstance(guests, dict) and set(guests) == {"command", "http"}
            and all(isinstance(value, dict) and isinstance(value.get("sha256"), str)
                    and re.fullmatch(r"[0-9a-f]{64}", value["sha256"]) for value in guests.values())
            and isinstance(runtimes, dict) and set(runtimes) == workloads
            and all(isinstance(revisions, dict) and set(revisions) == {"after"}
                    and all(isinstance(record, dict) and record.get("sourceKind") ==
                            "custom-platform-fixture-pinned-distroless-cc-and-unchanged-release-binary"
                            and isinstance(record.get("binarySHA256"), str)
                            and re.fullmatch(r"[0-9a-f]{64}", record["binarySHA256"])
                            and isinstance(record.get("base"), str)
                            and re.fullmatch(r"[^@\s]+@sha256:[0-9a-f]{64}", record["base"])
                            for record in revisions.values())
                    for revisions in runtimes.values()))


def validate(document, root, require_complete=False, *, structure_only=False, expected=None):
    errors, incomplete = [], []
    if document.get("schemaVersion") != 1 or document.get("contract") != "specs/SPECIFICATION.md §16":
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
    candidate_root = root.parent
    if not structure_only:
        try:
            expected = expected or working_identity(candidate_root)
        except (OSError, ValueError) as error:
            errors.append("candidate unavailable: " + str(error))
            return errors, [row.get("id") for row in document.get("requirements", [])]
    for name, check in checks.items():
        if name in NATIVE_SCOPES and (check.get("scope") != "real-node"
                                      or check.get("architecture") != NATIVE_SCOPES[name][0]):
            errors.append(f"{name}: native scope/architecture cannot be downgraded")
        if not check.get("command") or check.get("scope") not in ("contract", "integration", "inspection", "real-node"):
            errors.append(f"{name}: incomplete evidence command/scope")
        if check.get("outcome") not in ("passed", "failed", "pending", "skipped"):
            errors.append(f"{name}: unknown execution outcome")
        if check.get("outcome") != "passed":
            continue
        if structure_only:
            continue
        try:
            relative = check.get("result", "")
            result = load_receipt(candidate_root, "acceptance/" + relative)
        except (OSError, ValueError) as error:
            errors.append(f"{name}: {error}")
            continue
        obligations = {row["id"] for row in document.get("requirements", []) if name in row.get("checks", [])}
        problems = receipt_errors(result, obligations, live=check.get("scope") == "real-node",
                                  root=candidate_root, expected=expected)
        if check.get("scope") != "real-node" and result.get("scope") != check.get("scope"):
            problems.append("receipt scope differs from declared scope")
        if check.get("scope") != "real-node" and result.get("command") != check.get("command"):
            problems.append("receipt command differs from declared execution")
        if problems:
            errors.extend(f"{name}: {problem}" for problem in problems)
            continue
        if check.get("scope") == "real-node":
            if name not in NATIVE_SCOPES:
                errors.append(f"{name}: unknown native qualification scope")
                continue
            expected_architecture, scenario, workloads, obligations = NATIVE_SCOPES[name]
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
                if not wasm_provenance(result, workloads):
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
    if require_complete and not structure_only:
        errors.extend(f"{name}: mandatory native scope not qualified" for name in NATIVE_SCOPES
                      if name not in successful)
        errors.extend(f"{identifier}: not qualified" for identifier in incomplete)
    return errors, incomplete


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--require-complete", action="store_true")
    parser.add_argument("--structure-only", action="store_true",
                        help="inspect historical declarations, never qualify a candidate")
    args = parser.parse_args()
    if args.require_complete and args.structure_only:
        parser.error("--structure-only cannot satisfy --require-complete")
    root = Path(__file__).resolve().parent
    if args.require_complete:
        from acceptance.evidence import read_json
        from acceptance.verify_evidence import validate as verify
        try:
            errors = verify(root.parent, read_json(root / "evidence-index.json"), composition_only=True)
        except (OSError, ValueError) as error:
            errors = [str(error)]
        for error in errors:
            print(error)
        if not errors:
            print("29 composition obligations verified from actual indexed receipts")
        return int(bool(errors))
    document = json.loads((root / "composition-requirements.json").read_text())
    errors, incomplete = validate(document, root, args.require_complete, structure_only=args.structure_only)
    print(f'{len(document["requirements"])} composition obligations; {len(incomplete)} not qualified')
    for error in errors:
        print(error)
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(main())
