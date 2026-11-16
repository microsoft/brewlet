"""Live `brewlet k8s profile` CLI coverage against the invocation-owned cluster."""

import json
import sys

from harness import FENCE, require


POOL_KEY = "brewlet.test/pool"
CLEANUP_FINALIZER = "node.brewlet.sh/cleanup"
UNAPPROVED = "cli-unapproved"


def host_cli(h):
    # On macOS h.cli is a Linux binary run in a network-less container; the
    # k8s commands need a host binary that reaches the kind API server.
    if sys.platform != "darwin":
        return h.cli
    cli = h.work / "brewlet-host"
    h.run(["go", "build", "-mod=readonly", "-trimpath", "-o", cli, "./cmd/brewlet"], timeout=300)
    return cli


def ready_condition(obj):
    return next((c for c in obj.get("status", {}).get("conditions", []) if c["type"] == "Ready"), {})


def reconciled(obj, reason):
    """obj once the operator initialized its current generation with Ready reason."""
    if obj is None or CLEANUP_FINALIZER not in obj["metadata"].get("finalizers", []):
        return False
    generation = obj["metadata"]["generation"]
    status = obj.get("status", {})
    condition = ready_condition(obj)
    return obj if (status.get("ownershipInitialized") is True
                   and status.get("provisioningGeneration") == generation
                   and condition.get("observedGeneration") == generation
                   and condition.get("reason") == reason) else False


def require_spec(spec, expected, what):
    for key, value in expected.items():
        require(spec.get(key) == value, f"{what} spec.{key} is {spec.get(key)!r}, want {value!r}")


