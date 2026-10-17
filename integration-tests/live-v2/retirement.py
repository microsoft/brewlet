#!/usr/bin/env python3
"""Real worker loss, independent replacement serving and authorized retained retirement evidence."""

import argparse
import json
from pathlib import Path
import signal

import delivery
from harness import API, FENCE, IDS, OWNER, Failure, Harness, require, require_owned
from pinned_uninstall import install_chart_profile, uninstall


def missing_obligation(profile, node_name, node_uid):
    matches = [d for d in profile.get("status", {}).get("detachedRetirements", [])
               if d.get("phase") == "Missing" and len(d.get("targets", [])) == 1
               and d["targets"][0]["name"] == node_name and d["targets"][0]["uid"] == node_uid]
    require(len(matches) <= 1, "duplicate detached retirement obligation")
    return matches[0] if matches else False


def exercise(h):
    old = h.cluster + "-worker"
    node = h.get("node", old)
    h.kubectl("patch", "node", old, "--type=merge", "-p", json.dumps({
        "metadata": {"uid": h.node_uids[old], "resourceVersion": node["metadata"]["resourceVersion"],
                     "labels": {"brewlet.test/pool": h.run_id}}}))
    h.delivery.configure_node(old)
    spec = h.profile_object()["spec"]
    spec["runtimeImages"] = [{"id": IDS["python"], "install": "Eager"}]
    uid = install_chart_profile(h, spec)
    h.wait("original real worker validates Eager runtime", lambda:
           h.get("node", old)["metadata"].get("labels", {}).get(
               "brewlet.sh/runtime." + IDS["python"]) == "ready")
    pod = h.pod("retirement-original", "python")
    pod["spec"]["nodeSelector"]["kubernetes.io/hostname"] = old
    h.apply(pod)
    original = h.wait("actual original worker task", lambda: h.ready("retirement-original"))
    require(original["spec"]["nodeName"] == old, "original task was not on lost worker")
    old_lease = h.lease(original)
    frozen, container = h.decommission_worker(old)
    h.kubectl("delete", "--raw", "/api/v1/nodes/" + old, "-f", "-", input=json.dumps({
        "apiVersion": "v1", "kind": "DeleteOptions",
        "preconditions": {"uid": h.node_uids[old]}}))
    h.wait("original Node resource really gone", lambda: h.get("node", old, absent=True) is None)
    h.wait("real missing original target is retained independently", lambda:
           missing_obligation(h.get("nodeprofile", h.profile), old, frozen["metadata"]["uid"]))
    h.kubectl("taint", "node", h.node, FENCE + "=provisioning:NoSchedule")
    h.kubectl("label", "node", h.node, "brewlet.test/pool=" + h.run_id)
    h.wait("fresh standby replacement really provisions", lambda: h.advertised(IDS["python"], "ready"))
    replacement = h.pod("retirement-replacement", "python")
    h.apply(replacement)
    serving = h.wait("replacement serves before any external evidence", lambda:
                     h.ready("retirement-replacement"))
    require(serving["spec"]["nodeName"] == h.node
            and h.node_uid != frozen["metadata"]["uid"], "replacement was not a different real Node")
    h.assert_report(h.http("retirement-replacement"), "python", arguments=[], cwd="/app")
    profile = h.get("nodeprofile", h.profile)
    require_owned(profile, h.run_id, uid)
    obligation = missing_obligation(profile, old, frozen["metadata"]["uid"])
    require(obligation and not obligation["targets"][0].get("retirementEvidenceUID"),
            "missing-host history disappeared or was implicitly accepted as cleanup")
    h.record("physical worker destruction preserves missing obligation while fresh replacement serves",
             {"originalNodeUID": frozen["metadata"]["uid"], "replacementNodeUID": h.node_uid,
              "originalTaskID": old_lease["taskID"], "externalContainerID": container,
              "notHostCleanupEvidence": True}, ["OWN-01", "OWN-03", "OWN-02"])

    h.delete("pod", "retirement-replacement", h.namespace)
    h.wait("replacement application really drained", lambda:
           h.get("pod", "retirement-replacement", h.namespace, absent=True) is None)
    attempt = uninstall(h)
    require(attempt.returncode != 0, "uninstall abandoned original missing-host obligation")
    retained = h.get("nodeprofile", h.profile)
    require(retained["metadata"].get("deletionTimestamp") and retained["metadata"].get("finalizers")
            and missing_obligation(retained, old, frozen["metadata"]["uid"]),
            "failed uninstall discarded unresolved physical-host history")
    name = "retirement-evidence-" + h.run_id
    account = "retirement-attestor"
    h.apply({"apiVersion": "v1", "kind": "ServiceAccount",
             "metadata": {"name": account, "namespace": h.namespace}})
    actor = "system:serviceaccount:" + h.namespace + ":" + account
    target = obligation["targets"][0]
    evidence = {"apiVersion": API, "kind": "NodeRetirementEvidence",
        "metadata": {"name": name, "labels": {OWNER: h.run_id}},
        "spec": {"profileName": h.profile, "profileUID": uid, "nodeName": old,
                 "nodeUID": frozen["metadata"]["uid"], "providerID": target.get("providerID", ""),
                 "systemUUID": target.get("systemUUID", ""), "externalInstanceID": container,
                 "platformRecord": "Exact invocation-owned Docker worker successfully destroyed "
                                   "with docker rm -f -v; original Node UID removed afterward.",
                 "permanentlyDecommissioned": True}}
    denial = h.kubectl("create", "-f", "-", "--as=" + actor,
                       input=json.dumps(evidence), check=False)
    require(denial.returncode != 0 and "Forbidden" in denial.stderr,
            "unbound actor could assert permanent external retirement")
    require(h.get("noderetirementevidence", name, absent=True) is None,
            "denied external evidence still created authority")
    h.apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
             "metadata": {"name": h.cluster + "-attestor"},
             "subjects": [{"kind": "ServiceAccount", "name": account, "namespace": h.namespace}],
             "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole",
                         "name": h.cluster + "-brewlet-retirement-attestor"}})
    h.kubectl("create", "-f", "-", "--as=" + actor, input=json.dumps(evidence))
    accepted = h.wait("real authorized original-identity evidence resolves", lambda:
        (value if value.get("status", {}).get("resolvedAt") else False)
        if (value := h.get("noderetirementevidence", name)) else False)
    require_owned(accepted, h.run_id)
    status = accepted["status"]
    require(status.get("acceptedAt") and status["originalTarget"]["uid"] == frozen["metadata"]["uid"]
            and status.get("cleanupSpec") and status.get("cleanupGeneration", 0) > 0,
            "external evidence lost its frozen original cleanup identity/policy")
    h.wait("resolved external history permits genuine profile finalization", lambda:
           h.get("nodeprofile", h.profile, absent=True) is None)
    mutation = h.kubectl("patch", "noderetirementevidence", name, "--type=merge", "-p",
                         '{"spec":{"platformRecord":"changed"}}', check=False)
    require(mutation.returncode != 0 and "immutable" in mutation.stderr.lower(),
            "accepted retirement evidence was mutable")
    deletion = h.kubectl("delete", "noderetirementevidence", name, "--wait=false", check=False)
    require(deletion.returncode != 0 and "retained" in deletion.stderr.lower(),
            "accepted retirement evidence could be deleted after profile finalization")
    require(uninstall(h).returncode == 0, "resolved external obligation still prevents normal uninstall")
    preserved = h.get("noderetirementevidence", name)
    require(preserved["metadata"]["uid"] == accepted["metadata"]["uid"]
            and preserved["status"]["originalTarget"] == status["originalTarget"],
            "actual uninstall discarded accepted independent history")
    h.record("authorized immutable external retirement survives profile deletion and real uninstall",
             {"evidenceUID": accepted["metadata"]["uid"], "originalNodeUID": frozen["metadata"]["uid"],
              "replacementNodeUID": h.node_uid, "priorRBACDenial": True,
              "returningPhysicalHostVerified": False, "noFakeHostCleanupReceipt": True},
             ["OWN-03", "OWN-04"])


def run(work):
    h = Harness(work)
    status = 1
    h.result["gaps"].append("Focused real missing-host retirement; returning physical host not verified")
    try:
        for phase, action in (("prerequisites", h.preflight),
                              ("cluster", lambda: h.provision_cluster(workers=1)),
                              ("build", h.prepare_images),
                              ("delivery-bootstrap", lambda: delivery.bootstrap(h)),
                              ("install", h.install), ("external-retirement", lambda: exercise(h))):
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
        print("Private physical-retirement evidence:", h.work, flush=True)
    return status


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", required=True, type=Path)
    def interrupted(signum, frame):
        raise Failure("retirement acceptance interrupted by signal " + str(signum))
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    raise SystemExit(run(parser.parse_args().work))
