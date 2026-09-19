#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Real resource-metrics CPU scaling. No replica writes or synthetic metrics."""

import json
import hashlib
import re
import time
from urllib.request import Request, urlopen

from common import Fixture, download, wait

METRICS_IMAGE = "registry.k8s.io/metrics-server/metrics-server@sha256:89258156d0e9af60403eafd44da9676fd66f600c7934d468ccc17e42b199aee2"
METRICS_MANIFEST_SHA = "ff64d1a13b9ac3b0635f0dd985815fb44c23eed4706c04e5db1daadf6bc0a83b"
APP = "cpu-demo"


def cpu_millicores(quantity):
    match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)(n|u|m|)?", quantity)
    if not match:
        raise ValueError(f"Unrecognized CPU quantity: {quantity}")
    return float(match[1]) * {"n": 0.000001, "u": 0.001, "m": 1, "": 1000}[match[2] or ""]


def hpa_cpu_utilization(status):
    if "currentCPUUtilizationPercentage" in status:
        return status["currentCPUUtilizationPercentage"]
    for metric in status.get("currentMetrics", []):
        if metric.get("resource", {}).get("name") == "cpu":
            return metric["resource"].get("current", {}).get("averageUtilization", 0)
    return 0


def install_metrics(fixture):
    manifest = fixture.private / "metrics-server.yaml"
    download("https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.8.0/components.yaml",
             manifest, METRICS_MANIFEST_SHA)
    # The exception is confined to this disposable kind fixture's self-signed kubelet.
    text = manifest.read_text()
    image = "registry.k8s.io/metrics-server/metrics-server:v0.8.0"
    if text.count(image) != 1 or text.count("        - --cert-dir=/tmp") != 1:
        raise RuntimeError("Pinned metrics manifest no longer matches fixture substitutions")
    text = text.replace(image, METRICS_IMAGE).replace(
        "        - --cert-dir=/tmp",
        "        - --cert-dir=/tmp\n        - --kubelet-insecure-tls")
    fixture.kube("apply", "-f", "-", input=text)
    fixture.kube("rollout", "status", "deployment/metrics-server", "-n", "kube-system",
                 "--timeout=180s")
    fixture.kube("wait", "--for=condition=Available", "apiservice/v1beta1.metrics.k8s.io",
                 "--timeout=180s")
    fixture.record("real-resource-metrics-server", {"image": METRICS_IMAGE,
                   "manifestSHA256": METRICS_MANIFEST_SHA,
                   "fixtureOnlyKubeletInsecureTLS": True,
                   "downscaleStabilizationSeconds": 60, "hpaSyncSeconds": 10})


