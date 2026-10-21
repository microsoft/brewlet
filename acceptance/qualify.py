#!/usr/bin/env python3
"""Explicit native qualification runner and complete-suite evidence gate."""

import argparse
import json
from pathlib import Path
import platform
import subprocess
import sys
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from acceptance.evidence import ROOT, read_json, receipt_errors, working_identity
from acceptance.export_result import export
from acceptance.qualification import fixture_names, load_matrix


NATIVE = fixture_names(load_matrix(), "native")
WASM = fixture_names(load_matrix(), "wasm")
# Scenario identity and workload sets are independent of ledger declarations.
SUITES = {
    "core": ("harness.py", "complete", NATIVE),
    "cold": ("cold_install.py", "cold-install", NATIVE),
    "tutorial": (None, "reader-tutorial", ("python",)),
    "wasm": ("wasm.py", "wasm-runc-examples", WASM),
    "upstream": ("upstream.py", "unchanged-upstream-distroless", ("java", "python", "node")),
    "native": ("native_capacity.py", "native-capacity", NATIVE),
    "uninstall": ("pinned_uninstall.py", "pinned-uninstall", NATIVE),
    "retirement": ("retirement.py", "retirement", NATIVE),
    "activation": ("activation.py", "activation", NATIVE),
    "managed": ("managed.py", "managed-composition", NATIVE),
    "managed-wasm": ("managed_wasm.py", "managed-wasm-composition", WASM),
}
COMPLETE = tuple(name for name in SUITES if name not in ("managed", "managed-wasm"))
DEBUG_SUITES = {"control": ("harness.py", "control", NATIVE)}


def suite_errors(result, suite, architecture, root=ROOT, expected=None):
    _, scenario, languages = (SUITES | DEBUG_SUITES)[suite]
    if suite == "tutorial":
        errors = receipt_errors(result, root=root, expected=expected)
        destruction = result.get("fixtureDestruction")
        if (result.get("scope") != "integration" or result.get("selectedScenario") != scenario
                or result.get("architecture") != architecture
                or result.get("productionCleanupVerified") is not True
                or not isinstance(destruction, dict) or destruction.get("outcome") != "passed"):
            errors.append("reader tutorial execution and cleanup required")
        return errors
    errors = receipt_errors(result, live=True, architecture=architecture, scenario=scenario,
                            languages=languages, root=root, expected=expected)
    if suite == "core" and (result.get("scope") != "real-node-execution-and-remote-delivery"
                            or result.get("kubeletPullVerified") is not True
                            or result.get("tlsTransportVerified") is not True):
        errors.append("complete core suite requires actual authenticated TLS kubelet pulls")
    return errors


def validate_suites(directory, root=ROOT, expected=None):
    errors = []
    expected = expected or working_identity(root)
    for architecture in ("amd64", "arm64"):
        for suite in SUITES:
            name = f"candidate-{architecture}-{suite}.json"
            try:
                result = read_json(Path(directory) / name)
                if not isinstance(result, dict):
                    raise ValueError("receipt must be an object")
                problems = suite_errors(result, suite, architecture, root, expected)
                errors.extend(name + ": " + problem for problem in problems)
            except (OSError, ValueError) as error:
                errors.append(name + ": unavailable/invalid receipt: " + str(error))
    return errors


def run(scenario, architecture, output):
    machine = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    if platform.system() != "Linux" or machine != architecture:
        raise ValueError("qualification requires the selected native Linux architecture, not emulation")
    identity = working_identity()
    output = output.resolve()
    if output != ROOT / "acceptance-export":
        raise ValueError("qualification output must be the isolated ignored acceptance-export directory")
    output.mkdir(exist_ok=True)
    suites = COMPLETE if scenario == "complete" else (scenario,)
    invocation = uuid.uuid4().hex[:12]
    for suite in suites:
        script, selected, languages = (SUITES | DEBUG_SUITES)[suite]
        if suite == "tutorial":
            subprocess.run([sys.executable, "site/scripts/tutorial_check.py", "--run"], cwd=ROOT, check=True)
            result = {**identity, "outcome": "passed", "scope": "integration",
                      "architecture": architecture, "selectedScenario": selected,
                      "selectedLanguages": list(languages), "productionCleanupVerified": True,
                      "fixtureDestruction": {"outcome": "passed"},
                      "assertions": [{"name": "all published tutorial shell steps including owned cleanup",
                                      "outcome": "passed", "requirements": []}]}
        else:
            work = ROOT / "integration-tests/live-v2" / f"run-qualify-{invocation}-{suite}"
            try:
                command = [sys.executable, str(ROOT / "integration-tests/live-v2" / script),
                           "--work", str(work)]
                if suite == "control":
                    command += ["--scenario", "control"]
                subprocess.run(command, cwd=ROOT, check=True)
            finally:
                if (work / "result.json").is_file():
                    public = export(read_json(work / "result.json"))
                    (output / f"candidate-{architecture}-{suite}.json").write_text(json.dumps(public, indent=2) + "\n")
            result = read_json(work / "result.json")
        problems = suite_errors(result, suite, architecture, expected=identity)
        if problems or working_identity() != identity:
            raise ValueError(suite + ": " + "; ".join(problems or ["candidate changed during suite"]))
        (output / f"candidate-{architecture}-{suite}.json").write_text(json.dumps(export(result), indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", action="store_true", help="explicitly execute privileged disposable native suites")
    parser.add_argument("--scenario", choices=("complete", "control", "managed", "managed-wasm"), default="complete")
    parser.add_argument("--architecture", choices=("amd64", "arm64"), default="amd64")
    parser.add_argument("--results", type=Path, default=ROOT / "acceptance/results")
    args = parser.parse_args()
    try:
        if args.run:
            run(args.scenario, args.architecture, ROOT / "acceptance-export")
            return 0
        errors = validate_suites(args.results)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print("qualification failed:", error, file=sys.stderr)
        return 1
    for error in errors:
        print(error)
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(main())
