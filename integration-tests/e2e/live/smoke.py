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
    project = maven_app(fixture, "smoke-app")
    argv = [*fixture.maven_args, "-f", project / "pom.xml", "package",
            fixture.plugin + ":deploy",
            f"-Dbrewlet.image={fixture.registry}/apps/smoke:pr",
            f"-Dbrewlet.kubeconfig={fixture.kubeconfig}",
            f"-Dbrewlet.kubeContext={fixture.context}",
            f"-Dbrewlet.namespace={fixture.namespace}", f"-Dbrewlet.appName={APP}",
            "-Dbrewlet.jdkFeature=21", "-Dbrewlet.readinessPath=/healthz",
            "-Dbrewlet.resources.cpuRequest=100m", "-Dbrewlet.resources.cpuLimit=500m",
            "-Dbrewlet.resources.memoryRequest=128Mi", "-Dbrewlet.resources.memoryLimit=256Mi"]
    result = retry_transient("smoke Maven dependency resolution", lambda: fixture.run(
        argv, check=False, timeout=600), resolution_only=True)
    fixture.save("maven-deploy.log", result.stdout + result.stderr)
    if result.returncode:
        raise RuntimeError(f"Maven deployment failed ({result.returncode}); see maven-deploy.log")
    handoff = json.loads((project / "target/brewlet/push.json").read_text())
    image = handoff["deployImage"]
    if not re.fullmatch(re.escape(fixture.registry) + r"/apps/smoke@sha256:[a-f0-9]{64}", image):
        raise AssertionError("Maven deployment did not record the expected immutable image")
    manifest = (project / "target/brewlet/javaapplication.yaml").read_text()
    applied = fixture.get("javaapplication", APP, "-n", fixture.namespace)
    if image not in manifest or applied["spec"]["artifact"]["image"] != image:
        raise AssertionError("Maven manifest/applied image differs from its publication")
    return image


def exercise(fixture):
    image = deploy(fixture)
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
    fixture.record("maven-deploy-workload-serving", {"image": image, "node": fixture.node,
                                                    "pod": active[0]["metadata"]["name"]})


def main():
    with CheckoutFixture("smoke") as fixture:
        exercise(fixture)


if __name__ == "__main__":
    main()
