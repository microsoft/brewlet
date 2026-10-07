#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Destroy an owned kind worker and recover its profile through retained evidence.

This is provider-neutral runtime E2E, not an AKS/VMSS API verification test.
The replacement is a fresh, unprovisioned standby worker in the private cluster.
No existing kube context, host, NodeProfile or Docker container is accepted.
"""

from datetime import datetime, timezone
import json
import time

from checkout import CheckoutFixture
from common import sha256, wait


RESOURCE = "noderetirementevidence.node.brewlet.sh"
EVIDENCE = "retired-worker"
PROFILE = "live"
POOL = "brewlet.sh/e2e-pool=live"


def claimed_target(profile, node):
    targets = [t for t in profile.get("status", {}).get("targets", [])
               if t["name"] == node and t.get("claimed")]
    return targets[0] if len(targets) == 1 else None


def runtime_labels(node):
    return {k: v for k, v in node["metadata"]["labels"].items() if k.startswith("brewlet.sh/")}


def require_blocked(profile, original, replacement):
    status = profile.get("status", {})
    if not any(c.get("reason") == "CleanupBlocked" for c in status.get("conditions", [])):
        raise AssertionError("Missing evidence did not keep the profile CleanupBlocked")
    if original not in status.get("retirement", {}).get("targets", []):
        raise AssertionError("Blocked recovery lost the frozen original cleanup obligation")
    if claimed_target(profile, replacement):
        raise AssertionError("Replacement was claimed before resolving original retirement")


def retire_worker(f, name, target, profile):
    if name not in f.worker_ids or name != f.worker_names[0]:
        raise RuntimeError("Refusing to retire anything except the invocation's original worker")
    if f.get("namespace", "kube-system")["metadata"]["uid"] != f.cluster_uid:
        raise RuntimeError("Refusing to retire a worker after cluster identity changed")
    node = f.get("node", name)
    if node["metadata"]["uid"] != target["uid"] or target["name"] != name or not target.get("claimed"):
        raise RuntimeError("Original worker no longer matches the claimed Node identity")
    if not node["spec"].get("providerID") or node["spec"]["providerID"] != target.get("providerID"):
        raise RuntimeError("Original worker is missing its recorded provider identity")
    info = f.own_container(name)
    identifier = f.worker_ids[name]
    receipt = {"profileUID": profile["metadata"]["uid"], "target": target,
               "node": node, "container": info}
    f.save("retirement-host-before.json", receipt)
    f.run(["docker", "rm", "-f", "--volumes", identifier])
    del f.worker_ids[name]
    containers = f.run(["docker", "container", "ls", "-a", "--no-trunc",
                        "--format", "{{.ID}}"]).stdout.split()
    volumes = f.run(["docker", "volume", "ls", "--format", "{{.Name}}"]).stdout.split()
    if identifier in containers or any(m["Name"] in volumes for m in info["Mounts"] if m["Type"] == "volume"):
        raise AssertionError("Original container or its host volume survived retirement")
    receipt["retiredAt"] = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    receipt["containerAbsent"] = receipt["volumesAbsent"] = True
    f.save("retirement-host.json", receipt)
    # Node absence alone is never the proof: remove registration only after the
    # Docker host and its volumes have been irreversibly removed and recorded.
    f.kube("delete", "node", name, "--wait=true", "--timeout=60s")
    return receipt


def evidence_document(f, receipt, profile):
    target = receipt["target"]
    return {
        "apiVersion": "node.brewlet.sh/v1alpha1", "kind": "NodeRetirementEvidence",
        "metadata": {"name": EVIDENCE},
        "spec": {
            "profileName": PROFILE, "profileUID": profile["metadata"]["uid"],
            "nodeName": target["name"], "nodeUID": target["uid"],
            "providerID": target["providerID"], "instanceID": receipt["container"]["Id"],
            **({"systemUUID": target["systemUUID"]} if target.get("systemUUID") else {}),
            "evidenceRef": "urn:sha256:" + sha256(f.work / "retirement-host.json"),
            "identityBinding": "retirement-host.json records the private kind cluster's "
                               "Node UID, providerID, Docker container ID, ownership labels "
                               "and verified container/volume destruction.",
            "retiredAt": receipt["retiredAt"], "permanentlyDecommissioned": True,
        },
    }


def serve(f, name, node, image):
    f.apply({
        "apiVersion": "v1", "kind": "Pod",
        "metadata": {"name": name, "namespace": f.namespace},
        "spec": {
            "runtimeClassName": "brewlet", "restartPolicy": "Never",
            "nodeSelector": {"kubernetes.io/hostname": node},
            "securityContext": {"runAsNonRoot": True, "runAsUser": 10001,
                                "seccompProfile": {"type": "RuntimeDefault"}},
            "containers": [{
                "name": "application", "image": image,
                "securityContext": {"allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}},
                "ports": [{"containerPort": 8080}],
                "resources": {"requests": {"cpu": "100m", "memory": "128Mi"},
                              "limits": {"cpu": "500m", "memory": "256Mi"}},
                "readinessProbe": {"httpGet": {"path": "/healthz", "port": 8080}, "periodSeconds": 2},
            }],
        },
    })
    f.kube("wait", "--for=condition=Ready", "pod", name, "-n", f.namespace, "--timeout=240s")
    pod = f.get("pod", name, "-n", f.namespace)
    if pod["spec"]["nodeName"] != node or pod["spec"].get("runtimeClassName") != "brewlet":
        raise AssertionError("Workload did not use Brewlet on the intended node")
    require_response(f, name)
    f.record(f"{name}-serves-java", {"node": node, "podUID": pod["metadata"]["uid"], "image": image})
    return pod


def require_response(f, name):
    response = f.kube("get", "--raw",
                      f"/api/v1/namespaces/{f.namespace}/pods/{name}:8080/proxy/hello").stdout
    if "Hello from a JAR" not in response:
        raise AssertionError(f"{name} did not serve the Java application's response")


def exercise(f):
    original, replacement = f.worker_names
    f.kube("label", "node", original, POOL)
    wait("original worker really provisions", lambda: f.get("node", original)["metadata"]["labels"].get(
        "brewlet.sh/jdk.temurin-21") == "true", timeout=480)
    profile = wait("original target is claimed", lambda: (
        p if claimed_target(p := f.get("nodeprofile", PROFILE), original) else None))
    target = claimed_target(profile, original)
    image = f.publish_demo()
    serve(f, "original", original, image)
    survivor = serve(f, "survivor", f.node, image)
    survivor_labels = runtime_labels(f.get("node", f.node))
    fresh = f.get("node", replacement)
    fresh_labels = runtime_labels(fresh)
    if any(k in fresh_labels for k in ("brewlet.sh/owner-uid", "brewlet.sh/jdk.temurin-21")):
        raise AssertionError("Replacement was not an unprovisioned standby")
    receipt = retire_worker(f, original, target, profile)
    f.record("original-host-permanently-destroyed", {
        "nodeUID": target["uid"], "containerID": receipt["container"]["Id"],
        "proofSHA256": sha256(f.work / "retirement-host.json")})
    f.kube("label", "node", replacement, POOL)
    wait("missing evidence blocks retirement", lambda: any(
        c.get("reason") == "CleanupBlocked"
        for c in f.get("nodeprofile", PROFILE).get("status", {}).get("conditions", [])))
    for delay in (0, 15):
        time.sleep(delay)
        require_blocked(f.get("nodeprofile", PROFILE), target, replacement)
        labels = runtime_labels(f.get("node", replacement))
        if labels != dict(fresh_labels, **{"brewlet.sh/e2e-pool": "live"}):
            raise AssertionError("Blocked recovery touched the replacement's runtime/ownership")
    f.record("no-evidence-fails-closed", {"target": target, "replacementUID": fresh["metadata"]["uid"]})
    document = evidence_document(f, receipt, profile)
    f.apply({"apiVersion": "v1", "kind": "ServiceAccount",
             "metadata": {"name": "recovery", "namespace": f.namespace}})
    identity = f"system:serviceaccount:{f.namespace}:recovery"
    denied = f.kube("--as=" + identity, "create", "-f", "-", input=json.dumps(document), check=False)
    if denied.returncode == 0 or "Forbidden" not in denied.stderr:
        raise AssertionError("Unbound recovery identity did not receive an explicit Forbidden denial")
    f.record("unbound-submitter-denied", {"identity": identity})
    f.apply({
        "apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
        "metadata": {"name": "live-retirement-recovery"},
        "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole",
                    "name": "brewlet-retirement-recovery"},
        "subjects": [{"kind": "ServiceAccount", "name": "recovery", "namespace": f.namespace}],
    })
    wait("recovery grant is effective", lambda: f.kube(
        "--as=" + identity, "auth", "can-i", "create", RESOURCE, check=False).stdout.strip() == "yes",
        timeout=30, interval=1)
    f.kube("--as=" + identity, "create", "-f", "-", input=json.dumps(document))
    resolved = wait("evidence is durably resolved", lambda: (
        e if (e := f.get(RESOURCE, EVIDENCE)).get("status", {}).get("phase") == "Resolved" else None))
    obligation = resolved["status"]["obligation"]
    if (obligation["targets"] != [target] or obligation["phase"] != "ExternallyRetired"
            or resolved["spec"] != document["spec"] or resolved["metadata"].get("ownerReferences")):
        raise AssertionError("Resolved evidence did not retain the exact independent original obligation")
    f.save("retirement-evidence-resolved.json", resolved)
    wait("replacement runtime is actually ready", lambda: f.get("node", replacement)["metadata"]["labels"].get(
        "brewlet.sh/jdk.temurin-21") == "true", timeout=480)
    current = f.get("nodeprofile", PROFILE)
    replacement_target = claimed_target(current, replacement)
    if (current["metadata"]["uid"] != profile["metadata"]["uid"]
            or current["status"].get("retirement")
            or claimed_target(current, original)
            or not replacement_target or replacement_target["uid"] != fresh["metadata"]["uid"]
            or len(current["status"]["targets"]) != 2):
        raise AssertionError("Recovery did not resume with exactly the survivor and fresh replacement")
    f.record("replacement-claimed-under-new-identity", {"target": replacement_target})
    serve(f, "replacement", replacement, image)
    after = f.get("pod", "survivor", "-n", f.namespace)
    if (runtime_labels(f.get("node", f.node)) != survivor_labels
            or after["metadata"]["uid"] != survivor["metadata"]["uid"]
            or after["status"]["containerStatuses"] != survivor["status"]["containerStatuses"]):
        raise AssertionError("Recovery disturbed the surviving host or restarted its workload")
    require_response(f, "survivor")
    f.record("surviving-host-and-java-process-preserved", {"node": f.node})
    f.kube("delete", "pods", "survivor", "replacement", "-n", f.namespace, "--timeout=120s")
    f.kube("delete", "nodeprofile", PROFILE, "--wait=true", "--timeout=360s", timeout=390)
    retained = f.get(RESOURCE, EVIDENCE)
    if (retained["metadata"]["uid"] != resolved["metadata"]["uid"]
            or retained["spec"] != resolved["spec"] or retained["status"] != resolved["status"]
            or retained["metadata"].get("ownerReferences")):
        raise AssertionError("Historical evidence changed or disappeared after profile deletion")
    f.save("retirement-evidence-retained.json", retained)
    f.record("resolved-evidence-survives-profile-deletion", {"evidenceUID": retained["metadata"]["uid"]})


def main():
    with CheckoutFixture("retirement", workers=2) as fixture:
        exercise(fixture)


if __name__ == "__main__":
    main()
