#!/usr/bin/env python3
"""Real Wasmtime/Spin examples on the existing Brewlet/runc path."""

import argparse
import copy
import hashlib
import json
import re
import shutil
import signal
import sys
import time
from pathlib import Path

import delivery
from harness import API, FENCE, KIND_VERSION, OWNER, ROOT, STORE, WASM_HOSTS, Failure, Harness, artifact_target, require
from lifecycle import Lifecycle, daemon_restart_command
from wasm_inputs import EXAMPLE, build

_RUNTIME_IDS = {"wasmtime": "wasmtime-wasip1", "spin": "spin-http-sdk7"}
IDS = {host: _RUNTIME_IDS[host] for host in WASM_HOSTS}


def application_manifest(layout):
    descriptor = json.loads((layout / "index.json").read_text())["manifests"][0]
    target = json.loads(delivery.read_blob(layout, descriptor))
    if descriptor["mediaType"] == delivery.INDEX:
        require(len(target["manifests"]) == 1, "explicit single-platform Wasm artifact required")
        target = json.loads(delivery.read_blob(layout, target["manifests"][0]))
    require(target.get("mediaType") == delivery.MANIFEST and target.get("layers"),
            "runnable OCI application manifest required")
    return target


class WasmHarness(Harness):
    def __init__(self, work):
        super().__init__(work, pinned=False)
        self.languages = tuple(IDS)
        self.runtime_ids = dict(IDS)
        self.probe_language = "wasmtime"
        self.result.update(selectedScenario="wasm-runc-examples", selectedLanguages=list(IDS))
        self.result["gaps"] += [
            "Preimported OCI graphs; not remote registry authentication/TLS proof.",
            "Custom platform runtime roots: pinned distroless C/C++ userland plus unchanged pinned release binaries.",
            "Go WASIp1 commands and Rust wasm32-wasip2/Spin SDK 7 HTTP components only; not arbitrary WASI interfaces or native runwasi qualification.",
            "Guest filesystem/environment checks; no claim of a complete guest-network capability test.",
        ]

    def preflight(self):
        for tool in ("docker", "kind", "kubectl", "helm", "go", "python3", "cargo", "rustup"):
            require(shutil.which(tool), "mandatory Wasm example prerequisite missing: " + tool)
        require(sys.version_info >= (3, 12), "Python 3.12+ required")
        require(self.run(["kind", "version"]).stdout.split()[1] == KIND_VERSION, "pinned kind required")
        require(self.run(["docker", "info", "--format", "{{.OSType}}"]).stdout.strip() == "linux",
                "Linux Docker engine required")
        arch = self.run(["docker", "info", "--format", "{{.Architecture}}"]).stdout.strip()
        self.arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(arch)
        require(self.arch is not None, "native amd64/arm64 Docker required")
        self.result.update(architecture=self.arch, checkout=self.run(["git", "rev-parse", "HEAD"]).stdout.strip())
        self.result["inputFileSHA256"] = {
            str(p.relative_to(ROOT)): hashlib.sha256(p.read_bytes()).hexdigest()
            for p in sorted(EXAMPLE.rglob("*")) if p.is_file()
        }
        self.record("explicit Wasm source inputs and native prerequisites", {}, ["WASM-01", "WASM-02"])

    def import_named(self, layout, descriptor, tag):
        descriptor = copy.deepcopy(descriptor)
        descriptor["annotations"] = {"org.opencontainers.image.ref.name": tag}
        delivery.finish_layout(layout, descriptor)
        self.import_layout(layout)
        require(self.image_digest(tag) == descriptor["digest"], "imported OCI target changed")
        canonical = tag.rsplit(":", 1)[0] + "@" + descriptor["digest"]
        self.ctr("images", "tag", tag, canonical)
        return canonical

    def prepare(self):
        self.prepare_cli()
        self.prepare_components()
        self.runtime_images = {name: {} for name in IDS}
        for name, revisions in self.inputs.items():
            for revision, source in revisions.items():
                tag = f"registry.brewlet.test/{self.run_id}/wasm/{name}:{revision}"
                source["image"] = self.import_named(Path(source["layout"]), source["descriptor"], tag)
                self.runtime_images[name][revision] = source["image"]
        deps = self.work / "dependency-input"
        deps.mkdir(mode=0o755)
        (deps / "message.txt").write_bytes((EXAMPLE / "dependencies/message.txt").read_bytes())
        sets = self.work / "dependency-set"
        self.run([*self.cli_command, "layer-set", "example.invalid/wasm-dependencies:v1",
                  "--store", sets, "--config", EXAMPLE / "layerset.json",
                  "--layer", f"dependency={deps}:/app/dependencies"], timeout=180)
        set_target = json.loads((sets / "index.json").read_text())["manifests"][0]
        set_manifest = json.loads(delivery.read_blob(sets, set_target))
        expected = [d["digest"] for d in set_manifest["layers"]
                    if d.get("annotations", {}).get("brewlet.sh/layer") == "dependency"]
        require(expected, "independent dependency set has no payload")
        for name in IDS:
            app = self.work / (name + "-app")
            app.mkdir(mode=0o755)
            guest_file = "spin-guest.wasm" if name == "spin" else "guest.wasm"
            (app / "guest.wasm").write_bytes((self.work / guest_file).read_bytes())
            if name == "spin":
                (app / "spin.toml").write_bytes((EXAMPLE / "spin.toml").read_bytes())
            launch = EXAMPLE / (name + "-launch.json")
            layout = self.work / (name + "-application")
            tag = f"registry.brewlet.test/{self.run_id}/wasm-app/{name}:v1"
            self.run([*self.cli_command, "push", tag, "--store", layout,
                      "--platform", "linux/" + self.arch, "--launch", launch,
                      "--layer-set", sets, "--layer", f"app={app}:/app"], timeout=180)
            digest, _ = artifact_target(layout)
            outer = json.loads((layout / "index.json").read_text())["manifests"][0]
            manifest = application_manifest(layout)
            actual = [d["digest"] for d in manifest["layers"] if d["annotations"]["brewlet.sh/layer"] == "dependency"]
            require(actual == expected and "brewlet.sh/layerset-evidence" in manifest["annotations"],
                    "managed Wasm dependency graph was not reused byte-for-byte")
            self.app_images[name] = self.import_named(layout, outer, tag)
            require(self.app_images[name].endswith("@" + digest), "application target mismatch")
            self.record(name + " explicit app and independent dependency layer set composed",
                        {"image": self.app_images[name], "dependencyDigests": actual}, ["SCHEMA-01"])
        self.result.update(applicationImages=self.app_images, runtimeImages=self.runtime_images)
        launch = json.loads((EXAMPLE / "wasmtime-launch.json").read_text())
        launch["runtimeImages"] = []
        empty_file = self.work / "empty-launch.json"
        empty_file.write_text(json.dumps(launch))
        layout = self.work / "empty-application"
        tag = f"registry.brewlet.test/{self.run_id}/wasm-app/empty:v1"
        self.run([*self.cli_command, "push", tag, "--store", layout,
                  "--platform", "linux/" + self.arch, "--launch", empty_file,
                  "--layer-set", sets, "--layer", f"app={self.work / 'wasmtime-app'}:/app"], timeout=180)
        root = json.loads((layout / "index.json").read_text())["manifests"][0]
        self.empty_image = self.import_named(layout, root, tag)
        legacy = json.loads((EXAMPLE / "wasmtime-launch.json").read_text())
        legacy["requires"] = {"runtimeImages": legacy.pop("runtimeImages")}
        legacy_file = self.work / "legacy-launch.json"
        legacy_file.write_text(json.dumps(legacy))
        rejected = self.run([*self.cli_command, "push", "example.invalid/legacy:v1",
            "--store", self.work / "rejected-legacy", "--platform", "linux/" + self.arch,
            "--launch", legacy_file, "--layer", f"app={self.work / 'wasmtime-app'}:/app"],
            check=False, timeout=180)
        require(rejected.returncode and "InvalidLaunchConfig" in rejected.stdout + rejected.stderr
                and not (self.work / "rejected-legacy/index.json").exists(),
                "producer accepted or published the removed requires wrapper")
        layout = self.work / "legacy-shim-application"
        root = delivery.mutate_application(self.work / "wasmtime-application", layout,
            self.run_id + "-legacy", fault="legacy-requires")
        self.legacy_image = self.import_named(layout, root,
            f"registry.brewlet.test/{self.run_id}/wasm-app/legacy:v1")
        self.record("producer rejects removed wrapper and preserves explicit empty top-level allowlist",
                    {"emptyImage": self.empty_image}, ["SCHEMA-01"])

    def install(self):
        self.kubectl("apply", "-f", ROOT / "kubernetes/deploy/crds", timeout=90)
        self.run(["helm", "--kubeconfig", self.kubeconfig, "--kube-context", self.context,
                  "upgrade", "--install", self.cluster, ROOT / "kubernetes/charts/brewlet",
                  "--namespace", self.control_namespace, "--wait", "--timeout", "180s",
                  "--set-string", "operator.image=" + self.result["controlImage"],
                  "--set-string", "admission.image=" + self.result["controlImage"],
                  "--set-string", "provisioner.image=" + self.result["workerImage"],
                  "--set-string", "provisioner.gc.grace=20s",
                  "--set-string", "provisioner.gc.sweepInterval=2s"], timeout=240)
        self.wait_inventory_admission(self.runtime_images["wasmtime"]["before"])
        for name, rid in IDS.items():
            self.apply({"apiVersion": API, "kind": "RuntimeImage", "metadata": {"name": rid},
                        "spec": {"source": {"image": self.runtime_images[name]["before"]},
                                 "volumeMountPaths": ["/cache", "/work"],
                                 "user": {"uid": 65532, "gid": 65532}}})
        self.kubectl("label", "node", self.node, "brewlet.test/pool=" + self.run_id)
        self.apply({"apiVersion": API, "kind": "NodeProfile", "metadata": {"name": self.profile},
                    "spec": {"nodePool": {"key": "brewlet.test/pool", "names": [self.run_id],
                                         "includeControlPlane": True},
                             "runtimeImages": [{"id": rid, "install": "Eager"} for rid in IDS.values()],
                             "tolerations": [{"key": FENCE, "operator": "Equal", "value": "provisioning",
                                              "effect": "NoSchedule"}],
                             "rollout": {"maxUnavailable": 1, "containerdRestart": "validated"}}})
        self.wait("both Wasm hosts validated and installed", lambda:
                  all(self.advertised(rid, "ready") for rid in IDS.values()))
        self.record("approved numeric Wasm runtime inventory on an Eager owned pool", {}, ["WASM-01", "WASM-02"])

    def pod(self, name, engine, *, args=None, image=None, selected=None, once=False):
        container = {"name": "app", "image": image or self.app_images[engine], "imagePullPolicy": "Never",
                     "env": [{"name": "HOST_ONLY", "value": "must-not-reach-guest"},
                             {"name": "PATH", "value": "/app/forbidden"}],
                     "securityContext": {"readOnlyRootFilesystem": True},
                     "volumeMounts": [{"name": "work", "mountPath": "/work"},
                                      {"name": "cache", "mountPath": "/cache"}],
                     "resources": {"requests": {"cpu": "10m", "memory": "32Mi"},
                                   "limits": {"cpu": "1", "memory": "512Mi"}}}
        if args is not None:
            container["args"] = args
        if not once:
            container["readinessProbe"] = ({"httpGet": {"path": "/", "port": 8000}, "periodSeconds": 5,
                                            "timeoutSeconds": 10} if engine == "spin" else
                {"exec": {"command": self.probe()}, "periodSeconds": 10, "timeoutSeconds": 10})
        return {"apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": name, "namespace": self.namespace,
                             "annotations": {"brewlet.sh/runtime-image": selected or IDS[engine]}},
                "spec": {"runtimeClassName": "brewlet", "restartPolicy": "Never",
                         "nodeSelector": {"kubernetes.io/arch": self.arch, "brewlet.test/pool": self.run_id},
                         "containers": [container], "volumes": [
                             {"name": "work", "emptyDir": {}}, {"name": "cache", "emptyDir": {}}]}}

    def probe(self, *args):
        launch = json.loads((EXAMPLE / "wasmtime-launch.json").read_text())["process"]
        return launch["entrypoint"] + launch["args"][:-2] + list(args or ("probe", "two words"))


