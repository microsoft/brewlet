#!/usr/bin/env python3
"""Real native architecture selection while initially no eligible pool capacity is ready."""

import argparse
import json
from pathlib import Path
import signal

import delivery
from harness import (API, FENCE, OWNER, STORE, Failure, Harness, application_started,
                     docker_owned, require, require_owned, startup_taint_evidence)


def enrollment_patch(h, node, pool, runtime_id):
    name, metadata = node["metadata"]["name"], node["metadata"]
    require(name in h.node_uids and metadata["uid"] == h.node_uids[name],
            "native capacity Node UID changed")
    require(node.get("status", {}).get("nodeInfo", {}).get("architecture") == h.arch
            and node["status"]["nodeInfo"].get("operatingSystem") == "linux"
            and metadata.get("labels", {}).get("kubernetes.io/arch") == h.arch
            and metadata["labels"].get("kubernetes.io/os") == "linux",
            "native capacity requires actual unchanged hardware architecture")
    require(not metadata.get("labels", {}).get("brewlet.sh/owner-uid")
            and not metadata.get("annotations", {}).get("brewlet.sh/runtime-images"),
            "native capacity cannot adopt an already advertised/claimed node")
    require(any(t["key"] == FENCE and t.get("value") == "provisioning"
                and t["effect"] == "NoSchedule" for t in node.get("spec", {}).get("taints", [])),
            "native worker lacks its actual boot-time startup fence")
    return {"metadata": {"uid": metadata["uid"], "resourceVersion": metadata["resourceVersion"],
                         "labels": {"brewlet.test/pool": pool, OWNER: h.run_id,
                                    "brewlet.sh/runtime": "ready",
                                    "brewlet.sh/runtime." + runtime_id: "available"}}}


def observe_startup_fence(h, worker, pool, runtime_id, original):
    container = h.node_containers[worker]

    def owned_worker_state():
        info = json.loads(h.run(["docker", "inspect", container]).stdout)[0]
        docker_owned(info, h.run_id, container, kind_node=True)
        return info["State"]

    initial = owned_worker_state()
    require(initial.get("Running") and not initial.get("Paused"),
            "native worker was not running normally before the controlled startup delay")
    try:
        # Delay the actual kubelet, not authority records or the host clock.
        h.run(["docker", "pause", container])
        node = h.get("node", worker)
        enrolled = json.loads(h.kubectl(
            "patch", "node", worker, "--type=merge", "-p",
            json.dumps(enrollment_patch(h, node, pool, runtime_id)), "-o", "json").stdout)

        def fence_evidence():
            current = h.get("pod", "pending-native", h.namespace)
            require_owned(current, h.run_id, original["metadata"]["uid"])
            return startup_taint_evidence(enrolled, current,
                                          h.get("events", namespace=h.namespace)["items"])

        scheduling = h.wait("real templated native worker startup taint blocks scheduling",
                           fence_evidence, timeout=30)
    finally:
        if owned_worker_state().get("Paused"):
            h.run(["docker", "unpause", container])
    return scheduling


