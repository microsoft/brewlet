#!/usr/bin/env python3
"""Real API/controller acceptance, called after base rotations, before revocation.

Usage (the parent owns lifecycle/accounting): ``control_edges.execute(h)``, then
``before_cleanup(h); h.production_cleanup(); after_cleanup(h)``.
Importing this module performs no I/O. The callback uses its own namespace and
inventory IDs, preserves the base workload leases, and restores release values.
It never writes status, claims, finalizers, authorizations, or cleanup receipts.

Selected: real chart/RBAC double opt-in; native Deployment/StatefulSet/DaemonSet
rollouts and strategy barriers; epoch/UID idempotency; global slots/deadlines;
superseded pending work; excluded-node bound-pod diagnostics/recovery; actual
TLS admission refusals for OnDemand pins and stale capabilities; API-server CAS
and recreated *fixture* Node refusal. API CAS is explicitly NOT controller
claim-release/physical-cleanup proof. Missing selected protocol fails the run.

Still needs parent prerequisites: an additional real disposable worker for
recreatedNodes/foreign physical-host proof; a controllably stalled registry response for
RuntimeImageInstalling deadline/recovery. Those are retained as gaps, not passes.
No live operations are performed by the offline tests.
"""

import base64
import copy
from datetime import datetime, timezone
import json
import os
import shutil
import ssl
import threading
import time
import urllib.request

from harness import API, FENCE, HERE, IDS, OWNER, ROOT, STORE, Failure, inventory_spec_patch, require, require_owned


SELECTED = (
    "chart-rbac-double-opt-in",
    "approved-eager-failure-readiness-gate",
    "native-controller-strategies",
    "uid-epoch-idempotency",
    "global-pacing-and-deadlines",
    "superseded-targets",
    "excluded-bound-pod-recovery",
    "ondemand-pin-stale-capability-recovery",
    "real-api-cas-primitives",
)
GAPS = (
    "E2E-09 recreatedNodes/foreign physical-host refusal: needs another real owned "
    "worker; fixture Node UID/API CAS below is not production claim-release or "
    "host-cleanup proof. Real claim-release CAS requires parent before_cleanup/"
    "after_cleanup hooks and is not covered by execute alone",
    "ROT-09 cold-install deadline/recovery: needs a real runtime registry "
    "transfer gate held beyond workloadProgressDeadline; no fabricated "
    "RuntimeImageInstalling/status/timestamps are accepted",
    "FENCE-04 production uninstall while stale provider pins remain: callback "
    "proves live retention/recovery, not uninstall retention",
)
BARRIER = "brewlet.sh/rotated-to-generation"
RUNTIME = "brewlet.sh/runtime-image"
OPT_IN = "brewlet.sh/restart-on-rotation"
CASE = "acceptance.brewlet.test/control-case"
PROBE = "acceptance.brewlet.test/cas-probe"
DEADLINE = 30


def timestamp(value):
    require(isinstance(value, str) and bool(value), "missing server timestamp")
    return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()


def barrier_for(image):
    return {"id": image["metadata"]["name"], "inventoryUID": image["metadata"]["uid"],
            "generation": image["status"]["generation"]}


def template_barrier(obj):
    raw = obj["spec"]["template"].get("metadata", {}).get("annotations", {}).get(BARRIER)
    return json.loads(raw) if raw else None


def controller_reference(obj):
    owners = [r for r in obj.get("metadata", {}).get("ownerReferences", [])
              if r.get("controller") is True]
    require(len(owners) <= 1, "multiple controlling owners")
    return owners[0] if owners else None


def owned_pods(workload, pods, replicasets, run_id):
    """Selectors are discovery hints, never evidence of controller ownership."""
    uid = require_owned(workload, run_id)
    namespace = workload["metadata"]["namespace"]
    rs_uids = {rs["metadata"]["uid"] for rs in replicasets
               if rs["metadata"].get("namespace") == namespace
               and controller_reference(rs)
               and controller_reference(rs)["kind"] == "Deployment"
               and controller_reference(rs)["uid"] == uid}
    result = []
    for pod in pods:
        owner = controller_reference(pod)
        if pod["metadata"].get("namespace") != namespace or not owner:
            continue
        matches = (owner["kind"] == "ReplicaSet" and owner["uid"] in rs_uids
                   if workload["kind"] == "Deployment"
                   else owner["kind"] == workload["kind"] and owner["uid"] == uid)
        if matches:
            require_owned(pod, run_id)
            result.append(pod)
    return result


def pod_ready(pod):
    return (not pod["metadata"].get("deletionTimestamp")
            and pod.get("status", {}).get("phase") == "Running"
            and any(c["type"] == "Ready" and c["status"] == "True"
                    for c in pod.get("status", {}).get("conditions", [])))


def pod_identity(pod):
    statuses = pod.get("status", {}).get("containerStatuses", [])
    require(len(statuses) == 1 and bool(statuses[0].get("containerID")),
            "no actual application container identity")
    return (pod["metadata"]["uid"], statuses[0]["containerID"],
            statuses[0].get("restartCount", 0))


def eager_failure_evidence(node, profile, image, authorization):
    require(authorization.get("install") == "Eager" and not authorization.get("revoked"),
            "failed Eager probe lacks an active Eager authorization")
    require(authorization.get("state") in ("installing", "failed"),
            "invalid Eager root was made usable")
    if authorization["state"] != "failed":
        return False
    generation = profile["metadata"]["generation"]
    if (profile.get("status", {}).get("provisioningGeneration") != generation
            or authorization.get("profileGeneration") != generation):
        return False
    require(authorization.get("id") == image["metadata"]["name"]
            and authorization.get("inventoryUID") == image["metadata"]["uid"]
            and authorization.get("generation") == image["status"]["generation"]
            and authorization.get("digest") == image["status"]["digest"]
            and authorization.get("profileUID") == profile["metadata"]["uid"],
            "failed Eager authorization is not the actual approved target/owner")
    labels = node["metadata"].get("labels", {})
    require(labels.get("brewlet.sh/owner-uid") == profile["metadata"]["uid"]
            and labels.get("brewlet.sh/owner-node-uid") == node["metadata"]["uid"],
            "failed Eager observation lacks the literal profile/Node claim")
    fenced = any(t.get("key") == FENCE and t.get("value") == "provisioning"
                 and t.get("effect") == "NoSchedule" for t in node.get("spec", {}).get("taints", []))
    require(fenced or labels.get("brewlet.sh/runtime") != "ready",
            "approved Eager failure left the node effectively runtime-ready")
    require("brewlet.sh/runtime." + image["metadata"]["name"] not in labels,
            "failed Eager ID retained a scheduling capability")
    require(not any(c.get("type") == "Ready" and c.get("status") == "True"
                    and c.get("observedGeneration") == generation
                    for c in profile.get("status", {}).get("conditions", [])),
            "profile reported current-generation readiness despite failed required Eager root")
    return {"nodeUID": node["metadata"]["uid"], "profileUID": profile["metadata"]["uid"],
            "profileGeneration": generation, "inventoryUID": image["metadata"]["uid"],
            "generation": authorization["generation"], "state": authorization["state"],
            "startupFenced": fenced, "baselineLabel": labels.get("brewlet.sh/runtime")}


def native_complete(obj):
    s, spec = obj.get("status", {}), obj["spec"]
    if s.get("observedGeneration", 0) < obj["metadata"]["generation"]:
        return False
    if obj["kind"] == "Deployment":
        count = spec.get("replicas", 1)
        return all(s.get(k, 0) == count for k in
                   ("replicas", "updatedReplicas", "readyReplicas", "availableReplicas"))
    if obj["kind"] == "StatefulSet":
        return (s.get("updatedReplicas", 0) == spec.get("replicas", 1)
                and s.get("readyReplicas", 0) == spec.get("replicas", 1)
                and bool(s.get("currentRevision"))
                and s["currentRevision"] == s.get("updateRevision"))
    count = s.get("desiredNumberScheduled", 0)
    return (count > 0 and all(s.get(k, 0) == count for k in
                             ("updatedNumberScheduled", "numberAvailable", "numberReady"))
            and s.get("numberUnavailable", 0) == 0)


def restart_record(image, workload):
    target = barrier_for(image)
    matches = [r for r in image.get("status", {}).get("restarts", {}).get("workloads", [])
               if r.get("uid") == workload["metadata"]["uid"]
               and r.get("inventoryUID") == target["inventoryUID"]
               and r.get("generation") == target["generation"]]
    require(len(matches) <= 1, "duplicate UID/epoch/generation restart intent")
    return matches[0] if matches else None


def verify_restart(image, workload, before, after, record, *, expected):
    """A template patch is not a completed runtime/controller restart."""
    require(record is not None and record["phase"] == expected,
            "missing or incorrect per-workload restart outcome: " + str(record))
    require(record.get("uid") == workload["metadata"]["uid"]
            and record.get("inventoryUID") == image["metadata"]["uid"]
            and record.get("generation") == image["status"]["generation"],
            "restart outcome is for a different workload UID or inventory epoch/target")
    if expected == "EpochMismatch":
        require(record.get("reason") == "RuntimeImageEpochMismatch",
                "wrong-epoch barrier was not explicitly refused")
        require({pod_identity(p) for p in before} == {pod_identity(p) for p in after}
                and not record.get("completedAt"), "epoch refusal restarted or claimed completed pods")
        return
    require(template_barrier(workload) == barrier_for(image),
            "operator did not install the exact approved epoch/generation barrier")
    require(record.get("patched") is True, "restart patch lacks persisted intent/readback")
    if expected == "Complete":
        require(record.get("reason") == "RestartComplete" and native_complete(workload),
                "API patch was incorrectly accepted as native rollout completion")
        require(bool(after) and all(pod_ready(p) for p in after),
                "replacement application pods are not actually Running/Ready")
        old = {p["metadata"]["uid"] for p in before}
        require(not old.intersection(p["metadata"]["uid"] for p in after),
                "old pods survived a claimed completed restart")
        require(all(p["metadata"].get("annotations", {}).get(BARRIER)
                    and json.loads(p["metadata"]["annotations"][BARRIER]) == barrier_for(image)
                    for p in after), "replacement pods did not inherit the barrier")
    elif expected in ("Deferred", "NotApplicable"):
        require(record.get("reason") == "Restart" + expected,
                "non-actionable workload lacks explicit reason")
        require({pod_identity(p) for p in before} == {pod_identity(p) for p in after},
                "operator deleted/restarted a pod behind a paused/partition/OnDelete strategy")
        require(record.get("nonActionable") is True,
                "non-actionable workload retained a concurrency slot")
    else:
        raise Failure("unsupported restart assertion " + expected)


def verify_pacing(images, maximum, now, deadline):
    active = []
    for image in images:
        for row in image.get("status", {}).get("restarts", {}).get("workloads", []):
            if (row.get("inventoryUID") != image["metadata"]["uid"]
                    or row.get("generation") != image["status"]["generation"]):
                continue
            if row["phase"] in ("Patching", "Rolling") and not row.get("nonActionable"):
                started = timestamp(row.get("startedAt"))
                require(started <= now + 2, "restart intent timestamp is in the future")
                if now - started < deadline:
                    active.append(row)
    require(len(active) <= maximum, "cluster-wide restart concurrency exceeded")
    return active


