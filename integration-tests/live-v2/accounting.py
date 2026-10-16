"""RUN-08 real-node callback: execute(harness), before revocation and cleanup.

Requires the approved fixture-python runtime, its selected application delivery, a
private invocation-owned kind node, and crictl's real ContainerStats endpoint.
No fallback to file sizes, stdout counters, emptyDir, or skipped assertions.
"""

import copy
import json
import re
from datetime import datetime, timezone
from decimal import Decimal, InvalidOperation, ROUND_CEILING

from harness import Failure, OWNER, require, require_owned


MIB = 1024 * 1024
FIRST_BYTES = 16 * MIB
TOTAL_BYTES = 80 * MIB
LIMIT_BYTES = 48 * MIB
MIN_DELTA = FIRST_BYTES
DISK_RESERVE = 1024 * MIB
CONTAINER = "accounting"
IDLE = "import time; time.sleep(86400)"


class StatsPending(Failure):
    pass


PROBE = r"""
import json, os, stat
status = {}
for line in open('/proc/self/status'):
    key, _, value = line.partition(':')
    if key in ('Uid', 'CapEff', 'CapPrm', 'CapBnd', 'CapInh', 'CapAmb', 'NoNewPrivs'):
        status[key] = value.strip()
print(json.dumps({'mountinfo': open('/proc/self/mountinfo').read(),
                  'uid': os.getuid(), 'euid': os.geteuid(), 'status': status,
                  'tmpRealPath': os.path.realpath('/tmp'),
                  'rootDev': os.stat('/').st_dev, 'tmpDev': os.stat('/tmp').st_dev,
                  'tmpMode': stat.S_IMODE(os.stat('/tmp').st_mode)}))
"""
WRITE = r"""
import json, os, stat, sys, time
path, expected, target = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
assert path.startswith('/tmp/brewlet-accounting-') and '/' not in path[5:]
assert (expected, target) in ((0, 16777216), (16777216, 83886080))
flags = os.O_WRONLY | os.O_NOFOLLOW
flags |= (os.O_CREAT | os.O_EXCL) if expected == 0 else os.O_APPEND
fd = os.open(path, flags, 0o600)
try:
    before = os.fstat(fd)
    assert stat.S_ISREG(before.st_mode) and before.st_uid == os.getuid()
    assert before.st_size == expected and before.st_nlink == 1
    chunk = os.urandom(1024 * 1024)
    remaining = target - expected
    started = time.time_ns()
    while remaining:
        count = os.write(fd, chunk[:min(remaining, len(chunk))])
        assert count > 0
        remaining -= count
    os.fsync(fd)
    after = os.fstat(fd)
    assert after.st_size == target
    print(json.dumps({'path': path, 'size': after.st_size,
                      'allocatedBytes': after.st_blocks * 512,
                      'startedNS': started, 'finishedNS': time.time_ns()}), flush=True)
finally:
    os.close(fd)
"""


def now():
    return datetime.now(timezone.utc).isoformat()


def disk_quantity(value, capacity):
    require(isinstance(value, str), "disk threshold must be a quantity string")
    if value.endswith("%"):
        try:
            percentage = Decimal(value[:-1])
        except InvalidOperation as error:
            raise Failure("malformed disk threshold percentage") from error
        require(percentage.is_finite() and 0 <= percentage <= 100,
                "disk threshold percentage out of range")
        return int((Decimal(capacity) * percentage / 100).to_integral_value(rounding=ROUND_CEILING))
    match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)(Ki|Mi|Gi|Ti|K|M|G|T)?", value)
    require(match is not None, "unsupported disk threshold quantity: " + value)
    multipliers = {"": 1, "Ki": 1024, "Mi": 1024**2, "Gi": 1024**3, "Ti": 1024**4,
                   "K": 1000, "M": 1000**2, "G": 1000**3, "T": 1000**4}
    return int((Decimal(match[1]) * multipliers[match[2] or ""]).to_integral_value(rounding=ROUND_CEILING))


