"""Time a real native CRI StartContainer while its runtime transfer is held."""

import base64
import copy
from contextlib import contextmanager
import hashlib
import json
import re
import time

from harness import STORE, require


def container_config(original, name, image):
    config = copy.deepcopy(original)
    config["metadata"]["name"] = name
    config["metadata"]["attempt"] = 0
    config["image"] = {"image": image, "user_specified_image": image}
    if "log_path" in config:
        config["log_path"] = name + ".log"
    elif "logPath" in config:
        config["logPath"] = name + ".log"
    else:
        raise ValueError("actual kubelet container config omitted its log path")
    config.setdefault("labels", {})["io.kubernetes.container.name"] = name
    return config


def validate_wait(elapsed, timeout, response, status):
    require(response.returncode != 0 and "RuntimeImageInstalling" in response.stdout + response.stderr,
            "real CRI cold start did not fail with RuntimeImageInstalling")
    require(29 <= elapsed < 40 and elapsed < timeout,
            "actual default Create wait is not bounded near thirty seconds below kubelet timeout")
    require(status.get("startedAt") in (0, "0") and status.get("state") != "CONTAINER_RUNNING",
            "workload code started before validated runtime publication")


@contextmanager
def paused_kubelet(h, worker):
    require(h.node_exec_on(worker, "systemctl", "is-active", "kubelet").stdout.strip() == "active",
            "cold RPC probe requires an initially active owned kubelet")
    try:
        h.node_exec_on(worker, "systemctl", "stop", "kubelet", timeout=60)
        require(h.node_exec_on(worker, "systemctl", "is-active", "kubelet",
                               check=False).stdout.strip() == "inactive",
                "owned kubelet did not stop; CRI probe would race Pod reconciliation")
        yield
    finally:
        h.node_exec_on(worker, "systemctl", "start", "kubelet", timeout=60)
        h.wait("owned kubelet restored after cold CRI probe", lambda:
               h.node_exec_on(worker, "systemctl", "is-active", "kubelet",
                              check=False).stdout.strip() == "active")
        h.wait("actual native worker Ready after kubelet restoration", lambda:
               any(condition["type"] == "Ready" and condition["status"] == "True"
                   for condition in h.get("node", worker).get("status", {}).get("conditions", [])))


def cleanup_container(h, worker, identifier, name, pod_uid):
    status = json.loads(h.node_exec_on(worker, "crictl", "inspect", identifier).stdout)["status"]
    require(status["id"] == identifier and status["metadata"]["name"] == name
            and status["labels"]["io.kubernetes.pod.uid"] == pod_uid,
            "cold RPC container identity changed; refusing cleanup")
    if status.get("state") == "CONTAINER_RUNNING":
        h.node_exec_on(worker, "crictl", "stop", identifier)
    h.node_exec_on(worker, "crictl", "rm", identifier)


def execute(h, worker, runtime_id, blocked):
    require(blocked["node"] == worker and blocked.get("uid"), "exact cold blocked Pod identity required")
    pod_uid = blocked["uid"]

    def actual_config():
        result = h.node_exec_on(worker, "crictl", "ps", "--all", "--output=json",
                               "--label", "io.kubernetes.pod.uid=" + pod_uid)
        for row in json.loads(result.stdout)["containers"]:
            if row.get("labels", {}).get("io.kubernetes.pod.uid") != pod_uid:
                continue
            inspected = h.node_exec_on(worker, "crictl", "inspect", row["id"], check=False)
            if inspected.returncode:
                require(re.search(r"\bcode\s*=\s*NotFound\b", inspected.stderr),
                        "cold CRI container inspection failed")
                continue
            detail = json.loads(inspected.stdout)
            require(detail["status"]["id"] == row["id"]
                    and detail["status"]["labels"]["io.kubernetes.pod.uid"] == pod_uid,
                    "cold container identity changed")
            return row["podSandboxId"], detail["info"]["config"]
        return False

    sandbox_id, original = h.wait("actual kubelet cold container configuration", actual_config, timeout=120)
    sandbox = json.loads(h.node_exec_on(worker, "crictl", "inspectp", sandbox_id).stdout)
    require(sandbox["status"]["id"] == sandbox_id
            and sandbox["status"]["metadata"]["uid"] == pod_uid,
            "cold Pod sandbox identity changed")
    kubelet = h.node_exec_on(worker, "cat", "/var/lib/kubelet/config.yaml").stdout
    require(re.search(r"(?m)^runtimeRequestTimeout:\s*(?:2m(?:0s)?|120s)\s*$", kubelet),
            "actual native kubelet request timeout is not the configured two minutes")
    name = "bounded-create-" + h.run_id
    documents = {
        "/tmp/" + name + "-container.json": container_config(original, name, h.app_images["python"]),
        "/tmp/" + name + "-sandbox.json": sandbox["info"]["config"],
    }
    paths = []
    identifier = None
    try:
        for path, document in documents.items():
            content = json.dumps(document).encode()
            h.node_exec_on(worker, "test", "!", "-e", path)
            h.node_exec_on(worker, "/bin/sh", "-ec", 'umask 077; set -C; base64 -d > "$1"',
                           "cold-rpc", path, input=base64.b64encode(content).decode())
            paths.append((path, hashlib.sha256(content).hexdigest()))
        with paused_kubelet(h, worker):
            try:
                observed = json.loads(h.node_exec_on(worker, "crictl", "inspectp", sandbox_id).stdout)["status"]
                require(observed["metadata"]["uid"] == pod_uid and observed["state"] == "SANDBOX_READY",
                        "owned Pod sandbox changed while stopping kubelet")
                response = h.node_exec_on(worker, "crictl", "create", sandbox_id, *documents)
                created = response.stdout.strip()
                require(re.fullmatch(r"[0-9a-f]{64}", created), "CRI create returned an invalid container ID")
                identifier = created
                started = time.monotonic()
                response = h.node_exec_on(worker, "crictl", "--timeout=120s", "start", identifier,
                                         check=False, timeout=150)
                elapsed = time.monotonic() - started
                status = json.loads(h.node_exec_on(worker, "crictl", "inspect", identifier).stdout)["status"]
                validate_wait(elapsed, 120, response, status)
                h.node_exec_on(worker, "test", "!", "-e", STORE + "/" + runtime_id + "/current")
                h.record("actual default CRI Create wait returns Installing before kubelet timeout without workload code",
                         {"nodeUID": h.node_uids[worker], "podUID": pod_uid, "containerID": identifier,
                          "elapsedSeconds": elapsed, "kubeletRequestTimeoutSeconds": 120,
                          "startedAt": status["startedAt"], "noPublishedRoot": True}, ["OD-03"])
            finally:
                if identifier is not None:
                    cleanup_container(h, worker, identifier, name, pod_uid)
    finally:
        for path, digest in paths:
            require(h.node_exec_on(worker, "sha256sum", path).stdout.split()[0] == digest,
                    "cold RPC fixture file changed; refusing cleanup")
            h.node_exec_on(worker, "rm", "--", path)
