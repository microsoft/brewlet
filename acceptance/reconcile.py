#!/usr/bin/env python3
"""Prepare reviewed current evidence bindings; never execute ledger commands."""

import argparse
import copy
import json
from pathlib import Path
import shlex
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from acceptance.check import REAL_NODE_REQUIRED, REQUIRED_IDS
from acceptance.evidence import ROOT, read_json
from acceptance.qualify import SUITES
from acceptance.run_check import CHECKS


LIVE_SUITES = {
    "core": """ARCH-01 INV-03 INV-04 INV-05 INV-06 ROOT-01 ROOT-04 PROF-03 PROF-04 PROF-05
        LEASE-01 LEASE-02 LEASE-03 OD-04 OD-05 OD-06 ROT-01 ROT-02 ROT-04 ROT-05 ROT-06 ROT-07 ROT-08 ROT-09
        RUN-01 RUN-02 RUN-03 RUN-07 RUN-08 PROC-01 PROC-02 PROC-03 SEC-01 SEC-02 SEC-03 SEC-04
        ADM-01 ADM-02 ADM-03 ADM-04 LABEL-01 FENCE-01 FENCE-02 FENCE-03 FENCE-04 RC-01
        OWN-02 OBS-01 OBS-02 OBS-03 OBS-04 E2E-01 E2E-02 E2E-03 E2E-04 E2E-06 E2E-07 E2E-08 E2E-09
        LANG-01 LANG-02 FS-01 FS-02 FS-03 FS-04 UP-06""".split(),
    "cold": ["OD-03", "OD-05", "ROT-09", "ROT-10"],
    "wasm": ["SCHEMA-01", "WASM-01", "WASM-02", "WASM-03"],
    "upstream": ["ROOT-02", "ROOT-03", "UP-01", "UP-02", "UP-03", "UP-04", "UP-05"],
    "native": ["SCHED-01", "E2E-03", "E2E-05"],
    "uninstall": ["E2E-06", "FENCE-04", "LEASE-03", "OWN-04"],
    "retirement": ["FENCE-01", "OWN-01", "OWN-02", "OWN-03", "OWN-04"],
    "activation": ["LEASE-02", "LEASE-03", "OD-01", "OWN-02", "PROF-02", "RUN-06", "RUN-09", "RUN-10"],
}


def bindings():
    result = {identifier: [] for identifier in REQUIRED_IDS}
    for name, check in CHECKS.items():
        for identifier in check["requirements"]:
            if identifier not in result:
                continue
            for architecture in ("amd64", "arm64") if check.get("native") else (None,):
                suffix = "-" + architecture if architecture else ""
                evidence = {"command": shlex.join(check["command"]), "scope": check["scope"],
                            "outcome": "pending", "result": f"acceptance/results/{name}{suffix}.json"}
                if architecture:
                    evidence["architecture"] = architecture
                result[identifier].append(evidence)
    for suite, identifiers in LIVE_SUITES.items():
        if suite not in SUITES:
            raise ValueError("unknown bound suite: " + suite)
        for identifier in identifiers:
            if identifier not in result:
                raise ValueError("unknown bound obligation: " + identifier)
            for architecture in ("amd64", "arm64"):
                result[identifier].append({
                    "command": f"python3 acceptance/qualify.py --run --scenario complete --architecture {architecture}",
                    "scope": "real-node", "suite": suite, "architecture": architecture, "outcome": "pending",
                    "result": f"acceptance/results/candidate-{architecture}-{suite}.json"})
    for identifier, evidence in result.items():
        if not evidence:
            raise ValueError("obligation has no reviewed evidence binding: " + identifier)
        if identifier in REAL_NODE_REQUIRED and not any(row["scope"] == "real-node" for row in evidence):
            raise ValueError("mandatory live obligation has no native execution binding: " + identifier)
    return result


def prepare(document):
    document = copy.deepcopy(document)
    expected = bindings()
    rows = document["requirements"] + document.get("amendments", [])
    if {row["id"] for row in rows} != REQUIRED_IDS or len(rows) != len(REQUIRED_IDS):
        raise ValueError("baseline obligations changed or duplicated")
    for row in rows:
        if "historicalEvidence" not in row:
            row["historicalEvidence"] = {"status": row["status"], "evidence": row["evidence"]}
        row.update(status="pending", evidence=expected[row["id"]])
    document["historicalBaseline"] = (
        "Original status and execution declarations are retained unchanged under historicalEvidence. "
        "Active evidence bindings are pending fresh execution against frozen current inputs. "
        "Historical writable-root receipts do not establish the immutable-runtime-view contract.")
    return document


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--write", action="store_true", help="prepare source mappings before candidate freeze")
    args = parser.parse_args()
    path = ROOT / "acceptance/requirements.json"
    document = read_json(path)
    prepared = prepare(document)
    if args.write:
        lines = ["{"]
        for index, (key, value) in enumerate(prepared.items()):
            comma = "," if index < len(prepared) - 1 else ""
            if key in ("requirements", "amendments"):
                lines.append("  " + json.dumps(key) + ": [")
                lines.extend("    " + json.dumps(row, separators=(",", ":")) +
                             ("," if offset < len(value) - 1 else "") for offset, row in enumerate(value))
                lines.append("  ]" + comma)
            else:
                lines.append("  " + json.dumps(key) + ": " + json.dumps(value) + comma)
        path.write_text("\n".join(lines + ["}", ""]))
        return 0
    expected = {row["id"]: row for row in prepared["amendments"] + prepared["requirements"]}
    for row in document["requirements"] + document.get("amendments", []):
        planned = expected[row["id"]]
        actual = [{k: v for k, v in evidence.items() if k not in ("outcome", "result")}
                  for evidence in row["evidence"]]
        desired = [{k: v for k, v in evidence.items() if k not in ("outcome", "result")}
                   for evidence in planned["evidence"]]
        if actual != desired:
            print(row["id"] + ": active command/scope/architecture bindings differ from reviewed plan")
            return 1
    print(f"{len(REQUIRED_IDS)} obligations have reviewed current execution bindings; no execution is implied")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
