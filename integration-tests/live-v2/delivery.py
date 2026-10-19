"""Proposal0009 remote delivery callbacks; importing this module has no side effects.

Parent wiring: bootstrap(h) AFTER prepare_images and BEFORE install; execute(h)
after startup/execution, before revocation; cleanup(h) in the parent's finally,
independently of cluster destruction. bootstrap publishes NEW selected targets,
never imports them, and changes h.pod to Always plus a user-owned pull secret.
Existing local-import assertions remain local-import evidence.

Native scale-from-zero is a separate, mandatory-when-selected callback:
begin_native_capacity(h), then finish_native_capacity(h, real_node_name). Supply
a genuine invocation-owned, initially absent alternate-architecture Linux node;
neither an emulated kind node nor a relabeled same-architecture node qualifies.
"""

import base64
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import ssl
import subprocess
import sys
import tarfile
import time
import urllib.error
import urllib.request

from harness import API, CLI_USERLAND, FENCE, OWNER, REGISTRY_IMAGE, Failure, application_started
from harness import docker_owned, require, require_owned


MANIFEST = "application/vnd.oci.image.manifest.v1+json"
INDEX = "application/vnd.oci.image.index.v1+json"
CONFIG = "application/vnd.oci.image.config.v1+json"
LAYER = "application/vnd.oci.image.layer.v1.tar+gzip"
USERNAME = "delivery-reader"
NATIVE_SOURCE = '''package main
import ("encoding/json"; "fmt"; "os"; "runtime"; "time")
func main() {
 if len(os.Args)>1 && os.Args[1]=="--inspect" {
  cwd,err:=os.Getwd(); if err!=nil { panic(err) }
  if err:=json.NewEncoder(os.Stdout).Encode(map[string]any{"uid":os.Getuid(),"gid":os.Getgid(),"env":os.Environ(),"cwd":cwd});err!=nil{panic(err)}; return
 }
 if len(os.Args)>2 && os.Args[1]=="--read" {
  data,err:=os.ReadFile(os.Args[2]); if os.IsNotExist(err) { fmt.Println("ABSENT");return };if err!=nil{panic(err)};fmt.Print(string(data));return
 }
 if len(os.Args)>2 && os.Args[1]=="--write" {
  if err:=os.WriteFile(os.Args[2],[]byte("native-write"),0644);err!=nil{fmt.Fprintln(os.Stderr,err);os.Exit(7)};return
 }
 fmt.Printf("NATIVE %s/%s\\n", runtime.GOOS, runtime.GOARCH)
 if len(os.Args)>1 && os.Args[1]=="--probe" { return }
 time.Sleep(24*time.Hour)
}
'''


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def canonical(authority, repository, target):
    require(bool(re.fullmatch(r"[a-zA-Z0-9.-]+:[0-9]+", authority)),
            "explicit registry authority and port required")
    require(bool(re.fullmatch(r"[a-z0-9]+(?:[._/-][a-z0-9]+)*", repository)),
            "invalid registry repository")
    require(bool(re.fullmatch(r"sha256:[0-9a-f]{64}", target)), "invalid target digest")
    return authority + "/" + repository + "@" + target


def native_platform(arch):
    require(arch in ("amd64", "arm64"), "native Linux amd64/arm64 required")
    return "linux/" + arch


