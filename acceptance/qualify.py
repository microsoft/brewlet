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


PR_LANGUAGES = {"all": NATIVE, "python": ("python",)}


def pr_errors(result, architecture, root=ROOT, expected=None, languages="all"):
    if not isinstance(result, dict):
        return ["PR receipt must be an object"]
    if languages not in PR_LANGUAGES:
        return ["unsupported PR language selection"]
    selected = PR_LANGUAGES[languages]
    errors = receipt_errors(result, live=True, architecture=architecture, scenario="pr",
                            languages=selected, root=root, expected=expected)
    assertions = result.get("assertions")
    passed = {row["name"] for row in assertions
              if isinstance(row, dict) and isinstance(row.get("name"), str) and row.get("outcome") == "passed"
              } if isinstance(assertions, list) else set()
    for language in selected:
        for name in (
                language + " real startup, HTTP, dependency, exec/probe, identity and readonly mounts",
                language + " source rotation, unchanged app, retained task/exec lease, new barrier",
                language + " actual protected CRI identity resolves verified native descriptors and config"):
            if name not in passed:
                errors.append("missing PR assertion: " + name)
    if result.get("fullAcceptance") is not False:
        errors.append("PR execution must not claim full promotion acceptance")
    return errors


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


def suite_statuses(directory, root=ROOT, expected=None):
    statuses = []
    expected = expected or working_identity(root)
    for architecture in ("amd64", "arm64"):
        for suite in SUITES:
            name = f"candidate-{architecture}-{suite}.json"
            row = {"file": name, "suite": suite, "architecture": architecture}
            try:
                result = read_json(Path(directory) / name)
                if not isinstance(result, dict):
                    raise ValueError("receipt must be an object")
                row.update(executionOutcome=result.get("outcome"),
                           identity={key: result.get(key) for key in expected})
                problems = suite_errors(result, suite, architecture, root, expected)
                row.update(status="failed" if problems else "qualified",
                           errors=problems)
            except FileNotFoundError:
                row.update(status="missing", errors=["unavailable receipt"])
            except (OSError, ValueError) as error:
                row.update(status="failed", errors=["unavailable/invalid receipt: " + str(error)])
            statuses.append(row)
    return statuses


def validate_suites(directory, root=ROOT, expected=None):
    return [row["file"] + ": " + error
            for row in suite_statuses(directory, root, expected) for error in row["errors"]]


def run(scenario, architecture, output):
    machine = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    if platform.system() != "Linux" or machine != architecture:
        raise ValueError("qualification requires the selected native Linux architecture, not emulation")
    identity = working_identity()
    output = output.resolve()
    if output != ROOT.resolve() / "acceptance-export":
        raise ValueError("qualification output must be the isolated ignored acceptance-export directory")
    output.mkdir(exist_ok=True)
    suites = COMPLETE if scenario == "complete" else (scenario,)
    if any((output / f"candidate-{architecture}-{suite}.json").exists() for suite in suites):
        raise ValueError("refusing to overwrite existing suite receipts; preserve the prior attempt first")
    invocation = uuid.uuid4().hex[:12]
    for suite in suites:
        script, selected, languages = (SUITES | DEBUG_SUITES)[suite]
        result = {**identity, "outcome": "failed",
                  "scope": "integration" if suite == "tutorial" else "real-node-execution-only",
                  "architecture": architecture, "selectedScenario": selected,
                  "selectedLanguages": list(languages),
                  "assertions": [{"name": "selected suite execution", "outcome": "failed",
                                  "requirements": []}]}
        try:
            if suite == "tutorial":
                subprocess.run([sys.executable, "site/scripts/tutorial_check.py", "--run"], cwd=ROOT, check=True)
                result.update(outcome="passed", productionCleanupVerified=True,
                              fixtureDestruction={"outcome": "passed"},
                              assertions=[{"name": "all published tutorial shell steps including owned cleanup",
                                           "outcome": "passed", "requirements": []}])
            else:
                work = ROOT / "integration-tests/live-v2" / f"run-qualify-{invocation}-{suite}"
                command = [sys.executable, str(ROOT / "integration-tests/live-v2" / script),
                           "--work", str(work)]
                if suite == "control":
                    command += ["--scenario", "control"]
                try:
                    subprocess.run(command, cwd=ROOT, check=True)
                finally:
                    if (work / "result.json").is_file():
                        observed = read_json(work / "result.json")
                        if (not isinstance(observed, dict) or not isinstance(observed.get("assertions"), list)
                                or not all(isinstance(row, dict) for row in observed["assertions"])):
                            raise ValueError("suite result must be an object with assertion records")
                        result = observed
            problems = suite_errors(result, suite, architecture, expected=identity)
            if problems or working_identity() != identity:
                raise ValueError(suite + ": " + "; ".join(problems or ["candidate changed during suite"]))
        except (OSError, ValueError, subprocess.CalledProcessError):
            result["outcome"] = "failed"
            result["cleanSelectedSuitePassed"] = False
            result.setdefault("assertions", []).append({
                "name": "selected suite execution and candidate validation",
                "outcome": "failed", "requirements": []})
            raise
        finally:
            payload = json.dumps(export(result), indent=2) + "\n"
            with (output / f"candidate-{architecture}-{suite}.json").open("x") as receipt:
                receipt.write(payload)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--run", action="store_true", help="explicitly execute privileged disposable native suites")
    mode.add_argument("--status-json", action="store_true", help="report all 22 receipt statuses and candidate identities")
    mode.add_argument("--verify-pr", type=Path, help="verify actual PR E2E execution, never promotion evidence")
    parser.add_argument("--scenario", choices=("complete", "control", "managed", "managed-wasm"), default="complete")
    parser.add_argument("--architecture", choices=("amd64", "arm64"), default="amd64")
    parser.add_argument("--languages", choices=tuple(PR_LANGUAGES), default="all",
                        help="--verify-pr language selection: all four (default) or the Python-only PR scope")
    parser.add_argument("--results", type=Path, default=ROOT / "acceptance/results")
    args = parser.parse_args()
    try:
        if args.verify_pr:
            errors = pr_errors(read_json(args.verify_pr), args.architecture, languages=args.languages)
            for error in errors:
                print(error, file=sys.stderr)
            return int(bool(errors))
        if args.run:
            run(args.scenario, args.architecture, ROOT / "acceptance-export")
            return 0
        if args.status_json:
            identity = working_identity()
            statuses = suite_statuses(args.results, expected=identity)
            print(json.dumps({"candidate": identity, "receipts": statuses}, indent=2))
            return int(any(row["status"] != "qualified" for row in statuses))
        errors = validate_suites(args.results)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print("qualification failed:", error, file=sys.stderr)
        return 1
    for error in errors:
        print(error)
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(main())
