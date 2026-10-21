#!/usr/bin/env python3
"""Two actual native nodes; held TLS runtime transfer, real deadline and recovery."""

import argparse
import json
from pathlib import Path
import signal
import time

from control_edges import (ControlEdges, barrier_for, blocked_pod_evidence, restart_record,
                           timestamp, verify_pacing)
import delivery
from harness import FENCE, ROOT, STORE, Failure, Harness, require
from transfer_gate import TransferGate


DEADLINE = 60  # Longer than the real 30-second shim Create wait.


def verify_default_restart_arguments(rendered):
    arguments = {line.strip().removeprefix("- ").strip("'\"") for line in rendered.splitlines()}
    require("--restart-max-concurrent=5" in arguments
            and "--restart-workload-progress-deadline=10m" in arguments,
            "rendered opt-in defaults are not five slots and a ten-minute workload deadline")


def exercise(h, control, gate):
    worker = h.cluster + "-worker"
    rid = control.ids[0]
    h.apply({"apiVersion": "v1", "kind": "Namespace",
             "metadata": {"name": control.namespace}})
    h.delivery.install_pull_secret(control.namespace)
    control.original_values = control.release_values()
    rendered = control.helm(["template", h.cluster, ROOT / "kubernetes/charts/brewlet",
                             "--show-only", "templates/control-plane.yaml",
                             "--set", "operator.rotationRestarts.enabled=true"]).stdout
    verify_default_restart_arguments(rendered)
    h.record("rendered restart opt-in defaults preserve five slots and ten-minute workload deadline",
             {"scope": "rendered-config-contract", "notElapsedTenMinuteTimingProof": True},
             ["ROT-09"])
    control.create_inventory()
    profile = h.get("nodeprofile", h.profile)
    spec = profile["spec"]
    next(e for e in spec["runtimeImages"] if e["id"] == rid)["install"] = "OnDemand"
    control.patch_inventory_spec(profile, spec)
    h.wait("primary offers warm OnDemand ID", lambda: h.advertised(rid, "ready"))
    for name in ("cold-a", "cold-b"):
        control.apply_workload(control.workload(name, "Deployment", rid))
        control.old[name] = control.wait_ready(name)
        require(control.old[name][0]["spec"]["nodeName"] == h.node,
                "initial workload did not run on the exclusively offered primary")

    h.delivery.configure_node(worker)
    node = h.get("node", worker)
    require(node["metadata"]["uid"] == h.node_uids[worker]
            and not node["metadata"].get("labels", {}).get("brewlet.sh/owner-uid"),
            "cold worker is replaced or already claimed")
    require(any(t["key"] == FENCE and t.get("value") == "provisioning"
                and t["effect"] == "NoSchedule" for t in node["spec"].get("taints", [])),
            "worker did not join with its real startup fence")
    h.kubectl("patch", "node", worker, "--type=merge", "-p", json.dumps({
        "metadata": {"uid": h.node_uids[worker],
                     "resourceVersion": node["metadata"]["resourceVersion"],
                     "labels": {"brewlet.test/pool": h.run_id}}}))

    def cold_available():
        obj = h.get("node", worker)
        inventory = json.loads(obj["metadata"].get("annotations", {}).get(
            "brewlet.sh/runtime-images", "{}"))
        return (obj["metadata"].get("labels", {}).get("brewlet.sh/runtime." + rid) == "available"
                and inventory.get(rid, {}).get("installed") is False
                and not any(t["key"] == FENCE for t in obj["spec"].get("taints", [])))

    h.wait("genuine cold worker baseline/claim/fence release", cold_available)
    require(h.node_exec_on(worker, "test", "!", "-e", STORE + "/" + rid + "/current",
                          check=False).returncode == 0,
            "cold worker already had a runnable root for the selected ID")
    source = gate.publish_runtime()
    gate.start()
    for name in (h.node, worker):
        gate.trust_on(name)
    content = h.node_exec_on(worker, "ctr", "-n", "k8s.io", "content", "ls", "--quiet").stdout
    require(gate.layer["digest"] not in content.splitlines(),
            "gated runtime layer was not genuinely cold")

    control.conditional_node_update({"spec": {"unschedulable": True}})
    try:
        image = control.image(rid)
        previous = image["status"]["generation"]
        spec = image["spec"]
        spec["source"]["image"] = source
        control.patch_inventory_spec(image, spec)
        h.wait("actual new cold-runtime approval generation", lambda:
               control.image(rid).get("status", {}).get("generation", 0) > previous)
        image = control.complete_rotation(rid)
        target = barrier_for(image)
        require(any(n["name"] == h.node and n["uid"] == h.node_uid
                    for n in image["status"]["rotation"].get("excludedNodes", [])),
                "old host was not actually excluded from the completed rotation")
        control.enable(workload_deadline=DEADLINE)
        h.wait("actual authenticated layer GET held at transfer gate", gate.started, timeout=90)
        samples = []

        def deferred_and_handoff():
            image = control.image(rid)
            first, second = (restart_record(image, control.get_workload(name))
                             for name in ("cold-a", "cold-b"))
            verify_pacing([image], 1, time.time(), DEADLINE)
            samples.append({"first": first, "second": second})
            if not first or first["phase"] != "Deferred" or not second or not second.get("startedAt"):
                return False
            blocked = blocked_pod_evidence(first, control.pods("cold-a"), target["generation"], [])
            require(blocked["reason"] == "RuntimeImageInstalling" and blocked["node"] == worker,
                    "deferred outcome was not actual cold-node installation")
            require(timestamp(second["startedAt"]) - timestamp(first["startedAt"]) >= DEADLINE - 1,
                    "second global slot started before cold-install deadline")
            require(not first.get("completedAt"),
                    "runtime installation timeout was claimed complete")
            require(h.node_exec_on(worker, "test", "!", "-e", STORE + "/" + rid + "/current",
                                  check=False).returncode == 0,
                    "held runtime transfer published a premature current root")
            return first, second, blocked

        first, second, blocked = h.wait("cold install deadline frees global restart slot",
                                        deferred_and_handoff, timeout=DEADLINE + 65)
        snapshots = {name: control.get_workload(name) for name in ("cold-a", "cold-b")}
        h.record("real cold runtime transfer counts toward deadline and releases global slot",
                 {"first": first, "second": second, "blockedPod": blocked,
                  "deadlineSeconds": DEADLINE, "sampleCount": len(samples),
                  "workerUID": h.node_uids[worker], "freshLayer": gate.layer["digest"],
                  "noProtectedStateOrClockEdits": True}, ["ROT-09", "ROT-10"])
        from cold_rpc import execute as verify_cold_rpc
        verify_cold_rpc(h, worker, rid, blocked)
        from authority_checks import replace_installing_worker
        replace_installing_worker(h, worker, rid)
        gate.release()
        for name in ("cold-a", "cold-b"):
            pods = control.wait_ready(name)
            require(pods[0]["spec"]["nodeName"] == worker,
                    "cold replacement did not genuinely execute on the new worker")
            control.verify_running_generation(pods, control.image(rid), "after")
            h.wait("deferred restart eventually complete " + name, lambda name=name:
                   (restart_record(control.image(rid), control.get_workload(name)) or {})
                   .get("phase") == "Complete")
            current = control.get_workload(name)
            require(current["metadata"]["generation"] == snapshots[name]["metadata"]["generation"]
                    and current["spec"]["template"] == snapshots[name]["spec"]["template"],
                    "recovery repatched or changed the controller template")
        h.record("cold runtime installation recovers after real transfer release without repatching",
                 {"target": target, "workerUID": h.node_uids[worker],
                  "applicationImage": h.app_images["python"]}, ["ROT-09", "ROT-10"])
    finally:
        gate.release()
        control.conditional_node_update({"spec": {"unschedulable": False}})