def required_headroom(capacity, configuration):
    require(type(capacity) is int and capacity > 0, "invalid filesystem capacity")
    require(isinstance(configuration, dict), "actual kubelet configuration unavailable")
    require(configuration.get("mergeDefaultEvictionSettings") is False,
            "effective merged eviction thresholds cannot be established")
    hard = configuration.get("evictionHard")
    soft = configuration.get("evictionSoft") or {}
    reclaim = configuration.get("evictionMinimumReclaim") or {}
    require(isinstance(hard, dict) and isinstance(soft, dict) and isinstance(reclaim, dict),
            "actual kubelet disk threshold maps unavailable")
    gc = configuration.get("imageGCHighThresholdPercent")
    require(type(gc) is int and 0 <= gc <= 100, "actual image GC threshold unavailable")
    signals = ("nodefs.available", "imagefs.available", "containerfs.available")
    floors = [disk_quantity(str(100 - gc) + "%", capacity)]
    for thresholds in (hard, soft):
        floors.extend(disk_quantity(thresholds[key], capacity) for key in signals if key in thresholds)
    minimum_reclaim = max([0, *(disk_quantity(reclaim[key], capacity)
                               for key in signals if key in reclaim)])
    return max(floors) + minimum_reclaim + DISK_RESERVE + TOTAL_BYTES


def container_id(pod):
    statuses = pod.get("status", {}).get("containerStatuses", [])
    matches = [s for s in statuses if s.get("name") == CONTAINER]
    require(len(matches) == 1, "exact accounting container status unavailable")
    value = matches[0].get("containerID", "")
    require(bool(re.fullmatch(r"containerd://[0-9a-f]{64}", value)),
            "accounting container is not a canonical containerd container")
    require(matches[0].get("restartCount") == 0, "accounting container restarted")
    return value.split("://", 1)[1]


def usage(stats, *, uid, namespace, pod_name, cid):
    """Select a real CRI ContainerStats row, never pod/volume/other-container usage."""
    require(isinstance(stats, dict), "real CRI ContainerStats response unavailable")
    rows = stats.get("stats")
    require(isinstance(rows, list), "real CRI ContainerStats list unavailable")
    matches = []
    for row in rows:
        require(isinstance(row, dict), "malformed CRI ContainerStats entry")
        attrs = row.get("attributes", {})
        require(isinstance(attrs, dict), "malformed CRI container attributes")
        labels = attrs.get("labels", {})
        require(isinstance(labels, dict), "malformed CRI container labels")
        if (attrs.get("id") == cid
                and attrs.get("metadata", {}).get("name") == CONTAINER
                and labels.get("io.kubernetes.pod.uid") == uid
                and labels.get("io.kubernetes.pod.namespace") == namespace
                and labels.get("io.kubernetes.pod.name") == pod_name):
            matches.append(row)
    require(len(matches) == 1, "CRI stats do not uniquely identify the exact owned container")
    layer = matches[0].get("writableLayer", {})
    require(isinstance(layer, dict), "CRI writable layer unavailable")
    wrapper = layer.get("usedBytes", {})
    require(isinstance(wrapper, dict), "CRI writableLayer.usedBytes unavailable")
    value = wrapper.get("value")
    # CRI clients serialize protobuf uint64 wrappers as either numbers or strings.
    require(type(value) is int or isinstance(value, str)
            and bool(re.fullmatch(r"0|[1-9][0-9]*", value)),
            "CRI writableLayer.usedBytes unavailable")
    value = int(value)
    require(0 <= value < 2**64, "CRI writableLayer.usedBytes out of range")
    stamp = layer.get("timestamp")
    require(isinstance(stamp, (str, int)) and str(stamp).isdigit(),
            "CRI writable layer sample has no timestamp")
    if int(stamp) == 0:
        raise StatsPending("CRI writable layer sample has not been collected yet")
    return {"usedBytes": value, "timestampNS": int(stamp),
            "fsId": layer.get("fsId")}


