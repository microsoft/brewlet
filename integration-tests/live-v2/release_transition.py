#!/usr/bin/env python3
"""Explicit disposable native drain/reinstall of exact candidate distribution bytes."""

import argparse
import json
from pathlib import Path
import shutil
import signal
import sys

from harness import Harness, ROOT, STORE, require
from scripts.release import image_identity, members, policy, read_json, sha, validate_inventory, write_json
from scripts.release_assurance import receipt
import delivery


class DistributionHarness(Harness):
    def __init__(self, work, candidate):
        self.candidate = candidate.resolve()
        self.distribution = validate_inventory(self.candidate, policy())
        super().__init__(work, pinned=False, scenario="runtime", local_language="python")
        require(self.distribution["version"] == policy()["version"], "unsupported transition candidate")
        self.distribution_chart = self.candidate / f"brewlet-{self.distribution['version']}.tgz"
        self.distribution_crds = self.work / "release-crds"
        self.distribution_crds.mkdir()
        for name, (data, _) in members(self.distribution_chart).items():
            if name.startswith("brewlet/crds/") and name.endswith(".yaml"):
                (self.distribution_crds / Path(name).name).write_bytes(data)
        require(list(self.distribution_crds.glob("*.yaml")), "packaged CRDs missing")

    def prepare_cli(self):
        data = members(self.candidate /
                       f"brewlet_{self.distribution['version']}_linux_{self.arch}.tar.gz")
        self.cli = self.work / "brewlet"
        self.cli.write_bytes(data["brewlet"][0])
        self.cli.chmod(0o755)
        self.cli_command = [self.cli]
        observed = json.loads(self.run([self.cli, "version"]).stdout)
        require(observed == {"version": self.distribution["version"],
                             "commit": self.distribution["candidate"]["checkout"],
                             "platform": "linux/" + self.arch}, "wrong distribution CLI")
        self.result["cliExecution"] = {"scope": "native-linux"}
        self.result["cliSHA256"] = sha(data["brewlet"][0])

    def prepare_components(self):
        config = policy()
        for component in config["images"]:
            source = self.candidate / ("image-" + component)
            image = image_identity(source, component, config)
            reference = config["images"][component] + "@" + image["digest"]
            # The outer layout's discovery annotations are not image payload bytes.
            destination = self.work / ("import-release-" + component)
            shutil.copytree(source, destination)
            index = read_json(destination / "index.json")
            index["manifests"][0].setdefault("annotations", {}).update({
                "io.containerd.image.name": reference,
                "org.opencontainers.image.ref.name": reference})
            write_json(destination / "index.json", index)
            self.import_layout(destination)
            require(self.image_digest(reference) == image["digest"], "containerd changed distribution image digest")
            self.result[{"operator": "controlImage", "admission": "admissionImage",
                         "node": "workerImage"}[component]] = reference
        self.save()


def uninstall(h):
    h.run(["helm", "--kubeconfig", h.kubeconfig, "--kube-context", h.context,
           "uninstall", h.cluster, "--namespace", h.control_namespace,
           "--wait", "--timeout", "360s"], timeout=420)
    require(not h.get("nodeprofiles")["items"], "uninstall left profile obligations")
    node = h.node_snapshot()
    require(not node["metadata"].get("labels", {}).get("brewlet.sh/owner-uid"), "uninstall retained claim")
    h.node_exec("test", "!", "-e", "/usr/local/bin/containerd-shim-brewlet-v2")
    h.node_exec("crictl", "info")
    h.node_exec("ctr", "plugins", "ls")


def execute(h):
    assertions = []
    h.phase = "prerequisites"
    h.preflight()
    require(sys.platform == "linux", "release transition requires a native Linux runner")
    h.provision_cluster()
    original_config = h.node_exec("cat", "/etc/containerd/config.toml").stdout
    h.prepare_images()
    delivery.bootstrap(h)
    h.install()
    h.startup()
    h.application_execution()
    assertions.append({"name": "artifact-workload-execution", "outcome": "passed"})
    h.production_cleanup()
    assertions.append({"name": "production-drain", "outcome": "passed"})
    uninstall(h)
    require(h.node_exec("cat", "/etc/containerd/config.toml").stdout == original_config,
            "uninstall did not restore the original host configuration")
    assertions += [{"name": name, "outcome": "passed"} for name in
                   ("production-uninstall", "host-reversal-and-cri")]
    # Preserve shared schemas, but apply the packaged schemas before reinstall.
    retired = []
    for key, uid in list(h.owned.items()):
        if key[0] not in ("pod", "nodeprofile", "runtimeimage"):
            continue
        require(h.get(key[0], key[2], key[1] or None, absent=True) is None,
                "cannot reinstall over retained original obligations")
        retired.append({"kind": key[0], "namespace": key[1], "name": key[2], "uid": uid})
        del h.owned[key]
    h.result["completedOriginalObligations"] = retired
    h.initial = {}
    h.install()
    h.startup()
    assertions.append({"name": "target-reinstall", "outcome": "passed"})
    h.application_execution()
    assertions.append({"name": "target-workload-execution", "outcome": "passed"})
    h.production_cleanup()
    uninstall(h)
    require(h.node_exec("cat", "/etc/containerd/config.toml").stdout == original_config,
            "target teardown did not restore original host configuration")
    return assertions


def run(candidate, work):
    h = DistributionHarness(work, candidate)
    status, assertions = 1, []
    try:
        assertions = execute(h)
        h.result["outcome"] = "passed"
        status = 0
    except Exception as error:
        h.result.update(outcome="failed", failure=str(error), failurePhase=h.phase)
        print("release transition failed:", error, file=sys.stderr)
    finally:
        errors = []
        try:
            h.stop_helpers()
            h.diagnostics()
        except Exception as error:
            errors.append("diagnostics: " + str(error))
        try:
            delivery.cleanup(h)
        except Exception as error:
            errors.append("delivery: " + str(error))
        errors.extend(h.destroy())
        if errors:
            h.result.update(outcome="failed", cleanupErrors=errors)
            status = 1
        h.save()
    if status == 0:
        require(h.result["outcome"] == "passed"
                and h.result["productionCleanupVerified"] is True
                and h.result["fixtureDestruction"]["outcome"] == "passed",
                "transition cleanup or immutable source verification failed")
        require(not (h.work / "manual-intervention.json").exists(), "manual intervention invalidates transition")
        value = receipt(candidate, "transition", assertions)
        manifest = sha((candidate / "candidate.json").read_bytes())
        value.update(productionCleanupVerified=True, fixtureDestruction={"outcome": "passed"},
                     transition={"sourceVersion": h.distribution["version"],
                                 "targetVersion": h.distribution["version"],
                                 "method": "drain-teardown-reinstall",
                                 "sourceCandidateSHA256": manifest, "targetCandidateSHA256": manifest})
        destination = candidate / "evidence"
        destination.mkdir(exist_ok=True)
        write_json(destination / f"transition-linux-{h.arch}.json", value)
    return status


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--candidate", required=True, type=Path)
    parser.add_argument("--work", required=True, type=Path)
    parser.add_argument("--authorize-disposable-transition", action="store_true", required=True)
    args = parser.parse_args()
    def interrupted(signum, frame):
        raise RuntimeError("release transition interrupted: " + str(signum))
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    raise SystemExit(run(args.candidate.resolve(), args.work.resolve()))