def blocked_pod_evidence(record, pods, target, excluded):
    """Reject mere fixture annotations, inferred waiting, and UID/name mismatches."""
    require(record and record.get("phase") == "Deferred"
            and record.get("reason") == "RestartDeferred",
            "runtime-blocked rollout was not explicitly deferred")
    by_uid = {p["metadata"]["uid"]: p for p in pods}
    for blocked in record.get("blockedPods", []):
        pod = by_uid.get(blocked.get("uid"))
        if pod is None:
            continue
        node = pod.get("spec", {}).get("nodeName")
        require(blocked.get("node") == node and bool(node)
                and blocked.get("name") == pod["metadata"]["name"]
                and blocked.get("namespace") == pod["metadata"]["namespace"]
                and blocked.get("minimumGeneration") == target,
                "blocked diagnostic does not identify the actual Pod/node/minimum")
        messages = []
        for key in ("containerStatuses", "initContainerStatuses", "ephemeralContainerStatuses"):
            for status in pod.get("status", {}).get(key, []):
                state = status.get("state", {})
                if state.get("running") is not None:
                    continue
                messages.append(json.dumps(state.get("waiting", {})))
                failures = [state.get("terminated")]
                if state.get("waiting") is not None:
                    failures.append(status.get("lastState", {}).get("terminated"))
                for failure in failures:
                    if not failure or failure.get("reason") != "StartError":
                        continue
                    started = failure.get("startedAt")
                    if not started or timestamp(started) <= 0:
                        messages.append(failure.get("message", ""))
        waiting = " ".join(messages)
        reason = blocked.get("reason")
        genuine = reason in ("RuntimeImageInstalling", "RuntimeImageUpdating",
                             "RuntimeImageEpochMismatch", "RuntimeImageAuthorizationExpired",
                             "RuntimeImageRevoked") and reason in waiting
        genuine = genuine or (reason == "NodeExcluded"
                              and pod.get("status", {}).get("phase") != "Running"
                              and any(n.get("name") == node and n.get("uid") for n in excluded))
        require(genuine, "no genuine API waiting/excluded-node evidence for blocked pod")
        return blocked
    raise Failure("Deferred record lacks an actual matching blocked Pod UID")


def fixture_node(obj, run_id, expected_uid, live_node):
    require_owned(obj, run_id, expected_uid)
    require(obj.get("kind") == "Node" and obj["metadata"]["name"] != live_node
            and obj["metadata"]["name"].startswith("control-cas-" + run_id + "-"),
            "CAS probes may only mutate invocation-owned synthetic test Nodes")
    require(not obj.get("status", {}).get("nodeInfo", {}).get("systemUUID")
            and not obj.get("spec", {}).get("providerID"), "CAS probe is a real host")
    labels = obj["metadata"].get("labels", {})
    annotations = obj["metadata"].get("annotations", {})
    require(not any(k.startswith("brewlet.sh/") for k in (*labels, *annotations)),
            "CAS primitive probe must not edit claims or runtime state")
    require(bool(obj["metadata"].get("resourceVersion")), "missing server resourceVersion")


def cas_request(obj, run_id, expected_uid, live_node, value):
    fixture_node(obj, run_id, expected_uid, live_node)
    result = copy.deepcopy(obj)
    result["metadata"].setdefault("annotations", {})[PROBE] = value
    return "/api/v1/nodes/" + obj["metadata"]["name"], result


RELEASE_MARKER = "acceptance.brewlet.test/release-cas"


def pointer(key):
    return key.replace("~", "~0").replace("/", "~1")


def release_trigger(request, config):
    if request.get("resource", {}).get("resource") != "nodes" or request.get("operation") != "UPDATE":
        return False
    before, after = request.get("oldObject") or {}, request.get("object") or {}
    old, new = before.get("metadata", {}), after.get("metadata", {})
    return (old.get("name") == config["node"] and new.get("name") == config["node"]
            and old.get("uid") == config["nodeUID"] and new.get("uid") == config["nodeUID"]
            and old.get("labels", {}).get("brewlet.sh/owner-uid") == config["profileUID"]
            and "brewlet.sh/owner-uid" not in new.get("labels", {}))


def release_patch(node, config, request_uid):
    """The only injected write: a harmless annotation, behind atomic API tests."""
    metadata = node["metadata"]
    require(metadata["uid"] == config["nodeUID"] and metadata["name"] == config["node"],
            "release instrumentation Node identity mismatch")
    require(metadata.get("labels", {}).get("brewlet.sh/owner-uid") == config["profileUID"]
            and metadata.get("labels", {}).get("brewlet.sh/owner-node-uid") == config["nodeUID"]
            and metadata.get("annotations", {}).get("brewlet.sh/owner-name") == config["profile"],
            "release instrumentation found a foreign claim")
    require(RELEASE_MARKER not in metadata.get("annotations", {}),
            "release instrumentation must fire only once")
    require(bool(metadata.get("resourceVersion")) and bool(request_uid), "missing actual API identity")
    marker = json.dumps({"runID": config["runID"], "requestUID": request_uid,
                         "originalResourceVersion": metadata["resourceVersion"],
                         "retirementGeneration": config["generation"]}, sort_keys=True)
    return [
        {"op": "test", "path": "/metadata/uid", "value": config["nodeUID"]},
        {"op": "test", "path": "/metadata/resourceVersion", "value": metadata["resourceVersion"]},
        {"op": "test", "path": "/metadata/labels/" + pointer("brewlet.sh/owner-uid"),
         "value": config["profileUID"]},
        {"op": "test", "path": "/metadata/labels/" + pointer("brewlet.sh/owner-node-uid"),
         "value": config["nodeUID"]},
        {"op": "test", "path": "/metadata/annotations/" + pointer("brewlet.sh/owner-name"),
         "value": config["profile"]},
        {"op": "add", "path": "/metadata/annotations/" + pointer(RELEASE_MARKER), "value": marker},
    ]


def release_teardown(profile, before, after, config):
    """Require genuine production teardown; this function never manufactures it."""
    require(profile["metadata"]["uid"] == config["profileUID"]
            and profile["metadata"].get("deletionTimestamp"),
            "release must follow actual profile deletion")
    retirement = profile.get("status", {}).get("retirement") or {}
    generation = retirement.get("generation")
    require(retirement.get("phase") == "Teardown"
            and isinstance(generation, int) and generation > 0
            and any(t.get("name") == config["node"] and t.get("uid") == config["nodeUID"]
                    for t in retirement.get("targets", [])),
            "release lacks genuine UID-bound durable Teardown")
    for node in (before, after):
        metadata = node["metadata"]
        require(metadata.get("annotations", {}).get("brewlet.sh/cleanup-complete")
                == config["profileUID"] + "/" + str(generation),
                "release lacks genuine production worker cleanup receipt")
        require(not any(k == "brewlet.sh/runtime" or k.startswith("brewlet.sh/runtime.")
                        for k in metadata.get("labels", {})), "release retained runtime capabilities")
        require(metadata.get("annotations", {}).get("brewlet.sh/runtime-images", "") in ("", "{}"),
                "release retained runtime inventory")
    return generation


class ReleaseAdmission:
    """Runs *inside* the owned TLS admission pod, with restricted API credentials."""

    def __init__(self, api, emit):
        self.api, self.emit = api, emit
        self.lock = threading.Lock()

    def decision(self, request, config):
        if not release_trigger(request, config):
            return True, "not the exact invocation claim-release UPDATE"
        require(request.get("dryRun") is not True, "dry-run cannot prove a real release CAS conflict")
        require(request.get("userInfo", {}).get("username") == config["operatorUsername"],
                "claim release was not issued by the real operator service account")
        before, after = request["oldObject"], request["object"]
        with self.lock:
            profile = self.api("GET", "/apis/" + API + "/nodeprofiles/" + config["profile"])
            retirement_generation = release_teardown(profile, before, after, config)
            path = "/api/v1/nodes/" + config["node"]
            current = self.api("GET", path)
            require(current["metadata"]["uid"] == config["nodeUID"], "live Node was replaced")
            require(current["metadata"].get("labels", {}).get("brewlet.sh/owner-uid")
                    == config["profileUID"]
                    and current["metadata"].get("labels", {}).get("brewlet.sh/owner-node-uid")
                    == config["nodeUID"]
                    and current["metadata"].get("annotations", {}).get("brewlet.sh/owner-name")
                    == config["profile"], "live Node claim is foreign, incomplete or already released")
            marker_raw = current["metadata"].get("annotations", {}).get(RELEASE_MARKER)
            incoming_rv = after["metadata"]["resourceVersion"]
            if marker_raw:
                marker = json.loads(marker_raw)
                require(marker["runID"] == config["runID"]
                        and marker["requestUID"] != request["uid"]
                        and marker["originalResourceVersion"] != incoming_rv
                        and marker.get("retirementGeneration") == retirement_generation,
                        "release retry reused the stale original request/Node version")
                conditions = [c for c in profile.get("status", {}).get("conditions", [])
                              if c.get("reason") == "CleanupBlocked"
                              and config["node"] in c.get("message", "")
                              and ("object has been modified" in c.get("message", "")
                                   or "Conflict" in c.get("message", ""))]
                require(bool(conditions), "no genuine controller/API Conflict checkpoint after injection")
                self.emit({"event": "release-retry-validated", "requestUID": request["uid"],
                           "nodeUID": config["nodeUID"], "profileUID": config["profileUID"],
                           "requestResourceVersion": incoming_rv, "marker": marker,
                           "conflictCheckpoint": conditions, "operator": config["operatorUsername"]})
                return True, "fresh UID/claim/Teardown revalidation after actual release conflict"
            if current["metadata"]["resourceVersion"] != incoming_rv:
                # An ordinary concurrent heartbeat already made this request stale.
                return True, "pre-existing API race; no injection performed"
            patch = release_patch(current, {**config, "generation": retirement_generation}, request["uid"])
            changed = self.api("PATCH", path, patch)
            require(changed["metadata"]["uid"] == config["nodeUID"]
                    and changed["metadata"]["resourceVersion"] != incoming_rv
                    and changed["metadata"]["annotations"][RELEASE_MARKER] == patch[-1]["value"],
                    "harmless CAS mutation was not actually committed by the API")
            self.emit({"event": "release-cas-injected", "requestUID": request["uid"],
                       "nodeUID": config["nodeUID"], "profileUID": config["profileUID"],
                       "requestResourceVersion": incoming_rv,
                       "patchedResourceVersion": changed["metadata"]["resourceVersion"],
                       "marker": json.loads(patch[-1]["value"]),
                       "operator": config["operatorUsername"]})
            return True, "harmless annotation committed; original release PUT must fail API CAS"


def _in_cluster_api(method, path, data=None):
    # This function is used only by the explicitly installed admission pod.
    directory = "/var/run/secrets/kubernetes.io/serviceaccount"
    host = os.environ["KUBERNETES_SERVICE_HOST"]
    if ":" in host:
        host = "[" + host + "]"
    url = "https://" + host + ":" + os.environ.get("KUBERNETES_SERVICE_PORT_HTTPS", "443") + path
    with open(directory + "/token", encoding="utf-8") as stream:
        token = stream.read().strip()
    headers = {"Authorization": "Bearer " + token}
    body = None
    if data is not None:
        body = json.dumps(data).encode()
        headers["Content-Type"] = "application/json-patch+json"
    request = urllib.request.Request(url, data=body, method=method, headers=headers)
    with urllib.request.urlopen(request, context=ssl.create_default_context(cafile=directory + "/ca.crt"),
                                timeout=5) as response:
        return json.load(response)


def _serve_release_admission():
    """Entrypoint of the copied module, never run during import or offline tests."""
    import interceptor

    engine = ReleaseAdmission(_in_cluster_api, lambda row: print(json.dumps(row), flush=True))

    def decision(request, config):
        try:
            return engine.decision(request, config)
        except Exception as exc:
            print(json.dumps({"event": "release-cas-error", "requestUID": request.get("uid"),
                              "error": str(exc)}), flush=True)
            return False, "release CAS instrumentation failed: " + str(exc)

    interceptor.decision = decision
    interceptor.serve()


