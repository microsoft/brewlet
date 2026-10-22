#!/usr/bin/env python3
"""Specification §16 on unchanged roots: real dependencies, composition and rollout.

Fixture libraries demonstrate an observable compatible update, not an actual
production CVE. Vulnerability matching remains external to Brewlet.
"""

import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import threading
import time
import xml.etree.ElementTree as ET

import delivery
from harness import API, FIXTURES, IDS, OWNER, ROOT, STORE, Failure, Harness, require, require_owned
from upstream import EXECUTABLES, PINS, UpstreamDelivery


def complete_lock(directory, destination, ecosystem, version):
    entries = []
    for path in sorted(directory.rglob("*")):
        if path.is_dir():
            continue
        require(path.is_file() and not path.is_symlink(), "managed fixture inputs must be regular")
        relative = path.relative_to(directory).as_posix()
        purl = {"java": "pkg:maven/sh.brewlet.fixture/greeting",
                "python": "pkg:pypi/brewlet-fixture-greeting",
                "node": "pkg:npm/brewlet-fixture-greeting",
                "dotnet": "pkg:nuget/Greeting"}[ecosystem]
        entries.append({"ecosystem": ecosystem, "name": "greeting", "version": version,
                        "path": destination.rstrip("/") + "/" + relative,
                        "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
                        "purl": purl + "@" + version})
    require(entries, "empty dependency closure")
    return {"schemaVersion": 1, "entries": entries}


def replace_fixture_message(directory, suffix):
    changed = 0
    for path in sorted(directory.rglob("*")):
        if path.is_file() and path.suffix == suffix and "obj" not in path.parts:
            original = path.read_text()
            if "shared-contract" in original:
                path.write_text(original.replace("shared-contract", "shared-contract-patched"))
                changed += 1
    require(changed == 1, "expected one explicit greeting implementation")


def fixture_patch(h, language, source):
    target = h.work / ("patched-" + language)
    target.mkdir()
    if language in ("python", "node"):
        for path in source.iterdir():
            require(path.is_file() and not path.is_symlink(), "unexpected fixture dependency entry")
            shutil.copyfile(path, target / path.name)
        replace_fixture_message(target, ".py" if language == "python" else ".mjs")
    elif language == "java":
        project = h.work / "patched-java-project"
        shutil.copytree(h.work / "fixtures/java/dependency", project,
                        ignore=shutil.ignore_patterns("target"))
        replace_fixture_message(project / "src", ".java")
        pom = project / "pom.xml"
        tree = ET.parse(pom)
        tree.getroot().find("{http://maven.apache.org/POM/4.0.0}version").text = "2.0.1"
        ET.register_namespace("", "http://maven.apache.org/POM/4.0.0")
        ET.register_namespace("xsi", "http://www.w3.org/2001/XMLSchema-instance")
        tree.write(pom, encoding="utf-8", xml_declaration=True)
        h.run(["mvn", "-B", "-q", "-f", pom, "package"], timeout=300)
        shutil.copyfile(project / "target/greeting-2.0.1.jar", target / "greeting-2.0.1.jar")
    else:
        project = h.work / "patched-dotnet-project"
        shutil.copytree(FIXTURES / "dotnet/Greeting", project,
                        ignore=shutil.ignore_patterns("bin", "obj"))
        replace_fixture_message(project, ".cs")
        csproj = project / "Greeting.csproj"
        tree = ET.parse(csproj)
        ET.SubElement(tree.getroot().find("PropertyGroup"), "Version").text = "2.0.1"
        tree.write(csproj, encoding="utf-8", xml_declaration=True)
        output = h.work / "patched-dotnet-output"
        h.run(["dotnet", "build", csproj, "--configuration", "Release", "--output", output,
               "--nologo"], timeout=300)
        shutil.copyfile(output / "Greeting.dll", target / "Greeting.dll")
    return target


