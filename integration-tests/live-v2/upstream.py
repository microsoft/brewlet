#!/usr/bin/env python3
"""Unchanged upstream distroless execution; shell-free probes and normal cleanup."""

import argparse
import copy
import json
from pathlib import Path, PurePosixPath
import re
import signal
import tarfile
import time

import delivery
from harness import API, FIXTURES, IDS, STORE, Failure, Harness, require
from lifecycle import Lifecycle, daemon_restart_command

PINS = FIXTURES / "runtimes/upstream.json"
EXECUTABLES = {"java": "/usr/bin/java", "python": "/usr/bin/python3.13", "node": "/nodejs/bin/node"}


def baseline_audit(layout, metadata):
    """Inspect original tar metadata only; never extract upstream bytes on the host."""
    selected = metadata["descriptor"]
    manifest = json.loads(delivery.read_blob(layout, selected))
    entries, account = {}, b""
    setid = []
    for layer in manifest["layers"]:
        path = layout / "blobs/sha256" / layer["digest"][7:]
        with tarfile.open(path, "r|*") as archive:
            for item in archive:
                name = item.name.removeprefix("./").rstrip("/")
                if name in ("", "."):
                    continue
                parts = PurePosixPath(name).parts
                require(not name.startswith("/") and ".." not in parts, "unsafe upstream tar path")
                require(not PurePosixPath(name).name.startswith(".wh."),
                        "baseline audit needs explicit upstream whiteout handling")
                require(len(entries) < 200000, "upstream metadata audit limit exceeded")
                for parent in PurePosixPath(name).parents:
                    if str(parent) != ".":
                        entries.setdefault(str(parent), ("directory", ""))
                entries[name] = ("symlink", item.linkname) if item.issym() else (
                    "directory" if item.isdir() else "file", "")
                if item.isfile() and item.mode & 0o6000:
                    setid.append(name)
                if name == "etc/passwd" and item.isfile():
                    account = archive.extractfile(item).read(65537)
                    require(len(account) <= 65536, "oversized upstream passwd metadata")
    names = [line.split(":")[0] for line in account.decode().splitlines() if line and not line.startswith("#")]
    path_values = [value[5:] for value in metadata["config"]["config"].get("Env", []) if value.startswith("PATH=")]
    blockers = []
    if "app" not in names:
        blockers.append("pre-amendment named app account requirement")
    if not path_values:
        blockers.append("pre-amendment mandatory PATH")
    require(not setid, "unchanged upstream violates retained set-ID policy: " + repr(setid))
    require(not any(name in entries for name in ("bin/sh", "usr/bin/sh", "bin/bash", "usr/bin/bash", "busybox/sh")),
            "qualification input is not shell-free")
    return {"source": metadata["source"], "manifestDigest": selected["digest"],
            "configDigest": manifest["config"]["digest"], "rootDiffIDs": metadata["config"]["rootfs"]["diff_ids"],
            "accountNames": names, "PATH": path_values, "oldContractBlockers": blockers,
            "shellAbsent": True, "noImageMutation": True}


