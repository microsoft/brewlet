#!/usr/bin/env python3
"""Actual unsafe none-policy denial, managed activation and byte-exact restoration."""

import argparse
import copy
from datetime import datetime, timedelta, timezone
import hashlib
import json
from pathlib import Path
import re
import signal
import shutil
import tomllib

import delivery
from harness import FENCE, IDS, ROOT, STORE, Failure, Harness, require
from lifecycle import daemon_signal_command, orphan_signal_command


def unsafe_retention_config(dumped):
    config = tomllib.loads(dumped)
    version = config.get("version")
    require(version in (2, 3), "unsupported fixture containerd config version")
    text, count = re.subn(r"(?m)^([ \t]*discard_unpacked_layers[ \t]*=[ \t]*)(true|false)([ \t]*)$",
                         r"\g<1>true\g<3>", dumped)
    require(count == 1, "effective fixture config has ambiguous packed-layer retention")
    result = "# exact pre-Brewlet fixture bytes and formatting\n" + text
    parsed = tomllib.loads(result)
    section = (parsed["plugins"]["io.containerd.cri.v1.images"] if version == 3
               else parsed["plugins"]["io.containerd.grpc.v1.cri"]["containerd"])
    require(section.get("discard_unpacked_layers") is True, "fixture did not enable actual discard")
    return result


def file_digest(h):
    value = h.node_exec("sha256sum", "/etc/containerd/config.toml").stdout.split()
    require(len(value) == 2 and re.fullmatch(r"[0-9a-f]{64}", value[0]),
            "actual host configuration digest unavailable")
    return value[0]


def protected_cache_roots(state):
    roots = state.get("layerRoots")
    require(isinstance(roots, list) and roots
            and all(isinstance(root, str) and re.fullmatch(
                r"/var/lib/brewlet/layers/v[0-9]+-[0-9]+-[0-9a-f]{64}/rootfs", root)
                    for root in roots), "actual initial state has unsafe/missing cache provenance")
    return roots


def shim_kill_command(pid, start, executable):
    require(type(pid) is int and pid > 1 and isinstance(start, str) and start.isdigit(),
            "unsafe task-server process identity")
    require(executable == "/usr/local/bin/containerd-shim-brewlet-v2",
            "refusing to kill a non-Brewlet task server")
    return ["sh", "-ec",
            'test "$(readlink /proc/$1/exe)" = "$3"; '
            's=$(cat /proc/$1/stat); s=${s##*) }; '
            'test "$(printf "%s\\n" "$s" | awk \'{print $20}\')" = "$2"; '
            'kill -KILL "$1"',
            "owned-shim-crash", str(pid), start, executable]


def invalid_path_runtime(source, destination):
    shutil.copytree(source, destination)
    outer = json.loads((destination / "index.json").read_text())
    require(len(outer["manifests"]) == 1, "ambiguous golden runtime fixture")
    selected = outer["manifests"][0]
    require(selected["mediaType"] == delivery.MANIFEST, "expected native golden manifest")
    manifest = json.loads(delivery.read_blob(destination, selected))
    config = json.loads(delivery.read_blob(destination, manifest["config"]))
    env = config["config"]["Env"]
    require(sum(value.startswith("PATH=") for value in env) == 1,
            "golden runtime fixture lacks a unique PATH")
    config["config"]["Env"] = ["PATH=/app" if value.startswith("PATH=") else value for value in env]
    manifest["config"] = delivery.json_blob(destination, config, delivery.CONFIG)
    return delivery.finish_layout(destination, delivery.json_blob(destination, manifest, delivery.MANIFEST))


def failed_attempt_times(logs):
    return [datetime.fromisoformat(line.split()[0].replace("Z", "+00:00"))
            for line in logs.splitlines() if "provision-error: runtime install failed:" in line
            and "unsafe runtime PATH component" in line]