def increased(baseline, final):
    require(final["timestampNS"] >= baseline["timestampNS"], "CRI sample moved backwards")
    return (final["timestampNS"] > baseline["timestampNS"]
            and final["usedBytes"] - baseline["usedBytes"] >= MIN_DELTA)


def ephemeral_eviction(pod):
    status = pod.get("status", {})
    message = status.get("message", "").lower()
    return (status.get("phase") == "Failed" and status.get("reason") == "Evicted"
            and bool(re.search(r"\bephemeral(?:[ -]local)?[ -]storage\b", message))
            and "limit" in message
            and bool(re.search(r"\bexceed(?:s|ed)?\b", message))
            and "low on resource" not in message)


def guard_node(node, name, uid, run_id, *, healthy=True):
    require(name == "brewlet-v2-" + run_id + "-control-plane",
            "refusing a foreign Node name")
    require(node.get("metadata", {}).get("name") == name
            and node["metadata"].get("uid") == uid
            and not node["metadata"].get("deletionTimestamp"),
            "Node identity changed or is deleting")
    if healthy:
        conditions = {c["type"]: c["status"] for c in node.get("status", {}).get("conditions", [])}
        require(conditions.get("Ready") == "True", "Node is not Ready")
        require(conditions.get("DiskPressure") == "False", "Node DiskPressure is not explicitly False")


def guard_pod(pod, h, uid, cid=None):
    require_owned(pod, h.run_id, uid)
    require(pod["metadata"].get("namespace") == h.namespace
            and pod.get("spec", {}).get("nodeName") == h.node,
            "owned Pod namespace or Node changed")
    if cid is not None:
        require(container_id(pod) == cid, "accounting container was replaced")
    return pod


def mount_proof(probe, path):
    """Require /tmp to be on the containerd overlay upper, not a bind/tmpfs volume."""
    mounts = []
    for line in probe["mountinfo"].splitlines():
        left, right = line.split(" - ", 1)
        fields, filesystem = left.split(), right.split()
        mounts.append({"target": fields[4], "options": fields[5].split(","),
                       "type": filesystem[0], "source": filesystem[1],
                       "superOptions": filesystem[2].split(",")})
    roots = [m for m in mounts if m["target"] == "/"]
    require(len(roots) == 1, "unique root mount unavailable")
    root = roots[0]
    require(root["type"] == "overlay" and "rw" in root["options"],
            "root is not a writable overlay snapshot")
    upper = [o[9:] for o in root["superOptions"] if o.startswith("upperdir=")]
    require(len(upper) == 1 and re.fullmatch(
        r"/var/lib/containerd/io\.containerd\.snapshotter\.v1\.overlayfs/snapshots/[0-9]+/fs",
        upper[0]), "root upper is not a containerd-managed writable snapshot")
    require(not any(m["target"] != "/" and
                    (path == m["target"] or path.startswith(m["target"].rstrip("/") + "/")
                     or m["target"].startswith("/tmp/"))
                    for m in mounts), "/tmp or accounting file is volume-backed")
    app = [m for m in mounts if m["target"] == "/app"]
    require(len(app) == 1 and "ro" in app[0]["options"], "packaged /app is not read-only")
    require(probe["tmpRealPath"] == "/tmp" and probe["tmpDev"] == probe["rootDev"],
            "/tmp is redirected away from the root snapshot")
    require(probe["uid"] != 0 and probe["euid"] != 0 and probe["tmpMode"] == 0o1777,
            "nonroot identity or runtime /tmp mode violated")
    status = probe["status"]
    require(status.get("NoNewPrivs") == "1"
            and all(int(status.get(key, "-1"), 16) == 0
                    for key in ("CapEff", "CapPrm", "CapBnd", "CapInh", "CapAmb")),
            "capability or noNewPrivileges contract violated")
    return {"root": root, "app": app[0], "upperdir": upper[0],
            "uid": probe["uid"], "euid": probe["euid"], "tmpRealPath": probe["tmpRealPath"],
            "rootDev": probe["rootDev"], "tmpDev": probe["tmpDev"],
            "tmpMode": oct(probe["tmpMode"]), "status": status}


