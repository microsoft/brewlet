"""Production worker authority negatives on invocation-owned native nodes."""

import json
import re

from harness import FENCE, OWNER, require


def initial_probe(h):
    return {"apiVersion": "v1", "kind": "Pod",
            "metadata": {"name": "initial-fence-" + h.run_id, "namespace": h.control_namespace},
            "spec": {"nodeName": h.node, "restartPolicy": "Never",
                     "serviceAccountName": "brewlet-node",
                     "tolerations": [{"key": FENCE, "operator": "Equal",
                                      "value": "provisioning", "effect": "NoSchedule"}],
                     "containers": [{"name": "provisioner", "image": h.result["workerImage"],
                                     "imagePullPolicy": "Never", "securityContext": {"privileged": True},
                                     "env": [{"name": name, "value": value} for name, value in (
                                         ("NODE_NAME", h.node), ("BREWLET_PROFILE_NAME", h.profile),
                                         ("BREWLET_PROFILE_UID", "absent-" + h.run_id),
                                         ("BREWLET_PROFILE_GENERATION", "1"),
                                         ("BREWLET_TARGET_UIDS", "[]"), ("BREWLET_MODE", "provisioning"))],
                                     "volumeMounts": [{"name": "host", "mountPath": "/host"}]}],
                     "volumes": [{"name": "host", "hostPath": {"path": "/", "type": "Directory"}}]}}


def initial_fence(h):
    def snapshot():
        return h.node_exec("/bin/sh", "-ec",
            "for path in /opt/brewlet /var/lib/brewlet /usr/local/bin/containerd-shim-brewlet-v2 "
            "/etc/containerd/config.toml; do "
            "if [ -d \"$path\" ]; then find \"$path\" -xdev -printf '%p %y %m %U %G\\n' | sort; "
            "find \"$path\" -xdev -type f -exec sha256sum '{}' ';' | sort; "
            "elif [ -f \"$path\" ]; then stat -c '%n %a %u %g' \"$path\"; sha256sum \"$path\"; "
            "else printf 'absent %s\\n' \"$path\"; fi; done").stdout

    before = snapshot()
    probe = initial_probe(h)
    name = probe["metadata"]["name"]
    h.apply(probe)
    try:
        def stopped():
            pod = h.get("pod", name, h.control_namespace)
            statuses = pod.get("status", {}).get("containerStatuses", [])
            require(pod["metadata"]["uid"] == h.owned[("pod", h.control_namespace, name)]
                    and pod["metadata"].get("labels", {}).get(OWNER) == h.run_id,
                    "initial-fence fixture Pod identity changed")
            return statuses[0]["state"].get("terminated") if len(statuses) == 1 else False
        terminal = h.wait("production worker exits on missing initial authority", stopped, timeout=90)
        require(terminal["exitCode"] != 0, "production worker accepted missing initial authority")
        logs = h.kubectl("-n", h.control_namespace, "logs", name, "-c", "provisioner").stdout
        require("ownership-fence-failed" in logs, "initial worker failed for an unrelated reason")
        require(snapshot() == before, "failed initial authority changed the actual host runtime/config/cache state")
        h.record("actual production worker refuses initial authority without host mutation",
                 {"podUID": h.owned[("pod", h.control_namespace, name)],
                  "nodeUID": h.node_uid, "exitCode": terminal["exitCode"],
                  "hostStateUnchanged": True}, ["OD-05"])
    finally:
        h.delete("pod", name, h.control_namespace)
        h.wait("initial-fence fixture removed", lambda: h.get("pod", name, h.control_namespace, absent=True) is None)


