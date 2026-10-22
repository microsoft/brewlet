#!/usr/bin/env python3
"""Execute fixed real producer checks on their actual native Linux architecture."""

import argparse
from pathlib import Path
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from acceptance.native_contracts import ROOT, native_architecture


def command(name, architecture):
    if name not in ("fixtures", "registry") or architecture not in ("amd64", "arm64"):
        raise ValueError("explicit supported producer check and native architecture required")
    base = ["python3", "integration-tests/" + ("fixtures/check.py" if name == "fixtures" else "registry.py"),
            "--brewlet", "./bin/brewlet"]
    if name == "registry":
        base += ["--registry-platform", "linux/" + architecture]
    return base


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("check", choices=("fixtures", "registry"))
    args = parser.parse_args()
    architecture = native_architecture()
    metadata = subprocess.run(["go", "version", "-m", str(ROOT / "bin/brewlet")],
                              cwd=ROOT, check=True, text=True, stdout=subprocess.PIPE).stdout.split()
    if "GOOS=linux" not in metadata or "GOARCH=" + architecture not in metadata:
        raise ValueError("checkout-built producer CLI does not match the native Linux runner")
    subprocess.run(command(args.check, architecture), cwd=ROOT, check=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
