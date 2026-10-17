"""Actual authenticated TLS runtime-layer transfer gate; no protected-state edits."""

import hashlib
import http.server
import json
from pathlib import Path
import re
import shutil
import ssl
import sys
import time
import urllib.error
import urllib.request


def gated_path(repository, digest):
    if not re.fullmatch(r"[a-z0-9]+(?:[._/-][a-z0-9]+)*", repository):
        raise ValueError("invalid gate repository")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise ValueError("invalid gate blob digest")
    return "/v2/" + repository + "/blobs/" + digest


def serve(upstream, authority, directory, certificates):
    """Only forward to the explicit registry; TLS verification stays enabled."""
    directory, certificates = Path(directory), Path(certificates)
    context = ssl.create_default_context(cafile=str(certificates / "ca.crt"))

    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            raise RuntimeError("gate upstream redirect refused")

    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=context), NoRedirect())

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        def do_HEAD(self):
            self.forward()

        def do_GET(self):
            self.forward()

        def forward(self):
            if not self.path.startswith("/v2/") or "?" in self.path or "#" in self.path:
                self.send_error(400)
                return
            request = urllib.request.Request("https://" + upstream + self.path,
                method=self.command, headers={k: self.headers[k] for k in
                    ("Authorization", "Accept", "Range") if self.headers.get(k)})
            try:
                response = opener.open(request, timeout=180)
            except urllib.error.HTTPError as error:
                response = error
            except (urllib.error.URLError, TimeoutError) as error:
                print(json.dumps({"event": "gate-upstream-error",
                                  "type": type(error).__name__}), flush=True)
                self.send_error(502)
                return
            with response:
                self.send_response(response.status)
                for key in ("Content-Length", "Content-Type", "Docker-Content-Digest",
                            "WWW-Authenticate", "Content-Range", "Accept-Ranges"):
                    if response.headers.get(key):
                        self.send_header(key, response.headers[key])
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.flush()
                self.close_connection = True
                if self.command == "HEAD":
                    return
                if response.status == 200 and self.path == (directory / "path").read_text():
                    started = directory / "started"
                    started.touch(exist_ok=True)
                    deadline = time.monotonic() + 180
                    while not (directory / "release").exists():
                        if time.monotonic() >= deadline:
                            print('{"event":"gate-hold-deadline"}', flush=True)
                            return
                        time.sleep(0.1)
                try:
                    shutil.copyfileobj(response, self.wfile, length=65536)
                except (BrokenPipeError, ConnectionResetError):
                    print('{"event":"gate-client-disconnected"}', flush=True)

    server = http.server.ThreadingHTTPServer(("0.0.0.0", int(authority.rsplit(":", 1)[1])), Handler)
    server.daemon_threads = True
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(certificates / "tls.crt", certificates / "tls.key")
    server.socket = tls.wrap_socket(server.socket, server_side=True)
    (directory / "ready").touch()
    server.serve_forever()