def guest(report, dependency="shared-wasm-dependency"):
    require(report["message"] == "Hello from Wasm" and report["dependency"].strip() == dependency,
            "actual guest/dependency not executing")
    require(report["visible"] == "explicit" and report["hostOnly"] == ""
            and report["runtimeFileHidden"] and report["engineCacheHidden"] and report["dependencyWriteDenied"],
            "guest filesystem/environment boundary failed")


def inspect(h, name, engine, version, dependency="shared-wasm-dependency"):
    pod = h.ready(name)
    require(pod, "positive Wasm workload not ready")
    lease = h.lease(pod)
    detail = json.loads(h.node_exec("crictl", "inspect", lease["taskID"]).stdout)
    require(detail["status"]["id"] == lease["taskID"]
            and detail["status"]["labels"]["io.kubernetes.pod.uid"] == pod["metadata"]["uid"],
            "actual Wasm task/Pod UID differs")
    pid = detail["info"]["pid"]
    require(type(pid) is int and pid > 1, "actual Wasm host PID unavailable")
    require(h.node_exec("cat", f"/proc/{pid}/root/etc/runtime-sentinel").stdout == "host-only-runtime-data",
            "guest-hidden fixture file is not actually present in the host runtime")
    h.node_exec("test", "-d", f"/proc/{pid}/root/cache")
    status = h.node_exec("cat", f"/proc/{pid}/status").stdout
    fields = dict(line.split(":", 1) for line in status.splitlines() if ":" in line)
    require(fields["Uid"].split() == ["65532"] * 4 and fields["Gid"].split() == ["65532"] * 4,
            "Wasm host is not the approved numeric user/group")
    require(fields["NoNewPrivs"].strip() == "1" and all(
        int(fields[key].strip(), 16) == 0 for key in ("CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb")),
        "Wasm host privilege restrictions were weakened")
    environment = h.node_exec("cat", f"/proc/{pid}/environ").stdout.split("\0")
    require(not any(entry.startswith("PATH=") for entry in environment), "pathless runtime adopted CRI PATH")
    output = h.exec(name, ["/usr/local/bin/" + engine, "--version"]).stdout
    require(re.search(r"\b" + re.escape(version) + r"(?![\d.])", output),
            "Exec silently changed host version: " + output)
    if engine == "spin":
        report = h.http(name)
    else:
        report = json.loads(h.exec(name, h.probe()).stdout)
        require(report["arguments"] == ["probe", "two words"], "guest argv changed")
    guest(report, dependency)
    return pod, lease, {"pid": pid, "version": output.strip(), "guest": report}


