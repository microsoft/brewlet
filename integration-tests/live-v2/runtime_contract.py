"""Observe actual published roots, generic launch inputs and durable generations."""

import base64
import hashlib
import json
import re
from datetime import datetime, timezone

from harness import API, FENCE, IDS, LANGUAGES, STORE, require


def validate_publication(metadata, inventory, architecture, canonical, permissions):
    expected = f"{STORE}/{metadata['id']}/{inventory['metadata']['uid']}/{inventory['status']['generation']}"
    require(canonical == expected, "current is not the exact immutable epoch/generation directory")
    require(metadata["id"] == inventory["metadata"]["name"]
            and metadata["inventoryUID"] == inventory["metadata"]["uid"]
            and metadata["generation"] == inventory["status"]["generation"]
            and metadata["sourceDigest"] == inventory["status"]["digest"]
            and metadata["sourceDigest"] == inventory["spec"]["source"]["image"].split("@")[1],
            "published metadata/source does not match approved inventory")
    require(metadata["platform"]["os"] == "linux"
            and metadata["platform"]["architecture"] == architecture
            and metadata["uid"] == inventory["spec"]["user"]["uid"]
            and metadata["gid"] == inventory["spec"]["user"]["gid"]
            and metadata["uid"] > 0 and metadata["gid"] > 0
            and bool(metadata["validatedAt"]), "published numeric identity/platform/validation is incomplete")
    require(re.fullmatch(r"sha256:[0-9a-f]{64}", metadata["manifestDigest"])
            and datetime.fromisoformat(metadata["validatedAt"].replace("Z", "+00:00")).timestamp() > 0,
            "published manifest digest or validation timestamp is invalid")
    names = [entry.split("=", 1)[0] for entry in metadata["env"]]
    require(len(names) == len(set(names)), "published runtime environment contains duplicate names")
    require(len(permissions) == 2, "immutable publication permission observations missing")
    for index, line in enumerate(permissions):
        uid, gid, mode = line.split()
        require(uid == gid == "0" and int(mode, 8) & (0o77 if index == 1 else 0o22) == 0,
                "immutable generation or metadata is not protected by root ownership")
    return expected


def publication(h, language):
    inventory = h.get("runtimeimage", IDS[language])
    metadata = h.current(language)
    current = STORE + "/" + IDS[language] + "/current"
    canonical = h.node_exec("readlink", "-f", current).stdout.strip()
    permissions = h.node_exec("stat", "-Lc", "%u %g %a", current, current + "/metadata.json").stdout.splitlines()
    expected = validate_publication(metadata, inventory, h.arch, canonical, permissions)
    require(h.node_exec("cat", expected + "/rootfs/.brewlet-source").stdout.strip() == metadata["sourceDigest"],
            "actual published root source marker differs from immutable metadata")
    node = h.node_snapshot()
    labels = node["metadata"].get("labels", {})
    diagnostics = json.loads(node["metadata"].get("annotations", {}).get("brewlet.sh/runtime-images", "{}"))
    require(labels.get("brewlet.sh/runtime") == "ready"
            and labels.get("brewlet.sh/runtime." + IDS[language]) == "ready"
            and diagnostics[IDS[language]]["generation"] == metadata["generation"]
            and not any(key.startswith(("brewlet.sh/jdk.", "brewlet.sh/jdk-feature.")) for key in labels),
            "v2 readiness/diagnostics disagree or legacy JDK labels are published")
    h.record(language + " actual root publication binds protected epoch, generation, source and v2 readiness",
             {"nodeUID": h.node_uid, "runtime": metadata, "immutableDirectory": expected,
              "permissions": permissions}, ["ROOT-04", "PROF-04", "LABEL-01"])


def generic_launches(h):
    require(tuple(h.languages) == LANGUAGES, "generic qualification requires all four explicit language fixtures")
    kinds = {obj["spec"]["names"]["kind"] for obj in h.get("customresourcedefinitions")["items"]
             if obj["spec"]["group"] == "brewlet.sh" or obj["spec"]["group"].endswith(".brewlet.sh")}
    require(kinds == {"RuntimeImage", "NodeProfile", "NodeRetirementEvidence"},
            "actual Brewlet installation contains a language-specific workload API")
    for language in LANGUAGES:
        pod = h.get("pod", language + "-old", h.namespace)
        container = pod["spec"]["containers"][0]
        require(container["image"] == h.app_images[language] and "command" not in container
                and "args" not in container and "workingDir" not in container,
                "core fixture replaced explicitly published launch inputs")
        h.assert_report(h.http(language + "-old"), language, arguments=[], cwd="/app")
        h.assert_report(json.loads(h.exec(language + "-old", h.probe(language)).stdout), language,
                        arguments=["--probe", "two words"], cwd="/app")
    h.record("same generic producer and shim execute all four explicit published launch contracts",
             {"languages": list(LANGUAGES), "applicationImages": dict(h.app_images),
              "noWorkloadCRD": True}, ["ARCH-01", "UP-06"])