class ControlEdges:
    def __init__(self, h):
        self.h = h
        self.namespace = h.cluster + "-control"
        self.ids = ["control-" + h.run_id + suffix for suffix in ("-a", "-b")]
        require(not set(self.ids).intersection(IDS.values()), "base inventory ID collision")
        require(self.namespace not in (h.namespace, h.control_namespace),
                "control namespace collides with base workloads")
        self.workloads = {}
        self.old = {}
        self.original_values = None
        self.enabled = False
        self.pacing_samples = []
        h.result.setdefault("controlAcceptance", {})

    def get_workload(self, name):
        original = self.workloads[name]
        obj = self.h.get(original["kind"], name, self.namespace)
        require_owned(obj, self.h.run_id, original["metadata"]["uid"])
        return obj

    def pods(self, name):
        return owned_pods(self.get_workload(name),
                          self.h.get("pods", namespace=self.namespace)["items"],
                          self.h.get("replicasets", namespace=self.namespace)["items"],
                          self.h.run_id)

    def apply_workload(self, obj):
        require(obj["metadata"]["namespace"] == self.namespace, "foreign workload namespace")
        name = obj["metadata"]["name"]
        if name in self.workloads:
            require_owned(self.get_workload(name), self.h.run_id,
                          self.workloads[name]["metadata"]["uid"])
        self.workloads[name] = self.h.apply(obj)
        return self.workloads[name]

    def patch_workload(self, name, patch):
        require("status" not in patch, "acceptance must not write workload status")
        original = self.get_workload(name)
        for attempt in range(6):
            obj = original if attempt == 0 else self.get_workload(name)
            require(obj["metadata"]["uid"] == original["metadata"]["uid"]
                    and obj["metadata"].get("generation") == original["metadata"].get("generation")
                    and obj["spec"] == original["spec"],
                    "workload identity or spec changed during conditional patch")
            for field in ("annotations", "labels"):
                for key in patch.get("metadata", {}).get(field, {}):
                    require(obj["metadata"].get(field, {}).get(key)
                            == original["metadata"].get(field, {}).get(key),
                            "targeted workload metadata changed during conditional patch")
            candidate = copy.deepcopy(patch)
            candidate.setdefault("metadata", {}).update(uid=obj["metadata"]["uid"],
                                                        resourceVersion=obj["metadata"]["resourceVersion"])
            response = self.h.kubectl("-n", self.namespace, "patch", obj["kind"], name,
                                      "--type=merge", "-p", json.dumps(candidate), check=False)
            if response.returncode == 0:
                return response
            require("(Conflict)" in response.stderr,
                    "conditional workload patch failed: " + response.stderr[-2000:])
            if attempt < 5:
                time.sleep(0.2)
        raise Failure("conditional workload patch repeatedly conflicted; no ownership bypass")

    def patch_inventory_spec(self, obj, spec):
        kind, name = obj["kind"], obj["metadata"]["name"]
        require(kind in ("NodeProfile", "RuntimeImage"), "not an inventory spec")
        key = (kind.lower(), "", name)
        require(key in self.h.owned, "cannot edit unrecorded inventory")
        require_owned(obj, self.h.run_id, self.h.owned[key])
        require((kind == "NodeProfile" and name == self.h.profile)
                or (kind == "RuntimeImage" and name in self.ids),
                "control scenario may not edit base runtime inventory")
        # Do not replay status, claims, finalizers, or receipts from a GET.
        patch = inventory_spec_patch(obj, self.h.run_id, self.h.owned[key], spec)
        self.h.kubectl("patch", kind, name, "--type=json", "-p", json.dumps(patch))

    def workload(self, name, kind, runtime_id, *, opted=True, strategy=None, bound=False,
                 gated=False):
        template = copy.deepcopy(self.h.pod("unused", "python", bound=bound))
        template["metadata"] = {"labels": {OWNER: self.h.run_id, CASE: name},
                                "annotations": {RUNTIME: runtime_id}}
        if not opted:
            # Template-only annotation is intentionally not the object opt-in.
            template["metadata"]["annotations"][OPT_IN] = "true"
        template["spec"]["restartPolicy"] = "Always"
        template["spec"]["terminationGracePeriodSeconds"] = 2
        if kind == "DaemonSet":
            template["spec"].setdefault("tolerations", []).extend([
                {"key": "node-role.kubernetes.io/control-plane", "operator": "Exists",
                 "effect": "NoSchedule"}])
        if gated:
            template["spec"]["containers"][0]["readinessProbe"] = {
                "exec": {"command": ["python3", "-c",
                                    "import os; raise SystemExit("
                                    "0 if os.environ['BREWLET_RUNTIME_REVISION']=='before' else 1)"]},
                "periodSeconds": 1, "timeoutSeconds": 3, "failureThreshold": 1}
        obj = {"apiVersion": "apps/v1", "kind": kind,
               "metadata": {"name": name, "namespace": self.namespace,
                            "annotations": {OPT_IN: "true" if opted else "false"}},
               "spec": {"selector": {"matchLabels": {CASE: name}},
                        "template": {"metadata": template["metadata"], "spec": template["spec"]}}}
        if kind == "Deployment":
            obj["spec"].update(replicas=1, progressDeadlineSeconds=120,
                               strategy={"type": "RollingUpdate",
                                         "rollingUpdate": {"maxSurge": 1, "maxUnavailable": 0}})
        elif kind == "StatefulSet":
            obj["spec"].update(replicas=1, serviceName="control-headless",
                               updateStrategy=strategy or {"type": "RollingUpdate"})
        else:
            obj["spec"]["updateStrategy"] = strategy or {
                "type": "RollingUpdate", "rollingUpdate": {"maxUnavailable": 1}}
        return obj

    def wait_ready(self, name):
        def observed():
            obj, pods = self.get_workload(name), self.pods(name)
            self.h.result["controlAcceptance"].setdefault("workloadDiagnostics", {})[name] = {
                "uid": obj["metadata"]["uid"], "generation": obj["metadata"].get("generation"),
                "status": copy.deepcopy(obj.get("status", {})),
                "pods": [{"uid": p["metadata"]["uid"], "phase": p.get("status", {}).get("phase"),
                          "ready": pod_ready(p)} for p in pods]}
            pods = [p for p in pods if p.get("status", {}).get("phase")
                    not in ("Failed", "Succeeded")]
            return pods if native_complete(obj) and len(pods) == 1 and pod_ready(pods[0]) else False
        return self.h.wait("actual native controller ready " + name, observed)

    def image(self, runtime_id):
        image = self.h.get("runtimeimage", runtime_id)
        require_owned(image, self.h.run_id,
                      self.h.owned[("runtimeimage", "", runtime_id)])
        return image

    def complete_rotation(self, runtime_id):
        def observed():
            image = self.image(runtime_id)
            status = image.get("status", {})
            self.h.result["controlAcceptance"].setdefault("rotationDiagnostics", {})[runtime_id] = {
                "inventoryUID": image["metadata"]["uid"],
                "objectGeneration": image["metadata"].get("generation"),
                "status": {key: copy.deepcopy(status[key]) for key in (
                    "generation", "digest", "rotation", "rotationCompletedAt", "conditions")
                    if key in status}}
            return image if (status.get("rotationCompletedAt")
                             and status.get("rotation", {}).get("generation") == status.get("generation")
                             and any(c["type"] == "RotationComplete" and c["status"] == "True"
                                     for c in status.get("conditions", []))) else False
        try:
            return self.h.wait("controller RotationComplete " + runtime_id, observed)
        except Failure:
            try:
                self.capture_provisioning_diagnostics()
            except Exception as diagnostic_error:
                self.h.result["controlAcceptance"]["diagnosticFailure"] = str(diagnostic_error)
            raise

    def capture_provisioning_diagnostics(self):
        node = self.h.node_snapshot()
        profile = self.h.get("nodeprofile", self.h.profile)
        metadata = node["metadata"]
        inventory = json.loads(metadata.get("annotations", {}).get("brewlet.sh/runtime-images", "{}"))
        bindings = json.loads(metadata.get("annotations", {}).get("brewlet.sh/runtime-bindings", "{}"))
        self.h.result["controlAcceptance"]["provisioningDiagnostics"] = {
            "nodeUID": metadata["uid"],
            "ownerUID": metadata.get("labels", {}).get("brewlet.sh/owner-uid"),
            "ownerNodeUID": metadata.get("labels", {}).get("brewlet.sh/owner-node-uid"),
            "startupFences": [copy.deepcopy(t) for t in node.get("spec", {}).get("taints", [])
                              if t["key"] == FENCE],
            "runtimeInventory": {rid: copy.deepcopy(inventory.get(rid)) for rid in self.ids},
            "bindingObservation": {
                **{key: copy.deepcopy(bindings.get(key)) for key in (
                    "nodeUID", "profileUID", "profileGeneration", "observedAt", "complete")},
                "bindings": [{key: copy.deepcopy(binding.get(key)) for key in (
                    "containerID", "containerdNamespace", "podUID", "podName", "podNamespace",
                    "containerName", "id", "inventoryUID", "generation", "digest", "state")}
                    for binding in bindings.get("bindings", []) if binding.get("id") in self.ids],
            },
            "profileUID": profile["metadata"]["uid"],
            "profileGeneration": profile["metadata"].get("generation"),
            "profileStatus": {key: copy.deepcopy(profile.get("status", {}).get(key))
                              for key in ("conditions", "provisioningGeneration", "targets")},
        }
    def rotate(self, runtime_id, revision):
        image = self.image(runtime_id)
        previous = image["status"]["generation"]
        image["spec"]["source"]["image"] = self.h.runtime_images["python"][revision]
        self.patch_inventory_spec(image, image["spec"])
        self.h.wait("real source allocates new target " + runtime_id,
                    lambda: self.image(runtime_id).get("status", {}).get("generation", 0) > previous)

    def capture_provisioner_errors(self):
        from delivery import redact
        profile = self.h.get("nodeprofile", self.h.profile)
        require_owned(profile, self.h.run_id,
                      self.h.owned[("nodeprofile", "", self.h.profile)])
        observations = []
        for pod in self.h.get("pods", namespace=self.h.control_namespace)["items"]:
            if (pod["metadata"].get("labels", {}).get("brewlet.sh/owner-uid")
                    != profile["metadata"]["uid"]
                    or pod.get("spec", {}).get("nodeName") != self.h.node):
                continue
            response = self.h.kubectl("-n", self.h.control_namespace, "logs",
                pod["metadata"]["name"], "-c", "provisioner", "--tail=100", check=False)
            observations.append({
                "podUID": pod["metadata"]["uid"], "phase": pod.get("status", {}).get("phase"),
                "logReadSucceeded": response.returncode == 0,
                "errors": [redact(line, self.h.delivery.password)[:2000]
                    for line in response.stdout.splitlines() if any(marker in line for marker in (
                        "provision-error:", "runtime binding observation unavailable:",
                        "staging reconciliation blocked:", "runtime collection blocked",
                        "layer cache collection blocked:"))][-20:]})
        self.h.result["controlAcceptance"]["provisionerErrors"] = observations

    def assert_base_intact(self):
        for language, initial in self.h.initial.items():
            obj = self.h.get("pod", language + "-old", self.h.namespace)
            require(pod_ready(obj) and pod_identity(obj) == pod_identity(initial["pod"]),
                    "control test replaced/restarted a base application")
            require(self.h.lease(obj) == initial["lease"], "control test changed a base runtime lease")

    def helm(self, arguments):
        return self.h.run(["helm", "--kubeconfig", self.h.kubeconfig,
                           "--kube-context", self.h.context, *arguments], timeout=240)

    def release_values(self):
        return json.loads(self.helm(["get", "values", self.h.cluster, "-n",
                                     self.h.control_namespace, "-o", "json"]).stdout) or {}

    def check_rbac(self, enabled):
        for resource in ("deployments.apps", "statefulsets.apps", "daemonsets.apps"):
            for verb in ("get", "list", "watch", "patch"):
                # Worker lifecycle already needs read-only DaemonSet access.
                allowed = enabled or (resource == "daemonsets.apps" and verb != "patch")
                result = self.h.kubectl("auth", "can-i", verb, resource, "-n", self.namespace,
                                       "--as=system:serviceaccount:" + self.h.control_namespace
                                       + ":brewlet-operator", check=False)
                require(result.stdout.strip() == ("yes" if allowed else "no"),
                        "actual restart workload RBAC does not match release opt-in: "
                        + verb + " " + resource)

    def enable(self, *, workload_deadline=30):
        require(type(workload_deadline) is int and 1 <= workload_deadline <= 600,
                "invalid real workload deadline")
        self.helm(["upgrade", self.h.cluster, ROOT / "kubernetes/charts/brewlet",
                   "-n", self.h.control_namespace, "--reuse-values", "--wait", "--timeout", "180s",
                   "--set", "operator.rotationRestarts.enabled=true",
                   "--set", "operator.rotationRestarts.maxConcurrent=1",
                   "--set-string", "operator.rotationRestarts.nodeProgressDeadline=10m",
                   "--set-string", "operator.rotationRestarts.workloadProgressDeadline="
                   + str(workload_deadline) + "s"])
        self.enabled = True
        values = self.release_values()["operator"]["rotationRestarts"]
        require(values["enabled"] is True and values["maxConcurrent"] == 1
                and values["workloadProgressDeadline"] == str(workload_deadline) + "s"
                and values["nodeProgressDeadline"] == "10m", "real chart settings not installed")
        self.check_rbac(True)

    def restore_release(self):
        if self.original_values is None or not self.enabled:
            return
        path = self.h.work / "control-original-values.json"
        path.write_text(json.dumps(self.original_values))
        self.helm(["upgrade", self.h.cluster, ROOT / "kubernetes/charts/brewlet",
                   "-n", self.h.control_namespace, "--reset-values", "-f", path,
                   "--wait", "--timeout", "180s"])
        require(self.release_values() == self.original_values, "release values were not restored")
        self.check_rbac(False)
        self.enabled = False

    def create_inventory(self):
        for runtime_id in self.ids:
            self.h.apply({"apiVersion": API, "kind": "RuntimeImage",
                          "metadata": {"name": runtime_id},
                          "spec": {"source": {"image": self.h.runtime_images["python"]["before"]},
                                   "user": {"uid": 65532, "gid": 65532}}})
            self.h.wait("approved dedicated runtime " + runtime_id, lambda rid=runtime_id:
                        self.image(rid).get("status", {}).get("generation", 0) > 0)
        # Obtain a genuine previous inventory epoch, without installing any roots.
        old_epoch = self.image(self.ids[1])["metadata"]["uid"]
        self.h.delete("runtimeimage", self.ids[1])
        self.h.wait("uninstalled inventory epoch deleted", lambda:
                    self.h.get("runtimeimage", self.ids[1], absent=True) is None)
        self.h.owned.pop(("runtimeimage", "", self.ids[1]))
        self.h.apply({"apiVersion": API, "kind": "RuntimeImage",
                      "metadata": {"name": self.ids[1]},
                      "spec": {"source": {"image": self.h.runtime_images["python"]["before"]},
                               "user": {"uid": 65532, "gid": 65532}}})
        require(self.image(self.ids[1])["metadata"]["uid"] != old_epoch,
                "API did not allocate a new inventory epoch")
        profile = self.h.get("nodeprofile", self.h.profile)
        require_owned(profile, self.h.run_id, self.h.owned[("nodeprofile", "", self.h.profile)])
        profile["spec"]["runtimeImages"].extend({"id": rid, "install": "Eager"} for rid in self.ids)
        self.patch_inventory_spec(profile, profile["spec"])
        for rid in self.ids:
            self.h.wait("dedicated root actually installed " + rid,
                        lambda rid=rid: self.h.advertised(rid, "ready"))
            self.complete_rotation(rid)
        return old_epoch

    def eager_readiness_failure(self):
        from activation import invalid_path_runtime
        h = self.h
        runtime_id = "eager-invalid-" + h.run_id
        language = h.probe_language
        layout = h.delivery.work / "eager-invalid-path"
        selected = invalid_path_runtime(h.delivery.work / (language + "-before-golden"), layout)
        source = h.delivery.publish(layout, selected, "delivery/eager-invalid-path")
        h.apply({"apiVersion": API, "kind": "RuntimeImage", "metadata": {"name": runtime_id},
                 "spec": {"source": {"image": source},
                          "user": {"uid": 65532, "gid": 65532}}})
        def approved():
            image = self.image(runtime_id)
            return image if image.get("status", {}).get("generation", 0) > 0 and any(
                c.get("type") == "Approved" and c.get("status") == "True"
                and c.get("observedGeneration") == image["metadata"]["generation"]
                for c in image.get("status", {}).get("conditions", [])) else False

        h.wait("approved deliberately invalid Eager runtime PATH", approved)
        profile = h.get("nodeprofile", h.profile)
        spec = copy.deepcopy(profile["spec"])
        spec["runtimeImages"].append({"id": runtime_id})
        self.patch_inventory_spec(profile, spec)

        def failed():
            path = STORE + "/" + runtime_id + "/authorization.json"
            observed = h.node_exec("cat", path, check=False)
            if observed.returncode:
                require("No such file or directory" in observed.stderr and path in observed.stderr,
                        "failed to read Eager authorization: " + observed.stderr[:2000])
                return False
            authorization = json.loads(observed.stdout)
            evidence = eager_failure_evidence(h.node_snapshot(), h.get("nodeprofile", h.profile),
                                             self.image(runtime_id), authorization)
            if not evidence:
                return False
            self.capture_provisioner_errors()
            if not any("unsafe runtime PATH component" in line
                       for entry in h.result["controlAcceptance"]["provisionerErrors"]
                       for line in entry["errors"]):
                return False
            require(h.node_exec("/bin/sh", "-ec",
                'test ! -e "$1" && test ! -L "$1"', "eager",
                STORE + "/" + runtime_id + "/current", check=False).returncode == 0,
                "invalid Eager root published a current pointer")
            return evidence

        evidence = h.wait("actual approved Eager root rejection retains effective readiness gate", failed)
        name = "eager-gated"
        self.apply_workload(self.workload(name, "Deployment", IDS[language], opted=False))

        def unscheduled():
            pods = self.pods(name)
            require(all(not p["spec"].get("nodeName") for p in pods),
                    "workload scheduled through the failed Eager readiness gate")
            return pods if pods and all(any(c.get("type") == "PodScheduled" and c.get("status") == "False"
                for c in p.get("status", {}).get("conditions", [])) for p in pods) else False

        pending = h.wait("real scheduler keeps a good-runtime workload Pending behind Eager failure",
                         unscheduled)
        self.assert_base_intact()
        profile = h.get("nodeprofile", h.profile)
        spec = copy.deepcopy(profile["spec"])
        spec["runtimeImages"] = [entry for entry in spec["runtimeImages"] if entry["id"] != runtime_id]
        self.patch_inventory_spec(profile, spec)
        recovered = self.wait_ready(name)
        require({p["metadata"]["uid"] for p in pending} == {p["metadata"]["uid"] for p in recovered},
                "Eager gate recovery replaced the unscheduled workload instead of releasing readiness")
        self.verify_running_generation(recovered, self.image(IDS[language]), "after")
        node = h.node_snapshot()
        require(node["metadata"].get("labels", {}).get("brewlet.sh/runtime") == "ready"
                and not any(t.get("key") == FENCE for t in node.get("spec", {}).get("taints", [])),
                "Eager gate recovery did not establish actual unfenced baseline readiness")
        self.assert_base_intact()
        self.delete_object("Deployment", name, self.namespace)
        h.wait("Eager readiness canary and descendant Pods deleted", lambda:
               h.get("deployment", name, self.namespace, absent=True) is None
               and not any(p["metadata"].get("labels", {}).get(CASE) == name
                           for p in h.get("pods", namespace=self.namespace)["items"]))
        self.workloads.pop(name)
        h.delete("runtimeimage", runtime_id)
        h.wait("invalid Eager inventory finalizes under actual worker cleanup", lambda:
               h.get("runtimeimage", runtime_id, absent=True) is None)
        h.record("approved Eager root failure blocks real readiness and scheduling until profile correction",
                 {"failure": evidence, "recoveredPodUIDs": [p["metadata"]["uid"] for p in recovered],
                  "invalidRootWasNeverPublished": True}, ["PROF-03"])

    def strategy_matrix(self):
        old_epoch = self.create_inventory()
        a, b = self.ids
        cases = [
            ("rolling-deployment", "Deployment", None, "Complete"),
            ("rolling-stateful", "StatefulSet", None, "Complete"),
            ("rolling-daemon", "DaemonSet", None, "Complete"),
            ("ondelete-stateful", "StatefulSet", {"type": "OnDelete"}, "NotApplicable"),
            ("ondelete-daemon", "DaemonSet", {"type": "OnDelete"}, "NotApplicable"),
            ("partitioned", "StatefulSet",
             {"type": "RollingUpdate", "rollingUpdate": {"partition": 1}}, "Deferred"),
            ("paused", "Deployment", None, "Deferred"),
            ("object-opt-out", "Deployment", None, None),
        ]
        for name, kind, strategy, _ in cases:
            self.apply_workload(self.workload(name, kind, a, strategy=strategy,
                                              opted=name != "object-opt-out"))
            self.old[name] = self.wait_ready(name)
        for name in ("paused",):
            self.patch_workload(name, {"spec": {"paused": True}})
        self.apply_workload(self.workload("old-epoch", "Deployment", b))
        self.old["old-epoch"] = self.wait_ready("old-epoch")
        self.patch_workload("old-epoch", {"spec": {"paused": True}})
        old_barrier = {"id": b, "inventoryUID": old_epoch, "generation": 1}
        self.patch_workload("old-epoch", {
            "spec": {"template": {"metadata": {"annotations": {BARRIER: json.dumps(old_barrier)}}}}})
        self.rotate(a, "after")
        self.rotate(b, "after")
        self.complete_rotation(a)
        self.complete_rotation(b)
        # Disabled operator must not patch or replace old pods despite object opt-in.
        for name, _, _, _ in cases:
            require(template_barrier(self.get_workload(name)) is None,
                    "disabled operator patched a workload")
            require({pod_identity(p) for p in self.pods(name)}
                    == {pod_identity(p) for p in self.old[name]}, "disabled restart replaced pods")
        self.h.record("real release default denies workload patch and leaves opted-in old pods intact",
                      {"namespace": self.namespace, "runtimeIDs": self.ids},
                      ["ROT-01", "ROT-06"])
        self.enable()
        evidence = []
        for name, _, _, expected in cases:
            if expected is None:
                continue
            rid = a
            def outcome(name=name, rid=rid, expected=expected):
                image, workload = self.image(rid), self.get_workload(name)
                row = restart_record(image, workload)
                self.h.result["controlAcceptance"].setdefault("workloadDiagnostics", {})[name] = {
                    "uid": workload["metadata"]["uid"],
                    "generation": workload["metadata"].get("generation"), "expectedPhase": expected,
                    "status": copy.deepcopy(workload.get("status", {})), "restart": copy.deepcopy(row)}
                if not row or row["phase"] != expected:
                    return False
                after = self.pods(name)
                if expected == "Complete" and (not native_complete(workload)
                                               or not after or not all(pod_ready(p) for p in after)):
                    return False
                verify_restart(image, workload, self.old[name], after, row, expected=expected)
                return image, workload, after, row
            image, workload, after, row = self.h.wait("native restart outcome " + name, outcome)
            original = self.workloads[name]["spec"]
            strategy_key = "strategy" if workload["kind"] == "Deployment" else "updateStrategy"
            require(workload["spec"].get(strategy_key) == original.get(strategy_key),
                    "operator rewrote native controller strategy")
            if expected == "Complete":
                self.verify_running_generation(after, image, "after")
            else:
                for pod in after:
                    require(self.h.lease(pod)["runtime"]["generation"]
                            < image["status"]["generation"], "retained pod unexpectedly changed runtime")
            evidence.append({"name": name, "outcome": row, "podUIDs":
                             [p["metadata"]["uid"] for p in after]})
        self.h.wait("explicit old-epoch refusal", lambda:
                    (restart_record(self.image(b), self.get_workload("old-epoch")) or {}).get("phase")
                    == "EpochMismatch")
        epoch_row = restart_record(self.image(b), self.get_workload("old-epoch"))
        verify_restart(self.image(b), self.get_workload("old-epoch"),
                       self.old["old-epoch"], self.pods("old-epoch"), epoch_row,
                       expected="EpochMismatch")
        require(template_barrier(self.get_workload("old-epoch")) == old_barrier,
                "operator silently replaced a real previous-epoch barrier")
        optout = self.get_workload("object-opt-out")
        require(template_barrier(optout) is None
                and {pod_identity(p) for p in self.pods("object-opt-out")}
                == {pod_identity(p) for p in self.old["object-opt-out"]},
                "template-only opt-in restarted an object-opted-out workload")
        self.apply_workload(self.workload("born-after-completion", "Deployment", a))
        self.wait_ready("born-after-completion")
        completion = self.image(a)["status"]["rotationCompletedAt"]
        require(all(timestamp(p["status"]["startTime"]) >= timestamp(completion)
                    for p in self.pods("born-after-completion")),
                "new-vs-old assertion lacks genuinely post-completion pods")
        snapshots = {name: (self.get_workload(name)["metadata"]["generation"],
                            template_barrier(self.get_workload(name)))
                     for name in self.workloads}
        for _ in range(4):
            time.sleep(2)
            for name, (generation, barrier) in snapshots.items():
                obj = self.get_workload(name)
                require(obj["metadata"]["generation"] == generation
                        and template_barrier(obj) == barrier,
                        "same UID/epoch/target was repeatedly patched")
        require(template_barrier(self.get_workload("born-after-completion")) is None,
                "workload born after completion was unnecessarily restarted")
        old_uid = self.get_workload("born-after-completion")["metadata"]["uid"]
        self.delete_object("Deployment", "born-after-completion", self.namespace)
        self.h.wait("original controller and descendant pods deleted before UID-recreation test",
                    lambda: self.h.get("deployment", "born-after-completion",
                                       self.namespace, absent=True) is None
                    and not any(p["metadata"].get("labels", {}).get(CASE) == "born-after-completion"
                                for p in self.h.get("pods", namespace=self.namespace)["items"]))
        self.h.owned.pop(("deployment", self.namespace, "born-after-completion"))
        self.workloads.pop("born-after-completion")
        self.apply_workload(self.workload("born-after-completion", "Deployment", a))
        recreated = self.wait_ready("born-after-completion")
        replacement = self.get_workload("born-after-completion")
        require(replacement["metadata"]["uid"] != old_uid
                and all(timestamp(p["status"]["startTime"]) >= timestamp(completion) for p in recreated),
                "workload UID-recreation assertion lacks a genuine new controller/new pods")
        for _ in range(3):
            time.sleep(2)
            replacement = self.get_workload("born-after-completion")
            require(template_barrier(replacement) is None
                    and restart_record(self.image(a), replacement) is None,
                    "operator adopted a previous workload UID's restart intent")
        self.h.record("actual native strategy matrix, previous-epoch refusal and idempotent barriers",
                      {"workloads": evidence, "epochRefusal": epoch_row,
                       "completion": completion, "repeatObservations": 4,
                       "oldWorkloadUID": old_uid,
                       "recreatedWorkloadUID": replacement["metadata"]["uid"]},
                      ["ROT-01", "ROT-06", "ROT-07", "ROT-08"])

    def verify_running_generation(self, pods, image, revision):
        target = barrier_for(image)
        for pod in pods:
            require(pod_ready(pod), "runtime verification requires a real ready pod")
            lease = self.h.lease(pod)
            runtime = lease["runtime"]
            require(runtime["inventoryUID"] == target["inventoryUID"]
                    and runtime["generation"] >= target["generation"],
                    "native replacement did not acquire the approved runtime lease")
            actual = self.h.kubectl("-n", self.namespace, "exec", pod["metadata"]["name"],
                                    "-c", "app", "--", "python3", "-c",
                                    "import os; print(os.environ['BREWLET_RUNTIME_REVISION'])").stdout.strip()
            require(actual == revision, "native replacement is not actually executing rotated runtime")
            require(pod["spec"]["containers"][0]["image"] == self.h.app_images["python"],
                    "runtime rotation substituted application bytes")

    def superseded(self):
        a = self.ids[0]
        previous = self.image(a)["status"]["generation"]
        self.rotate(a, "before")
        self.complete_rotation(a)
        def observed():
            image = self.image(a)
            workload = self.get_workload("paused")
            rows = image.get("status", {}).get("restarts", {}).get("workloads", [])
            old = [r for r in rows if r.get("uid") == workload["metadata"]["uid"]
                   and r.get("generation") == previous
                   and r.get("inventoryUID") == image["metadata"]["uid"]]
            latest = restart_record(image, workload)
            return (old, latest) if (old and old[0]["phase"] == "Superseded"
                                     and old[0]["reason"] == "NewerRotation"
                                     and latest and latest["phase"] == "Deferred"
                                     and template_barrier(workload) == barrier_for(image)) else False
        old, latest = self.h.wait("new target supersedes deferred old target", observed)
        require({pod_identity(p) for p in self.pods("paused")}
                == {pod_identity(p) for p in self.old["paused"]},
                "superseding pending work bypassed paused strategy")
        self.h.record("new genuine rotation supersedes deferred target without resuming paused workload",
                      {"superseded": old, "latest": latest}, ["ROT-08"])
        for name in list(self.workloads):
            self.patch_workload(name, {"metadata": {"annotations": {OPT_IN: "false"}}})

    def pacing(self):
        # Both targets start on 'before'; both replacement probes refuse 'after'.
        deadline = 2 * DEADLINE
        self.restore_release()
        for index, rid in enumerate(self.ids):
            if self.image(rid)["spec"]["source"]["image"] != self.h.runtime_images["python"]["before"]:
                self.rotate(rid, "before")
                self.complete_rotation(rid)
            self.h.wait("actual warm unfenced capacity before pacing " + rid, lambda rid=rid:
                self.h.advertised(rid, "ready", self.image(rid)["status"]["generation"])
                and not any(t["key"] == FENCE for t in
                            self.h.node_snapshot().get("spec", {}).get("taints", [])))
            name = "pace-" + str(index)
            self.apply_workload(self.workload(name, "Deployment", rid, gated=True))
            self.wait_ready(name)
        for rid in self.ids:
            self.rotate(rid, "after")
        for rid in self.ids:
            self.complete_rotation(rid)
        self.enable(workload_deadline=deadline)
        saw_blocked_pending = False
        started = time.monotonic()
        final = None
        while time.monotonic() - started < self.h.timeout:
            all_images = self.h.get("runtimeimages")["items"]
            by_name = {im["metadata"]["name"]: im for im in all_images}
            images = [by_name[rid] for rid in self.ids]
            active = verify_pacing(all_images, 1, datetime.now(timezone.utc).timestamp(), deadline)
            rows = [restart_record(im, self.get_workload("pace-" + str(i)))
                    for i, im in enumerate(images)]
            sample = {"observedAt": datetime.now(timezone.utc).isoformat(),
                      "activeUIDs": [r["uid"] for r in active], "records": copy.deepcopy(rows)}
            self.pacing_samples.append(sample)
            self.h.result["controlAcceptance"]["pacingDiagnostics"] = {
                "sampleCount": len(self.pacing_samples), "sawBlockedPending": saw_blocked_pending,
                "recent": [{"observedAt": s["observedAt"], "activeUIDs": s["activeUIDs"],
                    "records": [{key: row[key] for key in (
                        "uid", "phase", "reason", "inventoryUID", "generation", "startedAt", "completedAt")
                        if key in row} if row else None for row in s["records"]]}
                    for s in self.pacing_samples[-10:]]}
            (self.h.work / "control-pacing.json").write_text(json.dumps(self.pacing_samples, indent=2))
            if (len(active) == 1 and active[0].get("namespace") == self.namespace
                    and active[0]["name"] in self.workloads
                    and any(r and r["phase"] == "Pending" for r in rows)):
                running_name = active[0]["name"]
                pods = self.pods(running_name)
                if any(p.get("metadata", {}).get("annotations", {}).get(BARRIER)
                       and not pod_ready(p) and p.get("status", {}).get("phase") == "Running"
                       for p in pods):
                    saw_blocked_pending = True
            if all(r and r["phase"] == "Failed" for r in rows):
                final = rows
                break
            require(not any(r and r["phase"] == "Complete" for r in rows),
                    "unready application rollout was reported complete")
            time.sleep(1)
        require(final is not None and saw_blocked_pending,
                "did not prove a real globally queued rollout and deadline slot handoff")
        ordered = sorted(final, key=lambda r: timestamp(r["startedAt"]))
        require(timestamp(ordered[1]["startedAt"]) - timestamp(ordered[0]["startedAt"])
                >= deadline - 1, "second global slot started before first deadline expired")
        for i, row in enumerate(final):
            require(row["reason"] == "RestartFailed"
                    and "progress deadline" in row.get("message", "").lower()
                    and not row.get("completedAt"), "timeout was counted as completed restart")
            self.patch_workload("pace-" + str(i), {"metadata": {"annotations": {OPT_IN: "false"}}})
        self.h.record("cluster-wide maxConcurrent=1 across two runtime IDs; real unready rollouts time out",
                      {"deadlineSeconds": deadline, "sampleCount": len(self.pacing_samples),
                       "final": final, "notAContinuousAPIAudit": True},
                      ["ROT-06", "ROT-09"])

    def conditional_node_update(self, patch):
        node = self.h.node_snapshot()
        require(node["metadata"]["uid"] == self.h.node_uid, "live Node identity changed")
        patch = copy.deepcopy(patch)
        patch.setdefault("metadata", {}).update(uid=self.h.node_uid,
                                                resourceVersion=node["metadata"]["resourceVersion"])
        return self.h.kubectl("patch", "node", self.h.node, "--type=merge", "-p", json.dumps(patch))

    def excluded_bound(self):
        rid = self.ids[0]
        self.apply_workload(self.workload("bound-excluded", "Deployment", rid, bound=True))
        self.old["bound-excluded"] = self.wait_ready("bound-excluded")
        require(not self.h.node_snapshot().get("spec", {}).get("unschedulable"),
                "excluded-node scenario requires originally schedulable owned Node")
        self.conditional_node_update({"spec": {"unschedulable": True}})
        try:
            self.rotate(rid, "before")
            image = self.complete_rotation(rid)
            excluded = image["status"]["rotation"].get("excludedNodes", [])
            require(any(n["name"] == self.h.node and n["uid"] == self.h.node_uid for n in excluded),
                    "real cordoned node missing from exclusion diagnostics")
            def deferred():
                im, workload = self.image(rid), self.get_workload("bound-excluded")
                row = restart_record(im, workload)
                if not row or row["phase"] != "Deferred":
                    return False
                diagnostic = blocked_pod_evidence(row, self.pods("bound-excluded"),
                                                  im["status"]["generation"],
                                                  im["status"]["rotation"].get("excludedNodes", []))
                return row, diagnostic
            row, diagnostic = self.h.wait("real bound replacement blocked/deferred on excluded Node",
                                          deferred)
            require(not row.get("completedAt"), "excluded-node timeout claimed completed restart")
            self.h.record("already-bound native replacement defers with actual Pod UID/node/minimum",
                          {"record": row, "blockedPod": diagnostic, "excludedNodes": excluded},
                          ["ROT-05", "ROT-09"])
        finally:
            self.conditional_node_update({"spec": {"unschedulable": False}})
        self.h.wait("excluded node returns to latest validated target",
                    lambda: self.h.advertised(rid, "ready", self.image(rid)["status"]["generation"]))
        recovered = self.wait_ready("bound-excluded")
        self.h.wait("deferred rollout eventually observed complete without repatching", lambda:
                    (restart_record(self.image(rid), self.get_workload("bound-excluded")) or {})
                    .get("phase") == "Complete")
        self.verify_running_generation(recovered, self.image(rid), "before")
        self.patch_workload("bound-excluded", {"metadata": {"annotations": {OPT_IN: "false"}}})

    def delete_object(self, kind, name, namespace=None):
        key = (kind.lower(), namespace or "", name)
        require(key in self.h.owned, "cannot delete an unrecorded control object")
        require(namespace == self.namespace or (kind == "Node" and name.startswith(
            "control-cas-" + self.h.run_id + "-")), "foreign control deletion")
        resources = {"Deployment": "deployments", "StatefulSet": "statefulsets",
                     "DaemonSet": "daemonsets", "Service": "services", "Node": "nodes",
                     "Namespace": "namespaces"}
        prefix = "/apis/apps/v1" if kind in ("Deployment", "StatefulSet", "DaemonSet") else "/api/v1"
        if namespace:
            prefix += "/namespaces/" + namespace
        def attempt():
            obj = self.h.get(kind, name, namespace, absent=True)
            if obj is None:
                return True
            require_owned(obj, self.h.run_id, self.h.owned[key])
            result = self.h.kubectl(
                "delete", "--raw", prefix + "/" + resources[kind] + "/" + name, "-f", "-",
                input=json.dumps({"apiVersion": "v1", "kind": "DeleteOptions",
                                  "preconditions": {"uid": obj["metadata"]["uid"],
                                                    "resourceVersion": obj["metadata"]["resourceVersion"]},
                                  "propagationPolicy": "Foreground"}), check=False)
            if result.returncode != 0:
                require("Conflict" in result.stderr,
                        "owned control deletion failed: " + result.stderr)
                return False
            return True

        self.h.wait("UID/version-guarded control deletion " + name, attempt, timeout=30)

    def api_cas(self):
        name = "control-cas-" + self.h.run_id + "-node"
        obj = self.h.apply({"apiVersion": "v1", "kind": "Node",
                            "metadata": {"name": name, "annotations": {PROBE: "original"}},
                            "spec": {"unschedulable": True}})
        uid = obj["metadata"]["uid"]
        def advance():
            current = self.h.get("node", name)
            path, stale = cas_request(current, self.h.run_id, uid, self.h.node, "stale")
            require(current["metadata"]["annotations"].get(PROBE) == "original",
                    "CAS setup annotation changed unexpectedly")
            _, changed = cas_request(current, self.h.run_id, uid, self.h.node, "changed")
            result = self.h.kubectl("replace", "--raw", path, "-f", "-",
                                    input=json.dumps(changed), check=False)
            if result.returncode != 0:
                require("Conflict" in result.stderr,
                        "CAS setup failed for unrelated reason: " + result.stderr)
                return False
            return path, stale

        # Node lifecycle controllers can update a newly created Node immediately.
        path, stale = self.h.wait("owned fixture Node CAS setup", advance, timeout=30)
        conflict = self.h.kubectl("replace", "--raw", path, "-f", "-",
                                  input=json.dumps(stale), check=False)
        require(conflict.returncode != 0 and "Conflict" in conflict.stderr,
                "real API accepted stale resourceVersion or failed for unrelated reason")
        require(self.h.get("node", name)["metadata"]["annotations"][PROBE] == "changed",
                "failed CAS changed the server's current state")
        self.delete_object("Node", name)
        self.h.wait("owned fixture Node deleted", lambda: self.h.get("node", name, absent=True) is None)
        self.h.owned.pop(("node", "", name))
        replacement = self.h.apply({"apiVersion": "v1", "kind": "Node",
                                    "metadata": {"name": name, "annotations": {PROBE: "replacement"}},
                                    "spec": {"unschedulable": True}})
        require(replacement["metadata"]["uid"] != uid, "fixture Node was not really recreated")
        # Use current RV but the old UID: a conflict alone would not prove UID fencing.
        stale["metadata"]["resourceVersion"] = replacement["metadata"]["resourceVersion"]
        refusal = self.h.kubectl("replace", "--raw", path, "-f", "-",
                                 input=json.dumps(stale), check=False)
        require(refusal.returncode != 0 and "uid" in refusal.stderr.lower()
                and ("precondition" in refusal.stderr.lower() or "match" in refusal.stderr.lower()),
                "recreated Node refusal does not establish actual UID precondition enforcement")
        observed = self.h.get("node", name)
        require(observed["metadata"]["uid"] == replacement["metadata"]["uid"]
                and observed["metadata"]["annotations"][PROBE] == "replacement",
                "stale request modified the recreated fixture Node")
        self.delete_object("Node", name)
        self.h.wait("replacement fixture Node deleted",
                    lambda: self.h.get("node", name, absent=True) is None)
        self.h.owned.pop(("node", "", name))
        self.h.record("real API rejects stale resourceVersion and old UID on recreated fixture Node",
                      {"oldUID": uid, "replacementUID": replacement["metadata"]["uid"],
                       "conflict": conflict.stderr, "UIDRefusal": refusal.stderr,
                       "scope": "API-primitives-only", "controllerClaimReleaseVerified": False,
                       "physicalCleanupVerified": False}, ["API-CAS"])

    def clean_workloads(self):
        for name in list(self.workloads):
            self.delete_object(self.workloads[name]["kind"], name, self.namespace)
        self.h.wait("control controllers and their actual pods drain",
                    lambda: not self.h.get("pods", namespace=self.namespace)["items"])
        for name, obj in self.workloads.items():
            self.h.wait("control object deletion " + name, lambda name=name, obj=obj:
                        self.h.get(obj["kind"], name, self.namespace, absent=True) is None)

    def cleanup(self):
        self.clean_workloads()
        # Keep IDs offered until the authorized worker has genuinely revoked
        # and cleaned their roots. Dropping placement first would orphan the
        # per-ID revocation work rather than demonstrate production cleanup.
        for rid in self.ids:
            self.h.delete("runtimeimage", rid)
            self.h.wait("dedicated inventory production revocation/cleanup " + rid, lambda rid=rid:
                        self.h.get("runtimeimage", rid, absent=True) is None)
        profile = self.h.get("nodeprofile", self.h.profile)
        require_owned(profile, self.h.run_id, self.h.owned[("nodeprofile", "", self.h.profile)])
        profile["spec"]["runtimeImages"] = [e for e in profile["spec"]["runtimeImages"]
                                            if e["id"] not in self.ids]
        self.patch_inventory_spec(profile, profile["spec"])
        self.h.wait("base-only profile authority reconciled after dedicated ID cleanup", lambda:
                    self.h.get("nodeprofile", self.h.profile).get("status", {}).get("provisioningGeneration")
                    == self.h.get("nodeprofile", self.h.profile)["metadata"]["generation"])
        self.h.delete("namespace", self.namespace)
        self.h.wait("dedicated control namespace deleted",
                    lambda: self.h.get("namespace", self.namespace, absent=True) is None)
        self.restore_release()
        self.assert_base_intact()