def create_release_policy(h):
    now = time.time()
    stamp = lambda value: time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(value))
    authority = {}
    for role in ("publisher", "composer"):
        key, public = (h.work / (role + suffix) for suffix in (".key", ".pub"))
        h.run(["openssl", "genpkey", "-algorithm", "EC", "-pkeyopt",
               "ec_paramgen_curve:P-256", "-out", key])
        os.chmod(key, 0o600)
        h.run(["openssl", "pkey", "-in", key, "-pubout", "-out", public])
        authority[role] = {"identity": "https://ci.brewlet.test/" + role,
            "keys": [{"publicKey": public.read_text(), "notBefore": stamp(now - 60),
                      "notAfter": stamp(now + 14400)}]}
    policy = h.work / "release-policy.json"
    policy.write_text(json.dumps({"schemaVersion": 1, "mode": "production",
        "notAfter": stamp(now + 14400), **authority}, indent=2))
    return policy, authority


def compose_layout(h, application, dependency, policy, name):
    layout, receipt = h.work / (name + "-composed"), h.work / (name + "-receipt.json")
    h.run([*h.cli_command, "compose", name, "--store", layout,
           "--application", application, "--layer-set", dependency,
           "--platform", "linux/" + h.arch, "--policy", policy,
           "--signing-key", h.work / "composer.key",
           "--builder-identity", "https://ci.brewlet.test/composer",
           "--push-result", receipt], timeout=300)
    return {"layout": layout, "target": json.loads((layout / "index.json").read_text())["manifests"][0],
            "receipt": json.loads(receipt.read_text())}


class ManagedDelivery(UpstreamDelivery):
    def __init__(self, h, pins):
        super().__init__(h, pins)
        self.compositions = {}
        self.sources = {}

    def pod(self, name, language, **kwargs):
        pod = super().pod(name, language, **kwargs)
        pod["spec"]["terminationGracePeriodSeconds"] = 15
        pod["spec"]["containers"][0]["lifecycle"] = {"preStop": {"sleep": {"seconds": 5}}}
        if language == "dotnet":
            # Exec readiness starts a second real runtime inside the same limit.
            pod["spec"]["containers"][0]["resources"]["limits"]["memory"] = "768Mi"
        return pod

    def bootstrap(self):
        h = self.h
        self.start_registry()
        self.configure_node()
        self.policy, authority = create_release_policy(h)
        inputs = json.loads((h.work / "host-reports.json").read_text())["layers"]
        for language in h.languages:
            root, selected = self.golden_layout(language, "before")
            remote = self.publish(root, selected, "managed/runtime-" + language)
            h.runtime_images[language] = {"before": remote, "after": remote}
            app_layout = h.work / ("managed-payload-" + language)
            command = [*h.cli_command, "push", "payload-" + language, "--store", app_layout,
                       "--platform", "linux/" + h.arch, "--launch", h.launch_file(language)]
            dependency = None
            for role, source, destination in inputs[language]:
                if role == "dependency":
                    require(dependency is None, "one complete dependency closure expected")
                    dependency = Path(source), destination
                else:
                    command += ["--layer", f"app={source}:{destination}"]
            require(dependency, "missing real dependency fixture")
            h.run(command, timeout=180)
            app = json.loads((app_layout / "index.json").read_text())["manifests"][0]
            self.sources[language] = self.publish(app_layout, app, "managed/payload-" + language)
            self.compositions[language] = {}
            for revision, version in (("before", "2.0.0"), ("after", "2.0.1")):
                directory = dependency[0] if revision == "before" else fixture_patch(h, language, dependency[0])
                lock = h.work / (language + "-" + revision + "-lock.json")
                package_version = "1.0.0" if language == "dotnet" and revision == "before" else version
                lock.write_text(json.dumps(complete_lock(directory, dependency[1], language, package_version)))
                config = h.work / (language + "-" + revision + "-set.json")
                config.write_text(json.dumps({"schemaVersion": 1, "name": "approved-" + language,
                    "version": version, "ecosystem": language}))
                layout = h.work / (language + "-" + revision + "-bundle")
                h.run([*h.cli_command, "layer-set", "approved-" + language, "--store", layout,
                       "--config", config, "--lock", lock, "--layer", f"dependency={directory}:{dependency[1]}",
                       "--signing-key", h.work / "publisher.key",
                       "--builder-identity", authority["publisher"]["identity"]], timeout=180)
                target = json.loads((layout / "index.json").read_text())["manifests"][0]
                bundle = self.publish(layout, target, f"managed/{language}-bundle-{revision}")
                application = (str(app_layout) + "@" + app["digest"] if revision == "before"
                    else str(self.compositions[language]["before"]["layout"]) + "@"
                         + self.compositions[language]["before"]["target"]["digest"])
                composed = compose_layout(h, application, str(layout) + "@" + target["digest"],
                                          self.policy, language + "-" + revision)
                reference = self.publish(composed["layout"], composed["target"], f"managed/{language}-{revision}")
                self.compositions[language][revision] = {**composed, "reference": reference, "bundle": bundle}
            before, after = (self.compositions[language][r]["receipt"]["verification"]["predicate"]
                             for r in ("before", "after"))
            require(before["applicationLayers"] == after["applicationLayers"]
                    and before["effectiveLaunch"] == after["effectiveLaunch"]
                    and before["dependencyEvidence"]["layers"] != after["dependencyEvidence"]["layers"]
                    and after["applicationRoot"] == before["applicationRoot"]
                    and bool(after["parentComposition"]), "composition rewrote application or lost history")
            h.app_images[language] = self.compositions[language]["before"]["reference"]
            h.record(language + " independent signed bundle, immutable application and verified replacement",
                {"application": self.sources[language], "before": before["output"], "after": after["output"],
                 "appLayers": before["applicationLayers"], "bundle": self.compositions[language]["before"]["bundle"]},
                ["COMP-01", "COMP-02", "COMP-03"])
        h.pod = self.pod
        h.result["applicationImages"] = h.app_images
        h.result["runtimeImages"] = h.runtime_images
        h.result["managedComposition"] = {"libraries": "explicit local fixture packages, not real CVE remediation",
                                        "runtimeImagesUnmodified": True}
        h.save()


