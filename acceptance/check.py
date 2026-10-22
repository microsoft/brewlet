#!/usr/bin/env python3
"""Validate the acceptance ledger; --require-complete is the release gate."""

import argparse
import json
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from acceptance.qualification import load_matrix
from acceptance.evidence import ROOT, load_receipt, read_json, receipt_errors, working_identity
from acceptance.qualify import SUITES, suite_errors


REAL_NODE_REQUIRED = {
    "PROF-03", "PROF-05", "LEASE-02", "RUN-07", "RUN-08", "RUN-09", "RUN-10",
    "PROC-03", "E2E-02", "E2E-03", "E2E-05", "E2E-06", "E2E-07",
    "E2E-08", "LANG-02",
    "UP-05",
    "WASM-01", "WASM-02", "WASM-03",
    "FS-03", "FS-04",
}
REQUIRED_IDS = {
    f"{prefix}-{number:02}"
    for prefix, count in {
        "ARCH": 2, "ART": 10, "DEP": 3, "INV": 6, "ROOT": 5, "PROF": 5,
        "LEASE": 3, "OD": 6, "ROT": 10, "RUN": 10, "PROC": 3, "SEC": 4,
        "ADM": 4, "LABEL": 2, "FENCE": 4, "SCHED": 1, "RC": 1, "CLI": 2,
        "MAV": 1, "OWN": 4, "OBS": 4, "E2E": 9, "LANG": 2, "FS": 4,
        "SCHEMA": 1, "WASM": 3, "UP": 6,
    }.items() for number in range(1, count + 1)
}
# These obligations previously depended on actual node execution. Ledger edits
# cannot silently downgrade them to contracts or inspection.
REAL_NODE_REQUIRED |= set("""
ARCH-01 INV-03 INV-04 INV-05 INV-06 ROOT-01 ROOT-02 ROOT-03 ROOT-04
PROF-03 PROF-04 PROF-05 LEASE-01 LEASE-02 LEASE-03
OD-01 OD-03 OD-04 OD-05 OD-06 ROT-01 ROT-02 ROT-04 ROT-05 ROT-06 ROT-07 ROT-08 ROT-09 ROT-10
RUN-01 RUN-02 RUN-03 RUN-06 RUN-07 RUN-08 RUN-09 RUN-10 PROC-01 PROC-02 PROC-03
SEC-02 SEC-03 SEC-04 LABEL-01 FENCE-01 FENCE-03 FENCE-04 SCHED-01 RC-01
OWN-01 OWN-02 OWN-03 OWN-04 OBS-01 OBS-02 OBS-03 OBS-04
E2E-02 E2E-03 E2E-05 E2E-06 E2E-07 E2E-08 E2E-09 LANG-02
FS-01 FS-02 FS-03 FS-04 SCHEMA-01 WASM-01 WASM-02 WASM-03
UP-01 UP-02 UP-03 UP-04 UP-05 UP-06
""".split())


