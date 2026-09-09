#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Post-benchmark real JDK build change, with the same Brewlet application digest."""
import argparse
import json
from pathlib import Path
import subprocess
import time
import contextlib

from setup import JDK
from resources import Ledger

OLD_JDK = "docker.io/library/eclipse-temurin@sha256:8f4860b60f1b5c5b63a1279e189342675a5904e41bbabc447820abaf65bd66c9"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", type=Path, required=True)
    parser.add_argument("--kubeconfig", required=True)
    parser.add_argument("--node", required=True)
    args = parser.parse_args()
    w = args.work
    ledger = Ledger(w)
    ledger.verify_node(args.node, args.kubeconfig)
    prefix = ledger.state["names"]["prefix"]
    kube = ["kubectl", "--kubeconfig", args.kubeconfig]
    node = ["docker", "exec", args.node]

    def run(argv, data=None):
        result = subprocess.run(argv, input=data, text=True, capture_output=True)
        if result.returncode:
            raise RuntimeError(result.stderr[-3000:])
        return result.stdout

    pods = json.loads(run(kube + ["get", "pods", "-n", "phase10", "-o", "json"]))
    if pods["items"]:
        raise SystemExit("Primary benchmark must finish and drain before runtime changes")
    setup = json.loads((w / "setup.json").read_text())
    launch = json.loads((w / "matched/launch.json").read_text())
    result = {"schema_version": 1, "scope": "post-primary experiment; not included in startup/memory trials",
              "application_image": setup["images"]["brewlet"], "transitions": [],
              "cache_note": "The alternate source was not explicitly prefetched on the node; the original source was already provisioned/cached. Policy readiness times are not a controlled comparison.",
              "old_jdk_source": OLD_JDK, "current_jdk_source": JDK}
    run(["docker", "cp", str(Path(__file__).with_name("collect-node.py")),
         args.node + ":/bench-collect.py"])
    profile = json.loads((w / "profile.json").read_text())

    def save():
        (w / "policy-update-raw.json").write_text(json.dumps(result, indent=2) + "\n")

    def change(source):
        profile["spec"]["jdks"][0]["source"]["image"] = source
        run(kube + ["apply", "-f", "-"], json.dumps(profile))
        start = time.monotonic()
        while time.monotonic() - start < 300:
            state = json.loads(run(kube + ["get", "nodeprofile", "phase10", "-o", "json"]))
            conditions = state.get("status", {}).get("conditions", [])
            if state.get("status", {}).get("observedGeneration") == state["metadata"]["generation"] \
                    and any(c["type"] == "Ready" and c["status"] == "True"
                            and c.get("observedGeneration") == state["metadata"]["generation"]
                            for c in conditions):
                return {"generation": state["metadata"]["generation"],
                        "observed_ready_seconds": time.monotonic() - start,
                        "source": source}
            time.sleep(1)
        raise RuntimeError("Runtime policy update did not become Ready")

    def smoke(index):
        name = "policy-proof-" + str(index)
        body = {"apiVersion": "v1", "kind": "Pod", "metadata": {
                    "name": name, "namespace": "phase10", "labels": {"benchmark": ledger.state["run_id"]},
                    "annotations": {"brewlet.sh/jdk": "temurin-21",
                                    "brewlet.sh/jvm-args": json.dumps(launch["flags"])}},
                "spec": {"runtimeClassName": "brewlet", "nodeName": args.node, "restartPolicy": "Never",
                         "terminationGracePeriodSeconds": 5, "automountServiceAccountToken": False,
                         "securityContext": {"runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532},
                         "containers": [{"name": "app", "image": setup["images"]["brewlet"],
                             "imagePullPolicy": "Never",
                             "resources": {"requests": {"cpu": "100m", "memory": "128Mi"},
                                           "limits": {"cpu": "1", "memory": "512Mi"}},
                             "securityContext": {"readOnlyRootFilesystem": True, "allowPrivilegeEscalation": False,
                                                 "capabilities": {"drop": ["ALL"]}},
                             "volumeMounts": [{"name": "work", "mountPath": "/work"}],
                             "readinessProbe": {"httpGet": {"path": "/", "port": 8080},
                                                "periodSeconds": 1, "timeoutSeconds": 1, "failureThreshold": 180}}],
                         "volumes": [{"name": "work", "emptyDir": {"sizeLimit": "64Mi"}}]}}
        try:
            run(kube + ["create", "-f", "-"], json.dumps(body))
            run(kube + ["wait", "-n", "phase10", "pod/" + name, "--for=condition=Ready", "--timeout=180s"])
            pod = json.loads(run(kube + ["get", "pod", name, "-n", "phase10", "-o", "json"]))
            (w / (name + ".log")).write_text(run(kube + ["logs", name, "-n", "phase10"]))
            cid = pod["status"]["containerStatuses"][0]["containerID"].split("://", 1)[1]
            snapshot = json.loads(run(["docker", "exec", "-i", args.node, "python3", "/bench-collect.py"],
                                      json.dumps([cid])))
            return {"pod_uid": pod["metadata"]["uid"], "pod_image": pod["spec"]["containers"][0]["image"],
                    "actual_jdk_release": snapshot["containers"][0]["jdk_release"],
                    "actual_java_executable_sha256": snapshot["containers"][0]["java_executable_sha256"]}
        finally:
            run(kube + ["delete", "pod", name, "-n", "phase10", "--ignore-not-found", "--wait=true", "--timeout=60s"])

    try:
        for index, source in enumerate([OLD_JDK, JDK]):
            transition = change(source)
            result["transitions"].append(transition)
            save()
            transition["smoke"] = smoke(index)
            save()
        result["same_app_digest_verified"] = all(t["smoke"]["pod_image"] == result["application_image"]
                                                for t in result["transitions"])
        result["distinct_jdk_builds_verified"] = (result["transitions"][0]["smoke"]["actual_jdk_release"] !=
                                                 result["transitions"][1]["smoke"]["actual_jdk_release"])
        if not result["same_app_digest_verified"] or not result["distinct_jdk_builds_verified"]:
            raise RuntimeError("Policy-change proof failed its identity checks")
    except Exception as exc:
        result["error"] = str(exc)
        save()
        raise
    finally:
        profile["spec"]["jdks"][0]["source"]["image"] = JDK
        run(kube + ["apply", "-f", "-"], json.dumps(profile))
        save()
    # Pull only after primary measurements. Rebase the exact matched payload
    # onto the real older source image; do not run it in the primary comparison.
    oldfile = w / "old-jdk.Dockerfile"
    oldfile.write_text((w / "matched/Dockerfile").read_text().replace(JDK, OLD_JDK))
    with (w / "old-jdk-build.log").open("w") as log:
        subprocess.run(["docker", "pull", "--platform", "linux/arm64", OLD_JDK],
                       check=True, stdout=log, stderr=subprocess.STDOUT)
        with contextlib.redirect_stdout(log):
            ledger.build(prefix + "/conventional:old-jdk", ["-f", str(oldfile), str(w / "matched")])
    # A named task-owned alias makes the old runtime available to storage.py.
    ledger.tag(OLD_JDK, prefix + "/jdk:old")
    # COPY --link explicitly decouples unchanged application layers from base
    # replacement. Include this optimized storage-only control rather than
    # treating a normal COPY cache miss as an inherent conventional-image cost.
    linked = (w / "matched/Dockerfile").read_text().replace("COPY ", "COPY --link ")
    for version, source in [("baseline", JDK), ("old-jdk", OLD_JDK)]:
        dockerfile = w / ("linked-" + version + ".Dockerfile")
        dockerfile.write_text(linked.replace(JDK, source))
        with (w / ("linked-" + version + "-build.log")).open("w") as log:
            with contextlib.redirect_stdout(log):
                ledger.build(prefix + "/conventional-linked:" + version,
                             ["-f", str(dockerfile), str(w / "matched")])
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
