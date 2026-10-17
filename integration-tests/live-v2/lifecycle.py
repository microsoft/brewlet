"""Real lifecycle callback; call execute(h) after rotation, before revocation.

The parent exclusively schedules live resources. Importing this module does
nothing. The callback requires the harness's private node/profile, Python app
and before/after runtime sources, crictl, ctr and the installed maintenance shim.
It temporarily appends dedicated IDs to the *actual* profile, using conditional
UID/resourceVersion replacements. No authorization, lease, claim, current,
finalizer, clock, or cleanup receipt is manufactured or edited.

Expiry takes the production five-minute authorization lifetime in real time.
Configure positive production provisioner.gc.grace/provisioner.gc.sweepInterval
(recommended 20s/2s) before the callback. Fresh fenced worker recovery calls
Store.Recover through real installed-shim host evidence. Retired-generation GC
uses the provisioner's actual retirement timestamp and the configured grace.
An exact fixture-owned shim STOP/CONT induces real task-inspection uncertainty;
failed recovery must retain its durable lease/root, and only a new complete
recovery marker after CONT establishes recovery. No task-server reconstruction
framework is inferred from the lease recovery requirement.
The orphan case drives genuine CRI sandbox/container creation (not a Kubernetes
workload-controller test), with shared sandbox PID namespace, and actual shim
Exec RPCs. Containerd restart proves surviving-shim reconnection and durable
lease recovery, NOT reconstruction of a killed shim process.
"""

import copy
from datetime import datetime, timedelta, timezone
import json
import math
import re
import time
import uuid

from harness import API, Failure, OWNER, STORE, require, require_owned, validate_run_id


PREFIXES = ("epoch", "idle", "cold", "recovery")
IDLE_TTL = 30
RETAIN_SECONDS = 12
CID = re.compile(r"[0-9a-f]{64}")
UID = re.compile(r"[a-zA-Z0-9][a-zA-Z0-9-]{0,127}")
MAIN = "import time; time.sleep(1800)"


def fixture_id(run_id, purpose):
    require(purpose in PREFIXES, "unknown lifecycle fixture purpose")
    return "life-" + purpose + "-" + validate_run_id(run_id)


def epoch(value):
    require(isinstance(value, dict), "runtime identity is not an object")
    rid, uid, generation = (value.get(k) for k in ("id", "inventoryUID", "generation"))
    require(isinstance(rid, str) and bool(re.fullmatch(
        r"life-(epoch|idle|cold|recovery)-[0-9a-f]{12}", rid)),
        "refusing a non-lifecycle runtime")
    require(isinstance(uid, str) and UID.fullmatch(uid), "invalid inventory UID")
    require(type(generation) is int and generation > 0, "invalid runtime generation")
    return rid, uid, generation


def root_path(value):
    rid, uid, generation = epoch(value)
    return f"{STORE}/{rid}/{uid}/{generation}"


def barrier(value):
    rid, uid, generation = epoch(value)
    return {"id": rid, "inventoryUID": uid, "generation": generation}


def recreated(before, after):
    old, new = epoch(before), epoch(after)
    require(old[0] == new[0] and old[1] != new[1] and new[2] == 1,
            "same-name recreation must allocate a new UID and start at generation one")
    return barrier(before)


def timestamp(value):
    require(isinstance(value, str), "missing real authorization expiry")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise Failure("invalid real authorization expiry") from exc
    require(parsed.tzinfo is not None, "expiry must carry a timezone")
    return parsed


def duration_seconds(value):
    require(isinstance(value, str), "missing production GC duration")
    match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)(ms|s|m|h)", value)
    require(match is not None, "unsupported production GC duration: " + value)
    seconds = float(match[1]) * {"ms": 0.001, "s": 1, "m": 60, "h": 3600}[match[2]]
    require(math.isfinite(seconds) and seconds > 0, "GC timing must be finite and strictly positive")
    return seconds


def expiry_proof(before, after, observed_at, *, elapsed, stopped):
    require(stopped is True, "renewal was not actually stopped")
    require(before == after, "authorization changed while renewal was stopped")
    epoch(before)
    require(not before.get("revoked"), "revocation is not authorization expiry")
    require(observed_at.tzinfo is not None and observed_at >= timestamp(before.get("expiresAt")),
            "authorization has not actually expired")
    require(elapsed >= 1, "no real stopped-renewal interval observed")
    return {"authorization": after, "observedAt": observed_at.isoformat(),
            "realElapsedSeconds": elapsed, "renewalStopped": True}


def profile_update(profile, run_id, uid, expected, changes):
    """Build a conditional full-object replace without adopting concurrent edits."""
    require_owned(profile, run_id, uid)
    require(profile["metadata"].get("resourceVersion"), "profile has no resourceVersion")
    require(not profile["metadata"].get("deletionTimestamp"), "profile is deleting")
    result = copy.deepcopy(profile)
    entries = result["spec"]["runtimeImages"]
    existing = {entry["id"]: entry for entry in entries}
    require(len(existing) == len(entries), "duplicate profile runtime IDs")
    for rid, previous in expected.items():
        require(existing.get(rid) == previous, "lifecycle entry changed concurrently: " + rid)
    for rid, replacement in changes.items():
        require(rid == fixture_id(run_id, rid.split("-")[1]),
                "profile edit is not invocation-scoped")
        require(rid in expected, "profile edit lacks a prior observation")
        entries[:] = [entry for entry in entries if entry["id"] != rid]
        if replacement is not None:
            require(replacement.get("id") == rid, "replacement ID changed")
            entries.append(copy.deepcopy(replacement))
    return result


def cid(value):
    require(isinstance(value, str) and CID.fullmatch(value), "invalid exact CRI container ID")
    return value


def task_command(action, container, *, exec_id=None):
    container = cid(container)
    if action == "terminate-main":
        require(exec_id is None, "main signal cannot name an exec")
        return ["ctr", "-n", "k8s.io", "tasks", "kill", "--signal", "SIGTERM", container]
    if action == "terminate-exec":
        require(isinstance(exec_id, str) and re.fullmatch(r"life-exec-[0-9a-f]{12}", exec_id),
                "invalid invocation-owned exec ID")
        return ["ctr", "-n", "k8s.io", "tasks", "kill", "--exec-id", exec_id,
                "--signal", "SIGTERM", container]
    raise Failure("unsupported lifecycle task mutation")


def exec_command(container, exec_id):
    cid(container)
    require(isinstance(exec_id, str) and re.fullmatch(r"life-exec-[0-9a-f]{12}", exec_id),
            "invalid invocation-owned exec ID")
    return ["ctr", "-n", "k8s.io", "tasks", "exec", "--exec-id", exec_id,
            "--detach", "--user", "65532:65532", container, "python3", "-c", MAIN]


def signal_command(pid, start, executable, action):
    require(type(pid) is int and pid > 1, "unsafe process PID")
    require(isinstance(start, str) and start.isdigit(), "missing process start identity")
    require(executable == "/brewlet-node", "only the owned provisioner may be paused")
    require(action in ("STOP", "CONT"), "unsupported process signal")
    # stat's comm can contain spaces; field 20 after the final ')' is starttime.
    script = (
        'test "$(readlink /proc/$1/exe)" = "$3"; '
        's=$(cat /proc/$1/stat); s=${s##*) }; '
        'test "$(printf "%s\\n" "$s" | awk \'{print $20}\')" = "$2"; '
        'kill -"$4" "$1"')
    return ["sh", "-ec", script, "lifecycle-worker", str(pid), start, executable, action]