class UpstreamDelivery(delivery.Delivery):
    def __init__(self, h, pins):
        super().__init__(h)
        require(not h.runtime_images, "unchanged-upstream suite must not build derived runtime images")
        h.runtime_images = {language: {} for language in h.languages}
        self.pins = pins
        self.metadata = {}
        self.importer = h.work / "runtimeimage"
        h.run(["go", "build", "-mod=readonly", "-o", self.importer,
               "./integration-tests/live-v2/runtimeimage"], timeout=180)

    def imported(self, language, revision, layout):
        pin = self.pins[language]
        source = pin["previous"][self.h.arch] if revision == "before" and "previous" in pin else pin[self.h.arch]
        metadata = json.loads(self.h.run([self.importer, "--source", source, "--output", layout], timeout=600).stdout)
        require(metadata["descriptor"]["digest"] == source.split("@")[1], "upstream digest changed during import")
        require(metadata["config"]["architecture"] == self.h.arch, "upstream platform differs from native node")
        audit = baseline_audit(layout, metadata)
        self.metadata[language, revision] = metadata
        self.h.result.setdefault("upstreamRuntimes", {}).setdefault(language, {})[revision] = audit
        path = audit["PATH"]
        if language in self.h.languages:
            require(len(path) == 1, "these pinned language fixtures use an explicit upstream PATH")
            if revision == "before":
                self.h.runtime_paths[language] = path[0]
            else:
                require(self.h.runtime_paths[language] == path[0], "upstream update changes its PATH compatibility promise")
                previous = self.metadata[language, "before"]["config"]["rootfs"]["diff_ids"]
                require(previous != metadata["config"]["rootfs"]["diff_ids"], "rotation did not change upstream root bytes")
        self.h.record(language + " unchanged upstream manifest and shell-free metadata verified",
                      audit, ["UP-04"])
        return metadata["descriptor"]

    def golden_layout(self, language, revision):
        layout = self.work / (language + "-" + revision + "-upstream")
        return layout, self.imported(language, revision, layout)

    def static_layout(self, layout):
        return self.imported("static", "after", layout)


def inspect_report(h, name, language, *, arguments=None, cwd="/app", runtime=None):
    pod = h.ready(name)
    require(pod, "positive upstream task is not ready")
    lease = h.lease(pod)
    report = h.http(name)
    h.assert_report(report, language, arguments=arguments or [], cwd=cwd, home=lease["runtime"]["home"])
    if runtime is not None:
        require(report["runtime"] == runtime, "existing task silently moved to a new runtime")
    result = json.loads(h.exec(name, h.probe(language)).stdout)
    h.assert_report(result, language, arguments=["--probe", "two words"], cwd=cwd, home=lease["runtime"]["home"])
    require(result["runtime"] == report["runtime"], "initial/Exec runtime differs")
    return pod, lease, report