class Scaling:
    def __init__(self, fixture):
        self.f = fixture
        self.samples = []
        self.requests = []
        self.loading = False
        self.loaded = set()
        self.load_failures = 0

    def pods(self):
        return [p for p in self.f.get("pods", "-n", self.f.namespace,
                "-l", f"app.kubernetes.io/name={APP}")["items"]
                if not p["metadata"].get("deletionTimestamp")]

    def load(self, seconds):
        successes = 0
        for pod in self.pods():
            if pod.get("status", {}).get("phase") != "Running":
                continue
            name = pod["metadata"]["name"]
            result = self.f.kube(
                "create", "--raw",
                f"/api/v1/namespaces/{self.f.namespace}/pods/{name}:8080/proxy/load?seconds={seconds}",
                "-f", "-", input="", check=False, timeout=50)
            self.requests.append({"time": time.time(), "kind": "load",
                                  "pod": name, "seconds": seconds,
                                  "success": result.returncode == 0,
                                  "response": result.stdout + result.stderr})
            if result.returncode == 0 and seconds:
                self.loaded.add(name)
                successes += 1
        if seconds:
            self.load_failures = 0 if successes else self.load_failures + 1
            if self.load_failures >= 3:
                self.f.save("requests.json", self.requests)
                raise AssertionError("CPU load failed for every running Pod three times")

    def metrics(self):
        result = self.f.kube("get", "--raw",
                            f"/apis/metrics.k8s.io/v1beta1/namespaces/{self.f.namespace}/pods",
                            check=False)
        if result.returncode:
            return None
        metrics = json.loads(result.stdout)
        names = {pod["metadata"]["name"] for pod in self.pods()}
        values = {pod["metadata"]["name"]:
                  sum(cpu_millicores(c["usage"]["cpu"]) for c in pod["containers"])
                  for pod in metrics["items"] if pod["metadata"]["name"] in names}
        if values.keys() != names or not names:
            return None
        return {"millicores": values, "raw": metrics}

    def sample(self, phase):
        if self.loading:
            self.load(20)
        hpa = self.f.get("hpa", APP, "-n", self.f.namespace)
        dep = self.f.get("deployment", APP, "-n", self.f.namespace)
        app = self.f.get("javaapplication", APP, "-n", self.f.namespace)
        result = self.f.kube("get", "--raw",
                            f"/api/v1/namespaces/{self.f.namespace}/services/{APP}:8080/proxy/hello",
                            check=False)
        serving = result.returncode == 0 and "Hello from a JAR" in result.stdout
        self.requests.append({"time": time.time(), "phase": phase, "kind": "serving",
                              "success": serving, "response": result.stdout + result.stderr})
        sample = {"time": time.time(), "phase": phase, "hpa": hpa["status"],
                  "desired": dep["spec"]["replicas"], "deployment": dep.get("status", {}),
                  "javaApplication": app.get("status", {}), "metrics": self.metrics(),
                  "serving": serving}
        if not 1 <= sample["desired"] <= 3:
            raise AssertionError(f"HPA replica count outside [1,3]: {sample['desired']}")
        self.samples.append(sample)
        self.f.save("scaling-samples.json", self.samples)
        self.f.save("requests.json", self.requests)
        return sample

    def idle(self, phase):
        sample = self.sample(phase)
        metrics = sample["metrics"]
        return (sample["desired"] == 1 and metrics is not None and
                max(metrics["millicores"].values()) < 50 and sample["serving"] and
                self.f.ready(APP, 1))

    def observe_source_gc(self, image):
        repository, digest = image.split("/", 1)[1].split("@")

        def manifest(digest):
            request = Request(f"http://{self.f.registry}/v2/{repository}/manifests/{digest}",
                              headers={"Accept": "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json"})
            with urlopen(request, timeout=10) as response:
                raw = response.read()
            if "sha256:" + hashlib.sha256(raw).hexdigest() != digest:
                raise AssertionError("Registry returned a mismatched manifest")
            return json.loads(raw)

        index = manifest(digest)
        matches = [entry for entry in index["manifests"]
                   if entry.get("platform", {}).get("architecture") == self.f.arch]
        if len(matches) != 1:
            raise AssertionError("Expected one platform manifest for the fixture node")
        selected = matches[0]["digest"]
        layers = [entry["digest"] for entry in manifest(selected)["layers"]]
        if not layers or any(not re.fullmatch(r"sha256:[a-f0-9]{64}", d) for d in layers):
            raise AssertionError("Invalid runnable layer descriptors")
        self.f.own_container(self.f.node)

        def collected():
            return all(self.f.run([
                "docker", "exec", self.f.node_id, "test", "!", "-e",
                "/var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/" + d[7:]],
                check=False).returncode == 0 for d in layers)
        wait("containerd naturally discards unpacked source layers", collected, timeout=180)
        if self.f.candidate == "shim":
            for layer in layers:
                path = (f"/tmp/brewlet-runnable/immutable-v2/{selected[7:]}/content/"
                        f"blobs/sha256/{layer[7:]}")
                result = self.f.run(["docker", "exec", self.f.node_id, "sha256sum", path])
                if result.stdout.split()[0] != layer[7:]:
                    raise AssertionError("Retained layer no longer matches its descriptor")
        self.f.record("post-unpack-source-gc-observed",
                      {"platformManifest": selected, "missingSourceLayers": layers,
                       "retainedBytesVerified": self.f.candidate == "shim"})

    def up(self):
        sample = self.sample("scale-up")
        metrics = sample["metrics"]
        hpa = sample["hpa"]
        return (sample["desired"] > 1 and metrics is not None and
                min(metrics["millicores"].values()) > 50 and
                hpa_cpu_utilization(hpa) > 50 and
                hpa.get("desiredReplicas") == sample["desired"] and sample["serving"] and
                self.f.ready(APP, sample["desired"]))

    def reconcile_ownership(self):
        for cycle in range(3):
            # Change a real spec field ignored while HPA is enabled. ObservedGeneration
            # then proves the operator reconciled, rather than merely waiting.
            stale = 7 + cycle
            self.f.kube("patch", "javaapplication", APP, "-n", self.f.namespace,
                        "--type=merge", "-p", json.dumps({"spec": {"replicas": stale}}))
            generation = self.f.get("javaapplication", APP, "-n", self.f.namespace)["metadata"]["generation"]

            def reconciled():
                sample = self.sample("ownership")
                return (sample["javaApplication"].get("observedGeneration") == generation and
                        sample["desired"] > 1 and self.f.ready(APP, sample["desired"]))
            wait(f"HPA ownership after reconciliation {cycle + 1}", reconciled,
                 timeout=120, interval=5)
        self.f.record("operator-preserves-live-hpa-replicas", {"cycles": 3,
                       "javaApplicationReplicas": [7, 8, 9], "hpaBounds": [1, 3]})

    def execute(self, image):
        try:
            wait("usable pod CPU metrics", self.metrics, timeout=180)
            wait("idle minimum, low real CPU, serving and endpoints", lambda: self.idle("baseline"),
                 timeout=420, interval=5)
            self.f.record("metrics-and-idle-preflight", self.samples[-1])
            self.observe_source_gc(image)
            self.loading = True
            wait("real CPU causes HPA scale-up and Ready Brewlet pods", self.up,
                 timeout=600, interval=5)
            if len(self.loaded) < 2:
                raise AssertionError("Additional Brewlet pods did not accept load")
            self.f.record("cpu-driven-scale-up", self.samples[-1])
            self.reconcile_ownership()
            self.loading = False
            self.load(0)
            wait("real CPU falls and HPA returns to minimum", lambda: self.idle("scale-down"),
                 timeout=420, interval=5)
            self.f.record("cpu-driven-scale-down", self.samples[-1])
            for phase in ("scale-up", "scale-down"):
                requests = [r for r in self.requests if r.get("phase") == phase]
                self.f.record(f"{phase}-serving-outcomes",
                               {"attempts": len(requests),
                                "successes": sum(r["success"] for r in requests),
                                "failures": sum(not r["success"] for r in requests)})
        finally:
            self.loading = False
            self.load(0)
            self.f.save("requests.json", self.requests)


def main():
    with Fixture("hpa") as fixture:
        install_metrics(fixture)
        capacity = fixture.get("node", fixture.node)["status"]["allocatable"]
        if cpu_millicores(capacity["cpu"]) < 3000:
            raise RuntimeError("Insufficient node CPU for the 3x500m bounded workload")
        fixture.record("capacity-preflight", capacity)
        image = fixture.publish_demo()
        fixture.java_application(APP, image, autoscaling=True)
        wait("operator creates Deployment",
             lambda: fixture.kube("get", "deployment", APP, "-n", fixture.namespace,
                                  check=False).returncode == 0)
        fixture.wait_ready(APP)
        if "Hello from a JAR" not in fixture.service_get(APP):
            raise AssertionError("Ready workload did not serve through its Service")
        fixture.record("initial-serving-brewlet-workload", {"image": image})
        Scaling(fixture).execute(image)


if __name__ == "__main__":
    main()
