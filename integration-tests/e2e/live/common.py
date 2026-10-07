#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Strict, invocation-owned fixtures; deliberately independent of the tier reset."""

import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid
from urllib.request import urlopen

ROOT = Path(__file__).resolve().parents[3]
# Exact pins keep every fresh cluster reproducible. The node image is the
# multi-arch (amd64/arm64) Kubernetes 1.34 image published with this kind release.
KIND_VERSION = "v0.33.0"
KIND_IMAGE = ("kindest/node:v1.34.11@sha256:"
              "44e222ee2132dab25ff87301682f89eb82c7880ea3a1bf543bfe9708fd08d67d")
REGISTRY_IMAGE = "registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e"
JDK_IMAGE = "docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b"
OWNER_LABEL = "sh.brewlet.live-owner"
GC_STARTUP_DELAY = "24h"
# Setup steps that reach external infrastructure (Maven Central, Go proxy, image
# registries, GitHub) are retried a bounded number of times on these transient
# network symptoms only. Mandatory assertions are never retried or skipped.
TRANSIENT_PATTERNS = (
    "nodename nor servname provided", "no such host", "Temporary failure in name resolution",
    "Name or service not known", "Could not resolve host", "UnknownHostException",
    "Connection reset", "connection reset by peer", "Connection timed out",
    "connect timed out", "Read timed out", "SocketTimeoutException", "i/o timeout",
    "TLS handshake timeout", "Remote host terminated the handshake",
    "Premature end of Content-Length", "502 Bad Gateway", "503 Service Unavailable",
    "504 Gateway Time", "status code: 502", "status code: 503", "status code: 504",
    "toomanyrequests", "net/http: request canceled",
)
# A Maven goal that publishes may only be rerun when it failed while resolving
# dependencies or plugins, i.e. before any mojo could have pushed anything.
MAVEN_RESOLUTION_MARKERS = (
    "Could not resolve dependencies", "Could not transfer artifact",
    "Failed to read artifact descriptor", "Failed to collect dependencies",
    "or one of its dependencies could not be resolved", "Could not resolve artifact",
    "Non-resolvable parent POM", "Plugin not found",
)
MAVEN_NETWORK_RETRIES = ["-Daether.connector.http.retryHandler.count=5",
                         "-Dmaven.wagon.http.retryHandler.count=5"]


class InfrastructureError(RuntimeError):
    """Setup could not reach external infrastructure; no assertion ran or was skipped."""


def failure_class(error):
    if isinstance(error, InfrastructureError):
        return "infrastructure"
    if isinstance(error, (AssertionError, TimeoutError)):
        return "assertion"
    return "error"


def transient_failure(text, *, resolution_only=False):
    """Return the matched transient symptom, or None for a real failure."""
    text = str(text)
    if resolution_only and not any(marker in text for marker in MAVEN_RESOLUTION_MARKERS):
        return None
    lowered = text.lower()
    return next((p for p in TRANSIENT_PATTERNS if p.lower() in lowered), None)


def retry_transient(description, attempt, *, attempts=4, backoff=10, resolution_only=False,
                    sleep=time.sleep):
    """Rerun a failed setup command only on transient infrastructure symptoms.

    ``attempt`` returns a CompletedProcess. Success and non-transient failures are
    returned to the caller unchanged; exhausted transient failures raise
    InfrastructureError so they are never confused with an assertion failure.
    """
    for number in range(1, attempts + 1):
        result = attempt()
        if result.returncode == 0:
            return result
        reason = transient_failure(result.stdout + result.stderr,
                                   resolution_only=resolution_only)
        if not reason:
            return result
        if number == attempts:
            raise InfrastructureError(
                f"INFRASTRUCTURE ERROR (not an assertion failure): {description} failed "
                f"{attempts} times on a transient network error ({reason!r}); no assertion "
                "ran or was skipped")
        delay = backoff * 2 ** (number - 1)
        print(f"INFRA RETRY {number}/{attempts - 1}: {description}: {reason!r}; "
              f"retrying in {delay}s", flush=True)
        sleep(delay)
    raise ValueError("attempts must be at least 1")