def pathless_and_shadowed_runtime(h):
    layout = h.work / "pathless-runtime"
    compressed, diff = delivery.tar_layer([
        ("app/", b"", 0o755), ("app/runtime-only", b"must-not-leak", 0o644),
        ("etc/", b"", 0o755), ("etc/runtime-sentinel", b"approved-runtime", 0o644),
        ("tmp/", b"", 0o1777)])
    layer = delivery.blob(layout, compressed, delivery.LAYER)
    config = delivery.json_blob(layout, {"architecture": h.arch, "os": "linux",
        "config": {"Env": [], "User": "0:0", "Entrypoint": ["/ignored"]},
        "rootfs": {"type": "layers", "diff_ids": [diff]}}, delivery.CONFIG)
    selected = delivery.finish_layout(layout, delivery.json_blob(layout, {
        "schemaVersion": 2, "mediaType": delivery.MANIFEST, "config": config, "layers": [layer]}, delivery.MANIFEST))
    source = h.delivery.publish(layout, selected, "upstream/pathless-shadow")
    rid = "pathless-" + h.run_id
    h.apply({"apiVersion": API, "kind": "RuntimeImage", "metadata": {"name": rid},
             "spec": {"source": {"image": source}, "user": {"uid": 12345, "gid": 23456}}})
    profile = h.get("nodeprofile", h.profile)
    spec = copy.deepcopy(profile["spec"])
    spec["runtimeImages"].append({"id": rid, "install": "OnDemand"})
    h.replace_inventory_spec(profile, spec)
    h.wait("pathless numeric runtime offered", lambda: h.advertised(rid, "available"))
    pod = h.delivery.native_pod(h.arch, "pathless-static")
    pod["metadata"]["annotations"]["brewlet.sh/runtime-image"] = rid
    h.apply(pod)
    running = h.wait("absolute static process starts with no accounts or PATH", lambda: h.ready("pathless-static"))
    lease = h.lease(running)
    report = json.loads(h.exec("pathless-static", ["/app/bin/native", "--inspect"]).stdout)
    env = dict(entry.split("=", 1) for entry in report["env"])
    require(report["uid"] == 12345 and report["gid"] == 23456 and env["HOME"] == "/tmp"
            and "PATH" not in env, "pathless numeric identity adopted CRI/user defaults")
    require(h.exec("pathless-static", ["/app/bin/native", "--read", "/app/runtime-only"]).stdout.strip() == "ABSENT",
            "runtime app content merged into application")
    require(h.exec("pathless-static", ["/app/bin/native", "--read", "/etc/runtime-sentinel"]).stdout == "approved-runtime",
            "application boundary hid unrelated approved runtime bytes")
    require(h.exec("pathless-static", ["/app/bin/native", "--write", "/app/forbidden"], check=False).returncode == 7,
            "packaged app became writable")
    h.exec("pathless-static", ["/app/bin/native", "--write", "/app/new-volume-target/allowed"])
    failed = h.exec("pathless-static", ["native", "--probe"], check=False)
    require(failed.returncode != 0 and "RuntimePATHRequired" in failed.stderr + failed.stdout,
            "relative Exec adopted default/cwd search")
    bad = h.delivery.native_pod(h.arch, "pathless-relative")
    bad["metadata"]["annotations"]["brewlet.sh/runtime-image"] = rid
    bad["spec"]["containers"][0].update(command=["native"], args=["--probe"])
    h.apply(bad)
    h.no_start("pathless-relative", "RuntimePATHRequired")
    h.record("actual numeric pathless execution, isolated runtime app, readonly and explicit volume parity",
             {"taskID": lease["taskID"], "runtime": lease["runtime"], "report": report,
              "syntheticContractFixture": True, "notAnUnchangedUpstreamClaim": True}, ["UP-01", "UP-02", "UP-03"])


