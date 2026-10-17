"""Actual containerd events and worker metrics, joined to durable task leases."""

import json
import hashlib
import base64
import math
import re
import subprocess

from harness import IDS, LANGUAGES, ROOT, require


def metric_samples(text):
    samples = []
    decoder = json.JSONDecoder()
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        match = re.fullmatch(r'([A-Za-z_:][A-Za-z0-9_:]*)(?:\{(.*)\})? ([^ ]+)(?: [0-9]+)?', line)
        require(match is not None, "malformed Prometheus sample")
        labels, remaining = {}, match[2] or ""
        while remaining:
            name = re.match(r"([A-Za-z_][A-Za-z0-9_]*)=", remaining)
            require(name is not None and name[1] not in labels, "invalid or duplicate metric label")
            value, end = decoder.raw_decode(remaining[name.end():])
            require(isinstance(value, str), "metric label is not a string")
            labels[name[1]] = value
            remaining = remaining[name.end() + end:]
            require(not remaining or remaining.startswith(","), "invalid metric label separator")
            if remaining:
                remaining = remaining[1:]
                require(bool(remaining), "trailing metric label separator")
        samples.append((match[1], labels, float(match[3])))
    return samples


def sample_value(samples, name, labels):
    matches = [value for metric, actual, value in samples if metric == name and actual == labels]
    require(len(matches) == 1 and math.isfinite(matches[0]),
            "missing, duplicate or nonfinite metric: " + name + " " + json.dumps(labels))
    return matches[0]


def runtime_events(text):
    events = []
    for line in text.splitlines():
        if not line:
            continue
        envelope = json.loads(line)
        require(isinstance(envelope, dict) and envelope.get("namespace") == "k8s.io"
                and envelope.get("topic") == "/brewlet/runtime-image"
                and isinstance(envelope.get("event"), dict), "invalid runtime event envelope")
        events.append(envelope["event"])
    return events


def verify_binding(samples, events, lease, pod):
    runtime = lease["runtime"]
    labels = {"namespace": pod["metadata"]["namespace"], "pod": pod["metadata"]["name"],
              "pod_uid": pod["metadata"]["uid"], "container": "app", "container_id": lease["taskID"],
              "id": runtime["id"], "inventory_uid": runtime["inventoryUID"],
              "generation": str(runtime["generation"]), "digest": runtime["sourceDigest"]}
    require(sample_value(samples, "brewlet_container_runtime_image_info", labels) == 1,
            "live runtime info metric does not identify the actual task")
    expected = {"reason": "RuntimeImageResolved", "containerID": lease["taskID"], "id": runtime["id"],
                "inventoryUID": runtime["inventoryUID"], "generation": str(runtime["generation"]),
                "digest": runtime["sourceDigest"]}
    matches = [event for event in events if event.get("reason") == "RuntimeImageResolved"
               and event.get("containerID") == lease["taskID"]]
    require(matches == [expected], "start event missing, duplicated or different from the durable lease")
    return labels


class Observer:
    def __init__(self, h):
        self.h = h
        self.path = h.work / "runtime-events.log"
        self.binary_path = "/usr/local/bin/brewlet-event-observer-" + h.run_id
        self.pid_path = "/tmp/brewlet-event-observer-" + h.run_id + ".pid"
        self.process = None

    def __enter__(self):
        h = self.h
        h.node_snapshot()
        h.node_exec("true")
        binary = h.work / "event-observer"
        h.run(["go", "build", "-mod=readonly", "-o", binary,
               "./integration-tests/live-v2/eventobserver"],
              env={"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": h.arch}, timeout=180)
        h.node_exec("test", "!", "-e", self.binary_path)
        h.node_exec("/bin/sh", "-ec",
                   'umask 077; set -C; base64 -d > "$1"; chmod 0700 "$1"', "observer",
                   self.binary_path, input=base64.b64encode(binary.read_bytes()).decode(), timeout=120)
        require(h.node_exec("sha256sum", self.binary_path).stdout.split()[0]
                == hashlib.sha256(binary.read_bytes()).hexdigest(), "observer binary transfer changed bytes")
        log = self.path.open("w")
        command = ["docker", "exec", h.node_container, "/bin/sh", "-ec",
                   'test ! -e "$1"; printf "%s %s\\n" "$$" "$(awk \'{print $22}\' /proc/$$/stat)" > "$1"; '
                   'exec "$2"', "observer", self.pid_path, self.binary_path]
        self.process = subprocess.Popen(command, stdout=log, stderr=log, cwd=ROOT,
                                        start_new_session=True)
        h.helpers.append((self.process, log))
        h.wait("owned containerd event observer", lambda:
               self.process.poll() is None and h.node_exec("test", "-s", self.pid_path,
                                                           check=False).returncode == 0, timeout=30)
        return self

    def __exit__(self, *_):
        self.h.node_exec("/bin/sh", "-ec",
            'read -r pid started < "$1"; '
            'case "$pid:$started" in *[!0-9:]*|:*) exit 81;; esac; '
            'test "$(awk \'{print $22}\' /proc/$pid/stat)" = "$started"; '
            'test "$(readlink /proc/$pid/exe)" = "$2"; '
            'kill -TERM "$pid"; rm -- "$1" "$2"', "observer", self.pid_path, self.binary_path)
        self.process.communicate(timeout=15)
        require(self.process.returncode in (0, 143), "containerd event observer failed")

    def verify(self):
        h = self.h
        require(self.process.poll() is None, "containerd event observer exited before verification")
        response = h.node_exec("curl", "--fail", "--silent", "--show-error", "--noproxy", "*",
                               "--max-time", "15", "http://127.0.0.1:9090/metrics")
        (h.work / "runtime-metrics.txt").write_text(response.stdout)
        samples = metric_samples(response.stdout)
        events = runtime_events(self.path.read_text())
        bindings = []
        for language in LANGUAGES:
            for revision in ("old", "new"):
                pod = h.get("pod", language + "-" + revision, h.namespace)
                lease = h.lease(pod)
                bindings.append(verify_binding(samples, events, lease, pod))
            runtime_id = IDS[language]
            require(any(event.get("reason") == "RuntimeImageInstalling"
                        and bool(event.get("containerID"))
                        and event.get("id") == runtime_id for event in events),
                    "cold Create did not emit its actual installation wait event")
            require(sample_value(samples, "brewlet_runtime_image_generations",
                                 {"id": runtime_id, "state": "current"}) == 1
                    and sample_value(samples, "brewlet_runtime_image_generations",
                                     {"id": runtime_id, "state": "retired"}) >= 1,
                    "generation metrics do not retain the active older generation")
            require(sample_value(samples, "brewlet_runtime_image_install_requests_total",
                                 {"id": runtime_id, "outcome": "ready"}) >= 1
                    and sample_value(samples, "brewlet_runtime_image_install_duration_seconds_count",
                                     {"id": runtime_id}) >= 1,
                    "real on-demand installation lacks request/duration metrics")
        storage = {store: sample_value(samples, "brewlet_shared_storage_bytes", {"store": store})
                   for store in ("runtimes", "layers")}
        require(all(value > 0 for value in storage.values()), "shared storage measurements are absent")
        h.record("real initial/rotated task events and metrics retain exact runtime and Pod identities",
                 {"bindings": bindings, "storage": storage}, ["OBS-01", "OBS-02", "OBS-03"])