def native_deadline_evidence(first, second, workload):
    if not first or first.get("phase") != "Failed" or not second or not second.get("startedAt"):
        return False
    require(first.get("reason") == "RestartFailed" and not first.get("completedAt"),
            "earlier native progress failure was claimed complete")
    require(workload.get("status", {}).get("observedGeneration", 0)
            >= workload["metadata"]["generation"], "native failure status is stale")
    conditions = [c for c in workload["status"].get("conditions", [])
                  if c["type"] == "Progressing" and c["status"] == "False"
                  and c.get("reason") == "ProgressDeadlineExceeded"]
    require(bool(conditions), "no actual Deployment progress-deadline failure")
    elapsed = timestamp(second["startedAt"]) - timestamp(first["startedAt"])
    require(9 <= elapsed < DEADLINE,
            "native progress failure did not free its slot before the workload deadline")
    return {"failed": first, "next": second, "nativeConditions": conditions,
            "slotHandoffSeconds": elapsed, "workloadDeadlineSeconds": DEADLINE,
            "nativeProgressDeadlineSeconds": 10}


def earlier_native_failure(h, control):
    rid = control.ids[1]
    for name in ("early-a", "early-b"):
        obj = control.workload(name, "Deployment", rid, gated=name == "early-a")
        obj["spec"]["progressDeadlineSeconds"] = 10
        control.apply_workload(obj)
        control.old[name] = control.wait_ready(name)
    control.rotate(rid, "after")
    control.complete_rotation(rid)

    def observed():
        image = control.image(rid)
        first = control.get_workload("early-a")
        second = control.get_workload("early-b")
        verify_pacing([image], 1, time.time(), DEADLINE)
        return native_deadline_evidence(restart_record(image, first), restart_record(image, second), first)

    proof = h.wait("real earlier Deployment progress failure releases global slot", observed, timeout=90)
    ready = control.wait_ready("early-b")
    control.verify_running_generation(ready, control.image(rid), "after")
    h.record("real Deployment native progress failure frees slot before configured workload deadline",
             proof, ["ROT-09"])