def deployment(h, language, revision="before"):
    pod = h.pod("template", language)
    pod["spec"].pop("restartPolicy", None)
    pod["spec"]["containers"][0]["image"] = h.delivery.compositions[language][revision]["reference"]
    labels = {OWNER: h.run_id, "managed.brewlet.test/app": language}
    return {"apiVersion": "apps/v1", "kind": "Deployment",
        "metadata": {"name": "managed-" + language, "namespace": h.namespace},
        "spec": {"replicas": 2, "strategy": {"type": "RollingUpdate",
            "rollingUpdate": {"maxUnavailable": 0, "maxSurge": 1}},
            "selector": {"matchLabels": {"managed.brewlet.test/app": language}},
            "template": {"metadata": {"labels": labels, "annotations": pod["metadata"]["annotations"]},
                         "spec": pod["spec"]}}}


def ready_pods(h, language, reference):
    pods = h.get("pods", namespace=h.namespace)["items"]
    selected = []
    for pod in pods:
        if pod["metadata"].get("labels", {}).get("managed.brewlet.test/app") != language:
            continue
        require_owned(pod, h.run_id)
        if pod["spec"]["containers"][0]["image"] != reference or pod["metadata"].get("deletionTimestamp"):
            continue
        h.owned[("pod", h.namespace, pod["metadata"]["name"])] = pod["metadata"]["uid"]
        if h.ready(pod["metadata"]["name"]):
            selected.append(pod)
    return selected if len(selected) == 2 else False


