#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Identity-aware assertions and teardown for never-executed E2E profiles."""

import copy
import json
import subprocess
import sys

OWNER = "brewlet.sh/owner-uid"
IDENTITY = "brewlet.sh/owner-node-uid"
OWNER_NAME = "brewlet.sh/owner-name"
ROLES = ("node-role.kubernetes.io/control-plane", "node-role.kubernetes.io/master")


def kubectl(*args):
    return subprocess.check_output(["kubectl", *args], text=True)


def get(*args):
    return json.loads(kubectl("get", *args, "-o", "json"))


def require(value, message):
    if not value:
        raise AssertionError(message)


def preflight():
    if kubectl("get", "crd", "nodeprofiles.node.brewlet.sh", "--ignore-not-found", "-o", "name").strip():
        require(not get("nodeprofiles")["items"], "existing profiles must be retired by their owner")
    require(not get("daemonsets,pods", "-A", "-l",
                    "app in (brewlet-node-provisioner,brewlet-cleanup)")["items"],
            "existing writer fixtures must be removed by their owner")
    for node in get("nodes")["items"]:
        meta = node["metadata"]
        labels, annotations = meta.get("labels", {}), meta.get("annotations", {})
        require(not any(labels.get(k) for k in (OWNER, IDENTITY, "brewlet.sh/runtime")) and
                not any(annotations.get(k) for k in (OWNER_NAME, "brewlet.sh/profile")),
                f"{meta['name']} has existing ownership/runtime state")


def matches(term, node):
    expressions = term.get("matchExpressions", [])
    fields = term.get("matchFields", [])
    if not expressions and not fields:
        return False
    for requirements, values in (
        (expressions, node["metadata"].get("labels", {})),
        (fields, {"metadata.name": node["metadata"]["name"]}),
    ):
        for req in requirements:
            key, op, wanted = req["key"], req["operator"], req.get("values", [])
            present, value = key in values, values.get(key)
            if op == "In":
                result = present and value in wanted
            elif op == "NotIn":
                result = not present or value not in wanted
            elif op == "Exists":
                result = present
            elif op == "DoesNotExist":
                result = not present
            else:
                raise AssertionError(f"unexpected selector operator {op}")
            if not result:
                return False
    return True