def recover(h, original):
    name, pod, lease, _ = original
    lifecycle = Lifecycle(h)
    token = "wasm-exec-" + h.run_id
    h.node_exec("ctr", "-n", "k8s.io", "tasks", "exec", "--exec-id", token, "--detach",
                "--user", "65532:65532", lease["taskID"], *h.probe("hold", token))
    def held_pid():
        found = []
        for pid in lifecycle.task_pids(lease["taskID"]):
            result = h.node_exec("cat", f"/proc/{pid}/cmdline", check=False)
            if result.returncode:
                require(result.returncode == 1 and h.node_exec(
                    "test", "!", "-d", f"/proc/{pid}", check=False).returncode == 0,
                    "task process identity unavailable")
                continue
            if token in result.stdout.split("\0"):
                found.append(pid)
        require(len(found) <= 1, "ambiguous held Wasm Exec")
        return found[0] if found else False
    pid = h.wait("real held Wasm Exec host PID", held_pid, timeout=60)
    identity = {"start": h.node_exec("cat", f"/proc/{pid}/stat").stdout.rsplit(") ", 1)[1].split()[19],
                "executable": h.node_exec("readlink", f"/proc/{pid}/exe").stdout.strip()}
    def guest_started():
        require(lifecycle.orphan_pids({pid: identity}) == [pid], "held Wasm host exited before guest hold")
        result = h.node_exec("test", "-f", f"/proc/{pid}/root/work/{token}-started", check=False)
        require(result.returncode in (0, 1), "held Wasm guest start evidence unavailable")
        return result.returncode == 0
    h.wait("held guest actually enters its loop", guest_started, timeout=60)
    runtime = lease["runtime"]
    root = f'{STORE}/{runtime["id"]}/{runtime["inventoryUID"]}/{runtime["generation"]}/rootfs'
    require(re.search(re.escape(root) + r"(?=$|[/,: \t\n])",
                      h.node_exec("cat", f"/proc/{pid}/mountinfo").stdout),
            "held Wasm guest host does not actually mount its leased root")
    durable = f'{STORE}/.tasks/{lease["taskID"]}/lease.json'
    require(lifecycle.read(durable) == lease, "Wasm durable lease differs")
    worker = lifecycle.worker()
    lifecycle.inspect(worker["cid"], pod_uid=worker["podUID"])
    h.node_exec("crictl", "stop", "--timeout", "10", worker["cid"])
    successor = h.wait("actual fenced worker restart", lambda: lifecycle.worker_if_running(worker["cid"]))
    def recovered():
        for line in lifecycle.worker_log_text(successor).splitlines():
            if "runtime-lease-recovery: " in line:
                report = json.loads(line.split("runtime-lease-recovery: ", 1)[1])
                if report.get("complete") is True and lease["taskID"] in (report.get("retainedTasks") or []):
                    require(not report.get("error") and lease["taskID"] not in (report.get("removedTasks") or []),
                            "ambiguous Wasm recovery evidence")
                    return report
        return False
    proof = h.wait("fresh worker recovers actual Wasm task/Exec lease", recovered)
    daemon = int(h.node_exec("systemctl", "show", "containerd", "--property=MainPID", "--value").stdout.strip())
    start = h.node_exec("cat", f"/proc/{daemon}/stat").stdout.rsplit(") ", 1)[1].split()[19]
    executable = h.node_exec("readlink", f"/proc/{daemon}/exe").stdout.strip()
    h.node_exec(*daemon_restart_command(daemon, start, executable), timeout=90)
    h.wait("CRI healthy after guarded daemon restart", lambda:
           h.node_exec("crictl", "info", check=False).returncode == 0)
    require(lifecycle.orphan_pids({pid: identity}) == [pid] and lifecycle.read(durable) == lease
            and h.node_exec("readlink", f"/proc/{pid}/exe").stdout.strip() == identity["executable"],
            "worker/daemon restart replaced the active Wasm Exec or lease")
    guest(json.loads(h.exec(name, h.probe("release", token)).stdout))
    h.wait("recovered held Wasm Exec drains normally", lambda: not lifecycle.orphan_pids({pid: identity}))
    h.record("actual Wasm task and detached Exec survive worker/daemon restart and drain",
             {"pid": pid, "identity": identity, "recovery": proof,
              "streamingContinuityClaimed": False}, ["WASM-03"])