def protected_identity(h):
    binary = h.work / "identity-probe"
    h.run(["go", "build", "-mod=readonly", "-o", binary, "./integration-tests/live-v2/identityprobe"],
          env={"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": h.arch}, timeout=180)
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    target = "/usr/local/bin/brewlet-identity-probe-" + h.run_id
    h.node_exec("test", "!", "-e", target)
    h.node_exec("/bin/sh", "-ec", 'umask 077; set -C; base64 -d > "$1"; chmod 0700 "$1"',
                 "identity-probe", target, input=base64.b64encode(binary.read_bytes()).decode())
    try:
        require(h.node_exec("sha256sum", target).stdout.split()[0] == digest, "identity probe transfer changed bytes")
        for language in h.languages:
            pod = h.get("pod", language + "-old", h.namespace)
            lease = h.lease(pod)
            observed = json.loads(h.node_exec(target, lease["taskID"]).stdout)
            require(observed["containerID"] == lease["taskID"] and observed["reference"] == h.app_images[language]
                     and observed["targetDigest"] == h.app_images[language].split("@")[1]
                     and observed["architecture"] == h.arch
                     and re.fullmatch(r"sha256:[0-9a-f]{64}", observed["configDigest"]),
                     "actual protected CRI application, config or native platform identity differs")
            h.record(language + " actual protected CRI identity resolves verified native descriptors and config",
                      observed, ["RUN-02"])
        h.bypass_admission()
        try:
            pod = h.pod("forged-artifact-hints", h.probe_language, bound=True)
            pod["metadata"]["annotations"].update({"brewlet.sh/artifact-container": "untrusted",
                 "brewlet.sh/artifact-ref": "untrusted.invalid/app@sha256:" + "0" * 64,
                 "brewlet.sh/artifact-digest": "sha256:" + "0" * 64})
            h.apply(pod)
            running = h.wait("forged compatibility hints cannot replace actual protected application", lambda:
                              h.ready("forged-artifact-hints"))
            observed = json.loads(h.node_exec(target, h.lease(running)["taskID"]).stdout)
            require(observed["reference"] == h.app_images[h.probe_language],
                     "untrusted hints replaced protected CRI requested image")
            h.assert_report(h.http("forged-artifact-hints"), h.probe_language, arguments=[], cwd="/app")
            h.record("actual admission-bypassing forged artifact hints are not execution authority",
                      observed, ["RUN-02", "ADM-04"])
            h.delete("pod", "forged-artifact-hints", h.namespace)
            h.wait("hint probe workload drains", lambda: h.get("pod", "forged-artifact-hints", h.namespace,
                                                                absent=True) is None)
        finally:
            h.restore_admission()
    finally:
        require(h.node_exec("sha256sum", target).stdout.split()[0] == digest,
                 "identity probe replaced; refusing cleanup")
        h.node_exec("rm", "--", target)


def generation_transition(previous, current, source):
    require(current["metadata"]["uid"] == previous["metadata"]["uid"]
            and current["status"]["generation"] > previous["status"]["generation"]
            and current["status"]["digest"] == source.split("@")[1],
            "controller recovery reused a generation or approved another digest")


def validate_quarantine(participant, excluded, node, lease, conditions, now):
    from control_edges import timestamp

    origin = participant.get("lastProgressAt") or participant["assignedAt"]
    elapsed = now - timestamp(origin)
    require(elapsed >= 600, "rotation quarantine occurred before the real ten-minute progress deadline")
    require(excluded["reason"] == "RotationStalled" and excluded["uid"] == node["metadata"]["uid"]
            and excluded["name"] == node["metadata"]["name"]
            and excluded.get("observedGeneration") == lease["runtime"]["generation"],
            "stalled rotation diagnostics lost actual Node UID or retained generation")
    withdrawn = "brewlet.sh/runtime." + lease["runtime"]["id"] not in node["metadata"].get("labels", {})
    fenced = any(t["key"] == FENCE and t.get("value") == "provisioning" and t["effect"] == "NoSchedule"
                 for t in node.get("spec", {}).get("taints", []))
    require(withdrawn or fenced, "stalled exclusion lacks confirmed capability withdrawal or node-wide fence")
    require(any(c["type"] == "RotationComplete" and c["status"] == "True" for c in conditions),
            "confirmed stalled withdrawal did not complete the selected rotation")
    return {"excluded": excluded, "elapsedSeconds": elapsed}


