"""Real main/init/sidecar/ephemeral execution and independently rejected bad artifacts."""

import copy
import json

from harness import IDS, application_started, require, require_owned


def sidecar(base, name, port, *, native=False):
    result = copy.deepcopy(base)
    result["name"] = name
    result["env"] = [entry for entry in result.get("env", [])
                     if entry["name"] != "BREWLET_TEST_PORT"]
    result["env"].append({"name": "BREWLET_TEST_PORT", "value": str(port)})
    if native:
        result["restartPolicy"] = "Always"
        result["startupProbe"] = result.pop("readinessProbe")
    return result


def ephemeral(base, name, image=None):
    allowed = ("image", "imagePullPolicy", "securityContext", "env", "volumeMounts", "workingDir")
    result = {key: copy.deepcopy(base[key]) for key in allowed if key in base}
    result.update(name=name, targetContainerName="app", command=["python3"],
                  args=["-c", "import time; time.sleep(600)"])
    if image is not None:
        result["image"] = image
    return result


def add_ephemeral(h, name, container):
    pod = h.get("pod", name, h.namespace)
    require_owned(pod, h.run_id, h.owned[("pod", h.namespace, name)])
    current = pod["spec"].get("ephemeralContainers", [])
    require(not any(entry["name"] == container["name"] for entry in current),
            "ephemeral container name already exists")
    h.kubectl("-n", h.namespace, "patch", "pod", name, "--subresource=ephemeralcontainers",
              "--type=json", "-p", json.dumps([
                  {"op": "test", "path": "/metadata/uid", "value": pod["metadata"]["uid"]},
                  {"op": "test", "path": "/spec", "value": pod["spec"]},
                  {"op": "add", "path": "/spec/ephemeralContainers", "value": [*current, container]}]))


def statuses(pod, name):
    return [status for key in ("containerStatuses", "initContainerStatuses", "ephemeralContainerStatuses")
            for status in pod.get("status", {}).get(key, []) if status["name"] == name]


def denied(h, pod_name, container):
    pod = h.get("pod", pod_name, h.namespace)
    require_owned(pod, h.run_id, h.owned[("pod", h.namespace, pod_name)])
    messages = []
    for status in statuses(pod, container):
        require(not application_started(status), "invalid role artifact executed application code")
        state = status.get("state", {})
        for value in (state.get("terminated"), status.get("lastState", {}).get("terminated")):
            if value:
                require(value.get("reason") == "StartError"
                        and value.get("startedAt") in (
                            None, "0001-01-01T00:00:00Z", "1970-01-01T00:00:00Z"),
                        "invalid role artifact actually started a process")
                messages.append(value.get("message", ""))
        messages.append(state.get("waiting", {}).get("message", ""))
    terminal = [s for s in statuses(pod, container) if
                s.get("state", {}).get("terminated") or s.get("lastState", {}).get("terminated")]
    if terminal:
        require("InvalidLaunchConfig" in " ".join(messages),
                "invalid role failed for an unrelated reason: " + " ".join(messages)[-1500:])
    return pod if "InvalidLaunchConfig" in " ".join(messages) else False


def execute(h):
    require(hasattr(h, "delivery"), "container-role case requires real authenticated delivery")
    name = "container-roles"
    pod = h.pod(name, "python")
    base = pod["spec"]["containers"][0]
    setup = copy.deepcopy(base)
    probe = h.probe("python")
    setup.update(name="setup", command=probe[:1], args=probe[1:])
    setup.pop("readinessProbe")
    pod["spec"]["initContainers"] = [setup, sidecar(base, "native-sidecar", 8001, native=True)]
    base["lifecycle"] = {"postStart": {"exec": {"command": [
        "python3", "-c", "from pathlib import Path; "
        "Path('/app/new-volume-target/post-start').write_text('executed')"]}}}
    pod["spec"]["containers"].append(sidecar(base, "regular-sidecar", 8002))
    pod["spec"]["containers"][1].pop("lifecycle", None)
    h.apply(pod)
    ready = h.wait("actual main, init and both sidecar forms", lambda: h.ready(name))
    h.assert_report(h.http(name), "python", arguments=[], cwd="/app")
    setup_report = h.kubectl("-n", h.namespace, "logs", name, "-c", "setup").stdout
    h.assert_report(json.loads(setup_report), "python", arguments=["--probe", "two words"], cwd="/app")
    add_ephemeral(h, name, ephemeral(base, "debug"))

    def debug_running():
        current = h.get("pod", name, h.namespace)
        require_owned(current, h.run_id, h.owned[("pod", h.namespace, name)])
        return current if any(s.get("state", {}).get("running") for s in statuses(current, "debug")) else False

    ready = h.wait("actual ephemeral application task", debug_running)
    leases = {}
    for container in ("app", "regular-sidecar", "native-sidecar", "debug"):
        output = h.kubectl("-n", h.namespace, "exec", name, "-c", container, "--", *probe).stdout
        h.assert_report(json.loads(output), "python", arguments=["--probe", "two words"], cwd="/app")
        lease = h.lease(ready, container)
        require(lease["runtime"]["id"] == IDS["python"],
                "container role selected a different runtime ID")
        leases[container] = lease["taskID"]
    require(len(set(leases.values())) == 4, "container roles shared one task lease")
    h.kubectl("-n", h.namespace, "exec", name, "-c", "app", "--", "python3", "-c",
              "from pathlib import Path; assert "
              "Path('/app/new-volume-target/post-start').read_text()=='executed'")
    h.record("main, init, regular/native sidecars, ephemeral, exec/probes and postStart share generic contract",
             {"podUID": ready["metadata"]["uid"], "taskIDs": leases,
              "image": h.app_images["python"]}, ["RUN-01", "PROC-02", "PROC-03", "SEC-03"])

    malformed = h.delivery.cases["malformed"]
    for role in ("init", "regular-sidecar", "native-sidecar"):
        bad_name = "invalid-role-" + role
        bad = h.pod(bad_name, "python")
        candidate = copy.deepcopy(bad["spec"]["containers"][0])
        candidate.update(name="rejected", image=malformed)
        candidate.pop("readinessProbe")
        if role == "regular-sidecar":
            bad["spec"]["containers"].append(candidate)
        else:
            if role == "native-sidecar":
                candidate["restartPolicy"] = "Always"
            bad["spec"]["initContainers"] = [candidate]
        h.apply(bad)
        rejected = h.wait("actual bad artifact rejected for " + role,
                          lambda bad_name=bad_name: denied(h, bad_name, "rejected"))
        h.record("malformed artifact independently rejected for " + role,
                 {"podUID": rejected["metadata"]["uid"], "reason": "InvalidLaunchConfig"}, ["RUN-01"])
    add_ephemeral(h, name, ephemeral(base, "rejected-debug", malformed))
    rejected = h.wait("actual bad ephemeral artifact rejected", lambda: denied(h, name, "rejected-debug"))
    h.record("malformed artifact independently rejected for ephemeral container",
             {"podUID": rejected["metadata"]["uid"], "reason": "InvalidLaunchConfig"}, ["RUN-01"])
    h.result["containerRoleAcceptance"] = {"outcome": "passed",
        "roles": ["main", "init", "regular-sidecar", "native-sidecar", "ephemeral"],
        "activeUntilProductionCleanup": True}
    h.save()