def validate(document, root=ROOT, *, structure_only=False, expected=None):
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
            if "suite" in evidence and (not isinstance(evidence["suite"], str) or evidence["suite"] not in SUITES
                    or evidence.get("scope") != "real-node"
                    or evidence.get("architecture") not in ("amd64", "arm64")):
                errors.append(f"{identifier}: invalid native suite binding")
            if item["status"] == "verified" and evidence.get("outcome") != "passed":
                errors.append(f"{identifier}: failed/unexecuted evidence cannot verify a requirement")
        if item["status"] == "verified" and identifier in REAL_NODE_REQUIRED:
            if not any(evidence.get("scope") in ("real-node", "provider") for evidence in item["evidence"]):
                errors.append(f"{identifier}: real-node/provider evidence is mandatory")
    required_scenarios = {f"E2E-{number:02}" for number in range(1, 10)}
    if seen != REQUIRED_IDS:
        errors.append("missing or unknown mandatory baseline/amendment obligations")
    if not required_scenarios <= seen:
        errors.append("missing specification §13.1 acceptance scenarios")
    if document.get("contractRevision") != "2026-10-20-immutable-runtime-view":
        errors.append("missing current immutable-runtime-view contract revision")
    if not {f"UP-{number:02}" for number in range(1, 7)} <= seen:
        errors.append("missing approved upstream-runtime amendment obligations")
    if not {"SCHEMA-01", "WASM-01", "WASM-02", "WASM-03"} <= seen:
        errors.append("missing flat schema or Wasm qualification obligations")
    if not {f"FS-{number:02}" for number in range(1, 5)} <= seen:
        errors.append("missing immutable runtime filesystem obligations")
    if structure_only:
        return errors
    try:
        expected = expected or working_identity(root)
    except (OSError, ValueError) as error:
        errors.append("candidate unavailable: " + str(error))
        return errors
    for item in requirements:
        identifier = item["id"]
        if item["status"] != "verified":
            continue
        for evidence in item["evidence"]:
            try:
                result = load_receipt(root, evidence.get("result"))
            except (OSError, ValueError) as error:
                errors.append(f"{identifier}: {error}")
                continue
            live = evidence.get("scope") in ("real-node", "provider")
            problems = receipt_errors(result, [identifier], live=live, root=root, expected=expected)
            if not live and result.get("scope") != evidence.get("scope"):
                problems.append("receipt scope differs from declared scope")
            if not live and result.get("command") != evidence.get("command"):
                problems.append("receipt command differs from declared execution")
            if "architecture" in evidence:
                if (evidence["architecture"] not in ("amd64", "arm64")
                        or result.get("architecture") != evidence["architecture"]
                        or (not live and result.get("nativeExecution") is not True)):
                    problems.append("wrong or unproven native architecture")
            if isinstance(evidence.get("suite"), str) and evidence["suite"] in SUITES:
                problems.extend(suite_errors(result, evidence["suite"], evidence.get("architecture"),
                                              root=root, expected=expected))
            if evidence.get("scope") == "provider" and result.get("providerScaleFromZeroVerified") is not True:
                problems.append("actual provider scale-from-zero evidence required")
            if identifier == "LANG-02" and live and not problems and (result.get("selectedScenario") != "complete"
                    or set(result.get("selectedLanguages", [])) != {"java", "python", "node", "dotnet"}):
                problems.append("complete four-language execution required")
            if identifier.startswith("FS-") and live and not problems:
                covered = {value for row in result.get("assertions", []) if isinstance(row, dict)
                           and row.get("outcome") == "passed" for value in row.get("requirements", [])}
                needed = {"FS-01", "FS-02", "FS-03"}
                if identifier == "FS-04":
                    needed.add("RUN-08")
                if (result.get("selectedScenario") not in ("complete", "accounting")
                        or "python" not in result.get("selectedLanguages", [])
                        or not needed <= covered):
                    problems.append("immutable-filesystem execution scope required")
            errors.extend(f"{identifier}: {problem}" for problem in problems)
    return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--require-complete", action="store_true")
    parser.add_argument("--structure-only", action="store_true",
                        help="inspect ledger structure, never qualify a candidate")
    args = parser.parse_args()
    if args.require_complete and args.structure_only:
        parser.error("--structure-only cannot satisfy --require-complete")
    if args.require_complete:
        from acceptance.verify_evidence import validate as verify
        try:
            errors = verify(ROOT, read_json(ROOT / "acceptance/evidence-index.json"), baseline_only=True)
        except (OSError, ValueError) as error:
            errors = [str(error)]
        for error in errors:
            print(error)
        if not errors:
            print("115 baseline obligations verified from actual indexed receipts")
        return int(bool(errors))
    load_matrix()
    document = json.loads(Path(__file__).with_name("requirements.json").read_text())
    errors = validate(document, structure_only=args.structure_only)
    requirements = document["requirements"] + document.get("amendments", [])
    incomplete = [item for item in requirements if item["status"] != "verified"]
    print(f'{len(requirements)} current obligations; {len(incomplete)} not declared verified; '
          + ("structure only, NOT candidate qualification" if args.structure_only else "candidate evidence checked"))
    if args.require_complete:
        errors.extend(f'{item["id"]} [{item["status"]}] {item["requirement"]}' for item in incomplete)
    for error in errors:
        print(error)
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(main())
