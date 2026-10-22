"""Real API admission, including invalid/conflicting/deleting zero-node profiles."""

import copy
import json
import sys

from harness import API, IDS, OWNER, ROOT, Failure, require, require_owned


HOLD = "acceptance.brewlet.test/admission-observation"


def profile_webhook(h):
    candidates = [obj for obj in h.get("validatingwebhookconfigurations")["items"]
                  if obj["metadata"].get("annotations", {}).get("meta.helm.sh/release-name") == h.cluster
                  and obj["metadata"].get("annotations", {}).get("meta.helm.sh/release-namespace") == h.control_namespace
                  and obj["metadata"].get("labels", {}).get("app.kubernetes.io/managed-by") == "Helm"
                  and any(hook.get("clientConfig", {}).get("service", {}).get("namespace") == h.control_namespace
                          and hook.get("clientConfig", {}).get("service", {}).get("path") == "/validate-nodeprofiles"
                          for hook in obj.get("webhooks", []))]
    require(len(candidates) == 1, "exact private release profile webhook not found")
    return candidates[0]


def release_observation_hold(h, name, uid):
    def release():
        obj = h.get("nodeprofile", name, absent=True)
        if obj is None:
            return True
        require_owned(obj, h.run_id, uid)
        finalizers = obj["metadata"].get("finalizers", [])
        if HOLD not in finalizers:
            return True
        index = finalizers.index(HOLD)
        patch = [{"op": "test", "path": "/metadata/uid", "value": uid},
                 {"op": "test", "path": "/metadata/resourceVersion", "value": obj["metadata"]["resourceVersion"]},
                 {"op": "test", "path": f"/metadata/finalizers/{index}", "value": HOLD},
                 {"op": "remove", "path": f"/metadata/finalizers/{index}"}]
        response = h.kubectl("patch", "nodeprofile", name, "--type=json", "-p", json.dumps(patch), check=False)
        if response.returncode == 0:
            return True
        require("(Conflict)" in response.stderr, "fixture hold release failed: " + response.stderr)
        return False
    h.wait("UID-bound fixture observation hold release", release)


def initialized_empty_ownership(obj, run_id, uid, generation, spec):
    require_owned(obj, run_id, uid)
    require(obj["metadata"]["generation"] == generation and obj["spec"] == spec,
            "empty-pool fixture changed while awaiting ownership initialization")
    status = obj.get("status", {})
    if (status.get("ownershipInitialized") is not True or status.get("provisioningGeneration") != generation
            or status.get("provisioningSpec") != spec):
        return False
    targets = status.get("targets", [])
    require(isinstance(targets, list) and not targets and not status.get("retirement")
            and not status.get("detachedRetirements"), "empty-pool fixture retains node/retirement obligations")
    return obj


def request_empty_profile_deletion(h, name, uid, valid_spec):
    require(h.owned.get(("nodeprofile", "", name)) == uid, "cannot clean an unrecorded admission fixture")
    valid_spec = copy.deepcopy(valid_spec)
    # Go status snapshots include these value-struct fields even when empty.
    valid_spec.setdefault("registry", {})
    valid_spec.setdefault("rollout", {})
    pool = valid_spec["nodePool"]
    require(pool.get("key") and pool.get("names"), "fixture cleanup must not select a catch-all pool")

    def no_nodes():
        nodes = h.get("nodes")["items"]
        require(not any(node["metadata"].get("labels", {}).get(pool["key"]) in pool["names"] for node in nodes),
                "admission fixture cleanup pool is not empty")
        require(not any(node["metadata"].get("labels", {}).get("brewlet.sh/owner-uid") == uid
                        or node["metadata"].get("annotations", {}).get("brewlet.sh/owner-name") == name for node in nodes),
                "admission fixture unexpectedly owns a Node")

    def restore():
        current = h.get("nodeprofile", name, absent=True)
        if current is None:
            return (None,)
        require_owned(current, h.run_id, uid)
        no_nodes()
        if current["metadata"].get("deletionTimestamp"):
            require(current["spec"] == valid_spec, "deleting invalid admission fixture must retain finalizers for review")
            return (current,)
        try:
            return (h.replace_inventory_spec(current, copy.deepcopy(valid_spec)),)
        except Failure as error:
            if "(Conflict)" not in str(error):
                raise
            return False
    (obj,) = h.wait("UID/version-bound admission empty-pool spec restoration", restore)
    if obj is None:
        return
    generation, spec = obj["metadata"]["generation"], copy.deepcopy(obj["spec"])
    if obj["metadata"].get("deletionTimestamp"):
        require(initialized_empty_ownership(obj, h.run_id, uid, generation, spec),
                "deleting admission fixture has no current initialized ownership ledger")
        return

    def request():
        current = h.get("nodeprofile", name, absent=True)
        if current is None:
            return True
        ready = initialized_empty_ownership(current, h.run_id, uid, generation, spec)
        if not ready:
            return False
        no_nodes()
        if current["metadata"].get("deletionTimestamp"):
            return True
        response = h.kubectl("delete", "--raw", "/apis/" + API + "/nodeprofiles/" + name, "-f", "-",
            input=json.dumps({"apiVersion": "v1", "kind": "DeleteOptions",
                              "preconditions": {"uid": uid, "resourceVersion": current["metadata"]["resourceVersion"]},
                              "propagationPolicy": "Foreground"}), check=False)
        if response.returncode == 0:
            return True
        require("(Conflict)" in response.stderr, "initialized admission fixture deletion failed: " + response.stderr)
        return False
    h.wait("current-generation empty ownership before UID/version-bound admission deletion", request)


