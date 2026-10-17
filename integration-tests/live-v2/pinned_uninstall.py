#!/usr/bin/env python3
"""Actual Helm uninstall retains authority and startup fencing behind stale provider pins."""

import argparse
import json
from pathlib import Path
import signal

from control_edges import ControlEdges, ProviderPins
import delivery
from harness import FENCE, IDS, OWNER, ROOT, STORE, Failure, Harness, require, require_owned


def helm_profile_identity(obj, release, namespace):
    metadata = obj["metadata"]
    require(metadata.get("uid") and not metadata.get("deletionTimestamp")
            and metadata.get("labels", {}).get("app.kubernetes.io/managed-by") == "Helm"
            and metadata.get("annotations", {}).get("meta.helm.sh/release-name") == release
            and metadata["annotations"].get("meta.helm.sh/release-namespace") == namespace,
            "profile was not actually created by the exact Helm release")
    return metadata["uid"]


def uninstall(h):
    return h.run(["helm", "--kubeconfig", h.kubeconfig, "--kube-context", h.context,
                  "uninstall", h.cluster, "--namespace", h.control_namespace,
                  "--wait", "--timeout", "60s"], check=False, timeout=90)


def blocked_cleanup(h, uid, stale):
    profile = h.get("nodeprofile", h.profile)
    require_owned(profile, h.run_id, uid)
    require(profile["metadata"].get("deletionTimestamp") and profile["metadata"].get("finalizers"),
            "blocked uninstall discarded the profile or finalizer")
    node = h.node_snapshot()
    require(node["metadata"]["labels"].get(stale) == "ready"
            and node["metadata"]["labels"].get("brewlet.sh/owner-uid") == uid
            and any(t["key"] == FENCE and t.get("effect") == "NoSchedule"
                    for t in node["spec"].get("taints", [])),
            "blocked uninstall lost the pinned label, claim or real startup fence")
    require(h.get("deployment", "brewlet-operator", h.control_namespace)["status"].get(
        "availableReplicas", 0) > 0, "failed uninstall removed the active cleanup operator")
    permission = h.kubectl("auth", "can-i", "update", "nodes",
                           "--as=system:serviceaccount:" + h.control_namespace + ":brewlet-operator",
                           check=False)
    require(permission.returncode == 0 and permission.stdout.strip() == "yes",
            "failed uninstall removed the operator's cleanup RBAC")
    logs = h.kubectl("-n", h.control_namespace, "logs", "job/brewlet-uninstall",
                      "-c", "coordinator").stdout
    require("uninstall cleanup incomplete" in logs,
            "uninstall failed for an unrelated reason: " + logs[-2000:])
    return profile