class TransferGate:
    def __init__(self, h):
        from delivery import private_file
        self.h, self.delivery = h, h.delivery
        self.work = h.work / "transfer-gate"
        self.work.mkdir(mode=0o700)
        self.authority = self.delivery.authority.rsplit(":", 1)[0] + ":5001"
        self.container = None
        self.repository = "delivery/cold-runtime"
        self.layer = None
        private_file(self.work / "path", "/not-armed")

    def publish_runtime(self):
        from delivery import (CONFIG, LAYER, MANIFEST, blob, finish_layout, json_blob,
                              private_file, read_blob, tar_layer)
        layout = self.work / "runtime"
        shutil.copytree(self.delivery.work / "python-after-golden", layout)
        selected = json.loads((layout / "index.json").read_text())["manifests"][0]
        manifest = json.loads(read_blob(layout, selected))
        config = json.loads(read_blob(layout, manifest["config"]))
        payload, diff = tar_layer([("usr/share/brewlet-cold-" + self.h.run_id,
                                    self.h.run_id.encode(), 0o644)])
        self.layer = blob(layout, payload, LAYER)
        manifest["layers"].append(self.layer)
        config["rootfs"]["diff_ids"].append(diff)
        manifest["config"] = json_blob(layout, config, CONFIG)
        selected = finish_layout(layout, json_blob(layout, manifest, MANIFEST))
        original = self.delivery.publish(layout, selected, self.repository)
        private_file(self.work / "path", gated_path(self.repository, self.layer["digest"]))
        return self.authority + original[len(self.delivery.authority):]

    def start(self):
        from harness import CLI_USERLAND, OWNER, docker_owned, require
        h = self.h
        name = h.cluster + "-transfer-gate"
        require(h.run(["docker", "inspect", name], check=False).returncode != 0,
                "transfer gate container name is occupied")
        command = ["docker", "create", "--platform", "linux/" + h.arch, "--name", name,
                   "--network", "container:" + self.delivery.registry_id,
                   "--label", OWNER + "=" + h.run_id, "--read-only", "--tmpfs", "/tmp",
                   "--mount", f"type=bind,source={self.delivery.work},target=/delivery,readonly",
                   "--mount", f"type=bind,source={self.work},target=/gate",
                   "--mount", f"type=bind,source={Path(__file__).resolve()},target=/transfer_gate.py,readonly",
                   "--entrypoint", "python3", CLI_USERLAND, "/transfer_gate.py", "--serve",
                   self.delivery.authority, self.authority, "/gate", "/delivery"]
        try:
            h.run(command)
        finally:
            observed = h.run(["docker", "inspect", name], check=False)
            if observed.returncode == 0:
                info = json.loads(observed.stdout)[0]
                docker_owned(info, h.run_id, info["Id"])
                self.container = info["Id"]
        require(self.container is not None, "transfer gate container was not created")
        h.run(["docker", "start", self.container])
        h.wait("TLS transfer gate bound its socket", lambda:
               h.run(["docker", "exec", self.container, "test", "-f", "/gate/ready"],
                     check=False).returncode == 0, timeout=30)
        challenge = (
            "import ssl,urllib.request,urllib.error\n"
            "try:\n"
            f" urllib.request.urlopen('https://{self.authority}/v2/',"
            "context=ssl.create_default_context(cafile='/delivery/ca.crt'),timeout=10)\n"
            "except urllib.error.HTTPError as e:\n"
            " assert e.code==401 and 'basic' in e.headers['WWW-Authenticate'].lower()\n"
            "else:\n raise SystemExit('proxy bypassed registry authentication')\n")
        h.run(["docker", "exec", self.container, "python3", "-c", challenge])

    def trust_on(self, node):
        from delivery import USERNAME
        from harness import require
        require(self.authority == self.delivery.authority.rsplit(":", 1)[0] + ":5001",
                "transfer trust must target the exact owned registry network")
        directory = "/etc/containerd/certs.d/" + self.authority
        files = {
            "ca.crt": (self.delivery.work / "ca.crt").read_text(),
            "hosts.toml": (f'server = "https://{self.authority}"\n'
                           f'[host."https://{self.authority}"]\n'
                           '  capabilities = ["pull", "resolve"]\n  ca = "ca.crt"\n'),
            "credentials.json": json.dumps({"username": USERNAME, "password": self.delivery.password}),
        }
        for name, data in files.items():
            self.h.node_exec_on(node, "mkdir", "-p", directory)
            self.h.node_exec_on(node, "chmod", "0700", directory)
            self.h.node_exec_on(node, "sh", "-ec", 'umask 077; cat > "$1"', "transfer",
                                directory + "/" + name, input=data)
            observed = self.h.node_exec_on(node, "sha256sum", directory + "/" + name).stdout.split()
            require(observed and observed[0] == hashlib.sha256(data.encode()).hexdigest(),
                    "transfer-gate registry file readback mismatch")

    def started(self):
        return (self.work / "started").exists()

    def release(self):
        (self.work / "release").touch()

    def close(self):
        from harness import docker_owned
        self.release()
        if self.container is not None:
            info = json.loads(self.h.run(["docker", "inspect", self.container]).stdout)[0]
            docker_owned(info, self.h.run_id, self.container)
            logs = self.h.run(["docker", "logs", self.container], check=False)
            (self.work / "proxy.log").write_text(logs.stdout + logs.stderr)
            self.h.run(["docker", "rm", "-f", "-v", self.container])
            self.container = None


if __name__ == "__main__":
    if len(sys.argv) != 6 or sys.argv[1] != "--serve":
        raise SystemExit("only the explicit owned-container --serve entrypoint is supported")
    serve(*sys.argv[2:])
