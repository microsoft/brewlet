"""Read actual binding/rotation evidence for the private documentation tutorial."""

import argparse
from datetime import datetime, timezone
import json
import os
import subprocess
import sys
import time


def get(kind, name, namespace=None):
    command = ["kubectl", "--kubeconfig", os.environ["KUBECONFIG"],
               "--context", os.environ["CONTEXT"], "--request-timeout=20s"]
    if namespace:
        command += ["-n", namespace]
    result = subprocess.run([*command, "get", kind, name, "-o", "json"],
                            check=True, capture_output=True, text=True, timeout=30)
    return json.loads(result.stdout)


def observe(mode, source):
    node = get("node", os.environ["NODE"])
    if node["metadata"]["uid"] != os.environ["NODE_UID"]:
        raise RuntimeError("original tutorial Node UID changed")
    image = get("runtimeimage", os.environ["RID"])
    if image["metadata"]["uid"] != os.environ["RUNTIME_UID"]:
        raise RuntimeError("original tutorial inventory UID changed")
    digest = source.split("@", 1)[1]
    if mode == "rotation":
        conditions = image.get("status", {}).get("conditions", [])
        fresh = any(c["type"] == "RotationComplete" and c["status"] == "True"
                    and c.get("observedGeneration") == image["metadata"]["generation"] for c in conditions)
        status = image.get("status", {})
        participants = status.get("rotation", {}).get("participants", [])
        if (fresh and status.get("digest") == digest
                and any(p["uid"] == os.environ["NODE_UID"] for p in participants)):
            return {"generation": status["generation"], "digest": digest}
        return None
    observation = json.loads(node["metadata"].get("annotations", {}).get(
        "brewlet.sh/runtime-bindings", "{}"))
    pod = get("pod", os.environ["POD"], os.environ["APP_NAMESPACE"])
    if not observation.get("complete") or observation.get("nodeUID") != os.environ["NODE_UID"]:
        return None
    observed = observation.get("observedAt")
    if not observed:
        return None
    age = (datetime.now(timezone.utc) - datetime.fromisoformat(observed)).total_seconds()
    if not 0 <= age <= 120:
        return None
    for binding in observation.get("bindings") or []:
        if (binding["podUID"] == pod["metadata"]["uid"]
                and binding["id"] == os.environ["RID"]
                and binding["inventoryUID"] == os.environ["RUNTIME_UID"]
                and binding["digest"] == digest and binding["state"] == "running"):
            return binding
    return None


def webhook_pending(error):
    return ("runtimeimages.brewlet.sh" in error and "brewlet-admission.brewlet.svc" in error
            and any(reason in error for reason in (
                "connect: connection refused", "no endpoints available", "context deadline exceeded"))
            and "x509" not in error)


def wait_webhook(filename):
    command = ["kubectl", "--kubeconfig", os.environ["KUBECONFIG"],
               "--context", os.environ["CONTEXT"], "--request-timeout=15s",
               "create", "--dry-run=server", "-f", filename]
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        result = subprocess.run(command, capture_output=True, text=True, timeout=20)
        if result.returncode == 0:
            print("Inventory admission is reachable; dry-run only.")
            return
        if not webhook_pending(result.stderr):
            raise RuntimeError("Inventory admission failed: " + result.stderr)
        print("Waiting for this cluster's inventory webhook: " + result.stderr.strip(), file=sys.stderr)
        time.sleep(2)
    raise TimeoutError("inventory webhook transport did not become reachable within 60 seconds")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["binding", "rotation", "webhook"])
    parser.add_argument("--source")
    parser.add_argument("--file")
    args = parser.parse_args()
    if args.mode == "webhook":
        if not args.file:
            parser.error("webhook needs --file")
        wait_webhook(args.file)
        return
    if not args.source:
        parser.error("binding/rotation needs --source")
    deadline = time.monotonic() + 300
    while time.monotonic() < deadline:
        result = observe(args.mode, args.source)
        if result is not None:
            print(json.dumps(result))
            return
        time.sleep(2)
    raise TimeoutError("fresh actual tutorial " + args.mode + " evidence did not converge")


if __name__ == "__main__":
    main()
