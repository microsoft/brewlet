"""Authenticate workflow-run provenance before downloading a candidate or receipts."""

import argparse
import json
import re
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from scripts.release import require, run


def validate(value, workflow, source):
    require(value["repository"]["full_name"] == "microsoft/brewlet"
            and value["head_repository"]["full_name"] == "microsoft/brewlet"
            and value["event"] == "workflow_dispatch"
            and value["path"] == ".github/workflows/" + workflow
            and value["head_sha"] == source
            and value["status"] == "completed" and value["conclusion"] == "success",
            "workflow artifacts must come from an exact successful same-repository/source manual run")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_id")
    parser.add_argument("--workflow", required=True, choices=("release-build.yml", "release-transition.yml"))
    parser.add_argument("--source", required=True)
    args = parser.parse_args()
    require(re.fullmatch(r"[1-9][0-9]*", args.run_id) and re.fullmatch(r"[0-9a-f]{40}", args.source),
            "invalid immutable run/source identity")
    value = json.loads(run(["ghx", "api", f"repos/microsoft/brewlet/actions/runs/{args.run_id}"]))
    validate(value, args.workflow, args.source)
