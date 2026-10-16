#!/usr/bin/env python3
"""Private real-node Brewlet v2 acceptance; never uses the current kube context."""

import argparse
import base64
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import threading
import time
import uuid


ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
FIXTURES = ROOT / "integration-tests/fixtures"
KIND_VERSION = "v0.33.0"
NODE_IMAGE = "kindest/node@sha256:4613778f3cfcd10e615029370f5786704559103cf27bef934597ba562b269661"
REGISTRY_IMAGE = "registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e"
CLI_USERLAND = "docker.io/library/python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258"
OWNER = "acceptance.brewlet.test/run"
FENCE = "startup-taint.cluster-autoscaler.kubernetes.io/brewlet"
API = "node.brewlet.sh/v2alpha1"
LANGUAGES = ("java", "python", "node", "dotnet")
IDS = {language: "fixture-" + language for language in LANGUAGES}
STORE = "/opt/brewlet/runtimes"
UNIMPLEMENTED = [
    "E2E-01 invalid/deleting/conflicting zero-node profile permutations (separate admission suite)",
    "E2E-05 actual late-arriving alternate-architecture capacity and unsupported-platform pull",
    "E2E-06 OnDemand provider pin refusal and stale-pin retention through production uninstall",
    "E2E-08 inventory delete/recreate epoch with real old roots; all-ID quarantine recovery",
    "E2E-09 injected resourceVersion race and recreated-Node conditional claim release",
    "LEASE-02 shim restart recovery and LEASE-03 orphaned exec after initial task exit",
    "OD-06 idle collection/mode-switch pin rejection and INV-06 offline authorization expiry",
    "ROT-01/06/08/09 automatic workload restart pacing, OnDelete/partitioned/paused controllers",
    "Maven-produced artifact on-node (generic CLI artifacts only here; producer suite is separate)",
    "Registry kubelet pull, private authentication and production TLS transport",
    "Real vendor security patch (rotation here changes approved image Env, not interpreter version)",
]


class Failure(RuntimeError):
    pass


def require(value, message):
    if not value:
        raise Failure(message)


def rotation_inputs(data):
    require(isinstance(data, dict) and set(data) == set(LANGUAGES),
            "rotation inputs must cover exactly the four fixture languages")
    for language, value in data.items():
        require(isinstance(value, dict) and set(value) == {"probe", "before", "after"}
                and isinstance(value["probe"], list) and value["probe"]
                and all(isinstance(arg, str) and arg and "\0" not in arg for arg in value["probe"]),
                "invalid fixture runtime probe: " + language)
        for revision in ("before", "after"):
            item = value[revision]
            require(isinstance(item, dict) and set(item) == {"image", "version"}
                    and isinstance(item["image"], str)
                    and re.fullmatch(r"[a-z0-9./-]+@sha256:[0-9a-f]{64}", item["image"])
                    and isinstance(item["version"], str) and item["version"].strip(),
                    "invalid pinned vendor runtime: " + language + "/" + revision)
        require(value["before"]["image"] != value["after"]["image"]
                and value["before"]["version"] != value["after"]["version"],
                "rotation requires different vendor bytes and versions: " + language)
    return data


def runtime_version_matches(output, expected):
    return re.search(r"(?<![A-Za-z0-9.])" + re.escape(expected.strip())
                     + r"(?![A-Za-z0-9.])", output) is not None


def validate_run_id(value):
    require(bool(re.fullmatch(r"[0-9a-f]{12}", value)), "invalid invocation UID")
    return value


def require_owned(obj, run_id, uid=None):
    metadata = obj.get("metadata", {})
    require(metadata.get("labels", {}).get(OWNER) == validate_run_id(run_id),
            "refusing a foreign Kubernetes object")
    require(bool(metadata.get("uid")), "object has no server UID")
    if uid is not None:
        require(metadata["uid"] == uid, "object UID changed; refusing mutation or cleanup")
    return metadata["uid"]


def docker_owned(info, run_id, expected_id, *, kind_node=False):
    require(info.get("Id") == expected_id, "Docker container identity changed")
    labels = info.get("Config", {}).get("Labels", {}) or {}
    key = "io.x-k8s.kind.cluster" if kind_node else OWNER
    expected = "brewlet-v2-" + validate_run_id(run_id) if kind_node else run_id
    require(labels.get(key) == expected, "refusing a foreign Docker container")


def kube_command(kubeconfig, context, args, *, stream=False):
    require(kubeconfig.is_absolute() and kubeconfig.name == "kubeconfig",
            "private absolute kubeconfig required")
    require(bool(re.fullmatch(r"kind-brewlet-v2-[0-9a-f]{12}", context)),
            "private kind context required")
    steering = ("--context", "--kubeconfig", "--server", "--cluster", "--user", "--token",
                "--insecure-skip-tls-verify", "--certificate-authority", "--client-certificate",
                "--client-key", "--username", "--password")
    require(not any(str(a).startswith(steering) or str(a) == "-s" for a in args),
            "caller cannot override cluster identity")
    return ["kubectl", "--kubeconfig", str(kubeconfig), "--context", context,
            "--request-timeout=" + ("0" if stream else "30s"), *map(str, args)]


def classify_evidence(records, gaps, delivery=None):
    """Never turn import execution or a partial suite into full acceptance."""
    passed = bool(records) and all(row["outcome"] == "passed" for row in records)
    delivery = delivery or {}
    remote = (delivery.get("bootstrapOnly") is False
              and all(delivery.get(key) is True for key in
                      ("kubeletPullVerified", "runtimeSourcePullVerified", "tlsTransportVerified")))
    return {"scope": "real-node-execution-and-remote-delivery" if remote else "real-node-execution-only",
            "transport": "verified-authenticated-tls-pull" if remote else "verified-local-containerd-import",
            "fullAcceptance": False, "selectedAssertionsPassed": passed,
            "kubeletPullVerified": remote, "tlsTransportVerified": remote,
            "providerScaleFromZeroVerified": False}


def empty_selector_outcome(result):
    if result.returncode:
        require("nodeSelectorTerms" in result.stderr
                and "must have at least one node selector term" in result.stderr,
                "empty selector failed for an unexpected reason")
        return "preserved-and-rejected-by-native-api-validation"
    admitted = json.loads(result.stdout)
    selector = admitted["spec"]["affinity"]["nodeAffinity"][
        "requiredDuringSchedulingIgnoredDuringExecution"]
    require(selector.get("nodeSelectorTerms", []) == [],
            "empty unsatisfiable selector was made satisfiable")
    return "preserved-unsatisfiable"


def startup_taint_evidence(node, pod, events):
    blockers = [(taint.get("key"), taint.get("value"), taint.get("effect"))
                for taint in node.get("spec", {}).get("taints", [])
                if taint.get("effect") == "NoSchedule"]
    require(blockers == [(FENCE, "provisioning", "NoSchedule")],
            "startup scheduling proof requires the real startup taint as the only NoSchedule blocker")
    require(not pod.get("spec", {}).get("nodeName"),
            "application scheduled through the actual startup fence")
    return [event.get("message", "") for event in events
            if event.get("involvedObject", {}).get("uid") == pod["metadata"]["uid"]
            and event.get("reason") == "FailedScheduling"
            and (FENCE in event.get("message", "")
                 or "untolerated taint" in event.get("message", "").lower())]


def application_started(status):
    if status.get("started") is True:
        return True
    for state in (status.get("state", {}), status.get("lastState", {})):
        if "running" in state:
            return True
        if "terminated" in state:
            timestamp = state["terminated"].get("startedAt")
            if timestamp and timestamp not in ("0001-01-01T00:00:00Z", "1970-01-01T00:00:00Z"):
                return True
    return False


def failure_class(phase, message):
    setup_network = ("connection reset", "no such host", "tls handshake timeout",
                     "network is unreachable", "failed to resolve", "i/o timeout",
                     "connection refused", "unexpected eof")
    return ("infrastructure" if phase in ("prerequisites", "cluster")
            or any(value in message.lower() for value in setup_network) else "acceptance")


def checked_blob(layout, descriptor):
    digest = descriptor["digest"]
    require(bool(re.fullmatch(r"sha256:[0-9a-f]{64}", digest)), "noncanonical OCI digest")
    data = (layout / "blobs/sha256" / digest[7:]).read_bytes()
    require(len(data) == descriptor["size"] and hashlib.sha256(data).hexdigest() == digest[7:],
            "OCI blob verification failed")
    return json.loads(data)


def artifact_target(layout):
    outer = json.loads((layout / "index.json").read_text())
    require(len(outer["manifests"]) == 1, "expected exactly one named OCI target")
    descriptor = outer["manifests"][0]
    target = checked_blob(layout, descriptor)
    manifests = target.get("manifests", [descriptor])
    roles = []
    for item in manifests:
        manifest = checked_blob(layout, item)
        require(manifest.get("annotations", {}).get("brewlet.sh/artifact-version") == "2",
                "not a v2 application")
        current_roles = [layer.get("annotations", {}).get("brewlet.sh/layer")
                         for layer in manifest["layers"]]
        require(current_roles[-1] == "launch" and current_roles.count("launch") == 1
                and "app" in current_roles and "dependency" in current_roles,
                "separate dependency/app/final-launch layers required")
        checked_blob(layout, manifest["config"])
        roles.append(current_roles)
    return descriptor["digest"], roles