def runc_lifecycle(h, survivor):
    response = h.exec(survivor, ["/bin/sh", "-ec", "printf 'runc-exec-output\\n'; exit 37"],
                      check=False)
    require(response.returncode == 37 and response.stdout == "runc-exec-output\n",
            "delegated Exec changed its output or nonzero exit code")
    h.record("actual delegated Exec preserves output and exact nonzero exit code",
             {"exitCode": response.returncode}, ["RUN-09"])
    name = "runc-signal"
    pod = h.pod(name, "python")
    pod["spec"]["restartPolicy"] = "Never"
    container = pod["spec"]["containers"][0]
    container.pop("readinessProbe")
    container["command"] = ["/bin/sh"]
    container["args"] = ["-ec", "trap 'exit 42' TERM; printf 'runc-signal-ready\\n'; "
                         "while :; do sleep 1; done"]
    h.apply(pod)
    running = h.wait("dedicated delegated signal task starts", lambda: h.ready(name))
    lease = h.lease(running)
    h.wait("owned initial process installs its signal handler", lambda:
           h.kubectl("-n", h.namespace, "logs", name, "-c", "app").stdout == "runc-signal-ready\n")
    h.node_exec("ctr", "-n", "k8s.io", "tasks", "kill", "--signal", "SIGTERM", lease["taskID"])

    def exited():
        current = h.get("pod", name, h.namespace)
        statuses = current.get("status", {}).get("containerStatuses", [])
        require(len(statuses) == 1 and statuses[0]["containerID"] == "containerd://" + lease["taskID"],
                "signal proof changed its protected container identity")
        terminal = statuses[0].get("state", {}).get("terminated")
        return terminal if terminal else False

    terminal = h.wait("delegated SIGTERM reaches the initial process with exact exit status", exited)
    require(terminal["exitCode"] == 42, "delegated signal/initial exit code changed")
    require(h.kubectl("-n", h.namespace, "logs", name, "-c", "app").stdout == "runc-signal-ready\n",
            "initial process logs changed after signal/exit")
    h.record("actual delegated initial process receives SIGTERM and preserves logs and exit code",
             {"taskID": lease["taskID"], "exitCode": terminal["exitCode"]}, ["RUN-09"])
    h.delete("pod", name, h.namespace)
    h.wait("signal proof task drains normally", lambda: h.get("pod", name, h.namespace, absent=True) is None)


def failed_rotation_recovery(h):
    h.apply(h.pod("retry-survivor", "python"))
    pod = h.wait("failed-rotation survivor starts", lambda: h.ready("retry-survivor"))
    lease = h.lease(pod)
    profile_uid = h.get("nodeprofile", h.profile)["metadata"]["uid"]
    previous = h.current("python")
    layout = h.delivery.work / "python-invalid-path"
    selected = invalid_path_runtime(h.delivery.work / "python-after-golden", layout)
    source = h.delivery.publish(layout, selected, "delivery/runtime-python-invalid-path")
    image = h.get("runtimeimage", IDS["python"])
    spec = copy.deepcopy(image["spec"])
    spec["source"]["image"] = source
    h.replace_inventory_spec(image, spec)

    def attempts():
        logs = h.kubectl("-n", h.control_namespace, "logs", "-l",
                         "brewlet.sh/owner-uid=" + profile_uid,
                         "-c", "provisioner", "--timestamps=true", "--tail=200").stdout
        times = failed_attempt_times(logs)
        return times if len(times) >= 4 else False

    times = h.wait("real invalid-root rotation fails repeatedly with exponential backoff", attempts)
    intervals = [(right - left).total_seconds() for left, right in zip(times[:3], times[1:4])]
    require(all(0.9 * (2 ** index) <= value < 90 for index, value in enumerate(intervals)),
            "real install retries did not preserve bounded exponential backoff: " + repr(intervals))
    auth = json.loads(h.node_exec("cat", STORE + "/" + IDS["python"] + "/authorization.json").stdout)
    require(auth["state"] == "failed" and auth["digest"] == source.split("@")[1],
            "failed rotation did not persist the rejected target state")
    require(IDS["python"] not in {
        key.removeprefix("brewlet.sh/runtime.") for key in
        h.node_snapshot()["metadata"].get("labels", {}) if key.startswith("brewlet.sh/runtime.")},
        "failed OnDemand rotation retained its scheduling capability")
    require(h.current("python") == previous, "failed rotation replaced the validated current root")
    require(h.lease(h.ready("retry-survivor")) == lease, "failed rotation changed a running lease")
    h.assert_report(h.http("retry-survivor"), "python", arguments=[], cwd="/app")
    h.record("actual installed OnDemand failure withdraws capability and retries with exponential backoff",
             {"inventoryUID": auth["inventoryUID"], "failedGeneration": auth["generation"],
              "retryIntervalsSeconds": intervals, "survivingTaskID": lease["taskID"]}, ["OD-01"])

    image = h.get("runtimeimage", IDS["python"])
    spec = copy.deepcopy(image["spec"])
    spec["source"]["image"] = h.runtime_images["python"]["after"]
    h.replace_inventory_spec(image, spec)
    h.wait("normal corrected target recovers installed OnDemand runtime", lambda:
           h.advertised(IDS["python"], "ready")
           and h.current("python")["sourceDigest"] == spec["source"]["image"].split("@")[1])
    h.apply(h.pod("retry-recovered", "python"))
    h.wait("new task starts after validated retry recovery", lambda: h.ready("retry-recovered"))
    h.assert_report(h.http("retry-recovered"), "python", arguments=[], cwd="/app")
    require(h.lease(h.ready("retry-survivor")) == lease, "retry recovery rebound the surviving lease")
    h.record("corrected OnDemand target recovers without rebuilding application or rebinding running lease",
             {"applicationImage": h.app_images["python"], "survivingTaskID": lease["taskID"]}, ["OD-01"])
    runc_lifecycle(h, "retry-survivor")
    for name in ("retry-survivor", "retry-recovered"):
        h.delete("pod", name, h.namespace)
        h.wait("retry proof task drains normally: " + name, lambda name=name:
               h.get("pod", name, h.namespace, absent=True) is None)


