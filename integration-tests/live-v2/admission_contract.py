"""Real API admission, including invalid/conflicting/deleting zero-node profiles."""

import copy
from contextlib import contextmanager
from dataclasses import dataclass
import json
import sys

from harness import API, IDS, OWNER, ROOT, Failure, require, require_owned


HOLD = "acceptance.brewlet.test/admission-observation"


def canonical_spec(spec):
    return json.dumps(spec, sort_keys=True, separators=(",", ":")).encode()


@dataclass(frozen=True)
class EmptyDeletionWitness:
    name: str
    uid: str
    owner: str
    generation: int
    resource_version: str
    spec: bytes
    provisioning_spec: bytes

    @classmethod
    def capture(cls, obj, run_id):
        metadata = obj["metadata"]
        generation = metadata["generation"]
        require(type(generation) is int and generation > 0 and not metadata.get("deletionTimestamp"),
                "pre-delete witness requires a live positive-generation fixture")
        require(isinstance(metadata.get("resourceVersion"), str) and metadata["resourceVersion"],
                "pre-delete witness requires an observed resource version")
        require(type(obj.get("status", {}).get("provisioningGeneration")) is int,
                "pre-delete witness requires an integer provisioning generation")
        require(initialized_empty_ownership(obj, run_id, metadata["uid"], generation, obj["spec"]),
                "pre-delete witness requires current initialized ownership")
        return cls(metadata["name"], metadata["uid"], run_id, generation, metadata["resourceVersion"],
                   canonical_spec(obj["spec"]), canonical_spec(obj["status"]["provisioningSpec"]))


def deletion_witnesses(h):
    return vars(h).setdefault("_empty_profile_deletions", {})


def observe_ownership(h, fixture, phase, obj, uid, generation=None, spec=None, *, witness=None):
    metadata = obj.get("metadata", {}) if obj else {}
    status = obj.get("status", {}) if obj else {}
    integer = lambda value: value if type(value) is int else None
    targets = status.get("targets", [])
    provisioning_generation = witness.generation if witness else generation
    observation = {
        "present": obj is not None,
        "uidMatches": metadata.get("uid") == uid,
        "ownerMatches": metadata.get("labels", {}).get(OWNER) == h.run_id,
        "generation": integer(metadata.get("generation")),
        "expectedGeneration": integer(generation),
        "generationMatches": generation is not None and metadata.get("generation") == generation,
        "specMatches": spec is not None and obj is not None and obj.get("spec") == spec,
        "ownershipInitialized": status.get("ownershipInitialized") is True,
        "ownershipInitializedPresent": "ownershipInitialized" in status,
        "provisioningGeneration": integer(status.get("provisioningGeneration")),
        "expectedProvisioningGeneration": integer(provisioning_generation),
        "provisioningGenerationMatches": provisioning_generation is not None
                                        and status.get("provisioningGeneration") == provisioning_generation,
        "provisioningSpecMatches": spec is not None and status.get("provisioningSpec") == spec,
        "targetsValid": isinstance(targets, list),
        "targetsEmpty": isinstance(targets, list) and not targets,
        "retirementPresent": status.get("retirement") is not None,
        "detachedRetirementsPresent": status.get("detachedRetirements", []) != [],
        "deleting": bool(metadata.get("deletionTimestamp")),
        "observationHoldPresent": HOLD in metadata.get("finalizers", []),
        "preDeleteWitnessPresent": witness is not None,
    }
    h.result.setdefault("fixtureLifecycle", {}).setdefault("observations", {}).setdefault(fixture, {})[phase] = observation


def record_failure(h, role, stage, error):
    message = str(error)
    code = ("timeout" if message.startswith("deadline waiting for ") else
            "command" if message.startswith("command failed ") else
            "ownership" if "ownership" in message else
            "obligations" if "obligations" in message else
            "contract" if isinstance(error, Failure) else "unexpected")
    lifecycle = h.result.setdefault("fixtureLifecycle", {})
    lifecycle.setdefault("errors", []).append(
        {"role": role, "stage": stage, "code": code, "privateMessage": message,
         "observations": copy.deepcopy(lifecycle.get("observations", {}))})
    return code


@contextmanager
def preserve_failures(h, cleanups):
    primary = None
    try:
        yield
    except Exception as error:
        primary = error
        record_failure(h, "primary", "assertions", error)
        raise
    finally:
        first_cleanup_error = None
        for stage, cleanup in cleanups:
            try:
                cleanup()
            except Exception as error:
                role = "secondary" if primary is not None or first_cleanup_error is not None else "primary"
                code = record_failure(h, role, stage, error)
                if role == "primary":
                    first_cleanup_error = error
                else:
                    original = primary if primary is not None else first_cleanup_error
                    note = "secondary fixture cleanup failure: stage=" + stage + " code=" + code
                    original.add_note(note)
                    print(note, file=sys.stderr, flush=True)
        if primary is None and first_cleanup_error is not None:
            raise first_cleanup_error


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