def placement(namespace, name, count, mode="provision"):
    profile = get("nodeprofile", name)
    prefix = "brewlet-cleanup-" if mode == "cleanup" else "brewlet-node-provisioner-"
    ds = get("daemonset", prefix + name, "-n", namespace)
    uid = profile["metadata"]["uid"]
    require(any(o["uid"] == uid and o.get("controller") for o in
                ds["metadata"].get("ownerReferences", [])), "DaemonSet owner UID mismatch")
    targets = profile["status"].get("targets", [])
    if mode == "cleanup" and profile["status"].get("retirement"):
        targets = profile["status"]["retirement"]["targets"]
    targets = [t for t in targets if t.get("claimed")]
    require(len(targets) == int(count), f"claimed targets: {len(targets)}, expected {count}")
    spec = ds["spec"]["template"]["spec"]
    provisioner = next(c for c in spec["containers"] if c["name"] == "provisioner")
    env = {e["name"]: e.get("value") for e in provisioner.get("env", [])}
    require(env.get("BREWLET_REQUIRE_NODE_CLAIM") == "true" and
            env.get("BREWLET_PROFILE_UID") == uid and env.get("BREWLET_PROFILE_NAME") == name,
            "provisioner does not enforce the profile claim")
    if mode == "cleanup":
        require(env.get("BREWLET_MODE") == "cleanup", "cleanup worker has the wrong mode")
        require(not get("daemonsets,pods", "-n", namespace, "-l",
                        f"app=brewlet-node-provisioner,brewlet.sh/nodeprofile={name}")["items"],
                "cleanup overlaps a prior provisioner DaemonSet or Pod")
    terms = spec["affinity"]["nodeAffinity"][
        "requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
    nodes = get("nodes")["items"]
    by_name = {n["metadata"]["name"]: n for n in nodes}
    eligible = lambda node: any(matches(term, node) for term in terms)
    require({n["metadata"]["name"] for n in nodes if eligible(n)} ==
            {t["name"] for t in targets}, "placement differs from the durable claimed fleet")
    if not targets:
        require(all(not t.get("matchExpressions") and not t.get("matchFields")
                    for t in terms) and bool(terms), "empty ledger must be match-none")
    pool = profile["spec"].get("nodePool", {})
    if mode != "cleanup":
        wanted = set()
        others = get("nodeprofiles")["items"] if not pool.get("names") else []
        for node in nodes:
            labels = node["metadata"].get("labels", {})
            if not pool.get("includeControlPlane", False) and any(r in labels for r in ROLES):
                continue
            if pool.get("names"):
                key = pool.get("key") or profile["status"].get("resolvedPoolKey")
                if labels.get(key) not in pool["names"]:
                    continue
            elif any(labels.get(p["spec"].get("nodePool", {}).get("key") or
                                p.get("status", {}).get("resolvedPoolKey")) in
                     p["spec"].get("nodePool", {}).get("names", []) for p in others):
                continue
            wanted.add(node["metadata"]["name"])
        require({t["name"] for t in targets} == wanted,
                "claimed fleet differs from independent pool/role membership")
    for target in targets:
        node = by_name[target["name"]]
        require(node["metadata"]["uid"] == target["uid"], "Node UID changed")
        require(node["metadata"]["labels"].get(OWNER) == uid, "node owner UID mismatch")
        require(node["metadata"]["labels"].get(IDENTITY) == target["uid"],
                "node identity fence mismatch")
        require(node["metadata"].get("annotations", {}).get(OWNER_NAME) == name,
                "node owner name mismatch")
        # Test all OR terms, not a positional first expression: none may admit
        # another owner, replacement Node, unrecorded name, pool, or node role.
        mutations = [("name", None, "unrecorded-node")]
        for key in (OWNER, IDENTITY):
            mutations.extend([("label", key, None), ("label", key, "different-uid")])
        mutations.extend(("label", IDENTITY, other["uid"]) for other in targets
                         if other["uid"] != target["uid"])
        if mode != "cleanup":
            if not pool.get("includeControlPlane", False):
                mutations.extend(("label", role, "") for role in ROLES)
            if pool.get("names"):
                key = pool.get("key") or profile["status"].get("resolvedPoolKey")
                require(bool(key), "named profile has no resolved pool key")
                mutations.extend([("label", key, "another-pool"), ("label", key, None)])
        for kind, key, value in mutations:
            changed = copy.deepcopy(node)
            if kind == "name":
                changed["metadata"]["name"] = value
            elif value is None:
                changed["metadata"]["labels"].pop(key, None)
            else:
                changed["metadata"]["labels"][key] = value
            require(not eligible(changed), f"placement admits altered {key or kind}")
    if mode != "cleanup" and not pool.get("includeControlPlane", False):
        require(not spec.get("tolerations"), "guarded fixture declares tolerations")
        require(not any(eligible(n) for n in nodes if
                        any(r in n["metadata"].get("labels", {}) for r in ROLES)),
                "guarded profile admits a control-plane node")


def teardown(namespace, name, uid, image):
    """Test-only abort, NOT evidence of production host cleanup.

    The caller must stop and wait for its manager first. Only this invocation's
    reserved-domain, never-started workers and exact profile UID are accepted.
    Any dirty/foreign worker or identity mismatch preserves claims/finalizers.
    """
    require(image.startswith("invalid.brewlet-e2e.invalid/"),
            "teardown requires the reserved non-pullable fixture image")
    raw = kubectl("get", "nodeprofile", name, "--ignore-not-found", "-o", "json")
    if not raw.strip():
        return
    profile = json.loads(raw)
    require(profile["metadata"]["uid"] == uid, "refusing a replacement profile")
    selector = f"brewlet.sh/nodeprofile={name}"
    workers = get("daemonsets,pods", "-A", "-l", selector)["items"]
    ds_uids = {w["metadata"]["uid"] for w in workers if w["kind"] == "DaemonSet"}
    for worker in workers:
        require(worker["metadata"]["namespace"] == namespace, "foreign namespace worker")
        refs = worker["metadata"].get("ownerReferences", [])
        allowed = {uid} if worker["kind"] == "DaemonSet" else ds_uids
        require(any(r["uid"] in allowed and r.get("controller") for r in refs),
                "foreign or orphaned worker")
        spec = worker["spec"] if worker["kind"] == "Pod" else worker["spec"]["template"]["spec"]
        containers = spec.get("containers", []) + spec.get("initContainers", [])
        require(containers and all(c["image"] == image for c in containers),
                "worker uses an executable image")
        if worker["kind"] == "Pod":
            status = worker.get("status", {})
            require(status.get("phase") not in ("Running", "Succeeded", "Failed"),
                    "worker may have executed")
            for c in status.get("containerStatuses", []) + status.get("initContainerStatuses", []):
                require(not c.get("containerID") and not c.get("imageID") and
                        not c.get("restartCount") and not c.get("lastState") and
                        "running" not in c.get("state", {}) and
                        "terminated" not in c.get("state", {}), "worker has execution history")
    print(f"Aborting fixture {namespace}/{name} ({uid}): "
          f"validated {len(workers)} never-started workers", flush=True)
    kubectl("delete", "daemonsets", "-n", namespace, "-l", selector,
            "--cascade=foreground", "--wait=true", "--timeout=60s")
    require(not get("daemonsets,pods", "-A", "-l", selector)["items"],
            "workers remain; retaining ownership")
    targets = {t["name"]: t["uid"] for t in profile.get("status", {}).get("targets", [])}
    claimed = {t["name"] for t in profile.get("status", {}).get("targets", []) if t.get("claimed")}
    for node in get("nodes")["items"]:
        meta = node["metadata"]
        labels, annotations = meta.get("labels", {}), meta.get("annotations", {})
        if labels.get(OWNER) != uid:
            require(meta["name"] not in claimed or not labels.get(OWNER),
                    "fixture target belongs to another owner")
            continue
        require(targets.get(meta["name"]) == meta["uid"] == labels.get(IDENTITY),
                "fixture node identity changed")
        require(annotations.get(OWNER_NAME) == name, "fixture owner name changed")
        require(not labels.get("brewlet.sh/runtime") and
                not annotations.get("brewlet.sh/profile"), "node advertises runtime state")
        patch = [
            {"op": "test", "path": "/metadata/uid", "value": meta["uid"]},
            {"op": "test", "path": "/metadata/resourceVersion", "value": meta["resourceVersion"]},
        ]
        for location, key in (("labels", OWNER), ("labels", IDENTITY), ("annotations", OWNER_NAME)):
            patch.append({"op": "remove", "path": f"/metadata/{location}/{key.replace('/', '~1')}"})
        if "brewlet.sh/provision-state" in annotations:
            patch.append({"op": "remove", "path": "/metadata/annotations/brewlet.sh~1provision-state"})
        kubectl("patch", "node", meta["name"], "--type=json", "-p", json.dumps(patch))
        print(f"Removed exact fixture fence from {meta['name']} ({meta['uid']})", flush=True)
    current = get("nodeprofile", name)["metadata"]
    patch = [
        {"op": "test", "path": "/metadata/uid", "value": uid},
        {"op": "test", "path": "/metadata/resourceVersion", "value": current["resourceVersion"]},
        {"op": "add", "path": "/metadata/finalizers",
         "value": [f for f in current.get("finalizers", []) if f != "node.brewlet.sh/cleanup"]},
    ]
    kubectl("patch", "nodeprofile", name, "--type=json", "-p", json.dumps(patch))
    kubectl("delete", "nodeprofile", name, "--ignore-not-found", "--wait=true", "--timeout=30s")


if __name__ == "__main__":
    try:
        {"preflight": preflight, "placement": placement, "teardown": teardown}[sys.argv[1]](*sys.argv[2:])
    except (AssertionError, KeyError, subprocess.CalledProcessError) as error:
        print(error, file=sys.stderr)
        sys.exit(1)