def killed_shim_recovery(h):
    target = h.pod("crashed-shim", "python")
    target["spec"]["shareProcessNamespace"] = True
    h.apply(target)
    pod = h.wait("dedicated actual shim-crash task starts", lambda: h.ready("crashed-shim"))
    lease = h.lease(pod)
    task = lease["taskID"]
    inspected = json.loads(h.node_exec("crictl", "inspect", task).stdout)
    require(inspected["status"]["id"] == task
            and inspected["status"]["labels"]["io.kubernetes.pod.uid"] == pod["metadata"]["uid"],
            "shim-crash target is not the exact owned Pod/task")
    main = inspected["info"]["pid"]
    require(type(main) is int and main > 1
            and task in h.node_exec("cat", f"/proc/{main}/cgroup").stdout,
            "task-server crash lacks actual owned main-process cgroup provenance")
    fields = h.node_exec("cat", f"/proc/{main}/stat").stdout.rsplit(") ", 1)[1].split()
    main_start = fields[19]
    main_exe = h.node_exec("readlink", f"/proc/{main}/exe").stdout.strip()
    status = h.node_exec("cat", f"/proc/{main}/status").stdout
    namespace_pid = re.search(r"^NSpid:\s+([0-9 \t]+)$", status, re.M)
    require(namespace_pid and int(namespace_pid.group(1).split()[-1]) != 1,
            "orphan proof requires actual shared sandbox PID namespace")
    exec_id = "crash-exec-" + h.run_id
    h.node_exec("ctr", "-n", "k8s.io", "tasks", "exec", "--exec-id", exec_id,
                "--detach", "--user", "65532:65532", task,
                "python3", "-c", "import time; time.sleep(1800)", exec_id)
    processes = h.node_exec("ctr", "-n", "k8s.io", "tasks", "ps", task).stdout.splitlines()
    pids = [int(line.split()[0]) for line in processes[1:] if line.split()]
    execs = [pid for pid in pids if pid != main
             and ("\0" + exec_id + "\0") in h.node_exec("cat", f"/proc/{pid}/cmdline").stdout]
    require(len(execs) == 1, "dedicated task does not have exactly one real detached Exec")
    exec_pid = execs[0]
    require(task in h.node_exec("cat", f"/proc/{exec_pid}/cgroup").stdout,
            "orphan Exec does not belong to the exact task cgroup")
    exec_start = h.node_exec("cat", f"/proc/{exec_pid}/stat").stdout.rsplit(") ", 1)[1].split()[19]
    shim = int(fields[1])
    shim_fields = h.node_exec("cat", f"/proc/{shim}/stat").stdout.rsplit(") ", 1)[1].split()
    executable = h.node_exec("readlink", f"/proc/{shim}/exe").stdout.strip()
    command = h.node_exec("cat", f"/proc/{shim}/cmdline").stdout
    require("-namespace\0k8s.io\0" in command and "-id\0" in command,
            "task parent is not the expected protected containerd shim namespace")
    kill = shim_kill_command(shim, shim_fields[19], executable)
    daemon = int(h.node_exec("systemctl", "show", "containerd", "--property=MainPID",
                             "--value").stdout.strip())
    daemon_start = h.node_exec("cat", f"/proc/{daemon}/stat").stdout.rsplit(") ", 1)[1].split()[19]
    daemon_exe = h.node_exec("readlink", f"/proc/{daemon}/exe").stdout.strip()
    query = ["/usr/local/bin/containerd-shim-brewlet-v2", "--brewlet-evidence"]
    request = json.dumps({"lease": lease})
    h.node_exec(*daemon_signal_command(daemon, daemon_start, daemon_exe, "STOP"))
    try:
        h.node_exec(*kill)
        h.wait("exact fixture task server really dies", lambda:
               h.node_exec("test", "!", "-e", f"/proc/{shim}/exe", check=False).returncode == 0,
               timeout=20)
        h.node_exec(*orphan_signal_command(main, main_start, main_exe))
        h.wait("original shared-namespace main exits after its server dies", lambda:
               h.node_exec("test", "!", "-e", f"/proc/{main}/exe", check=False).returncode == 0,
               timeout=20)
        orphan = h.node_exec("cat", f"/proc/{exec_pid}/stat").stdout.rsplit(") ", 1)[1].split()
        require(orphan[19] == exec_start and orphan[0] not in ("Z", "X"),
                "actual detached Exec did not survive server and main exit")
        mountinfo = h.node_exec("cat", f"/proc/{exec_pid}/mountinfo").stdout
        require(f"upperdir={lease['taskMounts']['upper']}" in mountinfo,
                "surviving orphan Exec does not mount its exact leased task root")
        uncertain = h.node_exec("timeout", "--signal=TERM", "--kill-after=2s", "10s",
                               *query, input=request, check=False, timeout=20)
        require(uncertain.returncode == 124, "killed-server evidence unexpectedly certified absence")
        retained = json.loads(h.node_exec("cat", f"{STORE}/.tasks/{task}/lease.json").stdout)
        require(retained == lease, "shim death with unavailable evidence discarded durable lease")
        identity = lease["runtime"]
        h.node_exec("test", "-d",
                    f"{STORE}/{identity['id']}/{identity['inventoryUID']}/{identity['generation']}/rootfs")
        h.record("actual killed task server retains durable lease/root while evidence is unavailable",
                 {"taskID": task, "shimPID": shim, "shimStart": shim_fields[19],
                  "inspectionExit": uncertain.returncode, "noStateMutation": True}, ["LEASE-02"])
        orphan = h.node_exec("cat", f"/proc/{exec_pid}/stat").stdout.rsplit(") ", 1)[1].split()
        require(orphan[19] == exec_start and orphan[0] not in ("Z", "X"),
                "orphan Exec drained before retained-reference observation completed")
        h.record("real orphan Exec after main/server exit retains durable lease and task root",
                 {"taskID": task, "execPID": exec_pid, "execStart": exec_start,
                  "mainExited": True, "shimExited": True, "actualUpper": lease["taskMounts"]["upper"],
                  "normalBackendCleanupStillRequired": True}, ["LEASE-03"])
    finally:
        h.node_exec(*daemon_signal_command(daemon, daemon_start, daemon_exe, "CONT"))
    h.delete("pod", "crashed-shim", h.namespace)
    h.wait("crashed task Pod is removed normally", lambda:
           h.get("pod", "crashed-shim", h.namespace, absent=True) is None)
    h.wait("normal backend cleanup and fresh fenced recovery release dead task lease", lambda:
           h.node_exec("test", "!", "-e", f"{STORE}/.tasks/{task}/lease.json",
                       check=False).returncode == 0)
    h.apply(h.pod("fresh-shim", "python"))
    fresh = h.wait("fresh actual task server uses durable inventory after crash", lambda: h.ready("fresh-shim"))
    h.assert_report(h.http("fresh-shim"), "python", arguments=[], cwd="/app")
    fresh_lease = h.lease(fresh)
    require(fresh_lease["runtime"]["inventoryUID"] == lease["runtime"]["inventoryUID"],
            "fresh task server adopted a different inventory epoch")
    h.delete("pod", "fresh-shim", h.namespace)
    h.wait("fresh-server proof task drains normally", lambda:
           h.get("pod", "fresh-shim", h.namespace, absent=True) is None)
    h.record("fresh shim after actual server death recovers durable inventory and complete cleanup",
             {"oldTaskID": task, "newTaskID": fresh_lease["taskID"],
              "oldContainerSurvivalRequired": False,
              "boundary": "normal delegated dead-server cleanup, not task-server resurrection"},
             ["LEASE-02"])