def workload_snapshot(pods, excluded_uid=None):
    result = {}
    for pod in pods:
        uid = pod["metadata"]["uid"]
        if uid == excluded_uid:
            continue
        status = pod.get("status", {})
        if status.get("phase") != "Running":
            continue
        result[uid] = {
            "namespace": pod["metadata"]["namespace"], "name": pod["metadata"]["name"],
            "containers": {s["name"]: {"id": s.get("containerID"), "ready": s.get("ready"),
                                      "restarts": s.get("restartCount")}
                           for s in status.get("containerStatuses", [])}}
    return result


def no_collateral(before, pods):
    after = workload_snapshot(pods)
    for uid, original in before.items():
        require(after.get(uid) == original, "unrelated running workload changed: "
                + original["namespace"] + "/" + original["name"])


def manifest(h, name):
    require(bool(re.fullmatch(r"[0-9a-f]{12}", h.run_id)), "invalid invocation ID")
    require("@sha256:" in h.app_images["python"], "digest-pinned Python fixture required")
    delivery = h.pod(name, "python", bound=True)["spec"]
    image = delivery["containers"][0]
    require(image["image"] == h.app_images["python"], "accounting delivery changed the selected image")
    result = {
        "apiVersion": "v1", "kind": "Pod",
        "metadata": {"name": name, "namespace": h.namespace, "labels": {OWNER: h.run_id},
                     "annotations": {"brewlet.sh/runtime-image": "fixture-python"}},
        "spec": {
            "runtimeClassName": "brewlet", "nodeName": h.node,
            "restartPolicy": "Never", "terminationGracePeriodSeconds": 1,
            "automountServiceAccountToken": False,
            "securityContext": {"runAsNonRoot": True, "runAsUser": 65532,
                                "runAsGroup": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
            "containers": [{
                "name": CONTAINER, "image": image["image"], "imagePullPolicy": image["imagePullPolicy"],
                "command": ["python3"], "args": ["-c", IDLE], "workingDir": "/app",
                "env": [{"name": "PYTHONDONTWRITEBYTECODE", "value": "1"}],
                "securityContext": {"readOnlyRootFilesystem": False, "privileged": False,
                                    "allowPrivilegeEscalation": False,
                                    "capabilities": {"drop": ["ALL"]}},
                "resources": {"requests": {"cpu": "10m", "memory": "32Mi",
                                          "ephemeral-storage": "32Mi"},
                              "limits": {"cpu": "500m", "memory": "128Mi",
                                         "ephemeral-storage": "48Mi"}}}]}}
    if "imagePullSecrets" in delivery:
        result["spec"]["imagePullSecrets"] = copy.deepcopy(delivery["imagePullSecrets"])
    return result


def cleanup(h, name, uid):
    pod = h.get("pod", name, h.namespace, absent=True)
    if pod is not None:
        # Still release only our Pod if a failed test found node pressure.
        guard_node(h.get("node", h.node), h.node, h.node_uid, h.run_id, healthy=False)
        guard_pod(pod, h, uid)
        require(h.owned.get(("pod", h.namespace, name)) == uid,
                "cleanup UID is not tracked by harness")
        # Harness.delete uses an atomic DeleteOptions UID precondition.
        h.delete("pod", name, h.namespace)

        def deleted():
            current = h.get("pod", name, h.namespace, absent=True)
            if current is None:
                return True
            guard_pod(current, h, uid)
            return False

        h.wait("accounting Pod UID deletion", deleted, timeout=90)


def execute(h):
    """Run actual CRI usage then kubelet eviction; raise Failure on any unavailable proof."""
    name = "accounting-" + h.run_id
    path = "/tmp/brewlet-accounting-" + h.run_id
    evidence = {"startedAt": now(), "node": h.node, "nodeUID": h.node_uid,
                "nodeContainerID": h.node_container, "namespace": h.namespace,
                "pod": name, "container": CONTAINER, "file": path,
                "firstBytes": FIRST_BYTES, "totalBytes": TOTAL_BYTES,
                "limitBytes": LIMIT_BYTES, "minimumDelta": MIN_DELTA,
                "samples": [], "writes": [], "outcome": "running"}
    uid = None
    before = None

    def persist():
        (h.work / "accounting-evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")

    def pods():
        return json.loads(h.kubectl("get", "pods", "-A", "--field-selector",
                                   "spec.nodeName=" + h.node, "-o", "json").stdout)["items"]

    def safety():
        node = h.get("node", h.node)
        guard_node(node, h.node, h.node_uid, h.run_id)
        if before is not None:
            no_collateral(before, pods())
        return node

    def get_pod(cid=None):
        safety()
        return guard_pod(h.get("pod", name, h.namespace), h, uid, cid)

    def exec_python(script, *args):
        get_pod(cid)
        command = ["exec", "-n", h.namespace, name, "-c", CONTAINER,
                   "--", "python3", "-c", script, *map(str, args)]
        return h.kubectl(*command, timeout=60)

    def sample():
        get_pod(cid)
        # node_exec fences the kind Docker container ID and invocation label.
        command = ["crictl", "stats", "-o", "json", cid]
        raw = json.loads(h.node_exec(*command, timeout=30).stdout)
        index = len(evidence["samples"])
        (h.work / f"accounting-cri-{index:03d}.json").write_text(json.dumps(raw, indent=2) + "\n")
        try:
            selected = usage(raw, uid=uid, namespace=h.namespace, pod_name=name, cid=cid)
        except StatsPending:
            evidence["samples"].append({"observedAt": now(), "source": "CRI ContainerStats",
                                        "command": command, "pending": True})
            persist()
            return None
        get_pod(cid)
        selected.update(observedAt=now(), source="CRI ContainerStats", command=command)
        evidence["samples"].append(selected)
        persist()
        return selected

    def headroom():
        safety()
        output = h.node_exec("df", "-B1", "--output=size,avail", "/var/lib/containerd").stdout
        capacity, free = map(int, output.splitlines()[-1].split())
        configuration = json.loads(h.kubectl(
            "get", "--raw", "/api/v1/nodes/" + h.node + "/proxy/configz").stdout)["kubeletconfig"]
        required = required_headroom(capacity, configuration)
        evidence["diskHeadroom"] = {"capacityBytes": capacity, "availableBytes": free,
                                    "requiredBytes": required, "reserveBytes": DISK_RESERVE,
                                    "evictionHard": configuration["evictionHard"],
                                    "evictionSoft": configuration.get("evictionSoft"),
                                    "imageGCHighThresholdPercent": configuration["imageGCHighThresholdPercent"],
                                    "observedAt": now()}
        require(free >= required,
                "insufficient safe node disk headroom; refusing bounded write")

    persist()
    (h.work / "accounting-probe.py").write_text(PROBE)
    (h.work / "accounting-write.py").write_text(WRITE)
    try:
        safety()
        headroom()
        before = workload_snapshot(pods())
        evidence["unrelatedBefore"] = before
        require(h.get("pod", name, h.namespace, absent=True) is None,
                "accounting Pod name already exists")
        pod = h.apply(manifest(h, name))
        uid = require_owned(pod, h.run_id)
        evidence["podUID"] = uid
        persist()

        def running():
            pod = get_pod()
            phase = pod.get("status", {}).get("phase")
            require(phase not in ("Failed", "Succeeded"), "accounting Pod ended before sampling")
            statuses = pod.get("status", {}).get("containerStatuses", [])
            return pod if phase == "Running" and any(
                s.get("name") == CONTAINER and s.get("state", {}).get("running")
                for s in statuses) else None

        pod = h.wait("accounting container Running", running, timeout=min(h.timeout, 240))
        cid = container_id(pod)
        evidence["containerID"] = cid
        admitted = [c for c in pod["spec"]["containers"] if c["name"] == CONTAINER]
        require(len(admitted) == 1 and
                admitted[0]["resources"]["limits"]["ephemeral-storage"] == "48Mi",
                "admitted accounting container ephemeral-storage limit changed")
        require(not admitted[0].get("volumeMounts") and not pod["spec"].get("volumes"),
                "accounting Pod must not have any injected or explicit volumes")
        evidence["mounts"] = mount_proof(json.loads(exec_python(PROBE).stdout), path)
        baseline = h.wait("first collected CRI writable-layer sample", sample,
                          timeout=min(h.timeout, 240))
        evidence["baseline"] = baseline
        require(baseline["usedBytes"] + FIRST_BYTES < LIMIT_BYTES,
                "baseline leaves no safe below-limit first-write window")

        def write(expected, target):
            headroom()
            get_pod(cid)
            command = ["exec", "-n", h.namespace, name, "-c", CONTAINER,
                       "--", "python3", "-c", WRITE, path, str(expected), str(target)]
            # The above-limit writer may be killed by the very eviction being
            # tested. Only a later exact kubelet eviction can justify that exit.
            result = h.kubectl(*command, timeout=60, check=expected == 0)
            output = json.loads(result.stdout) if result.returncode == 0 else None
            if output is not None:
                require(output["size"] == target and output["allocatedBytes"] >= target,
                        "bounded writer did not allocate the requested non-sparse file")
            evidence["writes"].append({"observedAt": now(), "command":
                                      ["python3", "-c", WRITE, path, str(expected), str(target)],
                                      "returncode": result.returncode,
                                      "diagnosticOnly": output,
                                      "error": result.stderr if result.returncode else None})
            persist()

        write(0, FIRST_BYTES)

        def accounted():
            pod = get_pod(cid)
            require(pod.get("status", {}).get("phase") == "Running",
                    "Pod evicted before below-limit CRI sample")
            final = sample()
            return final if final is not None and increased(baseline, final) else None

        final = h.wait("real CRI writable-layer delta >=16MiB", accounted,
                       timeout=min(h.timeout, 240))
        require(final["usedBytes"] < LIMIT_BYTES,
                "first sample already exceeds ephemeral-storage limit")
        evidence.update(final=final, deltaBytes=final["usedBytes"] - baseline["usedBytes"])
        write(FIRST_BYTES, TOTAL_BYTES)

        def evicted():
            pod = get_pod(cid)
            if pod.get("status", {}).get("phase") in ("Failed", "Succeeded"):
                require(ephemeral_eviction(pod),
                        "Pod terminated without local ephemeral-storage limit eviction")
                return pod
            return None

        pod = h.wait("kubelet ephemeral-storage limit eviction", evicted,
                     timeout=min(h.timeout, 600))
        evidence["eviction"] = {"observedAt": now(), "status": pod["status"]}
        evidence["unrelatedAfter"] = workload_snapshot(pods(), uid)
        safety()
        evidence["outcome"] = "passed"
    except BaseException as exc:
        evidence.update(outcome="failed", error=str(exc))
        raise
    finally:
        try:
            if uid is not None:
                pod = h.get("pod", name, h.namespace, absent=True)
                if pod is not None:
                    guard_pod(pod, h, uid)
                    events = h.kubectl("get", "events", "-n", h.namespace, "--field-selector",
                                       "involvedObject.uid=" + uid, "-o", "json").stdout
                    (h.work / "accounting-events.json").write_text(events)
                    logs = h.kubectl("logs", "-n", h.namespace, name, "-c", CONTAINER,
                                     "--tail=40", check=False)
                    (h.work / "accounting-container.log").write_text(logs.stdout + logs.stderr)
        except Exception as exc:
            evidence["diagnosticsError"] = str(exc)
        try:
            if uid is not None:
                cleanup(h, name, uid)
                evidence["cleanup"] = {"podUID": uid, "deletedAt": now()}
                safety()
        except Exception as exc:
            evidence.update(outcome="failed", cleanupError=str(exc))
            raise
        finally:
            evidence["finishedAt"] = now()
            persist()
    h.record("RUN-08 real CRI writable snapshot usage and ephemeral-storage eviction",
             evidence, ["RUN-08"])
    return evidence