def durable_generations(h):
    initial = {language: h.get("runtimeimage", IDS[language]) for language in h.languages}
    h.kubectl("-n", h.control_namespace, "rollout", "restart", "deployment/brewlet-operator")
    h.kubectl("-n", h.control_namespace, "rollout", "status", "deployment/brewlet-operator",
              "--timeout=120s", timeout=150)
    records = {}
    for language in h.languages:
        previous = initial[language]
        recovered = h.get("runtimeimage", IDS[language])
        require(recovered["status"]["generation"] == previous["status"]["generation"]
                and recovered["status"]["digest"] == previous["status"]["digest"],
                "operator restart rewrote an unchanged durable generation")
        transitions = []
        for revision in ("before", "after"):
            source = h.runtime_images[language][revision]
            obj = h.get("runtimeimage", IDS[language])
            obj["spec"]["source"]["image"] = source
            h.replace_inventory_spec(obj)
            def advanced():
                current = h.get("runtimeimage", IDS[language])
                if current.get("status", {}).get("generation", 0) <= previous["status"]["generation"]:
                    return False
                generation_transition(previous, current, source)
                return current if h.advertised(IDS[language], "ready", current["status"]["generation"]) else False
            current = h.wait(language + " durable digest rotation after actual operator restart", advanced)
            transitions.append({"generation": current["status"]["generation"], "digest": current["status"]["digest"]})
            previous = current
        old = h.get("pod", language + "-old", h.namespace)
        require(h.lease(old) == h.initial[language]["lease"], "durable rotation changed a running lease")
        records[language] = transitions
    h.record("actual operator restart preserves journaled targets and allocates fresh monotonic digest generations",
             records, ["INV-03"])


