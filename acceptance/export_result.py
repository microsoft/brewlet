#!/usr/bin/env python3
"""Export only public identities/outcomes, never private kubeconfigs or keys."""

import argparse
import json
from pathlib import Path


def lifecycle_observations(source):
    fixtures = ("admission-held", "admission-conflict", "startup-zero", "empty-profile")
    phases = ("restore", "deletion", "hold-release", "absence", "deleting-observation")
    booleans = (
        "present", "uidMatches", "ownerMatches", "generationMatches", "specMatches",
        "ownershipInitialized", "ownershipInitializedPresent", "provisioningGenerationMatches",
        "provisioningSpecMatches", "targetsValid", "targetsEmpty", "retirementPresent",
        "detachedRetirementsPresent", "deleting", "observationHoldPresent",
        "preDeleteWitnessPresent",
    )
    integers = ("generation", "expectedGeneration", "provisioningGeneration", "expectedProvisioningGeneration")
    observations = {}
    if isinstance(source, dict):
        for fixture in fixtures:
            record = source.get(fixture)
            if not isinstance(record, dict):
                continue
            observations[fixture] = {}
            for phase in phases:
                row = record.get(phase)
                if not isinstance(row, dict):
                    continue
                observations[fixture][phase] = {
                    key: value for key, value in row.items()
                    if (key in booleans and type(value) is bool
                        or key in integers and (type(value) is int or value is None))}
    return observations


def fixture_lifecycle(source):
    """Strict public projection; exception text and raw Kubernetes data stay private."""
    errors = []
    raw_errors = source.get("errors", [])
    if isinstance(raw_errors, list):
        for row in raw_errors:
            if (isinstance(row, dict) and row.get("role") in ("primary", "secondary")
                    and row.get("stage") in ("assertions", "conflict-cleanup", "held-cleanup", "webhook-restore")
                    and row.get("code") in ("timeout", "command", "ownership", "obligations", "contract", "unexpected")):
                record = {key: row[key] for key in ("role", "stage", "code")}
                if "observations" in row:
                    record["observations"] = lifecycle_observations(row["observations"])
                errors.append(record)
    return {"observations": lifecycle_observations(source.get("observations")), "errors": errors}


def export(source):
    fields = (
        "outcome", "scope", "selectedScenario", "selectedLanguages", "cleanSelectedSuitePassed", "fullAcceptance",
        "checkout", "productionSourceManifestSHA256", "architecture",
        "identityVersion", "qualificationInputsSHA256",
        "nativeExecution",
        "productionCleanupVerified",
        "command",
        "kubeletPullVerified", "tlsTransportVerified", "providerScaleFromZeroVerified",
        "kubeletRuntimeRequestTimeoutSeconds",
        "pins", "applicationImages", "runtimeImages", "runtimeVendorInputs", "fixtureDestruction", "gaps",
        "upstreamRuntimes",
        "wasmGuestInputs",
    )
    result = {key: source[key] for key in fields if key in source}
    if isinstance(source.get("fixtureLifecycle"), dict):
        result["fixtureLifecycle"] = fixture_lifecycle(source["fixtureLifecycle"])
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
