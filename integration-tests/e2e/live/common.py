#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Strict, invocation-owned fixtures; deliberately independent of the tier reset."""

import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import signal
import subprocess
import tarfile
import tempfile
import time
import uuid
from urllib.request import urlopen

ROOT = Path(__file__).resolve().parents[3]
RELEASE = "0.5.0"
RELEASE_COMMIT = "f0b9334f7b29177d2ba4b49b044163ef69e16af7"
KIND_IMAGE = "kindest/node@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"
REGISTRY_IMAGE = "registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e"
JDK_IMAGE = "docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b"
CHART_SHA = "c270ac8ec08067fe6044bba2bd856e40d010d7692447e1e7d95b39200afd26a5"
COMPONENTS = {
    "operator": "ghcr.io/microsoft/brewlet-operator@sha256:40719bb42728a50a20655223365597e6e632bab5f5fd97425a8a7985bd487f69",
    "admission": "ghcr.io/microsoft/brewlet-admission@sha256:351cf755b60d9de01048a9c3127824a4a803dc1bb2e50b46f510f2c72fbc2267",
    "provisioner": "ghcr.io/microsoft/brewlet-node-provisioner@sha256:783c9f88c73c750eb2a195cbcbceccbc8e45a69b8b3e4a4397946780c3d2396f",
}
ASSETS = {
    "darwin_arm64": "67ff59526fa3c448d9e976615ea63c7f70ec7b22795524389912afe782cfcff1",
    "darwin_amd64": "dbccf7bc9b3417f1c773373386120ef0eaf35df2dc3cd811616d72453d61763f",
    "linux_arm64": "0e4a4e55da038d05ce5fc11226fba6eae9957d38ea93dc803f5360f40a436687",
    "linux_amd64": "888fc032fc5f3a773585f014ddf23695ee63e97cb9dbf54b56884e7000385ed1",
}
OWNER_LABEL = "sh.brewlet.live-owner"


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
    run(["curl", "--fail", "--silent", "--show-error", "--location",
         "--retry", "3", "--max-time", "180", url, "-o", path])
    if sha256(path) != digest:
        raise RuntimeError(f"Checksum mismatch: {path.name}")


def owned_container(info, identifier, label, owner):
    return (info["Id"] == identifier and
            info["Config"].get("Labels", {}).get(label) == owner)


