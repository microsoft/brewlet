#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Paired fresh-JVM CRI benchmark; requires an explicitly isolated kind cluster."""
import argparse
import datetime
import json
from pathlib import Path
import random
import re
import subprocess
import time
from resources import Ledger


def collect_idle(work, node, kubectl, remote="/bench-collect.py"):
    control = json.loads(run(kubectl + ["get", "pods", "-n", "brewlet", "-o", "json"]).stdout)
    ids = [c["containerID"].split("://", 1)[1] for p in control["items"]
           for c in p["status"].get("containerStatuses", []) if "running" in c["state"]]
    if not ids:
        raise RuntimeError("No running control-plane containers available for idle measurement")
    return json.loads(run(["docker", "exec", "-i", node, "python3", remote, "--descendants"],
                          data=json.dumps(ids)).stdout)


def run(argv, *, data=None, check=True):
    result = subprocess.run(argv, input=data, text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE)
    if check and result.returncode:
        raise RuntimeError(f"{argv[0]} failed: {result.stderr[-2000:]}")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", required=True, type=Path)
    parser.add_argument("--node", required=True)
    parser.add_argument("--kubeconfig", required=True)
    parser.add_argument("--trials", type=int, default=12)
    parser.add_argument("--cohorts", type=int, default=3)
    parser.add_argument("--seed", type=int, default=781005)
    args = parser.parse_args()
    if args.trials < 0 or args.cohorts < 0 or (args.cohorts and not args.trials):
        raise SystemExit("Trials/cohorts must be nonnegative; cohorts require paired trials")
    work = args.work
    ledger = Ledger(work)
    ledger.verify_node(args.node, args.kubeconfig)
    setup = json.loads((work / "setup.json").read_text())
    launch = json.loads((work / "matched/launch.json").read_text())
    kubectl = ["kubectl", "--kubeconfig", args.kubeconfig]
    node = ["docker", "exec", args.node]
    context = run(kubectl + ["config", "current-context"]).stdout.strip()
    if context != "kind-" + args.node.removesuffix("-control-plane"):
        raise SystemExit("Kubeconfig context does not match the dedicated node")
    remote = "/bench-collect.py"
    run(["docker", "cp", str(Path(__file__).with_name("collect-node.py")), args.node + ":" + remote])
    result = {"schema_version": 1, "metadata": setup, "seed": args.seed,
              "cache_definition": "fresh JVM on image-warm node; no cache eviction",
              "readiness_period_seconds": 1, "settle_seconds": 10,
              "http_initialization": ["/", "/owners/find"] * 5,
              "flags": launch["flags"], "trials": [], "cohorts": [], "failures": []}

    def save():
        staged = work / "runtime-raw.next.json"
        staged.write_text(json.dumps(result, indent=2) + "\n")
        staged.replace(work / "runtime-raw.json")

    def pod(name, variant):
        annotations = {"brewlet.sh/jdk": "temurin-21",
                       "brewlet.sh/jvm-args": json.dumps(launch["flags"])} if variant == "brewlet" else {}
        spec = {"restartPolicy": "Never", "terminationGracePeriodSeconds": 5,
                "nodeName": args.node, "automountServiceAccountToken": False,
                "securityContext": {"runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532},
                "containers": [{"name": "app", "image": setup["images"][variant],
                    "imagePullPolicy": "Never",
                    "env": [{"name": "HOME", "value": "/"},
                            {"name": "LANG", "value": "en_US.UTF-8"},
                            {"name": "LANGUAGE", "value": "en_US:en"},
                            {"name": "LC_ALL", "value": "en_US.UTF-8"},
                            {"name": "JAVA_VERSION", "value": "jdk-21.0.12+8"},
                            {"name": "PATH", "value": "/opt/jdk/bin:/usr/bin:/bin"},
                            {"name": "JAVA_HOME", "value": "/opt/jdk"}],
                    "resources": {"requests": {"cpu": "100m", "memory": "128Mi"},
                                  "limits": {"cpu": "1", "memory": "512Mi"}},
                    "securityContext": {"readOnlyRootFilesystem": True,
                                        "allowPrivilegeEscalation": False,
                                        "capabilities": {"drop": ["ALL"]}},
                    "volumeMounts": [{"name": "work", "mountPath": "/work"}],
                    "readinessProbe": {"httpGet": {"path": "/", "port": 8080},
                                       "periodSeconds": 1, "timeoutSeconds": 1,
                                       "failureThreshold": 180}}],
                "volumes": [{"name": "work", "emptyDir": {"sizeLimit": "64Mi"}}]}
        if variant == "brewlet":
            spec["runtimeClassName"] = "brewlet"
        return {"apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": name, "namespace": "phase10",
                             "labels": {"benchmark": ledger.state["run_id"]}, "annotations": annotations},
                "spec": spec}

    def delete(names):
        run(kubectl + ["delete", "pod", "-n", "phase10", *names,
                       "--ignore-not-found", "--wait=true", "--timeout=60s"])

    def sample(names):
        items = [json.loads(run(kubectl + ["get", "pod", name, "-n", "phase10", "-o", "json"]).stdout)
                 for name in names]
        ids = [p["status"]["containerStatuses"][0]["containerID"].split("://", 1)[1] for p in items]
        snapshot = json.loads(run(["docker", "exec", "-i", args.node, "python3", remote],
                                  data=json.dumps(ids)).stdout)
        for entry in snapshot["containers"]:
            if entry["cmdline"] != launch["command"]:
                raise RuntimeError("Actual JVM argv does not match the common launch contract: " + str(entry["cmdline"]))
            if entry["uid"] != ["65532"] * 4 or not entry["oci_readonly_root"]:
                raise RuntimeError("JVM UID or readonly root differs from contract")
            limits = entry["cgroup_values"]
            if limits["cpu.max"] != "100000 100000" or limits["memory.max"] != "536870912":
                raise RuntimeError("Actual cgroup limit mismatch: " + str(limits))
            identity = [entry["jdk_release_sha256"], entry.get("java_executable_sha256")]
            if not all(identity):
                raise RuntimeError("Actual JDK identity unavailable")
            if "verified_jdk_identity" not in result:
                result["verified_jdk_identity"] = identity
            if identity != result["verified_jdk_identity"]:
                raise RuntimeError("Actual JDK identity differs between trials")
            if "verified_runtime_environment" not in result:
                result["verified_runtime_environment"] = entry["runtime_environment"]
            if entry["runtime_environment"] != result["verified_runtime_environment"]:
                raise RuntimeError("Actual runtime environment differs between trials")
        return items, snapshot

    def execute(variant, index, size=1, warmup=False):
        names = [f"{variant}-{'warm' if warmup else 'trial'}-{index}-{i}" for i in range(size)]
        record = {"variant": variant, "pair": index, "size": size, "warmup": warmup,
                  "host_create_unix_ns": time.time_ns(), "status": "failed", "pods": []}
        start = time.monotonic_ns()
        try:
            for name in names:
                run(kubectl + ["create", "-f", "-"], data=json.dumps(pod(name, variant)))
            wait = run(kubectl + ["wait", "-n", "phase10", "--for=condition=Ready",
                       *["pod/" + name for name in names], "--timeout=180s"], check=False)
            record["host_ready_observed_unix_ns"] = time.time_ns()
            record["host_create_to_observed_ready_seconds"] = (time.monotonic_ns() - start) / 1e9
            if wait.returncode:
                raise RuntimeError(wait.stderr)
            for name in names:
                item = json.loads(run(kubectl + ["get", "pod", name, "-n", "phase10", "-o", "json"]).stdout)
                logs = run(kubectl + ["logs", name, "-n", "phase10"]).stdout
                (work / (name + ".log")).write_text(logs)
                ready = next(x["lastTransitionTime"] for x in item["status"]["conditions"]
                             if x["type"] == "Ready" and x["status"] == "True")
                started = item["status"]["containerStatuses"][0]["state"]["running"]["startedAt"]
                created = item["metadata"]["creationTimestamp"]
                timestamp = lambda t: datetime.datetime.fromisoformat(t.replace("Z", "+00:00")).timestamp()
                spring = re.search(r"Started PetClinicApplication in ([\d.]+) seconds \(process running for ([\d.]+)\)", logs)
                record["pods"].append({"name": name, "uid": item["metadata"]["uid"],
                    "created_at": created, "container_started_at": started, "ready_at": ready,
                    "creation_to_ready_seconds": timestamp(ready) - timestamp(created),
                    "container_start_to_ready_seconds": timestamp(ready) - timestamp(started),
                    "spring_startup_seconds": float(spring[1]) if spring else None,
                    "spring_process_seconds": float(spring[2]) if spring else None})
                for path in result["http_initialization"]:
                    run(node + ["curl", "--max-time", "5", "-fsS", "-o", "/dev/null",
                                f"http://{item['status']['podIP']}:8080{path}"])
            time.sleep(10)
            _, record["memory"] = sample(names)
            record["status"] = "ok"
        except Exception as exc:
            record["error"] = str(exc)
            for name in names:
                diagnostics = run(kubectl + ["describe", "pod", name, "-n", "phase10"], check=False)
                (work / (name + "-failure.log")).write_text(diagnostics.stdout + diagnostics.stderr)
                logs = run(kubectl + ["logs", name, "-n", "phase10"], check=False)
                (work / (name + ".log")).write_text(logs.stdout + logs.stderr)
            result["failures"].append({"variant": variant, "pair": index, "error": str(exc)})
        finally:
            try:
                delete(names)
            except Exception as exc:
                record["cleanup_error"] = str(exc)
                result["incomplete_trial"] = record
                save()
                raise
        print(json.dumps({"variant": variant, "pair": index, "size": size,
                          "warmup": warmup, "status": record["status"]}), flush=True)
        return record

    for index in range(2):
        for variant in ["conventional", "brewlet"]:
            result["trials"].append(execute(variant, index, warmup=True))
            save()
            if result["trials"][-1]["status"] != "ok":
                raise SystemExit("Warmup failed: retained diagnostics; fix setup before starting measurements")
    order = [["conventional", "brewlet"]] * (args.trials // 2) + [["brewlet", "conventional"]] * (args.trials - args.trials // 2)
    random.Random(args.seed).shuffle(order)
    result["paired_order"] = order
    save()
    for index, variants in enumerate(order):
        for variant in variants:
            result["trials"].append(execute(variant, index))
            save()
    for index in range(args.cohorts):
        for variant in order[index % len(order)]:
            result["cohorts"].append(execute(variant, 100 + index, size=4))
            save()
    result["control_plane_idle_detailed"] = collect_idle(work, args.node, kubectl, remote)
    result["managed_jdk_du"] = run(node + ["du", "-s", "-B1", "/opt/brewlet/jdks/temurin-21"]).stdout
    result["managed_jdk_java_home_du"] = run(node + ["du", "-s", "-B1", "/opt/brewlet/jdks/temurin-21/opt/java/openjdk"]).stdout
    save()


if __name__ == "__main__":
    main()