def cache_lifetime(h, pod):
    lease = h.lease(pod)
    task = lease["taskID"]
    state = json.loads(h.node_exec("cat", f"{STORE}/.tasks/{task}/initial.json").stdout)
    roots = protected_cache_roots(state)
    receipts = [json.loads(h.node_exec("cat", root.removesuffix("/rootfs") + "/receipt.json").stdout)
                for root in roots]
    eligible = max(datetime.fromisoformat(r["publishedAt"].replace("Z", "+00:00"))
                   for r in receipts) + timedelta(seconds=6)
    h.wait("positive real cache grace and multiple normal sweep periods elapse",
           lambda: datetime.now(timezone.utc) >= eligible)
    for root in roots:
        h.node_exec("test", "-d", root)
    h.assert_report(h.http("activation-probe"), "python", arguments=[], cwd="/app")
    h.record("actual durable task and live mounts retain persistent layers beyond positive GC grace",
             {"taskID": task, "layerRoots": roots, "publicationReceipts": receipts,
              "noReceiptOrClockMutation": True}, ["RUN-06"])
    h.delete("pod", "activation-probe", h.namespace)
    h.wait("activation probe drains normally", lambda:
           h.get("pod", "activation-probe", h.namespace, absent=True) is None)
    h.wait("actual task lease disappears before unreferenced cache collection", lambda:
           h.node_exec("test", "!", "-e", f"{STORE}/.tasks/{task}/lease.json",
                       check=False).returncode == 0)
    h.wait("normal fenced worker collects actual unreferenced expired layer entries", lambda:
           all(h.node_exec("test", "!", "-e", root.removesuffix("/rootfs"),
                           check=False).returncode == 0 for root in roots))
    canonical = h.app_images["python"]
    h.apply(h.pod("cache-recreated", "python"))
    current = h.wait("same immutable artifact rematerializes after actual cache GC",
                     lambda: h.ready("cache-recreated"))
    h.assert_report(h.http("cache-recreated"), "python", arguments=[], cwd="/app")
    recreated = h.lease(current)
    state = json.loads(h.node_exec("cat", f"{STORE}/.tasks/{recreated['taskID']}/initial.json").stdout)
    require(protected_cache_roots(state) == roots and h.app_images["python"] == canonical,
            "cache recreation changed digest/policy roots or republished the application")
    for root, original in zip(roots, receipts):
        receipt = json.loads(h.node_exec("cat", root.removesuffix("/rootfs") + "/receipt.json").stdout)
        require(receipt["digest"] == original["digest"] and receipt["diffId"] == original["diffId"]
                and receipt["publishedAt"] != original["publishedAt"],
                "cache recreation did not reverify immutable layer identity")
    h.record("normal post-drain cache GC and same-artifact verified rematerialization succeed",
             {"originalTaskID": task, "replacementTaskID": recreated["taskID"],
              "applicationImage": canonical, "layerRoots": roots}, ["RUN-06"])
    h.delete("pod", "cache-recreated", h.namespace)
    h.wait("replacement cache task drains normally", lambda:
           h.get("pod", "cache-recreated", h.namespace, absent=True) is None)