def daemon_guard(pid, start, executable):
    require(type(pid) is int and pid > 1, "unsafe containerd PID")
    require(isinstance(start, str) and start.isdigit(), "missing daemon start identity")
    require(executable in ("/usr/bin/containerd", "/usr/local/bin/containerd", "/bin/containerd"),
            "unexpected containerd executable")
    return (
        'test "$(systemctl show containerd --property=MainPID --value)" = "$1"; '
        'test "$(readlink /proc/$1/exe)" = "$3"; '
        's=$(cat /proc/$1/stat); s=${s##*) }; '
        'test "$(printf "%s\\n" "$s" | awk \'{print $20}\')" = "$2"; ')


def daemon_restart_command(pid, start, executable):
    script = daemon_guard(pid, start, executable) + 'systemctl restart containerd'
    return ["sh", "-ec", script, "lifecycle-daemon", str(pid), start, executable]


def daemon_signal_command(pid, start, executable, action):
    require(action in ("STOP", "CONT"), "unsupported daemon signal")
    script = daemon_guard(pid, start, executable) + 'kill -"$4" "$1"'
    return ["sh", "-ec", script, "lifecycle-daemon", str(pid), start, executable, action]


def kubelet_signal_command(pid, start, executable, action):
    require(type(pid) is int and pid > 1, "unsafe kubelet PID")
    require(isinstance(start, str) and start.isdigit(), "missing kubelet start identity")
    require(executable in ("/usr/bin/kubelet", "/usr/local/bin/kubelet", "/bin/kubelet"),
            "unexpected kubelet executable")
    require(action in ("STOP", "CONT"), "unsupported kubelet signal")
    script = (
        'test "$(systemctl show kubelet --property=MainPID --value)" = "$1"; '
        'test "$(readlink /proc/$1/exe)" = "$3"; '
        's=$(cat /proc/$1/stat); s=${s##*) }; '
        'test "$(printf "%s\\n" "$s" | awk \'{print $20}\')" = "$2"; '
        'kill -"$4" "$1"')
    return ["sh", "-ec", script, "lifecycle-kubelet", str(pid), start, executable, action]


def orphan_signal_command(pid, start, executable):
    require(type(pid) is int and pid > 1, "unsafe orphan exec PID")
    require(isinstance(start, str) and start.isdigit(), "missing orphan start identity")
    require(isinstance(executable, str) and executable.startswith("/") and "\0" not in executable,
            "missing orphan executable identity")
    script = (
        'test "$(readlink /proc/$1/exe)" = "$3"; '
        's=$(cat /proc/$1/stat); s=${s##*) }; '
        'test "$(printf "%s\\n" "$s" | awk \'{print $20}\')" = "$2"; '
        'kill -TERM "$1"')
    return ["sh", "-ec", script, "lifecycle-orphan", str(pid), start, executable]


def validate_lease(lease, identity, container):
    require(lease.get("taskID") == cid(container) and lease.get("namespace") == "k8s.io",
            "lease is not the exact CRI task")
    require(epoch(lease.get("runtime")) == epoch(identity), "lease changed inventory epoch/generation")
    require(lease["runtime"].get("sourceDigest") == identity.get("sourceDigest"),
            "lease source digest changed")
    return lease


def retained_refs(lease, identity, container, *, main_exited, exec_pids, mountinfo):
    validate_lease(lease, identity, container)
    require(type(main_exited) is bool, "initial process state unavailable")
    require(isinstance(exec_pids, list) and exec_pids
            and all(type(p) is int and p > 1 for p in exec_pids),
            "no genuine live exec")
    root = root_path(identity)
    require(any(re.search(re.escape(root + "/rootfs") + r"(?=$|[/,: \t\n])", text)
                for text in mountinfo),
            "live exec mount namespace does not reference the exact runtime root")
    return {"lease": lease, "execPIDs": exec_pids, "root": root,
            "mainExited": main_exited, "mountReferenced": True}


def recovery_proof(report, identity, container, *, retained):
    """Validate a fresh maintenance process's observation, not an inferred refcount."""
    require(isinstance(report, dict) and not report.get("error") and report.get("complete") is True,
            "real maintenance recovery reported uncertainty/error")
    snapshots = {}
    for key in ("retainedTasks", "removedTasks"):
        require(key in report, "maintenance recovery omitted " + key)
        # Go's nil slices encode as null; complete=True makes these empty sets,
        # not an omitted or unavailable observation.
        rows = [] if report[key] is None else report[key]
        require(isinstance(rows, list) and all(
            isinstance(row, str) and CID.fullmatch(row) for row in rows),
            "malformed recovery task identity set")
        require(len(set(rows)) == len(rows), "ambiguous duplicate recovery task identity")
        snapshots[key] = rows
    require(not set(snapshots["retainedTasks"]) & set(snapshots["removedTasks"]),
            "recovery reported the same task retained and removed")
    epoch(identity)
    cid(container)
    if retained:
        require(container in snapshots["retainedTasks"],
                "fresh recovery did not retain the genuinely referenced task lease")
    else:
        require(container in snapshots["removedTasks"],
                "recovery did not observe removal of the drained task lease")
    return {"taskID": container, "identity": barrier(identity),
            "retained": retained, "observation": report,
            "proofBoundary": "fresh fenced worker recovery; installed-shim real task/mount evidence"}