class Harness:
    def __init__(self, work, *, timeout=600, pinned=True):
        self.run_id = validate_run_id(uuid.uuid4().hex[:12])
        self.work = work.resolve()
        require(self.work.is_relative_to(HERE) and self.work != HERE,
                "work directory must be a new directory below integration-tests/live-v2")
        require(not self.work.exists(), "work directory already exists")
        self.work.mkdir(parents=True, mode=0o700)
        (self.work / ".gitignore").write_text("*\n")
        self.kubeconfig = self.work / "kubeconfig"
        self.cluster = "brewlet-v2-" + self.run_id
        self.context = "kind-" + self.cluster
        self.namespace = self.cluster
        self.control_namespace = self.cluster + "-system"
        self.registry = self.cluster + "-registry"
        self.profile = self.cluster
        self.timeout = timeout
        self.pinned = pinned
        self.records = []
        self.owned = {}
        self.node = None
        self.node_uid = None
        self.node_container = None
        self.registry_id = None
        self.images = {}
        self.app_images = {}
        self.runtime_images = {}
        self.runtime_paths = {}
        self.initial = {}
        self.helpers = []
        self.command_index = 0
        self.phase = "initialization"
        self.result = {"runID": self.run_id, "referenceMain":
                       "fa7085335b220e29df6affbece4558c5f1259ecb",
                       "pins": {"kind": KIND_VERSION, "node": NODE_IMAGE,
                                "registry": REGISTRY_IMAGE}, "gaps": list(UNIMPLEMENTED)}
        if not pinned:
            self.result["gaps"].append("E2E-06/07/08 real admission interceptor disabled explicitly")
        self.save()

    def save(self):
        self.result.update(classify_evidence(self.records, self.result["gaps"], self.result.get("delivery")))
        intervention = self.work / "manual-intervention.json"
        if intervention.exists():
            self.result["interventions"] = [json.loads(intervention.read_text())]
            self.result["cleanSelectedSuitePassed"] = False
        else:
            self.result["cleanSelectedSuitePassed"] = (
                self.result.get("outcome") == "passed"
                and self.result.get("fixtureDestruction", {}).get("outcome") == "passed"
                and self.result["selectedAssertionsPassed"])
        self.result["assertions"] = self.records
        self.result["ownership"] = {"cluster": self.cluster, "context": self.context,
                                    "node": self.node, "nodeUID": self.node_uid,
                                    "nodeContainerID": self.node_container,
                                    "registryID": self.registry_id,
                                    "objects": [{"kind": key[0], "namespace": key[1],
                                                 "name": key[2], "uid": uid}
                                                for key, uid in self.owned.items()],
                                    "images": self.images}
        (self.work / "result.json").write_text(json.dumps(self.result, indent=2) + "\n")

    def record(self, name, evidence, requirements=()):
        self.records.append({"name": name, "outcome": "passed", "evidence": evidence,
                             "requirements": list(requirements)})
        self.save()
        print("PASS:", name, flush=True)

    def run(self, command, *, input=None, timeout=120, check=True, env=None):
        self.command_index += 1
        command = list(map(str, command))
        print("+", " ".join(command), flush=True)
        environment = dict(os.environ)
        # Do not inherit a shared cluster configuration into any child.
        environment["KUBECONFIG"] = str(self.kubeconfig)
        environment["TMPDIR"] = str(self.work)
        environment["PYTHONDONTWRITEBYTECODE"] = "1"
        if hasattr(self, "arch"):
            environment["DOCKER_DEFAULT_PLATFORM"] = "linux/" + self.arch
        if env:
            environment.update(env)
        log = self.work / f"command-{self.command_index:04d}.log"
        process = subprocess.Popen(command, text=True, stdin=subprocess.PIPE if input is not None else None,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=ROOT,
                                   env=environment, start_new_session=True)
        try:
            stdout, stderr = process.communicate(input=input, timeout=timeout)
        except BaseException as exc:
            # Kill only this command's process group, including fixture servers
            # and build tool children, never processes selected by executable name.
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                stdout, stderr = process.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                stdout, stderr = process.communicate(timeout=5)
            log.write_text(stdout + stderr)
            if isinstance(exc, subprocess.TimeoutExpired):
                raise Failure(f"bounded command timed out ({timeout}s): {command}") from exc
            raise
        result = subprocess.CompletedProcess(command, process.returncode, stdout, stderr)
        log.write_text(result.stdout + result.stderr)
        if check and result.returncode:
            raise Failure(f"command failed ({result.returncode}), {log.name}:\n"
                          + (result.stdout + result.stderr)[-6000:])
        return result

    def kubectl(self, *args, **kwargs):
        return self.run(kube_command(self.kubeconfig, self.context, args), **kwargs)

    def get(self, kind, name=None, namespace=None, absent=False):
        args = ["get", kind]
        if name:
            args.append(name)
        if namespace:
            args += ["-n", namespace]
        args += ["-o", "json"]
        if absent:
            args.append("--ignore-not-found")
        output = self.kubectl(*args).stdout
        return json.loads(output) if output.strip() else None

    def apply(self, obj):
        obj = copy.deepcopy(obj)
        obj.setdefault("metadata", {}).setdefault("labels", {})[OWNER] = self.run_id
        key = (obj["kind"].lower(), obj["metadata"].get("namespace", ""), obj["metadata"]["name"])
        if key in self.owned:
            require_owned(self.get(key[0], key[2], key[1] or None), self.run_id, self.owned[key])
        result = json.loads(self.kubectl("apply", "-f", "-", "-o", "json",
                                        input=json.dumps(obj)).stdout)
        self.owned[key] = require_owned(result, self.run_id, self.owned.get(key))
        self.save()
        return result

    def delete(self, kind, name, namespace=None):
        key = (kind.lower(), namespace or "", name)
        require(key in self.owned, "cannot delete an unrecorded Kubernetes object")
        obj = self.get(kind, name, namespace, absent=True)
        if obj is None:
            return
        require_owned(obj, self.run_id, self.owned[key])
        # An atomic UID precondition prevents an object replacement between GET and DELETE.
        resource = {"pod": "pods", "nodeprofile": "nodeprofiles", "runtimeimage": "runtimeimages",
                    "namespace": "namespaces"}[kind.lower()]
        prefix = "/api/v1" if kind.lower() in ("pod", "namespace") else "/apis/" + API
        if namespace:
            prefix += "/namespaces/" + namespace
        self.kubectl("delete", "--raw", prefix + "/" + resource + "/" + name, "-f", "-",
                     input=json.dumps({"apiVersion": "v1", "kind": "DeleteOptions",
                                       "preconditions": {"uid": self.owned[key]},
                                       "propagationPolicy": "Foreground"}))

    def wait(self, description, predicate, timeout=None):
        deadline = time.monotonic() + (timeout or self.timeout)
        last = None
        while time.monotonic() < deadline:
            last = predicate()
            if last:
                return last
            time.sleep(2)
        raise Failure(f"deadline waiting for {description}; last observation: {last!r}")

    def node_exec(self, *args, **kwargs):
        require(self.node_container is not None, "no owned kind node")
        info = json.loads(self.run(["docker", "inspect", self.node_container]).stdout)[0]
        docker_owned(info, self.run_id, self.node_container, kind_node=True)
        stdin = ["-i"] if kwargs.get("input") is not None else []
        return self.run(["docker", "exec", *stdin, self.node_container, *args], **kwargs)

    def ctr(self, *args):
        return self.node_exec("ctr", "-n", "k8s.io", *args).stdout

    def image_digest(self, reference):
        for line in self.ctr("images", "ls").splitlines()[1:]:
            columns = line.split()
            if columns[0] == reference:
                require(bool(re.fullmatch(r"sha256:[0-9a-f]{64}", columns[2])),
                        "ctr did not report a target digest")
                return columns[2]
        raise Failure("named image absent from containerd: " + reference)

    def load_image(self, tag):
        self.run(["kind", "load", "docker-image", "--name", self.cluster, tag], timeout=300)
        return self.image_digest(tag)

    def build(self, tag, dockerfile, context, *, build_args=()):
        self.run(["docker", "build", "--load", "--platform", "linux/" + self.arch,
                  "--label", OWNER + "=" + self.run_id, "-t", tag,
                  "-f", dockerfile, *build_args, context], timeout=1200)
        info = json.loads(self.run(["docker", "image", "inspect", tag]).stdout)[0]
        require(info["Config"]["Labels"][OWNER] == self.run_id, "built image lost ownership")
        self.images[tag] = info["Id"]
        self.save()

    def preflight(self):
        for tool in ("docker", "kind", "kubectl", "helm", "go", "mvn", "java", "python3",
                     "node", "dotnet", "openssl"):
            require(shutil.which(tool), "mandatory prerequisite missing: " + tool)
        require(sys.version_info >= (3, 12), "Python 3.12+ required")
        version = self.run(["kind", "version"]).stdout.split()
        require(len(version) > 1 and version[1] == KIND_VERSION,
                "kind must be exactly " + KIND_VERSION)
        arch = self.run(["docker", "info", "--format", "{{.Architecture}}"]).stdout.strip()
        self.arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(arch)
        require(self.arch, "Docker architecture must be Linux amd64 or arm64")
        require(self.run(["docker", "info", "--format", "{{.OSType}}"]).stdout.strip() == "linux",
                "Linux Docker engine required")
        self.result["architecture"] = self.arch
        for tool, args in (("go", ["version"]), ("kubectl", ["version", "--client", "-o", "json"]),
                           ("helm", ["version", "--short"]), ("mvn", ["--version"]),
                           ("java", ["-version"]), ("node", ["--version"]),
                           ("dotnet", ["--info"])):
            result = self.run([tool, *args])
            self.result.setdefault("toolVersions", {})[tool] = result.stdout + result.stderr
        require("10.0.401" in self.run(["dotnet", "--list-sdks"]).stdout,
                "fixture requires .NET SDK exactly 10.0.401 (global.json disallows roll-forward)")
        self.result["checkout"] = self.run(["git", "rev-parse", "HEAD"]).stdout.strip()
        pinned_files = [ROOT / "provisioner/Dockerfile", ROOT / "kubernetes/Dockerfile",
                        FIXTURES / "runtimes/rotation.json",
                        *sorted((FIXTURES / "runtimes").glob("*.Dockerfile"))]
        self.result["inputFileSHA256"] = {
            str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in pinned_files}
        self.record("mandatory prerequisites and exact kind pin", {"architecture": self.arch})

    def provision_cluster(self):
        # Never use kind's default kubeconfig path, even while creating the cluster.
        config = {"kind": "Cluster", "apiVersion": "kind.x-k8s.io/v1alpha4",
                  "nodes": [{"role": "control-plane", "image": NODE_IMAGE}],
                  "containerdConfigPatches": [
                      '[plugins."io.containerd.grpc.v1.cri".registry]\n'
                      '  config_path = "/etc/containerd/certs.d"\n']}
        config_path = self.work / "kind.json"
        config_path.write_text(json.dumps(config))
        existing = self.run(["kind", "get", "clusters"]).stdout.splitlines()
        require(self.cluster not in existing, "cluster name collision")
        try:
            self.run(["kind", "create", "cluster", "--name", self.cluster,
                      "--kubeconfig", self.kubeconfig, "--config", config_path,
                      "--wait", "180s"], timeout=360)
        finally:
            # Capture identity even after a partially successful create.
            nodes = self.run(["docker", "ps", "-aq", "--filter",
                              "label=io.x-k8s.kind.cluster=" + self.cluster]).stdout.split()
            if len(nodes) == 1:
                info = json.loads(self.run(["docker", "inspect", nodes[0]]).stdout)[0]
                self.node_container = info["Id"]
                docker_owned(info, self.run_id, self.node_container, kind_node=True)
                self.save()
        os.chmod(self.kubeconfig, 0o600)
        require(self.kubectl("config", "current-context").stdout.strip() == self.context,
                "kind produced an unexpected context")
        nodes = self.get("nodes")["items"]
        require(len(nodes) == 1, "private cluster must have exactly one node")
        self.node = nodes[0]["metadata"]["name"]
        self.node_uid = nodes[0]["metadata"]["uid"]
        require(self.node == self.cluster + "-control-plane", "unexpected kind node name")
        require(nodes[0]["metadata"]["labels"]["kubernetes.io/arch"] == self.arch,
                "Docker/node architecture mismatch")
        require(self.node_exec("stat", "-fc", "%T", "/sys/fs/cgroup").stdout.strip() == "cgroup2fs",
                "cgroup v2 is mandatory")
        for ns in (self.namespace, self.control_namespace):
            self.apply({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": ns}})
        try:
            self.run(["docker", "run", "--platform", "linux/" + self.arch,
                      "-d", "--name", self.registry, "--network", "kind",
                      "--label", OWNER + "=" + self.run_id, REGISTRY_IMAGE])
        finally:
            result = self.run(["docker", "inspect", self.registry], check=False)
            if result.returncode == 0:
                info = json.loads(result.stdout)[0]
                docker_owned(info, self.run_id, info["Id"])
                self.registry_id = info["Id"]
                self.save()
        self.wait("private registry health", lambda:
                  self.run(["docker", "exec", self.registry_id, "wget", "-qO-",
                            "http://127.0.0.1:5000/v2/"], check=False).returncode == 0, timeout=60)
        self.record("isolated kind node and owned registry",
                    {"node": self.node, "nodeUID": self.node_uid,
                     "note": "registry readiness is not application pull/TLS coverage"})

    def prepare_images(self):
        cli = self.work / "brewlet"
        build_environment = ({"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": self.arch}
                             if sys.platform == "darwin" else None)
        self.run(["go", "build", "-mod=readonly", "-trimpath", "-o", cli, "./cmd/brewlet"],
                 timeout=300, env=build_environment)
        self.cli = cli
        self.cli_command = [cli]
        if sys.platform == "darwin":
            self.cli_command = [
                "docker", "run", "--rm", "--platform", "linux/" + self.arch, "--network", "none",
                "--user", f"{os.getuid()}:{os.getgid()}", "--cap-drop", "ALL",
                "--security-opt", "no-new-privileges", "--read-only", "--tmpfs", "/tmp",
                "--entrypoint", "/brewlet",
                "--mount", f"type=bind,source={cli},target=/brewlet,readonly",
                "--mount", f"type=bind,source={ROOT},target={ROOT},readonly",
                "--mount", f"type=bind,source={self.work},target={self.work}",
                "--workdir", self.work, CLI_USERLAND,
            ]
            self.result["cliExecution"] = {"scope": "linux-container", "userland": CLI_USERLAND,
                                           "note": "macOS provenance xattrs remain rejected"}
        else:
            self.result["cliExecution"] = {"scope": "native-linux"}
        self.result["cliSHA256"] = hashlib.sha256(cli.read_bytes()).hexdigest()
        # Reuse the parent's actual source builders, not replacement applications.
        build_work = self.work / "fixtures"
        build_work.mkdir()
        java_temp = self.work / "java-scratch"
        java_temp.mkdir()
        # The existing fixture helper starts host toolchains. Isolate it in a
        # bounded process, with private Maven/temp state and captured diagnostics.
        self.run([sys.executable, "-c",
                  "import importlib.util,json,sys; from pathlib import Path; "
                  "s=importlib.util.spec_from_file_location('fixtures',sys.argv[1]); "
                  "m=importlib.util.module_from_spec(s); s.loader.exec_module(m); "
                  "r,l,j=m.build_and_probe(Path(sys.argv[2])); "
                  "Path(sys.argv[3]).write_text(json.dumps({'reports':r,'layers':"
                  "{k:[[a,str(b),c] for a,b,c in v] for k,v in l.items()}}))",
                  FIXTURES / "check.py", build_work, self.work / "host-reports.json"],
                 timeout=900, env={"MAVEN_OPTS": "-Dmaven.repo.local=" + str(self.work / "m2"),
                                   "JAVA_TOOL_OPTIONS": "-Djava.io.tmpdir=" + str(java_temp)})
        layers = json.loads((self.work / "host-reports.json").read_text())["layers"]
        for language, inputs in layers.items():
            tag = f"registry.brewlet.test/{self.run_id}/apps/{language}:v2"
            layout = self.work / ("oci-" + language)
            command = [*self.cli_command, "push", tag, "--store", layout,
                       "--platform", "linux/amd64", "--platform", "linux/arm64",
                       "--launch", FIXTURES / language / "launch.json"]
            for role, source, target in inputs:
                command += ["--layer", f"{role}={source}:{target}"]
            self.run(command, timeout=180)
            digest, roles = artifact_target(layout)
            self.import_layout(layout)
            require(self.image_digest(tag) == digest,
                    "import target differs from inner application index (outer index is not runnable)")
            canonical = tag.rsplit(":", 1)[0] + "@" + digest
            self.ctr("images", "tag", "--force", tag, canonical)
            require(self.image_digest(canonical) == digest, "canonical application import mismatch")
            self.app_images[language] = canonical
            self.record(language + " explicit application/dependency artifact imported",
                        {"image": canonical, "roles": roles}, ["ART-01", "LANG-01"])
        # Never send this run's kubeconfig, keys or unrelated checkout files in
        # Docker's build context. These are exactly the production COPY inputs.
        snapshot = self.work / "production-context"
        snapshot.mkdir()
        for name in ("go.mod", "go.sum"):
            shutil.copy2(ROOT / name, snapshot / name)
        for name in ("pkg", "kubernetes", "cmd", "provisioner", "shim"):
            shutil.copytree(ROOT / name, snapshot / name,
                            ignore=shutil.ignore_patterns(".git", "__pycache__", ".DS_Store",
                                                          ".test-*", "*.test", ".validation.*",
                                                          ".admission-*", ".operator-*", ".node-*"))
        hashes = {str(path.relative_to(snapshot)): hashlib.sha256(path.read_bytes()).hexdigest()
                  for path in sorted(snapshot.rglob("*")) if path.is_file()}
        (self.work / "production-source-sha256.json").write_text(json.dumps(hashes, indent=2))
        self.result["productionSourceManifestSHA256"] = hashlib.sha256(
            (self.work / "production-source-sha256.json").read_bytes()).hexdigest()
        for component, dockerfile in (("control", snapshot / "kubernetes/Dockerfile"),
                                      ("worker", snapshot / "provisioner/Dockerfile")):
            tag = f"registry.brewlet.test/{self.run_id}/{component}:checkout"
            self.build(tag, dockerfile, snapshot)
            self.load_image(tag)
            self.result[component + "Image"] = tag
        self.runtime_vendor_inputs = rotation_inputs(json.loads(
            (FIXTURES / "runtimes/rotation.json").read_text()))
        self.result["runtimeVendorInputs"] = self.runtime_vendor_inputs
        for language in LANGUAGES:
            self.runtime_images[language] = {}
            for revision in ("before", "after"):
                base_suffix = "base" if revision == "before" else "base-after"
                base = f"registry.brewlet.test/{self.run_id}/runtime/{language}:{base_suffix}"
                vendor = self.runtime_vendor_inputs[language][revision]["image"]
                self.build(base, FIXTURES / "runtimes" / (language + ".Dockerfile"),
                           FIXTURES / "runtimes", build_args=["--build-arg", "RUNTIME_BASE=" + vendor])
                config = json.loads(self.run(["docker", "image", "inspect", base]).stdout)[0]["Config"]
                paths = [entry[5:] for entry in config["Env"] if entry.startswith("PATH=")]
                require(len(paths) == 1 and paths[0] and all(
                    part.startswith("/") and part != "/app" and not part.startswith("/app/")
                    for part in paths[0].split(":")), "fixture runtime PATH is not explicit")
                if revision == "before":
                    self.runtime_paths[language] = paths[0]
                else:
                    require(paths[0] == self.runtime_paths[language],
                            "fixture runtime update changes its stable PATH contract")
                directory = self.work / (language + "-" + revision)
                directory.mkdir()
                dockerfile = directory / "Dockerfile"
                dockerfile.write_text(f"FROM {base}\nENV BREWLET_RUNTIME_REVISION={revision} "
                                      "BREWLET_TEST_VALUE=runtime-default\n")
                tag = f"registry.brewlet.test/{self.run_id}/runtime/{language}:{revision}"
                self.build(tag, dockerfile, directory)
                digest = self.load_image(tag)
                canonical = tag.rsplit(":", 1)[0] + "@" + digest
                self.ctr("images", "tag", "--force", tag, canonical)
                require(self.image_digest(canonical) == digest, "runtime target mismatch")
                self.runtime_images[language][revision] = canonical
        self.result["applicationImages"] = self.app_images
        self.result["runtimeImages"] = self.runtime_images
        self.save()

    def import_layout(self, layout):
        # Do not pass --index-name: it names the outer OCI layout index.
        command = ["docker", "exec", "-i", self.node_container, "ctr", "-n", "k8s.io",
                   "images", "import", "--all-platforms", "-"]
        info = json.loads(self.run(["docker", "inspect", self.node_container]).stdout)[0]
        docker_owned(info, self.run_id, self.node_container, kind_node=True)
        with (self.work / ("import-" + layout.name + ".log")).open("wb") as log:
            process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=log, stderr=log)
            watchdog = threading.Timer(180, process.kill)
            watchdog.start()
            try:
                with tarfile.open(fileobj=process.stdin, mode="w|") as archive:
                    for path in sorted(layout.iterdir()):
                        archive.add(path, arcname=path.name)
                process.stdin.close()
                require(process.wait(timeout=180) == 0, "containerd OCI streaming import failed")
            finally:
                watchdog.cancel()
                if process.poll() is None:
                    process.kill()
                    process.wait(timeout=10)

    def install(self):
        # Current schemas must be applied before every Helm install/upgrade.
        crds = ROOT / "kubernetes/deploy/crds"
        require(crds.is_dir(), "current deploy CRDs directory is missing")
        self.kubectl("apply", "-f", crds, timeout=90)
        self.run(["helm", "--kubeconfig", self.kubeconfig, "--kube-context", self.context,
                  "upgrade", "--install", self.cluster, ROOT / "kubernetes/charts/brewlet",
                  "--namespace", self.control_namespace, "--wait", "--timeout", "180s",
                  "--set-string", "operator.image=" + self.result["controlImage"],
                  "--set-string", "admission.image=" + self.result["controlImage"],
                  "--set-string", "provisioner.image=" + self.result["workerImage"],
                  "--set-string", "provisioner.gc.grace=20s",
                  "--set-string", "provisioner.gc.sweepInterval=2s"],
                 timeout=240)
        # Fail-open webhook availability is not proof of admission execution.
        self.kubectl("-n", self.control_namespace, "rollout", "status",
                     "deployment/brewlet-admission", "--timeout=120s", timeout=150)
        self.wait_inventory_admission(self.runtime_images["python"]["before"])
        for language in LANGUAGES:
            self.apply({"apiVersion": API, "kind": "RuntimeImage",
                        "metadata": {"name": IDS[language]},
                        "spec": {"source": {"image": self.runtime_images[language]["before"]},
                                 "user": {"uid": 65532, "gid": 65532}}})
        for runtime_id in IDS.values():
            self.wait("approved inventory " + runtime_id, lambda rid=runtime_id:
                      any(c["type"] == "Approved" and c["status"] == "True" for c in
                          self.get("runtimeimage", rid).get("status", {}).get("conditions", [])))

    def wait_inventory_admission(self, source):
        probe = {"apiVersion": API, "kind": "RuntimeImage",
                 "metadata": {"name": "webhook-ready-" + self.run_id},
                 "spec": {"source": {"image": source}, "user": {"uid": 65532, "gid": 65532}}}

        def reachable():
            response = self.kubectl("create", "--dry-run=server", "-f", "-",
                                    input=json.dumps(probe), check=False)
            if response.returncode == 0:
                return True
            transport = ("connect: connection refused", "no endpoints available for service",
                         "i/o timeout", "tls handshake timeout")
            require(any(value in response.stderr.lower() for value in transport),
                    "inventory admission failed instead of becoming reachable: " + response.stderr)
            return False

        self.wait("real inventory webhook service reachability", reachable, timeout=60)

    def node_snapshot(self):
        obj = self.get("node", self.node)
        require(obj["metadata"]["uid"] == self.node_uid, "private Node UID changed")
        return obj

    def advertised(self, runtime_id, state, generation=None):
        node = self.node_snapshot()
        metadata = node["metadata"]
        inventory = json.loads(metadata.get("annotations", {}).get("brewlet.sh/runtime-images", "{}"))
        detail = inventory.get(runtime_id, {})
        if metadata.get("labels", {}).get("brewlet.sh/runtime." + runtime_id) != state:
            return False
        if detail.get("state") != state:
            return False
        if generation is not None and detail.get("generation") != generation:
            return False
        return detail

    def profile_object(self):
        return {"apiVersion": API, "kind": "NodeProfile", "metadata": {"name": self.profile},
                "spec": {"nodePool": {"key": "brewlet.test/pool", "names": [self.run_id],
                                      "includeControlPlane": True},
                         "runtimeImages": [{"id": IDS[language], "install": "OnDemand"}
                                           for language in LANGUAGES],
                         "tolerations": [{"key": FENCE, "operator": "Equal",
                                          "value": "provisioning", "effect": "NoSchedule"}],
                         "rollout": {"maxUnavailable": 1, "containerdRestart": "validated"}}}

    def startup(self):
        self.kubectl("label", "node", self.node, "brewlet.test/pool=" + self.run_id)
        self.kubectl("taint", "node", self.node, FENCE + "=provisioning:NoSchedule")
        self.kubectl("label", "node", self.node, "brewlet.sh/runtime=ready",
                     *["brewlet.sh/runtime." + rid + "=available" for rid in IDS.values()])
        # Admission has valid inventory but no provisioner yet. This is a real
        # scheduler observation, not merely an API affinity inspection.
        zero = self.profile_object()
        zero["metadata"]["name"] = self.profile + "-zero"
        zero["spec"]["nodePool"]["names"] = ["absent-" + self.run_id]
        self.apply(zero)
        self.admission_constraints(zero["metadata"]["name"])
        pending = self.pod("startup-pending", "python")
        self.apply(pending)
        def pending_scheduled():
            obj = self.get("pod", "startup-pending", self.namespace)
            require_owned(obj, self.run_id,
                          self.owned[("pod", self.namespace, "startup-pending")])
            require(not obj.get("spec", {}).get("nodeName"),
                    "application scheduled through the actual startup fence")
            return obj if any(c["type"] == "PodScheduled" and c["status"] == "False"
                              for c in obj.get("status", {}).get("conditions", [])) else False
        obj = self.wait("real startup-fenced scheduler Pending condition", pending_scheduled, timeout=60)
        messages = self.wait("UID-scoped startup taint scheduler evidence", lambda:
            startup_taint_evidence(self.node_snapshot(), obj,
                                   self.get("events", namespace=self.namespace)["items"]), timeout=60)
        self.record("actual startup taint blocks application scheduling before provisioning",
                    {"podUID": obj["metadata"]["uid"], "nodeUID": self.node_uid,
                     "schedulerMessages": messages}, ["E2E-03"])
        for toleration in ({"operator": "Exists"}, {"key": FENCE, "operator": "Exists"}):
            rejected = self.pod("forbidden-toleration", "python")
            rejected["spec"]["tolerations"] = [toleration]
            response = self.kubectl("create", "--dry-run=server", "-f", "-",
                                    input=json.dumps(rejected), check=False)
            require(response.returncode != 0 and "StartupTaintTolerationNotSupported"
                    in response.stderr, "startup toleration did not fail with the specified reason")
        self.delete("pod", "startup-pending", self.namespace)
        self.wait("startup fixture pod deletion", lambda:
                  self.get("pod", "startup-pending", self.namespace, absent=True) is None)
        self.delete("nodeprofile", zero["metadata"]["name"])
        self.wait("zero-node profile cleanup", lambda:
                  self.get("nodeprofile", zero["metadata"]["name"], absent=True) is None)
        self.apply(self.profile_object())
        self.wait("OnDemand-only baseline without startup fence", lambda:
                  self.node_snapshot()["metadata"].get("labels", {}).get("brewlet.sh/runtime") == "ready"
                  and not any(t["key"] == FENCE for t in
                              self.node_snapshot().get("spec", {}).get("taints", [])))
        for language in LANGUAGES:
            rid = IDS[language]
            self.wait("available cold ID " + rid, lambda rid=rid: self.advertised(rid, "available"))
            require(self.node_exec("test", "!", "-e", STORE + "/" + rid + "/current",
                                   check=False).returncode == 0,
                    "OnDemand baseline fetched a runtime before first use")
        node = self.node_snapshot()
        labels = node["metadata"]["labels"]
        profile_uid = self.owned[("nodeprofile", "", self.profile)]
        require(labels.get("brewlet.sh/owner-uid") == profile_uid
                and labels.get("brewlet.sh/owner-node-uid") == self.node_uid,
                "baseline ownership claim is not bound to exact profile and Node UIDs")
        self.record("OnDemand-only baseline advertises available with no current roots",
                    {"nodeUID": self.node_uid, "profileUID": profile_uid}, ["E2E-02", "PROF-03", "FENCE-01"])

    def admission_constraints(self, zero_profile):
        pod = self.pod("affinity-dryrun", "python")
        affinity = {"nodeAffinity": {
            "requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [
                {"matchExpressions": [{"key": "acceptance.brewlet.test/zone", "operator": "In",
                                        "values": ["one"]}],
                 "matchFields": [{"key": "metadata.name", "operator": "In", "values": [self.node]}]},
                {"matchExpressions": [{"key": "acceptance.brewlet.test/zone", "operator": "In",
                                        "values": ["two"]}]}]},
            "preferredDuringSchedulingIgnoredDuringExecution": [
                {"weight": 7, "preference": {"matchExpressions": [
                    {"key": "acceptance.brewlet.test/preference", "operator": "Exists"}]}}]}}
        pod["spec"]["affinity"] = copy.deepcopy(affinity)
        response = self.kubectl("create", "--dry-run=server", "-f", "-", "-o", "json",
                                input=json.dumps(pod))
        admitted = json.loads(response.stdout)
        require(zero_profile in response.stderr,
                "zero-node inventory admission did not warn with its eligible profile name")
        actual = admitted["spec"]["affinity"]["nodeAffinity"]
        expected = affinity["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
        terms = actual["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
        require(len(terms) == len(expected), "admission introduced a bypassing OR alternative")
        key = "brewlet.sh/runtime." + IDS["python"]
        for before, after in zip(expected, terms):
            require(after.get("matchFields", []) == before.get("matchFields", []),
                    "admission changed matchFields")
            expressions = after.get("matchExpressions", [])
            require(all(item in expressions for item in before["matchExpressions"]),
                    "admission dropped author affinity")
            require({"key": key, "operator": "In", "values": ["ready", "available"]} in expressions,
                    "runtime requirement was not ANDed into every OR term")
        preferred = actual["preferredDuringSchedulingIgnoredDuringExecution"]
        require(preferred[0] == affinity["nodeAffinity"]["preferredDuringSchedulingIgnoredDuringExecution"][0]
                and any(term["weight"] == 100 and {"key": key, "operator": "In", "values": ["ready"]}
                        in term["preference"].get("matchExpressions", []) for term in preferred),
                "warm preference replaced author preference")
        require(admitted["spec"]["nodeSelector"]["kubernetes.io/arch"] == self.arch,
                "native architecture selector was lost")
        pod["spec"]["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"] = {
            "nodeSelectorTerms": []}
        empty_result = self.kubectl("create", "--dry-run=server", "-f", "-", "-o", "json",
                                    input=json.dumps(pod), check=False)
        empty_outcome = empty_selector_outcome(empty_result)
        self.record("zero-node warning, architecture selector and multi-term/empty affinity preservation",
                    {"profile": zero_profile, "nodeUID": self.node_uid,
                     "emptySelector": empty_outcome}, ["E2E-01", "E2E-04", "ADM-02"])

    def launch(self, language):
        return json.loads((FIXTURES / language / "launch.json").read_text())["process"]

    def probe(self, language):
        process = self.launch(language)
        return process["entrypoint"] + process.get("args", []) + ["--probe", "two words"]

    def pod(self, name, language, *, barrier=None, overrides=False, bound=False):
        annotations = {"brewlet.sh/runtime-image": IDS[language]}
        if barrier is not None:
            annotations["brewlet.sh/rotated-to-generation"] = (
                barrier if isinstance(barrier, str) else json.dumps(barrier))
        container = {"name": "app", "image": self.app_images[language],
                     "imagePullPolicy": "Never",
                     "env": [{"name": "BREWLET_TEST_VALUE", "value": "pod-value"},
                             {"name": "PATH", "value": "/app/forbidden-path"}],
                     "securityContext": {"readOnlyRootFilesystem": True},
                     "readinessProbe": {"exec": {"command": self.probe(language)},
                                        "periodSeconds": 2, "timeoutSeconds": 5,
                                        "failureThreshold": 15},
                     "volumeMounts": [{"name": "data", "mountPath": "/app/new-volume-target"}],
                     "resources": {"requests": {"cpu": "10m", "memory": "32Mi"},
                                   "limits": {"cpu": "500m", "memory": "256Mi"}}}
        if overrides:
            launch = self.launch(language)
            container.update(command=launch["entrypoint"], args=launch.get("args", []) + ["two words"],
                             workingDir="/home/app")
        spec = {"runtimeClassName": "brewlet", "restartPolicy": "Never",
                "nodeSelector": {"kubernetes.io/arch": getattr(self, "arch", "arm64")},
                "securityContext": {"fsGroup": 65532}, "containers": [container],
                "volumes": [{"name": "data", "emptyDir": {}}]}
        if bound:
            spec["nodeName"] = self.node
            container["securityContext"].update(runAsUser=65532, runAsGroup=65532,
                                                runAsNonRoot=True, allowPrivilegeEscalation=False,
                                                capabilities={"drop": ["ALL"]})
        return {"apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": name, "namespace": self.namespace,
                             "labels": {OWNER: self.run_id}, "annotations": annotations}, "spec": spec}

    def ready(self, name):
        obj = self.get("pod", name, self.namespace)
        require_owned(obj, self.run_id, self.owned[("pod", self.namespace, name)])
        status = obj.get("status", {})
        if status.get("phase") in ("Failed", "Succeeded"):
            details = [status.get("message", "")]
            for container in status.get("containerStatuses", []):
                stopped = container.get("state", {}).get("terminated")
                if stopped:
                    details.append(f'{container["name"]}: {stopped.get("reason", "")} '
                                   f'exit={stopped.get("exitCode")} {stopped.get("message", "")}')
            raise Failure(f'positive application {name} became terminal {status["phase"]}: '
                          + "; ".join(part for part in details if part))
        return obj if any(c["type"] == "Ready" and c["status"] == "True"
                          for c in status.get("conditions", [])) else False

    def exec(self, name, argv, *, check=True, timeout=40):
        obj = self.get("pod", name, self.namespace)
        require_owned(obj, self.run_id, self.owned[("pod", self.namespace, name)])
        return self.kubectl("-n", self.namespace, "exec", name, "-c", "app", "--", *argv,
                            check=check, timeout=timeout)

    def start_held_exec(self, language):
        name = language + "-old"
        script = ("printf %s \"$BREWLET_RUNTIME_REVISION\" > /app/new-volume-target/exec-started; "
                  "while [ ! -e /app/new-volume-target/exec-release ]; do sleep 1; done; "
                  "printf %s \"$BREWLET_RUNTIME_REVISION\"")
        command = kube_command(self.kubeconfig, self.context,
                               ["-n", self.namespace, "exec", name, "-c", "app", "--",
                                "/bin/sh", "-ec", script], stream=True)
        log = (self.work / (language + "-held-exec.log")).open("w")
        process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=log, text=True,
                                   start_new_session=True, cwd=ROOT,
                                   env=dict(os.environ, KUBECONFIG=str(self.kubeconfig),
                                            TMPDIR=str(self.work)))
        self.helpers.append((process, log))
        self.wait(language + " held exec actually running", lambda:
                  self.exec(name, ["/bin/sh", "-ec",
                      "test \"$(cat /app/new-volume-target/exec-started)\" = before"],
                      check=False).returncode == 0, timeout=60)
        require(process.poll() is None, "held exec exited before runtime rotation")
        return process

    def finish_held_exec(self, language, process):
        require(process.poll() is None, "rotation terminated the active old-generation exec")
        self.exec(language + "-old", ["/bin/sh", "-ec", "touch /app/new-volume-target/exec-release"])
        stdout, _ = process.communicate(timeout=30)
        require(process.returncode == 0 and stdout == "before",
                "exec spanning rotation did not retain its original runtime Env/lease")

    def stop_helpers(self):
        for process, log in self.helpers:
            if process.poll() is None:
                try:
                    os.killpg(process.pid, signal.SIGTERM)
                except ProcessLookupError:
                    process.poll()
                try:
                    process.communicate(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.communicate(timeout=5)
            else:
                process.communicate(timeout=5)
            log.close()

    def http(self, name):
        obj = self.get("pod", name, self.namespace)
        # Probe the actual CNI-connected HTTP service from the owned kind node.
        address = obj["status"]["podIP"]
        require(bool(re.fullmatch(r"[0-9a-fA-F:.]+", address)), "unexpected Pod IP")
        host = "[" + address + "]" if ":" in address else address
        return json.loads(self.node_exec("curl", "--fail", "--silent", "--show-error",
                                        "--noproxy", "*", "--max-time", "10", f"http://{host}:8000/").stdout)

    def assert_report(self, report, language, *, arguments, cwd, uid=65532, home="/home/app"):
        require(report["language"] == language and bool(report["runtime"])
                and report["dependency"] == "shared-contract", "runtime or separate dependency not executing")
        require(report["arguments"] == arguments and report["cwd"] == cwd,
                "Kubernetes argv/cwd semantics were not preserved: " + repr(report))
        require(report["uid"] == uid and report["gid"] == 65532,
                f"initial/exec identity is not {uid}:65532")
        require(report["home"] == home and report["path"] == self.runtime_paths[language],
                "runtime PATH/HOME not preserved in initial/exec process")
        require(report["value"] == "pod-value", "explicit pod environment did not win")

    def current(self, language):
        return json.loads(self.node_exec("cat", STORE + "/" + IDS[language] + "/current/metadata.json").stdout)

    def lease(self, obj):
        container_id = obj["status"]["containerStatuses"][0]["containerID"].removeprefix("containerd://")
        require(bool(re.fullmatch(r"[0-9a-f]{64}", container_id)), "unexpected container identity")
        lease = json.loads(self.node_exec("cat", STORE + "/.tasks/" + container_id + "/lease.json").stdout)
        require(lease["taskID"] == container_id and lease["namespace"] == "k8s.io",
                "durable lease task identity mismatch")
        return lease

    def four_language_execution(self):
        for language in LANGUAGES:
            name = language + "-old"
            self.apply(self.pod(name, language))
            obj = self.wait(language + " initial start and real exec readiness probe", lambda: self.ready(name))
            self.assert_report(self.http(name), language, arguments=[], cwd="/app")
            self.assert_report(json.loads(self.exec(name, self.probe(language)).stdout), language,
                               arguments=["--probe", "two words"], cwd="/app")
            # The mountpoint deliberately does not exist in the packaged layers.
            self.exec(name, ["/bin/sh", "-ec",
                            "test ! -e /launch.json; "
                            "if touch /app/packaged-must-stay-readonly; then exit 81; fi; "
                            "if touch /etc/root-must-stay-readonly; then exit 82; fi; "
                            "printf writable > /app/new-volume-target/proof; "
                            "test \"$(cat /app/new-volume-target/proof)\" = writable; "
                            "awk '$5==\"/app\"&&$6~/(^|,)ro(,|$)/ {found=1} "
                            "END {exit !found}' /proc/self/mountinfo; "
                            "awk '$5==\"/\"&&$6~/(^|,)ro(,|$)/ {found=1} "
                            "END {exit !found}' /proc/self/mountinfo; "
                            "grep -q '^NoNewPrivs:[[:space:]]*1$' /proc/self/status; "
                            "grep -q '^CapEff:[[:space:]]*0000000000000000$' /proc/self/status; "
                            "grep -q '^NoNewPrivs:[[:space:]]*1$' /proc/1/status; "
                            "grep -q '^CapEff:[[:space:]]*0000000000000000$' /proc/1/status"])
            self.wait(language + " ready advertised", lambda: self.advertised(IDS[language], "ready"))
            metadata = self.current(language)
            inventory = self.get("runtimeimage", IDS[language])
            require(metadata["inventoryUID"] == inventory["metadata"]["uid"]
                    and metadata["generation"] == inventory["status"]["generation"]
                    and metadata["sourceDigest"] == self.runtime_images[language]["before"].split("@")[1],
                    "current root is not the approved epoch/digest/generation")
            require(self.node_exec("cat", STORE + "/" + IDS[language] + "/current/rootfs/.brewlet-source"
                                  ).stdout.strip() == metadata["sourceDigest"],
                    "root source marker does not match approved runtime metadata")
            lease = self.lease(obj)
            require(lease["runtime"]["inventoryUID"] == metadata["inventoryUID"]
                    and lease["runtime"]["generation"] == metadata["generation"],
                    "task lease does not identify the runtime that started it")
            require(self.exec(name, ["/bin/sh", "-ec", "printf %s \"$BREWLET_RUNTIME_REVISION\""]
                              ).stdout == "before", "runtime config Env missing in exec")
            self.initial[language] = {"pod": obj, "lease": lease, "metadata": metadata}
            self.record(language + " real startup, HTTP, dependency, exec/probe, identity and readonly mounts",
                        {"podUID": obj["metadata"]["uid"], "lease": lease, "current": metadata},
                        ["LANG-02", "RUN-07", "PROC-01", "PROC-02", "SEC-03", "SEC-04"])
            override = language + "-override"
            self.apply(self.pod(override, language, overrides=True))
            self.wait(language + " explicit command/args/workingDir overrides", lambda: self.ready(override))
            self.assert_report(self.http(override), language, arguments=["two words"], cwd="/home/app")
            self.record(language + " Kubernetes command/args/cwd overrides",
                        {"pod": override}, ["LANG-02", "PROC-01"])
            custom = language + "-custom-user"
            custom_pod = self.pod(custom, language)
            custom_pod["spec"]["containers"][0]["securityContext"]["runAsUser"] = 12345
            self.apply(custom_pod)
            custom_obj = self.wait(language + " arbitrary UID with independent group default",
                                   lambda: self.ready(custom))
            security = custom_obj["spec"]["containers"][0]["securityContext"]
            require(security["runAsUser"] == 12345 and security["runAsGroup"] == 65532,
                    "admission did not independently default the missing GID")
            self.assert_report(self.http(custom), language, arguments=[], cwd="/app",
                               uid=12345, home="/tmp")
            self.assert_report(json.loads(self.exec(custom, self.probe(language)).stdout), language,
                               arguments=["--probe", "two words"], cwd="/app", uid=12345, home="/tmp")
            self.record(language + " custom UID/default GID and HOME fallback match initial/exec",
                        {"podUID": custom_obj["metadata"]["uid"]}, ["SEC-01", "SEC-04", "PROC-03"])

    def no_start(self, name, reason, timeout=120):
        def observe():
            obj = self.get("pod", name, self.namespace)
            require_owned(obj, self.run_id, self.owned[("pod", self.namespace, name)])
            for status in obj.get("status", {}).get("containerStatuses", []):
                require(not application_started(status),
                        "unauthorized/below-barrier application actually started")
            events = self.get("events", namespace=self.namespace)["items"]
            text = "\n".join(e.get("message", "") for e in events
                             if e.get("involvedObject", {}).get("uid") == obj["metadata"]["uid"])
            text += json.dumps(obj.get("status", {}))
            return text if reason in text else False
        evidence = self.wait(name + " exact failure " + reason, observe, timeout=timeout)
        self.record(name + " rejects start: " + reason, {"reason": reason, "observation": evidence})
        self.delete("pod", name, self.namespace)
        self.wait(name + " removed", lambda: self.get("pod", name, self.namespace, absent=True) is None)

    def bypass_admission(self):
        # Only the invocation namespace bypasses the invocation chart's webhook.
        configurations = self.get("mutatingwebhookconfigurations")["items"]
        candidates = [item for item in configurations
                      if item["metadata"].get("annotations", {}).get("meta.helm.sh/release-name") == self.cluster
                      and item["metadata"].get("annotations", {}).get("meta.helm.sh/release-namespace") == self.control_namespace
                      and item["metadata"].get("labels", {}).get("app.kubernetes.io/managed-by") == "Helm"
                      and any(hook.get("clientConfig", {}).get("service", {}).get("namespace") == self.control_namespace
                              and hook.get("clientConfig", {}).get("service", {}).get("path") == "/mutate-pods"
                              for hook in item.get("webhooks", []))]
        require(len(candidates) == 1, "exact private-release pod webhook could not be identified")
        webhook = candidates[0]
        self.original_pod_webhook = copy.deepcopy(webhook)
        for hook in webhook["webhooks"]:
            if hook.get("clientConfig", {}).get("service", {}).get("path") == "/mutate-pods":
                hook.setdefault("namespaceSelector", {}).setdefault("matchExpressions", []).append(
                    {"key": "kubernetes.io/metadata.name", "operator": "NotIn",
                     "values": [self.namespace]})
        self.kubectl("replace", "-f", "-", input=json.dumps(webhook))

    def restore_admission(self):
        obj = self.get("mutatingwebhookconfiguration", self.original_pod_webhook["metadata"]["name"])
        require(obj["metadata"]["uid"] == self.original_pod_webhook["metadata"]["uid"],
                "private pod webhook was replaced; refusing restoration")
        obj["webhooks"] = self.original_pod_webhook["webhooks"]
        self.kubectl("replace", "-f", "-", input=json.dumps(obj))

    def rotated_vendor_versions(self, language, replacement):
        old = language + "-old"
        require(replacement != old, "vendor update proof requires a distinct replacement pod")
        versions = {}
        inputs = self.runtime_vendor_inputs[language]
        for revision, name in (("before", old), ("after", replacement)):
            observed = self.exec(name, inputs["probe"])
            output = observed.stdout + observed.stderr
            require(runtime_version_matches(output, inputs[revision]["version"]),
                    language + " did not execute its expected vendor runtime " + revision)
            versions[revision] = output.strip()
        return versions

    def barriers_and_rotation(self):
        # Bypass mutation to prove the shim rather than an admission-only denial.
        self.bypass_admission()
        try:
            for name, changes, reason in (
                ("root-bound", {"runAsUser": 0, "runAsNonRoot": False}, "RootNotSupported"),
                ("privilege-bound", {"allowPrivilegeEscalation": True}, "PrivilegeEscalationNotSupported"),
            ):
                pod = self.pod(name, "python", bound=True)
                pod["spec"]["containers"][0]["securityContext"].update(changes)
                self.apply(pod)
                self.no_start(name, reason)
            metadata = self.initial["python"]["metadata"]
            for name, barrier, reason in (
                ("barrier-malformed", "{", "InvalidRuntimeGeneration"),
                ("barrier-epoch", {"id": IDS["python"], "inventoryUID": "wrong-epoch",
                                   "generation": metadata["generation"]}, "RuntimeImageEpochMismatch"),
                ("barrier-future", {"id": IDS["python"], "inventoryUID": metadata["inventoryUID"],
                                    "generation": metadata["generation"] + 100}, "RuntimeImageUpdating"),
            ):
                self.apply(self.pod(name, "python", barrier=barrier, bound=True))
                self.no_start(name, reason, timeout=180)
        finally:
            self.restore_admission()
        for language in LANGUAGES:
            old = self.initial[language]
            held_exec = self.start_held_exec(language)
            inventory = self.get("runtimeimage", IDS[language])
            inventory["spec"]["source"]["image"] = self.runtime_images[language]["after"]
            self.apply(inventory)
            def converged():
                ri = self.get("runtimeimage", IDS[language])
                generation = ri.get("status", {}).get("generation", 0)
                if generation <= old["metadata"]["generation"]:
                    return False
                if not self.advertised(IDS[language], "ready", generation):
                    return False
                meta = self.current(language)
                return meta if meta["generation"] == generation and meta["sourceDigest"] == (
                    self.runtime_images[language]["after"].split("@")[1]) else False
            metadata = self.wait(language + " rotated current generation", converged)
            def rotation_complete():
                status = self.get("runtimeimage", IDS[language]).get("status", {})
                rotation = status.get("rotation", {})
                if not (rotation.get("generation") == metadata["generation"]
                        and rotation.get("digest") == metadata["sourceDigest"]
                        and status.get("rotationCompletedAt")
                        and any(c["type"] == "RotationComplete" and c["status"] == "True"
                                for c in status.get("conditions", []))):
                    return False
                require(any(node["uid"] == self.node_uid for node in rotation.get("participants", [])),
                        "rotation completion omitted the actual installed participating Node UID")
                return status
            rotation_status = self.wait(language + " actual participant rotation completion", rotation_complete)
            completed_at = rotation_status["rotationCompletedAt"]
            require(self.node_exec("cat", STORE + "/" + IDS[language] + "/current/rootfs/.brewlet-source"
                                  ).stdout.strip() == metadata["sourceDigest"],
                    "rotated root marker does not match target source")
            old_obj = self.ready(language + "-old")
            require(bool(old_obj), "rotation disrupted old task/probe")
            require(old_obj["metadata"]["uid"] == old["pod"]["metadata"]["uid"]
                    and old_obj["status"]["containerStatuses"][0]["containerID"]
                    == old["pod"]["status"]["containerStatuses"][0]["containerID"],
                    "runtime rotation replaced an existing task")
            require(self.lease(old_obj) == old["lease"], "rotation changed the existing durable lease")
            self.finish_held_exec(language, held_exec)
            self.assert_report(self.http(language + "-old"), language, arguments=[], cwd="/app")
            require(self.exec(language + "-old", ["/bin/sh", "-ec",
                         "printf %s \"$BREWLET_RUNTIME_REVISION\""]).stdout == "before",
                    "exec silently moved onto newly rotated runtime")
            old_root = f"{STORE}/{IDS[language]}/{old['metadata']['inventoryUID']}/{old['metadata']['generation']}"
            self.node_exec("test", "-d", old_root + "/rootfs")
            barrier = {"id": IDS[language], "inventoryUID": metadata["inventoryUID"],
                       "generation": metadata["generation"]}
            name = language + "-new"
            self.apply(self.pod(name, language, barrier=barrier))
            new_obj = self.wait(language + " replacement behind propagated generation barrier",
                                lambda: self.ready(name))
            require(self.lease(new_obj)["runtime"]["generation"] >= metadata["generation"],
                    "new task lease is below its minimum generation")
            require(self.exec(name, ["/bin/sh", "-ec",
                         "printf %s \"$BREWLET_RUNTIME_REVISION\""]).stdout == "after",
                    "new initial/exec task did not use the new runtime config")
            require(new_obj["spec"]["containers"][0]["image"] == self.app_images[language],
                    "rotation rebuilt or substituted application bytes")
            self.assert_report(self.http(name), language, arguments=[], cwd="/app")
            self.assert_report(json.loads(self.exec(name, self.probe(language)).stdout), language,
                               arguments=["--probe", "two words"], cwd="/app")
            versions = self.rotated_vendor_versions(language, name)
            require(self.get("runtimeimage", IDS[language])["status"]["rotationCompletedAt"] == completed_at,
                    "rotation completion timestamp was rewritten for the same target")
            self.record(language + " source rotation, unchanged app, retained task/exec lease, new barrier",
                        {"oldLease": old["lease"], "newLease": self.lease(new_obj),
                         "rotation": rotation_status["rotation"], "rotationCompletedAt": completed_at,
                         "heldExecRevision": "before", "application": self.app_images[language],
                         "vendorRuntimeVersions": versions},
                        ["LANG-02", "ROT-07", "LEASE-01", "PROF-05"])

    def install_interceptor(self):
        directory = self.work / "interceptor"
        directory.mkdir()
        shutil.copy2(HERE / "interceptor.py", directory / "interceptor.py")
        # The base was built from the vendor-pinned runtime Dockerfile above.
        base = f"registry.brewlet.test/{self.run_id}/runtime/python:base"
        dockerfile = directory / "Dockerfile"
        dockerfile.write_text(f"FROM {base}\nCOPY interceptor.py /interceptor.py\n"
                              'ENTRYPOINT ["python3", "/interceptor.py"]\n')
        tag = f"registry.brewlet.test/{self.run_id}/interceptor:fixture"
        self.build(tag, dockerfile, directory)
        self.load_image(tag)
        dns = "interceptor." + self.control_namespace + ".svc"
        self.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
                  "-subj", "/CN=" + dns, "-addext", "subjectAltName=DNS:" + dns,
                  "-keyout", directory / "tls.key", "-out", directory / "tls.crt"])
        os.chmod(directory / "tls.key", 0o600)
        certificate = base64.b64encode((directory / "tls.crt").read_bytes()).decode()
        self.apply({"apiVersion": "v1", "kind": "Secret",
                    "metadata": {"name": "interceptor-tls", "namespace": self.control_namespace},
                    "type": "kubernetes.io/tls",
                    "data": {"tls.crt": certificate,
                             "tls.key": base64.b64encode((directory / "tls.key").read_bytes()).decode()}})
        self.interceptor_revision = 0
        self.configure_interceptor([], False, wait=False)
        self.apply({"apiVersion": "v1", "kind": "Service",
                    "metadata": {"name": "interceptor", "namespace": self.control_namespace},
                    "spec": {"selector": {"app": self.cluster + "-interceptor"},
                             "ports": [{"port": 443, "targetPort": 9443}]}})
        self.apply({"apiVersion": "apps/v1", "kind": "Deployment",
                    "metadata": {"name": "interceptor", "namespace": self.control_namespace},
                    "spec": {"replicas": 1, "selector": {"matchLabels": {"app": self.cluster + "-interceptor"}},
                             "template": {"metadata": {"labels": {"app": self.cluster + "-interceptor"}},
                                          "spec": {"containers": [{"name": "interceptor", "image": tag,
                                                    "imagePullPolicy": "Never",
                                                    "env": [{"name": "CONFIG", "value": "/config/config.json"}],
                                                    "ports": [{"containerPort": 9443}],
                                                    "readinessProbe": {"httpGet": {"scheme": "HTTPS",
                                                        "path": "/ready", "port": 9443}},
                                                    "volumeMounts": [{"name": "tls", "mountPath": "/certs"},
                                                                     {"name": "config", "mountPath": "/config"}]}],
                                                   "volumes": [{"name": "tls", "secret": {"secretName": "interceptor-tls"}},
                                                               {"name": "config", "secret": {"secretName": "interceptor-config"}}]}}}})
        self.kubectl("-n", self.control_namespace, "rollout", "status",
                     "deployment/interceptor", "--timeout=120s", timeout=150)
        self.apply({"apiVersion": "admissionregistration.k8s.io/v1",
                    "kind": "ValidatingWebhookConfiguration",
                    "metadata": {"name": self.cluster + "-node-interceptor"},
                    "webhooks": [{"name": "node-refusal.acceptance.brewlet.test",
                                  "admissionReviewVersions": ["v1"], "sideEffects": "None",
                                  "failurePolicy": "Fail", "timeoutSeconds": 5,
                                  "clientConfig": {"caBundle": certificate, "service": {
                                      "name": "interceptor", "namespace": self.control_namespace, "path": "/"}},
                                  "rules": [{"operations": ["UPDATE"], "apiGroups": [""],
                                             "apiVersions": ["v1"], "resources": ["nodes"]}]}]})

    def configure_interceptor(self, labels, deny_fence, *, wait=True):
        self.interceptor_revision += 1
        config = {"revision": self.interceptor_revision, "node": self.node,
                  "nodeUID": self.node_uid, "pinnedLabels": labels, "denyFence": deny_fence}
        self.apply({"apiVersion": "v1", "kind": "Secret",
                    "metadata": {"name": "interceptor-config", "namespace": self.control_namespace},
                    "stringData": {"config.json": json.dumps(config)}})
        if wait:
            def observed():
                pods = self.get("pods", namespace=self.control_namespace)["items"]
                matches = [p for p in pods if p["metadata"].get("labels", {}).get("app")
                           == self.cluster + "-interceptor"
                           and p.get("status", {}).get("phase") == "Running"]
                if len(matches) != 1:
                    return False
                response = self.kubectl("-n", self.control_namespace, "exec", matches[0]["metadata"]["name"],
                    "--", "python3", "-c",
                    "import json; print(json.load(open('/config/config.json'))['revision'])").stdout.strip()
                return response == str(self.interceptor_revision)
            self.wait("interceptor observes configured refusal revision", observed, timeout=120)

    def pinned_revocation(self):
        # Eager pins are supported; OnDemand pins deliberately are not.
        profile = self.get("nodeprofile", self.profile)
        for entry in profile["spec"]["runtimeImages"]:
            entry["install"] = "Eager"
        self.apply(profile)
        self.wait("profile eager generation reconciled", lambda:
                  self.get("nodeprofile", self.profile).get("status", {}).get("provisioningGeneration")
                  == self.get("nodeprofile", self.profile)["metadata"]["generation"])
        for language in LANGUAGES:
            self.wait("eager ready before pin injection " + language,
                      lambda language=language: self.advertised(IDS[language], "ready"))
        self.install_interceptor()
        key = "brewlet.sh/runtime." + IDS["python"]
        self.configure_interceptor([key], True)
        self.delete("runtimeimage", IDS["python"])
        # Prove both writes are actually refused by the API server. The worker
        # must not turn this into a confirmed exclusion or completed deletion.
        self.wait("actual label and taint refusals appear in interceptor audit", lambda:
                  all(text in self.kubectl("-n", self.control_namespace, "logs",
                      "deployment/interceptor", "--tail=500").stdout for text in
                      ("provider-pinned label refusal", "provider startup-taint write refusal")), timeout=180)
        time.sleep(8)
        ri = self.get("runtimeimage", IDS["python"])
        require(ri["metadata"].get("deletionTimestamp") and ri["metadata"].get("finalizers"),
                "inventory deletion completed despite unconfirmed fencing and active leases")
        node = self.node_snapshot()
        require(node["metadata"]["labels"].get(key) == "ready"
                and not any(t["key"] == FENCE for t in node.get("spec", {}).get("taints", [])),
                "double-refusal observation was not the intended retained-label/unfenced state")
        diagnostics = json.dumps(ri.get("status", {})) + json.dumps(
            self.get("nodeprofile", self.profile).get("status", {}))
        require(any(reason in diagnostics for reason in
                    ("node-advertisement-failed", "refus", "blocked", "Blocked", "without startup fence")),
                "both-write failure lacks explicit blocked/error status")
        require(not any(c["type"] == "CleanupComplete" and c["status"] == "True"
                        for c in ri.get("status", {}).get("conditions", [])),
                "deletion reported cleanup complete without confirmed fencing")
        self.record("real admission refuses both label withdrawal and taint fallback; deletion blocked",
                    {"runtimeImage": ri, "diagnostics": diagnostics}, ["E2E-07", "FENCE-03"])
        self.configure_interceptor([key], False)
        self.wait("node-wide fallback fence confirmed after label refusal", lambda:
                  any(t["key"] == FENCE and t.get("value") == "provisioning"
                      and t["effect"] == "NoSchedule"
                      for t in self.node_snapshot().get("spec", {}).get("taints", [])), timeout=180)
        require(self.node_snapshot()["metadata"]["labels"].get(key) == "ready",
                "provider-pinned label was not retained")
        for language in LANGUAGES:
            self.assert_report(self.http(language + "-old"), language, arguments=[], cwd="/app")
            require(self.lease(self.get("pod", language + "-old", self.namespace))
                    == self.initial[language]["lease"], "fencing disrupted a running lease")
        self.bypass_admission()
        try:
            self.apply(self.pod("revoked-bound", "python", bound=True))
            self.no_start("revoked-bound", "RuntimeImageRevoked", timeout=180)
        finally:
            self.restore_admission()
        pending = self.pod("nodewide-fence-pending", "node")
        self.apply(pending)
        time.sleep(8)
        require(not self.get("pod", "nodewide-fence-pending", self.namespace)["spec"].get("nodeName"),
                "fallback fence did not block another runtime ID's new scheduling")
        self.record("real pinned-label fallback is node-wide; old leases survive and bound revoked start fails",
                    {"nodeUID": self.node_uid, "retainedLabel": key}, ["E2E-06", "E2E-08", "INV-05"])
        self.configure_interceptor([], False)

    def production_cleanup(self):
        # Active tasks must prevent root cleanup/claim release. Never remove a
        # finalizer or invent cleanup receipts to make this assertion pass.
        self.delete("nodeprofile", self.profile)
        time.sleep(8)
        obj = self.get("nodeprofile", self.profile)
        require(obj["metadata"].get("deletionTimestamp") and obj["metadata"].get("finalizers"),
                "profile cleanup released finalizers while runtime tasks were active")
        require(self.node_snapshot()["metadata"]["labels"].get("brewlet.sh/owner-uid")
                == self.owned[("nodeprofile", "", self.profile)],
                "active task cleanup released the profile's claim")
        for language in LANGUAGES:
            self.node_exec("test", "-d", f"{STORE}/{IDS[language]}/"
                           f"{self.initial[language]['metadata']['inventoryUID']}/"
                           f"{self.initial[language]['metadata']['generation']}/rootfs")
            self.assert_report(self.http(language + "-old"), language, arguments=[], cwd="/app")
        self.record("production profile deletion preserves active roots, finalizer and UID claim",
                    {"profileUID": obj["metadata"]["uid"]}, ["LEASE-03", "OWN-02"])
        pods = self.get("pods", namespace=self.namespace)["items"]
        for pod in pods:
            require_owned(pod, self.run_id)
            self.delete("pod", pod["metadata"]["name"], self.namespace)
        self.wait("all invocation workload pods deleted", lambda:
                  not self.get("pods", namespace=self.namespace)["items"])
        self.wait("production profile cleanup and finalizer complete", lambda:
                  self.get("nodeprofile", self.profile, absent=True) is None)
        node = self.node_snapshot()
        require(not node["metadata"].get("labels", {}).get("brewlet.sh/owner-uid"),
                "production cleanup left profile claim after finalization")
        for language in LANGUAGES:
            rid = IDS[language]
            require(self.node_exec("test", "!", "-e", STORE + "/" + rid + "/current",
                                   check=False).returncode == 0, "cleanup left a current root")
            self.delete("runtimeimage", rid)
            self.wait("inventory cleanup " + rid, lambda rid=rid:
                      self.get("runtimeimage", rid, absent=True) is None)
        self.record("production cleanup drains workloads before root/finalizer/claim release",
                    {"nodeUID": self.node_uid}, ["OWN-02", "LEASE-03"])

    def diagnostics(self):
        if not self.kubeconfig.exists():
            return
        commands = [
            ("objects", ["get", "nodes,nodeprofiles,runtimeimages", "-o", "json"]),
            ("pods", ["get", "pods", "-A", "-o", "json"]),
            ("events", ["get", "events", "-A", "-o", "json"]),
            ("operator", ["-n", self.control_namespace, "logs", "deployment/brewlet-operator", "--tail=1000"]),
        ]
        profile_uid = self.owned.get(("nodeprofile", "", self.profile))
        if profile_uid:
            commands.append(("workers", [
                "-n", self.control_namespace, "logs", "-l", "brewlet.sh/owner-uid=" + profile_uid,
                "--all-containers=true", "--prefix=true", "--tail=1000", "--max-log-requests=5"]))
        for label, args in commands:
            try:
                result = self.kubectl(*args, check=False, timeout=40)
                (self.work / ("diagnostic-" + label + ".log")).write_text(result.stdout + result.stderr)
            except Exception as exc:
                (self.work / ("diagnostic-" + label + ".log")).write_text(str(exc))

    def destroy(self):
        """Disposable-fixture destruction, never evidence of production cleanup."""
        errors = []
        if self.node_container:
            try:
                nodes = self.run(["docker", "ps", "-aq", "--filter",
                                  "label=io.x-k8s.kind.cluster=" + self.cluster]).stdout.split()
                require(len(nodes) == 1, "cluster node inventory changed; refusing cluster deletion")
                info = json.loads(self.run(["docker", "inspect", nodes[0]]).stdout)[0]
                docker_owned(info, self.run_id, self.node_container, kind_node=True)
                self.run(["kind", "delete", "cluster", "--name", self.cluster,
                          "--kubeconfig", self.kubeconfig], timeout=180)
                require(not self.run(["docker", "ps", "-aq", "--filter",
                                     "label=io.x-k8s.kind.cluster=" + self.cluster]).stdout.strip(),
                        "owned kind node remains after fixture destruction")
            except Exception as exc:
                errors.append(str(exc))
        if self.registry_id:
            try:
                info = json.loads(self.run(["docker", "inspect", self.registry_id]).stdout)[0]
                docker_owned(info, self.run_id, self.registry_id)
                self.run(["docker", "rm", "-f", "-v", self.registry_id])
                require(self.run(["docker", "inspect", self.registry_id], check=False).returncode != 0,
                        "owned registry remains after fixture destruction")
            except Exception as exc:
                errors.append(str(exc))
        for tag, expected in self.images.items():
            try:
                result = self.run(["docker", "image", "inspect", tag], check=False)
                if result.returncode:
                    continue
                info = json.loads(result.stdout)[0]
                require(info["Id"] == expected and info["Config"]["Labels"].get(OWNER) == self.run_id,
                        "image tag now refers to foreign bytes; refusing removal")
                # Only untag the recorded name, never remove by shared image ID.
                self.run(["docker", "image", "rm", tag])
            except Exception as exc:
                errors.append(str(exc))
        self.result["fixtureDestruction"] = {"outcome": "failed" if errors else "passed", "errors": errors,
                                             "notProductionCleanupEvidence": True}
        self.save()
        return errors

    def execute(self):
        status = 1
        try:
            self.phase = "prerequisites"
            self.preflight()
            self.phase = "cluster"
            self.provision_cluster()
            self.phase = "build"
            self.prepare_images()
            self.phase = "remote-delivery-bootstrap"
            from delivery import bootstrap as bootstrap_delivery
            bootstrap_delivery(self)
            self.phase = "install"
            self.install()
            self.phase = "startup"
            self.startup()
            self.phase = "execution"
            self.four_language_execution()
            self.phase = "rotation"
            self.barriers_and_rotation()
            self.phase = "cri-accounting"
            from accounting import execute as verify_accounting
            verify_accounting(self)
            self.phase = "remote-delivery"
            from delivery import execute as verify_delivery
            verify_delivery(self)
            self.phase = "lifecycle"
            from lifecycle import execute as verify_lifecycle
            verify_lifecycle(self)
            self.phase = "control-edges"
            from control_edges import execute as verify_control_edges
            verify_control_edges(self)
            if self.pinned:
                self.phase = "pinned-refusal"
                self.pinned_revocation()
            self.phase = "production-cleanup"
            from control_edges import before_cleanup, after_cleanup
            before_cleanup(self)
            self.production_cleanup()
            after_cleanup(self)
            require(not (self.work / "manual-intervention.json").exists(),
                    "manual intervention invalidates a clean selected-suite acceptance pass")
            self.result["outcome"] = "passed"
            status = 0
        except Exception as exc:
            self.result["outcome"] = "failed"
            self.result["failureClass"] = failure_class(self.phase, str(exc))
            self.result["failurePhase"] = self.phase
            self.result["failure"] = str(exc)
            self.records.append({"name": "run completion", "outcome": "failed", "evidence": str(exc),
                                 "requirements": []})
            print("FAIL:", exc, file=sys.stderr, flush=True)
        finally:
            errors = []
            try:
                self.stop_helpers()
                self.diagnostics()
            except Exception as exc:
                errors.append("diagnostic/helper cleanup: " + str(exc))
            finally:
                try:
                    from delivery import cleanup as cleanup_delivery
                    cleanup_delivery(self)
                except Exception as exc:
                    errors.append("remote-delivery fixture cleanup: " + str(exc))
                errors.extend(self.destroy())
            if errors:
                self.result["outcome"] = "failed"
                self.records.append({"name": "fixture destruction", "outcome": "failed",
                                     "evidence": errors, "requirements": []})
                status = 1
            self.save()
            print("Retained private evidence:", self.work, flush=True)
        return status


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", type=Path, required=True,
                        help="new directory beneath integration-tests/live-v2; retains private kubeconfig/keys")
    parser.add_argument("--timeout", type=int, default=600,
                        help="bounded assertion deadline in seconds (default 600)")
    parser.add_argument("--without-pinned-scenarios", action="store_true",
                        help="explicit partial execution suite; records the omitted coverage as a gap")
    args = parser.parse_args()
    require(30 <= args.timeout <= 1800, "timeout must be between 30 and 1800 seconds")
    def interrupted(signum, frame):
        raise Failure("interrupted by signal " + str(signum))
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    harness = Harness(args.work, timeout=args.timeout, pinned=not args.without_pinned_scenarios)
    return harness.execute()


if __name__ == "__main__":
    sys.exit(main())
