#!/usr/bin/env python3
"""Persist an actual fixed non-live check; never execute commands from a ledger."""

import argparse
import json
from pathlib import Path
import shlex
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from acceptance.evidence import ROOT, working_identity


CHECKS = {
    "go-contracts": {
        "command": ["go", "test", "-count=1", "-race", "-mod=readonly", "./..."],
        "scope": "contract",
        "requirements": ["ARCH-02", "ART-01", "ART-02", "ART-03", "ART-04", "ART-05",
                         "ART-06", "ART-07", "ART-08", "ART-09", "ART-10", "DEP-01",
                         "INV-01", "INV-02", "PROF-01", "PROF-02", "OD-02", "ROT-03",
                         "RUN-04", "RUN-05", "LABEL-02", "CLI-01",
                         "COMP-01", "COMP-02", "COMP-03", "COMP-05", "COMP-06", "COMP-08",
                         *[f"COMP-{number:02}" for number in range(9, 26) if number not in (19, 24)]]},
    "maven": {
        "command": ["mvn", "-B", "-q", "-f", "maven-plugin/pom.xml", "verify"],
        "scope": "contract",
        "requirements": ["MAV-01", "COMP-01", "COMP-19"]},
    "documentation": {
        "command": ["make", "site-build"],
        "scope": "inspection",
        "requirements": ["ROOT-05", "COMP-18", "COMP-19", "COMP-20", "COMP-23", "COMP-24", "COMP-WASM-04"]},
}


def execute(name, output):
    identity = working_identity()
    check = CHECKS[name]
    result = {**identity, "scope": check["scope"], "command": shlex.join(check["command"]),
              "outcome": "failed", "assertions": []}
    try:
        outcome = subprocess.run(check["command"], cwd=ROOT, check=False).returncode
    except OSError as error:
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
    output.write_text(json.dumps(result, indent=2) + "\n")
    return outcome


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("check", choices=(*CHECKS, "all"))
    args = parser.parse_args()
    try:
        checks = CHECKS if args.check == "all" else (args.check,)
        outcome = 0
        for name in checks:
            if execute(name, ROOT / "acceptance-export" / (name + ".json")):
                outcome = 1
        return outcome
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print("non-live qualification failed:", error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
