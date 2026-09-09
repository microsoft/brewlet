#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Recompute benchmark summaries and reject incomplete or inconsistent data."""

import argparse
import json
import math
from pathlib import Path
import statistics


MIB = 1024 * 1024
VARIANTS = ("conventional", "brewlet")


def require(condition, message):
    if not condition:
        raise ValueError(message)


def distribution(values):
    require(bool(values), "Cannot summarize an empty sample")
    require(all(isinstance(value, (int, float)) and math.isfinite(value) for value in values),
            "Samples must be finite numbers")
    quartiles = statistics.quantiles(values, n=4, method="inclusive") if len(values) > 1 else values * 3
    return {
        "n": len(values), "median": statistics.median(values),
        "q1": quartiles[0], "q3": quartiles[2],
        "min": min(values), "max": max(values),
    }


def image_objects(images, *names):
    objects = {}
    for name in names:
        image = images[name]
        descriptors = [
            {"digest": image["manifest_digest"], "size": image["manifest_bytes"]},
            image["config"], *image["layers"],
        ]
        for descriptor in descriptors:
            digest, size = descriptor["digest"], descriptor["size"]
            require(isinstance(size, int) and size >= 0, "Invalid descriptor size")
            require(digest not in objects or objects[digest] == size, "One digest has different sizes")
            objects[digest] = size
    return objects


def missing_objects(images, wanted, cached=()):
    present = image_objects(images, *cached)
    return {digest: size for digest, size in image_objects(images, *wanted).items()
            if digest not in present}


def storage_summary(storage):
    images = storage["images"]
    cases = {
        "empty_cache": {
            "conventional": (("conventional",), ()),
            "fat": (("fat",), ()),
            "brewlet_app_plus_runtime": (("brewlet", "jdk"), ()),
        },
        "runtime_cached": {
            name: ((name,), ("jdk",)) for name in (*VARIANTS, "fat")
        },
        "second_identical_app_or_replica": {
            name: ((name,), (name,)) for name in (*VARIANTS, "fat")
        },
        "resource_revision": {
            name: ((name + "-revision",), (name,)) for name in (*VARIANTS, "fat")
        },
        "runtime_build_update": {
            "conventional_linked_rebase": (("conventional-linked",), ("conventional-linked-old-jdk",)),
            "conventional_standard_copy_rebuild": (("conventional",), ("conventional-old-jdk",)),
            "brewlet_runtime_plus_unchanged_app": (("jdk", "brewlet"), ("jdk-old", "brewlet")),
            "brewlet_app_only": (("brewlet",), ("brewlet",)),
        },
    }
    result = {}
    for case, variants in cases.items():
        result[case] = {}
        for name, (wanted, cached) in variants.items():
            objects = missing_objects(images, wanted, cached)
            declared = storage["scenarios"][case][name]
            require(set(declared["digests"]) == set(objects), f"Digest-set mismatch: {case}/{name}")
            size = sum(objects.values())
            require(declared["bytes"] == size and declared["unique_objects"] == len(objects),
                    f"Byte/count mismatch: {case}/{name}")
            result[case][name] = {"bytes": size, "MiB": size / MIB, "unique_objects": len(objects)}
    components = missing_objects(images, ("operator", "admission", "provisioner"), ("jdk", "brewlet"))
    declared = storage["scenarios"]["brewlet_components_additional"]
    require(set(declared["digests"]) == set(components)
            and declared["bytes"] == sum(components.values())
            and declared["unique_objects"] == len(components), "Component-byte mismatch")
    result["brewlet_components_additional"] = {
        "bytes": sum(components.values()), "MiB": sum(components.values()) / MIB,
    }
    return result