def exercise(h):
    require(not h.get("nodeprofiles")["items"], "admission contract requires its own empty profile inventory")
    name = h.profile + "-admission"
    profile = h.profile_object()
    profile["metadata"].update(name=name, finalizers=[HOLD])
    profile["spec"]["runtimeImages"] = [{"id": IDS[h.probe_language], "install": "OnDemand"}]
    profile["spec"]["nodePool"]["names"] = ["absent-" + h.run_id]
    original_spec = copy.deepcopy(profile["spec"])
    conflict_spec = copy.deepcopy(original_spec)
    conflict_spec["nodePool"]["names"] = ["disjoint-" + h.run_id]
    obj = h.apply(profile)
    uid = obj["metadata"]["uid"]
    webhook = None
    competing = None
    try:
        runtime = h.get("runtimeimage", IDS[h.probe_language])
        h.run([sys.executable, ROOT / "integration-tests/admission.py",
               "--kubeconfig", h.kubeconfig, "--runtime-id", IDS[h.probe_language],
               "--runtime-source", h.runtime_images[h.probe_language]["before"],
               "--app-image", h.app_images[h.probe_language],
               "--profile-name", name, "--pool-name", profile["spec"]["nodePool"]["names"][0],
               "--existing-runtime-uid", runtime["metadata"]["uid"], "--existing-profile-uid", uid],
              timeout=300)
        h.record("real API security defaults, exact selection, affinity, hints and startup-fence refusal",
                 {"profileUID": uid, "inventoryUID": runtime["metadata"]["uid"]},
                 ["SEC-01", "SEC-02", "ADM-01", "ADM-02", "ADM-04", "FENCE-02", "E2E-04"])

        webhook = copy.deepcopy(profile_webhook(h))
        changed = copy.deepcopy(webhook)
        for hook in changed["webhooks"]:
            if hook.get("clientConfig", {}).get("service", {}).get("path") == "/validate-nodeprofiles":
                hook.setdefault("objectSelector", {}).setdefault("matchExpressions", []).append(
                    {"key": OWNER, "operator": "NotIn", "values": [h.run_id]})
        h.kubectl("replace", "-f", "-", input=json.dumps(changed))
        pod = h.pod("admission-profile-selection", h.probe_language)

        def selection(allowed):
            result = h.kubectl("create", "--dry-run=server", "-f", "-", "-o", "json",
                               input=json.dumps(pod), check=False)
            if allowed:
                require(result.returncode == 0 and name in result.stderr,
                        "valid zero-node profile did not authorize API admission")
            else:
                require(result.returncode != 0 and "NoCompatibleRuntime" in result.stderr,
                        "ineligible profile authorized placement or failed for a different reason")

        invalid = copy.deepcopy(original_spec)
        invalid.setdefault("registry", {})["mirrors"] = {"registry.example.com": "forbidden.invalid"}
        h.replace_inventory_spec(h.get("nodeprofile", name), invalid)
        selection(False)
        h.replace_inventory_spec(h.get("nodeprofile", name), original_spec)
        selection(True)
        conflict = copy.deepcopy(profile)
        conflict["metadata"] = {"name": name + "-conflict"}
        competing = h.apply(conflict)
        selection(False)
        request_empty_profile_deletion(h, competing["metadata"]["name"], competing["metadata"]["uid"], conflict_spec)
        h.wait("conflicting zero-node fixture finalized", lambda:
               h.get("nodeprofile", competing["metadata"]["name"], absent=True) is None)
        selection(True)
        request_empty_profile_deletion(h, name, uid, original_spec)
        h.wait("real profile deletion begins", lambda: h.get("nodeprofile", name)["metadata"].get("deletionTimestamp"))
        selection(False)
    finally:
        try:
            if competing is not None and h.get("nodeprofile", competing["metadata"]["name"], absent=True) is not None:
                request_empty_profile_deletion(h, competing["metadata"]["name"], competing["metadata"]["uid"], conflict_spec)
                h.wait("conflicting admission fixture removed", lambda:
                       h.get("nodeprofile", competing["metadata"]["name"], absent=True) is None)
            current = h.get("nodeprofile", name, absent=True)
            if current is not None:
                require_owned(current, h.run_id, uid)
                request_empty_profile_deletion(h, name, uid, original_spec)
                release_observation_hold(h, name, uid)
                h.wait("zero-node admission fixture finalized", lambda: h.get("nodeprofile", name, absent=True) is None)
        finally:
            if webhook is not None:
                current = h.get("validatingwebhookconfiguration", webhook["metadata"]["name"])
                require(current["metadata"]["uid"] == webhook["metadata"]["uid"],
                        "private profile webhook replaced; refusing restoration")
                current["webhooks"] = webhook["webhooks"]
                h.kubectl("replace", "-f", "-", input=json.dumps(current))
    h.record("real invalid-mirror, conflicting and genuinely deleting zero-node profiles are excluded",
             {"profileUID": uid, "noStatusOrProductionFinalizerEdits": True}, ["ADM-03", "E2E-01"])