class ProviderPins:
    """A second, uniquely named instance of the harness's real TLS interceptor.

    It does not consume or monkeypatch h.install_interceptor, so the subsequent
    base pinned_revocation callback can install its own independent interceptor.
    """

    def __init__(self, control):
        self.c, self.h = control, control.h
        self.name = "control-interceptor"
        self.revision = 0
        self.audit = []

    def install(self):
        h = self.h
        directory = h.work / self.name
        directory.mkdir()
        shutil.copy2(HERE / "interceptor.py", directory / "interceptor.py")
        base = f"registry.brewlet.test/{h.run_id}/runtime/python:base"
        dockerfile = directory / "Dockerfile"
        dockerfile.write_text(f"FROM {base}\nCOPY interceptor.py /interceptor.py\n"
                              'ENTRYPOINT ["python3", "/interceptor.py"]\n')
        tag = f"registry.brewlet.test/{h.run_id}/control-interceptor:fixture"
        h.build(tag, dockerfile, directory)
        h.load_image(tag)
        dns = self.name + "." + self.c.namespace + ".svc"
        h.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
               "-subj", "/CN=" + dns, "-addext", "subjectAltName=DNS:" + dns,
               "-keyout", directory / "tls.key", "-out", directory / "tls.crt"])
        os.chmod(directory / "tls.key", 0o600)
        certificate = base64.b64encode((directory / "tls.crt").read_bytes()).decode()
        h.apply({"apiVersion": "v1", "kind": "Secret",
                 "metadata": {"name": self.name + "-tls", "namespace": self.c.namespace},
                 "type": "kubernetes.io/tls",
                 "data": {"tls.crt": certificate,
                          "tls.key": base64.b64encode((directory / "tls.key").read_bytes()).decode()}})
        self.configure([], wait=False)
        h.apply({"apiVersion": "v1", "kind": "Service",
                 "metadata": {"name": self.name, "namespace": self.c.namespace},
                 "spec": {"selector": {CASE: self.name},
                          "ports": [{"port": 443, "targetPort": 9443}]}})
        h.apply({"apiVersion": "apps/v1", "kind": "Deployment",
                 "metadata": {"name": self.name, "namespace": self.c.namespace},
                 "spec": {"replicas": 1, "selector": {"matchLabels": {CASE: self.name}},
                          "template": {"metadata": {"labels": {OWNER: h.run_id, CASE: self.name}},
                                       "spec": {"containers": [{
                                           "name": "interceptor", "image": tag, "imagePullPolicy": "Never",
                                           "env": [{"name": "CONFIG", "value": "/config/config.json"}],
                                           "readinessProbe": {"httpGet": {"scheme": "HTTPS",
                                                                        "path": "/ready", "port": 9443}},
                                           "volumeMounts": [{"name": "tls", "mountPath": "/certs"},
                                                            {"name": "config", "mountPath": "/config"}]}],
                                                "volumes": [
                                                    {"name": "tls", "secret": {
                                                        "secretName": self.name + "-tls"}},
                                                    {"name": "config", "secret": {
                                                        "secretName": self.name + "-config"}}]}}}})
        h.kubectl("-n", self.c.namespace, "rollout", "status",
                  "deployment/" + self.name, "--timeout=120s", timeout=150)
        h.apply({"apiVersion": "admissionregistration.k8s.io/v1",
                 "kind": "ValidatingWebhookConfiguration",
                 "metadata": {"name": h.cluster + "-control-pins"},
                 "webhooks": [{"name": "control-pins.acceptance.brewlet.test",
                               "admissionReviewVersions": ["v1"], "sideEffects": "None",
                               "failurePolicy": "Fail", "timeoutSeconds": 5,
                               "clientConfig": {"caBundle": certificate, "service": {
                                   "name": self.name, "namespace": self.c.namespace, "path": "/"}},
                               "rules": [{"operations": ["UPDATE"], "apiGroups": [""],
                                          "apiVersions": ["v1"], "resources": ["nodes"]}]}]})
        self.configure([])

    def configure(self, labels, *, wait=True, initial_values=None, pool_owner=None):
        self.revision += 1
        config = {"revision": self.revision, "node": self.h.node, "nodeUID": self.h.node_uid,
                  "pinnedLabels": labels, "denyFence": False}
        if initial_values is not None:
            require(bool(pool_owner) and set(initial_values) <= set(labels)
                    and all(value in ("ready", "available") for value in initial_values.values()),
                    "initial provider labels require an exact pool owner and pinned values")
            config.update(initialPinnedLabels=initial_values, poolOwner=pool_owner)
        self.h.apply({"apiVersion": "v1", "kind": "Secret",
                      "metadata": {"name": self.name + "-config", "namespace": self.c.namespace},
                      "stringData": {"config.json": json.dumps(config)}})
        if wait:
            self.h.wait("TLS interceptor observes pin configuration revision", lambda:
                        self.h.kubectl("-n", self.c.namespace, "exec", "deployment/" + self.name,
                                       "--", "python3", "-c",
                                       "import json; print(json.load(open('/config/config.json'))"
                                       "['revision'])").stdout.strip() == str(self.revision), timeout=120)

    def refused(self, key):
        response = self.h.kubectl("-n", self.c.namespace, "logs", "deployment/" + self.name,
                                  "--tail=2000").stdout
        for line in response.splitlines():
            try:
                row = json.loads(line)
            except ValueError:
                continue
            if (row.get("revision") == self.revision and row.get("allowed") is False
                    and row.get("requestUID")
                    and row.get("message") == "provider-pinned label refusal: " + key):
                self.audit.append(row)
                return row
        return False

    def fenced(self):
        return any(t["key"] == FENCE and t.get("value") == "provisioning"
                   and t["effect"] == "NoSchedule"
                   for t in self.h.node_snapshot().get("spec", {}).get("taints", []))

    def diagnostics(self):
        profile = self.h.get("nodeprofile", self.h.profile)
        profile_uid = require_owned(
            profile, self.h.run_id, self.h.owned[("nodeprofile", "", self.h.profile)])
        outputs = [json.dumps(profile.get("status", {})), json.dumps(
            self.h.node_snapshot()["metadata"].get("annotations", {}))]
        for pod in self.h.get("pods", namespace=self.h.control_namespace)["items"]:
            labels = pod["metadata"].get("labels", {})
            if (labels.get("brewlet.sh/owner-uid") != profile_uid
                    or labels.get("brewlet.sh/mode") != "provisioning"
                    or pod.get("spec", {}).get("nodeName") != self.h.node):
                continue
            containers = [v for v in pod["spec"]["containers"] if v["name"] == "provisioner"]
            require(len(containers) == 1, "worker diagnostics lack a single real provisioner")
            env = {v["name"]: v.get("value") for v in containers[0].get("env", [])}
            require(env.get("BREWLET_PROFILE_UID") == profile_uid,
                    "worker diagnostic profile identity mismatch")
            if not pod.get("status", {}).get("containerStatuses"):
                continue
            outputs.append(self.h.kubectl("-n", self.h.control_namespace, "logs",
                                          pod["metadata"]["name"], "-c", "provisioner",
                                          "--tail=500").stdout)
        return "\n".join(outputs)

    def exercise(self):
        c, h = self.c, self.h
        rid = c.ids[0]
        key, stale = "brewlet.sh/runtime." + rid, "brewlet.sh/runtime.control-stale-" + h.run_id
        self.install()
        self.configure([key])
        profile = h.get("nodeprofile", h.profile)
        for entry in profile["spec"]["runtimeImages"]:
            if entry["id"] == rid:
                entry["install"] = "OnDemand"
        c.patch_inventory_spec(profile, profile["spec"])
        h.wait("actual OnDemand removal/value mutability probe is refused",
               lambda: self.refused(key), timeout=180)
        h.wait("unsupported OnDemand pin retains confirmed node-wide fence", self.fenced, timeout=180)
        h.wait("OnDemand pin has explicit advertisement failure",
               lambda: "node-advertisement-failed" in self.diagnostics(), timeout=180)
        require(h.node_snapshot()["metadata"]["labels"].get(key) == "ready",
                "provider pin evidence was silently normalized or removed")
        c.assert_base_intact()
        # Act as the fixture's pool owner, introducing an explicitly stale label.
        c.conditional_node_update({"metadata": {"labels": {stale: "ready"}}})
        self.configure([key, stale])
        refusal = h.kubectl("patch", "node", h.node, "--type=merge", "-p", json.dumps({
            "metadata": {"uid": h.node_uid,
                         "resourceVersion": h.node_snapshot()["metadata"]["resourceVersion"],
                         "labels": {stale: None}}}), check=False)
        require(refusal.returncode != 0 and "provider-pinned label refusal: " + stale in refusal.stderr,
                "stale key is only a fixture label, not a real API refusal")
        h.wait("actual stale capability removal refusal audited", lambda: self.refused(stale), timeout=60)
        # Correct the OnDemand pin, but leave the stale pool configuration broken.
        self.configure([stale])
        for _ in range(4):
            time.sleep(2)
            require(self.fenced() and h.node_snapshot()["metadata"]["labels"].get(stale) == "ready",
                    "worker released node-wide fence with a stale retained capability")
        h.wait("stale capability prevents authorized recovery with explicit error",
               lambda: "node-advertisement-failed" in self.diagnostics()
               and any(word in self.diagnostics() for word in ("nonoffered", "stale", "obsolete")),
               timeout=180)
        c.assert_base_intact()
        # Pool correction is explicit; only the actual provisioner releases the fence.
        self.configure([])
        c.conditional_node_update({"metadata": {"labels": {stale: None}}})
        h.wait("all-ID authorized worker recovery releases startup fence",
               lambda: not self.fenced()
               and h.node_snapshot()["metadata"]["labels"].get("brewlet.sh/runtime") == "ready")
        for runtime_id in (*h.runtime_ids.values(), *c.ids):
            image = h.get("runtimeimage", runtime_id)
            h.wait("latest approved current restored " + runtime_id, lambda runtime_id=runtime_id,
                   image=image: h.advertised(runtime_id, "ready", image["status"]["generation"]))
        c.assert_base_intact()
        h.record("real TLS provider refuses OnDemand/stale capabilities; all-ID fence retention/recovery",
                 {"audit": self.audit, "staleRefusal": refusal.stderr,
                  "nodeUID": h.node_uid, "uninstallVerified": False},
                 ["E2E-06", "E2E-08", "OD-06", "FENCE-04"])
        name = h.cluster + "-control-pins"
        obj = h.get("validatingwebhookconfiguration", name)
        require_owned(obj, h.run_id, h.owned[("validatingwebhookconfiguration", "", name)])
        h.kubectl("delete", "--raw",
                  "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/" + name,
                  "-f", "-", input=json.dumps({"apiVersion": "v1", "kind": "DeleteOptions",
                                                "preconditions": {"uid": obj["metadata"]["uid"],
                                                                  "resourceVersion":
                                                                      obj["metadata"]["resourceVersion"]}}))
        # Its namespace is removed only after the webhook no longer references it.
        self.h.wait("control pin webhook removed", lambda:
                    self.h.get("validatingwebhookconfiguration", name, absent=True) is None)
        c.workloads[self.name] = h.get("deployment", self.name, c.namespace)


