#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Export compact, sanitized raw measurements; full diagnostics remain in WORK."""
import argparse
import json
from pathlib import Path


def export_results(work, output):
    runtime = json.loads((work / "runtime-raw.json").read_text())
    launch = json.loads((work / "matched/launch.json").read_text())
    release = next((c["jdk_release"] for t in runtime["trials"]
                    for c in t.get("memory", {}).get("containers", [])
                    if c.get("jdk_release")), None)
    runtime["verified_launch_contract"] = {
        "command": launch["command"], "jdk_release": release,
        "uid": 65532, "readonly_root": True, "cpu_max": "100000 100000",
        "memory_max_bytes": 536870912, "payload_files": launch["files"]}
    for record in [*runtime["trials"], *runtime["cohorts"],
                   {"memory": runtime.get("control_plane_idle", {})},
                   {"memory": runtime.get("control_plane_idle_detailed", {})}]:
        for item in record.get("memory", {}).get("containers", []):
            is_app = item.get("jdk_release") is not None
            if is_app:
                verified = (item.get("cmdline") == launch["command"] and
                            item.get("jdk_release_sha256") == runtime.get("verified_jdk_identity", [None])[0]
                            and item.get("java_executable_sha256") ==
                                runtime.get("verified_jdk_identity", [None, None])[1]
                            and item.get("runtime_environment") == runtime.get("verified_runtime_environment")
                            and item.get("uid") == ["65532"] * 4 and item.get("oci_readonly_root") is True
                            and item.get("cgroup_values", {}).get("cpu.max") == "100000 100000"
                            and item.get("cgroup_values", {}).get("memory.max") == "536870912")
                item.pop("cmdline", None)
                item.pop("jdk_release", None)
                item.pop("runtime_environment", None)
                item["verified_launch_contract"] = verified
                item["default_cds_mapped"] = bool(item.pop("cds_mappings", []))
            item.pop("cgroup", None)
            values = item.get("cgroup_values", {})
            for key in ["memory.stat", "cpu.stat"]:
                values.pop(key, None)
    data = {"schema_version": 1, "runtime": runtime,
            "storage": json.loads((work / "storage-raw.json").read_text()),
            "cleanup": json.loads((work / "cleanup.json").read_text()),
            "limitations": [
                "Single shared Docker Desktop VM; no significance or tail-latency claims.",
                "Kubernetes ready/start timestamps have one-second resolution; Spring reports separate finer application timing.",
                "Image-warm fresh JVMs, not cold-node or cold-registry startup.",
                "Optimized conventional means layered application packaging, not the smallest possible runtime. Both primary variants use the same full JDK and source-image userland; JRE-only and jlink alternatives are not measured.",
                "JVM PSS, per-container charge, and runtime-shim PSS are distinct accounting views.",
                "RSS must not be summed as physical memory; neither design shares JVM heaps.",
                "No custom AppCDS on either variant; default JDK CDS verified in process mappings.",
                "Compressed OCI object bytes are not observed wire traffic, elapsed transfer time, or physical disk savings.",
                "The JDK build-update experiment is separate from the primary matched-JDK comparison; it does not verify CVE remediation.",
                "Deferred managed-tar/signer, Ratify/HPA, and Node UID-loss recovery work is not validated.",
            ]}
    if "failures" not in data["cleanup"]:
        raise ValueError("Cleanup must provide an explicit failures list; completion cannot be inferred")
    if "control_plane_idle_detailed" not in runtime:
        runtime["control_plane_idle_detailed"] = None
        runtime["failures"].append({"stage": "idle_collection",
                                    "error": "Detailed idle metrics unavailable; run is incomplete"})
    setup_file = work / "preflight-notes.json"
    if setup_file.exists():
        data["preflight"] = json.loads(setup_file.read_text())
    policy = work / "policy-update-raw.json"
    if policy.exists():
        data["runtime_policy_update"] = json.loads(policy.read_text())
    # Diagnostics may contain local paths; no environment dump is exported.
    def sanitize(value):
        if isinstance(value, str):
            return value.replace(str(work.resolve()), "<work>").replace(str(Path.home()), "<home>")
        if isinstance(value, dict):
            return {key: sanitize(item) for key, item in value.items()}
        if isinstance(value, list):
            return [sanitize(item) for item in value]
        return value
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(sanitize(data), indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    export_results(args.work, args.output)
    print(args.output)


if __name__ == "__main__":
    main()
