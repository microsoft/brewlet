#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""One checkout-built install/provision/publish/admit/serve cycle for PR CI.

Uses the strict fixture's private cluster, registry, evidence and cleanup.
It retains that fixture's documented cold-start GC deferral; this is not
coverage of arbitrary containerd GC timing, HPA, or release compatibility.
"""

import json
import re

from checkout import CheckoutFixture
from common import retry_transient
from workflows import maven_app


APP = "pr-smoke"


def deploy(fixture):
    project = maven_app(fixture, "smoke-app", configuration="""
          <ports><port><name>http</name><containerPort>8080</containerPort></port></ports>
          <probes><readiness><path>/healthz</path><port>8080</port>
            <periodSeconds>2</periodSeconds><timeoutSeconds>2</timeoutSeconds>
          </readiness></probes>
        """)
    argv = [*fixture.maven_args, "-f", project / "pom.xml", "package",
            fixture.plugin + ":push", fixture.plugin + ":manifest",
            f"-Dbrewlet.image={fixture.registry}/apps/smoke:pr",
            f"-Dbrewlet.appName={APP}", f"-Dbrewlet.namespace={fixture.namespace}",
            "-Dbrewlet.jdkFeature=21", "-Dbrewlet.jdkDistribution=temurin",
            "-Dbrewlet.resources.cpuRequest=100m", "-Dbrewlet.resources.cpuLimit=500m",
            "-Dbrewlet.resources.memoryRequest=128Mi", "-Dbrewlet.resources.memoryLimit=256Mi"]
    result = retry_transient("smoke Maven dependency resolution", lambda: fixture.run(
        argv, check=False, timeout=600), resolution_only=True)
    fixture.save("maven-push-manifest.log", result.stdout + result.stderr)
    if result.returncode:
        raise RuntimeError(f"Maven publication/manifest failed ({result.returncode}); see maven-push-manifest.log")
    handoff = json.loads((project / "target/brewlet/push.json").read_text())
    image = handoff["deployImage"]
    if not re.fullmatch(re.escape(fixture.registry) + r"/apps/smoke@sha256:[a-f0-9]{64}", image):
        raise AssertionError("Maven publication did not record the expected immutable image")
    manifest = project / "target/brewlet/javaapplication.yaml"
    if not manifest.is_file():
        raise AssertionError("Maven manifest goal did not generate javaapplication.yaml")
    fixture.save("javaapplication.yaml", manifest.read_text())
    rendered = json.loads(fixture.kube("apply", "--dry-run=client", "-f", manifest, "-o", "json").stdout)
    if (rendered.get("apiVersion") != "apps.brewlet.sh/v1alpha1"
            or rendered.get("kind") != "JavaApplication"
            or rendered.get("metadata", {}).get("name") != APP
            or rendered.get("metadata", {}).get("namespace") != fixture.namespace):
        raise AssertionError("Generated manifest does not target the fixture-owned JavaApplication")
    spec = rendered["spec"]
    if spec["artifact"]["image"] != image:
        raise AssertionError("Generated manifest image differs from Maven publication")
    if (str(spec["jvm"]["version"]) != "21" or spec["jvm"]["distribution"] != "temurin"
            or spec["ports"] != [{"name": "http", "containerPort": 8080}]
            or spec["probes"]["readiness"]["httpGet"] != {"path": "/healthz", "port": 8080}):
        raise AssertionError("Generated manifest lost the fixture runtime, port, or readiness configuration")
    fixture.kube("apply", "-f", manifest, "-n", fixture.namespace)
    applied = fixture.get("javaapplication", APP, "-n", fixture.namespace)
    if applied["spec"]["artifact"]["image"] != image:
        raise AssertionError("Applied image differs from Maven publication")
    return image


def exercise(fixture):
    image = deploy(fixture)
    ready = fixture.run([fixture.cli, "k8s", "--kubeconfig", fixture.kubeconfig,
                         "--context", fixture.context, "app", "wait", APP,
                         "--namespace", fixture.namespace, "--wait-timeout", "5m"],
                        check=False, timeout=330)
    fixture.save("cli-app-wait.log", ready.stdout + ready.stderr)
    if ready.returncode:
        raise RuntimeError(f"CLI readiness failed ({ready.returncode}); see cli-app-wait.log")
    fixture.wait_ready(APP)
    pods = fixture.get("pods", "-n", fixture.namespace,
                       "-l", f"app.kubernetes.io/name={APP}")["items"]
    active = [pod for pod in pods if not pod["metadata"].get("deletionTimestamp")]
    if len(active) != 1:
        raise AssertionError("Expected exactly one serving smoke Pod")
    spec = active[0]["spec"]
    if (spec.get("runtimeClassName") != "brewlet"
            or spec.get("nodeName") != fixture.node
            or spec["containers"][0]["image"] != image):
        raise AssertionError("Smoke Pod did not execute the published image on the provisioned node")
    if "Hello from a JAR" not in fixture.service_get(APP):
        raise AssertionError("Smoke Service did not return the Java application's response")
    fixture.record("maven-push-manifest-kubectl-cli-workload-serving",
                   {"image": image, "node": fixture.node, "pod": active[0]["metadata"]["name"]})


def main():
    with CheckoutFixture("smoke") as fixture:
        exercise(fixture)


if __name__ == "__main__":
    main()
