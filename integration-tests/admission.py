#!/usr/bin/env python3
"""Real API-server admission checks. This does not establish node execution."""

import argparse
import copy
import json
from pathlib import Path
import subprocess
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kubeconfig", type=Path, required=True)
    parser.add_argument("--runtime-source", required=True)
    parser.add_argument("--app-image", required=True)
    parser.add_argument("--runtime-id", default="python-3.12")
    parser.add_argument("--profile-name", default="v2-zero-capacity")
    parser.add_argument("--pool-name", default="absent-test-pool")
    parser.add_argument("--existing-runtime-uid",
                        help="retry only against this exact already-created test inventory UID")
    parser.add_argument("--existing-profile-uid",
                        help="retry only against this exact already-created zero-capacity profile UID")
    args = parser.parse_args()
    base = ["kubectl", "--kubeconfig", str(args.kubeconfig.resolve())]

    def kubectl(arguments, document=None, success=True):
        result = subprocess.run(base + arguments, text=True, input=(
            json.dumps(document) if document is not None else None),
            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if success and result.returncode:
            raise AssertionError(f'kubectl failed: {result.stdout}\n{result.stderr}')
        return result

    runtime = {
        "apiVersion": "node.brewlet.sh/v1alpha1", "kind": "RuntimeImage",
        "metadata": {"name": args.runtime_id},
        "spec": {
            "source": {"image": args.runtime_source},
            "description": "Invocation-owned, pinned functional runtime fixture",
            "user": {"uid": 65532, "gid": 65532},
        },
    }
    profile = {
        "apiVersion": "node.brewlet.sh/v1alpha1", "kind": "NodeProfile",
        "metadata": {"name": args.profile_name},
        "spec": {
            "nodePool": {"key": "brewlet.test/pool", "names": [args.pool_name]},
            "runtimeImages": [{"id": args.runtime_id, "install": "OnDemand"}],
            "rollout": {"maxUnavailable": 1, "containerdRestart": "validated"},
        },
    }
    if args.existing_runtime_uid:
        existing = json.loads(kubectl(["get", "runtimeimage", args.runtime_id, "-o", "json"]).stdout)
        assert existing["metadata"]["uid"] == args.existing_runtime_uid
        assert existing["spec"]["source"] == runtime["spec"]["source"]
        assert existing["spec"]["user"] == runtime["spec"]["user"]
    else:
        kubectl(["create", "-f", "-"], runtime)
    deadline = time.monotonic() + 45
    while True:
        observed = json.loads(kubectl(["get", "runtimeimage", args.runtime_id, "-o", "json"]).stdout)
        if any(item["type"] == "Approved" and item["status"] == "True"
               for item in observed.get("status", {}).get("conditions", [])):
            break
        if time.monotonic() >= deadline:
            raise AssertionError(f"Runtime inventory never approved: {observed.get('status')}")
        time.sleep(0.25)
    if args.existing_profile_uid:
        existing = json.loads(kubectl(["get", "nodeprofile", args.profile_name, "-o", "json"]).stdout)
        assert existing["metadata"]["uid"] == args.existing_profile_uid
        assert existing["spec"]["nodePool"]["names"] == profile["spec"]["nodePool"]["names"]
        assert existing["spec"]["runtimeImages"] == profile["spec"]["runtimeImages"]
    else:
        kubectl(["create", "-f", "-"], profile)

    pod = {
        "apiVersion": "v1", "kind": "Pod",
        "metadata": {
            "name": "v2-admission-contract",
            "annotations": {"brewlet.sh/runtime-image": args.runtime_id,
                            "brewlet.sh/artifact-container": "untrusted",
                            "brewlet.sh/artifact-ref": "untrusted",
                            "brewlet.sh/artifact-digest": "sha256:untrusted"},
        },
        "spec": {
            "runtimeClassName": "brewlet",
            "securityContext": {"runAsUser": 2000},
            "nodeSelector": {"kubernetes.io/arch": "arm64"},
            "initContainers": [{"name": "init", "image": args.app_image}],
            "containers": [
                {"name": "app", "image": args.app_image,
                 "env": [{"name": "PATH", "value": "/app/not-runtime-path"}]},
                {"name": "sidecar", "image": args.app_image,
                 "securityContext": {"runAsUser": 3000, "runAsGroup": 4000}},
            ],
            "affinity": {"nodeAffinity": {
                "requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [
                    {"matchExpressions": [{"key": "kubernetes.io/arch", "operator": "In", "values": ["arm64"]}],
                     "matchFields": [{"key": "metadata.name", "operator": "In", "values": ["future-test-node"]}]},
                    {"matchExpressions": [{"key": "brewlet.test/placement", "operator": "Exists"}]},
                ]},
                "preferredDuringSchedulingIgnoredDuringExecution": [{
                    "weight": 20, "preference": {"matchExpressions": [
                        {"key": "brewlet.test/warm", "operator": "Exists"}
                    ]},
                }],
            }},
        },
    }
    admitted = kubectl(["create", "--dry-run=server", "-f", "-", "-o", "json"], pod)
    result = json.loads(admitted.stdout)
    assert result["metadata"]["annotations"]["brewlet.sh/artifact-container"] == "app"
    assert result["metadata"]["annotations"]["brewlet.sh/artifact-ref"] == args.app_image
    assert result["metadata"]["annotations"]["brewlet.sh/artifact-digest"] == args.app_image.split("@")[1]
    assert args.profile_name in admitted.stderr, "zero-capacity warning must name eligible profile"
    assert "PATH" in admitted.stderr, "explicit PATH must warn"
    for key, value in pod["spec"]["nodeSelector"].items():
        assert result["spec"]["nodeSelector"][key] == value
    assert result["spec"]["nodeSelector"]["brewlet.sh/runtime"] == "ready"
    affinity = result["spec"]["affinity"]["nodeAffinity"]
    terms = affinity["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
    assert len(terms) == 2
    assert terms[0]["matchFields"] == pod["spec"]["affinity"]["nodeAffinity"][
        "requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"][0]["matchFields"]
    for term in terms:
        assert any(expression["key"] == f"brewlet.sh/runtime.{args.runtime_id}"
                   and expression["operator"] == "In"
                   and expression["values"] == ["ready", "available"]
                   for expression in term["matchExpressions"])
    preferred = affinity["preferredDuringSchedulingIgnoredDuringExecution"]
    assert preferred[0] == pod["spec"]["affinity"]["nodeAffinity"][
        "preferredDuringSchedulingIgnoredDuringExecution"][0]
    assert preferred[-1]["weight"] == 100
    for container in result["spec"]["initContainers"] + result["spec"]["containers"]:
        context = container["securityContext"]
        assert context["runAsNonRoot"] is True
        assert context["allowPrivilegeEscalation"] is False
        assert context["capabilities"]["drop"] == ["ALL"]
        if container["name"] == "sidecar":
            assert context["runAsUser"] == 3000 and context["runAsGroup"] == 4000
        else:
            assert context.get("runAsUser", 2000) == 2000 and context["runAsGroup"] == 65532
    group_only = copy.deepcopy(pod)
    group_only["spec"]["securityContext"].pop("runAsUser")
    group_only["spec"]["containers"][0]["securityContext"] = {"runAsGroup": 5000}
    response = kubectl(["create", "--dry-run=server", "-f", "-", "-o", "json"], group_only)
    context = json.loads(response.stdout)["spec"]["containers"][0]["securityContext"]
    assert context["runAsUser"] == 65532 and context["runAsGroup"] == 5000

    def denied(name, reason, mutate):
        invalid = copy.deepcopy(pod)
        mutate(invalid)
        response = kubectl(["create", "--dry-run=server", "-f", "-", "-o", "json"],
                           invalid, success=False)
        assert response.returncode != 0, f"{name}: invalid Pod admitted"
        assert reason in response.stderr, f"{name}: expected {reason}: {response.stderr}"

    denied("missing selection", "RuntimeImageRequired",
           lambda item: item["metadata"]["annotations"].clear())
    denied("invalid ID", "InvalidRuntimeImageID",
           lambda item: item["metadata"]["annotations"].update({"brewlet.sh/runtime-image": "python:3.12"}))
    denied("unknown ID", "UnknownRuntimeImage",
           lambda item: item["metadata"]["annotations"].update({"brewlet.sh/runtime-image": "unknown-runtime"}))
    for annotation in ("brewlet.sh/jdk", "brewlet.sh/jvm-args", "brewlet.sh/arch"):
        denied("old annotation", "UnsupportedAnnotation",
               lambda item, annotation=annotation: item["metadata"]["annotations"].update({annotation: "forbidden"}))
    for field, value in (("runAsUser", 0), ("runAsGroup", 0), ("runAsNonRoot", False)):
        denied(field, "RootNotSupported",
               lambda item, field=field, value=value: item["spec"]["containers"][0].update(
                   {"securityContext": {field: value}}))
    denied("supplementary root", "RootNotSupported",
           lambda item: item["spec"]["securityContext"].update({"supplementalGroups": [0]}))
    for context in ({"privileged": True}, {"allowPrivilegeEscalation": True},
                    {"capabilities": {"add": ["NET_ADMIN"]}}):
        denied("privilege escalation", "PrivilegeEscalationNotSupported",
               lambda item, context=context: item["spec"]["containers"][0].update({"securityContext": context}))
    denied("startup bypass", "StartupTaintTolerationNotSupported",
           lambda item: item["spec"].update({"tolerations": [{"operator": "Exists", "effect": "NoSchedule"}]}))

    empty = copy.deepcopy(pod)
    empty["spec"]["affinity"]["nodeAffinity"][
        "requiredDuringSchedulingIgnoredDuringExecution"] = {"nodeSelectorTerms": []}
    response = kubectl(["create", "--dry-run=server", "-f", "-", "-o", "json"], empty, success=False)
    if response.returncode:
        assert "nodeSelectorTerms" in response.stderr
        assert "must have at least one node selector term" in response.stderr
        empty_outcome = "preserved-and-rejected-by-native-api-validation"
    else:
        preserved = json.loads(response.stdout)["spec"]["affinity"]["nodeAffinity"][
            "requiredDuringSchedulingIgnoredDuringExecution"].get("nodeSelectorTerms", [])
        assert preserved == [], "empty selector must not become a satisfiable alternative"
        empty_outcome = "preserved-unsatisfiable"
    print(json.dumps({
        "scope": "real-api-admission", "outcome": "passed",
        "zeroNodeProfile": args.profile_name,
        "emptySelector": empty_outcome,
        "realNodeExecution": False, "providerAutoscaling": False,
    }))


if __name__ == "__main__":
    main()
