"""Real API admission, including invalid/conflicting/deleting zero-node profiles."""

import copy
import json
import sys

from harness import IDS, OWNER, ROOT, require, require_owned


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
    obj = h.get("nodeprofile", name, absent=True)
    if obj is None:
        return
    require_owned(obj, h.run_id, uid)
    finalizers = obj["metadata"].get("finalizers", [])
    if HOLD in finalizers:
        index = finalizers.index(HOLD)
        patch = [{"op": "test", "path": "/metadata/uid", "value": uid},
                 {"op": "test", "path": "/metadata/resourceVersion", "value": obj["metadata"]["resourceVersion"]},
                 {"op": "test", "path": f"/metadata/finalizers/{index}", "value": HOLD},
                 {"op": "remove", "path": f"/metadata/finalizers/{index}"}]
        h.kubectl("patch", "nodeprofile", name, "--type=json", "-p", json.dumps(patch))


def exercise(h):
    require(not h.get("nodeprofiles")["items"], "admission contract requires its own empty profile inventory")
    name = h.profile + "-admission"
    profile = h.profile_object()
    profile["metadata"].update(name=name, finalizers=[HOLD])
    profile["spec"]["runtimeImages"] = [{"id": IDS[h.probe_language], "install": "OnDemand"}]
    profile["spec"]["nodePool"]["names"] = ["absent-" + h.run_id]
    original_spec = copy.deepcopy(profile["spec"])
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
        h.delete("nodeprofile", competing["metadata"]["name"])
        h.wait("conflicting zero-node fixture finalized", lambda:
               h.get("nodeprofile", competing["metadata"]["name"], absent=True) is None)
        selection(True)
        h.delete("nodeprofile", name)
        h.wait("real profile deletion begins", lambda: h.get("nodeprofile", name)["metadata"].get("deletionTimestamp"))
        selection(False)
    finally:
        try:
            if competing is not None and h.get("nodeprofile", competing["metadata"]["name"], absent=True) is not None:
                h.delete("nodeprofile", competing["metadata"]["name"])
                h.wait("conflicting admission fixture removed", lambda:
                       h.get("nodeprofile", competing["metadata"]["name"], absent=True) is None)
            current = h.get("nodeprofile", name, absent=True)
            if current is not None:
                require_owned(current, h.run_id, uid)
                if not current["metadata"].get("deletionTimestamp"):
                    h.replace_inventory_spec(current, original_spec)
                    h.delete("nodeprofile", name)
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
