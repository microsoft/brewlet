#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Invocation-scoped resource ledger, provenance guards, and fail-closed cleanup."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import uuid

PRODUCTION_PATHS = ("core", "kubernetes", "provisioner", "maven-plugin", "scripts",
                    "integration-tests/fixtures", "legal", "LICENSE.txt", ".dockerignore",
                    "go.work", "go.work.sum")
FIXTURE_OVERRIDES = ("PETCLINIC_REPO", "PETCLINIC_REF", "PETCLINIC_JAR")


def execute(argv, *, check=True):
    result = subprocess.run([str(x) for x in argv], text=True, capture_output=True)
    if check and result.returncode:
        raise RuntimeError(f"{argv[0]} failed: {result.stderr.strip()}")
    return result


def preflight(repo=Path(".")):
    for key in FIXTURE_OVERRIDES:
        if os.environ.get(key):
            raise ValueError(f"Standard benchmark refuses nonempty {key}")
    status = execute(["git", "-C", repo, "status", "--porcelain", "--untracked-files=all",
                      "--", *PRODUCTION_PATHS]).stdout
    if status.strip():
        raise ValueError("Production/build/fixture source must be clean:\n" + status)
    return execute(["git", "-C", repo, "rev-parse", "HEAD"]).stdout.strip()


def initialize(work, repo=Path(".")):
    head = preflight(repo)
    work = Path(work).absolute()
    if work.exists() or work.is_symlink():
        raise ValueError("Output directory must be new")
    work.resolve().relative_to(repo.resolve())
    run_id = "run-" + uuid.uuid4().hex[:12]
    cluster = "brewlet-bench-" + run_id
    state = {"schema_version": 1, "run_id": run_id, "source_commit": head,
             "names": {"cluster": cluster, "node": cluster + "-control-plane",
                       "registry": cluster + "-registry",
                       "prefix": "localhost/phase10-" + run_id,
                       "registry_repository": "phase10-" + run_id},
             "images": [], "containers": [], "helm_attempted": False,
             "profile_attempted": False}
    work.mkdir(parents=True)
    Ledger(work, state).save()
    return state