def private_file(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW
    fd = os.open(path, flags, 0o600)
    with os.fdopen(fd, "wb") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(data.encode() if isinstance(data, str) else data)
    return path


def redact(text, password):
    auth = base64.b64encode((USERNAME + ":" + password).encode()).decode()
    for value in (password, auth, base64.b64encode(password.encode()).decode()):
        text = text.replace(value, "<redacted>")
    return re.sub(r"(?i)(authorization[\" :=]+)(?:basic|bearer) [^\s\",]+",
                  r"\1<redacted>", text)


def blob(layout, data, media):
    value = digest(data)
    private_file(layout / "blobs/sha256" / value[7:], data)
    return {"mediaType": media, "digest": value, "size": len(data)}


def json_blob(layout, value, media):
    return blob(layout, json.dumps(value, separators=(",", ":"), sort_keys=True).encode(), media)


def read_blob(layout, descriptor):
    value = descriptor["digest"]
    require(bool(re.fullmatch(r"sha256:[0-9a-f]{64}", value)), "invalid OCI descriptor digest")
    data = (layout / "blobs/sha256" / value[7:]).read_bytes()
    require(digest(data) == value and len(data) == descriptor["size"], "OCI descriptor identity mismatch")
    return data


def tar_layer(entries):
    """Craft exact, deterministic tar bytes, including deliberately invalid cases."""
    out = io.BytesIO()
    with tarfile.open(fileobj=out, mode="w", format=tarfile.USTAR_FORMAT) as archive:
        for name, data, mode in entries:
            entry = tarfile.TarInfo(name)
            entry.mode, entry.uid, entry.gid, entry.mtime = mode, 0, 0, 0
            if name.endswith("/"):
                entry.type, entry.size = tarfile.DIRTYPE, 0
                archive.addfile(entry)
            else:
                entry.size = len(data)
                archive.addfile(entry, io.BytesIO(data))
    raw = out.getvalue()
    return gzip.compress(raw, mtime=0), digest(raw)


def finish_layout(layout, descriptor):
    private_file(layout / "oci-layout", '{"imageLayoutVersion":"1.0.0"}')
    private_file(layout / "index.json", json.dumps({"schemaVersion": 2, "manifests": [descriptor]}))
    return descriptor


def mutate_application(source, destination, nonce, *, fault=None, platform=None):
    """Preserve bytes except explicit fixture changes; recompute every affected ID."""
    shutil.copytree(source, destination)
    outer = json.loads((destination / "index.json").read_text())
    require(len(outer["manifests"]) == 1, "one selected layout target required")
    selected = outer["manifests"][0]
    target = json.loads(read_blob(destination, selected))
    children = target.get("manifests", [selected])
    updated = []
    for child in children:
        manifest = json.loads(read_blob(destination, child))
        config = json.loads(read_blob(destination, manifest["config"]))
        if platform and config["architecture"] != platform:
            continue
        launch_descriptor = manifest["layers"][-1]
        require(launch_descriptor.get("annotations", {}).get("brewlet.sh/layer") == "launch",
                "last layer is not launch")
        with tarfile.open(fileobj=io.BytesIO(gzip.decompress(
                read_blob(destination, launch_descriptor))), mode="r:") as archive:
            launch = json.load(archive.extractfile("launch.json"))
        launch["process"].setdefault("env", []).append({"name": "BREWLET_DELIVERY_RUN", "value": nonce})
        config["config"].setdefault("Env", []).append("BREWLET_DELIVERY_RUN=" + nonce)
        if fault == "malformed":
            launch["unexpectedField"] = True
        launch_data, diff_id = tar_layer([("launch.json", json.dumps(launch).encode(), 0o644)])
        new_launch = blob(destination, launch_data, LAYER)
        new_launch["annotations"] = {"brewlet.sh/layer": "launch"}
        manifest["layers"][-1] = new_launch
        config["rootfs"]["diff_ids"][-1] = diff_id
        if fault == "unsafe":
            bad, bad_diff = tar_layer([("etc/delivery-outside-app", b"must not execute", 0o644)])
            descriptor = blob(destination, bad, LAYER)
            descriptor["annotations"] = {"brewlet.sh/layer": "app"}
            manifest["layers"].insert(-1, descriptor)
            config["rootfs"]["diff_ids"].insert(-1, bad_diff)
        if fault == "mirror":
            config["config"]["WorkingDir"] = "/must-not-match-launch"
        manifest["config"] = json_blob(destination, config, CONFIG)
        descriptor = json_blob(destination, manifest, MANIFEST)
        descriptor["platform"] = {k: config[k] for k in ("os", "architecture", "variant") if k in config}
        updated.append(descriptor)
    require(updated, "requested platform absent; no unsupported fallback")
    if "manifests" in target:
        target["manifests"] = updated
        selected = json_blob(destination, target, INDEX)
    else:
        selected = updated[0]
    return finish_layout(destination, selected)


def static_runtime(layout):
    """A real minimal golden root for static native binaries, not a language mock."""
    entries = [(name, b"", 0o755) for name in ("etc/", "usr/", "usr/bin/", "home/", "home/app/", "tmp/")]
    entries += [("etc/passwd", b"app:x:65532:65532:app:/home/app:/sbin/nologin\n", 0o644),
                ("etc/group", b"app:x:65532:\n", 0o644)]
    data, diff = tar_layer(entries)
    layer = blob(layout, data, LAYER)
    manifests = []
    for arch in ("amd64", "arm64"):
        config = json_blob(layout, {"architecture": arch, "os": "linux",
                                   "config": {"Env": ["PATH=/usr/bin"]},
                                   "rootfs": {"type": "layers", "diff_ids": [diff]}}, CONFIG)
        manifest = json_blob(layout, {"schemaVersion": 2, "mediaType": MANIFEST,
                                     "config": config, "layers": [layer]}, MANIFEST)
        manifest["platform"] = {"os": "linux", "architecture": arch}
        manifests.append(manifest)
    return finish_layout(layout, json_blob(layout, {"schemaVersion": 2, "mediaType": INDEX,
                                                   "manifests": manifests}, INDEX))


class Delivery:
    def __init__(self, h):
        self.h = h
        self.work = h.work / "delivery"
        self.work.mkdir(mode=0o700)
        self.password = secrets.token_urlsafe(32)
        self.registry_id = None
        self.producer_id = None
        self.registry_name = h.cluster + "-tls"
        self.producer_name = h.cluster + "-delivery-producer"
        self.secret = "delivery-pull-" + h.run_id
        self.static_id = "delivery-static-" + h.run_id
        self.original_pod = h.pod
        self.cases = {}
        self.native = {}
        self.proofs = set()

    def private_run(self, command, *, input=None, env=None, check=True, timeout=180):
        """No raw stdout/stderr/credentials in Harness command logs or results."""
        environment = dict(os.environ, KUBECONFIG=str(self.h.kubeconfig),
                           TMPDIR=str(self.work), PYTHONDONTWRITEBYTECODE="1",
                           DOCKER_DEFAULT_PLATFORM=native_platform(self.h.arch))
        for key in ("BREWLET_REGISTRY_USERNAME", "BREWLET_REGISTRY_PASSWORD", "DOCKER_CONFIG"):
            environment.pop(key, None)
        environment.update(env or {})
        process = subprocess.Popen(list(map(str, command)), cwd=self.work, env=environment,
                                   stdin=subprocess.PIPE if input is not None else None,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                                   start_new_session=True)
        try:
            stdout, stderr = process.communicate(input, timeout=timeout)
        except BaseException:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.communicate(timeout=5)
            raise Failure("bounded private delivery command interrupted")
        result = subprocess.CompletedProcess(command, process.returncode, stdout, stderr)
        if check and result.returncode:
            raise Failure("private delivery command failed: " + redact(stderr[-1500:], self.password))
        return result

    def registry_info(self):
        info = json.loads(self.h.run(["docker", "inspect", self.registry_id]).stdout)[0]
        docker_owned(info, self.h.run_id, self.registry_id)
        return info

    def start_registry(self):
        h = self.h
        require(shutil.which("htpasswd"), "mandatory bcrypt htpasswd tool missing")
        auth = self.private_run(["htpasswd", "-niB", USERNAME],
                                input=self.password + "\n").stdout
        require(auth.startswith(USERNAME + ":$2"), "registry requires bcrypt authentication")
        private_file(self.work / "htpasswd", auth)
        self.private_run(["htpasswd", "-vi", self.work / "htpasswd", USERNAME],
                         input=self.password + "\n")
        private_file(self.work / "registry.yml", """version: 0.1
log:
  level: info
storage:
  filesystem:
    rootdirectory: /var/lib/registry
http:
  addr: :5000
  tls:
    certificate: /delivery/tls.crt
    key: /delivery/tls.key
auth:
  htpasswd:
    realm: brewlet-delivery
    path: /delivery/htpasswd
""")
        # The registry waits for its own SAN certificate after Docker allocates
        # its real address. Nothing listens in plaintext during this interval.
        require(h.run(["docker", "inspect", self.registry_name], check=False).returncode != 0,
                "TLS registry name is already occupied")
        command = ["docker", "create", "--platform", native_platform(h.arch),
                        "--name", self.registry_name, "--network", "kind",
                        "--label", OWNER + "=" + h.run_id, "--publish", "127.0.0.1::5000",
                        "--read-only", "--tmpfs", "/var/lib/registry",
                        "--tmpfs", "/tmp", "--mount",
                        f"type=bind,source={self.work},target=/delivery,readonly",
                        "--entrypoint", "/bin/sh", REGISTRY_IMAGE, "-ec",
                        "while [ ! -s /delivery/tls.crt ]; do sleep 1; done; "
                        "exec /bin/registry serve /delivery/registry.yml"]
        try:
            h.run(command)
        finally:
            # A bounded create can be interrupted after Docker committed it.
            # Acquire its exact identity even on that partial-failure path.
            result = h.run(["docker", "inspect", self.registry_name], check=False)
            if result.returncode == 0:
                info = json.loads(result.stdout)[0]
                docker_owned(info, h.run_id, info["Id"])
                self.registry_id = info["Id"]
        require(self.registry_id and bool(re.fullmatch(r"[0-9a-f]{64}", self.registry_id)),
                "registry exact ID missing")
        self.registry_info()
        h.run(["docker", "start", self.registry_id])
        info = self.registry_info()
        address = info["NetworkSettings"]["Networks"]["kind"]["IPAddress"]
        require(bool(re.fullmatch(r"[0-9.]+", address)), "registry Docker IPv4 missing")
        port = info["NetworkSettings"]["Ports"]["5000/tcp"][0]["HostPort"]
        self.authority = address + ":5000"
        self.untrusted_authority = self.registry_name + ":5000"
        self.producer_authority = ("host.docker.internal:" + port if sys.platform == "darwin"
                                   else self.authority)
        self.local_origin = "https://127.0.0.1:" + port
        private_file(self.work / "ca.key", "")
        private_file(self.work / "tls.key", "")
        h.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
               "-subj", "/CN=delivery-ca-" + h.run_id, "-addext", "basicConstraints=critical,CA:TRUE",
               "-addext", "keyUsage=critical,keyCertSign,cRLSign",
               "-addext", "subjectKeyIdentifier=hash",
               "-keyout", self.work / "ca.key", "-out", self.work / "ca.crt"])
        os.chmod(self.work / "ca.key", 0o600)
        h.run(["openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes",
               "-subj", "/CN=" + self.registry_name, "-keyout", self.work / "tls.key",
               "-out", self.work / "tls.csr"])
        os.chmod(self.work / "tls.key", 0o600)
        private_file(self.work / "extensions",
                     "subjectAltName=DNS:" + self.registry_name +
                     ",DNS:host.docker.internal,IP:127.0.0.1,IP:" + address +
                     "\nextendedKeyUsage=serverAuth\nbasicConstraints=critical,CA:FALSE\n"
                     "keyUsage=critical,digitalSignature,keyEncipherment\n"
                     "subjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid,issuer\n")
        # Publish the cert atomically: the registry's wait must not observe a
        # partially written PEM file.
        h.run(["openssl", "x509", "-req", "-in", self.work / "tls.csr",
               "-CA", self.work / "ca.crt", "-CAkey", self.work / "ca.key", "-CAcreateserial",
               "-days", "2", "-extfile", self.work / "extensions", "-out", self.work / "ready.crt"])
        os.replace(self.work / "ready.crt", self.work / "tls.crt")
        self.context = ssl.create_default_context(cafile=str(self.work / "ca.crt"))

        def challenged():
            info = self.registry_info()
            require(info["State"]["Running"],
                    "TLS registry exited before readiness (exit "
                    + str(info["State"].get("ExitCode")) + ")")
            try:
                self.fetch("/v2/", authenticated=False)
            except urllib.error.HTTPError as error:
                require(error.code == 401 and "basic" in error.headers.get("WWW-Authenticate", "").lower(),
                        "TLS registry did not require basic authentication")
                return True
            except urllib.error.URLError as error:
                if isinstance(error.reason, ssl.SSLCertVerificationError):
                    raise Failure("TLS registry certificate verification failed: " + str(error.reason)) from error
                return False
            except (TimeoutError, ConnectionResetError):
                return False
            raise Failure("anonymous registry unexpectedly authorized")
        h.wait("TLS-only registry returns authenticated challenge", challenged, timeout=60)

    def fetch(self, path, *, authenticated=True):
        request = urllib.request.Request(self.local_origin + path,
                                         headers={"Accept": INDEX + ", " + MANIFEST})
        if authenticated:
            request.add_header("Authorization", "Basic " +
                               base64.b64encode((USERNAME + ":" + self.password).encode()).decode())
        # No redirects can forward the fixture credential to a different origin.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, headers, newurl):
                raise Failure("registry redirected outside the explicit delivery endpoint")
        opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=self.context), NoRedirect())
        with opener.open(request, timeout=10) as response:
            return response.read()

    def producer(self):
        command = list(map(str, self.h.cli_command))
        if command[0] != "docker":
            return command
        require(command[:2] == ["docker", "run"] and "--network" in command,
                "unrecognized trusted Linux producer prefix")
        require("--platform" in command and command[command.index("--platform") + 1]
                == native_platform(self.h.arch) and CLI_USERLAND in command,
                "trusted producer requires explicit native platform and pinned userland")
        # h.cli_command's Linux CLI container is the trusted producer; only that
        # invocation receives network access and its CA/auth environment file.
        at = command.index("--network")
        require(command[at + 1] == "none", "producer network was not isolated")
        command[at + 1] = "bridge"
        environment_file = private_file(self.work / "producer.env",
            f"SSL_CERT_FILE={self.work / 'ca.crt'}\nBREWLET_REGISTRY_USERNAME={USERNAME}\n"
            f"BREWLET_REGISTRY_PASSWORD={self.password}\n"
            "HTTP_PROXY=\nHTTPS_PROXY=\nALL_PROXY=\nhttp_proxy=\nhttps_proxy=\nall_proxy=\n")
        command[2:2] = ["--label", OWNER + "=" + self.h.run_id,
                        "--name", self.producer_name,
                        "--env-file", str(environment_file)]
        return command

    def run_producer(self, command, *, env=None):
        if command[:2] == ["docker", "run"]:
            create = ["docker", "create", *[arg for arg in command[2:] if arg != "--rm"]]
            require(self.private_run(["docker", "inspect", self.producer_name],
                                     check=False).returncode != 0, "producer name already occupied")
            try:
                identity = self.private_run(create).stdout.strip()
                require(bool(re.fullmatch(r"[0-9a-f]{64}", identity)), "producer exact ID missing")
                self.producer_id = identity
                # Config.Env contains credentials: NEVER use h.run/its logger.
                info = json.loads(self.private_run(["docker", "inspect", self.producer_id]).stdout)[0]
                docker_owned(info, self.h.run_id, self.producer_id)
                self.private_run(["docker", "start", "--attach", self.producer_id], timeout=300)
                info = json.loads(self.private_run(["docker", "inspect", self.producer_id]).stdout)[0]
                require(info["State"]["ExitCode"] == 0, "trusted producer failed publication")
            finally:
                if not self.producer_id:
                    result = self.private_run(["docker", "inspect", self.producer_name], check=False)
                    if result.returncode == 0:
                        info = json.loads(result.stdout)[0]
                        docker_owned(info, self.h.run_id, info["Id"])
                        self.producer_id = info["Id"]
                self.cleanup_producer()
        else:
            self.private_run(command, env=env, timeout=300)

    def publish(self, layout, descriptor, repository):
        producer_ref = self.producer_authority + "/" + repository + ":delivery"
        command = [*self.producer(), "imagepush", producer_ref, "--store", str(layout),
                   "--source", descriptor["digest"]]
        self.run_producer(command, env={"SSL_CERT_FILE": str(self.work / "ca.crt"),
                          "BREWLET_REGISTRY_USERNAME": USERNAME,
                          "BREWLET_REGISTRY_PASSWORD": self.password,
                          **{key: "" for key in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
                                                "http_proxy", "https_proxy", "all_proxy")}})
        data = self.fetch("/v2/" + repository + "/manifests/" + descriptor["digest"])
        require(digest(data) == descriptor["digest"] and len(data) == descriptor["size"],
                "producer/node-authority target digest changed")
        return canonical(self.authority, repository, descriptor["digest"])

    def node_private_file(self, authority, name, data, *, node=None):
        require(authority in (self.authority, self.untrusted_authority),
                "trust mutation outside invocation registry")
        require(name in ("ca.crt", "hosts.toml", "credentials.json"), "invalid trusted host file")
        execute = self.h.node_exec if node is None else (
            lambda *args, **kwargs: self.h.node_exec_on(node, *args, **kwargs))
        directory = "/etc/containerd/certs.d/" + authority
        execute("mkdir", "-p", directory)
        execute("chmod", "0700", directory)
        execute("sh", "-ec", 'umask 077; cat > "$1"', "delivery",
                directory + "/" + name, input=data)
        observed = execute("sha256sum", directory + "/" + name).stdout.split()
        require(observed and observed[0] == hashlib.sha256(data.encode()).hexdigest(),
                "node registry file readback differs from the supplied bytes")

    def configure_node(self, node=None):
        self.node_private_file(self.authority, "ca.crt", (self.work / "ca.crt").read_text(), node=node)
        self.node_private_file(self.authority, "hosts.toml",
            f'server = "https://{self.authority}"\n[host."https://{self.authority}"]\n'
            '  capabilities = ["pull", "resolve"]\n  ca = "ca.crt"\n', node=node)
        # CRI ignores this file: only the worker resolver reads it. Kubelet
        # authorization MUST come from the workload's imagePullSecrets.
        self.node_private_file(self.authority, "credentials.json",
                               json.dumps({"username": USERNAME, "password": self.password}), node=node)
        config = (self.h.node_exec("cat", "/etc/containerd/config.toml") if node is None else
                  self.h.node_exec_on(node, "cat", "/etc/containerd/config.toml")).stdout
        require("/etc/containerd/certs.d" in config,
                "containerd CRI trusted host config_path is not configured")
        if node is None:
            self.install_pull_secret(self.h.namespace)

    def install_pull_secret(self, namespace):
        key = ("namespace", "", namespace)
        require(key in self.h.owned, "pull secret destination is not an owned namespace")
        require_owned(self.h.get("namespace", namespace), self.h.run_id, self.h.owned[key])
        require(("secret", namespace, self.secret) not in self.h.owned,
                "pull secret already installed in destination namespace")
        auth = base64.b64encode((USERNAME + ":" + self.password).encode()).decode()
        obj = {"apiVersion": "v1", "kind": "Secret",
               "metadata": {"name": self.secret, "namespace": namespace,
                            "labels": {OWNER: self.h.run_id}},
               "type": "kubernetes.io/dockerconfigjson",
               "stringData": {".dockerconfigjson": json.dumps(
                   {"auths": {self.authority: {"auth": auth}}})}}
        # h.apply prints the returned Secret JSON. Use name-only output and a
        # metadata-only read so private credentials never enter command logs.
        self.h.kubectl("create", "-f", "-", "-o", "name", input=json.dumps(obj))
        uid = self.h.kubectl("-n", namespace, "get", "secret", self.secret,
                             "-o", "jsonpath={.metadata.uid}").stdout.strip()
        require(uid, "user-controlled imagePullSecret has no UID")
        self.h.owned[("secret", namespace, self.secret)] = uid

    def fresh(self, reference):
        target = reference.split("@")[1]
        images = self.h.ctr("images", "ls")
        require(reference not in images and target not in images,
                "selected remote target was already imported or pulled")
        require(target not in self.h.ctr("content", "ls", "--quiet").splitlines(),
                "selected remote manifest already in content store")

    def pod(self, *args, **kwargs):
        pod = self.original_pod(*args, **kwargs)
        pod["spec"]["imagePullSecrets"] = [{"name": self.secret}]
        for container in pod["spec"]["containers"]:
            container["imagePullPolicy"] = "Always"
        return pod

    def golden_layout(self, language, revision):
        h = self.h
        tag = f"registry.brewlet.test/{h.run_id}/runtime/{language}:{revision}"
        info = json.loads(h.run(["docker", "image", "inspect", tag]).stdout)[0]
        require(info["Id"] == h.images[tag] and info["Config"]["Labels"][OWNER] == h.run_id,
                "refusing foreign golden image")
        require(info["Os"] == "linux" and info["Architecture"] == h.arch,
                "golden image is not the selected native platform")
        archive_path = self.work / (language + "-" + revision + ".docker.tar")
        h.run(["docker", "image", "save", "--output", archive_path, info["Id"]], timeout=300)
        layout = self.work / (language + "-" + revision + "-golden")
        with tarfile.open(archive_path) as archive:
            rows = json.load(archive.extractfile("manifest.json"))
            require(len(rows) == 1, "ambiguous golden Docker archive")
            row = rows[0]
            config = json.load(archive.extractfile(row["Config"]))
            # A new, explicit approval digest over the same golden root bytes.
            # .brewlet-source and runtime filesystem markers are never edited.
            config["config"].setdefault("Labels", {})["acceptance.brewlet.test/delivery"] = h.run_id
            config_descriptor = json_blob(layout, config, CONFIG)
            layers = []
            for index, name in enumerate(row["Layers"]):
                source = archive.extractfile(name)
                magic = source.read(4)
                source.seek(0)
                if magic[:2] == b"\x1f\x8b":
                    source = gzip.GzipFile(fileobj=source, mode="rb")
                require(magic != b"\x28\xb5\x2f\xfd",
                        "Docker save zstd layer requires an explicit supported decoder")
                raw_hash = hashlib.sha256()
                compressed = self.work / "compressing-layer"
                with compressed.open("wb") as output:
                    # Vary only the compression envelope, never approved root
                    # files or .brewlet-source. The cold compressed descriptors
                    # force invocation-fresh remote golden-layer transfer.
                    with gzip.GzipFile(filename="", fileobj=output, mode="wb",
                                       mtime=int(h.run_id, 16) % (2**32)) as zipped:
                        while chunk := source.read(1024 * 1024):
                            raw_hash.update(chunk)
                            zipped.write(chunk)
                require("sha256:" + raw_hash.hexdigest() == config["rootfs"]["diff_ids"][index],
                        "golden layer diff ID mismatch")
                hashed = hashlib.sha256()
                with compressed.open("rb") as content:
                    while chunk := content.read(1024 * 1024):
                        hashed.update(chunk)
                layer_digest = "sha256:" + hashed.hexdigest()
                require(layer_digest not in h.ctr("content", "ls", "--quiet").splitlines(),
                        "selected golden compressed layer was preimported")
                target = layout / "blobs/sha256" / layer_digest[7:]
                target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                size = compressed.stat().st_size
                os.replace(compressed, target)
                layers.append({"mediaType": LAYER, "digest": layer_digest, "size": size})
        archive_path.unlink()
        return layout, finish_layout(layout, json_blob(layout, {"schemaVersion": 2, "mediaType": MANIFEST,
                                                    "config": config_descriptor, "layers": layers}, MANIFEST))

    def static_layout(self, layout):
        return static_runtime(layout)

    def prepare_native(self, arch):
        directory = self.work / ("native-" + arch)
        directory.mkdir(mode=0o700)
        private_file(directory / "main.go", NATIVE_SOURCE)
        source = directory / "app"
        source.mkdir()
        self.h.run(["go", "build", "-trimpath", "-o", source / "native", directory / "main.go"],
                   env={"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch}, timeout=180)
        os.chmod(source / "native", 0o755)
        dependency = directory / "dependency"
        dependency.mkdir()
        (dependency / "contract").write_text("native-fixture\n")
        os.chmod(dependency / "contract", 0o644)
        launch = private_file(directory / "launch.json", json.dumps({
            "schemaVersion": 2, "process": {"entrypoint": ["/app/bin/native"]}}))
        layout = directory / "layout"
        command = list(map(str, self.h.cli_command))
        if command[:2] == ["docker", "run"]:
            require("--platform" in command and command[command.index("--platform") + 1]
                    == native_platform(self.h.arch) and CLI_USERLAND in command,
                    "native fixture producer must itself run without emulation")
            command[2:2] = ["--label", OWNER + "=" + self.h.run_id,
                            "--name", self.producer_name]
        self.run_producer([*command, "push", "registry.brewlet.test/native:" + arch,
                           "--store", str(layout), "--platform", native_platform(arch),
                           "--launch", str(launch), "--layer", f"dependency={dependency}:/app/dependency",
                           "--layer", f"app={source}:/app/bin"])
        descriptor = json.loads((layout / "index.json").read_text())["manifests"][0]
        if descriptor["mediaType"] != INDEX:
            # A single manifest can be accepted by CRI before its config's
            # architecture is considered. Use an explicit single-platform
            # index so the unsupported case tests real pull-time selection.
            descriptor["platform"] = {"os": "linux", "architecture": arch}
            descriptor = finish_layout(layout, json_blob(layout, {
                "schemaVersion": 2, "mediaType": INDEX, "manifests": [descriptor]}, INDEX))
        self.native[arch] = self.publish(layout, descriptor, "delivery/native-" + arch)

    def bootstrap(self):
        self.start_registry()
        self.configure_node()
        for language in self.h.languages:
            layout = self.work / ("app-" + language)
            descriptor = mutate_application(self.h.work / ("oci-" + language), layout,
                                            self.h.run_id + "-" + language)
            reference = self.publish(layout, descriptor, "delivery/apps-" + language)
            self.fresh(reference)
            self.h.app_images[language] = reference
            for revision in ("before", "after"):
                golden, selected = self.golden_layout(language, revision)
                remote = self.publish(golden, selected, f"delivery/runtime-{language}-{revision}")
                self.fresh(remote)
                self.h.runtime_images[language][revision] = remote
        for case, fault in (("unauthorized", None), ("untrusted", None), ("corrupt", None),
                            ("unsafe", "unsafe"), ("malformed", "malformed"), ("mirror", "mirror")):
            layout = self.work / ("negative-" + case)
            descriptor = mutate_application(self.h.work / ("oci-" + self.h.probe_language), layout,
                                            self.h.run_id + "-" + case, fault=fault)
            repository = "delivery/negative-" + case
            self.cases[case] = self.publish(layout, descriptor, repository)
            self.fresh(self.cases[case])
            if case == "corrupt":
                selected = json.loads(read_blob(layout, descriptor))
                children = selected.get("manifests", [descriptor])
                child = next((value for value in children if value.get("platform", {}).get(
                    "architecture") == self.h.arch), children[0])
                manifest = json.loads(read_blob(layout, child))
                corrupt = manifest["layers"][-1]["digest"][7:]
                # Only this invocation/case's nonce-bearing launch blob changes.
                path = "/var/lib/registry/docker/registry/v2/blobs/sha256/" + corrupt[:2] + "/" + corrupt + "/data"
                self.registry_info()
                self.h.run(["docker", "exec", self.registry_id, "/bin/sh", "-ec",
                            'printf X | dd of="$1" bs=1 count=1 conv=notrunc', "delivery", path])
        for arch in ("amd64", "arm64"):
            self.prepare_native(arch)
        layout = self.work / "static-golden"
        selected = self.static_layout(layout)
        self.static_source = self.publish(layout, selected, "delivery/static-golden")
        self.fresh(self.static_source)
        self.h.pod = self.pod
        self.h.result["delivery"] = {
            "transport": "https-with-private-ca-and-basic-auth", "registryID": self.registry_id,
            "registryImage": REGISTRY_IMAGE, "nodeAuthority": self.authority,
            "producerAuthority": self.producer_authority, "bootstrapOnly": True,
            "selectedDigestsNotPreimported": True, "kubeletPullVerified": False,
            "runtimeSourcePullVerified": False, "nativeAlternateCapacityVerified": False}
        self.h.result["applicationImages"] = self.h.app_images
        self.h.result["runtimeImages"] = self.h.runtime_images
        self.h.save()

    def negative(self, case, patterns):
        h = self.h
        reference = self.cases[case]
        self.fresh(reference)
        pod = self.pod("delivery-" + case, h.probe_language, bound=True)
        pod["spec"]["containers"][0]["image"] = reference
        if case == "unauthorized":
            pod["spec"].pop("imagePullSecrets")
            pod["spec"]["serviceAccountName"] = "default"
            require(not h.get("serviceaccount", "default", h.namespace).get("imagePullSecrets"),
                    "unauthorized fixture serviceaccount unexpectedly has registry credentials")
        if case == "untrusted":
            pod["spec"]["containers"][0]["image"] = reference.replace(self.authority, self.untrusted_authority, 1)
        h.apply(pod)
        name = pod["metadata"]["name"]

        def rejected():
            obj = h.get("pod", name, h.namespace)
            require_owned(obj, h.run_id, h.owned[("pod", h.namespace, name)])
            for status in obj.get("status", {}).get("containerStatuses", []):
                require(not application_started(status), case + " application actually started")
            events = h.get("events", namespace=h.namespace)["items"]
            messages = [event.get("message", "") for event in events
                        if event.get("involvedObject", {}).get("uid") == obj["metadata"]["uid"]]
            observation = "\n".join(messages) + json.dumps(obj.get("status", {}))
            matched = any(re.search(pattern, observation, re.I) for pattern in patterns)
            require(matched or obj.get("status", {}).get("phase") not in ("Failed", "Succeeded"),
                    "remote rejection " + case + " ended with an unexpected reason: " + observation[-2500:])
            return obj if matched else False
        obj = h.wait("fresh remote rejection " + case, rejected, timeout=180)
        h.record("remote TLS delivery rejects " + case,
                 {"podUID": obj["metadata"]["uid"], "image": pod["spec"]["containers"][0]["image"],
                  "noApplicationStart": True, "matchedReasons": patterns}, ["ART-01", "RUN-01"])
        h.delete("pod", name, h.namespace)
        h.wait("negative pod removed", lambda: h.get("pod", name, h.namespace, absent=True) is None)
        self.proofs.add(case)

    def native_pod(self, arch, name):
        pod = self.pod(name, self.h.probe_language)
        pod["metadata"]["annotations"]["brewlet.sh/runtime-image"] = self.static_id
        pod["spec"]["nodeSelector"] = {"kubernetes.io/arch": arch, "brewlet.test/pool": self.h.run_id}
        pod["spec"]["containers"][0].update(image=self.native[arch],
            readinessProbe={"exec": {"command": ["/app/bin/native", "--probe"]},
                            "periodSeconds": 2, "timeoutSeconds": 5})
        return pod

    def execute(self):
        h = self.h
        for language in h.languages:
            reference = h.app_images[language]
            require(h.image_digest(reference) == reference.split("@")[1],
                    "kubelet pulled a different application target")
            pod = h.get("pod", language + "-old", h.namespace)
            require_owned(pod, h.run_id, h.initial[language]["pod"]["metadata"]["uid"])
            require(pod["spec"]["containers"][0]["imagePullPolicy"] == "Always"
                    and pod["spec"].get("imagePullSecrets") == [{"name": self.secret}],
                    "positive application did not use workload-controlled Always pull")
            uid = pod["metadata"]["uid"]
            events = h.get("events", namespace=h.namespace)["items"]
            require(any(e.get("involvedObject", {}).get("uid") == uid and e.get("reason") == "Pulled"
                        and "pulled" in e.get("message", "").lower()
                        and "already present" not in e.get("message", "").lower() for e in events),
                    "positive fresh kubelet remote pull event missing")
            metadata = h.initial[language]["metadata"]
            require(metadata["sourceDigest"] == h.runtime_images[language]["before"].split("@")[1],
                    "worker current source is not the new remote golden target")
            h.record(language + " kubelet Always TLS/auth pull and remote approved runtime acquisition",
                     {"podUID": uid, "application": reference,
                      "runtimeSource": h.runtime_images[language]["before"],
                      "imagePullSecretUID": h.owned[("secret", h.namespace, self.secret)],
                      "selectedTargetWasNotPreimported": True}, ["ART-01", "RUN-01", "ROOT-01"])
        self.registry_info()
        # Registry logging may be on stderr. Count only successful GET blob
        # responses, not producer PUT/HEAD/upload requests.
        result = h.run(["docker", "logs", self.registry_id])
        lines = (result.stdout + result.stderr).splitlines()
        reads = [line for line in lines if re.search(r'http.request.method="?GET"?', line)
                 and re.search(r'http.response.status="?200"?', line)]
        for repository in ("delivery/apps-" + language for language in h.languages):
            require(any("/v2/" + repository + "/blobs/" in line for line in reads),
                    "registry never served selected application blobs over TLS")
        for language in h.languages:
            require(any("/v2/delivery/runtime-" + language + "-before/blobs/" in line for line in reads),
                    "runtime source was not acquired remotely")
        for case, reasons in {
            "unauthorized": [r"401", r"unauthori[sz]ed", r"authorization failed"],
            "untrusted": [r"x509", r"certificate signed by unknown authority"],
            "corrupt": [r"digest.*(?:mismatch|invalid|unexpected)", r"unexpected.*digest",
                        r"failed size validation", r"invalid checksum"],
            "unsafe": ["LayerPathViolation"],
            "malformed": ["InvalidLaunchConfig"],
            "mirror": ["LaunchConfigMismatch"],
        }.items():
            self.negative(case, reasons)
        h.apply({"apiVersion": API, "kind": "RuntimeImage", "metadata": {"name": self.static_id},
                 "spec": {"source": {"image": self.static_source}, "user": {"uid": 65532, "gid": 65532}}})
        h.wait("static golden inventory approved", lambda:
               any(c["type"] == "Approved" and c["status"] == "True"
                   for c in h.get("runtimeimage", self.static_id).get("status", {}).get("conditions", [])))
        profile = h.get("nodeprofile", h.profile)
        profile["spec"]["runtimeImages"].append({"id": self.static_id, "install": "OnDemand"})
        h.replace_inventory_spec(profile)
        h.wait("native static runtime available", lambda: h.advertised(self.static_id, "available"))
        self.fresh(self.native[h.arch])
        pod = self.native_pod(h.arch, "delivery-native")
        h.apply(pod)
        obj = h.wait("real native artifact executes on selected architecture", lambda: h.ready("delivery-native"))
        require(obj["spec"]["nodeSelector"]["kubernetes.io/arch"] == h.arch
                and obj["spec"]["nodeName"] == h.node, "native Kubernetes node selection changed")
        require(h.exec("delivery-native", ["/app/bin/native", "--probe"]).stdout.strip()
                == "NATIVE " + native_platform(h.arch), "native binary did not execute its compiled platform")
        h.record("remote native artifact executes with preserved standard architecture selector",
                 {"podUID": obj["metadata"]["uid"], "nodeUID": h.node_uid, "platform": native_platform(h.arch),
                  "image": self.native[h.arch]}, ["E2E-05", "ART-01"])
        other = "amd64" if h.arch == "arm64" else "arm64"
        self.cases["platform"] = self.native[other]
        self.negative("platform", [r"no match.*platform", r"no matching.*platform", r"platform.*not found"])
        h.result["delivery"].update(bootstrapOnly=False, kubeletPullVerified=True,
            runtimeSourcePullVerified=True, tlsTransportVerified=True,
            nativeExecutionVerified=True, platformMismatchRejected=True,
            negativeFreshStarts=sorted(self.proofs),
            limitation="Alternate native capacity requires begin/finish_native_capacity on a real second node.")
        h.save()

    def cleanup_producer(self):
        if self.producer_id:
            info = json.loads(self.private_run(["docker", "inspect", self.producer_id]).stdout)[0]
            docker_owned(info, self.h.run_id, self.producer_id)
            self.private_run(["docker", "rm", "--force", "--volumes", self.producer_id])
            self.producer_id = None

    def cleanup(self):
        # Always independent from parent cluster cleanup; a failed node teardown
        # must not prevent removing the exact owned TLS registry.
        try:
            self.cleanup_producer()
        finally:
            if self.registry_id:
                info = self.registry_info()
                try:
                    logs = self.private_run(["docker", "logs", self.registry_id], check=False)
                    private_file(self.work / "registry-redacted.log",
                                 redact(logs.stdout + logs.stderr, self.password))
                finally:
                    self.h.run(["docker", "rm", "--force", "--volumes", info["Id"]])
                    require(self.h.run(["docker", "inspect", info["Id"]], check=False).returncode != 0,
                            "owned TLS registry remains after cleanup")
                    self.registry_id = None


def bootstrap(h):
    require(not hasattr(h, "delivery"), "delivery already bootstrapped")
    h.delivery = Delivery(h)  # Retain cleanup handle even after partial bootstrap.
    h.delivery.bootstrap()


def execute(h):
    require(hasattr(h, "delivery"), "selected remote delivery requires bootstrap before installation")
    h.delivery.execute()


def cleanup(h):
    if hasattr(h, "delivery"):
        h.delivery.cleanup()


def begin_native_capacity(h):
    """After execute(h), select only if a real alternate native node can join."""
    require(hasattr(h, "delivery"), "remote native fixtures not bootstrapped")
    delivery = h.delivery
    other = "amd64" if h.arch == "arm64" else "arm64"
    require(not any(n["metadata"]["labels"].get("kubernetes.io/arch") == other
                    for n in h.get("nodes")["items"]), "alternate capacity was not initially absent")
    # A native-only pool must not inherit the base suite's single-architecture
    # Java/Python/Node/.NET golden approvals.
    delivery.native_pool = h.run_id + "-native"
    delivery.native_profile = h.profile + "-native"
    h.apply({"apiVersion": API, "kind": "NodeProfile",
             "metadata": {"name": delivery.native_profile},
             "spec": {"nodePool": {"key": "brewlet.test/pool", "names": [delivery.native_pool]},
                      "runtimeImages": [{"id": delivery.static_id, "install": "OnDemand"}],
                      "tolerations": [{"key": FENCE, "operator": "Equal",
                                       "value": "provisioning", "effect": "NoSchedule"}],
                      "rollout": {"maxUnavailable": 1, "containerdRestart": "validated"}}})
    pod = delivery.native_pod(other, "delivery-native-capacity")
    pod["spec"]["nodeSelector"]["brewlet.test/pool"] = delivery.native_pool
    h.apply(pod)
    def pending():
        obj = h.get("pod", pod["metadata"]["name"], h.namespace)
        require(not obj["spec"].get("nodeName"), "alternate artifact scheduled without native capacity")
        conditions = obj.get("status", {}).get("conditions", [])
        return obj if any(c["type"] == "PodScheduled" and c["status"] == "False"
                          and c.get("reason") == "Unschedulable" for c in conditions) else False
    obj = h.wait("native alternate capacity genuinely Pending", pending, timeout=60)
    require(obj["spec"]["nodeSelector"]["kubernetes.io/arch"] == other,
            "admission changed alternate architecture constraint")
    delivery.pending_native_uid = obj["metadata"]["uid"]
    delivery.initial_node_uids = {n["metadata"]["uid"] for n in h.get("nodes")["items"]}
    return {"requiredPlatform": native_platform(other), "podUID": obj["metadata"]["uid"],
            "runnerPlan": "Join a second private native Linux VM using this invocation's kubeadm "
                          "bootstrap, actual Node architecture, removable Brewlet startup taint and "
                          "brewlet.test/pool=" + delivery.native_pool + ". Label its exact Node UID "
                          "with " + OWNER + "=" + h.run_id + ". Configure only that owned node's "
                          "CA and origin-bound worker credentials (never kubelet host credentials); "
                          "use a private route to the invocation registry DockerIP and the same "
                          "authenticated dual-platform static golden source. Never "
                          "use a shared context, QEMU, or architecture relabeling."}


def finish_native_capacity(h, node_name):
    delivery = getattr(h, "delivery", None)
    require(delivery and hasattr(delivery, "pending_native_uid") and node_name,
            "selected alternate-native case lacks its real late-arriving node")
    node = h.get("node", node_name)
    require_owned(node, h.run_id)
    other = "amd64" if h.arch == "arm64" else "arm64"
    require(node["metadata"]["uid"] not in delivery.initial_node_uids
            and node["status"]["nodeInfo"]["architecture"] == other
            and node["status"]["nodeInfo"]["operatingSystem"] == "linux"
            and node["metadata"]["labels"].get("brewlet.test/pool") == delivery.native_pool,
            "late capacity is not a genuinely new alternate native Linux node")
    obj = h.wait("pending native pod starts on actual alternate capacity",
                 lambda: h.ready("delivery-native-capacity"))
    require(obj["metadata"]["uid"] == delivery.pending_native_uid
            and obj["spec"]["nodeName"] == node_name
            and obj["spec"]["nodeSelector"]["kubernetes.io/arch"] == other,
            "native capacity changed pod identity, selected architecture, or actual placement")
    require(h.exec("delivery-native-capacity", ["/app/bin/native", "--probe"]).stdout.strip()
            == "NATIVE " + native_platform(other), "alternate native code never executed")
    h.result["delivery"]["nativeAlternateCapacityVerified"] = True
    h.record("genuine late alternate-native capacity runs previously Pending native artifact",
             {"podUID": obj["metadata"]["uid"], "nodeUID": node["metadata"]["uid"],
              "platform": native_platform(other), "notProviderAutoscalerProof": True}, ["E2E-05"])
    h.delete("pod", "delivery-native-capacity", h.namespace)
    h.wait("alternate native task drained", lambda:
           h.get("pod", "delivery-native-capacity", h.namespace, absent=True) is None)
    h.delete("nodeprofile", delivery.native_profile)
    h.wait("alternate native profile completes production cleanup", lambda:
           h.get("nodeprofile", delivery.native_profile, absent=True) is None)
