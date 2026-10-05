#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Export selected, redacted tier logs, never the private work directory."""

import argparse
import json
from pathlib import Path
import re


def redact(text):
    text = re.sub(r"-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----",
                  "[REDACTED PRIVATE KEY]", text, flags=re.S)
    text = re.sub(r"(?i)(https?://)[^/\s:@]+:[^/\s@]+@", r"\1[REDACTED]@", text)
    text = re.sub(r"<(password|token|secret|auth)\b[^>]*>.*?</\1>",
                  "[REDACTED CREDENTIAL LINE]", text, flags=re.I | re.S)
    text = re.sub(
        r"^([ \t]*)(?:password|token|client-key-data|client-certificate-data|"
        r"certificate-authority-data|secret):[^\n]*\n(?:\1[ \t]+[^\n]*(?:\n|$))+",
        "[REDACTED CREDENTIAL BLOCK]\n", text, flags=re.I | re.M)
    # Drop whole credential-bearing lines, including structured and command output.
    return re.sub(
        r"^.*(?:authorization|password|passwd|token|client-key|client-certificate|"
        r"certificate-authority-data|private.key|\"auth\"|<auth>|secret).*$",
        "[REDACTED CREDENTIAL LINE]", text, flags=re.I | re.M)


def collect(work, output):
    if output.resolve() == work.resolve() or work.resolve() in output.resolve().parents:
        raise ValueError("diagnostics output must be outside the private work directory")
    output.mkdir(parents=True, exist_ok=False)
    names = []
    if work.exists():
        for path in sorted(work.iterdir()):
            if not (path.name == "runner.log" or
                    re.fullmatch(r"diag-[\w.-]+\.log|t\d+-fixture-teardown\.log", path.name)):
                continue
            if path.is_symlink() or not path.is_file():
                raise ValueError(f"refusing non-regular diagnostic: {path.name}")
            (output / path.name).write_text(redact(path.read_text()))
            names.append(path.name)
    (output / "manifest.json").write_text(json.dumps({"logs": names, "workPresent": work.exists()}) + "\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("work", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    collect(args.work, args.output)