class Ledger:
    def __init__(self, work, state=None):
        self.work = Path(work)
        self.path = self.work / "resources.json"
        self.state = state if state is not None else json.loads(self.path.read_text())

    def save(self):
        stage = self.path.with_suffix(".next.json")
        stage.write_text(json.dumps(self.state, indent=2) + "\n")
        stage.replace(self.path)

    @property
    def kube(self):
        return ["kubectl", "--kubeconfig", str(self.work / "kubeconfig")]

    def inspect(self, kind, ref):
        args = ["docker", "image", "inspect", ref] if kind == "image" else ["docker", "inspect", ref]
        result = execute(args, check=False)
        if result.returncode:
            if "No such image" in result.stderr or "No such object" in result.stderr or "No such container" in result.stderr:
                return None
            raise RuntimeError("Cannot establish resource identity: " + result.stderr.strip())
        item = json.loads(result.stdout)[0]
        return {"id": item["Id"], "labels": item.get("Config", {}).get("Labels") or {}}

    def expected_ref(self, ref):
        prefixes = [self.state["names"]["prefix"] + "/"]
        if "registry_endpoint" in self.state:
            prefixes.append(self.state["registry_endpoint"] + "/" +
                            self.state["names"]["registry_repository"] + "/")
        if not any(ref.startswith(prefix) for prefix in prefixes):
            raise ValueError("Image reference is outside this invocation")

    def image(self, ref, argv, *, built=False):
        self.expected_ref(ref)
        known = next((x for x in self.state["images"] if x["ref"] == ref), None)
        current = self.inspect("image", ref)
        if current is not None:
            if known is None or known["id"] != current["id"]:
                raise ValueError("Refusing to adopt/overwrite preexisting image " + ref)
            if built:
                raise ValueError("Refusing to rebuild an existing invocation image")
            source = self.inspect("image", argv[-2])
            if source is None or source["id"] != current["id"]:
                raise ValueError("Existing invocation alias no longer matches its source")
            return
        if known is not None:
            raise ValueError("Invocation image disappeared before reuse: " + ref)
        outcome = execute(argv, check=False)
        created = self.inspect("image", ref)
        if created is not None:
            if built and created["labels"].get("brewlet.benchmark") != self.state["run_id"]:
                raise RuntimeError("Built image lacks invocation ownership label")
            if not built:
                source = self.inspect("image", argv[-2])
                if source is None or source["id"] != created["id"]:
                    raise RuntimeError("Tagged image does not match source identity")
            self.state["images"].append({"ref": ref, "id": created["id"], "built": built})
            self.save()
        if outcome.returncode:
            raise RuntimeError(outcome.stdout + outcome.stderr)
        if created is None:
            raise RuntimeError("Image command succeeded without a recorded image")
        print(outcome.stdout, end="")

    def build(self, ref, args):
        self.image(ref, ["docker", "build", "--platform", "linux/arm64", "--provenance=false",
                        "--label", "brewlet.benchmark=" + self.state["run_id"], "-t", ref, *args],
                   built=True)

    def tag(self, source, ref):
        self.image(ref, ["docker", "tag", source, ref])

    def create_node(self, kind, image):
        name = self.state["names"]["node"]
        if self.inspect("container", name) is not None:
            raise ValueError("Refusing preexisting benchmark node")
        self.state["kind_binary"] = str(Path(kind).absolute())
        self.save()
        outcome = execute([kind, "create", "cluster", "--name", self.state["names"]["cluster"],
                           "--image", image, "--kubeconfig", self.work / "kubeconfig", "--wait", "180s"],
                          check=False)
        current = self.inspect("container", name)
        if current is not None:
            if current["labels"].get("io.x-k8s.kind.cluster") != self.state["names"]["cluster"]:
                raise RuntimeError("Created node lacks expected kind ownership label")
            self.state["containers"].append({"role": "node", "name": name, **current})
            self.save()
        if outcome.returncode or current is None:
            raise RuntimeError(outcome.stdout + outcome.stderr)

    def create_registry(self):
        name = self.state["names"]["registry"]
        if self.inspect("container", name) is not None:
            raise ValueError("Refusing preexisting registry")
        created = execute(["docker", "create", "--platform", "linux/arm64", "--name", name,
                           "--label", "brewlet.benchmark=" + self.state["run_id"],
                           "--cpus", ".25", "--memory", "128m", "-p", "127.0.0.1::5000", "registry:3"])
        identity = self.inspect("container", name)
        if identity is None or identity["id"] != created.stdout.strip() or \
                identity["labels"].get("brewlet.benchmark") != self.state["run_id"]:
            raise RuntimeError("Registry identity/ownership mismatch")
        self.state["containers"].append({"role": "registry", "name": name, **identity})
        self.save()
        execute(["docker", "start", identity["id"]])
        port = execute(["docker", "inspect", name, "--format",
                        '{{(index (index .NetworkSettings.Ports "5000/tcp") 0).HostPort}}']).stdout.strip()
        if not port.isdigit():
            raise RuntimeError("Registry did not publish a loopback port")
        self.state["registry_endpoint"] = "127.0.0.1:" + port
        self.save()
        return self.state["registry_endpoint"]

    def verify_node(self, node, kubeconfig):
        expected = next((x for x in self.state["containers"] if x["role"] == "node"), None)
        if expected is None or node != expected["name"]:
            raise ValueError("Node is not owned by this invocation")
        current = self.inspect("container", node)
        if current is None or current["id"] != expected["id"] or \
                current["labels"].get("io.x-k8s.kind.cluster") != self.state["names"]["cluster"]:
            raise ValueError("Node identity changed")
        if Path(kubeconfig).resolve() != (self.work / "kubeconfig").resolve():
            raise ValueError("Kubeconfig is outside this invocation")
        if execute(self.kube + ["config", "current-context"]).stdout.strip() != \
                "kind-" + self.state["names"]["cluster"]:
            raise ValueError("Kubeconfig context does not match owned node")

    def cleanup(self, exit_code=0):
        result = {"schema_version": 1, "run_id": self.state["run_id"], "exit_code": exit_code,
                  "profile_finalization_succeeded": None, "helm_uninstall_succeeded": None,
                  "node_removed": None, "registry_removed": None, "failures": [],
                  "removed_image_references": [], "remaining_owned_image_references": []}
        with (self.work / "cleanup.log").open("a") as log:
            def attempt(argv):
                outcome = execute(argv, check=False)
                log.write("$ " + " ".join(str(x) for x in argv) + "\n" + outcome.stdout + outcome.stderr)
                log.flush()
                if outcome.returncode:
                    result["failures"].append(f"{argv[0]} cleanup failed: {outcome.stderr.strip()}")
                return outcome.returncode == 0

            for resource in self.state["containers"]:
                role = resource["role"]
                try:
                    current = self.inspect("container", resource["name"])
                    if current is None:
                        result[role + "_removed"] = True
                        if role == "node" and (self.state["helm_attempted"] or self.state["profile_attempted"]):
                            result["failures"].append("Node disappeared before normal profile/Helm cleanup could be verified")
                        continue
                    label = "io.x-k8s.kind.cluster" if role == "node" else "brewlet.benchmark"
                    wanted = self.state["names"]["cluster"] if role == "node" else self.state["run_id"]
                    if current["id"] != resource["id"] or current["labels"].get(label) != wanted:
                        raise ValueError("Refusing changed/foreign container " + resource["name"])
                    if role == "node":
                        try:
                            self.verify_node(resource["name"], self.work / "kubeconfig")
                            drained = attempt(self.kube + ["delete", "pods", "-n", "phase10", "-l",
                                "benchmark=" + self.state["run_id"], "--ignore-not-found", "--wait=true", "--timeout=60s"])
                            if drained and self.state["profile_attempted"]:
                                result["profile_finalization_succeeded"] = attempt(self.kube + [
                                    "delete", "nodeprofile", "phase10", "--ignore-not-found", "--wait=true", "--timeout=240s"])
                            if self.state["helm_attempted"] and drained and \
                                    result["profile_finalization_succeeded"] is not False:
                                result["helm_uninstall_succeeded"] = attempt(["helm", "--kubeconfig",
                                    self.work / "kubeconfig", "uninstall", "phase10", "-n", "default", "--timeout", "300s"])
                        except (ValueError, RuntimeError, OSError) as exc:
                            result["failures"].append(str(exc))
                        attempt([self.state["kind_binary"], "delete", "cluster", "--name",
                                 self.state["names"]["cluster"], "--kubeconfig", self.work / "kubeconfig"])
                    else:
                        attempt(["docker", "rm", "-f", "-v", resource["id"]])
                    result[role + "_removed"] = self.inspect("container", resource["name"]) is None
                    if not result[role + "_removed"]:
                        result["failures"].append("Container remains: " + resource["name"])
                except (ValueError, RuntimeError, OSError) as exc:
                    result[role + "_removed"] = False
                    result["failures"].append(str(exc))
            for image in reversed(self.state["images"]):
                try:
                    self.expected_ref(image["ref"])
                    current = self.inspect("image", image["ref"])
                    if current is None:
                        continue
                    if current["id"] != image["id"] or (image["built"] and
                            current["labels"].get("brewlet.benchmark") != self.state["run_id"]):
                        raise ValueError("Refusing changed/foreign image " + image["ref"])
                    if attempt(["docker", "image", "rm", "--no-prune", image["ref"]]):
                        if self.inspect("image", image["ref"]) is not None:
                            raise RuntimeError("Image reference remains after removal " + image["ref"])
                        result["removed_image_references"].append(image["ref"])
                    else:
                        result["remaining_owned_image_references"].append(image["ref"])
                except (ValueError, RuntimeError, OSError) as exc:
                    result["failures"].append(str(exc))
                    result["remaining_owned_image_references"].append(image["ref"])
        result["emergency_cluster_teardown"] = bool(result["failures"])
        if result["failures"]:
            result["exit_code"] = exit_code or 1
        (self.work / "cleanup.json").write_text(json.dumps(result, indent=2) + "\n")
        return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work", required=True, type=Path)
    sub = parser.add_subparsers(dest="action", required=True)
    sub.add_parser("init")
    field = sub.add_parser("field")
    field.add_argument("name")
    node = sub.add_parser("node")
    node.add_argument("--kind", required=True)
    node.add_argument("--image", required=True)
    sub.add_parser("registry")
    build = sub.add_parser("build")
    build.add_argument("ref")
    build.add_argument("args", nargs=argparse.REMAINDER)
    mark = sub.add_parser("mark")
    mark.add_argument("name", choices=["helm_attempted", "profile_attempted"])
    clean = sub.add_parser("cleanup")
    clean.add_argument("--exit-code", type=int, default=0)
    args = parser.parse_args()
    if args.action == "init":
        initialize(args.work)
        return
    ledger = Ledger(args.work)
    if args.action == "field":
        print(ledger.state.get(args.name, ledger.state["names"].get(args.name)))
    elif args.action == "node":
        ledger.create_node(args.kind, args.image)
    elif args.action == "registry":
        print(ledger.create_registry())
    elif args.action == "build":
        ledger.build(args.ref, args.args[1:] if args.args[:1] == ["--"] else args.args)
    elif args.action == "mark":
        ledger.state[args.name] = True
        ledger.save()
    elif args.action == "cleanup":
        raise SystemExit(ledger.cleanup(args.exit_code)["exit_code"])


if __name__ == "__main__":
    main()