def exercise(h, original):
    digest = hashlib.sha256(original.encode()).hexdigest()
    h.run(["helm", "--kubeconfig", h.kubeconfig, "--kube-context", h.context,
           "upgrade", h.cluster, ROOT / "kubernetes/charts/brewlet", "-n", h.control_namespace,
           "--reuse-values", "--set", "provisioner.gc.grace=2s",
           "--set", "provisioner.gc.sweepInterval=1s", "--wait", "--timeout", "180s"],
          timeout=240)
    h.kubectl("label", "node", h.node, "brewlet.test/pool=" + h.run_id)
    h.kubectl("taint", "node", h.node, FENCE + "=provisioning:NoSchedule")
    profile = h.profile_object()
    profile["spec"]["runtimeImages"] = [{"id": IDS["python"], "install": "OnDemand"}]
    profile["spec"]["rollout"]["containerdRestart"] = "none"
    created = h.apply(profile)

    def denied():
        logs = h.kubectl("-n", h.control_namespace, "logs", "-l",
                         "brewlet.sh/owner-uid=" + created["metadata"]["uid"],
                         "--all-containers=true", "--tail=100", check=False)
        return "packed-layer retention" in logs.stdout + logs.stderr

    h.wait("real none-policy worker rejects unsafe packed-layer retention", denied)
    node = h.node_snapshot()
    require(node["metadata"]["uid"] == h.node_uid
            and node["metadata"].get("labels", {}).get("brewlet.sh/runtime") != "ready"
            and any(t["key"] == FENCE for t in node["spec"].get("taints", [])),
            "unsafe none policy advertised or released runtime readiness")
    require(file_digest(h) == digest, "none policy changed original host configuration")
    for path in ("/usr/local/bin/containerd-shim-brewlet-v2", STORE + "/.host",
                 STORE + "/" + IDS["python"] + "/current"):
        require(h.node_exec("test", "!", "-e", path, check=False).returncode == 0,
                "unsafe none policy mutated host activation/runtime state: " + path)
    h.record("actual none policy rejects unsafe retention without config, shim or root publication",
             {"originalConfigSHA256": digest, "profileUID": created["metadata"]["uid"]},
             ["RUN-10", "PROF-02"])

    current = h.get("nodeprofile", h.profile)
    spec = copy.deepcopy(current["spec"])
    spec["rollout"]["containerdRestart"] = "validated"
    h.replace_inventory_spec(current, spec)
    h.wait("normal validated activation repairs cold-start prerequisite", lambda:
           h.advertised(IDS["python"], "available"))
    dumped = tomllib.loads(h.node_exec("containerd", "config", "dump").stdout)
    section = (dumped["plugins"]["io.containerd.cri.v1.images"] if dumped["version"] == 3
               else dumped["plugins"]["io.containerd.grpc.v1.cri"]["containerd"])
    require(section["discard_unpacked_layers"] is False,
            "effective retention was not restored before first Create")
    h.apply(h.pod("activation-probe", "python"))
    pod = h.wait("fresh application genuinely starts after managed repair", lambda: h.ready("activation-probe"))
    h.assert_report(h.http("activation-probe"), "python", arguments=[], cwd="/app")
    h.record("managed activation enables effective packed retention before a real cold application",
             {"originalConfigSHA256": digest}, ["RUN-10"])
    cache_lifetime(h, pod)
    killed_shim_recovery(h)
    failed_rotation_recovery(h)
    h.delete("nodeprofile", h.profile)
    h.wait("managed activation production cleanup finalizes", lambda:
           h.get("nodeprofile", h.profile, absent=True) is None)
    require(file_digest(h) == digest, "cleanup did not restore exact original config bytes")
    require(not h.node_snapshot()["metadata"].get("labels", {}).get("brewlet.sh/owner-uid"),
            "activation cleanup retained original claim")
    require(h.node_exec("test", "!", "-e", "/usr/local/bin/containerd-shim-brewlet-v2",
                        check=False).returncode == 0, "activation cleanup retained owned shim")
    h.node_exec("crictl", "info")
    h.result["productionCleanupVerified"] = True
    h.record("actual production cleanup restores exact original containerd bytes and live service",
             {"restoredConfigSHA256": digest, "nodeUID": h.node_uid}, ["RUN-10", "OWN-02"])