class Fixture:
    def __init__(self, scenario):
        if scenario not in ("hpa", "admission"):
            raise ValueError("scenario must be hpa or admission")
        self.candidate = os.environ.get("BREWLET_LIVE_CANDIDATE", "release")
        if self.candidate not in ("release", "shim"):
            raise ValueError("BREWLET_LIVE_CANDIDATE must be release or shim")
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
        self.registry_id = None
        self.network_id = None
        self.candidate_image_id = None
        self.children = []
        self.evidence = []
        self.env = dict(os.environ, KUBECONFIG=str(self.kubeconfig),
                        KIND_EXPERIMENTAL_DOCKER_NETWORK=self.name)
        docker_config = self.private / "docker"
        docker_config.mkdir()
        (docker_config / "config.json").write_text("{}")
        self.env["DOCKER_CONFIG"] = str(docker_config)
        self.env["HELM_REGISTRY_CONFIG"] = str(self.private / "helm-registry.json")
        Path(self.env["HELM_REGISTRY_CONFIG"]).write_text("{}")
        self.source = self.private / "source"
        self.cli = self.private / "bin" / "brewlet"
        self.maven_args = ["mvn", "-B", "--no-transfer-progress", "-q",
                           f"-Dmaven.repo.local={self.private / 'm2'}",
                           "--settings", str(self.private / "settings.xml")]
        (self.private / "settings.xml").write_text("<settings/>")
        self.old_signals = {}
        print(f"Evidence: {self.work}", flush=True)

    def run(self, argv, **kwargs):
        kwargs.setdefault("env", self.env)
        return run(argv, **kwargs)

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
            try:
                self.save("failure.txt", str(error))
            finally:
                self.finish(False)
            raise
        return self

    def __exit__(self, exc_type, exc, _traceback):
        try:
            if exc:
                self.save("failure.txt", str(exc))
        finally:
            self.finish(exc_type is None)
        return False

    def start(self):
        for tool in ("kind", "docker", "kubectl", "helm", "java", "javac",
                     "jar", "mvn", "curl", "openssl", "git", "tar"):
            if not shutil.which(tool):
                raise RuntimeError(f"Required prerequisite missing: {tool}; no assertions skipped")
        if "v0.30.0" not in self.run(["kind", "version"]).stdout:
            raise RuntimeError("This fixture requires kind v0.30.0")
        engine = json.loads(self.run(["docker", "info", "--format", "{{json .}}"]).stdout)
        if engine["NCPU"] < 4 or engine["MemTotal"] < 7 * 1024 ** 3:
            raise RuntimeError("Live fixture requires at least 4 Docker CPUs and 7 GiB RAM")
        self.arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64",
                     "amd64": "amd64"}[engine["Architecture"]]
        self.env["DOCKER_DEFAULT_PLATFORM"] = f"linux/{self.arch}"
        self.release()
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
        config.write_text(f"""kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
nodes:
- role: control-plane
  labels:
    sh.brewlet/live-owner: {self.name}
  kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    controllerManager:
      extraArgs:
        horizontal-pod-autoscaler-downscale-stabilization: "60s"
        horizontal-pod-autoscaler-sync-period: "10s"
        horizontal-pod-autoscaler-cpu-initialization-period: "30s"
""")
        try:
            result = self.run(["kind", "create", "cluster", "--name", self.name,
                               "--kubeconfig", self.kubeconfig, "--image", KIND_IMAGE,
                               "--config", config, "--wait", "180s", "--retain"], timeout=360)
            self.save("kind-create.log", result.stdout + result.stderr)
        finally:
            result = self.run(["docker", "inspect", self.node], check=False)
            if result.returncode == 0:
                info = json.loads(result.stdout)[0]
                if info["Config"].get("Labels", {}).get("io.x-k8s.kind.cluster") != self.name:
                    raise RuntimeError("Unexpected kind node ownership")
                self.node_id = info["Id"]
                self.save("node-identity.json", {"id": self.node_id,
                          "name": self.node, "mounts": info["Mounts"]})
        self.own_container(self.node)
        self.run(["docker", "update", "--cpus", "3", "--memory", "6g",
                  "--memory-swap", "6g", self.node_id])
        self.kube("wait", "--for=condition=Ready", "nodes", "--all", "--timeout=180s")
        nodes = self.get("nodes")["items"]
        if len(nodes) != 1 or nodes[0]["metadata"]["labels"].get("sh.brewlet/live-owner") != self.name:
            raise RuntimeError("Kubeconfig does not identify the owned disposable node")
        self.cluster_uid = self.get("namespace", "kube-system")["metadata"]["uid"]
        for host in (self.registry, self.registry_internal):
            target = f"/etc/containerd/certs.d/{host}"
            self.run(["docker", "exec", self.node_id, "mkdir", "-p", target])
            self.run(["docker", "exec", "-i", self.node_id, "tee", f"{target}/hosts.toml"],
                     input=f'[host."http://{self.registry_internal}"]\n'
                           '  capabilities = ["pull", "resolve"]\n')
        self.save("identity.json", {"name": self.name, "clusterUID": self.cluster_uid,
                  "nodeID": self.node_id, "registryID": self.registry_id,
                  "networkID": self.network_id, "arch": self.arch})
        self.apply({"apiVersion": "v1", "kind": "Namespace",
                    "metadata": {"name": self.namespace}})
        self.provision()

    def release(self):
        host_arch = {"aarch64": "arm64", "arm64": "arm64",
                     "x86_64": "amd64", "amd64": "amd64"}[platform.machine()]
        host = platform.system().lower() + "_" + host_arch
        base = "https://github.com/microsoft/brewlet/releases/download/v0.5.0/"
        archive = self.private / "cli.tar.gz"
        download(base + f"brewlet_0.5.0_{host}.tar.gz", archive, ASSETS[host])
        self.cli.parent.mkdir()
        self.run(["tar", "-xzf", archive, "-C", self.cli.parent])
        if self.run([self.cli, "version"]).stdout.strip() != RELEASE:
            raise RuntimeError("Downloaded CLI is not 0.5.0")
        for suffix, digest in (
            ("jar", "e2bd00643f6d5a89ef448ae6f9724d472a9d09ef58159994a05c928df64764e1"),
            ("pom", "760abd6fc426d6b7e4a278527d4751071e6e4e4c20737f9ad7ce75d75a5b615e"),
        ):
            download(base + f"brewlet-maven-plugin-0.5.0.{suffix}",
                     self.private / f"plugin.{suffix}", digest)
        result = self.run([*self.maven_args,
                           "org.apache.maven.plugins:maven-install-plugin:3.1.4:install-file",
                           f"-Dfile={self.private / 'plugin.jar'}",
                           f"-DpomFile={self.private / 'plugin.pom'}"], timeout=360)
        self.save("maven-install.log", result.stdout + result.stderr)
        self.source.mkdir()
        self.run(["git", "archive", RELEASE_COMMIT, "-o", self.private / "source.tar"],
                 cwd=ROOT)
        self.run(["tar", "-xf", self.private / "source.tar", "-C", self.source])
        result = self.run(["helm", "pull", "oci://ghcr.io/microsoft/charts/brewlet",
                           "--version", RELEASE, "--destination", self.private])
        self.save("chart-pull.log", result.stdout + result.stderr)
        self.chart = self.private / "brewlet-0.5.0.tgz"
        if sha256(self.chart) != CHART_SHA:
            raise RuntimeError("Released chart checksum changed")
        sources = sorted(
            [p for p in (ROOT / "integration-tests/e2e/live").iterdir()
             if p.suffix in (".py", ".go") and p.is_file()] +
            [p for p in (ROOT / "integration-tests/fixtures/demo-app").rglob("*")
             if p.is_file() and "target" not in p.parts])
        if self.candidate == "shim":
            sources += [ROOT / p for p in self.run(
                ["git", "ls-files", "core"], cwd=ROOT).stdout.splitlines()]
        source_hashes = {str(p.relative_to(ROOT)): sha256(p) for p in sources}
        self.save("fixture-source-hashes.json", source_hashes)
        with tarfile.open(self.work / "fixture-source.tar.gz", "w:gz") as archive:
            for path in sources:
                if path.is_symlink():
                    raise RuntimeError(f"Fixture source must not be a symlink: {path}")
                archive.add(path, arcname=str(path.relative_to(ROOT)))
        self.save("versions.json", {
            "release": RELEASE, "releaseSource": RELEASE_COMMIT,
            "fixtureSource": self.run(["git", "rev-parse", "HEAD"], cwd=ROOT).stdout.strip(),
            "fixtureDirty": bool(self.run(["git", "status", "--porcelain"], cwd=ROOT).stdout),
            "fixtureSourceArchiveSHA256": sha256(self.work / "fixture-source.tar.gz"),
            "fixtureSourceManifestSHA256": sha256(self.work / "fixture-source-hashes.json"),
            "cliSHA256": ASSETS[host], "chartSHA256": CHART_SHA, "components": COMPONENTS,
            "kind": "0.30.0", "kindImage": KIND_IMAGE, "registryImage": REGISTRY_IMAGE,
            "jdkImage": JDK_IMAGE, "hostJava": self.run(["java", "-version"]).stderr,
        })

    def provision(self):
        images = dict(COMPONENTS)
        if self.candidate == "shim":
            images["provisioner"] = self.build_candidate_shim()
        values = {"defaultProfile": {"enabled": False},
                  "images": images,
                  "operator": {"leaderElect": False},
                  "admission": {"failurePolicy": "Ignore", "nodeProfileFailurePolicy": "Fail"}}
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
        self.record("runtime-provisioned", {"mode": self.candidate,
                    "status": self.get("nodeprofile", "live")["status"]})

    def build_candidate_shim(self):
        build = self.private / "candidate"
        build.mkdir()
        env = dict(self.env, CGO_ENABLED="0", GOOS="linux", GOARCH=self.arch)
        result = self.run(["go", "build", "-trimpath", "-o",
                           build / "containerd-shim-brewlet-v2",
                           "./shim/cmd/containerd-shim-brewlet-v2"],
                          cwd=ROOT / "core", env=env, timeout=600)
        self.save("candidate-build.log", result.stdout + result.stderr)
        (build / "Dockerfile").write_text(
            f"FROM {COMPONENTS['provisioner']}\n"
            "COPY --chmod=0755 containerd-shim-brewlet-v2 "
            "/opt/brewlet-dist/containerd-shim-brewlet-v2\n")
        tag = f"brewlet.local/{self.name}-provisioner:candidate"
        result = self.run(["docker", "build", "--platform", f"linux/{self.arch}",
                           "--label", f"{OWNER_LABEL}={self.name}", "-t", tag, build],
                          timeout=600)
        self.save("candidate-image.log", result.stdout + result.stderr)
        image = json.loads(self.run(["docker", "image", "inspect", tag]).stdout)[0]
        self.candidate_image_id = image["Id"]
        self.load_image(tag)
        rows = self.run(["docker", "exec", self.node_id, "ctr", "-n", "k8s.io",
                         "images", "ls"]).stdout.splitlines()
        digest = next((row.split()[2] for row in rows if row.split()[0] == tag), "")
        if not re.fullmatch(r"sha256:[a-f0-9]{64}", digest):
            raise RuntimeError("Could not pin the imported candidate provisioner manifest")
        pinned = tag.split(":")[0] + "@" + digest
        self.run(["docker", "exec", self.node_id, "ctr", "-n", "k8s.io",
                  "images", "tag", tag, pinned])
        versions = json.loads((self.work / "versions.json").read_text())
        versions["candidate"] = {
            "component": "shim", "baseProvisioner": COMPONENTS["provisioner"],
            "image": pinned, "dockerImageID": self.candidate_image_id,
            "binarySHA256": sha256(build / "containerd-shim-brewlet-v2"),
            "goVersion": self.run(["go", "version"], env=env).stdout.strip(),
            "sourceManifestSHA256": versions["fixtureSourceManifestSHA256"],
        }
        self.save("versions.json", versions)
        return pinned

    def load_image(self, ref):
        self.own_container(self.node)
        return self.run(["kind", "load", "docker-image", "--name", self.name, ref],
                        timeout=300)

    def publish_demo(self):
        app = self.private / "demo-app"
        shutil.copytree(ROOT / "integration-tests/fixtures/demo-app", app,
                        ignore=shutil.ignore_patterns("target"))
        result = self.run([*self.maven_args, "-f", app / "pom.xml", "package",
                           "sh.brewlet:brewlet-maven-plugin:0.5.0:push",
                           f"-Dbrewlet.image={self.registry}/apps/demo:live",
                           "-Dbrewlet.jdk=21"], timeout=360)
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
        for name, identifier in ((self.node, self.node_id),
                                 (getattr(self, "registry_name", ""), self.registry_id)):
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
        if self.candidate_image_id:
            try:
                image = json.loads(self.run(
                    ["docker", "image", "inspect", self.candidate_image_id]).stdout)[0]
                if image["Config"].get("Labels", {}).get(OWNER_LABEL) != self.name:
                    raise RuntimeError("Refusing to remove foreign candidate image")
                self.run(["docker", "image", "rm", self.candidate_image_id])
            except (RuntimeError, subprocess.TimeoutExpired, OSError) as error:
                errors.append(f"candidate cleanup: {error}")
        for sig, handler in self.old_signals.items():
            signal.signal(sig, handler)
        try:
            shutil.rmtree(self.private)
        except OSError as error:
            errors.append(f"private material cleanup: {error}")
        try:
            self.save("result.json", {"passed": passed and not errors, "errors": errors,
                                     "scenario": self.scenario, "assertions": len(self.evidence)})
        except OSError as error:
            errors.append(f"result persistence: {error}")
        if errors:
            raise RuntimeError("; ".join(errors))