def _release_config(h):
    profile = h.get("nodeprofile", h.profile)
    uid = require_owned(profile, h.run_id, h.owned[("nodeprofile", "", h.profile)])
    node = h.node_snapshot()
    require(node["metadata"]["uid"] == h.node_uid
            and node["metadata"]["labels"].get("brewlet.sh/owner-uid") == uid
            and node["metadata"]["labels"].get("brewlet.sh/owner-node-uid") == h.node_uid
            and node["metadata"].get("annotations", {}).get("brewlet.sh/owner-name") == h.profile,
            "release hook requires actual live profile/Node ownership")
    require(not profile["metadata"].get("deletionTimestamp")
            and profile.get("status", {}).get("provisioningGeneration") == profile["metadata"]["generation"],
            "release hook must precede deletion of the reconciled live profile")
    require(RELEASE_MARKER not in node["metadata"].get("annotations", {}),
            "previous release CAS instrumentation already exists")
    return {"revision": 1, "runID": h.run_id, "node": h.node, "nodeUID": h.node_uid,
            "profile": h.profile, "profileUID": uid, "generation": profile["metadata"]["generation"],
            "pinnedLabels": [], "denyFence": False,
            "operatorUsername": "system:serviceaccount:" + h.control_namespace + ":brewlet-operator"}