def run(work):
    h = Harness(work)
    h.result.update(selectedScenario="cold-install", selectedLanguages=list(h.languages))
    gate, control = None, None
    status = 1
    h.result["gaps"].append("Focused cold-install run; complete combined suite is separate")
    try:
        for phase, action in (("prerequisites", h.preflight),
                              ("cluster", lambda: h.provision_cluster(workers=1)),
                              ("build", h.prepare_images),
                              ("delivery-bootstrap", lambda: delivery.bootstrap(h)),
                              ("install", h.install), ("startup", h.startup)):
            h.phase = phase
            action()
        h.phase = "cold-install"
        gate, control = TransferGate(h), ControlEdges(h)
        exercise(h, control, gate)
        h.phase = "earlier-native-progress-failure"
        earlier_native_failure(h, control)
        h.phase = "cold-case-cleanup"
        gate.release()
        control.cleanup()
        h.delete("nodeprofile", h.profile)
        h.wait("two-node profile really finalizes", lambda:
               h.get("nodeprofile", h.profile, absent=True) is None)
        for name, uid in h.node_uids.items():
            node = h.get("node", name)
            require(node["metadata"]["uid"] == uid
                    and not node["metadata"].get("labels", {}).get("brewlet.sh/owner-uid"),
                    "two-node cleanup retained or released a foreign claim")
        h.result["productionCleanupVerified"] = True
        h.result["outcome"] = "passed"
        status = 0
    except Exception as error:
        h.result.update(outcome="failed", failurePhase=h.phase, failure=str(error))
        print("FAIL:", error, flush=True)
    finally:
        errors = []
        if gate is not None:
            gate.release()
        try:
            h.stop_helpers()
            h.diagnostics()
        except Exception as error:
            errors.append("diagnostics: " + str(error))
        if gate is not None:
            try:
                gate.close()
            except Exception as error:
                errors.append("transfer gate cleanup: " + str(error))
        try:
            delivery.cleanup(h)
        except Exception as error:
            errors.append("delivery cleanup: " + str(error))
        errors.extend(h.destroy())
        if errors:
            h.result.update(outcome="failed", cleanupErrors=errors)
            status = 1
        h.save()
        print("Private actual-node evidence:", h.work, flush=True)
    return status if h.result.get("outcome") == "passed" else 1


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", required=True, type=Path)
    def interrupted(signum, frame):
        raise Failure("cold-install acceptance interrupted by signal " + str(signum))
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    raise SystemExit(run(parser.parse_args().work))