def require_empty_nodes(h, name, uid, spec):
    pool = spec["nodePool"]
    require(pool.get("key") and pool.get("names"), "fixture cleanup must not select a catch-all pool")
    nodes = h.get("nodes")["items"]
    require(not any(node["metadata"].get("labels", {}).get(pool["key"]) in pool["names"] for node in nodes),
            "admission fixture cleanup pool is not empty")
    require(not any(node["metadata"].get("labels", {}).get("brewlet.sh/owner-uid") == uid
                    or node["metadata"].get("annotations", {}).get("brewlet.sh/owner-name") == name for node in nodes),
            "admission fixture unexpectedly owns a Node")


def witnessed_deletion(obj, run_id, uid, spec, witness):
    require_owned(obj, run_id, uid)
    require(witness is not None, "deleting admission fixture has no pre-delete witness")
    metadata, status = obj["metadata"], obj.get("status", {})
    require((witness.name, witness.uid, witness.owner) == (metadata["name"], uid, run_id)
            and witness.spec == canonical_spec(spec), "pre-delete witness identity/spec mismatch")
    # API registry markAsDeleting bumps positive generation once for non-graceful finalization.
    require(metadata.get("deletionTimestamp")
            and type(metadata.get("deletionGracePeriodSeconds")) is int
            and metadata["deletionGracePeriodSeconds"] == 0
            and metadata["generation"] == witness.generation + 1
            and isinstance(metadata.get("resourceVersion"), str)
            and metadata["resourceVersion"] not in ("", witness.resource_version),
            "unexpected witnessed fixture deletion transition")
    require(canonical_spec(obj["spec"]) == witness.spec
            and status.get("ownershipInitialized") is True
            and type(status.get("provisioningGeneration")) is int
            and status.get("provisioningGeneration") == witness.generation
            and canonical_spec(status.get("provisioningSpec")) == witness.provisioning_spec,
            "deleting fixture lost witnessed spec/ownership freshness")
    require_empty_obligations(obj)
    return obj


def release_observation_hold(h, name, uid, *, valid_spec, fixture="empty-profile"):
    def release():
        obj = h.get("nodeprofile", name, absent=True)
        witness = deletion_witnesses(h).get((name, uid))
        observe_ownership(h, fixture, "hold-release", obj, uid,
                          witness.generation + 1 if witness else None, valid_spec, witness=witness)
        if obj is None:
            return True
        witnessed_deletion(obj, h.run_id, uid, valid_spec, witness)
        finalizers = obj["metadata"].get("finalizers", [])
        if HOLD not in finalizers:
            return True
        require_empty_nodes(h, name, uid, valid_spec)
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


def require_empty_obligations(obj):
    status = obj.get("status", {})
    targets = status.get("targets", [])
    detached = status.get("detachedRetirements", [])
    require(isinstance(targets, list) and not targets and status.get("retirement") is None
            and isinstance(detached, list) and not detached, "empty-pool fixture retains node/retirement obligations")


def initialized_empty_ownership(obj, run_id, uid, generation, spec):
    require_owned(obj, run_id, uid)
    require(obj["metadata"]["generation"] == generation and obj["spec"] == spec,
            "empty-pool fixture changed while awaiting ownership initialization")
    status = obj.get("status", {})
    if (status.get("ownershipInitialized") is not True or status.get("provisioningGeneration") != generation
            or status.get("provisioningSpec") != spec):
        return False
    require_empty_obligations(obj)
    return obj


def empty_spec(valid_spec):
    spec = copy.deepcopy(valid_spec)
    # Go status snapshots include these value-struct fields even when empty.
    spec.setdefault("registry", {})
    spec.setdefault("rollout", {})
    return spec


