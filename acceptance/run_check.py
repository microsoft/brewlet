#!/usr/bin/env python3
"""Persist an actual fixed non-live check; never execute commands from a ledger."""

import argparse
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from acceptance.evidence import ROOT, working_identity
from acceptance.native_contracts import native_architecture


CHECKS = {
    "go-contracts": {
        "command": ["go", "test", "-count=1", "-race", "-mod=readonly", "./..."],
        "scope": "contract",
        "requirements": [*"""ARCH-01 ARCH-02 ART-01 ART-02 ART-03 ART-04 ART-05 ART-06 ART-07 ART-08 ART-09 ART-10
                         DEP-01 INV-01 INV-02 INV-03 INV-04 INV-05 INV-06 ROOT-01 ROOT-02 ROOT-03 ROOT-04
                         PROF-01 PROF-02 PROF-03 PROF-04 LEASE-01 LEASE-02 LEASE-03 OD-01 OD-02 OD-03 OD-04 OD-05
                         ROT-02 ROT-03 ROT-04 ROT-05 ROT-06 ROT-07 RUN-01 RUN-02 RUN-03 RUN-04 RUN-05 RUN-06 RUN-07
                         RUN-09 RUN-10 PROC-01 PROC-02 PROC-03 SEC-02 SEC-03 SEC-04 ADM-01 ADM-02 ADM-03 ADM-04
                         LABEL-01 LABEL-02 FENCE-01 FENCE-02 FENCE-03 RC-01 CLI-01 OWN-01 OWN-02 OWN-03 OWN-04
                         OBS-01 OBS-02 OBS-03 OBS-04 E2E-01 E2E-04 FS-01 FS-02 SCHEMA-01 UP-01 UP-02 UP-03 UP-06""".split(),
                         "COMP-01", "COMP-02", "COMP-03", "COMP-05", "COMP-06", "COMP-08",
                         *[f"COMP-{number:02}" for number in range(9, 26) if number not in (19, 24)]]},
    "maven": {
        "command": ["mvn", "-B", "-q", "-f", "maven-plugin/pom.xml", "verify"],
        "scope": "contract",
        "requirements": ["MAV-01", "UP-06", "COMP-01", "COMP-19"]},
    "documentation": {
        "command": ["make", "site-build"],
        "scope": "inspection",
        "requirements": ["ROOT-05", "UP-06", "COMP-18", "COMP-19", "COMP-20", "COMP-23", "COMP-24", "COMP-WASM-04"]},
    "fixtures": {
        "command": ["python3", "acceptance/producer_check.py", "fixtures"],
        "scope": "integration", "native": True,
        "requirements": ["ART-01", "ART-06", "ART-10", "DEP-03", "CLI-02", "LANG-01", "MAV-01", "SCHEMA-01"]},
    "registry": {
        "command": ["python3", "acceptance/producer_check.py", "registry"],
        "scope": "integration", "native": True,
        "requirements": ["DEP-02", "DEP-03"]},
    "native-contracts": {
        "command": ["python3", "acceptance/native_contracts.py"],
        "scope": "contract", "native": True,
        "requirements": ["ART-05", "ART-10", "INV-05", "ROOT-02", "ROOT-03", "ROOT-04",
                         "PROF-01", "PROF-02", "PROF-03", "PROF-04", "LEASE-01", "LEASE-02",
                         "LEASE-03", "OD-01", "OD-02", "OD-03", "OD-04", "OD-05",
                         "ROT-05", "ROT-07", "RUN-01", "RUN-02", "RUN-03", "RUN-04",
                         "RUN-05", "RUN-06", "RUN-09", "RUN-10", "PROC-02", "PROC-03",
                         "SEC-04", "LABEL-01", "FENCE-03", "OWN-01", "OWN-02",
                         "OBS-01", "OBS-02", "OBS-04", "FS-01", "FS-02",
                         "SCHEMA-01", "UP-02", "UP-03", "UP-06"]},
    "go-vet": {
        "command": ["go", "vet", "-mod=readonly", "./..."],
        "scope": "contract", "native": True, "requirements": ["UP-06"]},
    "site-integration": {
        "command": ["npm", "test", "--prefix", "site"],
        "scope": "integration", "requirements": ["UP-06"]},
}


def execute(name, output):
    if output.exists():
        raise ValueError("refusing to overwrite an existing execution receipt: " + str(output))
    identity = working_identity()
    check = CHECKS[name]
    result = {**identity, "scope": check["scope"], "command": shlex.join(check["command"]),
              "outcome": "failed", "assertions": []}
    try:
        environment = None
        if check.get("native"):
            architecture = native_architecture()
            result.update(architecture=architecture, nativeExecution=True)
            environment = dict(os.environ, GOOS="linux", GOARCH=architecture)
        outcome = subprocess.run(check["command"], cwd=ROOT, check=False, env=environment).returncode
    except (OSError, ValueError) as error:
        outcome = 1
        result["failure"] = str(error)
        print(name + " failed: " + str(error), file=sys.stderr)
    result["outcome"] = "passed" if outcome == 0 else "failed"
    result["assertions"].append({"name": name + " command exit status",
                                 "requirements": check["requirements"], "outcome": result["outcome"]})
    try:
        if working_identity() != identity:
            raise ValueError("candidate changed during non-live check")
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        result.update(outcome="failed", failure=str(error))
        result["assertions"].append({"name": "unchanged qualification inputs", "outcome": "failed",
                                     "requirements": []})
        print(name + " failed: " + str(error), file=sys.stderr)
        outcome = 1
    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open("x") as receipt:
        receipt.write(json.dumps(result, indent=2) + "\n")
    return outcome


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("check", choices=(*CHECKS, "all"))
    args = parser.parse_args()
    try:
        if (ROOT / "acceptance-export").is_symlink():
            raise ValueError("receipt output must be the isolated acceptance-export directory, not a symlink")
        checks = CHECKS if args.check == "all" else (args.check,)
        outcome = 0
        for name in checks:
            suffix = "-" + native_architecture() if CHECKS[name].get("native") else ""
            if execute(name, ROOT / "acceptance-export" / (name + suffix + ".json")):
                outcome = 1
        return outcome
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print("non-live qualification failed:", error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
