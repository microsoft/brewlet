#!/usr/bin/env python3
"""Replicate scoped Brewlet workflows using native Kubernetes image volumes only."""

import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tarfile
import time
import urllib.error
import urllib.request
import uuid


NODE = "kindest/node@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed"
REGISTRY = "registry@sha256:ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8"
RUNTIMES = (
    ("3.13.10", "python@sha256:05b118ecc93ea09e30569706568fb251c71b77d2a3908d338b77be033e162b59"),
    ("3.13.11", "python@sha256:2b9c9803c6a287cafa0a8c917211dddd23dcd2016f049690ee5219f5d3f1636e"),
)
APP = '''import hashlib
import json
import os
import platform
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
import greeting


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({
            "python": platform.python_version(),
            "dependency": greeting.MESSAGE,
            "appSHA256": hashlib.sha256(open(__file__, "rb").read()).hexdigest(),
            "readOnly": {p: bool(os.statvfs(p).f_flag & os.ST_RDONLY)
                         for p in ("/", "/app", "/dependencies")},
            "volumeConfigIgnored": "VOLUME_CONFIG_SHOULD_NOT_APPEAR" not in os.environ,
            "uid": os.getuid(),
        }).encode()
        with open("/tmp/write-probe", "w") as probe:
            probe.write("writable")
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
        if self.path == "/exit":
            threading.Timer(0.5, lambda: os._exit(0)).start()


HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
'''
BASES = {
    "cc": "gcr.io/distroless/cc-debian13@sha256:cb87acc574abe719b6e5c547384defa2f7b1f24af9605d3f618cf852495d8be2",
    "static": "gcr.io/distroless/static-debian13@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931",
}
LANGUAGES = {
    "python-3.13": {
        "mount": "/usr/local", "subPath": "usr/local", "versions": RUNTIMES,
        "command": ["/usr/local/bin/python3", "/app/main.py"],
        "env": [{"name": "PYTHONDONTWRITEBYTECODE", "value": "1"}]},
    "java-21": {
        "mount": "/opt/java/openjdk", "subPath": "opt/java/openjdk",
        "versions": (
            ("21.0.7", "eclipse-temurin@sha256:9eb51387a4badf1a874e655d130538b89dfe16a3dab24fb9a4a6e2b08778411c"),
            ("21.0.8", "eclipse-temurin@sha256:20e7f7288e1c18eebe8f06a442c9f7183342d9b022d3b9a9677cae2b558ddddd")),
        "command": ["/opt/java/openjdk/bin/java", "-Xmx64m", "-jar", "/app/app.jar"], "env": []},
    "node-22": {
        "mount": "/usr/local", "subPath": "usr/local",
        "versions": (
            ("22.17.0", "node@sha256:b04ce4ae4e95b522112c2e5c52f781471a5cbc3b594527bcddedee9bc48c03a0"),),
        "command": ["/usr/local/bin/node", "/app/main.js"], "env": []},
}
MUSL_JAVA = "eclipse-temurin@sha256:7b5c88eb4182a92aab3a4b10550061a6e18639bf176e7ebca21f866b19f853c1"
JDK = "eclipse-temurin@sha256:03b731a231066bd04ada0a7ee9003cdbc1460083344f9277245b233e5ffcbf47"
PAUSE = "registry.k8s.io/pause:3.10"
PYTHON_APP = '''import os
import platform
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/mountinfo":
            body = open("/proc/self/mountinfo", "rb").read()
        else:
            body = ('{"runtime": "python", "version": "%s"}' % platform.python_version()).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
        if self.path == "/exit":
            threading.Timer(0.5, lambda: os._exit(0)).start()


HTTPServer(("0.0.0.0", int(os.environ.get("PORT", "8080"))), Handler).serve_forever()
'''
JAVA_APP = '''import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;

public class Main {
    public static void main(String[] args) throws Exception {
        HttpServer server = HttpServer.create(
            new InetSocketAddress(Integer.parseInt(System.getenv().getOrDefault("PORT", "8080"))), 0);
        server.createContext("/", exchange -> {
            String path = exchange.getRequestURI().getPath();
            byte[] body = path.equals("/mountinfo")
                ? Files.readAllBytes(Path.of("/proc/self/mountinfo"))
                : ("{\\"runtime\\": \\"java\\", \\"version\\": \\"" + System.getProperty("java.version")
                    + "\\"}").getBytes(StandardCharsets.UTF_8);
            exchange.sendResponseHeaders(200, body.length);
            exchange.getResponseBody().write(body);
            exchange.close();
            if (path.equals("/exit")) {
                new Thread(() -> {
                    try { Thread.sleep(500); } catch (InterruptedException ignored) { }
                    Runtime.getRuntime().halt(0);
                }).start();
            }
        });
        server.start();
    }
}
'''
JAVA_BUILD = f'''FROM {JDK} AS build
COPY Main.java /src/
RUN javac --release 21 -d /classes /src/Main.java \\
 && jar --create --file /app.jar --main-class Main -C /classes .
FROM scratch
COPY --from=build /app.jar /app.jar
'''
NODE_APP = '''const fs = require("fs");
const http = require("http");
http.createServer((request, response) => {
  const body = request.url === "/mountinfo"
    ? fs.readFileSync("/proc/self/mountinfo")
    : JSON.stringify({runtime: "node", version: process.versions.node});
  response.end(body);
  if (request.url === "/exit") setTimeout(() => process.exit(0), 500);
}).listen(Number(process.env.PORT || 8080));
'''
APPS = {
    "python-3.13": {"files": {"main.py": PYTHON_APP}},
    "java-21": {"files": {"Main.java": JAVA_APP}, "dockerfile": JAVA_BUILD},
    "node-22": {"files": {"main.js": NODE_APP}},
}
POD_SECURITY = {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532,
                "seccompProfile": {"type": "RuntimeDefault"}}