def request_empty_profile_deletion(h, name, uid, valid_spec, *, fixture="empty-profile"):
    require(h.owned.get(("nodeprofile", "", name)) == uid, "cannot clean an unrecorded admission fixture")
    valid_spec = empty_spec(valid_spec)
    witnesses = deletion_witnesses(h)

    def restore():
        current = h.get("nodeprofile", name, absent=True)
        witness = witnesses.get((name, uid))
        generation = current["metadata"]["generation"] if current else None
        if witness:
            generation = witness.generation + 1
        observe_ownership(h, fixture, "restore", current, uid, generation, valid_spec, witness=witness)
        if current is None:
            return (None,)
        require_owned(current, h.run_id, uid)
        require_empty_nodes(h, name, uid, valid_spec)
        if current["metadata"].get("deletionTimestamp"):
            observe_ownership(h, fixture, "deletion", current, uid,
                              witness.generation + 1 if witness else None, valid_spec, witness=witness)
            witnessed_deletion(current, h.run_id, uid, valid_spec, witness)
            return (current,)
        require((name, uid) not in witnesses, "acknowledged fixture deletion is no longer observable")
        if current["spec"] == valid_spec:
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
        return

    def request():
        current = h.get("nodeprofile", name, absent=True)
        observe_ownership(h, fixture, "deletion", current, uid, generation, spec)
        if current is None:
            return True
        require(not current["metadata"].get("deletionTimestamp"),
                "fixture began deleting without an acknowledged pre-delete witness")
        ready = initialized_empty_ownership(current, h.run_id, uid, generation, spec)
        if not ready:
            return False
        require_empty_nodes(h, name, uid, valid_spec)
        witness = EmptyDeletionWitness.capture(current, h.run_id)
        response = h.kubectl("delete", "--raw", "/apis/" + API + "/nodeprofiles/" + name, "-f", "-",
            input=json.dumps({"apiVersion": "v1", "kind": "DeleteOptions",
                              "preconditions": {"uid": uid, "resourceVersion": current["metadata"]["resourceVersion"]},
                              "propagationPolicy": "Foreground"}), check=False)
        if response.returncode == 0:
            witnesses[(name, uid)] = witness
            return True
        require("(Conflict)" in response.stderr, "initialized admission fixture deletion failed: " + response.stderr)
        return False
    h.wait("current-generation empty ownership before UID/version-bound admission deletion", request)


def finish_empty_profile_deletion(h, name, uid, valid_spec, *, fixture="empty-profile"):
    request_empty_profile_deletion(h, name, uid, valid_spec, fixture=fixture)
    release_observation_hold(h, name, uid, valid_spec=empty_spec(valid_spec), fixture=fixture)
    def absent():
        obj = h.get("nodeprofile", name, absent=True)
        witness = deletion_witnesses(h).get((name, uid))
        observe_ownership(h, fixture, "absence", obj, uid,
                          witness.generation + 1 if witness else None, empty_spec(valid_spec), witness=witness)
        if obj is not None:
            witnessed_deletion(obj, h.run_id, uid, empty_spec(valid_spec), witness)
        return obj is None
    h.wait("normal empty-profile deletion", absent)


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

    def cleanup_conflict():
        if competing is not None:
            finish_empty_profile_deletion(h, competing["metadata"]["name"], competing["metadata"]["uid"],
                                          conflict_spec, fixture="admission-conflict")

    def cleanup_held():
        finish_empty_profile_deletion(h, name, uid, original_spec, fixture="admission-held")

    def restore_webhook():
        if webhook is not None:
            current = h.get("validatingwebhookconfiguration", webhook["metadata"]["name"])
            require(current["metadata"]["uid"] == webhook["metadata"]["uid"],
                    "private profile webhook replaced; refusing restoration")
            current["webhooks"] = webhook["webhooks"]
            h.kubectl("replace", "-f", "-", input=json.dumps(current))

    with preserve_failures(h, (("conflict-cleanup", cleanup_conflict), ("held-cleanup", cleanup_held),
                               ("webhook-restore", restore_webhook))):
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
        finish_empty_profile_deletion(h, competing["metadata"]["name"], competing["metadata"]["uid"],
                                      conflict_spec, fixture="admission-conflict")
        selection(True)
        request_empty_profile_deletion(h, name, uid, original_spec, fixture="admission-held")

        def deleting():
            current = h.get("nodeprofile", name, absent=True)
            witness = deletion_witnesses(h).get((name, uid))
            observe_ownership(h, "admission-held", "deleting-observation", current, uid,
                              witness.generation + 1 if witness else None, empty_spec(original_spec), witness=witness)
            require(current is not None, "held fixture disappeared before deleting-state assertion")
            witnessed_deletion(current, h.run_id, uid, empty_spec(original_spec), witness)
            require(HOLD in current["metadata"].get("finalizers", []),
                    "fixture observation hold disappeared before deleting-state assertion")
            return bool(current["metadata"].get("deletionTimestamp"))

        h.wait("real profile deletion begins", deleting)
        selection(False)
        require(deleting(), "held fixture stopped deleting during admission assertion")
    h.record("real invalid-mirror, conflicting and genuinely deleting zero-node profiles are excluded",
             {"profileUID": uid, "noStatusOrProductionFinalizerEdits": True}, ["ADM-03", "E2E-01"])