def exercise(h):
    h.kubectl("label", "node", h.node, "brewlet.test/pool=" + h.run_id)
    h.kubectl("taint", "node", h.node, FENCE + "=provisioning:NoSchedule")
    spec = h.profile_object()["spec"]
    spec["runtimeImages"] = [{"id": IDS["python"], "install": "Eager"}]
    values = h.work / "helm-owned-profile.json"
    values.write_text(json.dumps({"provisioner": {"profiles": [{"name": h.profile, "spec": spec}]},
                                 "uninstall": {"timeoutSeconds": 15}}))
    h.run(["helm", "--kubeconfig", h.kubeconfig, "--kube-context", h.context,
           "upgrade", h.cluster, ROOT / "kubernetes/charts/brewlet", "-n", h.control_namespace,
           "--reuse-values", "-f", values, "--wait", "--timeout", "180s"], timeout=240)
    observed = h.get("nodeprofile", h.profile)
    uid = helm_profile_identity(observed, h.cluster, h.control_namespace)
    # Add only the invocation's discovery label, never Helm ownership or authority.
    h.kubectl("patch", "nodeprofile", h.profile, "--type=json", "-p", json.dumps([
        {"op": "test", "path": "/metadata/uid", "value": uid},
        {"op": "test", "path": "/metadata/annotations/meta.helm.sh~1release-name", "value": h.cluster},
        {"op": "test", "path": "/metadata/annotations/meta.helm.sh~1release-namespace",
         "value": h.control_namespace},
        {"op": "test", "path": "/metadata/labels/app.kubernetes.io~1managed-by", "value": "Helm"},
        {"op": "add", "path": "/metadata/labels/" + OWNER.replace("/", "~1"), "value": h.run_id}]))
    h.owned[("nodeprofile", "", h.profile)] = uid
    h.wait("actual chart-owned Eager root ready", lambda: h.advertised(IDS["python"], "ready"))
    h.apply(h.pod("uninstall-active", "python"))
    pod = h.wait("real task holds root during uninstall", lambda: h.ready("uninstall-active"))
    lease = h.lease(pod)
    control = ControlEdges(h)
    h.apply({"apiVersion": "v1", "kind": "Namespace",
             "metadata": {"name": control.namespace}})
    pins = ProviderPins(control)
    pins.install()
    stale = "brewlet.sh/runtime.stale-" + h.run_id
    control.conditional_node_update({"metadata": {"labels": {stale: "ready"}}})
    pins.configure([stale])
    refusal = h.kubectl("patch", "node", h.node, "--type=merge", "-p",
                        json.dumps({"metadata": {"labels": {stale: None}}}), check=False)
    require(refusal.returncode != 0 and "provider-pinned label refusal: " + stale in refusal.stderr,
            "stale pin did not cause a real admission refusal")
    h.wait("actual provider refusal audited", lambda: pins.refused(stale), timeout=60)
    h.wait("stale provider pin retains node-wide startup fence", pins.fenced)
    first = uninstall(h)
    require(first.returncode != 0, "uninstall bypassed an actual active task lease")
    blocked_cleanup(h, uid, stale)
    runtime = lease["runtime"]
    root = f"{STORE}/{runtime['id']}/{runtime['inventoryUID']}/{runtime['generation']}/rootfs"
    h.node_exec("test", "-d", root)
    h.assert_report(h.http("uninstall-active"), "python", arguments=[], cwd="/app")
    h.record("actual Helm uninstall retains operator, finalizer, claim and active leased root",
             {"profileUID": uid, "taskID": lease["taskID"], "pinRefusal": refusal.stderr},
             ["OWN-04", "LEASE-03", "FENCE-04"])
    h.delete("pod", "uninstall-active", h.namespace)
    h.wait("active uninstall task really drained", lambda:
           h.get("pod", "uninstall-active", h.namespace, absent=True) is None)
    second = uninstall(h)
    require(second.returncode != 0, "uninstall discarded a stale provider capability")
    blocked_cleanup(h, uid, stale)
    h.record("stale provider capability retains startup fence through drained production uninstall",
             {"profileUID": uid, "staleCapability": stale, "forcedFinalizerRemoval": False},
             ["E2E-06", "FENCE-04", "OWN-04"])
    pins.configure([])
    control.conditional_node_update({"metadata": {"labels": {stale: None}}})
    h.wait("pool correction permits genuine profile cleanup/finalization", lambda:
           h.get("nodeprofile", h.profile, absent=True) is None)
    final = uninstall(h)
    require(final.returncode == 0, "corrected production uninstall did not complete: " +
            (final.stdout + final.stderr)[-2000:])
    node = h.node_snapshot()
    require(not node["metadata"].get("labels", {}).get("brewlet.sh/owner-uid"),
            "successful uninstall retained the profile claim")
    h.node_exec("test", "!", "-e", STORE + "/" + IDS["python"] + "/current")
    h.record("pool correction completes real Helm uninstall without force or forged cleanup",
             {"profileUID": uid, "nodeUID": h.node_uid, "releaseRemoved": True},
             ["E2E-06", "FENCE-04", "OWN-04"])


def run(work):
    h = Harness(work)
    status = 1
    h.result["gaps"].append("Focused stale-pin uninstall case; other acceptance suites are separate")
    try:
        for phase, action in (("prerequisites", h.preflight), ("cluster", h.provision_cluster),
                              ("build", h.prepare_images),
                              ("delivery-bootstrap", lambda: delivery.bootstrap(h)),
                              ("install", h.install), ("pinned-uninstall", lambda: exercise(h))):
            h.phase = phase
            action()
        h.result["outcome"] = "passed"
        status = 0
    except Exception as error:
        h.result.update(outcome="failed", failurePhase=h.phase, failure=str(error))
        print("FAIL:", error, flush=True)
    finally:
        errors = []
        try:
            h.stop_helpers()
            h.diagnostics()
        except Exception as error:
            errors.append("diagnostics: " + str(error))
        try:
            delivery.cleanup(h)
        except Exception as error:
            errors.append("delivery cleanup: " + str(error))
        errors.extend(h.destroy())
        if errors:
            h.result.update(outcome="failed", cleanupErrors=errors)
            status = 1
        h.save()
        print("Private uninstall evidence:", h.work, flush=True)
    return status


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", required=True, type=Path)
    def interrupted(signum, frame):
        raise Failure("pinned uninstall acceptance interrupted by signal " + str(signum))
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    raise SystemExit(run(parser.parse_args().work))