class Lifecycle:
    def __init__(self, h):
        self.h = h
        validate_run_id(h.run_id)
        self.profile_uid = h.owned[("nodeprofile", "", h.profile)]
        self.entries = {}
        self.inventories = {}
        self.pods = {}
        self.cri = None
        self.cri_directory = None

    def guard(self):
        node = self.h.node_snapshot()
        require(node["metadata"]["uid"] == self.h.node_uid, "Node UID changed")
        labels = node["metadata"].get("labels", {})
        require(labels.get("brewlet.sh/owner-uid") == self.profile_uid
                and labels.get("brewlet.sh/owner-node-uid") == self.h.node_uid,
                "lifecycle lost its real profile/Node claim")
        return node

    def read(self, path):
        return json.loads(self.h.node_exec("cat", path).stdout)

    def exists(self, path):
        response = self.h.node_exec("test", "-e", path, check=False)
        require(response.returncode in (0, 1), "root existence inspection failed")
        return response.returncode == 0

    def mounts_gone(self, identity):
        self.guard()
        response = json.loads(self.h.node_exec(
            "nsenter", "--mount=/proc/1/ns/mnt", "--root=/proc/1/root", "--wd=/",
            "--", "/usr/local/bin/containerd-shim-brewlet-v2", "--brewlet-evidence",
            input=json.dumps({"path": root_path(identity)})).stdout)
        require(not response.get("error") and type(response.get("gone")) is bool,
                "complete production host mount inspection failed: " + repr(response))
        return response["gone"]

    def current(self, rid):
        obj = self.read(f"{STORE}/{rid}/current/metadata.json")
        require(epoch(obj)[0] == rid, "current resolved another runtime ID")
        return obj

    def auth(self, rid):
        obj = self.read(f"{STORE}/{rid}/authorization.json")
        require(epoch(obj)[0] == rid and obj.get("profileUID") == self.profile_uid,
                "authorization not from the actual profile")
        return obj

    def edit_profile(self, changes):
        self.guard()
        expected = {rid: self.entries.get(rid) for rid in changes}
        updated = self.replace_owned("nodeprofile", self.h.profile, self.profile_uid,
            lambda current: profile_update(current, self.h.run_id, self.profile_uid,
                                           expected, changes))
        for rid, entry in changes.items():
            if entry is None:
                self.entries.pop(rid, None)
            else:
                self.entries[rid] = copy.deepcopy(entry)
        generation = updated["metadata"]["generation"]
        self.h.wait("actual profile generation " + str(generation), lambda:
            self.h.get("nodeprofile", self.h.profile).get("status", {}).get(
                "provisioningGeneration") == generation)
        return generation

    def replace_owned(self, kind, name, uid, build):
        for attempt in range(6):
            self.guard()
            current = self.h.get(kind, name)
            require_owned(current, self.h.run_id, uid)
            require(current["metadata"].get("resourceVersion"), "missing conditional update version")
            require(not current["metadata"].get("deletionTimestamp"), "update target is deleting")
            candidate = build(current)
            response = self.h.kubectl("replace", "-f", "-", "-o", "json",
                                      input=json.dumps(candidate), check=False)
            if response.returncode == 0:
                updated = json.loads(response.stdout)
                require_owned(updated, self.h.run_id, uid)
                return updated
            require("(Conflict)" in response.stderr,
                    "conditional lifecycle update failed: " + response.stderr[-2000:])
            if attempt < 5:
                time.sleep(0.2)
        raise Failure("conditional lifecycle update repeatedly conflicted; no ownership bypass")

    def inventory(self, purpose, revision="before", *, install="OnDemand", ttl=None):
        rid = fixture_id(self.h.run_id, purpose)
        require(self.h.get("runtimeimage", rid, absent=True) is None,
                "lifecycle inventory name already exists")
        key = ("runtimeimage", "", rid)
        # A completed, UID-bound deletion is the only allowed recreation.
        if key in self.h.owned:
            require(rid in self.inventories, "unrecognized inventory ownership history")
            require(self.inventories[rid].get("deleted") is True,
                    "inventory was not genuinely finalized")
            del self.h.owned[key]
        obj = self.h.apply({"apiVersion": API, "kind": "RuntimeImage",
            "metadata": {"name": rid},
            "spec": {"source": {"image": self.h.runtime_images["python"][revision]},
                     "user": {"uid": 65532, "gid": 65532}}})
        self.inventories[rid] = {"uid": obj["metadata"]["uid"], "deleted": False}
        self.h.wait("approved dedicated runtime " + rid, lambda:
            any(c.get("type") == "Approved" and c.get("status") == "True"
                for c in self.h.get("runtimeimage", rid).get("status", {}).get("conditions", [])))
        if rid not in self.entries:
            entry = {"id": rid, "install": install}
            if ttl is not None:
                entry["idleTTL"] = str(ttl) + "s"
            self.edit_profile({rid: entry})
        self.h.wait("dedicated runtime authorized " + rid, lambda:
            self.h.advertised(rid, "ready" if install == "Eager" else "available"))
        return rid

    def rotate(self, rid, revision):
        self.guard()
        obj = self.h.get("runtimeimage", rid)
        require_owned(obj, self.h.run_id, self.inventories[rid]["uid"])
        previous = obj["status"]["generation"]
        old_source = obj["spec"]["source"]["image"]
        def change_source(current):
            require(current["spec"]["source"]["image"] == old_source,
                    "inventory source changed concurrently")
            current["spec"]["source"]["image"] = self.h.runtime_images["python"][revision]
            return current
        self.replace_owned("runtimeimage", rid, self.inventories[rid]["uid"], change_source)
        return self.h.wait("dedicated runtime source rotation", lambda:
            (value if value.get("status", {}).get("generation", 0) > previous else False)
            if (value := self.h.get("runtimeimage", rid)) else False)

    def pod(self, rid, suffix, *, minimum=None):
        name = "life-" + suffix + "-" + self.h.run_id
        obj = self.h.pod(name, "python", bound=True, barrier=minimum)
        obj["metadata"]["annotations"]["brewlet.sh/runtime-image"] = rid
        container = obj["spec"]["containers"][0]
        container.update(command=["python3"], args=["-c", MAIN], imagePullPolicy="IfNotPresent")
        container.pop("readinessProbe", None)
        self.h.apply(obj)
        self.pods[name] = self.h.owned[("pod", self.h.namespace, name)]
        return name

    def ready(self, name):
        obj = self.h.wait("dedicated live task " + name, lambda: self.h.ready(name))
        lease = self.h.lease(obj)
        identity = lease["runtime"]
        validate_lease(lease, identity, lease["taskID"])
        require(identity["id"] in self.entries, "task did not use a dedicated lifecycle ID")
        require(self.inventories[identity["id"]]["uid"] == identity["inventoryUID"],
                "task did not use current inventory epoch")
        return obj, lease

    def drain_pod(self, name, lease):
        require(name in self.pods, "cannot drain another fixture's Pod")
        obj = self.h.get("pod", name, self.h.namespace)
        require_owned(obj, self.h.run_id, self.pods[name])
        validate_lease(self.h.lease(obj), lease["runtime"], lease["taskID"])
        self.h.delete("pod", name, self.h.namespace)
        self.h.wait("UID-scoped lifecycle Pod removal", lambda:
            self.h.get("pod", name, self.h.namespace, absent=True) is None)
        self.h.wait("real task and mount lease drains", lambda:
            not self.exists(f"{STORE}/.tasks/{lease['taskID']}/lease.json"))
        del self.pods[name]

    def revoke(self, rid):
        self.guard()
        obj = self.h.get("runtimeimage", rid)
        require_owned(obj, self.h.run_id, self.inventories[rid]["uid"])
        self.h.delete("runtimeimage", rid)
        self.h.wait("real provisioner revocation tombstone", lambda: self.auth(rid).get("revoked"))

    def finalize(self, rid, old):
        require(epoch(old)[0] == rid and old["inventoryUID"] == self.inventories[rid]["uid"],
                "cleanup is not scoped to the recorded inventory UID")
        self.h.wait("genuine inventory finalization " + rid, lambda:
            self.h.get("runtimeimage", rid, absent=True) is None)
        require(not self.exists(root_path(old)), "finalization retained an old-epoch root")
        require(not self.exists(f"{STORE}/{rid}/{old['inventoryUID']}"),
                "finalization retained another generation in the old epoch")
        require(not self.exists(f"{STORE}/{rid}/current"), "finalization retained current")
        self.inventories[rid]["deleted"] = True

    def inventory_epoch(self):
        rid = self.inventory("epoch")
        name = self.pod(rid, "epoch-old")
        obj, lease = self.ready(name)
        old = lease["runtime"]
        self.revoke(rid)
        started = time.monotonic()
        while time.monotonic() - started < RETAIN_SECONDS:
            inventory = self.h.get("runtimeimage", rid)
            require_owned(inventory, self.h.run_id, old["inventoryUID"])
            require(inventory["metadata"].get("deletionTimestamp")
                    and inventory["metadata"].get("finalizers"),
                    "inventory deletion bypassed a real active task lease")
            validate_lease(self.h.lease(self.h.get("pod", name, self.h.namespace)),
                           old, lease["taskID"])
            require(self.exists(root_path(old) + "/rootfs"), "live leased root was collected")
            self.h.exec(name, ["python3", "-c", "print('lease-alive')"])
            time.sleep(2)
        self.h.record("inventory deletion is blocked by a real UID-bound task lease",
                      {"inventoryUID": old["inventoryUID"], "lease": lease,
                       "podUID": obj["metadata"]["uid"]}, ["INV-05", "LEASE-01"])
        self.drain_pod(name, lease)
        self.finalize(rid, old)
        self.h.record("inventory deletion genuinely cleans roots and finalizes after drain",
                      {"oldEpoch": old, "rootGone": True}, ["INV-05", "OWN-02"])
        self.inventory("epoch", "after")
        name = self.pod(rid, "epoch-new")
        _, new_lease = self.ready(name)
        new = new_lease["runtime"]
        minimum = recreated(old, new)
        require(not self.exists(root_path(old)), "new epoch adopted an old-epoch root")
        # Bound and bypass admission to prove the actual shim rejection.
        self.h.bypass_admission()
        try:
            rejected = self.pod(rid, "old-epoch-barrier", minimum=minimum)
            self.h.no_start(rejected, "RuntimeImageEpochMismatch", timeout=180)
            self.pods.pop(rejected)
        finally:
            self.h.restore_admission()
        self.h.record("same-name new UID generation one rejects genuine old-epoch barrier",
                      {"old": old, "new": new, "barrier": minimum}, ["INV-04", "ROT-07"])
        self.drain_pod(name, new_lease)
        self.revoke(rid)
        self.finalize(rid, new)
        self.edit_profile({rid: None})

    def worker(self, *, allow_starting=False, predecessor=None):
        self.guard()
        profile = self.h.get("nodeprofile", self.h.profile)
        require_owned(profile, self.h.run_id, self.profile_uid)
        candidates = []
        for pod in self.h.get("pods", namespace=self.h.control_namespace)["items"]:
            if pod.get("spec", {}).get("nodeName") != self.h.node:
                continue
            for container in pod["spec"].get("containers", []):
                env = {v["name"]: v.get("value") for v in container.get("env", [])}
                if (env.get("BREWLET_PROFILE_UID") == self.profile_uid
                        and env.get("BREWLET_PROFILE_GENERATION") == str(profile["metadata"]["generation"])
                        and env.get("BREWLET_MODE") == "provisioning"):
                    candidates.append((pod, container))
        if allow_starting and not candidates:
            return False
        require(len(candidates) == 1, "could not select the exact current provisioner")
        pod, container = candidates[0]
        settings = {v["name"]: v.get("value") for v in container.get("env", [])
                    if v["name"] in ("BREWLET_GC_GRACE", "BREWLET_GC_SWEEP_INTERVAL")}
        owners = [o for o in pod["metadata"].get("ownerReferences", [])
                  if o.get("kind") == "DaemonSet" and o.get("controller")]
        require(len(owners) == 1, "worker has no exact DaemonSet owner")
        ds = self.h.get("daemonset", owners[0]["name"], self.h.control_namespace)
        require(ds["metadata"]["uid"] == owners[0]["uid"] and any(
            o.get("kind") == "NodeProfile" and o.get("uid") == self.profile_uid
            for o in ds["metadata"].get("ownerReferences", [])), "worker ownership chain changed")
        status = [s for s in pod.get("status", {}).get("containerStatuses", [])
                  if s.get("name") == container["name"] and "running" in s.get("state", {})]
        if allow_starting and not status:
            return False
        require(len(status) == 1, "worker is not actually running")
        container_id = cid(status[0]["containerID"].removeprefix("containerd://"))
        if predecessor is not None and container_id == predecessor:
            return False
        detail = self.inspect(container_id, pod_uid=pod["metadata"]["uid"])
        pid = detail["info"].get("pid")
        require(type(pid) is int and pid > 1, "worker host PID unavailable")
        self.h.node_exec("test", "/brewlet-node", "=",
                         self.h.node_exec("readlink", f"/proc/{pid}/exe").stdout.strip())
        stat = self.h.node_exec("cat", f"/proc/{pid}/stat").stdout
        start = stat.rsplit(") ", 1)[1].split()[19]
        return {"cid": container_id, "pid": pid, "start": start,
                "podUID": pod["metadata"]["uid"], "pod": pod["metadata"]["name"],
                "gcSettings": settings}

    def inspect(self, container, *, pod_uid):
        self.guard()
        detail = json.loads(self.h.node_exec("crictl", "inspect", cid(container)).stdout)
        status = detail.get("status", {})
        require(status.get("id") == container
                and status.get("labels", {}).get("io.kubernetes.pod.uid") == pod_uid
                and status["labels"].get("io.kubernetes.pod.namespace") in (
                    self.h.namespace, self.h.control_namespace),
                "CRI container/Pod UID identity changed")
        return detail

    def worker_stopped(self, worker):
        self.inspect(worker["cid"], pod_uid=worker["podUID"])
        stat = self.h.node_exec("cat", f"/proc/{worker['pid']}/stat").stdout
        fields = stat.rsplit(") ", 1)[1].split()
        require(fields[19] == worker["start"], "worker PID was reused")
        return fields[0] in ("T", "t")

    def expiry_and_recovery(self):
        rid = self.inventory("recovery")
        name = self.pod(rid, "expiry-held")
        obj, lease = self.ready(name)
        worker = self.worker()
        self.h.node_exec(*signal_command(worker["pid"], worker["start"], "/brewlet-node", "STOP"))
        started = time.monotonic()
        try:
            require(self.worker_stopped(worker), "SIGSTOP did not stop actual renewal")
            initial = self.auth(rid)
            deadline = timestamp(initial["expiresAt"])
            require(deadline > datetime.now(timezone.utc), "authorization was already expired")
            source_target = self.rotate(rid, "after")
            target_generation = source_target["status"]["generation"]
            budget = max(self.h.timeout, (deadline - datetime.now(timezone.utc)).total_seconds() + 60)
            def expired():
                require(self.worker_stopped(worker), "renewal unexpectedly resumed")
                observed = self.auth(rid)
                require(initial == observed, "stopped worker still renewed authorization")
                return datetime.now(timezone.utc) >= deadline
            self.h.wait("real authorization wall-clock expiry with stopped renewal", expired,
                        timeout=budget)
            proof = expiry_proof(initial, self.auth(rid), datetime.now(timezone.utc),
                                 elapsed=time.monotonic() - started, stopped=self.worker_stopped(worker))
            validate_lease(self.h.lease(self.h.get("pod", name, self.h.namespace)),
                           lease["runtime"], lease["taskID"])
            self.h.exec(name, ["python3", "-c", "print('old-task-survived-expiry')"])
            self.h.bypass_admission()
            try:
                rejected = self.pod(rid, "expired-bound")
                self.h.no_start(rejected, "RuntimeImageAuthorizationExpired", timeout=180)
                self.pods.pop(rejected)
            finally:
                self.h.restore_admission()
            self.h.record("real stopped renewal expires authorization and bound starts fail closed",
                          proof, ["INV-06"])
        finally:
            # PID, starttime, executable, CID and Pod UID must all still match.
            self.inspect(worker["cid"], pod_uid=worker["podUID"])
            self.h.node_exec(*signal_command(worker["pid"], worker["start"], "/brewlet-node", "CONT"))
        renewed = self.h.wait("fresh real inventory authorization after renewal resumes", lambda:
            (a if timestamp(a["expiresAt"]) > deadline and not a.get("revoked")
             and a["generation"] == target_generation
             and a["digest"] == self.h.runtime_images["python"]["after"].split("@")[1] else False)
            if (a := self.auth(rid)) else False)
        self.h.wait("latest rotated runtime after renewal recovery", lambda:
            self.h.advertised(rid, "ready", target_generation))
        recovered_name = self.pod(rid, "renewal-recovered")
        _, recovered_lease = self.ready(recovered_name)
        require(recovered_lease["runtime"]["inventoryUID"] == lease["runtime"]["inventoryUID"]
                and recovered_lease["runtime"]["generation"] == target_generation
                and recovered_lease["runtime"]["sourceDigest"] == renewed["digest"],
                "recovered start did not use the newly approved source generation")
        validate_lease(self.h.lease(self.h.get("pod", name, self.h.namespace)),
                       lease["runtime"], lease["taskID"])
        self.h.record("fresh inventory checks recover renewal without replacing running tasks",
                      {"renewed": renewed, "heldLease": lease,
                       "newLease": recovered_lease, "sourceTarget": source_target["status"]},
                      ["INV-06", "LEASE-02"])
        self.drain_pod(recovered_name, recovered_lease)
        maintenance = self.reconnect_and_worker_recovery(name, obj, lease)
        self.drain_pod(name, lease)
        self.h.wait("real production GC collects the drained retired generation", lambda:
            not self.exists(root_path(lease["runtime"])))
        require(self.mounts_gone(lease["runtime"]),
                "retired root collection raced a remaining actual mount")
        current = self.current(rid)
        require(epoch(current) == epoch(recovered_lease["runtime"])
                and self.exists(root_path(current) + "/rootfs"),
                "generation GC removed or changed the current approved runtime")
        require(not self.auth(rid).get("revoked"),
                "retired generation GC was actually inventory revocation cleanup")
        self.h.record("positive real GC grace retains recovered live leases then collects after actual drain",
                      {"retired": lease["runtime"], "current": current,
                       "maintenance": maintenance, "oldRootGone": True}, ["LEASE-01", "LEASE-02"])
        self.revoke(rid)
        self.finalize(rid, lease["runtime"])
        self.edit_profile({rid: None})

    def reconnect_and_worker_recovery(self, name, original, lease):
        worker = self.worker()
        timing = self.gc_timing(worker)
        rid, uid, generation = epoch(lease["runtime"])
        retirement_path = f"{STORE}/{rid}/.retirements/{uid}-{generation}.json"
        self.h.wait("real provisioner retirement record", lambda: self.exists(retirement_path))
        retirement = self.read(retirement_path)
        require(retirement.get("inventoryUID") == uid and retirement.get("generation") == generation,
                "retirement record belongs to another inventory epoch/generation")
        eligible = timestamp(retirement.get("retiredAt")) + timedelta(seconds=timing["graceSeconds"])
        self.h.wait("actual positive retirement grace elapses", lambda:
            datetime.now(timezone.utc) >= eligible)
        maintenance = self.restart_and_recover(lease)
        successor = maintenance["successor"]
        self.guard()
        before = self.h.node_exec("systemctl", "show", "containerd", "--property=MainPID",
                                  "--value").stdout.strip()
        require(before.isdigit() and int(before) > 1, "containerd service PID unavailable")
        executable = self.h.node_exec("readlink", f"/proc/{before}/exe").stdout.strip()
        stat = self.h.node_exec("cat", f"/proc/{before}/stat").stdout
        start = stat.rsplit(") ", 1)[1].split()[19]
        self.guard()
        self.h.node_exec(*daemon_restart_command(int(before), start, executable), timeout=90)
        after = self.h.node_exec("systemctl", "show", "containerd", "--property=MainPID",
                                 "--value").stdout.strip()
        require(after.isdigit() and int(after) > 1 and after != before,
                "containerd daemon did not actually restart")
        self.h.wait("containerd task service reconnect", lambda:
            self.h.node_exec("crictl", "info", check=False).returncode == 0)
        recovered = self.h.wait("same surviving task after daemon restart",
                                lambda: self.h.ready(name))
        require(recovered["metadata"]["uid"] == original["metadata"]["uid"]
                and recovered["status"]["containerStatuses"][0]["containerID"]
                == original["status"]["containerStatuses"][0]["containerID"],
                "restart replaced rather than recovered the actual task")
        validate_lease(self.h.lease(recovered), lease["runtime"], lease["taskID"])
        result = self.h.exec(name, ["python3", "-c",
            "import os,json;print(json.dumps({'uid':os.getuid(),'home':os.environ['HOME'],"
            "'revision':os.environ['BREWLET_RUNTIME_REVISION']}))"])
        probe = json.loads(result.stdout)
        require(probe == {"uid": 65532, "home": "/home/app", "revision": "before"},
                "recovered Exec lost protected initial state")
        self.h.record("real worker restart and containerd reconnect preserve task/lease/Exec state",
                      {"oldWorker": worker, "newWorker": successor, "lease": lease,
                       "containerdBefore": before, "containerdAfter": after,
                       "boundary": "surviving task-server reconnect; maintenance lease recovery is separate"})
        uncertainty = self.uncertain_runtime_evidence(name, lease)
        return {"timing": timing, "retirement": retirement,
                "observedAfterGraceAt": datetime.now(timezone.utc).isoformat(),
                "recovery": maintenance, "uncertainty": uncertainty}

    def gc_timing(self, worker):
        settings = worker["gcSettings"]
        grace = duration_seconds(settings.get("BREWLET_GC_GRACE", "1h"))
        interval = duration_seconds(settings.get("BREWLET_GC_SWEEP_INTERVAL", "1m"))
        require(grace + 2 * interval < self.h.timeout,
                "parent must configure positive production BREWLET_GC_GRACE and "
                "BREWLET_GC_SWEEP_INTERVAL within the assertion budget (recommended 20s/2s); "
                "default one-hour grace is not bypassed")
        return {"graceSeconds": grace, "sweepSeconds": interval,
                "observedWorkerCID": worker["cid"]}

    def restart_and_recover(self, lease):
        """Fresh worker recovery uses the installed shim's real host evidence."""
        identity, container = lease["runtime"], lease["taskID"]
        validate_lease(self.read(f"{STORE}/.tasks/{container}/lease.json"), identity, container)
        before_pids = self.task_pids(container)
        require(before_pids, "recovery fixture has no actual task or exec process")
        mountinfo = [self.h.node_exec("cat", f"/proc/{pid}/mountinfo").stdout for pid in before_pids]
        require(any(re.search(re.escape(root_path(identity) + "/rootfs") + r"(?=$|[/,: \t\n])",
                              text) for text in mountinfo),
                "actual task/exec process does not mount its leased runtime root")
        require(not self.mounts_gone(identity), "recovery fixture has no actual live root mount")
        worker = self.worker()
        self.inspect(worker["cid"], pod_uid=worker["podUID"])
        self.h.node_exec("crictl", "stop", "--timeout", "10", worker["cid"])
        successor = self.h.wait("actual provisioner container restart", lambda:
            (value if value["cid"] != worker["cid"] else False)
            if (value := self.worker_if_running(worker["cid"])) else False)
        report = self.h.wait("fresh worker successfully recovers the exact durable task lease", lambda:
            self.recovery_log(successor, lease))
        validate_lease(self.read(f"{STORE}/.tasks/{container}/lease.json"), identity, container)
        after_pids = self.task_pids(container)
        require(set(before_pids) & set(after_pids),
                "fresh recovery replaced rather than retained actual task/exec processes")
        require(self.exists(root_path(identity) + "/rootfs") and not self.mounts_gone(identity),
                "fresh recovery/collection discarded a genuinely mounted leased root")
        return {"predecessor": worker, "successor": successor,
                "taskID": container, "lease": lease,
                "processesBefore": before_pids, "processesAfter": after_pids,
                "successfulRecovery": report,
                "proofBoundary": "fresh fenced worker recovery + installed maintenance shim host evidence"}

    def worker_log_text(self, worker):
        self.inspect(worker["cid"], pod_uid=worker["podUID"])
        result = self.h.node_exec("crictl", "logs", worker["cid"])
        return result.stdout + result.stderr

    def recovery_log(self, worker, lease, *, after_prefix=""):
        output = self.worker_log_text(worker)
        require(output.startswith(after_prefix), "worker recovery log was replaced or truncated")
        for line in output[len(after_prefix):].splitlines():
            if "runtime-lease-recovery: " not in line:
                continue
            report = json.loads(line.split("runtime-lease-recovery: ", 1)[1])
            if report.get("complete") is True and lease["taskID"] in (report.get("retainedTasks") or []):
                return recovery_proof(report, lease["runtime"], lease["taskID"], retained=True)
        return False

    def uncertain_runtime_evidence(self, name, lease):
        """Make actual task-service evidence unavailable without touching leases."""
        obj = self.h.get("pod", name, self.h.namespace)
        require_owned(obj, self.h.run_id, self.pods[name])
        require(name.startswith("life-"), "uncertainty probe is not a lifecycle task")
        detail = self.inspect(lease["taskID"], pod_uid=obj["metadata"]["uid"])
        main_pid = detail.get("info", {}).get("pid")
        require(type(main_pid) is int and main_pid > 1, "live initial task host PID unavailable")
        pid = int(self.h.node_exec("systemctl", "show", "containerd",
                                  "--property=MainPID", "--value").stdout.strip())
        executable = self.h.node_exec("readlink", f"/proc/{pid}/exe").stdout.strip()
        stat = self.h.node_exec("cat", f"/proc/{pid}/stat").stdout
        start = stat.rsplit(") ", 1)[1].split()[19]
        query = ["nsenter", "--mount=/proc/1/ns/mnt", "--root=/proc/1/root", "--wd=/",
                 "--", "/usr/local/bin/containerd-shim-brewlet-v2", "--brewlet-evidence"]
        request = json.dumps({"lease": lease})
        known = self.h.node_exec(*query, input=request)
        response = json.loads(known.stdout)
        require(response.get("gone") is False and not response.get("error"),
                "live task absence query is not initially valid")
        self.h.node_exec("timeout", "--version")
        worker = self.worker()
        prefix = self.worker_log_text(worker)
        self.guard()
        self.h.node_exec(*daemon_signal_command(pid, start, executable, "STOP"))
        paused_at = time.monotonic()
        try:
            stat = self.h.node_exec("cat", f"/proc/{pid}/stat").stdout
            fields = stat.rsplit(") ", 1)[1].split()
            require(fields[19] == start and fields[0] in ("T", "t"),
                    "owned containerd service was not stopped")
            unavailable = self.h.node_exec(
                "timeout", "--signal=TERM", "--kill-after=2s", "10s", *query,
                input=request, check=False, timeout=20)
            require(unavailable.returncode == 124,
                    "stopped task service did not produce a bounded inspection timeout")
            validate_lease(self.read(f"{STORE}/.tasks/{lease['taskID']}/lease.json"),
                           lease["runtime"], lease["taskID"])
            mounts = self.h.node_exec("cat", f"/proc/{main_pid}/mountinfo").stdout
            require(self.exists(root_path(lease["runtime"]) + "/rootfs") and re.search(
                re.escape(root_path(lease["runtime"]) + "/rootfs") + r"(?=$|[/,: \t\n])", mounts),
                "real task-service uncertainty permitted root or lease collection")
        finally:
            self.h.node_exec(*daemon_signal_command(pid, start, executable, "CONT"))
        recovered = self.h.wait("restored task service permits fresh complete fenced recovery", lambda:
            self.recovery_log(worker, lease, after_prefix=prefix))
        validate_lease(self.read(f"{STORE}/.tasks/{lease['taskID']}/lease.json"),
                       lease["runtime"], lease["taskID"])
        require(main_pid in self.task_pids(lease["taskID"]), "service resume replaced the real task")
        proof = {"daemonPID": pid, "daemonStart": start,
                 "lease": lease, "realPausedSeconds": time.monotonic() - paused_at,
                 "inspectionExitCode": unavailable.returncode,
                 "freshSuccessfulRecovery": recovered,
                 "boundary": "real task-service timeout; no task-server reconstruction or forged state"}
        self.h.record("actual task-service uncertainty retains lease/root and fresh recovery follows CONT",
                      proof, ["LEASE-02"])
        return proof

    def worker_if_running(self, predecessor=None):
        return self.worker(allow_starting=True, predecessor=predecessor)

    def on_demand(self):
        cold = self.inventory("cold")
        require(not self.exists(f"{STORE}/{cold}/current"), "cold ID was preinstalled")
        rotated = self.rotate(cold, "after")
        target = rotated["status"]["generation"]
        started = time.monotonic()
        while time.monotonic() - started < RETAIN_SECONDS:
            require(self.h.advertised(cold, "available"), "rotation changed cold capability")
            require(not self.exists(f"{STORE}/{cold}/current")
                    and not self.exists(f"{STORE}/{cold}/{rotated['metadata']['uid']}"),
                    "rotation fetched or published a cold OnDemand generation")
            time.sleep(2)
        cold_name = self.pod(cold, "cold-latest")
        _, cold_lease = self.ready(cold_name)
        require(cold_lease["runtime"]["generation"] == target
                and cold_lease["runtime"]["sourceDigest"]
                == self.h.runtime_images["python"]["after"].split("@")[1],
                "first use did not install the latest cold rotated target")
        self.h.record("cold OnDemand rotation does not install bytes; first use installs latest target",
                      {"lease": cold_lease, "target": target}, ["OD-04", "PROF-05"])
        self.drain_pod(cold_name, cold_lease)
        self.revoke(cold)
        self.finalize(cold, cold_lease["runtime"])
        self.edit_profile({cold: None})
        rid = self.inventory("idle", install="Eager")
        eager = self.current(rid)
        self.edit_profile({rid: {"id": rid, "install": "OnDemand", "idleTTL": str(IDLE_TTL) + "s"}})
        require(self.h.advertised(rid, "ready")
                and epoch(self.current(rid)) == epoch(eager),
                "Eager to OnDemand removed an existing installation")
        name = self.pod(rid, "idle-held")
        _, lease = self.ready(name)
        started = time.monotonic()
        while time.monotonic() - started < IDLE_TTL + 5:
            require(self.exists(root_path(lease["runtime"]) + "/rootfs")
                    and self.h.advertised(rid, "ready"),
                    "idle TTL collected a genuinely leased runtime")
            validate_lease(self.h.lease(self.h.get("pod", name, self.h.namespace)),
                           lease["runtime"], lease["taskID"])
            time.sleep(2)
        self.drain_pod(name, lease)
        drained = time.monotonic()
        idle_since = timestamp(self.read(f"{STORE}/{rid}/usage.json")["idleSince"])
        self.h.wait("real idle TTL reclamation after lease and mount drain", lambda:
            self.h.advertised(rid, "available") and not self.exists(f"{STORE}/{rid}/current"))
        require((datetime.now(timezone.utc) - idle_since).total_seconds() >= IDLE_TTL,
                "idle TTL did not start conservatively at actual lease release")
        require(not self.exists(root_path(eager)), "idle reclamation left generation bytes")
        self.edit_profile({rid: {"id": rid, "install": "Eager"}})
        self.h.wait("OnDemand to Eager installs without workload requests", lambda:
            self.h.advertised(rid, "ready"))
        installed = self.current(rid)
        require(epoch(installed) == epoch(eager), "mode switch changed inventory identity")
        self.h.record("mode transitions retain warm roots; idle TTL respects real leases and reinstalls eagerly",
                      {"identity": eager, "idleTTLSeconds": IDLE_TTL,
                       "realDrainToReclaimSeconds": time.monotonic() - drained}, ["OD-06"])
        self.revoke(rid)
        self.finalize(rid, installed)
        self.edit_profile({rid: None})

    def cri_file(self, name, value):
        directory = "/var/lib/brewlet/lifecycle-" + validate_run_id(self.h.run_id)
        require(name in ("sandbox.json", "container.json"), "unrecognized CRI input file")
        if self.cri_directory is None:
            self.h.node_exec("sh", "-ec", 'umask 077; mkdir "$1"',
                             "lifecycle-input", directory)
            self.cri_directory = directory
        require(self.cri_directory == directory, "CRI input directory identity changed")
        path = directory + "/" + name
        self.h.node_exec("sh", "-ec",
            'umask 077; test -d "$1"; test ! -L "$1"; '
            'test ! -e "$2"; test ! -L "$2"; set -C; cat > "$2"',
            "lifecycle-input", directory, path, input=json.dumps(value))
        return path

    def orphan_exec(self):
        rid = self.inventory("recovery", install="Eager")
        # Kubelet otherwise garbage-collects a sandbox absent from its Pod manager.
        self.guard()
        pid = int(self.h.node_exec("systemctl", "show", "kubelet",
                                  "--property=MainPID", "--value").stdout.strip())
        executable = self.h.node_exec("readlink", f"/proc/{pid}/exe").stdout.strip()
        stat = self.h.node_exec("cat", f"/proc/{pid}/stat").stdout
        start = stat.rsplit(") ", 1)[1].split()[19]
        self.h.node_exec(*kubelet_signal_command(pid, start, executable, "STOP"))
        resumed = False
        def resume():
            nonlocal resumed
            if not resumed:
                self.h.node_exec(*kubelet_signal_command(pid, start, executable, "CONT"))
                resumed = True
        try:
            self.orphan_exec_cri(rid, resume)
        finally:
            resume()

    def orphan_exec_cri(self, rid, resume_kubelet):
        # A genuine CRI lifecycle, separate from kubelet's eager container
        # teardown. The sandbox UID is invocation-owned CRI metadata, not a
        # fabricated Kubernetes object, lease, or cleanup receipt.
        sandbox_uid = str(uuid.uuid4())
        sandbox = {"metadata": {"name": "life-cri-" + self.h.run_id,
                               "namespace": self.h.namespace, "uid": sandbox_uid, "attempt": 0},
                   "log_directory": "/var/log/life-" + self.h.run_id,
                   "annotations": {"brewlet.sh/runtime-image": rid},
                   "linux": {"cgroup_parent": "system.slice",
                             "security_context": {"namespace_options": {"network": 2, "pid": 0}}}}
        initial = self.h.initial["python"]["pod"]
        base = self.h.get("pod", initial["metadata"]["name"], self.h.namespace)
        require_owned(base, self.h.run_id, initial["metadata"]["uid"])
        images = [s["image"] for s in base.get("status", {}).get("containerStatuses", [])
                  if s.get("name") == "app" and "running" in s.get("state", {})]
        require(len(images) == 1 and re.fullmatch(r"sha256:[0-9a-f]{64}", images[0]),
                "actual kubelet-resolved CRI image-config identity unavailable")
        config = {"metadata": {"name": "life-orphan", "attempt": 0},
                  "image": {"image": images[0], "user_specified_image": self.h.app_images["python"]},
                  "labels": {"io.kubernetes.pod.uid": sandbox_uid,
                             "io.kubernetes.pod.namespace": self.h.namespace,
                             "io.kubernetes.pod.name": sandbox["metadata"]["name"],
                             "io.kubernetes.container.name": "life-orphan"},
                  "command": ["python3"], "args": ["-c", MAIN],
                  "envs": [{"key": "PYTHONPATH", "value": "/app/dependencies"}],
                  "log_path": "orphan.log",
                  "linux": {"resources": {"memory_limit_in_bytes": 256 * 1024 * 1024,
                                         "cpu_period": 100000, "cpu_quota": 50000},
                            "security_context": {
                      "run_as_user": {"value": 65532}, "run_as_group": {"value": 65532},
                      "no_new_privs": True, "capabilities": {"drop_capabilities": ["ALL"]}}}}
        sandbox_path = self.cri_file("sandbox.json", sandbox)
        config_path = self.cri_file("container.json", config)
        self.guard()
        sandbox_id = cid(self.h.node_exec("crictl", "--timeout", "60s", "runp", "--runtime", "brewlet",
                                          sandbox_path).stdout.strip())
        self.cri = {"sandbox": sandbox_id, "uid": sandbox_uid, "container": None}
        container = cid(self.h.node_exec("crictl", "create", sandbox_id,
                                         config_path, sandbox_path).stdout.strip())
        self.cri["container"] = container
        self.inspect(container, pod_uid=sandbox_uid)
        self.h.node_exec("crictl", "--timeout", "60s", "start", container)
        detail = self.inspect(container, pod_uid=sandbox_uid)
        main_pid = detail["info"]["pid"]
        main_stat = self.h.node_exec("cat", f"/proc/{main_pid}/stat").stdout
        main_identity = {
            "start": main_stat.rsplit(") ", 1)[1].split()[19],
            "executable": self.h.node_exec("readlink", f"/proc/{main_pid}/exe").stdout.strip()}
        lease = self.read(f"{STORE}/.tasks/{container}/lease.json")
        identity = lease["runtime"]
        validate_lease(lease, identity, container)
        exec_id = "life-exec-" + self.h.run_id
        self.h.node_exec(*exec_command(container, exec_id))
        pids = self.task_pids(container)
        exec_pids = [p for p in pids if p != main_pid]
        require(exec_pids, "real detached exec did not start")
        exec_identities = {}
        for pid in exec_pids:
            stat = self.h.node_exec("cat", f"/proc/{pid}/stat").stdout
            exec_identities[pid] = {
                "start": stat.rsplit(") ", 1)[1].split()[19],
                "executable": self.h.node_exec("readlink", f"/proc/{pid}/exe").stdout.strip()}
        worker = self.worker()
        prefix = self.worker_log_text(worker)
        maintenance = self.h.wait("fresh production recovery observes the real task and exec", lambda:
            self.recovery_log(worker, lease, after_prefix=prefix))
        self.revoke(rid)
        started = time.monotonic()
        proof = None
        while time.monotonic() - started < RETAIN_SECONDS:
            live = self.orphan_pids(exec_identities)
            require(live and self.orphan_pids({main_pid: main_identity}),
                    "initial or detached exec ended before the active-reference proof")
            mounts = [self.h.node_exec("cat", f"/proc/{pid}/mountinfo").stdout for pid in live]
            for pid in live:
                status = self.h.node_exec("cat", f"/proc/{pid}/status").stdout
                require(re.search(r"^Uid:\s+65532\s+65532\s+65532\s+65532$", status, re.M)
                        and re.search(r"^Gid:\s+65532\s+65532\s+65532\s+65532$", status, re.M)
                        and re.search(r"^NoNewPrivs:\s+1$", status, re.M)
                        and re.search(r"^CapEff:\s+0+$", status, re.M),
                        "real orphan exec lost protected nonroot security state")
            proof = retained_refs(self.read(f"{STORE}/.tasks/{container}/lease.json"),
                                  identity, container, main_exited=False,
                                  exec_pids=live, mountinfo=mounts)
            require(self.exists(root_path(identity) + "/rootfs"), "orphan exec root was collected")
            inventory = self.h.get("runtimeimage", rid)
            require_owned(inventory, self.h.run_id, identity["inventoryUID"])
            require(inventory["metadata"].get("finalizers"), "orphan exec lost deletion finalizer")
            time.sleep(2)
        self.h.record("actual task/exec/mount references retain root, lease and revocation finalizer",
                      {"references": proof, "freshRecovery": maintenance}, ["LEASE-02", "LEASE-03"])
        self.h.node_exec(*orphan_signal_command(main_pid, main_identity["start"],
                                               main_identity["executable"]))
        self.h.wait("actual managed main process exit", lambda:
            not self.orphan_pids({main_pid: main_identity}), timeout=20)
        live = self.orphan_pids(exec_identities)
        for pid in live:
            expected = exec_identities[pid]
            self.h.node_exec(*orphan_signal_command(pid, expected["start"], expected["executable"]))
        self.h.wait("exact detached exec process drain", lambda:
            not self.orphan_pids(exec_identities), timeout=60)
        if not self.mounts_gone(identity):
            require(self.exists(root_path(identity) + "/rootfs")
                    and self.exists(f"{STORE}/.tasks/{container}/lease.json"),
                    "lease released before actual remaining rootfs mounts drained")
        self.inspect(container, pod_uid=sandbox_uid)
        self.h.node_exec("crictl", "rm", container)
        self.cri["container"] = None
        self.remove_sandbox()
        resume_kubelet()
        self.h.wait("real orphan task lease release after exec and mount drain", lambda:
            not self.exists(f"{STORE}/.tasks/{container}/lease.json"))
        require(self.mounts_gone(identity), "runtime mount survived real CRI task teardown")
        self.finalize(rid, identity)
        self.edit_profile({rid: None})
        self.h.node_exec("sh", "-ec",
            'test -d "$1"; test ! -L "$1"; '
            'test -f "$1/sandbox.json"; test ! -L "$1/sandbox.json"; '
            'test -f "$1/container.json"; test ! -L "$1/container.json"; '
            'rm -- "$1/sandbox.json" "$1/container.json"; rmdir -- "$1"',
            "lifecycle-input-cleanup", self.cri_directory)
        self.cri_directory = None
        self.h.record("managed CRI root releases only after actual exec and task/mount drain",
                      {"identity": identity, "containerID": container, "rootGone": True,
                       "orphanExecObservedAfterMainExit": bool(live),
                       "boundary": "normal delegated runc/CRI exit semantics; no forced orphan survival"},
                      ["LEASE-03"])

    def task_pids(self, container):
        output = self.h.node_exec("ctr", "-n", "k8s.io", "tasks", "ps", cid(container)).stdout
        lines = output.splitlines()
        require(lines and lines[0].split()[0] == "PID", "actual task process list unavailable")
        require(all(line.split() and line.split()[0].isdigit() for line in lines[1:]),
                "ambiguous task process evidence")
        return [int(line.split()[0]) for line in lines[1:]]

    def orphan_pids(self, identities):
        live = []
        for pid, expected in identities.items():
            observed = self.h.node_exec("cat", f"/proc/{pid}/stat", check=False)
            if observed.returncode:
                require(observed.returncode == 1 and self.h.node_exec(
                    "test", "!", "-d", f"/proc/{pid}", check=False).returncode == 0,
                    "orphan process evidence unavailable")
                continue
            fields = observed.stdout.rsplit(") ", 1)[1].split()
            require(fields[19] == expected["start"], "orphan PID was reused")
            if fields[0] not in ("Z", "X"):
                live.append(pid)
        return live

    def remove_sandbox(self):
        sandbox = self.cri
        inspected = json.loads(self.h.node_exec("crictl", "inspectp", sandbox["sandbox"]).stdout)
        status = inspected.get("status", {})
        require(status.get("id") == sandbox["sandbox"]
                and status.get("metadata", {}).get("uid") == sandbox["uid"]
                and status["metadata"].get("namespace") == self.h.namespace,
                "CRI sandbox identity changed; refusing cleanup")
        self.h.node_exec("crictl", "stopp", sandbox["sandbox"])
        self.h.node_exec("crictl", "rmp", sandbox["sandbox"])
        self.cri = None

    def execute(self):
        self.guard()
        # No broad process/service mutation occurs on an unrelated node.
        require(self.h.node == "brewlet-v2-" + self.h.run_id + "-control-plane",
                "lifecycle requires the exact private kind node")
        self.gc_timing(self.worker())
        disk = self.h.node_exec("df", "-Pk", STORE).stdout.splitlines()
        require(len(disk) == 2 and len(disk[1].split()) >= 4
                and disk[1].split()[3].isdigit()
                and int(disk[1].split()[3]) * 1024 >= 1024**3,
                "lifecycle requires at least 1GiB free on the real runtime filesystem")
        original = copy.deepcopy(self.h.get("nodeprofile", self.h.profile)["spec"]["runtimeImages"])
        initial = copy.deepcopy(self.h.initial)
        self.inventory_epoch()
        self.expiry_and_recovery()
        self.on_demand()
        self.orphan_exec()
        require(not self.entries and not self.pods and self.cri is None,
                "lifecycle resources did not genuinely drain")
        require(self.h.get("nodeprofile", self.h.profile)["spec"]["runtimeImages"] == original,
                "lifecycle disturbed base profile entries")
        require(self.h.initial == initial, "lifecycle modified base task evidence")
        for language, held in initial.items():
            current = self.h.get("pod", language + "-old", self.h.namespace)
            require_owned(current, self.h.run_id, held["pod"]["metadata"]["uid"])
            require(current["status"]["containerStatuses"][0]["containerID"]
                    == held["pod"]["status"]["containerStatuses"][0]["containerID"]
                    and self.h.lease(current) == held["lease"],
                    "lifecycle disturbed a base fixture task or lease")
            self.h.assert_report(self.h.http(language + "-old"), language, arguments=[], cwd="/app")
        return {"scope": "real-node-lifecycle-only", "fullAcceptance": False,
                "durableLeaseMaintenanceRecoveryVerified": True,
                "retiredGenerationGCVerified": True,
                "shimCrashReconstructionVerified": False,
                "orphanExecSurvivalVerified": False,
                "execLifecycleBoundary": "normal delegated runc/CRI may drain exec with main exit; "
                                         "actual active refs and complete drain are verified",
                "proofBoundary": "fresh fenced recovery using real task/mount evidence; "
                                 "no runc task-server reconstruction requirement inferred"}


def execute(h):
    """Run strict assertions; errors propagate to the parent's evidence/cleanup.

    No skip path and no offline result is recorded as a live pass. On failure,
    retain UID-bound objects for the parent's diagnostic/destruction phase;
    never remove finalizers to force cleanup. STOP always has guarded CONT.
    """
    return Lifecycle(h).execute()