def replace_installing_worker(h, node, runtime_id):
    profile = h.get("nodeprofile", h.profile)
    uid = h.owned[("nodeprofile", "", h.profile)]
    require(profile["metadata"]["uid"] == uid, "profile identity changed before worker authority test")

    def workers():
        return [pod for pod in h.get("pods", namespace=h.control_namespace)["items"]
                if pod.get("spec", {}).get("nodeName") == node
                and pod["metadata"].get("labels", {}).get("brewlet.sh/owner-uid") == uid
                and any(any(env.get("name") == "BREWLET_MODE" and env.get("value") == "provisioning"
                            for env in container.get("env", []))
                        for container in pod["spec"].get("containers", []))]

    old = workers()
    require(len(old) == 1, "exact installing worker not found")
    old_uid = old[0]["metadata"]["uid"]
    statuses = [row for row in old[0].get("status", {}).get("containerStatuses", [])
                if row["name"] == "provisioner" and "running" in row.get("state", {})]
    require(len(statuses) == 1, "installing worker is not actually running")
    old_cid = statuses[0]["containerID"].removeprefix("containerd://")
    require(re.fullmatch(r"[0-9a-f]{64}", old_cid), "installing worker container identity is invalid")
    detail = json.loads(h.node_exec_on(node, "crictl", "inspect", old_cid).stdout)
    require(detail["status"]["id"] == old_cid
            and detail["status"]["labels"]["io.kubernetes.pod.uid"] == old_uid,
            "installing worker CRI identity does not match its actual Pod")
    old_pid = detail["info"]["pid"]
    require(type(old_pid) is int and old_pid > 1, "installing worker host PID missing")
    old_start = h.node_exec_on(node, "cat", f"/proc/{old_pid}/stat").stdout.rsplit(") ", 1)[1].split()[19]
    spec = profile["spec"]
    selected = next(entry for entry in spec["runtimeImages"] if entry["id"] == runtime_id)
    require(selected.get("install") == "OnDemand", "authority replacement requires a genuine cold OnDemand install")
    selected["idleTTL"] = "1h"
    h.replace_inventory_spec(profile, spec)
    generation = h.get("nodeprofile", h.profile)["metadata"]["generation"]
    require(generation > profile["metadata"]["generation"], "profile authority generation did not advance")

    def replaced():
        current = workers()
        if any(pod["metadata"]["uid"] == old_uid for pod in current):
            return False
        if len(current) != 1:
            return False
        env = {value["name"]: value.get("value")
               for container in current[0]["spec"]["containers"] for value in container.get("env", [])}
        if env.get("BREWLET_PROFILE_GENERATION") != str(generation):
            return False
        statuses = current[0].get("status", {}).get("containerStatuses", [])
        return current[0] if any("running" in status.get("state", {}) for status in statuses) else False

    replacement = h.wait("old installing worker exits before fresh-generation successor runs", replaced)
    def terminated():
        result = h.node_exec_on(node, "crictl", "inspect", old_cid, check=False)
        if result.returncode:
            require(re.search(r"\bcode\s*=\s*NotFound\b", result.stderr),
                    "old worker inspection failed rather than proving termination")
        elif json.loads(result.stdout)["status"]["state"] == "CONTAINER_RUNNING":
            return False
        process = h.node_exec_on(node, "cat", f"/proc/{old_pid}/stat", check=False)
        if process.returncode:
            require("No such file" in process.stderr, "old worker process inspection failed")
            return True
        return process.stdout.rsplit(") ", 1)[1].split()[19] != old_start
    h.wait("actual prior worker container and protected PID have terminated", terminated)
    def revalidated():
        authorization = json.loads(h.node_exec_on(node, "cat",
            "/opt/brewlet/runtimes/" + runtime_id + "/authorization.json").stdout)
        return authorization if (authorization["profileUID"] == uid
                                 and authorization["profileGeneration"] == generation) else False
    h.wait("successor publishes freshly revalidated installation authority", revalidated)
    h.node_exec_on(node, "test", "!", "-e", "/opt/brewlet/runtimes/" + runtime_id + "/current")
    h.record("held cold installation stops old worker and successor revalidates actual authority",
             {"nodeUID": h.node_uids[node], "previousWorkerUID": old_uid,
              "previousContainerID": old_cid, "previousPID": old_pid, "previousStart": old_start,
              "successorWorkerUID": replacement["metadata"]["uid"], "profileGeneration": generation,
              "noPrematurePublication": True}, ["OD-05"])