def validate_runtime(runtime):
    require(not runtime["failures"], "Runtime failures must not be silently excluded")
    identities = set()
    contract = runtime["verified_launch_contract"]
    for trial in runtime["trials"] + runtime["cohorts"]:
        require(trial["status"] == "ok", "An unsuccessful trial cannot be summarized as complete")
        require(trial["variant"] in VARIANTS, "Unknown variant")
        require(len(trial["pods"]) == trial["size"], "Incomplete Pod sample")
        containers, shims = trial["memory"]["containers"], trial["memory"]["shims"]
        require(len(containers) == trial["size"] and len(shims) == trial["size"],
                "Incomplete JVM/shim sample")
        processes = containers + shims
        require(len({process["pid"] for process in processes}) == len(processes),
                "Duplicate process would double-count memory")
        require(all(process["smaps_available"] for process in processes), "PSS is unavailable")
        for container in containers:
            require(container["verified_launch_contract"], "Launch contract was not verified")
            require(all(int(uid) == contract["uid"] for uid in container["uid"]), "Process UID mismatch")
            require(container["oci_readonly_root"] == contract["readonly_root"], "Root policy mismatch")
            require(container["cgroup_values"]["cpu.max"] == contract["cpu_max"], "CPU limit mismatch")
            require(int(container["cgroup_values"]["memory.max"]) == contract["memory_max_bytes"],
                    "Memory limit mismatch")
            expected = "io.containerd.brewlet.v2" if trial["variant"] == "brewlet" else "io.containerd.runc.v2"
            require(container["runtime_type"] == expected, "Wrong runtime handler")
            require(container["default_cds_mapped"], "Default CDS state differs")
            identities.add((container["jdk_release_sha256"], container["java_executable_sha256"]))
    require(len(identities) == 1, "The variants did not run the same JDK")
    trials = [trial for trial in runtime["trials"] if not trial["warmup"]]
    pairs = {}
    for trial in trials:
        require(trial["size"] == 1, "Startup comparison requires single-Pod trials")
        pair = pairs.setdefault(trial["pair"], {})
        require(trial["variant"] not in pair, "Duplicate variant in a pair")
        pair[trial["variant"]] = trial
    require(bool(pairs) and all(set(pair) == set(VARIANTS) for pair in pairs.values()),
            "Unbalanced trial pairs")
    return trials, pairs


def memory_summary(rows):
    def total(row, group, key):
        return sum(process[key] for process in row["memory"][group]) / MIB
    return {
        "jvm_rss_MiB": distribution([total(row, "containers", "rss_bytes") for row in rows]),
        "jvm_pss_MiB": distribution([total(row, "containers", "pss_bytes") for row in rows]),
        "shim_rss_MiB": distribution([total(row, "shims", "rss_bytes") for row in rows]),
        "shim_pss_MiB": distribution([total(row, "shims", "pss_bytes") for row in rows]),
        "jvm_plus_shim_pss_MiB": distribution([
            total(row, "containers", "pss_bytes") + total(row, "shims", "pss_bytes") for row in rows]),
        "container_memory_current_MiB": distribution([
            sum(int(process["cgroup_values"]["memory.current"])
                for process in row["memory"]["containers"]) / MIB for row in rows]),
    }


def analyze(data):
    runtime = data["runtime"]
    require(data["schema_version"] == runtime["schema_version"] == data["storage"]["schema_version"] == 1,
            "Unsupported result schema")
    require(data["storage"]["units"] == "bytes", "Unexpected storage units")
    trials, pairs = validate_runtime(runtime)
    result = {
        "source_commit": runtime["metadata"]["source_commit"],
        "quartile_method": "statistics.quantiles(n=4, method='inclusive')",
        "warmups": sum(trial["warmup"] for trial in runtime["trials"]),
        "startup_seconds": {}, "single_Pod_memory": {}, "four_JVM_cohort_memory": {},
    }
    for name in VARIANTS:
        rows = [trial for trial in trials if trial["variant"] == name]
        startup = {
            field: distribution([trial["pods"][0][field] for trial in rows])
            for field in ("spring_startup_seconds", "spring_process_seconds",
                          "creation_to_ready_seconds", "container_start_to_ready_seconds")
        }
        startup["host_create_to_observed_ready_seconds"] = distribution([
            trial["host_create_to_observed_ready_seconds"] for trial in rows])
        result["startup_seconds"][name] = startup
        result["single_Pod_memory"][name] = memory_summary(rows)
        cohorts = [trial for trial in runtime["cohorts"] if trial["variant"] == name]
        require(cohorts and all(trial["size"] == 4 for trial in cohorts), "Expected four-JVM cohorts")
        result["four_JVM_cohort_memory"][name] = memory_summary(cohorts)
    result["paired_spring_startup_delta_seconds_brewlet_minus_conventional"] = distribution([
        pair["brewlet"]["pods"][0]["spring_startup_seconds"]
        - pair["conventional"]["pods"][0]["spring_startup_seconds"] for pair in pairs.values()])
    platform = runtime["control_plane_idle_detailed"]
    processes = platform["containers"] + platform["shims"]
    processes += [child for process in platform["containers"] for child in process.get("descendant_processes", [])]
    require(len({process["pid"] for process in processes}) == len(processes), "Duplicate platform process")
    result["separate_platform_idle_process_PSS_MiB"] = sum(process["pss_bytes"] for process in processes) / MIB
    result["transfer_payload"] = storage_summary(data["storage"])
    require(not data["cleanup"]["failures"], "Cleanup failures require explicit investigation")
    result["cleanup_verified"] = all(data["cleanup"][key] for key in (
        "profile_finalization_succeeded", "helm_uninstall_succeeded", "registry_removed", "node_removed"))
    require(result["cleanup_verified"], "Normal cleanup was not verified")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("results", type=Path)
    args = parser.parse_args()
    print(json.dumps(analyze(json.loads(args.results.read_text())), indent=2))


if __name__ == "__main__":
    main()