MIN_PYTHON = (3, 12)


def require_python(version=sys.version_info):
    if tuple(version[:2]) < MIN_PYTHON:
        raise RuntimeError(
            f"Live scenarios require Python {MIN_PYTHON[0]}.{MIN_PYTHON[1]}+ (found "
            f"{version[0]}.{version[1]}); run them with a newer interpreter, e.g. python3.12")


def require_kind_version(output):
    found = re.search(r"\bkind (v\d+\.\d+\.\d+\S*)", output or "")
    found = found.group(1) if found else "an unrecognized version"
    if found != KIND_VERSION:
        raise RuntimeError(
            f"This fixture requires kind {KIND_VERSION} (found {found}); install it with "
            f"'go install sigs.k8s.io/kind@{KIND_VERSION}' and put \"$(go env GOPATH)/bin\" "
            "first on PATH. The exact pin keeps the node image and cluster reproducible.")
    return found


def redact(text):
    text = re.sub(r"-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----",
                  "[REDACTED PRIVATE KEY]", str(text), flags=re.S)
    return re.sub(r'(?i)(authorization[": =]+(?:bearer|basic)\s+)\S+',
                  r"\1[REDACTED]", text)


def run(argv, *, input=None, check=True, timeout=300, cwd=None, env=None):
    argv = [str(value) for value in argv]
    process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, text=True, cwd=cwd, env=env,
                               start_new_session=True)
    try:
        stdout, stderr = process.communicate(input=input, timeout=timeout)
    except BaseException:
        # Own process group only; never terminate by executable name.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.communicate()
        raise
    result = subprocess.CompletedProcess(argv, process.returncode, stdout, stderr)
    if check and result.returncode:
        raise RuntimeError(redact(f"Command failed ({result.returncode}): "
                                  f"{' '.join(argv)}\n{result.stdout}\n{result.stderr}"))
    return result


def wait(description, predicate, timeout=300, interval=5):
    deadline = time.monotonic() + timeout
    while True:
        value = predicate()
        if value:
            return value
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError(f"Timed out after {timeout}s: {description}")
        time.sleep(min(interval, remaining))


def sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def download(url, path, digest):
    argv = ["curl", "--fail", "--silent", "--show-error", "--location",
            "--retry", "3", "--max-time", "180", url, "-o", path]
    result = retry_transient(f"download {url}", lambda: run(argv, check=False))
    if result.returncode:
        raise RuntimeError(redact(f"Download failed ({result.returncode}): {url}\n{result.stderr}"))
    if sha256(path) != digest:
        raise RuntimeError(f"Checksum mismatch: {path.name}")


def kind_config(name, workers=0):
    # Stock kind discards packed layers after unpack, and containerd collects them
    # within about a second of a pull. A cold runnable-image start needs them until
    # the shim publishes its verified stage (docs/live-validation.md), so defer
    # automatic GC on this disposable node. Fixture.request_containerd_gc() later
    # runs containerd's own collector, after which periodic GC resumes.
    config = f"""kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
- |-
  [plugins."io.containerd.gc.v1.scheduler"]
    deletion_threshold = 0
    startup_delay = "{GC_STARTUP_DELAY}"
nodes:
- role: control-plane
  labels:
    sh.brewlet/live-owner: {name}
  kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    controllerManager:
      extraArgs:
        horizontal-pod-autoscaler-downscale-stabilization: "60s"
        horizontal-pod-autoscaler-sync-period: "10s"
        horizontal-pod-autoscaler-cpu-initialization-period: "30s"
"""
    return config + (f"""- role: worker
  labels:
    sh.brewlet/live-owner: {name}
""" * workers)


def owned_container(info, identifier, label, owner):
    return (info["Id"] == identifier and
            info["Config"].get("Labels", {}).get(label) == owner)


