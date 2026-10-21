#!/usr/bin/env python3
"""Signed Wasm composition and external-asset replacement; never a linked-code patch."""

import argparse
import hashlib
import json
from pathlib import Path
import signal
import threading
import time

import delivery
import managed
from harness import OWNER, STORE, Failure, require
from wasm import IDS, WasmHarness, guest, inspect as inspect_wasm
from wasm_inputs import EXAMPLE, build

MESSAGES = {"before": "shared-wasm-dependency", "after": "shared-wasm-dependency-patched"}


def dependency_lock(data, version):
    return {"schemaVersion": 1, "entries": [{
        "ecosystem": "generic", "name": "wasm-message", "version": version,
        "path": "/app/dependencies/message.txt", "sha256": hashlib.sha256(data).hexdigest(),
        "purl": "pkg:generic/brewlet/wasm-message@" + version}]}


class ManagedWasmDelivery(delivery.Delivery):
    def __init__(self, h):
        super().__init__(h)
        self.compositions = {engine: {} for engine in IDS}
        self.applications = {}
        self.bundles = {}

    def bootstrap(self):
        h = self.h
        self.start_registry()
        self.configure_node()
        self.policy, _ = managed.create_release_policy(h)
        h.runtime_images = {}
        for engine in IDS:
            runtime = h.inputs[engine]["after"]
            reference = self.publish(Path(runtime["layout"]), runtime["descriptor"], "managed-wasm/runtime-" + engine)
            h.runtime_images[engine] = {"before": reference, "after": reference}
            app = h.work / (engine + "-payload")
            app.mkdir(mode=0o755)
            filename = "guest.wasm" if engine == "wasmtime" else "spin-guest.wasm"
            (app / "guest.wasm").write_bytes((h.work / filename).read_bytes())
            if engine == "spin":
                (app / "spin.toml").write_bytes((EXAMPLE / "spin.toml").read_bytes())
            layout = h.work / (engine + "-payload-oci")
            h.run([*h.cli_command, "push", engine + "-payload", "--store", layout,
                   "--platform", "linux/" + h.arch, "--launch", EXAMPLE / (engine + "-launch.json"),
                   "--layer", f"app={app}:/app"], timeout=180)
            target = json.loads((layout / "index.json").read_text())["manifests"][0]
            reference = self.publish(layout, target, "managed-wasm/payload-" + engine)
            self.applications[engine] = {"layout": layout, "target": target, "reference": reference}
        self.publish_revision("before")
        h.app_images = {engine: self.compositions[engine]["before"]["reference"] for engine in IDS}
        h.result.update(applicationImages=h.app_images, runtimeImages=h.runtime_images)
        self.reject_invalid_compositions()
        h.save()

    def publish_revision(self, revision):
        h = self.h
        version = "1" if revision == "before" else "2"
        data = (MESSAGES[revision] + "\n").encode()
        directory = h.work / (revision + "-external-assets")
        directory.mkdir(mode=0o755)
        (directory / "message.txt").write_bytes(data)
        config, lock = h.work / (revision + "-config.json"), h.work / (revision + "-lock.json")
        config.write_text(json.dumps({"schemaVersion": 1, "name": "wasm-external-assets",
                                     "version": version, "ecosystem": "generic"}))
        lock.write_text(json.dumps(dependency_lock(data, version)))
        layout = h.work / (revision + "-bundle")
        h.run([*h.cli_command, "layer-set", "wasm-external-assets-" + version, "--store", layout,
               "--config", config, "--lock", lock, "--layer", f"dependency={directory}:/app/dependencies",
               "--signing-key", h.work / "publisher.key",
               "--builder-identity", "https://ci.brewlet.test/publisher"], timeout=180)
        target = json.loads((layout / "index.json").read_text())["manifests"][0]
        reference = self.publish(layout, target, "managed-wasm/external-assets-" + revision)
        self.bundles[revision] = {"layout": layout, "target": target, "reference": reference}
        for engine in IDS:
            application = (self.applications[engine] if revision == "before"
                           else self.compositions[engine]["before"])
            result = managed.compose_layout(h, str(application["layout"]) + "@" + application["target"]["digest"],
                str(layout) + "@" + target["digest"], self.policy, engine + "-" + revision)
            h.run([*h.cli_command, "verify", str(result["layout"]) + "@" + result["target"]["digest"],
                   "--platform", "linux/" + h.arch, "--policy", self.policy], timeout=300)
            result["reference"] = self.publish(result["layout"], result["target"], "managed-wasm/" + engine + "-" + revision)
            result["bundle"] = reference
            self.compositions[engine][revision] = result
            verification = result["receipt"]["verification"]
            require(verification["compositionSignature"] and verification["publisherProvenance"]
                    and verification["policyMode"] == "production", "Wasm output is not separately publisher/release verified")
            if revision == "after":
                before = self.compositions[engine]["before"]["receipt"]["verification"]["predicate"]
                after = verification["predicate"]
                require(before["applicationLayers"] == after["applicationLayers"]
                        and before["effectiveLaunch"] == after["effectiveLaunch"]
                        and before["originalLaunch"] == after["originalLaunch"]
                        and before["applicationRoot"] == after["applicationRoot"]
                        and before["dependencyEvidence"]["layers"] != after["dependencyEvidence"]["layers"]
                        and after["parentComposition"], "Wasm replacement rewrote application/launch or lost verified history")
            h.record(engine + " signed " + revision + " external bundle and final composition verified",
                {"image": result["reference"], "bundle": reference,
                 "predicate": verification["predicate"]}, ["COMP-WASM-01"])

    def reject_invalid_compositions(self):
        h = self.h
        bad_policy = json.loads(self.policy.read_text())
        bad_policy["publisher"]["identity"] = "https://ci.brewlet.test/not-the-publisher"
        policy = h.work / "wrong-publisher-policy.json"
        policy.write_text(json.dumps(bad_policy))
        bundle = self.bundles["before"]
        shadow = h.work / "shadow-external-assets"
        shadow.mkdir(mode=0o755)
        (shadow / "message.txt").write_text("must-not-shadow-approved-bytes")
        for engine in IDS:
            layout = h.work / (engine + "-shadow-payload")
            h.run([*h.cli_command, "push", "shadow-" + engine, "--store", layout,
                   "--platform", "linux/" + h.arch, "--launch", EXAMPLE / (engine + "-launch.json"),
                   "--layer", f"app={h.work / (engine + '-payload')}:/app",
                   "--layer", f"app={shadow}:/app/dependencies"], timeout=180)
            target = json.loads((layout / "index.json").read_text())["manifests"][0]
            for case, application, release_policy, expected in (
                ("wrong-publisher", self.applications[engine], policy, "provenance"),
                ("shadow", {"layout": layout, "target": target}, self.policy, "ManagedPathConflict")):
                destination = h.work / (engine + "-rejected-" + case)
                receipt = h.work / (engine + "-rejected-" + case + ".json")
                response = h.run([*h.cli_command, "compose", "rejected", "--store", destination,
                    "--application", str(application["layout"]) + "@" + application["target"]["digest"],
                    "--layer-set", str(bundle["layout"]) + "@" + bundle["target"]["digest"],
                    "--platform", "linux/" + h.arch, "--policy", release_policy,
                    "--signing-key", h.work / "composer.key",
                    "--builder-identity", "https://ci.brewlet.test/composer",
                    "--push-result", receipt], check=False, timeout=180)
                require(response.returncode and expected in response.stdout + response.stderr
                        and not (destination / "index.json").exists() and not receipt.exists(),
                        "invalid Wasm composition published accepted output: " + case)
            h.record(engine + " wrong publisher and managed-path shadow fail without accepted output",
                     {}, ["COMP-WASM-01"])