def release_dockerfile(run_id):
    command = ["python3", "-c", "import sys; sys.path.insert(0, '/acceptance/live-v2'); "
               "import control_edges; control_edges._serve_release_admission()"]
    return (f"FROM registry.brewlet.test/{run_id}/runtime/python:base\n"
            "COPY harness.py interceptor.py control_edges.py /acceptance/live-v2/\n"
            "ENTRYPOINT " + json.dumps(command) + "\n")


def before_cleanup(h):
    """Install ONE-shot real release-CAS interception, before production_cleanup.

    The parent must call after_cleanup afterwards; merely installing this hook
    is never counted as a pass. No host identity/claims or cleanup state change.
    """
    require(not hasattr(h, "_control_release_config"), "release CAS hook already installed")
    h.wait("current profile generation reconciled before real release hook", lambda:
           h.get("nodeprofile", h.profile).get("status", {}).get("provisioningGeneration")
           == h.get("nodeprofile", h.profile)["metadata"]["generation"])
    config = _release_config(h)
    name = "control-release-cas"
    directory = h.work / name
    directory.mkdir()
    for source in (HERE / "harness.py", HERE / "interceptor.py", HERE / "control_edges.py"):
        shutil.copy2(source, directory / source.name)
    dockerfile = directory / "Dockerfile"
    dockerfile.write_text(release_dockerfile(h.run_id))
    tag = f"registry.brewlet.test/{h.run_id}/release-cas:fixture"
    h.build(tag, dockerfile, directory)
    h.load_image(tag)
    dns = name + "." + h.control_namespace + ".svc"
    h.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
           "-subj", "/CN=" + dns, "-addext", "subjectAltName=DNS:" + dns,
           "-keyout", directory / "tls.key", "-out", directory / "tls.crt"])
    os.chmod(directory / "tls.key", 0o600)
    certificate = base64.b64encode((directory / "tls.crt").read_bytes()).decode()
    h.apply({"apiVersion": "v1", "kind": "Secret",
             "metadata": {"name": name + "-tls", "namespace": h.control_namespace},
             "type": "kubernetes.io/tls",
             "data": {"tls.crt": certificate,
                      "tls.key": base64.b64encode((directory / "tls.key").read_bytes()).decode()}})
    h.apply({"apiVersion": "v1", "kind": "Secret",
             "metadata": {"name": name + "-config", "namespace": h.control_namespace},
             "stringData": {"config.json": json.dumps(config)}})
    h.apply({"apiVersion": "v1", "kind": "ServiceAccount",
             "metadata": {"name": name, "namespace": h.control_namespace}})
    role = h.cluster + "-release-cas"
    h.apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
             "metadata": {"name": role},
             "rules": [{"apiGroups": [""], "resources": ["nodes"],
                        "resourceNames": [h.node], "verbs": ["get", "patch"]},
                       {"apiGroups": ["node.brewlet.sh"], "resources": ["nodeprofiles"],
                        "resourceNames": [h.profile], "verbs": ["get"]}]})
    h.apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
             "metadata": {"name": role},
             "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": role},
             "subjects": [{"kind": "ServiceAccount", "name": name, "namespace": h.control_namespace}]})
    h.apply({"apiVersion": "v1", "kind": "Service",
             "metadata": {"name": name, "namespace": h.control_namespace},
             "spec": {"selector": {CASE: name}, "ports": [{"port": 443, "targetPort": 9443}]}})
    h.apply({"apiVersion": "apps/v1", "kind": "Deployment",
             "metadata": {"name": name, "namespace": h.control_namespace},
             "spec": {"replicas": 1, "selector": {"matchLabels": {CASE: name}},
                      "template": {"metadata": {"labels": {OWNER: h.run_id, CASE: name}},
                                   "spec": {"serviceAccountName": name, "containers": [{
                                       "name": "interceptor", "image": tag, "imagePullPolicy": "Never",
                                       "env": [{"name": "CONFIG", "value": "/config/config.json"}],
                                       "readinessProbe": {"httpGet": {"scheme": "HTTPS",
                                                                    "path": "/ready", "port": 9443}},
                                       "volumeMounts": [{"name": "tls", "mountPath": "/certs"},
                                                        {"name": "config", "mountPath": "/config"}]}],
                                            "volumes": [
                                                {"name": "tls", "secret": {"secretName": name + "-tls"}},
                                                {"name": "config", "secret": {
                                                    "secretName": name + "-config"}}]}}}})
    h.kubectl("-n", h.control_namespace, "rollout", "status",
              "deployment/" + name, "--timeout=120s", timeout=150)
    h.wait("release interceptor reads its exact invocation configuration", lambda:
           h.kubectl("-n", h.control_namespace, "exec", "deployment/" + name, "--",
                     "python3", "-c",
                     "import json; print(json.load(open('/config/config.json'))['nodeUID'])")
           .stdout.strip() == h.node_uid, timeout=120)
    webhook = h.cluster + "-release-cas"
    h.apply({"apiVersion": "admissionregistration.k8s.io/v1",
             "kind": "ValidatingWebhookConfiguration",
             "metadata": {"name": webhook},
             "webhooks": [{"name": "release-cas.acceptance.brewlet.test",
                           "admissionReviewVersions": ["v1"], "sideEffects": "NoneOnDryRun",
                           "failurePolicy": "Fail", "timeoutSeconds": 15,
                           "clientConfig": {"caBundle": certificate, "service": {
                               "name": name, "namespace": h.control_namespace, "path": "/"}},
                           "rules": [{"operations": ["UPDATE"], "apiGroups": [""],
                                      "apiVersions": ["v1"], "resources": ["nodes"]}]}]})
    h._control_release_config = config
    h.result["claimReleaseCAS"] = {"outcome": "armed-not-verified", "nodeUID": h.node_uid,
                                   "profileUID": config["profileUID"],
                                   "webhook": webhook, "config": config,
                                   "recreatedPhysicalNodeVerified": False}
    h.save()