def recovery(h, original):
    name, pod, lease = original
    lifecycle = Lifecycle(h)
    token = "upstream-held-" + h.run_id
    marker = "/app/new-volume-target/" + token
    script = ("import pathlib,sys,time; p=pathlib.Path(sys.argv[1]); "
              "p.with_suffix('.started').touch()\n"
              "while not p.exists(): time.sleep(0.1)\n"
              "p.with_suffix('.finished').write_text(sys.argv[2])")
    h.node_exec("ctr", "-n", "k8s.io", "tasks", "exec", "--exec-id", token,
                "--detach", "--user", "65532:65532", lease["taskID"],
                EXECUTABLES["python"], "-c", script, marker, token)
    def held_started():
        return h.exec(name, [EXECUTABLES["python"], "-c",
            "import pathlib,sys;sys.exit(not pathlib.Path(sys.argv[1]).exists())",
            marker + ".started"], check=False).returncode == 0
    h.wait("actual shell-free held Exec starts", held_started, timeout=60)
    candidates = []
    for pid in lifecycle.task_pids(lease["taskID"]):
        observed = h.node_exec("cat", f"/proc/{pid}/cmdline", check=False)
        if observed.returncode:
            require(observed.returncode == 1 and h.node_exec(
                "test", "!", "-d", f"/proc/{pid}", check=False).returncode == 0,
                "cannot inspect actual task process identity")
            continue
        if token in observed.stdout.split("\0"):
            candidates.append(pid)
    require(len(candidates) == 1, "held Exec lacks an unambiguous actual host process identity")
    pid = candidates[0]
    identity = {"start": h.node_exec("cat", f"/proc/{pid}/stat").stdout.rsplit(") ", 1)[1].split()[19],
                "executable": h.node_exec("readlink", f"/proc/{pid}/exe").stdout.strip()}
    runtime = lease["runtime"]
    rid = IDS["python"]
    require(runtime["id"] == rid and runtime["inventoryUID"] == h.owned[("runtimeimage", "", rid)]
            and type(runtime["generation"]) is int and runtime["generation"] > 0,
            "upstream recovery lease is not the exact owned runtime epoch")
    root = f'{STORE}/{rid}/{runtime["inventoryUID"]}/{runtime["generation"]}/rootfs'
    require(re.search(re.escape(root) + r"(?=$|[/,: \t\n])",
                      h.node_exec("cat", f"/proc/{pid}/mountinfo").stdout),
            "held upstream Exec does not actually mount its leased root")
    durable = f'{STORE}/.tasks/{lease["taskID"]}/lease.json'
    require(lifecycle.read(durable) == lease, "durable upstream lease changed before recovery")
    worker = lifecycle.worker()
    lifecycle.inspect(worker["cid"], pod_uid=worker["podUID"])
    h.node_exec("crictl", "stop", "--timeout", "10", worker["cid"])
    successor = h.wait("fresh upstream provisioner container", lambda:
        lifecycle.worker_if_running(worker["cid"]))
    def recovered():
        output = lifecycle.worker_log_text(successor)
        for line in output.splitlines():
            if "runtime-lease-recovery: " not in line:
                continue
            report = json.loads(line.split("runtime-lease-recovery: ", 1)[1])
            if report.get("complete") is True and lease["taskID"] in (report.get("retainedTasks") or []):
                require(not report.get("error") and lease["taskID"] not in (report.get("removedTasks") or []),
                        "fresh upstream recovery reported ambiguous/error evidence")
                return report
        return False
    observation = h.wait("fresh worker recovers actual unchanged-upstream Exec lease", recovered)
    require(lifecycle.read(durable) == lease and lifecycle.orphan_pids({pid: identity}) == [pid],
            "fresh upstream worker changed the lease or lost the active Exec")
    maintenance = {"predecessor": worker, "successor": successor, "taskID": lease["taskID"],
                   "lease": lease, "successfulRecovery": observation,
                   "proofBoundary": "fresh fenced worker; installed-shim actual task/mount evidence"}
    daemon = int(h.node_exec("systemctl", "show", "containerd", "--property=MainPID", "--value").stdout.strip())
    start = h.node_exec("cat", f"/proc/{daemon}/stat").stdout.rsplit(") ", 1)[1].split()[19]
    executable = h.node_exec("readlink", f"/proc/{daemon}/exe").stdout.strip()
    h.node_exec(*daemon_restart_command(daemon, start, executable), timeout=90)
    h.wait("fresh daemon recovers upstream task service", lambda:
           h.node_exec("crictl", "info", check=False).returncode == 0)
    current = h.wait("unchanged upstream task survives daemon restart", lambda: h.ready(name))
    require(h.lease(current) == lease and current["metadata"]["uid"] == pod["metadata"]["uid"],
            "recovery replaced upstream task rather than preserving lease")
    require(lifecycle.orphan_pids({pid: identity}) == [pid]
            and h.node_exec("readlink", f"/proc/{pid}/exe").stdout.strip() == identity["executable"],
            "restart lost or replaced the active shell-free Exec")
    inspect_report(h, name, "python")
    h.exec(name, [EXECUTABLES["python"], "-c",
                  "import pathlib,sys;pathlib.Path(sys.argv[1]).touch()", marker])
    h.wait("held upstream Exec finishes its own code after both restarts", lambda:
        h.exec(name, [EXECUTABLES["python"], "-c",
            "import pathlib,sys;print(pathlib.Path(sys.argv[1]).read_text())",
            marker + ".finished"], check=False).stdout.strip() == token)
    h.wait("actual recovered Exec process drains", lambda: not lifecycle.orphan_pids({pid: identity}))
    maintenance["heldExec"] = {"pid": pid, **identity, "survivedBothRestarts": True,
                               "completedApplicationCode": True,
                               "transportBoundary": "detached Exec; no claim that CRI streams survive daemon restart"}
    h.record("real worker and daemon restart preserve unchanged-upstream task and Exec state",
             maintenance, ["UP-05"])