CONTAINER_SECURITY = {"allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}}
RUNTIME_LABEL = "runtime.example.com/id"
BLOCKED = ("ErrImagePull", "ImagePullBackOff")


def image_volume(name, reference, policy="IfNotPresent"):
    volume = {"name": name, "image": {"pullPolicy": policy}}
    if reference:
        volume["image"]["reference"] = reference
    return volume


def runtime_reference(pod):
    return next(volume["image"].get("reference") for volume in pod["spec"]["volumes"]
                if volume["name"] == "runtime")


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


class Experiment:
    def __init__(self, work):
        self.work = work
        self.name = "image-volumes-" + uuid.uuid4().hex[:10]
        self.registry = self.name + "-registry"
        self.node = self.name + "-control-plane"
        self.worker = self.name + "-worker"
        self.nodes = (self.node, self.worker)
        self.kubeconfig = work / "kubeconfig"
        self.env = dict(os.environ)
        self.env["DOCKER_DEFAULT_PLATFORM"] = self.run(
            "docker", "info", "--format", "{{.Architecture}}").strip()
        arch = {"aarch64": "arm64", "x86_64": "amd64"}.get(
            self.env["DOCKER_DEFAULT_PLATFORM"], self.env["DOCKER_DEFAULT_PLATFORM"])
        require(arch in ("arm64", "amd64"), "requires a native arm64 or amd64 Docker engine")
        self.env["DOCKER_DEFAULT_PLATFORM"] = "linux/" + arch
        self.report = {"scope": "Brewlet use cases replicated with native Kubernetes image volumes",
                       "checks": [], "platform": self.env["DOCKER_DEFAULT_PLATFORM"], "inputs": {
                           "node": NODE, "registry": REGISTRY, "bases": BASES,
                           "languages": {key: value["versions"] for key, value in LANGUAGES.items()},
                           "muslJava": MUSL_JAVA, "jdk": JDK, "pause": PAUSE},
                       "cluster": self.name}
        self.images = []
        self.cluster_attempted = False
        self.registry_created = False

    def run(self, *args, data=None, check=True, timeout=600):
        result = subprocess.run(list(map(str, args)), input=data, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                env=getattr(self, "env", None), timeout=timeout)
        if check and result.returncode:
            raise RuntimeError(f"{' '.join(map(str, args))}\n{result.stdout}\n{result.stderr}")
        return result.stdout if check else result

    def kubectl(self, *args, **kwargs):
        return self.run("kubectl", "--kubeconfig", self.kubeconfig,
                        "--context", "kind-" + self.name, *args, **kwargs)

    def apply(self, obj):
        return self.kubectl("apply", "-f", "-", data=json.dumps(obj))

    def check(self, name):
        self.report["checks"].append(name)
        print("PASS:", name, flush=True)

    def setup(self):
        config = {"kind": "Cluster", "apiVersion": "kind.x-k8s.io/v1alpha4",
                  "nodes": [{"role": "control-plane"}, {"role": "worker"}]}
        config_path = self.work / "kind.json"
        config_path.write_text(json.dumps(config, indent=2) + "\n")
        self.cluster_attempted = True
        self.run("kind", "create", "cluster", "--name", self.name, "--image", NODE,
                 "--kubeconfig", self.kubeconfig, "--config", config_path, "--wait", "180s")
        self.run("docker", "run", "--detach", "--name", self.registry,
                 "--network", "kind", "--label", "brewlet.image-volumes=" + self.name,
                 "--env", "REGISTRY_STORAGE_DELETE_ENABLED=true",
                 "--publish", "127.0.0.1::5000", REGISTRY)
        self.registry_created = True
        info = json.loads(self.run("docker", "inspect", self.registry))[0]
        port = info["NetworkSettings"]["Ports"]["5000/tcp"][0]["HostPort"]
        self.authority = "127.0.0.1:" + port
        for attempt in range(60):
            try:
                with urllib.request.urlopen(f"http://{self.authority}/v2/", timeout=2):
                    break
            except (OSError, TimeoutError):
                if attempt == 59:
                    raise
                time.sleep(1)
        host_dir = "/etc/containerd/certs.d/" + self.authority
        for node in self.nodes:
            self.run("docker", "exec", node, "mkdir", "-p", host_dir)
            self.run("docker", "exec", "-i", node, "cp", "/dev/stdin", host_dir + "/hosts.toml",
                     data=f'[host."http://{self.registry}:5000"]\n  capabilities = ["pull", "resolve"]\n')
        self.kubectl("taint", "nodes", self.node, "node-role.kubernetes.io/control-plane:NoSchedule-")
        gates = {}
        for line in self.kubectl("get", "--raw", f"/api/v1/nodes/{self.node}/proxy/metrics").splitlines():
            for gate in ("ImageVolume", "ImageVolumeWithDigest", "KubeletEnsureSecretPulledImages"):
                if line.startswith(f'kubernetes_feature_enabled{{name="{gate}"'):
                    gates[gate] = line
        self.report["versions"] = {
            "kubernetes": json.loads(self.kubectl("version", "-o", "json"))["serverVersion"],
            "containerd": self.run("docker", "exec", self.node, "containerd", "--version").strip(),
            "kubeletFeatureGates": gates,
        }
        restricted = {"pod-security.kubernetes.io/enforce": "restricted"}
        self.apply({"apiVersion": "v1", "kind": "List", "items": [
            {"apiVersion": "v1", "kind": "Namespace", "metadata": {
                "name": "experiment", "labels": {"image-volumes-experiment": self.name, **restricted}}},
            {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "platform", "labels": restricted}},
            {"apiVersion": "v1", "kind": "Namespace", "metadata": {
                "name": "catalog", "labels": {"runtime-catalog": "enabled", **restricted}}},
            {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "native", "labels": restricted}}]})

    def publish(self, name, source=None, files=None, dockerfile=None):
        tag = self.authority + "/experiment/" + name + ":test"
        if source:
            self.run("docker", "pull", "--quiet", source)
            self.run("docker", "tag", source, tag)
        else:
            context = self.work / name
            context.mkdir()
            for path, content in files.items():
                (context / path).write_text(content)
            (context / "Dockerfile").write_text(dockerfile or (
                "FROM scratch\nCOPY " + " ".join(files) + " /\n"
                "ENV VOLUME_CONFIG_SHOULD_NOT_APPEAR=1\n"
                'ENTRYPOINT ["/deliberately-not-an-executable"]\n'))
            self.run("docker", "build", "--provenance=false", "-q", "-t", tag, context)
        self.images.append(tag)
        archive = self.work / (name + ".tar")
        self.run("docker", "image", "save", "--output", archive, tag)
        base = f"http://{self.authority}/v2/experiment/{name}"

        def upload(content, media_type):
            digest = "sha256:" + hashlib.sha256(content).hexdigest()
            with urllib.request.urlopen(urllib.request.Request(
                    base + "/blobs/uploads/", data=b"", method="POST"), timeout=30) as response:
                location = response.headers["Location"]
            separator = "&" if "?" in location else "?"
            with urllib.request.urlopen(urllib.request.Request(
                    location + separator + "digest=" + digest, data=content, method="PUT",
                    headers={"Content-Type": "application/octet-stream"}), timeout=120):
                pass
            return {"mediaType": media_type, "digest": digest, "size": len(content)}

        with tarfile.open(archive) as saved:
            manifests = json.load(saved.extractfile("manifest.json"))
            require(len(manifests) == 1, "expected a single-platform saved image")
            manifest = manifests[0]
            config = upload(saved.extractfile(manifest["Config"]).read(),
                            "application/vnd.oci.image.config.v1+json")
            layers = []
            for path in manifest["Layers"]:
                content = saved.extractfile(path).read()
                media_type = "application/vnd.oci.image.layer.v1.tar"
                if content.startswith(b"\x1f\x8b"):
                    media_type += "+gzip"
                layers.append(upload(content, media_type))
        content = json.dumps({"schemaVersion": 2,
                              "mediaType": "application/vnd.oci.image.manifest.v1+json",
                              "config": config, "layers": layers}).encode()
        with urllib.request.urlopen(urllib.request.Request(
                base + "/manifests/test", data=content, method="PUT",
                headers={"Content-Type": "application/vnd.oci.image.manifest.v1+json"}),
                timeout=30):
            pass
        archive.unlink()
        request = urllib.request.Request(
            base + "/manifests/test",
            headers={"Accept": "application/vnd.oci.image.index.v1+json, "
                     "application/vnd.oci.image.manifest.v1+json, "
                     "application/vnd.docker.distribution.manifest.v2+json"})
        with urllib.request.urlopen(request, timeout=10) as response:
            digest = "sha256:" + hashlib.sha256(response.read()).hexdigest()
            require(response.headers["Docker-Content-Digest"] == digest,
                    "registry returned inconsistent image identity")
        return tag.rsplit(":", 1)[0] + "@" + digest

    def template(self, runtime, app, dependency):
        return {"metadata": {"labels": {"app": "image-volumes"}}, "spec": {
            "securityContext": {"runAsNonRoot": True, "runAsUser": 65532,
                                "runAsGroup": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
            "containers": [{
                "name": "app", "image": runtime, "imagePullPolicy": "IfNotPresent",
                "command": ["/usr/local/bin/python3", "/app/main.py"], "workingDir": "/app",
                "env": [{"name": "PYTHONPATH", "value": "/dependencies"},
                        {"name": "PYTHONDONTWRITEBYTECODE", "value": "1"}],
                "securityContext": {"readOnlyRootFilesystem": True,
                                    "allowPrivilegeEscalation": False,
                                    "capabilities": {"drop": ["ALL"]}},
                "resources": {"requests": {"cpu": "10m", "memory": "32Mi"},
                              "limits": {"cpu": "500m", "memory": "96Mi"}},
                "readinessProbe": {"httpGet": {"path": "/", "port": 8080}, "periodSeconds": 1},
                "volumeMounts": [
                    {"name": "application", "mountPath": "/app", "readOnly": True},
                    {"name": "dependencies", "mountPath": "/dependencies", "readOnly": True},
                    {"name": "tmp", "mountPath": "/tmp"}],
            }],
            "volumes": [
                {"name": "application", "image": {"reference": app, "pullPolicy": "IfNotPresent"}},
                {"name": "dependencies", "image": {"reference": dependency,
                                                 "pullPolicy": "IfNotPresent"}},
                {"name": "tmp", "emptyDir": {"sizeLimit": "8Mi"}}]}}

    def policy(self, runtimes, app, dependencies):
        expression = (
            "object.spec.containers.all(c, c.image in " + json.dumps(runtimes) + ") && "
            "object.spec.volumes.filter(v, has(v.image)).all(v, v.image.reference in " +
            json.dumps([app, *dependencies]) + ")")
        self.apply({"apiVersion": "admissionregistration.k8s.io/v1",
                    "kind": "ValidatingAdmissionPolicy", "metadata": {"name": self.name},
                    "spec": {"failurePolicy": "Fail", "matchConstraints": {
                        "resourceRules": [{"apiGroups": [""], "apiVersions": ["v1"],
                                           "operations": ["CREATE"], "resources": ["pods"]}]},
                        "validations": [{"expression": expression,
                                         "message": "image-volume experiment: unapproved digest"}]}})
        self.apply({"apiVersion": "admissionregistration.k8s.io/v1",
                    "kind": "ValidatingAdmissionPolicyBinding",
                    "metadata": {"name": self.name}, "spec": {
                        "policyName": self.name, "validationActions": ["Deny"],
                        "matchResources": {"namespaceSelector": {"matchLabels": {
                            "image-volumes-experiment": self.name}}}}})

    def dry_run(self, pod, allowed):
        deadline = time.monotonic() + 30
        while True:
            result = self.kubectl("create", "--dry-run=server", "-f", "-",
                                  data=json.dumps(pod), check=False)
            denied = "image-volume experiment: unapproved digest" in result.stderr
            if (allowed and result.returncode == 0) or (not allowed and denied):
                return result.stdout + result.stderr
            if result.returncode and not denied:
                raise RuntimeError(result.stderr)
            require(time.monotonic() < deadline, "admission policy did not converge")
            time.sleep(1)

    def pod(self, name, template):
        return {"apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": name, "namespace": "experiment"},
                "spec": copy.deepcopy(template["spec"])}

    def observe(self, name):
        response = self.kubectl(
            "-n", "experiment", "exec", name, "--", "/usr/local/bin/python3", "-c",
            "import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:8080/').read().decode())")
        return json.loads(response)

    def snapshot(self, phase, version, dependency):
        self.kubectl("-n", "experiment", "rollout", "status", "deployment/app", "--timeout=180s")
        pods = json.loads(self.kubectl("-n", "experiment", "get", "pods", "-l",
                                      "app=image-volumes", "-o", "json"))["items"]
        pods = [pod for pod in pods if "deletionTimestamp" not in pod["metadata"]]
        require(len(pods) == 2, "expected exactly two live Deployment replicas")
        observations = []
        for pod in pods:
            spec = pod["spec"]
            require(spec["containers"][0]["image"] == self.deployment["spec"]["template"][
                "spec"]["containers"][0]["image"], "Pod runtime differs from desired runtime")
            require(spec["volumes"][:2] == self.deployment["spec"]["template"]["spec"]["volumes"][:2],
                    "Pod image-volume references differ from desired references")
            observed = self.observe(pod["metadata"]["name"])
            require(observed["python"] == version, "running interpreter version did not change")
            require(observed["dependency"] == dependency, "running dependency did not change")
            require(observed["appSHA256"] == hashlib.sha256(APP.encode()).hexdigest(),
                    "application bytes changed")
            require(all(observed["readOnly"].values()), "root or image volume is writable")
            require(observed["uid"] == 65532, "application did not run as non-root")
            require(observed["volumeConfigIgnored"], "volume image config leaked into process")
            observations.append({"pod": pod, "response": observed})
        self.report.setdefault("observations", {})[phase] = observations
        self.check(phase)

    def execute(self):
        self.setup()
        before, after = [self.publish("runtime-" + version, source=image)
                         for version, image in RUNTIMES]
        app = self.publish("app", files={"main.py": APP})
        dep1 = self.publish("dependency-before", files={"greeting.py": "MESSAGE = 'original'\n"})
        dep2 = self.publish("dependency-after", files={"greeting.py": "MESSAGE = 'patched'\n"})
        self.report["images"] = {"runtimeBefore": before, "runtimeAfter": after,
                                "application": app, "dependencyBefore": dep1, "dependencyAfter": dep2}
        refs = {"base": self.publish("base-cc", source=BASES["cc"]),
                "static": self.publish("base-static", source=BASES["static"]),
                "musl": self.publish("runtime-java-musl", source=MUSL_JAVA),
                "runtimes": {"python-3.13": [before, after]}, "apps": {}}
        for runtime_id, language in LANGUAGES.items():
            if runtime_id != "python-3.13":
                refs["runtimes"][runtime_id] = [
                    self.publish(f"runtime-{runtime_id.split('-')[0]}-{version}", source=image)
                    for version, image in language["versions"]]
            refs["apps"][runtime_id] = self.publish("app-" + runtime_id, **APPS[runtime_id])
        self.report["images"].update(refs)
        self.runtime_image_layout(before, after, app, dep1, dep2)
        self.catalog(refs)
        self.mixed_runtimes(refs)
        self.abi(refs)
        self.restart_authorization(refs)

    def runtime_image_layout(self, before, after, app, dep1, dep2):
        template = self.template(before, app, dep1)
        self.policy([before, after], app, [dep1, dep2])
        held = self.pod("held", template)
        invalid = copy.deepcopy(held)
        invalid["metadata"]["name"] = "denied"
        invalid["spec"]["containers"][0]["image"] = "python:latest"
        self.report["runtimeDenial"] = self.dry_run(invalid, allowed=False)
        invalid = copy.deepcopy(held)
        invalid["spec"]["volumes"][1]["image"]["reference"] = "python:latest"
        self.report["dependencyDenial"] = self.dry_run(invalid, allowed=False)
        self.dry_run(held, allowed=True)
        self.check("native admission rejects unapproved runtime and dependency references")
        self.deployment = {"apiVersion": "apps/v1", "kind": "Deployment",
                           "metadata": {"name": "app", "namespace": "experiment"}, "spec": {
                               "replicas": 2, "selector": {"matchLabels": {"app": "image-volumes"}},
                               "strategy": {"type": "RollingUpdate", "rollingUpdate": {
                                   "maxUnavailable": 0, "maxSurge": 1}}, "template": template}}
        self.apply(self.deployment)
        self.apply(held)
        self.kubectl("-n", "experiment", "wait", "pod/held", "--for=condition=Ready", "--timeout=180s")
        self.snapshot("separate application and dependency image volumes", "3.13.10", "original")
        template["spec"]["containers"][0]["image"] = after
        self.apply(self.deployment)
        self.snapshot("runtime rotation without rebuilding application or dependency", "3.13.11", "original")
        require(self.observe("held")["python"] == "3.13.10", "running Pod was unexpectedly hot-patched")
        self.check("existing Pod keeps its old runtime after Deployment rotation")
        template["spec"]["volumes"][1]["image"]["reference"] = dep2
        self.apply(self.deployment)
        self.snapshot("dependency-only remediation without rebuilding application", "3.13.11", "patched")
        template["spec"]["volumes"][1]["image"]["reference"] = dep1
        template["spec"]["containers"][0]["image"] = before
        self.apply(self.deployment)
        self.snapshot("runtime and dependency rollback with unchanged application", "3.13.10", "original")
        self.policy([after], app, [dep1, dep2])
        self.report["revokedPodDenial"] = self.dry_run(self.pod("revoked", self.template(
            before, app, dep1)), allowed=False)
        self.check("native admission rejects new Pods selecting a revoked runtime digest")
        old = json.loads(self.kubectl("-n", "experiment", "get", "pod/held", "-o", "json"))
        self.kubectl("-n", "experiment", "exec", "held", "--", "/usr/local/bin/python3", "-c",
                     "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/exit').read()")
        deadline = time.monotonic() + 90
        while True:
            current = json.loads(self.kubectl("-n", "experiment", "get", "pod/held", "-o", "json"))
            statuses = current["status"].get("containerStatuses", [])
            if statuses and statuses[0]["restartCount"] > old["status"]["containerStatuses"][0]["restartCount"]:
                if statuses[0].get("ready"):
                    break
            require(time.monotonic() < deadline, "held container did not restart")
            time.sleep(1)
        require(current["metadata"]["uid"] == old["metadata"]["uid"], "restart replaced the Pod")
        require(self.observe("held")["python"] == "3.13.10", "restart did not use the revoked runtime")
        self.report["restartAfterRevocation"] = {"before": old, "after": current,
                                                 "response": self.observe("held")}
        self.check("gap reproduced: existing Pod restarts revoked runtime without Pod admission")
        template["spec"]["containers"][0]["image"] = after
        self.apply(self.deployment)
        self.snapshot("recovery onto approved runtime", "3.13.11", "original")

    def get(self, namespace, name):
        return json.loads(self.kubectl("-n", namespace, "get", name, "-o", "json"))

    def wait(self, description, probe, timeout=180, interval=2):
        deadline = time.monotonic() + timeout
        while True:
            value = probe()
            if value:
                return value
            require(time.monotonic() < deadline, "timed out: " + description)
            time.sleep(interval)

    def live_pods(self, namespace, selector):
        items = json.loads(self.kubectl("-n", namespace, "get", "pods", "-l", selector,
                                        "-o", "json"))["items"]
        return sorted((pod for pod in items if "deletionTimestamp" not in pod["metadata"]),
                      key=lambda pod: pod["metadata"]["creationTimestamp"])

    def proxy(self, namespace, pod, path="", port=8080):
        return self.kubectl("get", "--raw",
                            f"/api/v1/namespaces/{namespace}/pods/{pod}:{port}/proxy/{path}")

    def inspect_app(self, namespace, pod, port=8080):
        response = json.loads(self.proxy(namespace, pod, port=port))
        mounts = {}
        for line in self.proxy(namespace, pod, "mountinfo", port).splitlines():
            fields = line.split()
            mounts[fields[4]] = "ro" in fields[5].split(",")
        response["readOnly"] = mounts
        return response

    def node_images(self, node):
        images = json.loads(self.run("docker", "exec", node, "crictl", "images", "-o", "json"))
        return {digest for image in images["images"] for digest in image.get("repoDigests") or []}

    def events(self, namespace, pod):
        return [{"reason": event["reason"], "message": event["message"],
                 "count": event.get("count"), "last": event.get("lastTimestamp")}
                for event in json.loads(self.kubectl(
                    "-n", namespace, "get", "events", "--field-selector",
                    "involvedObject.name=" + pod, "-o", "json"))["items"]]

    def manifest(self, reference, method="GET", data=None):
        repository, digest = reference.split("/", 1)[1].split("@")
        request = urllib.request.Request(
            f"http://{self.authority}/v2/{repository}/manifests/{digest}", data=data,
            method=method, headers={"Accept": "application/vnd.oci.image.manifest.v1+json",
                                    "Content-Type": "application/vnd.oci.image.manifest.v1+json"})
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()

    def app_container(self, name, runtime_id, image, runtime_volume="runtime",
                      app_volume="application", port=8080):
        language = LANGUAGES[runtime_id]
        return {"name": name, "image": image, "imagePullPolicy": "IfNotPresent",
                "command": language["command"],
                "env": [*language["env"], {"name": "PORT", "value": str(port)}],
                "securityContext": {**CONTAINER_SECURITY, "readOnlyRootFilesystem": False},
                "resources": {"requests": {"cpu": "10m", "memory": "32Mi"},
                              "limits": {"cpu": "500m", "memory": "256Mi"}},
                "readinessProbe": {"httpGet": {"path": "/", "port": port}, "periodSeconds": 1},
                "volumeMounts": [
                    {"name": runtime_volume, "mountPath": language["mount"],
                     "subPath": language["subPath"]},
                    {"name": app_volume, "mountPath": "/app"}]}

    def native_pod(self, name, containers, volumes, restart="Always"):
        return {"apiVersion": "v1", "kind": "Pod",
                "metadata": {"name": name, "namespace": "native"},
                "spec": {"restartPolicy": restart, "securityContext": POD_SECURITY,
                         "containers": containers, "volumes": volumes}}

    def catalog_pod(self, runtime_id, app, name=None):
        template = {"metadata": {"labels": {"app": runtime_id, RUNTIME_LABEL: runtime_id}},
                    "spec": {"securityContext": POD_SECURITY,
                             "containers": [self.app_container("app", runtime_id, "platform-base")],
                             "volumes": [image_volume("runtime", None),
                                         image_volume("application", app)]}}
        if name:
            return {"apiVersion": "v1", "kind": "Pod", "metadata": {
                "name": name, "namespace": "catalog", **template["metadata"]},
                "spec": template["spec"]}
        return template

    def set_catalog(self, catalog):
        self.apply({"apiVersion": "v1", "kind": "ConfigMap",
                    "metadata": {"name": "runtime-catalog", "namespace": "platform"},
                    "data": catalog})
        require(self.get("platform", "configmap/runtime-catalog")["data"] == catalog,
                "runtime catalog update did not apply")
        self.report.setdefault("catalogHistory", []).append(dict(catalog))

    def admission(self, pod, accept, description):
        def attempt():
            result = self.kubectl("create", "--dry-run=server", "-o", "json", "-f", "-",
                                  data=json.dumps(pod), check=False)
            return result if accept(result) else None
        return self.wait(description, attempt, timeout=60)

    def admitted_runtime(self, runtime_id, app, reference):
        def accept(result):
            return not result.returncode and runtime_reference(
                json.loads(result.stdout)) == reference
        result = self.admission(self.catalog_pod(runtime_id, app, "admission-probe"), accept,
                                f"catalog admission did not select {reference}")
        return json.loads(result.stdout)

    def denied(self, pod, message):
        result = self.admission(pod, lambda result: result.returncode and message in result.stderr,
                                "admission did not deny with " + message)
        return result.stderr.strip()

    def catalog_policies(self):
        has_id = f"has(object.metadata.labels) && '{RUNTIME_LABEL}' in object.metadata.labels"
        selected = f"object.metadata.labels['{RUNTIME_LABEL}']"
        api = "admissionregistration.k8s.io/v1"
        rules = {"resourceRules": [{"apiGroups": [""], "apiVersions": ["v1"],
                                    "operations": ["CREATE"], "resources": ["pods"]}]}
        param = {"name": "runtime-catalog", "namespace": "platform",
                 "parameterNotFoundAction": "Deny"}
        scope = {"namespaceSelector": {"matchLabels": {"runtime-catalog": "enabled"}}}
        mutation = (
            "Object{spec: Object.spec{containers: [Object.spec.containers{name: 'app', "
            "image: params.data['base']}], volumes: [Object.spec.volumes{name: 'runtime', "
            f"image: Object.spec.volumes.image{{reference: {selected} in params.data ? "
            f"params.data[{selected}] : 'unknown-runtime-id'}}}}]}}}}")
        selector = (
            "(has(object.spec.nodeSelector) ? [] : [JSONPatch{op: 'add', "
            "path: '/spec/nodeSelector', value: {}}]) + [JSONPatch{op: 'add', "
            "path: '/spec/nodeSelector/' + jsonpatch.escapeKey('runtime.example.com/' + "
            f"{selected}), value: 'offered'}}]")
        offered = "variables.id != '' && variables.id != 'base' && variables.id in params.data"
        self.apply({"apiVersion": "v1", "kind": "List", "items": [
            {"apiVersion": api, "kind": "MutatingAdmissionPolicy",
             "metadata": {"name": "runtime-selection"}, "spec": {
                 "failurePolicy": "Fail", "reinvocationPolicy": "Never",
                 "paramKind": {"apiVersion": "v1", "kind": "ConfigMap"},
                 "matchConstraints": rules,
                 "matchConditions": [{"name": "runtime-id", "expression": has_id}],
                 "mutations": [
                     {"patchType": "ApplyConfiguration",
                      "applyConfiguration": {"expression": mutation}},
                     {"patchType": "JSONPatch", "jsonPatch": {"expression": selector}}]}},
            {"apiVersion": api, "kind": "MutatingAdmissionPolicyBinding",
             "metadata": {"name": "runtime-selection"}, "spec": {
                 "policyName": "runtime-selection", "paramRef": param, "matchResources": scope}},
            {"apiVersion": api, "kind": "ValidatingAdmissionPolicy",
             "metadata": {"name": "runtime-catalog-check"}, "spec": {
                 "failurePolicy": "Fail", "paramKind": {"apiVersion": "v1", "kind": "ConfigMap"},
                 "matchConstraints": rules,
                 "variables": [{"name": "id", "expression": f"{has_id} ? {selected} : ''"}],
                 "validations": [
                     {"expression": offered, "messageExpression": (
                         "variables.id == '' ? 'Pods in catalog namespaces must select a runtime ID'"
                         " : 'runtime ID ' + variables.id + ' is not offered by the catalog'")},
                     {"expression": "object.spec.containers.all(c, c.image == params.data['base'])"
                                    " && (!has(object.spec.initContainers) || object.spec."
                                    "initContainers.all(c, c.image == params.data['base']))",
                      "message": "containers must use the platform base image"},
                     {"expression": f"!({offered}) || object.spec.volumes.exists(v, "
                                    "v.name == 'runtime' && has(v.image) && "
                                    "v.image.reference == params.data[variables.id])",
                      "message": "runtime volume does not match the catalog digest"},
                     {"expression": "variables.id == '' || (has(object.spec.nodeSelector) && "
                                    "object.spec.nodeSelector['runtime.example.com/' + "
                                    "variables.id] == 'offered')",
                      "message": "Pod is not restricted to nodes offering its runtime"}]}},
            {"apiVersion": api, "kind": "ValidatingAdmissionPolicyBinding",
             "metadata": {"name": "runtime-catalog-check"}, "spec": {
                 "policyName": "runtime-catalog-check", "validationActions": ["Deny"],
                 "paramRef": param, "matchResources": scope}}]})

    def prefetch(self, runtime_id, reference):
        name = "prefetch-" + runtime_id.replace(".", "-")
        self.apply({"apiVersion": "apps/v1", "kind": "DaemonSet",
                    "metadata": {"name": name, "namespace": "platform"}, "spec": {
                        "selector": {"matchLabels": {"prefetch": runtime_id}},
                        "template": {"metadata": {"labels": {"prefetch": runtime_id}}, "spec": {
                            "nodeSelector": {"runtime.example.com/" + runtime_id: "offered"},
                            "securityContext": POD_SECURITY,
                            "containers": [{
                                "name": "hold", "image": PAUSE, "imagePullPolicy": "IfNotPresent",
                                "securityContext": {**CONTAINER_SECURITY,
                                                    "readOnlyRootFilesystem": True},
                                "resources": {"requests": {"cpu": "1m", "memory": "4Mi"},
                                              "limits": {"memory": "16Mi"}},
                                "volumeMounts": [{"name": "runtime", "mountPath": "/runtime"}]}],
                            "volumes": [image_volume("runtime", reference)]}}}})
        self.kubectl("-n", "platform", "rollout", "status", "daemonset/" + name,
                     "--timeout=180s")

    def inventory(self, catalog):
        rows = []
        for pod in self.live_pods("catalog", RUNTIME_LABEL):
            runtime_id = pod["metadata"]["labels"][RUNTIME_LABEL]
            statuses = pod["status"].get("containerStatuses") or [{}]
            rows.append({"pod": pod["metadata"]["name"], "node": pod["spec"].get("nodeName"),
                         "runtimeID": runtime_id, "runtime": runtime_reference(pod),
                         "current": catalog.get(runtime_id) == runtime_reference(pod),
                         "baseImageID": statuses[0].get("imageID"),
                         "statusMentionsRuntime": runtime_reference(pod).split("@")[1]
                         in json.dumps(pod["status"])})
        return rows

    def catalog_versions(self, deployment, runtime_id, expected, replicas):
        self.kubectl("-n", "catalog", "rollout", "status", "deployment/" + deployment,
                     "--timeout=240s")

        def converged():
            pods = self.live_pods("catalog", "app=" + runtime_id)
            ready = [pod for pod in pods if any(
                condition["type"] == "Ready" and condition["status"] == "True"
                for condition in pod["status"].get("conditions", []))]
            return pods if len(pods) == replicas == len(ready) else None
        pods = self.wait(f"{deployment} did not converge on {replicas} ready Pods", converged)
        versions = [self.inspect_app("catalog", pod["metadata"]["name"])["version"]
                    for pod in pods]
        require(versions == [expected] * replicas,
                f"{deployment} runs {versions}, expected {expected}")
        return pods

    def catalog(self, refs):
        runtimes = refs["runtimes"]
        catalog = {"base": refs["base"], **{key: value[0] for key, value in runtimes.items()}}
        self.set_catalog(catalog)
        self.catalog_policies()
        offers = {self.node: ("python-3.13", "node-22"), self.worker: ("python-3.13", "java-21")}
        for node, runtime_ids in offers.items():
            self.kubectl("label", "node", node, *[
                f"runtime.example.com/{runtime_id}=offered" for runtime_id in runtime_ids])
        self.report["nodeOffers"] = offers
        python_app = refs["apps"]["python-3.13"]
        admitted = self.admitted_runtime("python-3.13", python_app, catalog["python-3.13"])
        forged = self.catalog_pod("python-3.13", python_app, "forged")
        forged["spec"]["containers"][0]["image"] = "python:latest"
        forged["spec"]["volumes"][0]["image"]["reference"] = "python:latest"
        forged = json.loads(self.admission(forged, lambda result: not result.returncode,
                                           "forged Pod was not admitted")
                            .stdout)
        for pod in (admitted, forged):
            require(pod["spec"]["containers"][0]["image"] == catalog["base"],
                    "catalog admission did not inject the base image")
            require(runtime_reference(pod) == catalog["python-3.13"],
                    "catalog admission did not inject the runtime digest")
            require(pod["spec"]["nodeSelector"] == {"runtime.example.com/python-3.13": "offered"},
                    "catalog admission did not restrict the Pod to offering nodes")
        self.report["catalogAdmission"] = {"admitted": admitted["spec"],
                                           "forged": forged["spec"]}
        self.check("catalog admission injects base image, runtime digest and pool selector")
        unknown = self.catalog_pod("python-3.13", python_app, "unknown")
        unknown["metadata"]["labels"][RUNTIME_LABEL] = "ruby-3"
        unlabeled = self.catalog_pod("python-3.13", python_app, "unlabeled")
        del unlabeled["metadata"]["labels"][RUNTIME_LABEL]
        unlabeled["spec"]["containers"][0]["image"] = "python:latest"
        unlabeled["spec"]["volumes"][0]["image"]["reference"] = "python:latest"
        self.report["catalogDenials"] = {
            "unknown": self.denied(unknown, "runtime ID ruby-3 is not offered by the catalog"),
            "unlabeled": self.denied(unlabeled,
                                     "Pods in catalog namespaces must select a runtime ID")}
        self.check("catalog admission denies unknown runtime IDs and unselected runtimes")

        java_old, java_new = runtimes["java-21"]
        node_runtime = runtimes["node-22"][0]
        before = {node: self.node_images(node) for node in self.nodes}
        require(all(java_old not in images and node_runtime not in images
                    for images in before.values()), "runtimes were cached before the experiment")
        self.prefetch("java-21", java_old)
        after = {node: self.node_images(node) for node in self.nodes}
        require(java_old in after[self.worker] and java_old not in after[self.node],
                "prefetch did not follow the java-21 node offer")
        require(all(node_runtime not in images for images in after.values()),
                "OnDemand runtime was pulled before use")
        self.check("Eager: DaemonSet pre-pulls java-21 only on nodes offering it")

        deployments = {"python-3.13": "python", "java-21": "java", "node-22": "node"}
        for runtime_id, deployment in deployments.items():
            self.apply({"apiVersion": "apps/v1", "kind": "Deployment",
                        "metadata": {"name": deployment, "namespace": "catalog"}, "spec": {
                            "replicas": 1, "revisionHistoryLimit": 5,
                            "selector": {"matchLabels": {"app": runtime_id}},
                            "template": self.catalog_pod(runtime_id, refs["apps"][runtime_id])}})
        expected = {"python-3.13": RUNTIMES[0][0],
                    "java-21": LANGUAGES["java-21"]["versions"][0][0],
                    "node-22": LANGUAGES["node-22"]["versions"][0][0]}
        observations = {}
        for runtime_id, deployment in deployments.items():
            pod = self.catalog_versions(deployment, runtime_id, expected[runtime_id], 1)[0]
            observed = self.inspect_app("catalog", pod["metadata"]["name"])
            mount = LANGUAGES[runtime_id]["mount"]
            require(observed["readOnly"]["/"] is False, "base root filesystem was not writable")
            require(observed["readOnly"][mount] and observed["readOnly"]["/app"],
                    "runtime or application volume was writable")
            require(pod["spec"]["nodeName"] in [node for node, ids in offers.items()
                                                if runtime_id in ids],
                    f"{runtime_id} Pod ran on a node that does not offer it")
            observations[runtime_id] = {"pod": pod["metadata"]["name"],
                                        "node": pod["spec"]["nodeName"], "response": observed}
        self.report["catalogObservations"] = observations
        self.check("Python, Java and Node apps run on one distroless/cc base with runtime volumes")
        self.check("runtime and application mounts are read-only while the base root is writable")
        self.check("runtime ID placement keeps each Pod on nodes offering its runtime")

        java_pod = observations["java-21"]["pod"]
        node_pod = observations["node-22"]["pod"]

        def present(pod, reference):
            events = self.events("catalog", pod)
            return events if any(event["reason"] == "Pulled" and reference in event["message"]
                                 and "already present" in event["message"]
                                 for event in events) else None

        def pulled(pod, reference):
            events = self.events("catalog", pod)
            return events if any(event["reason"] == "Pulling" and reference in event["message"]
                                 for event in events) else None
        java_events = self.wait("Java Pod did not report a cached runtime",
                                lambda: present(java_pod, java_old), timeout=60)
        require(not any(event["reason"] == "Pulling" and java_old in event["message"]
                        for event in java_events), "Java Pod pulled a prefetched runtime")
        node_events = self.wait("Node Pod did not pull its runtime",
                                lambda: pulled(node_pod, node_runtime), timeout=60)
        self.report["placementEvents"] = {"java": java_events, "node": node_events}
        self.check("Eager Java Pod starts from the cached runtime; OnDemand Node Pod pulls it")

        old_java = self.get("catalog", "pod/" + java_pod)
        catalog["java-21"] = java_new
        self.set_catalog(catalog)
        self.prefetch("java-21", java_new)
        require(java_new in self.node_images(self.worker),
                "rotated runtime was not pre-pulled before workloads used it")
        self.admitted_runtime("java-21", refs["apps"]["java-21"], java_new)
        current = self.get("catalog", "pod/" + java_pod)
        require(current["metadata"]["uid"] == old_java["metadata"]["uid"] and
                current["status"]["containerStatuses"][0]["restartCount"] == 0 and
                runtime_reference(current) == java_old and
                self.inspect_app("catalog", java_pod)["version"] == expected["java-21"],
                "catalog rotation changed a running Pod")
        self.check("catalog rotation pre-pulls the new runtime and leaves running Pods unchanged")
        self.kubectl("-n", "catalog", "scale", "deployment/java", "--replicas=2")

        def scaled():
            pods = self.live_pods("catalog", "app=java-21")
            ready = [pod for pod in pods if pod["status"].get("containerStatuses") and
                     pod["status"]["containerStatuses"][0].get("ready")]
            return pods if len(ready) == 2 else None
        pods = self.wait("Java scale-up did not become ready", scaled)
        versions = sorted(self.inspect_app("catalog", pod["metadata"]["name"])["version"]
                          for pod in pods)
        hashes = {pod["metadata"]["labels"]["pod-template-hash"] for pod in pods}
        require(versions == sorted(version for version, _ in LANGUAGES["java-21"]["versions"]),
                f"scale-up did not mix runtime versions: {versions}")
        require(len(hashes) == 1, "mixed versions came from different ReplicaSets")
        self.report["mixedReplicaSet"] = {"versions": versions, "podTemplateHash": hashes.pop()}
        self.check("scale-up after rotation mixes runtime versions inside one ReplicaSet")
        inventory = self.inventory(catalog)
        self.report["inventory"] = inventory
        require([row["pod"] for row in inventory if not row["current"]] == [java_pod],
                "inventory did not identify exactly the stale Java Pod")
        self.check("inventory from Pod specs identifies the stale runtime digest")
        require(not any(row["statusMentionsRuntime"] for row in inventory),
                "Pod status unexpectedly reported an image-volume digest")
        self.check("Pod status omits image-volume digests while ImageVolumeWithDigest is off")

        self.kubectl("-n", "catalog", "rollout", "restart", "deployment/java")
        self.catalog_versions("java", "java-21", LANGUAGES["java-21"]["versions"][1][0], 2)
        self.check("rollout restart converges every replica on the rotated runtime")
        require(java_old in self.node_images(self.worker),
                "replaced runtime was removed from the node")
        self.check("replaced runtime stays cached on the node after its last Pod exits")
        history = self.kubectl("-n", "catalog", "rollout", "history", "deployment/java")
        self.kubectl("-n", "catalog", "rollout", "undo", "deployment/java")
        self.catalog_versions("java", "java-21", LANGUAGES["java-21"]["versions"][1][0], 2)
        self.report["rolloutHistory"] = history
        self.check("rollout undo keeps the rotated runtime because templates carry no digest")
        catalog["java-21"] = java_old
        self.set_catalog(catalog)
        self.prefetch("java-21", java_old)
        self.admitted_runtime("java-21", refs["apps"]["java-21"], java_old)
        self.kubectl("-n", "catalog", "rollout", "restart", "deployment/java")
        self.catalog_versions("java", "java-21", expected["java-21"], 2)
        self.check("catalog rollback plus rollout restart restores the previous runtime")

        node_before = self.get("catalog", "pod/" + node_pod)
        del catalog["node-22"]
        self.set_catalog(catalog)
        self.report["catalogDenials"]["removed"] = self.denied(
            self.catalog_pod("node-22", refs["apps"]["node-22"], "removed"),
            "runtime ID node-22 is not offered by the catalog")
        node_after = self.get("catalog", "pod/" + node_pod)
        require(node_after["metadata"]["uid"] == node_before["metadata"]["uid"] and
                node_after["status"]["containerStatuses"][0]["ready"] and
                self.inspect_app("catalog", node_pod)["version"] == expected["node-22"],
                "catalog removal disturbed a running Pod")
        self.check("removing a runtime ID denies new Pods without stopping running Pods")

    def mixed_runtimes(self, refs):
        pod = self.native_pod("mixed", [
            self.app_container("python", "python-3.13", refs["base"], "python-runtime",
                               "python-app", 8080),
            self.app_container("node", "node-22", refs["base"], "node-runtime", "node-app", 8081)],
            [image_volume("python-runtime", refs["runtimes"]["python-3.13"][0]),
             image_volume("python-app", refs["apps"]["python-3.13"]),
             image_volume("node-runtime", refs["runtimes"]["node-22"][0]),
             image_volume("node-app", refs["apps"]["node-22"])])
        self.apply(pod)
        self.kubectl("-n", "native", "wait", "pod/mixed", "--for=condition=Ready",
                     "--timeout=180s")
        observed = {"python": self.inspect_app("native", "mixed", 8080),
                    "node": self.inspect_app("native", "mixed", 8081)}
        require(observed["python"]["version"] == RUNTIMES[0][0] and
                observed["node"]["version"] == LANGUAGES["node-22"]["versions"][0][0],
                "mixed-runtime Pod did not run both runtimes")
        self.report["mixedRuntimePod"] = observed
        self.check("one Pod runs Python and Node containers from different runtime volumes")

    def abi(self, refs):
        cases = {
            "python-on-static": ("python-3.13", refs["static"],
                                 refs["runtimes"]["python-3.13"][0]),
            "musl-java-on-cc": ("java-21", refs["base"], refs["musl"])}
        for name, (runtime_id, base, runtime) in cases.items():
            container = self.app_container("app", runtime_id, base)
            del container["readinessProbe"]
            self.apply(self.native_pod(name, [container], [
                image_volume("runtime", runtime),
                image_volume("application", refs["apps"][runtime_id])], restart="Never"))
        results = {}
        for name, (runtime_id, _, _) in cases.items():
            def terminated():
                statuses = self.get("native", "pod/" + name)["status"].get(
                    "containerStatuses") or [{}]
                return statuses[0].get("state", {}).get("terminated")
            state = self.wait(name + " did not terminate", terminated, timeout=120)
            log = self.kubectl("-n", "native", "logs", name).strip()
            require(state["exitCode"] != 0 and "no such file or directory" in log,
                    name + " did not fail as an ABI mismatch")
            results[name] = {"terminated": state, "log": log}
        self.report["abiMismatches"] = results
        self.check("ABI mismatch: glibc Python runtime cannot start on distroless/static")
        self.check("ABI mismatch: musl Temurin runtime cannot start on glibc distroless/cc")

    def restart_authorization(self, refs):
        revocable = self.publish("revocable-runtime", source=RUNTIMES[1][1])
        app = refs["apps"]["python-3.13"]
        pods = {"always": image_volume("runtime", revocable, "Always"),
                "cached": image_volume("runtime", refs["runtimes"]["python-3.13"][1])}
        for name, runtime in pods.items():
            self.apply(self.native_pod(name, [
                self.app_container("app", "python-3.13", refs["base"])],
                [runtime, image_volume("application", app)]))
        for name in pods:
            self.kubectl("-n", "native", "wait", "pod/" + name, "--for=condition=Ready",
                         "--timeout=180s")
        evidence = {"reference": revocable}

        def status(name):
            pod = self.get("native", "pod/" + name)
            return pod, pod["status"]["containerStatuses"][0]

        def failures(name):
            return sum(event.get("count") or 1 for event in self.events("native", name)
                       if event["reason"] == "Failed" and revocable in event["message"])

        def blocked(name, restarts, uid, failed_before):
            self.wait(name + " did not stop serving",
                      lambda: not status(name)[1].get("ready"), timeout=60)
            time.sleep(40)
            pod, current = status(name)
            reason = current["state"].get("waiting", {}).get("reason")
            failed = failures(name)
            require(pod["metadata"]["uid"] == uid and current["restartCount"] == restarts and
                    not current["ready"], name + " restarted while its runtime was unavailable")
            require(reason in BLOCKED or failed > failed_before,
                    name + " has no runtime pull failure evidence")
            return {"state": current["state"], "restartCount": current["restartCount"],
                    "pullFailures": failed - failed_before}

        def restarted(name, restarts, uid, timeout):
            def probe():
                pod, current = status(name)
                return (pod, current) if current["restartCount"] > restarts and \
                    current.get("ready") else None
            pod, current = self.wait(name + " did not restart in place", probe, timeout=timeout)
            require(pod["metadata"]["uid"] == uid, name + " restart replaced the Pod")
            return current

        uids = {name: status(name)[0]["metadata"]["uid"] for name in pods}
        code, manifest = self.manifest(revocable)
        require(code == 200, "could not read the revocable runtime manifest")
        code, _ = self.manifest(revocable, "DELETE")
        require(code == 202 and self.manifest(revocable)[0] == 404,
                "registry did not revoke the runtime manifest")
        time.sleep(20)
        running = status("always")[1]
        require(running["ready"] and running["restartCount"] == 0 and
                self.inspect_app("native", "always")["version"] == RUNTIMES[1][0],
                "registry revocation disturbed the running container")
        self.check("registry revocation leaves the running container serving")
        failed_before = failures("always")
        for name in pods:
            self.proxy("native", name, "exit")
        evidence["afterRevocation"] = {
            "always": blocked("always", 0, uids["always"], failed_before),
            "cached": restarted("cached", 0, uids["cached"], 120)}
        self.check("pullPolicy Always blocks restart after registry revocation")
        self.check("IfNotPresent restarts the revoked runtime from the node cache")
        code, _ = self.manifest(revocable, "PUT", manifest)
        require(code == 201, "could not restore the runtime manifest")
        evidence["afterRestore"] = restarted("always", 0, uids["always"], 420)
        self.check("restoring the manifest lets the blocked Pod restart in place")
        self.run("docker", "network", "disconnect", "kind", self.registry)
        try:
            failed_before = failures("always")
            for name in pods:
                self.proxy("native", name, "exit")
            evidence["duringOutage"] = {
                "always": blocked("always", 1, uids["always"], failed_before),
                "cached": restarted("cached", 1, uids["cached"], 120)}
            self.check("registry outage blocks pullPolicy Always restarts but not cached ones")
        finally:
            self.run("docker", "network", "connect", "kind", self.registry)
        evidence["afterOutage"] = restarted("always", 1, uids["always"], 420)
        evidence["events"] = self.events("native", "always")
        self.report["restartAuthorization"] = evidence
        self.check("registry recovery lets the blocked Pod restart in place")

    def cleanup(self):
        errors = []
        if self.cluster_attempted:
            result = self.run("kind", "delete", "cluster", "--name", self.name, check=False)
            if result.returncode:
                errors.append(result.stderr)
        if self.registry_created:
            result = self.run("docker", "rm", "-f", "-v", self.registry, check=False)
            if result.returncode:
                errors.append(result.stderr)
        for image in self.images:
            result = self.run("docker", "image", "rm", image, check=False)
            if result.returncode:
                errors.append(result.stderr)
        self.report["cleanupErrors"] = errors
        if errors:
            self.report["result"] = "failed"
        require(not errors, "experiment cleanup failed: " + "\n".join(errors))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", required=True, type=Path,
                        help="new run-* directory directly under integration-tests/live-v2")
    args = parser.parse_args()
    work = args.work.resolve()
    require(work.parent == Path(__file__).resolve().parent and work.name.startswith("run-"),
            "--work must be a new run-* directory directly under integration-tests/live-v2")
    work.mkdir(mode=0o700)
    experiment = Experiment(work)
    try:
        experiment.execute()
        experiment.report["result"] = "passed"
    except Exception as error:
        experiment.report["result"] = "failed"
        experiment.report["error"] = str(error)
        if experiment.kubeconfig.exists():
            diagnostic = experiment.kubectl("get", "pods,events,daemonsets,deployments",
                                            "--all-namespaces", "-o", "yaml", check=False)
            (work / "diagnostics.yaml").write_text(diagnostic.stdout + diagnostic.stderr)
        raise
    finally:
        try:
            experiment.cleanup()
        finally:
            (work / "result.json").write_text(json.dumps(experiment.report, indent=2) + "\n")
    print("Evidence:", work / "result.json", flush=True)


if __name__ == "__main__":
    main()