def verify_release_audit(rows, node, config):
    injections = [r for r in rows if r.get("event") == "release-cas-injected"]
    retries = [r for r in rows if r.get("event") == "release-retry-validated"]
    require(len(injections) == 1 and bool(retries), "missing one-shot injection/real fresh release retry")
    first = injections[0]
    require(first.get("nodeUID") == config["nodeUID"]
            and first.get("profileUID") == config["profileUID"]
            and first.get("requestResourceVersion") != first.get("patchedResourceVersion")
            and bool(first.get("patchedResourceVersion"))
            and first.get("requestUID") and first.get("operator") == config["operatorUsername"],
            "CAS injection lacks actual changed resourceVersion/operator identity")
    matching = []
    for retry in retries:
        conflicts = retry.get("conflictCheckpoint", [])
        require(retry.get("nodeUID") == config["nodeUID"]
                and retry.get("profileUID") == config["profileUID"]
                and retry.get("operator") == config["operatorUsername"]
                and retry.get("requestUID") != first["requestUID"]
                and retry.get("requestResourceVersion") != first["requestResourceVersion"]
                and bool(retry.get("requestResourceVersion"))
                and retry.get("marker") == first.get("marker"),
                "release retry does not prove a fresh UID/claim-bound request")
        matching.extend(c for c in conflicts if c.get("reason") == "CleanupBlocked"
                        and config["node"] in c.get("message", "")
                        and ("object has been modified" in c.get("message", "")
                             or "Conflict" in c.get("message", "")))
    require(bool(matching), "no real API Conflict checkpoint; attempted injection is not success")
    require(node["metadata"]["uid"] == config["nodeUID"], "cleanup replaced the real physical Node")
    labels, annotations = node["metadata"].get("labels", {}), node["metadata"].get("annotations", {})
    require(not labels.get("brewlet.sh/owner-uid") and not labels.get("brewlet.sh/owner-node-uid")
            and not annotations.get("brewlet.sh/owner-name"),
            "genuine production cleanup did not release the UID-bound claim")
    require(not any(k == "brewlet.sh/runtime" or k.startswith("brewlet.sh/runtime.") for k in labels)
            and annotations.get("brewlet.sh/runtime-images", "") in ("", "{}"),
            "claim release left runtime state")
    require(json.loads(annotations.get(RELEASE_MARKER, "{}")) == first.get("marker"),
            "one-shot annotation readback does not match actual intercepted request")
    return {"injection": first, "retries": retries, "firstRealConflict": matching[0]}