def exercise(h):
    originals = {}
    for language in h.languages:
        name = language + "-upstream-old"
        h.apply(h.pod(name, language))
        h.wait("unchanged upstream " + language + " starts", lambda name=name: h.ready(name))
        pod, lease, report = inspect_report(h, name, language)
        source = h.delivery.metadata[language, "before"]["descriptor"]["digest"]
        require(lease["runtime"]["sourceDigest"] == source, "approved root is not the unchanged upstream digest")
        require(not h.node_exec("test", "-e", STORE + "/" + IDS[language] + "/current/rootfs/bin/sh", check=False).returncode == 0,
                "a shell was installed into unchanged upstream root")
        originals[language] = name, pod, lease, report
        h.record(language + " unchanged upstream dependencies, HTTP, identity and direct Exec/probe parity",
                 {"podUID": pod["metadata"]["uid"], "lease": lease, "report": report}, ["UP-05"])
    for language in h.languages:
        name, pod, lease, report = originals[language]
        image = h.get("runtimeimage", IDS[language])
        spec = copy.deepcopy(image["spec"])
        spec["source"]["image"] = h.runtime_images[language]["after"]
        h.replace_inventory_spec(image, spec)
        h.wait(language + " unchanged upstream target becomes current", lambda language=language:
               h.advertised(IDS[language], "ready") and h.current(language)["sourceDigest"] ==
               h.runtime_images[language]["after"].split("@")[1])
        inspect_report(h, name, language, runtime=report["runtime"])
        require(h.lease(h.ready(name)) == lease, "rotation changed existing upstream lease")
        new = language + "-upstream-new"
        h.apply(h.pod(new, language))
        h.wait(language + " replacement uses updated upstream root", lambda new=new: h.ready(new))
        _, updated, next_report = inspect_report(h, new, language)
        require(updated["runtime"]["generation"] > lease["runtime"]["generation"]
                and updated["runtime"]["sourceDigest"] == spec["source"]["image"].split("@")[1],
                "new task did not use the actual updated upstream generation")
        override = language + "-upstream-override"
        obj = h.pod(override, language, overrides=True)
        obj["spec"]["containers"][0]["workingDir"] = "/tmp"
        h.apply(obj)
        h.wait(language + " normal Kubernetes overrides execute", lambda override=override: h.ready(override))
        inspect_report(h, override, language, arguments=["two words"], cwd="/tmp")
        h.record(language + " real upstream root update preserves artifact and old lease with native overrides",
                 {"applicationDigest": h.app_images[language], "before": lease["runtime"],
                  "after": updated["runtime"], "beforeVersion": report["runtime"],
                  "afterVersion": next_report["runtime"], "noRuntimeImageDerivation": True}, ["UP-05"])

    h.apply({"apiVersion": API, "kind": "RuntimeImage", "metadata": {"name": h.delivery.static_id},
             "spec": {"source": {"image": h.delivery.static_source}, "user": {"uid": 65532, "gid": 65532}}})
    profile = h.get("nodeprofile", h.profile)
    spec = copy.deepcopy(profile["spec"])
    spec["runtimeImages"].append({"id": h.delivery.static_id, "install": "OnDemand"})
    h.replace_inventory_spec(profile, spec)
    h.wait("unchanged distroless static root offered", lambda: h.advertised(h.delivery.static_id, "available"))
    h.apply(h.delivery.native_pod(h.arch, "upstream-static"))
    pod = h.wait("native app starts on unchanged upstream static root", lambda: h.ready("upstream-static"))
    require(h.exec("upstream-static", ["/app/bin/native", "--probe"]).stdout.strip() == "NATIVE linux/" + h.arch,
            "unchanged static root did not execute actual native application")
    h.record("unchanged upstream static image executes the generic native artifact without a shell",
             {"lease": h.lease(pod), "source": h.result["upstreamRuntimes"]["static"]["after"]["source"]}, ["UP-05"])
    pathless_and_shadowed_runtime(h)
    recovery(h, originals["python"][:3])

    h.delete("runtimeimage", IDS["python"])
    h.wait("upstream revocation remains blocked by actual leases", lambda:
           h.get("runtimeimage", IDS["python"])["metadata"].get("deletionTimestamp"))
    inspect_report(h, originals["python"][0], "python", runtime=originals["python"][3]["runtime"])
    h.bypass_admission()
    try:
        h.apply(h.pod("upstream-revoked", "python", bound=True))
        h.no_start("upstream-revoked", "RuntimeImageRevoked")
    finally:
        h.restore_admission()
    h.record("unchanged upstream revocation preserves running lease and rejects a bound unauthorized start",
             {"lease": originals["python"][2]}, ["UP-05"])

    h.delete("nodeprofile", h.profile)
    time.sleep(3)
    profile = h.get("nodeprofile", h.profile)
    require(profile["metadata"].get("finalizers") and profile["metadata"].get("deletionTimestamp"),
            "active upstream tasks lost their cleanup obligation")
    for pod in h.get("pods", namespace=h.namespace)["items"]:
        h.delete("pod", pod["metadata"]["name"], h.namespace)
    h.wait("all actual upstream tasks drain", lambda: not h.get("pods", namespace=h.namespace)["items"])
    h.wait("upstream production cleanup finalizes", lambda: h.get("nodeprofile", h.profile, absent=True) is None)
    require("brewlet.sh/owner-uid" not in h.node_snapshot()["metadata"].get("labels", {}),
            "upstream cleanup retained node claim")
    h.node_exec("test", "!", "-e", "/usr/local/bin/containerd-shim-brewlet-v2")
    h.node_exec("crictl", "info")
    h.record("normal unchanged-upstream lease drain, host restoration, worker teardown and claim cleanup",
             {"nodeUID": h.node_uid, "profileUID": profile["metadata"]["uid"]}, ["UP-05"])