def exercise(h):
    d = h.delivery
    worker, pool = h.cluster + "-worker", h.run_id + "-native"
    h.profile = h.cluster + "-native"
    h.apply({"apiVersion": API, "kind": "RuntimeImage", "metadata": {"name": d.static_id},
             "spec": {"source": {"image": d.static_source}, "user": {"uid": 65532, "gid": 65532}}})
    h.wait("approved explicit native runtime", lambda:
           any(c["type"] == "Approved" and c["status"] == "True"
               for c in h.get("runtimeimage", d.static_id).get("status", {}).get("conditions", [])))
    h.apply({"apiVersion": API, "kind": "NodeProfile", "metadata": {"name": h.profile},
             "spec": {"nodePool": {"key": "brewlet.test/pool", "names": [pool]},
                      "runtimeImages": [{"id": d.static_id, "install": "OnDemand"}],
                      "tolerations": [{"key": FENCE, "operator": "Equal",
                                       "value": "provisioning", "effect": "NoSchedule"}],
                      "rollout": {"maxUnavailable": 1, "containerdRestart": "validated"}}})
    require(not any(n["metadata"].get("labels", {}).get("brewlet.test/pool") == pool
                    for n in h.get("nodes")["items"]),
            "native offering already had eligible capacity")
    pod = d.native_pod(h.arch, "pending-native")
    pod["spec"]["nodeSelector"]["brewlet.test/pool"] = pool
    preview = h.kubectl("create", "--dry-run=server", "-f", "-", input=json.dumps(pod))
    require(h.profile in preview.stderr,
            "actual zero-capacity admission did not warn with the eligible native profile")
    h.apply(pod)

    def pending():
        current = h.get("pod", "pending-native", h.namespace)
        require_owned(current, h.run_id, h.owned[("pod", h.namespace, "pending-native")])
        require(not current["spec"].get("nodeName"),
                "native application scheduled before eligible pool capacity appeared")
        return current if any(c["type"] == "PodScheduled" and c["status"] == "False"
                              and c.get("reason") == "Unschedulable"
                              for c in current.get("status", {}).get("conditions", [])) else False

    original = h.wait("native application genuinely Pending without ready pool capacity", pending)
    require(original["spec"]["nodeSelector"]["kubernetes.io/arch"] == h.arch,
            "admission changed the explicit standard architecture constraint")
    d.configure_node(worker)
    scheduling = observe_startup_fence(h, worker, pool, d.static_id, original)
    ready = h.wait("unchanged Pending native Pod starts after real worker baseline/install",
                   lambda: h.ready("pending-native"))
    require(ready["metadata"]["uid"] == original["metadata"]["uid"]
            and ready["spec"]["nodeName"] == worker
            and ready["spec"]["nodeSelector"]["kubernetes.io/arch"] == h.arch,
            "native scheduling replaced the Pod or changed architecture/placement")
    require(h.get("node", worker)["status"]["nodeInfo"]["architecture"] == h.arch,
            "worker architecture was relabelled instead of running native hardware")
    output = h.exec("pending-native", ["/app/bin/native", "--probe"]).stdout.strip()
    require(output == "NATIVE linux/" + h.arch, "native artifact did not execute its actual architecture")
    lease = h.lease(ready)
    require(lease["runtime"]["id"] == d.static_id, "native process used another runtime ID")
    h.record("architecture-constrained native Pod waits for real pool capacity then executes unchanged",
             {"podUID": ready["metadata"]["uid"], "nodeUID": h.node_uids[worker],
              "architecture": h.arch, "startupTaintEvents": scheduling,
              "notAlternateArchitectureProof": True, "notProviderAutoscalerProof": True},
             ["E2E-05", "SCHED-01", "E2E-03"])

    other = "amd64" if h.arch == "arm64" else "arm64"
    unsupported = d.native_pod(other, "unsupported-native")
    unsupported["spec"]["nodeSelector"] = {"brewlet.test/pool": pool}
    h.apply(unsupported)

    def wrong_platform():
        current = h.get("pod", "unsupported-native", h.namespace)
        require_owned(current, h.run_id, h.owned[("pod", h.namespace, "unsupported-native")])
        for status in current.get("status", {}).get("containerStatuses", []):
            require(not application_started(status), "unsupported native image executed")
        messages = [e.get("message", "") for e in h.get("events", namespace=h.namespace)["items"]
                    if e.get("involvedObject", {}).get("uid") == current["metadata"]["uid"]]
        if current["spec"].get("nodeName") != worker:
            return False
        return current if any("platform" in m.lower() and
                              ("no match" in m.lower() or "not found" in m.lower())
                              for m in messages) else False

    failed = h.wait("architecture-unconstrained unsupported image genuinely fails at pull",
                    wrong_platform, timeout=180)
    h.record("unsupported native pull on real capacity is not rescheduling or autoscaling success",
             {"podUID": failed["metadata"]["uid"], "nodeUID": h.node_uids[worker],
              "architectureSelectorAbsent": True, "applicationStarted": False,
              "providerScaleFromZeroVerified": False}, ["E2E-05", "SCHED-01"])
    for name in ("pending-native", "unsupported-native"):
        h.delete("pod", name, h.namespace)
        h.wait("native task/Pod drained " + name, lambda name=name:
               h.get("pod", name, h.namespace, absent=True) is None)
    h.delete("nodeprofile", h.profile)
    h.wait("native profile production cleanup and finalization", lambda:
           h.get("nodeprofile", h.profile, absent=True) is None)
    node = h.get("node", worker)
    require(node["metadata"]["uid"] == h.node_uids[worker]
            and not node["metadata"].get("labels", {}).get("brewlet.sh/owner-uid"),
            "native cleanup did not release its exact UID-bound claim")
    require(h.node_exec_on(worker, "test", "!", "-e", STORE + "/" + d.static_id + "/current",
                          check=False).returncode == 0,
            "native production cleanup retained its current runtime root")
    h.delete("runtimeimage", d.static_id)
    h.wait("native inventory really finalized", lambda:
           h.get("runtimeimage", d.static_id, absent=True) is None)
    h.result["productionCleanupVerified"] = True


def run(work):
    h = Harness(work)
    h.result.update(selectedScenario="native-capacity", selectedLanguages=list(h.languages))
    status = 1
    h.result["gaps"].append("Focused native scheduling case; combined and cold deadline cases are separate")
    try:
        for phase, action in (("prerequisites", h.preflight),
                              ("cluster", lambda: h.provision_cluster(workers=1)),
                              ("build", h.prepare_images),
                              ("delivery-bootstrap", lambda: delivery.bootstrap(h)),
                              ("install", h.install), ("native-capacity", lambda: exercise(h))):
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
        print("Private native scheduling evidence:", h.work, flush=True)
    return status if h.result.get("outcome") == "passed" else 1


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", type=Path, required=True)
    def interrupted(signum, frame):
        raise Failure("native-capacity acceptance interrupted by signal " + str(signum))
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    raise SystemExit(run(parser.parse_args().work))