def _delete_hook_object(h, kind, name, namespace=None):
    paths = {
        "ValidatingWebhookConfiguration":
            "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations",
        "Deployment": "/apis/apps/v1/namespaces/" + h.control_namespace + "/deployments",
        "ClusterRole": "/apis/rbac.authorization.k8s.io/v1/clusterroles",
        "ClusterRoleBinding": "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings",
        "Service": "/api/v1/namespaces/" + h.control_namespace + "/services",
        "ServiceAccount": "/api/v1/namespaces/" + h.control_namespace + "/serviceaccounts",
        "Secret": "/api/v1/namespaces/" + h.control_namespace + "/secrets",
    }
    key = (kind.lower(), namespace or "", name)
    require(key in h.owned, "release hook cleanup requires recorded invocation ownership")
    obj = h.get(kind, name, namespace)
    require_owned(obj, h.run_id, h.owned[key])
    h.kubectl("delete", "--raw", paths[kind] + "/" + name, "-f", "-",
              input=json.dumps({"apiVersion": "v1", "kind": "DeleteOptions",
                                "preconditions": {"uid": obj["metadata"]["uid"],
                                                  "resourceVersion": obj["metadata"]["resourceVersion"]},
                                "propagationPolicy": "Foreground"}))


def after_cleanup(h):
    """Verify real API 409 + new request/Teardown revalidation + actual release."""
    require(hasattr(h, "_control_release_config"), "before_cleanup hook was not installed")
    config = h._control_release_config
    name = "control-release-cas"
    rows = []
    log = h.kubectl("-n", h.control_namespace, "logs", "deployment/" + name,
                    "-c", "interceptor", "--tail=5000").stdout
    (h.work / "control-release-cas-audit.log").write_text(log)
    for line in log.splitlines():
        try:
            rows.append(json.loads(line))
        except ValueError:
            continue
    require(h.get("nodeprofile", h.profile, absent=True) is None,
            "production cleanup has not actually finalized the original profile")
    try:
        evidence = verify_release_audit(rows, h.node_snapshot(), config)
    except Exception as exc:
        h.result["claimReleaseCAS"].update(outcome="failed", failure=str(exc))
        h.save()
        raise
    h.record("real controller claim-release PUT hits API CAS conflict, freshly revalidates and releases",
             {**evidence, "nodeUID": config["nodeUID"], "profileUID": config["profileUID"],
              "claimReleaseVerified": True, "recreatedPhysicalNodeVerified": False},
             ["E2E-09", "OWN-02"])
    h.result["claimReleaseCAS"].update(outcome="passed-real-release-cas", evidence=evidence)
    webhook = h.cluster + "-release-cas"
    _delete_hook_object(h, "ValidatingWebhookConfiguration", webhook)
    h.wait("release CAS webhook removed before its endpoint",
           lambda: h.get("validatingwebhookconfiguration", webhook, absent=True) is None)
    _delete_hook_object(h, "Deployment", name, h.control_namespace)
    h.wait("release interceptor deployment deleted", lambda:
           h.get("deployment", name, h.control_namespace, absent=True) is None)
    for kind, obj_name, namespace in (
        ("Service", name, h.control_namespace), ("Secret", name + "-tls", h.control_namespace),
        ("Secret", name + "-config", h.control_namespace), ("ServiceAccount", name, h.control_namespace),
        ("ClusterRoleBinding", h.cluster + "-release-cas", None),
        ("ClusterRole", h.cluster + "-release-cas", None),
    ):
        _delete_hook_object(h, kind, obj_name, namespace)
    h.save()


def execute(h):
    """Parent-scheduled real acceptance; never invoked automatically."""
    require(bool(h.initial) and set(h.initial) == set(h.languages),
            "control callback must run after selected application execution/rotations")
    control = ControlEdges(h)
    control.original_values = control.release_values()
    require(not control.original_values.get("operator", {}).get(
        "rotationRestarts", {}).get("enabled", False), "initial release already enables restarts")
    h.result["controlAcceptance"] = {"selected": list(SELECTED), "outcome": "running",
                                     "fullProposalAcceptance": False, "liveExecuted": True,
                                     "namespace": control.namespace, "runtimeIDs": control.ids,
                                     "gaps": list(GAPS)}
    for gap in GAPS:
        if gap not in h.result["gaps"]:
            h.result["gaps"].append(gap)
    h.save()
    control.check_rbac(False)
    h.apply({"apiVersion": "v1", "kind": "Namespace",
             "metadata": {"name": control.namespace}})
    if hasattr(h, "delivery"):
        h.delivery.install_pull_secret(control.namespace)
    h.apply({"apiVersion": "v1", "kind": "Service",
             "metadata": {"name": "control-headless", "namespace": control.namespace},
             "spec": {"clusterIP": "None", "selector": {OWNER: h.run_id},
                      "ports": [{"port": 8000}]}})
    try:
        control.api_cas()
        control.eager_readiness_failure()
        control.strategy_matrix()
        control.superseded()
        control.pacing()
        control.excluded_bound()
        control.clean_workloads()
        ProviderPins(control).exercise()
        control.cleanup()
        h.result["controlAcceptance"]["outcome"] = "passed-selected-only"
        h.save()
    except BaseException as exc:
        h.result["controlAcceptance"].update(outcome="failed", failure=str(exc))
        try:
            control.capture_provisioning_diagnostics()
            control.capture_provisioner_errors()
        except Exception as diagnostic_error:
            h.result["controlAcceptance"]["diagnosticFailure"] = str(diagnostic_error)
        h.save()
        # Leave safety state and resources intact for parent diagnostics/destruction.
        # No forced-finalizer/claim/receipt cleanup is attempted on failed acceptance.
        raise