class ManagedWasmHarness(WasmHarness):
    def __init__(self, work):
        super().__init__(work)
        self.result["selectedScenario"] = "managed-wasm-composition"
        self.result["gaps"] = [gap for gap in self.result["gaps"]
                              if not gap.startswith("Preimported OCI graphs;")]
        self.result["gaps"] += [
            "External message-file update, not an executable library/component or compiled-in CVE patch.",
            "Fixed platform-built Wasm hosts; runtime version rotation/restart recovery are separate qualification.",
        ]

    def prepare(self):
        self.prepare_cli()
        self.prepare_components()
        self.delivery = ManagedWasmDelivery(self)
        self.delivery.bootstrap()

    def pod(self, name, engine, **kwargs):
        pod = super().pod(name, engine, **kwargs)
        pod["metadata"]["labels"] = {OWNER: self.run_id}
        pod["spec"]["imagePullSecrets"] = [{"name": self.delivery.secret}]
        pod["spec"]["containers"][0]["imagePullPolicy"] = "Always"
        pod["spec"]["containers"][0]["lifecycle"] = {"preStop": {"sleep": {"seconds": 5}}}
        pod["spec"]["terminationGracePeriodSeconds"] = 15
        return pod


def inspect_guest(h, pod, engine, revision):
    name = pod["metadata"]["name"]
    _, lease, observation = inspect_wasm(h, name, engine,
        h.inputs[engine]["after"]["releaseVersion"], dependency=MESSAGES[revision])
    kind = "command" if engine == "wasmtime" else "http"
    actual = h.node_exec("sha256sum", f'/proc/{observation["pid"]}/root/app/guest.wasm').stdout.split()[0]
    require(actual == h.result["wasmGuestInputs"][kind]["sha256"], "deployed guest bytes were rebuilt or modified")
    if engine == "wasmtime":
        logs = h.kubectl("-n", h.namespace, "logs", name, "-c", "app").stdout
        reports = [json.loads(line) for line in logs.splitlines() if line.startswith("{")]
        require(len(reports) == 1 and reports[0]["arguments"] == ["hold", "main"],
                "initial WASIp1 command did not produce its explicit startup report")
        guest(reports[0], MESSAGES[revision])
    runtime = lease["runtime"]
    require(runtime["sourceDigest"] == h.inputs[engine]["after"]["descriptor"]["digest"],
            "selected host source differs from the pinned platform fixture")
    return {"podUID": pod["metadata"]["uid"], "lease": lease, "guestSHA256": actual, "observation": observation}