def run(work):
    h = Harness(work)
    h.result.update(selectedScenario="activation", selectedLanguages=list(h.languages))
    h.result["gaps"].append("Focused activation/retention case; complete and companion suites are separate")
    status = 1
    try:
        h.phase = "prerequisites"
        h.preflight()
        h.phase = "cluster"
        h.provision_cluster()
        require(h.node_snapshot()["metadata"]["uid"] == h.node_uid, "owned Node changed")
        original = unsafe_retention_config(h.node_exec("containerd", "config", "dump").stdout)
        h.node_exec("/bin/sh", "-ec", 'cat > /etc/containerd/config.toml', input=original)
        h.node_exec("systemctl", "restart", "containerd", timeout=90)
        h.wait("fixture unsafe-config daemon restart completes", lambda:
               h.node_exec("crictl", "info", check=False).returncode == 0)
        for phase, action in (("build", h.prepare_images),
                              ("delivery-bootstrap", lambda: delivery.bootstrap(h)),
                              ("install", h.install),
                              ("activation-contract", lambda: exercise(h, original))):
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
            delivery.cleanup(h)
        except Exception as error:
            errors.append("fixture teardown: " + str(error))
        errors.extend(h.destroy())
        if errors:
            h.result.update(outcome="failed", cleanupErrors=errors)
            status = 1
        h.save()
    return status if h.result.get("outcome") == "passed" else 1


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", required=True, type=Path)

    def interrupted(signum, frame):
        raise Failure("activation acceptance interrupted by signal " + str(signum))

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    raise SystemExit(run(parser.parse_args().work))