def exercise(h):
    binary = host_cli(h)
    rid = h.runtime_ids[h.probe_language]
    main = h.profile
    main_uid = h.owned[("nodeprofile", "", main)]
    main_generation = h.get("nodeprofile", main)["metadata"]["generation"]
    name = main + "-cli"
    pool = "absent-cli-" + h.run_id
    require(h.get("nodeprofile", name, absent=True) is None, "CLI fixture profile already exists")

    def cli(command, *args, check=True):
        return h.run([binary, "k8s", "profile", command, "--kubeconfig", h.kubeconfig,
                      "--context", h.context, *args], check=check, timeout=60)

    def refused(result, text, description):
        require(result.returncode != 0 and text in result.stderr,
                description + ": " + (result.stdout + result.stderr)[-2000:])

    def main_untouched():
        obj = h.get("nodeprofile", main)
        require(obj["metadata"]["uid"] == main_uid and not obj["metadata"].get("deletionTimestamp")
                and obj["metadata"]["generation"] == main_generation,
                "a refused or previewed CLI command changed the harness profile")

    # Read paths against the harness-applied, node-claiming profile.
    listed = json.loads(cli("list", "-o", "json").stdout)
    require(listed["kind"] == "NodeProfileList"
            and main_uid in [p["metadata"]["uid"] for p in listed["items"]],
            "profile list -o json omits the harness profile")
    require(main in cli("list").stdout and POOL_KEY in cli("list", "-o", "wide").stdout,
            "profile list table output omits the harness profile or its pool key")
    report = json.loads(cli("status", main, "-o", "json").stdout)
    require(report["uid"] == main_uid
            and [(n["name"], n["claimed"], n["found"]) for n in report["nodes"]] == [(h.node, True, True)],
            "profile status does not report the claimed kind node")
    require(main_uid in cli("status", main).stdout, "profile status table omits the profile UID")

    # Destructive commands refuse, or only preview, a profile with assigned nodes.
    refused(cli("delete", main, check=False), "--yes",
            "delete without --yes did not refuse a profile with assigned nodes")
    preview = cli("delete", main, "--dry-run", "-o", "json")
    require(json.loads(preview.stdout)["metadata"]["name"] == main and h.node in preview.stderr
            and "dry run: no changes made" in preview.stderr,
            "delete --dry-run did not preview the assigned node")
    refused(cli("create", name, "--pool", h.run_id, "--pool-key", POOL_KEY, "--runtime", rid, check=False),
            "PoolConflict", "create did not refuse a pool that another profile owns")
    main_untouched()
    require(h.get("nodeprofile", name, absent=True) is None, "a refused create left a profile")

    # The dry-run manifest is accepted by the real CRD schema and admission webhook.
    flags = ["--pool", pool, "--pool-key", POOL_KEY, "--runtime", rid + ":OnDemand",
             "--toleration", FENCE + "=provisioning:NoSchedule", "--max-unavailable", "1"]
    expected = {"nodePool": {"key": POOL_KEY, "names": [pool]},
                "runtimeImages": [{"id": rid, "install": "OnDemand"}],
                "tolerations": [{"key": FENCE, "operator": "Equal", "value": "provisioning",
                                 "effect": "NoSchedule"}]}
    dry = cli("create", name, *flags, "--dry-run", "-o", "json")
    require_spec(json.loads(dry.stdout)["spec"], expected, "create --dry-run manifest")
    server = json.loads(h.kubectl("create", "--dry-run=server", "-f", "-", "-o", "json",
                                  input=dry.stdout).stdout)
    require_spec(server["spec"], expected, "server dry-run of the CLI manifest")
    require(h.get("nodeprofile", name, absent=True) is None, "create --dry-run created a profile")

    created = cli("create", name, *flags)
    require("created" in created.stdout, "create did not report the new profile")
    obj = h.get("nodeprofile", name)
    uid = obj["metadata"]["uid"]
    require_spec(obj["spec"], expected, "created profile")
    try:
        obj = h.wait("operator initialization of the CLI profile",
                     lambda: reconciled(h.get("nodeprofile", name), "EmptyPool"), timeout=120)
        report = json.loads(cli("status", name, "-o", "json").stdout)
        require(report["uid"] == uid and report["ready"] == "False (EmptyPool)" and report["nodes"] == []
                and [(r["id"], r["install"], r["approved"]) for r in report["runtimes"]]
                == [(rid, "OnDemand", "True")],
                "profile status does not report the empty, approved CLI profile")
        rows = [line.split() for line in cli("list").stdout.splitlines()]
        require(any(row[0] == name and "0/0" in row and "(EmptyPool)" in row for row in rows if row),
                "profile list does not show the CLI profile with no nodes")

        added = cli("add-runtime", name, rid, "--install", "OnDemand")
        require("already offers" in added.stdout, "re-adding an offered runtime was not a no-op")
        added = cli("add-runtime", name, UNAPPROVED, "--install", "OnDemand")
        require("RuntimeImage " + UNAPPROVED + " does not exist" in added.stderr,
                "add-runtime did not warn about a missing RuntimeImage")
        obj = h.wait("UnknownRuntimeImage after add-runtime",
                     lambda: reconciled(h.get("nodeprofile", name), "UnknownRuntimeImage"), timeout=120)
        require([e["id"] for e in obj["spec"]["runtimeImages"]] == [rid, UNAPPROVED],
                "add-runtime did not append the runtime")
        removed = cli("remove-runtime", name, UNAPPROVED)
        require("no longer offers" in removed.stdout, "remove-runtime did not report the removal")
        obj = h.wait("EmptyPool after remove-runtime",
                     lambda: reconciled(h.get("nodeprofile", name), "EmptyPool"), timeout=120)
        generation = obj["metadata"]["generation"]
        refused(cli("remove-runtime", name, rid, check=False), "cannot be empty",
                "remove-runtime removed the last runtime")
        require(h.get("nodeprofile", name)["metadata"]["generation"] == generation,
                "a refused remove-runtime changed the profile")

        preview = cli("delete", name, "--dry-run")
        require("No nodes or pods are affected." in preview.stderr
                and h.get("nodeprofile", name)["metadata"]["uid"] == uid,
                "delete --dry-run of an empty profile did not preview without deleting")
        deleted = cli("delete", name)
        require("deleted" in deleted.stdout, "delete of an empty profile needed --yes or failed")
        h.wait("operator release of the deleted CLI profile",
               lambda: h.get("nodeprofile", name, absent=True) is None, timeout=180)
    except BaseException:
        current = h.get("nodeprofile", name, absent=True)
        if current is not None and current["metadata"]["uid"] == uid:
            h.kubectl("delete", "nodeprofile", name, "--wait=false", check=False)
        raise
    main_untouched()
    h.record("brewlet k8s profile CLI lists, creates, edits and deletes NodeProfiles",
             {"cliProfileUID": uid, "harnessProfileUID": main_uid,
              "cli": "host-native" if sys.platform == "darwin" else "native-linux"})