def check_execution(h, pod, language, expected):
    name = pod["metadata"]["name"]
    report = h.http(name)
    require(report["dependency"] == expected, "actual dependency implementation was not loaded")
    lease = h.lease(pod)
    adjusted = dict(report, dependency="shared-contract")
    h.assert_report(adjusted, language, arguments=[], cwd="/app", home=lease["runtime"]["home"])
    probe = json.loads(h.exec(name, h.probe(language)).stdout)
    require(probe["dependency"] == expected, "Exec used a different dependency implementation")
    h.assert_report(dict(probe, dependency="shared-contract"), language,
                    arguments=["--probe", "two words"], cwd="/app", home=lease["runtime"]["home"])
    runtime = lease["runtime"]
    require(runtime["sourceDigest"] == h.delivery.metadata[language, "before"]["descriptor"]["digest"],
            "approved runtime root was derived or rewritten")
    task = lease["taskID"]
    spec = json.loads(h.node_exec("cat", "/run/containerd/io.containerd.runtime.v2.task/k8s.io/"
                                 + task + "/config.json").stdout)
    require(spec["process"]["user"]["uid"] == 65532 and spec["process"]["user"]["gid"] == 65532
            and not spec["process"].get("capabilities", {}).get("effective"),
            "actual runc process escaped non-root/empty-capability contract")
    require(any(m["destination"] == "/app" and "ro" in m["options"] for m in spec["mounts"]),
            "actual application boundary is not read-only")
    require(bool(spec["linux"]["resources"].get("memory")) and bool(spec["linux"]["resources"].get("cpu")),
            "actual runc resource configuration is absent")
    return {"podUID": pod["metadata"]["uid"], "lease": lease, "report": report,
            "readOnlyApp": True, "resourcesObserved": True}


def inventory_snapshot(h, name):
    expected = {status["containerID"].removeprefix("containerd://")
                for pod in h.get("pods", namespace=h.namespace)["items"]
                for status in pod.get("status", {}).get("containerStatuses", [])
                if status.get("state", {}).get("running")}
    def observed():
        snapshot = json.loads(h.node_snapshot()["metadata"].get("annotations", {}).get(
            "brewlet.sh/runtime-bindings", "{}"))
        current = {binding["containerID"] for binding in snapshot.get("bindings", [])
                   if binding.get("application")}
        return snapshot.get("complete") and expected <= current
    h.wait("protected application binding publication before " + name, observed)
    # Use the owned kind network, its actual API IP and its original client
    # credentials. The export never needs kubelet/host write permissions.
    config = json.loads(h.delivery.private_run(["kubectl", "--kubeconfig", h.kubeconfig,
        "--context", h.context, "config", "view", "--minify", "--raw", "-o", "json"]).stdout)
    node = json.loads(h.run(["docker", "inspect", h.node_container]).stdout)[0]
    address = node["NetworkSettings"]["Networks"]["kind"]["IPAddress"]
    config["clusters"][0]["cluster"]["server"] = "https://" + address + ":6443"
    kubeconfig = delivery.private_file(h.work / "observer-kubeconfig", json.dumps(config))
    report = h.work / (name + "-inventory.json")
    command = h.delivery.producer()
    if command[:2] == ["docker", "run"]:
        at = command.index("--network")
        command[at + 1] = "kind"
        command[2:2] = ["--env", "BREWLET_REGISTRY_ORIGIN=https://" + h.delivery.authority]
    command += ["inventory", "--kubeconfig", str(kubeconfig), "--context", h.context,
        "--namespace", h.namespace, "--policy", str(h.delivery.policy),
        "--registry-https", "--output", str(report)]
    h.delivery.run_producer(command, env={"SSL_CERT_FILE": str(h.delivery.work / "ca.crt"),
        "BREWLET_REGISTRY_USERNAME": delivery.USERNAME, "BREWLET_REGISTRY_PASSWORD": h.delivery.password,
        "BREWLET_REGISTRY_ORIGIN": "https://" + h.delivery.authority})
    result = json.loads(report.read_text())
    require(result["complete"] and result["containers"] and result["artifacts"],
            "external-scanner inventory could not verify actual application graphs")
    for row in result["containers"]:
        require(row["status"] == "verified" and row["binding"]["application"]["manifestDigest"],
                "unverified row was presented as a known graph")
    return result


