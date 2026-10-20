#!/usr/bin/env python3
"""Export only public identities/outcomes, never private kubeconfigs or keys."""

import argparse
import json
from pathlib import Path


def export(source):
    fields = (
        "outcome", "scope", "selectedScenario", "selectedLanguages", "cleanSelectedSuitePassed", "fullAcceptance",
        "checkout", "productionSourceManifestSHA256", "architecture",
        "kubeletPullVerified", "tlsTransportVerified", "providerScaleFromZeroVerified",
        "kubeletRuntimeRequestTimeoutSeconds",
        "pins", "applicationImages", "runtimeImages", "runtimeVendorInputs", "fixtureDestruction", "gaps",
        "upstreamRuntimes",
        "wasmGuestInputs",
    )
    result = {key: source[key] for key in fields if key in source}
    if "wasmRuntimes" in source:
        public = {"releaseVersion", "archive", "binarySHA256", "descriptor",
                  "base", "interpreter", "sourceKind", "image"}
        result["wasmRuntimes"] = {
            name: {revision: {key: value for key, value in record.items()
                              if key in public}
                   for revision, record in revisions.items()}
            for name, revisions in source["wasmRuntimes"].items()
        }
    result["assertions"] = [
        {key: item[key] for key in ("name", "outcome", "requirements") if key in item}
        for item in source["assertions"]
    ]
    if source.get("interventions"):
        result["cleanSelectedSuitePassed"] = False
        result["outcome"] = "intervened"
    return result


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    result = export(json.loads(args.source.read_text()))
    args.output.write_text(json.dumps(result, indent=2) + "\n")