def run(work):
    h = Harness(work, pinned=False)
    h.languages = ("java", "python", "node")
    h.runtime_ids = {language: IDS[language] for language in h.languages}
    h.result.update(selectedScenario="unchanged-upstream-distroless", selectedLanguages=list(h.languages))
    h.result["gaps"].append("Focused upstream suite; full-userland controls and complete proposal are separate.")
    h.launch_files = {}
    for language in h.languages:
        launch = json.loads((FIXTURES / language / "launch.json").read_text())
        launch["process"]["entrypoint"] = [EXECUTABLES[language]]
        h.launch_files[language] = h.work / (language + "-launch.json")
        h.launch_files[language].write_text(json.dumps(launch))
    h.build_runtime_images = lambda: None
    status = 1
    try:
        for phase, action in (("prerequisites", h.preflight), ("cluster", h.provision_cluster),
                              ("build", h.prepare_images)):
            h.phase = phase
            action()
        h.phase = "upstream-import-and-delivery"
        h.delivery = UpstreamDelivery(h, json.loads(PINS.read_text()))
        h.delivery.bootstrap()
        h.result.pop("runtimeVendorInputs", None)
        h.phase = "install"
        h.install()
        h.startup()
        h.phase = "upstream-contract"
        exercise(h)
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
            errors.append("upstream fixture teardown: " + str(error))
        errors.extend(h.destroy())
        if errors:
            h.result.update(outcome="failed", cleanupErrors=errors)
            status = 1
        h.save()
    return status


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", required=True, type=Path)
    def interrupted(signum, frame):
        raise Failure("upstream acceptance interrupted by signal " + str(signum))
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    raise SystemExit(run(parser.parse_args().work))