def inventory(h, name, revision):
    report = managed.inventory_snapshot(h, name)
    bundle = h.delivery.bundles[revision]["target"]["digest"]
    require(len(report["artifacts"]) == 2 and len(report["containers"]) == 4,
            "two-engine/two-replica Wasm scope was not captured completely")
    for row in report["containers"]:
        require(row["workload"]["kind"] == "Deployment", "Wasm owner UID chain was not followed")
    for facts in report["artifacts"].values():
        require(facts["verification"]["predicate"]["dependencySet"]["digest"] == bundle
                and len(facts["packages"]) == 1 and facts["packages"][0]["version"] ==
                ("1" if revision == "before" else "2"), "observed Wasm package/SBOM binding did not update")
    h.record("observed " + revision + " Wasm application/package/SBOM identities exported for external tooling",
        {"containers": len(report["containers"]), "bundle": bundle}, ["COMP-WASM-03"])


def exercise(h):
    originals = {}
    for engine in IDS:
        h.apply(managed.deployment(h, engine))
        reference = h.delivery.compositions[engine]["before"]["reference"]
        pods = h.wait(engine + " original composed Deployment ready", lambda: managed.ready_pods(h, engine, reference))
        originals[engine] = [inspect_guest(h, pod, engine, "before") for pod in pods]
        h.record(engine + " initial guest/host Exec and protected filesystem/security observations",
                 originals[engine], ["COMP-WASM-02"])
    inventory(h, "wasm-before", "before")
    h.delivery.publish_revision("after")
    for engine in IDS:
        old_reference = h.delivery.compositions[engine]["before"]["reference"]
        old_pods = managed.ready_pods(h, engine, old_reference)
        require(old_pods, "publishing a new dependency bundle replaced old workloads")
        for pod in old_pods:
            proof = inspect_guest(h, pod, engine, "before")
            original = next(row for row in originals[engine] if row["podUID"] == proof["podUID"])
            require(proof["lease"] == original["lease"], "bundle publication hot-swapped an old lease")
        candidate = h.pod("candidate-" + engine, engine,
                         image=h.delivery.compositions[engine]["after"]["reference"])
        h.apply(candidate)
        pod = h.wait(engine + " candidate compatibility ready", lambda: h.ready("candidate-" + engine))
        inspect_guest(h, pod, engine, "after")
        current = h.get("deployment", "managed-" + engine, h.namespace)
        require(current["spec"]["template"]["spec"]["containers"][0]["image"] == old_reference,
                "candidate publication changed a workload before compatibility checks")
        h.record(engine + " candidate consumes new external bytes while old guest/lease stays unchanged",
                 {}, ["COMP-WASM-02", "COMP-WASM-03"])
        h.delete("pod", "candidate-" + engine, h.namespace)
        h.wait(engine + " candidate removed", lambda: h.get("pod", "candidate-" + engine, h.namespace, absent=True) is None)
        stopped, failures, responses = threading.Event(), [], []
        observer = None
        if engine == "spin":
            service = h.apply({"apiVersion": "v1", "kind": "Service",
                "metadata": {"name": "managed-spin", "namespace": h.namespace},
                "spec": {"selector": {"managed.brewlet.test/app": "spin"},
                         "ports": [{"port": 8000, "targetPort": 8000}]}})["spec"]["clusterIP"]
            def poll():
                while not stopped.is_set():
                    try:
                        response = h.node_exec("curl", "--fail", "--silent", "--show-error", "--noproxy", "*",
                            "--max-time", "5", "http://" + service + ":8000/", check=False, timeout=10)
                        if response.returncode:
                            failures.append("HTTP request failed")
                        else:
                            report = json.loads(response.stdout)
                            require(report["dependency"].strip() in MESSAGES.values(), "unexpected guest data")
                            guest(report, report["dependency"].strip())
                            responses.append(report["dependency"].strip())
                    except (Failure, ValueError, KeyError) as error:
                        failures.append(str(error))
                    stopped.wait(0.1)
            # Establish actual Service routing before counting availability.
            h.wait("Spin Service is reachable", lambda: h.node_exec("curl", "--fail", "--silent",
                "--noproxy", "*", "--max-time", "5", "http://" + service + ":8000/",
                check=False, timeout=10).returncode == 0)
            observer = threading.Thread(target=poll, daemon=True)
            observer.start()
        try:
            time.sleep(2)
            h.apply(managed.deployment(h, engine, "after"))
            reference = h.delivery.compositions[engine]["after"]["reference"]
            pods = h.wait(engine + " replacement Deployment ready", lambda: managed.ready_pods(h, engine, reference))
            h.wait(engine + " old actual container cohorts removed", lambda: not any(
                pod["metadata"].get("labels", {}).get("managed.brewlet.test/app") == engine
                and pod["spec"]["containers"][0]["image"] != reference
                for pod in h.get("pods", namespace=h.namespace)["items"]))
            time.sleep(2)
        finally:
            stopped.set()
            if observer is not None:
                observer.join(timeout=15)
                require(not observer.is_alive(), "Spin availability observer did not stop")
        if engine == "spin":
            require(len(responses) >= 10 and not failures and MESSAGES["after"] in responses,
                    "controlled Spin rollout failed HTTP continuity")
        for pod in pods:
            proof = inspect_guest(h, pod, engine, "after")
            require(proof["podUID"] not in {row["podUID"] for row in originals[engine]},
                    "old Wasm Pod was hot-patched instead of normally replaced")
            for field in ("inventoryUID", "generation", "sourceDigest"):
                require(proof["lease"]["runtime"][field] == originals[engine][0]["lease"]["runtime"][field],
                        "dependency update unexpectedly rotated Wasm host " + field)
        h.record(engine + " verified normal rollout with unchanged guest hashes and actual host generation",
            {"HTTPRequests": len(responses) if engine == "spin" else None,
             "HTTPFailures": len(failures) if engine == "spin" else None,
             "HTTPQualification": engine == "spin"}, ["COMP-WASM-02", "COMP-WASM-03", "COMP-WASM-04"])
    inventory(h, "wasm-after", "after")
    h.delete("nodeprofile", h.profile)
    time.sleep(3)
    profile = h.get("nodeprofile", h.profile)
    require(profile["metadata"].get("finalizers") and h.node_snapshot()["metadata"]["labels"].get(
        "brewlet.sh/owner-uid") == profile["metadata"]["uid"], "live Wasm tasks lost cleanup ownership")
    for engine in IDS:
        h.delete("deployment", "managed-" + engine, h.namespace)
    h.delete("service", "managed-spin", h.namespace)
    h.wait("all managed Wasm Pods drained", lambda: not h.get("pods", namespace=h.namespace)["items"])
    h.wait("normal managed Wasm host cleanup", lambda: h.get("nodeprofile", h.profile, absent=True) is None)
    require("brewlet.sh/owner-uid" not in h.node_snapshot()["metadata"].get("labels", {}),
            "managed Wasm cleanup retained UID claim")
    for rid in IDS.values():
        h.node_exec("test", "!", "-e", STORE + "/" + rid + "/current")
        h.delete("runtimeimage", rid)
        h.wait("normal managed Wasm inventory deletion", lambda rid=rid:
               h.get("runtimeimage", rid, absent=True) is None)
    h.node_exec("test", "!", "-e", "/usr/local/bin/containerd-shim-brewlet-v2")
    h.node_exec("crictl", "info")
    h.result["productionCleanupVerified"] = True
    h.record("normal managed Wasm task/root cleanup, host restoration and exact UID claim release",
             {}, ["COMP-WASM-04"])


def run(work):
    h = ManagedWasmHarness(work)
    status = 1
    try:
        h.phase = "managed-wasm-inputs"
        h.preflight()
        h.inputs = build(h, revisions=("after",))
        for phase, action in (("cluster", h.provision_cluster), ("build", h.prepare),
                              ("install", h.install), ("managed-wasm-execution", lambda: exercise(h))):
            h.phase = phase
            action()
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
            errors.append("managed Wasm diagnostics/fixture cleanup: " + str(error))
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
        raise Failure("managed Wasm acceptance interrupted by signal " + str(signum))
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    raise SystemExit(run(parser.parse_args().work))