class Fixture:
    def __init__(self, scenario, *, workers=0):
        if scenario not in ("smoke", "hpa", "admission", "workflows", "retirement"):
            raise ValueError("unknown live scenario")
        if workers not in (0, 2):
            raise ValueError("fixtures support zero or two disposable workers")
        self.scenario = scenario
        self.name = f"brewlet-live-{scenario}-{uuid.uuid4().hex[:12]}"
        base = Path(os.environ.get("BREWLET_LIVE_OUTPUT", tempfile.gettempdir()))
        base.mkdir(parents=True, exist_ok=True)
        self.work = base / self.name
        self.work.mkdir(mode=0o700)
        # Sibling, not a child of the uploaded diagnostics directory.
        self.private = Path(tempfile.mkdtemp(prefix=f"{self.name}-private-"))
        self.kubeconfig = self.private / "kubeconfig"
        self.context = f"kind-{self.name}"
        self.namespace = "live-e2e"
        self.node = f"{self.name}-control-plane"
        self.node_id = None
        self.worker_names = [f"{self.name}-worker{n if n > 1 else ''}"
                             for n in range(1, workers + 1)]
        self.worker_ids = {}
        self.registry_id = None
        self.network_id = None
        self.children = []
        # Extra invocation-owned cleanup callables, run before the node is removed.
        self.cleanups = []
        self.evidence = []
        self.env = dict(os.environ, KUBECONFIG=str(self.kubeconfig),
                        KIND_EXPERIMENTAL_DOCKER_NETWORK=self.name)
        if not self.env.get("DOCKER_HOST"):
            # The private DOCKER_CONFIG below hides the active context (e.g. Docker
            # Desktop's desktop-linux), so pin its endpoint first.
            endpoint = subprocess.run(
                ["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, env=self.env,
                check=False).stdout.strip()
            if endpoint:
                self.env["DOCKER_HOST"] = endpoint
        docker_config = self.private / "docker"
        docker_config.mkdir()
        (docker_config / "config.json").write_text("{}")
        # Keep CLI plugins (buildx) reachable without exposing the user's credentials.
        plugins = Path(os.environ.get("DOCKER_CONFIG") or Path.home() / ".docker") / "cli-plugins"
        if plugins.is_dir():
            (docker_config / "cli-plugins").symlink_to(plugins.resolve(), target_is_directory=True)
        self.env["DOCKER_CONFIG"] = str(docker_config)
        self.env["HELM_REGISTRY_CONFIG"] = str(self.private / "helm-registry.json")
        Path(self.env["HELM_REGISTRY_CONFIG"]).write_text("{}")
        self.cli = self.private / "bin" / "brewlet"
        self.maven_args = ["mvn", "-B", "--no-transfer-progress", "-q", *MAVEN_NETWORK_RETRIES,
                           f"-Dmaven.repo.local={self.private / 'm2'}",
                           "--settings", str(self.private / "settings.xml")]
        (self.private / "settings.xml").write_text("<settings/>")
        self.old_signals = {}
        self.failure_class = None
        print(f"Evidence: {self.work}", flush=True)

    def run(self, argv, **kwargs):
        kwargs.setdefault("env", self.env)
        return run(argv, **kwargs)

    def pull(self, ref):
        """Pre-pull a pinned image once, with bounded retries, before it is needed."""
        result = retry_transient(f"docker pull {ref}", lambda: self.run(
            ["docker", "pull", "--platform", f"linux/{self.arch}", ref],
            check=False, timeout=600))
        if result.returncode:
            raise RuntimeError(redact(f"docker pull failed ({result.returncode}): {ref}\n"
                                      f"{result.stderr}"))
        return result

    def classify(self, error):
        self.failure_class = failure_class(error)
        if self.failure_class == "infrastructure":
            print(redact(str(error)), flush=True)

    def save(self, name, data):
        if Path(name).name != name:
            raise ValueError("diagnostic names must be basenames")
        text = data if isinstance(data, str) else json.dumps(data, indent=2)
        (self.work / name).write_text(redact(text) + "\n")

    def record(self, name, details):
        self.evidence.append({"assertion": name, "time": time.time(), "details": details})
        self.save("assertions.json", self.evidence)
        print(f"PASS: {name}", flush=True)

    def kube(self, *args, **kwargs):
        if any(str(arg).startswith(("--context", "--kubeconfig", "--server", "--cluster"))
               for arg in args):
            raise ValueError("fixture Kubernetes target cannot be overridden")
        return self.run(["kubectl", "--kubeconfig", self.kubeconfig,
                         "--context", self.context, "--request-timeout=45s", *args],
                        **kwargs)

    def get(self, *args):
        return json.loads(self.kube("get", *args, "-o", "json").stdout)

    def apply(self, obj):
        return self.kube("apply", "-f", "-", input=json.dumps(obj))

    def own_container(self, name):
        info = json.loads(self.run(["docker", "inspect", name]).stdout)[0]
        if name == self.node:
            valid = owned_container(info, self.node_id, "io.x-k8s.kind.cluster", self.name)
        elif name in self.worker_ids:
            valid = owned_container(info, self.worker_ids[name], "io.x-k8s.kind.cluster", self.name)
        else:
            valid = owned_container(info, self.registry_id, OWNER_LABEL, self.name)
        if not valid:
            raise RuntimeError(f"Refusing to touch foreign/replaced container: {name}")
        return info

    def __enter__(self):
        def interrupted(signum, _frame):
            raise InterruptedError(f"Fixture interrupted by signal {signum}")
        for sig in (signal.SIGINT, signal.SIGTERM):
            self.old_signals[sig] = signal.signal(sig, interrupted)
        try:
            self.start()
        except BaseException as error:
            self.classify(error)
            try:
                self.save("failure.txt", str(error))
            finally:
                self.finish(False)
            raise
        return self

    def __exit__(self, exc_type, exc, _traceback):
        try:
            if exc:
                self.classify(exc)
                self.save("failure.txt", str(exc))
        finally:
            self.finish(exc_type is None)
        return False

    def start(self):
        require_python()
        for tool in ("kind", "docker", "kubectl", "helm", "java", "javac",
                     "jar", "mvn", "curl", "openssl", "git", "tar"):
            if not shutil.which(tool):
                raise RuntimeError(f"Required prerequisite missing: {tool}; no assertions skipped")
        require_kind_version(self.run(["kind", "version"]).stdout)
        engine = json.loads(self.run(["docker", "info", "--format", "{{json .}}"]).stdout)
        if engine["NCPU"] < 4 or engine["MemTotal"] < 7 * 1024 ** 3:
            raise RuntimeError("Live fixture requires at least 4 Docker CPUs and 7 GiB RAM")
        self.arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64",
                     "amd64": "amd64"}[engine["Architecture"]]
        self.env["DOCKER_DEFAULT_PLATFORM"] = f"linux/{self.arch}"
        for image in (REGISTRY_IMAGE, KIND_IMAGE):
            self.pull(image)
        self.build()
        try:
            self.run(["docker", "network", "create", "--label",
                      f"{OWNER_LABEL}={self.name}", self.name])
        finally:
            result = self.run(["docker", "network", "inspect", self.name], check=False)
            if result.returncode == 0:
                network = json.loads(result.stdout)[0]
                if network.get("Labels", {}).get(OWNER_LABEL) != self.name:
                    raise RuntimeError("Refusing to adopt a foreign fixture network")
                self.network_id = network["Id"]
        self.registry_name = self.name + "-registry"
        try:
            self.run([
                "docker", "create", "--platform", f"linux/{self.arch}", "--name",
                self.registry_name, "--label", f"{OWNER_LABEL}={self.name}",
                "--network", self.name, "--cpus", "0.5", "--memory", "256m",
                "-p", "127.0.0.1::5000", REGISTRY_IMAGE])
        finally:
            result = self.run(["docker", "inspect", self.registry_name], check=False)
            if result.returncode == 0:
                info = json.loads(result.stdout)[0]
                if info["Config"].get("Labels", {}).get(OWNER_LABEL) != self.name:
                    raise RuntimeError("Refusing to adopt a foreign fixture registry")
                self.registry_id = info["Id"]
                self.save("registry-identity.json", {"id": self.registry_id,
                          "name": self.registry_name, "mounts": info["Mounts"]})
        self.run(["docker", "start", self.registry_id])
        info = self.own_container(self.registry_name)
        self.save("registry-identity.json", {"id": self.registry_id,
                  "name": self.registry_name, "mounts": info["Mounts"]})
        port = info["NetworkSettings"]["Ports"]["5000/tcp"][0]["HostPort"]
        self.registry = f"localhost:{port}"
        self.registry_internal = f"{self.registry_name}:5000"
        self.registry_ip = info["NetworkSettings"]["Networks"][self.name]["IPAddress"]
        wait("registry /v2/", lambda: self.run(
            ["curl", "-fsS", "--max-time", "5", f"http://{self.registry}/v2/"],
            check=False).returncode == 0, timeout=60)
        config = self.private / "kind.yaml"
        config.write_text(kind_config(self.name, len(self.worker_names)))
        try:
            result = self.run(["kind", "create", "cluster", "--name", self.name,
                               "--kubeconfig", self.kubeconfig, "--image", KIND_IMAGE,
                               "--config", config, "--wait", "180s", "--retain"], timeout=360)
            self.save("kind-create.log", result.stdout + result.stderr)
        finally:
            for name in [self.node, *self.worker_names]:
                result = self.run(["docker", "inspect", name], check=False)
                if result.returncode == 0:
                    info = json.loads(result.stdout)[0]
                    if info["Config"].get("Labels", {}).get("io.x-k8s.kind.cluster") != self.name:
                        raise RuntimeError("Unexpected kind node ownership")
                    if name == self.node:
                        self.node_id = info["Id"]
                    else:
                        self.worker_ids[name] = info["Id"]
                    filename = "node-identity.json" if name == self.node else f"{name}-identity.json"
                    self.save(filename, {"id": info["Id"],
                              "name": name, "mounts": info["Mounts"]})
        self.own_container(self.node)
        memory = "3g" if self.worker_names else "6g"
        self.run(["docker", "update", "--cpus", "3", "--memory", memory,
                  "--memory-swap", memory, self.node_id])
        for name, identifier in self.worker_ids.items():
            self.own_container(name)
            self.run(["docker", "update", "--cpus", "2", "--memory", "1536m",
                      "--memory-swap", "1536m", identifier])
        self.kube("wait", "--for=condition=Ready", "nodes", "--all", "--timeout=180s")
        nodes = self.get("nodes")["items"]
        if ({n["metadata"]["name"] for n in nodes} != {self.node, *self.worker_names}
                or any(n["metadata"]["labels"].get("sh.brewlet/live-owner") != self.name for n in nodes)):
            raise RuntimeError("Kubeconfig does not identify the owned disposable nodes")
        self.cluster_uid = self.get("namespace", "kube-system")["metadata"]["uid"]
        for identifier in [self.node_id, *self.worker_ids.values()]:
            for host in (self.registry, self.registry_internal):
                target = f"/etc/containerd/certs.d/{host}"
                self.run(["docker", "exec", identifier, "mkdir", "-p", target])
                self.run(["docker", "exec", "-i", identifier, "tee", f"{target}/hosts.toml"],
                         input=f'[host."http://{self.registry_internal}"]\n'
                               '  capabilities = ["pull", "resolve"]\n')
        self.save("identity.json", {"name": self.name, "clusterUID": self.cluster_uid,
                  "nodeID": self.node_id, "registryID": self.registry_id,
                  "networkID": self.network_id, "arch": self.arch, "workerIDs": self.worker_ids})
        self.apply({"apiVersion": "v1", "kind": "Namespace",
                    "metadata": {"name": self.namespace}})
        if self.worker_names:
            self.kube("taint", "node", self.node, "node-role.kubernetes.io/control-plane:NoSchedule-")
        self.provision()

    def build(self):
        raise NotImplementedError("Live scenarios must supply checkout-built components")

    def component_images(self):
        raise NotImplementedError("Live scenarios must supply checkout-built images")

    def provision(self):
        images = self.component_images()
        values = {"defaultProfile": {"enabled": False},
                  "images": images,
                  "operator": {"leaderElect": False},
                  "admission": {"failurePolicy": "Ignore", "nodeProfileFailurePolicy": "Fail"}}
        if self.worker_names:
            for component in ("operator", "admission"):
                values[component]["nodeSelector"] = {"kubernetes.io/hostname": self.node}
        path = self.private / "values.json"
        path.write_text(json.dumps(values))
        result = self.run(["helm", "--kubeconfig", self.kubeconfig,
                           "--kube-context", self.context, "install", "brewlet",
                           self.chart, "--namespace", "default", "-f", path,
                           "--wait", "--timeout", "240s"], timeout=300)
        self.save("helm-install.log", result.stdout + result.stderr)
        # Bootstrap with the shipped default; fail closed before creating any workload.
        self.kube("patch", "mutatingwebhookconfiguration", "brewlet-admission",
                  "--type=json", "-p", json.dumps([
                      {"op": "replace", "path": "/webhooks/0/failurePolicy", "value": "Fail"}]))
        self.kube("label", "node", self.node, "brewlet.sh/e2e-pool=live")
        self.apply({
            "apiVersion": "node.brewlet.sh/v1alpha1", "kind": "NodeProfile",
            "metadata": {"name": "live"},
            "spec": {"nodePool": {"key": "brewlet.sh/e2e-pool", "names": ["live"],
                                  "includeControlPlane": True},
                     "jdks": [{"distribution": "temurin", "feature": 21,
                               "source": {"image": JDK_IMAGE, "javaHome": "/opt/java/openjdk"}}],
                     "rollout": {"validate": True, "containerdRestart": "validated"}},
        })
        wait("real provisioner advertises temurin-21",
             lambda: self.get("node", self.node)["metadata"]["labels"].get(
                 "brewlet.sh/jdk.temurin-21") == "true", timeout=480)
        self.kube("rollout", "status", "daemonset/brewlet-node-provisioner-live",
                  "-n", "brewlet", "--timeout=180s")
        self.record("runtime-provisioned", {"mode": "checkout",
                    "status": self.get("nodeprofile", "live")["status"]})

    def node_blob_present(self, digest):
        self.own_container(self.node)
        return self.run(["docker", "exec", self.node_id, "test", "-e",
                         "/var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/"
                         + digest.removeprefix("sha256:")], check=False).returncode == 0

    def request_containerd_gc(self):
        """Run containerd's own collector once; it never deletes blobs directly."""
        self.own_container(self.node)
        lease = f"brewlet-live-gc-{uuid.uuid4().hex[:12]}"
        ctr = ["docker", "exec", self.node_id, "ctr", "-n", "k8s.io", "leases"]
        self.run([*ctr, "create", "--id", lease])
        self.run([*ctr, "delete", "--sync", lease], timeout=120)
        return lease

    def load_image(self, ref):
        self.own_container(self.node)
        return self.run(["kind", "load", "docker-image", "--name", self.name, ref],
                        timeout=300)

    def publish_demo(self):
        app = self.private / "demo-app"
        shutil.copytree(ROOT / "integration-tests/fixtures/demo-app", app,
                        ignore=shutil.ignore_patterns("target"))
        argv = [*self.maven_args, "-f", app / "pom.xml", "package", self.plugin + ":push",
                f"-Dbrewlet.image={self.registry}/apps/demo:live", "-Dbrewlet.jdk=21"]
        result = retry_transient("publish-demo dependency resolution", lambda: self.run(
            argv, check=False, timeout=360), resolution_only=True)
        if result.returncode:
            raise RuntimeError(redact(f"publish-demo failed ({result.returncode})\n"
                                      f"{result.stdout}\n{result.stderr}"))
        self.save("publish-demo.log", result.stdout + result.stderr)
        with urlopen(f"http://{self.registry}/v2/apps/demo/manifests/live", timeout=10) as response:
            digest = response.headers["Docker-Content-Digest"]
        if not re.fullmatch(r"sha256:[a-f0-9]{64}", digest or ""):
            raise RuntimeError("Registry omitted immutable demo digest")
        return f"{self.registry_internal}/apps/demo@{digest}"

    def java_application(self, name, image, autoscaling=False):
        obj = {"apiVersion": "apps.brewlet.sh/v1alpha1", "kind": "JavaApplication",
               "metadata": {"name": name, "namespace": self.namespace},
               "spec": {"artifact": {"image": image, "pullPolicy": "IfNotPresent"},
                        "replicas": 1, "jvm": {"version": 21, "distribution": "temurin"},
                        "ports": [{"name": "http", "containerPort": 8080}],
                        "resources": {"requests": {"cpu": "100m", "memory": "128Mi"},
                                      "limits": {"cpu": "500m", "memory": "256Mi"}},
                        "probes": {"readiness": {"httpGet": {"path": "/healthz", "port": 8080},
                                                  "periodSeconds": 2, "timeoutSeconds": 2}}}}
        if autoscaling:
            obj["spec"]["autoscaling"] = {"enabled": True, "minReplicas": 1, "maxReplicas": 3,
                                         "targetCPUUtilizationPercentage": 50}
            obj["spec"]["jvm"]["args"] = ["-Dbrewlet.test.cpuLoad=true"]
        self.apply(obj)
        return obj

    def ready(self, name, count=None):
        dep = self.get("deployment", name, "-n", self.namespace)
        app = self.get("javaapplication", name, "-n", self.namespace)
        n = dep["spec"]["replicas"]
        if count is not None and n != count:
            return False
        if dep.get("status", {}).get("readyReplicas", 0) != n:
            return False
        if app.get("status", {}).get("readyReplicas", 0) != n:
            return False
        if app["status"].get("observedGeneration") != app["metadata"]["generation"]:
            return False
        if not any(c["type"] == "Ready" and c["status"] == "True" and
                   c.get("observedGeneration") == app["metadata"]["generation"]
                   for c in app["status"].get("conditions", [])):
            return False
        pods = self.get("pods", "-n", self.namespace,
                        "-l", f"app.kubernetes.io/name={name}")["items"]
        active = [p for p in pods if not p["metadata"].get("deletionTimestamp")]
        if len(active) != n or any(
            p["spec"].get("runtimeClassName") != "brewlet" or
            not any(c["type"] == "Ready" and c["status"] == "True"
                    for c in p.get("status", {}).get("conditions", [])) for p in active):
            return False
        endpoints = self.get("endpointslice", "-n", self.namespace,
                             "-l", f"kubernetes.io/service-name={name}")["items"]
        ready_ips = {address for s in endpoints for e in s.get("endpoints", [])
                     if e.get("conditions", {}).get("ready")
                     for address in e["addresses"]}
        return ready_ips == {p["status"]["podIP"] for p in active} and n > 0

    def wait_ready(self, name):
        self.kube("rollout", "status", f"deployment/{name}", "-n", self.namespace,
                  "--timeout=240s")
        wait(f"{name} status, runtime pods and endpoints agree", lambda: self.ready(name))

    def service_get(self, name, path="/hello"):
        return self.kube("get", "--raw",
                         f"/api/v1/namespaces/{self.namespace}/services/{name}:8080/proxy"
                         + path, timeout=50).stdout

    def diagnostics(self):
        if self.node_id:
            self.own_container(self.node)
            for filename, args in (
                ("resources.json", ["get", "pods,deploy,hpa,javaapplication,nodeprofile,endpointslice", "-A", "-o", "json"]),
                ("events.json", ["get", "events", "-A", "-o", "json"]),
                ("metrics.json", ["get", "--raw", "/apis/metrics.k8s.io/v1beta1/pods"]),
                ("nodes.json", ["get", "nodes", "-o", "json"]),
                ("retirement-evidence.json", ["get", "noderetirementevidence.node.brewlet.sh", "-o", "json"]),
            ):
                result = self.kube(*args, check=False, timeout=60)
                self.save(filename, result.stdout + result.stderr)
            pods = self.kube("get", "pods", "-A", "-o", "json", check=False)
            if pods.returncode == 0:
                for pod in json.loads(pods.stdout)["items"]:
                    meta = pod["metadata"]
                    result = self.kube("logs", "-n", meta["namespace"], meta["name"],
                                       "--all-containers", "--tail=250", check=False, timeout=60)
                    self.save(f"pod-{meta['namespace']}-{meta['name']}.log",
                              result.stdout + result.stderr)
            result = self.run(["docker", "exec", self.node_id, "journalctl", "-u", "kubelet",
                               "-u", "containerd", "--no-pager", "-n", "500"], check=False)
            self.save("node.log", result.stdout + result.stderr)
            for name, identifier in self.worker_ids.items():
                self.own_container(name)
                result = self.run(["docker", "exec", identifier, "journalctl", "-u", "kubelet",
                                   "-u", "containerd", "--no-pager", "-n", "500"], check=False)
                self.save(f"{name}.log", result.stdout + result.stderr)
        if self.registry_id:
            self.own_container(self.registry_name)
            result = self.run(["docker", "logs", "--tail", "500", self.registry_id], check=False)
            self.save("registry.log", result.stdout + result.stderr)

    def finish(self, passed):
        errors = []
        for child in self.children:
            try:
                if child.poll() is None:
                    child.terminate()
                    try:
                        child.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        child.kill()
                        child.wait(timeout=10)
            except ProcessLookupError:
                # The child exited between poll and terminate.
                pass
            except (OSError, subprocess.TimeoutExpired) as error:
                errors.append(f"child cleanup: {error}")
        try:
            self.diagnostics()
        except (RuntimeError, subprocess.TimeoutExpired, OSError) as error:
            errors.append(f"diagnostics: {error}")
        for cleanup in reversed(getattr(self, "cleanups", [])):
            try:
                cleanup()
            except (RuntimeError, subprocess.TimeoutExpired, OSError) as error:
                errors.append(f"cleanup: {error}")
        for name, identifier in [(self.node, self.node_id),
                                 *getattr(self, "worker_ids", {}).items(),
                                 (getattr(self, "registry_name", ""), self.registry_id)]:
            if identifier:
                try:
                    self.own_container(name)
                    self.run(["docker", "rm", "-f", "--volumes", identifier])
                except (RuntimeError, subprocess.TimeoutExpired, OSError) as error:
                    errors.append(f"cleanup: {error}")
        if self.network_id:
            try:
                network = json.loads(self.run(["docker", "network", "inspect", self.name]).stdout)[0]
                if network["Id"] != self.network_id or network["Labels"].get(OWNER_LABEL) != self.name:
                    raise RuntimeError("Refusing to remove foreign/replaced network")
                self.run(["docker", "network", "rm", self.network_id])
            except (RuntimeError, subprocess.TimeoutExpired, OSError) as error:
                errors.append(f"cleanup: {error}")
        for sig, handler in self.old_signals.items():
            signal.signal(sig, handler)
        try:
            shutil.rmtree(self.private)
        except OSError as error:
            errors.append(f"private material cleanup: {error}")
        try:
            self.save("result.json", {"passed": passed and not errors, "errors": errors,
                                     "failureClass": getattr(self, "failure_class", None),
                                     "scenario": self.scenario, "assertions": len(self.evidence)})
        except OSError as error:
            errors.append(f"result persistence: {error}")
        if errors:
            raise RuntimeError("; ".join(errors))