def exercise(h):
    original = {}
    for engine in IDS:
        name = engine + "-old"
        h.apply(h.pod(name, engine))
        h.wait(engine + " real guest readiness", lambda name=name: h.ready(name))
        pod, lease, report = inspect(h, name, engine, h.inputs[engine]["before"]["releaseVersion"])
        original[engine] = name, pod, lease, report
        h.record(engine + " real guest, dependencies, numeric host security, Exec and capabilities",
                 {"lease": lease, "report": report}, ["WASM-01" if engine == "wasmtime" else "WASM-02"])
    for mode, code in (("probe", 0), ("exit37", 37)):
        name = "wasmtime-" + mode
        launch = json.loads((EXAMPLE / "wasmtime-launch.json").read_text())["process"]["args"][:-2]
        h.apply(h.pod(name, "wasmtime", args=launch + [mode], once=True))
        def terminated():
            obj = h.get("pod", name, h.namespace)
            for status in obj.get("status", {}).get("containerStatuses", []):
                if "terminated" in status.get("state", {}):
                    value = status["state"]["terminated"]
                    require(value["exitCode"] == code, "WASI command exit status changed")
                    return value
            return False
        ended = h.wait("real WASI command exit " + str(code), terminated)
        report = json.loads(h.kubectl("-n", h.namespace, "logs", name).stdout)
        guest(report)
        h.record("real WASI command preserves exit " + str(code), ended, ["WASM-01"])
    h.apply(h.pod("wrong-allowlist", "wasmtime", selected=IDS["spin"]))
    h.no_start("wrong-allowlist", "RuntimeImageIncompatible")
    h.apply(h.pod("empty-allowlist", "wasmtime", image=h.empty_image))
    h.no_start("empty-allowlist", "RuntimeImageIncompatible")
    h.apply(h.pod("legacy-wrapper", "wasmtime", image=h.legacy_image))
    h.no_start("legacy-wrapper", "InvalidLaunchConfig")
    h.record("real shim enforces flat allowlist membership/deny-all and rejects legacy wrapper",
             {}, ["SCHEMA-01"])
    relative = h.exec("wasmtime-old", ["wasmtime", "--version"], check=False)
    require(relative.returncode and "RuntimePATHRequired" in relative.stderr + relative.stdout,
            "pathless Wasm host adopted CRI executable search")
    for engine in IDS:
        obj = h.get("runtimeimage", IDS[engine])
        spec = copy.deepcopy(obj["spec"])
        spec["source"]["image"] = h.runtime_images[engine]["after"]
        h.replace_inventory_spec(obj, spec)
        h.wait(engine + " new source published", lambda engine=engine:
            h.advertised(IDS[engine], "ready") and json.loads(h.node_exec(
                "cat", f"{STORE}/{IDS[engine]}/current/metadata.json").stdout)["sourceDigest"]
            == h.runtime_images[engine]["after"].split("@")[1])
        old_name, _, old_lease, _ = original[engine]
        _, retained, old_report = inspect(h, old_name, engine, h.inputs[engine]["before"]["releaseVersion"])
        require(retained == old_lease, "runtime update changed an existing Wasm lease")
        new = engine + "-new"
        h.apply(h.pod(new, engine))
        h.wait(engine + " updated host actually executes", lambda new=new: h.ready(new))
        _, updated, report = inspect(h, new, engine, h.inputs[engine]["after"]["releaseVersion"])
        require(updated["runtime"]["generation"] > old_lease["runtime"]["generation"]
                and updated["runtime"]["sourceDigest"] == spec["source"]["image"].split("@")[1],
                "new Wasm task used stale bytes")
        h.record(engine + " real host-version rotation retains old lease and unchanged app",
                 {"before": old_lease, "after": updated, "beforeVersion": old_report["version"],
                  "afterVersion": report["version"], "application": h.app_images[engine]}, ["WASM-03"])
    recover(h, original["wasmtime"])
    for engine in IDS:
        inspect(h, engine + "-old", engine, h.inputs[engine]["before"]["releaseVersion"])
    h.delete("nodeprofile", h.profile)
    time.sleep(2)
    profile = h.get("nodeprofile", h.profile)
    require(profile["metadata"].get("deletionTimestamp") and profile["metadata"].get("finalizers"),
            "live Wasm workloads lost their cleanup obligation")
    for pod in h.get("pods", namespace=h.namespace)["items"]:
        h.delete("pod", pod["metadata"]["name"], h.namespace)
    h.wait("all actual Wasm Pods drain", lambda: not h.get("pods", namespace=h.namespace)["items"])
    h.wait("normal Wasm profile/host cleanup", lambda: h.get("nodeprofile", h.profile, absent=True) is None)
    require("brewlet.sh/owner-uid" not in h.node_snapshot()["metadata"].get("labels", {}),
            "normal Wasm cleanup retained owner claim")
    for rid in IDS.values():
        h.node_exec("test", "!", "-e", STORE + "/" + rid + "/current")
        h.delete("runtimeimage", rid)
        h.wait("normal Wasm inventory cleanup " + rid, lambda rid=rid:
               h.get("runtimeimage", rid, absent=True) is None)
    h.node_exec("test", "!", "-e", "/usr/local/bin/containerd-shim-brewlet-v2")
    h.node_exec("crictl", "info")
    h.record("normal Wasm lease drain, host reversal, worker teardown and claim cleanup", {}, ["WASM-03"])


def run(work):
    h = WasmHarness(work)
    status = 1
    try:
        h.phase = "wasm-inputs"
        h.preflight()
        h.inputs = build(h)
        for phase, action in (("cluster", h.provision_cluster), ("build", h.prepare),
                              ("install", h.install), ("wasm-execution", lambda: exercise(h))):
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
            if h.kubeconfig.exists():
                for key in list(h.owned):
                    if key[0] == "pod" and key[1] == h.namespace:
                        obj = h.get("pod", key[2], key[1], absent=True)
                        if obj is not None and obj.get("status", {}).get("containerStatuses"):
                            h.kubectl("-n", key[1], "logs", key[2], "--tail=100", check=False)
            h.diagnostics()
        except Exception as error:
            errors.append("Wasm diagnostics/helper cleanup: " + str(error))
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
        raise Failure("Wasm acceptance interrupted by signal " + str(signum))
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    raise SystemExit(run(parser.parse_args().work))
