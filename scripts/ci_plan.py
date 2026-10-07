#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Conservative PR impact selection and validation of the aggregate CI result."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys


JOBS = ("core", "kubernetes", "admission", "helm", "containers", "maven",
        "notices", "docs", "host", "live")


def plan(paths, full=False):
    selected = set()
    host = set()
    integrity = maven_release = False
    paths = list(paths)
    reasons = []
    for path in paths:
        if (path.startswith((".github/workflows/", "scripts/", "specs/"))
                or path in ("Makefile", ".dockerignore", ".gitmodules")):
            full = True
            reasons.append(f"shared CI/build/contract input: {path}")
            continue
        if (path.startswith(("docs/", "site/"))
                or ("/" not in path and path.endswith(".md"))
                or Path(path).name in ("README.md", "AGENTS.md")):
            selected.add("docs")
        elif path.startswith("core/"):
            # Kubernetes CLI integration builds and executes this module's CLI.
            selected.update(("core", "kubernetes", "admission", "containers", "notices", "live"))
            host.update((2, 3))
            if path.startswith(("core/internal/registry/", "core/internal/artifact/", "core/pkg/")):
                selected.add("maven")
            if path.endswith(("go.mod", "go.sum")):
                selected.update(("kubernetes", "helm", "maven"))
        elif path.startswith("kubernetes/"):
            selected.update(("kubernetes", "helm", "containers", "notices", "live"))
            # Preserve publisher compatibility coverage alongside the deployment smoke.
            if path.startswith(("kubernetes/api/", "kubernetes/charts/", "kubernetes/deploy/")):
                selected.add("maven")
            if path.endswith(("go.mod", "go.sum")):
                selected.add("maven")
        elif path.startswith("provisioner/"):
            selected.update(("core", "containers", "live"))
            host.add(3)
            integrity |= (path in ("provisioner/Dockerfile", "provisioner/dockerfile_test.sh",
                                   "provisioner/download-verified.sh")
                          or path.startswith("provisioner/checksums/"))
        elif path.startswith("admission/"):
            selected.add("admission")
        elif path.startswith("maven-plugin/"):
            selected.update(("maven", "live"))
            host.add(2)
            maven_release = True
        elif path.startswith("integration-tests/"):
            # Harness and fixture dependencies cross tiers; do not guess a subset.
            full = True
            reasons.append(f"shared integration fixture: {path}")
        elif path.startswith(".github/extensions/e2e-monitor/"):
            # Its offline suite runs unconditionally in safeguards.
            pass
        elif path in ("LICENSE.txt", "NOTICE.txt"):
            selected.update(("notices", "docs", "maven", "containers"))
            maven_release = True
        else:
            full = True
            reasons.append(f"unclassified path: {path}")

    if full or not paths:
        selected.update(JOBS)
        host.update((2, 3))
        integrity = True
        maven_release = True
        reasons.append("full CI coverage (component checks and smoke only)")
    if host:
        selected.add("host")
    codeql = []
    if selected.intersection(("core", "kubernetes", "admission")):
        codeql.append({"language": "go", "build-mode": "autobuild"})
    if "maven" in selected:
        codeql.append({"language": "java", "build-mode": "none"})
    return {
        **{job: job in selected for job in JOBS},
        "codeql": bool(codeql),
        "codeql_matrix": {"include": codeql},
        "image_integrity": integrity,
        "maven_release": maven_release,
        "host_matrix": sorted(host) or [2],
        "reasons": reasons,
    }


def changed_paths(base, head):
    for revision in (base, head):
        if not re.fullmatch(r"[0-9a-f]{40}", revision):
            raise ValueError("PR base and head must be full commit SHAs")
    # Disable rename detection so both removed and added paths select their owners.
    result = subprocess.run(
        ["git", "diff", "--no-renames", "--name-only", "-z", f"{base}...{head}"],
        check=True, stdout=subprocess.PIPE)
    return [p.decode("utf-8") for p in result.stdout.split(b"\0") if p]


def check_results(needs):
    expected = {"changes", "safeguards", *JOBS}
    if set(needs) != expected:
        raise ValueError(f"aggregate dependencies differ: {set(needs) ^ expected}")
    for job in ("changes", "safeguards"):
        if needs[job]["result"] != "success":
            raise ValueError(f"{job}: expected success, got {needs[job]['result']}")
    outputs = needs["changes"]["outputs"]
    for job in JOBS:
        if outputs.get(job) not in ("true", "false"):
            raise ValueError(f"missing or invalid selection for {job}")
        wanted = "success" if outputs[job] == "true" else "skipped"
        actual = needs[job]["result"]
        if actual != wanted:
            raise ValueError(f"{job}: expected {wanted}, got {actual}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("plan", "gate"))
    args = parser.parse_args()
    if args.command == "gate":
        check_results(json.loads(os.environ["CI_NEEDS"]))
        print("All selected PR checks passed; only unselected jobs were skipped.")
        return
    paths = []
    full = os.environ["GITHUB_EVENT_NAME"] != "pull_request"
    if not full:
        try:
            paths = changed_paths(os.environ["PR_BASE_SHA"], os.environ["PR_HEAD_SHA"])
        except (KeyError, ValueError, UnicodeError, subprocess.CalledProcessError) as error:
            print(f"::warning::Cannot determine PR impact; selecting full coverage: {error}",
                  file=sys.stderr)
            full = True
    result = plan(paths, full=full)
    print(json.dumps({"paths": paths, **result}, indent=2))
    with Path(os.environ["GITHUB_OUTPUT"]).open("a") as output:
        for key, value in result.items():
            if key != "reasons":
                output.write(f"{key}={json.dumps(value, separators=(',', ':'))}\n")
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with Path(summary).open("a") as output:
            output.write("### CI impact selection\n\n```json\n"
                         + json.dumps(result, indent=2) + "\n```\n")


if __name__ == "__main__":
    main()