def exercise(h):
    old = {}
    for language in h.languages:
        h.apply(deployment(h, language))
        reference = h.delivery.compositions[language]["before"]["reference"]
        pods = h.wait(language + " managed Deployment ready", lambda: ready_pods(h, language, reference))
        old[language] = [check_execution(h, p, language, "shared-contract") for p in pods]
        h.apply({"apiVersion": "v1", "kind": "Service", "metadata": {
            "name": "managed-" + language, "namespace": h.namespace},
            "spec": {"selector": {"managed.brewlet.test/app": language},
                     "ports": [{"port": 8000, "targetPort": 8000}]}})
        h.record(language + " composed managed dependencies on unchanged upstream root with real HTTP/Exec",
                 old[language], ["COMP-04"])
    before_inventory = inventory_snapshot(h, "before")
    require(len(before_inventory["artifacts"]) == 4
            and all(row["workload"]["kind"] == "Deployment" for row in before_inventory["containers"]),
            "shared packages/Deployment owners were not attributed through actual images")
    h.record("read-only fleet export follows actual image identities and native owner UIDs for external scanning",
        {"containers": len(before_inventory["containers"]), "artifacts": len(before_inventory["artifacts"]),
         "namespaces": before_inventory["namespaces"]}, ["COMP-08", "COMP-16"])
    for language in h.languages:
        candidate = h.pod("candidate-" + language, language)
        candidate["spec"]["containers"][0]["image"] = h.delivery.compositions[language]["after"]["reference"]
        h.apply(candidate)
        pod = h.wait(language + " compatibility candidate ready", lambda: h.ready("candidate-" + language))
        check_execution(h, pod, language, "shared-contract-patched")
        current = h.get("deployment", "managed-" + language, h.namespace)
        require(current["spec"]["template"]["spec"]["containers"][0]["image"] ==
                h.delivery.compositions[language]["before"]["reference"],
                "candidate publication deployed before compatibility approval")
        h.record(language + " separate candidate compatibility gate before workload update",
                 {"candidateUID": pod["metadata"]["uid"], "originalDeploymentUID": current["metadata"]["uid"]},
                 ["COMP-05"])
        service = h.get("service", "managed-" + language, h.namespace)["spec"]["clusterIP"]
        stopped, errors, responses = threading.Event(), [], []
        def poll():
            while not stopped.is_set():
                response = h.node_exec("curl", "--fail", "--silent", "--show-error", "--noproxy", "*",
                    "--max-time", "5", "http://" + service + ":8000/", check=False, timeout=10)
                if response.returncode:
                    errors.append("HTTP request failed")
                else:
                    try:
                        responses.append(json.loads(response.stdout)["dependency"])
                    except (ValueError, KeyError):
                        errors.append("invalid HTTP response")
                stopped.wait(0.1)
        thread = threading.Thread(target=poll, daemon=True)
        thread.start()
        try:
            time.sleep(2)
            h.apply(deployment(h, language, "after"))
            reference = h.delivery.compositions[language]["after"]["reference"]
            pods = h.wait(language + " patched Deployment ready", lambda: ready_pods(h, language, reference))
            def drained():
                current = h.get("pods", namespace=h.namespace)["items"]
                return not any(p["metadata"].get("labels", {}).get("managed.brewlet.test/app") == language
                    and p["spec"]["containers"][0]["image"] != reference for p in current)
            h.wait(language + " actual old image cohorts drained", drained)
            time.sleep(2)
        finally:
            stopped.set()
            thread.join(timeout=15)
            require(not thread.is_alive(), "availability observer did not stop")
        require(len(responses) >= 10 and not errors and "shared-contract-patched" in responses,
                "controlled rolling update dropped requests or did not reach patched dependency")
        proof = [check_execution(h, p, language, "shared-contract-patched") for p in pods]
        h.record(language + " unchanged application-layer dependency replacement and observed native rollout",
            {"requests": len(responses), "failures": len(errors), "running": proof,
             "oldPodUIDs": [p["podUID"] for p in old[language]]}, ["COMP-06"])
        h.delete("pod", "candidate-" + language, h.namespace)
    after_inventory = inventory_snapshot(h, "after")
    require(all(package["version"] == "2.0.1" for facts in after_inventory["artifacts"].values()
                for package in facts["packages"]), "actual post-rollout inventory still includes old dependencies")
    h.record("post-remediation export verifies all observed running dependency identities, not a template patch",
        {"containers": len(after_inventory["containers"]), "artifacts": len(after_inventory["artifacts"]),
         "noBuiltinScanner": True, "fixtureVersionsOnly": True}, ["COMP-08"])
    # Normal cleanup must still block until all actual Deployment tasks drain.
    h.delete("nodeprofile", h.profile)
    time.sleep(3)
    profile = h.get("nodeprofile", h.profile)
    require(profile["metadata"].get("finalizers"), "active managed workloads lost cleanup ownership")
    for language in h.languages:
        h.delete("deployment", "managed-" + language, h.namespace)
        h.delete("service", "managed-" + language, h.namespace)
    h.wait("all managed workload Pods drained", lambda: not h.get("pods", namespace=h.namespace)["items"])
    h.wait("managed profile normal cleanup complete", lambda: h.get("nodeprofile", h.profile, absent=True) is None)
    require("brewlet.sh/owner-uid" not in h.node_snapshot()["metadata"].get("labels", {}),
            "normal cleanup retained node claim")
    h.node_exec("test", "!", "-e", "/usr/local/bin/containerd-shim-brewlet-v2")
    h.node_exec("crictl", "info")
    h.result["productionCleanupVerified"] = True
    h.record("managed Deployment drain preserves normal host cleanup and UID claim release",
             {"nodeUID": h.node_uid, "profileUID": profile["metadata"]["uid"]}, ["COMP-07"])