def rotation_participants(h):
    from activation import invalid_path_runtime

    operator = h.get("deployment", "brewlet-operator", h.control_namespace)
    arguments = [arg for container in operator["spec"]["template"]["spec"]["containers"]
                 for arg in container.get("args", [])]
    require("--node-progress-deadline=10m" in arguments, "actual node rotation deadline is not ten minutes")
    rid = "rotation-proof-" + h.run_id
    h.apply({"apiVersion": API, "kind": "RuntimeImage", "metadata": {"name": rid},
             "spec": {"source": {"image": h.runtime_images["python"]["before"]},
                      "user": {"uid": 65532, "gid": 65532}}})
    h.wait("rotation proof inventory approved", lambda:
           h.get("runtimeimage", rid).get("status", {}).get("generation", 0) > 0)
    profile = h.get("nodeprofile", h.profile)
    profile["spec"]["runtimeImages"].append({"id": rid, "install": "OnDemand"})
    h.replace_inventory_spec(profile)
    h.wait("cold OnDemand rotation proof offered", lambda: h.advertised(rid, "available"))
    def cold_rotation():
        image = h.get("runtimeimage", rid)
        status = image.get("status", {})
        return image if (status.get("rotation", {}).get("generation") == status.get("generation")
                         and any(c["type"] == "RotationComplete" and c["status"] == "True"
                                 for c in status.get("conditions", []))) else False
    cold = h.wait("controller observes cold nonparticipant target", cold_rotation)
    inventory = json.loads(h.node_snapshot()["metadata"]["annotations"]["brewlet.sh/runtime-images"])
    require(inventory[rid].get("installed") is False, "cold nonparticipant already advertises an installed root")
    h.node_exec("test", "!", "-e", f"{STORE}/{rid}/current")
    require(not cold["status"]["rotation"].get("participants"),
            "uninstalled cold OnDemand entry was counted as an installed participant")
    pod = h.pod("rotation-participant", "python")
    pod["metadata"]["annotations"]["brewlet.sh/runtime-image"] = rid
    h.apply(pod)
    running = h.wait("rotation proof actual first use", lambda: h.ready("rotation-participant"))
    lease = h.lease(running)
    node = h.node_snapshot()
    require(not node.get("spec", {}).get("unschedulable")
            and not node["metadata"].get("deletionTimestamp")
            and not any(t["key"] == FENCE and t["effect"] == "NoSchedule"
                        for t in node.get("spec", {}).get("taints", []))
            and node["metadata"]["labels"]["brewlet.sh/owner-node-uid"] == h.node_uid
            and node["metadata"]["labels"]["brewlet.sh/owner-uid"] == profile["metadata"]["uid"]
            and any(condition["type"] == "Ready" and condition["status"] == "True"
                    for condition in node.get("status", {}).get("conditions", [])),
            "rotation participant is not a live Ready schedulable UID-owned native node")
    selected = invalid_path_runtime(h.delivery.work / "python-after-golden",
                                    h.delivery.work / "rotation-proof-invalid")
    source = h.delivery.publish(h.delivery.work / "rotation-proof-invalid", selected, "delivery/rotation-proof-invalid")
    image = h.get("runtimeimage", rid)
    previous = image["status"]["generation"]
    image["spec"]["source"]["image"] = source
    h.replace_inventory_spec(image)

    def retrying():
        image = h.get("runtimeimage", rid)
        if image.get("status", {}).get("generation", 0) <= previous:
            return False
        authorization = json.loads(h.node_exec("cat", f"{STORE}/{rid}/authorization.json").stdout)
        if authorization.get("state") != "failed" or authorization.get("digest") != source.split("@")[1]:
            return False
        rotation = image["status"].get("rotation", {})
        participant = next((row for row in rotation.get("participants", []) if row["uid"] == h.node_uid), None)
        require(not any(c["type"] == "RotationComplete" and c["status"] == "True"
                        for c in image["status"].get("conditions", [])),
                "failed installed participant was declared complete")
        return participant if participant and participant.get("assignedAt") else False

    participant = h.wait("failed installed OnDemand runtime remains a genuine rotation participant", retrying)
    h.record("cold OnDemand exclusion and actual installed retry participation retain native owner identity",
             {"nodeUID": h.node_uid, "participant": participant, "liveLease": lease}, ["ROT-02"])

    def quarantined():
        image = h.get("runtimeimage", rid)
        excluded = next((row for row in image["status"].get("rotation", {}).get("excludedNodes", [])
                         if row["uid"] == h.node_uid), None)
        if not excluded or excluded.get("reason") != "RotationStalled":
            return False
        return validate_quarantine(participant, excluded, h.node_snapshot(), lease,
                                   image["status"].get("conditions", []),
                                   datetime.now(timezone.utc).timestamp())

    proof = h.wait("real ten-minute rotation deadline produces confirmed quarantine", quarantined, timeout=720)
    require(h.lease(h.ready("rotation-participant")) == lease, "quarantine changed an existing task lease")
    h.record("actual ten-minute stall requires confirmed withdrawal and preserves UID/generation/reason",
             proof, ["ROT-04"])
    image = h.get("runtimeimage", rid)
    failed_generation = image["status"]["generation"]
    image["spec"]["source"]["image"] = h.runtime_images["python"]["after"]
    h.replace_inventory_spec(image)
    def recovered():
        image = h.get("runtimeimage", rid)
        status = image.get("status", {})
        return (status.get("generation", 0) > failed_generation
                and status.get("digest") == h.runtime_images["python"]["after"].split("@")[1]
                and h.advertised(rid, "ready", status["generation"]))
    h.wait("corrected stalled runtime actually recovers", recovered)
    profile = h.get("nodeprofile", h.profile)
    next(entry for entry in profile["spec"]["runtimeImages"] if entry["id"] == rid)["install"] = "Eager"
    h.replace_inventory_spec(profile)
    generation = h.get("nodeprofile", h.profile)["metadata"]["generation"]
    def eager():
        authorization = json.loads(h.node_exec("cat", f"{STORE}/{rid}/authorization.json").stdout)
        image = h.get("runtimeimage", rid)
        return (authorization.get("install") == "Eager"
                and authorization.get("profileUID") == profile["metadata"]["uid"]
                and authorization.get("profileGeneration") == generation
                and h.advertised(rid, "ready", image["status"]["generation"])
                and any(row["uid"] == h.node_uid
                        for row in image["status"].get("rotation", {}).get("participants", [])))
    h.wait("actual Eager participant retains validated readiness", eager)
    h.record("actual Eager and installed OnDemand modes both retain the same valid native participant",
             {"nodeUID": h.node_uid, "profileUID": profile["metadata"]["uid"]}, ["ROT-02"])
    h.delete("pod", "rotation-participant", h.namespace)
    h.wait("rotation proof workload drains", lambda: h.get("pod", "rotation-participant", h.namespace, absent=True) is None)
    h.delete("runtimeimage", rid)
    h.wait("rotation proof inventory normally finalizes", lambda: h.get("runtimeimage", rid, absent=True) is None)
    profile = h.get("nodeprofile", h.profile)
    profile["spec"]["runtimeImages"] = [entry for entry in profile["spec"]["runtimeImages"] if entry["id"] != rid]
    h.replace_inventory_spec(profile)