def run(work):
    h = Harness(work, pinned=False)
    h.result.update(selectedScenario="managed-composition", selectedLanguages=list(h.languages))
    h.result["gaps"].append("Selected specification §16 native matrix; other architectures and scenarios are separate.")
    h.launch_files = {}
    for language in h.languages:
        launch = json.loads((FIXTURES / language / "launch.json").read_text())
        launch["process"]["entrypoint"] = [EXECUTABLES.get(language, "/usr/bin/dotnet")]
        launch["runtimeImages"] = [IDS[language]]
        h.launch_files[language] = h.work / (language + "-managed-launch.json")
        h.launch_files[language].write_text(json.dumps(launch))
    h.build_runtime_images = lambda: None
    status = 1
    try:
        for phase, action in (("prerequisites", h.preflight), ("cluster", h.provision_cluster),
                              ("build", h.prepare_images)):
            h.phase = phase
            action()
        h.phase = "managed-publication-and-composition"
        pins = json.loads(PINS.read_text())
        pins["dotnet"] = json.loads((FIXTURES / "runtimes/managed-dotnet.json").read_text())
        h.delivery = ManagedDelivery(h, pins)
        h.delivery.bootstrap()
        h.phase = "install"
        h.install()
        h.startup()
        h.phase = "managed-real-node-lifecycle"
        exercise(h)
        require(h.arch in ("amd64", "arm64") and h.result["fullAcceptance"] is False,
                "selected composition run must not relabel historical baseline acceptance")
        h.record("selected native architecture composition, not historical baseline acceptance",
                 {"architecture": h.arch, "fullAcceptance": False}, ["COMP-25"])
        h.result["outcome"] = "passed"
        status = 0
    except Exception as error:
        h.result.update(outcome="failed", failurePhase=h.phase, failure=str(error))
        print("FAIL:", error, flush=True)
    finally:
        errors = []
        try:
            h.stop_helpers()
            h.diagnostics()
            delivery.cleanup(h)
        except Exception as error:
            errors.append("managed fixture teardown: " + str(error))
        errors.extend(h.destroy())
        if errors:
            h.result.update(outcome="failed", cleanupErrors=errors)
            status = 1
        h.save()
    return status if h.result.get("outcome") == "passed" else 1


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", required=True, type=Path)
    def interrupted(signum, frame):
        raise Failure("managed acceptance interrupted by signal " + str(signum))
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    raise SystemExit(run(parser.parse_args().work))
