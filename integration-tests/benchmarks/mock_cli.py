#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline-test CLI emulator. Never contacts Docker or Kubernetes."""
import json
import os
from pathlib import Path
import sys


def main():
    path = Path(os.environ["BENCHMARK_MOCK_STATE"])
    state = json.loads(path.read_text())
    tool, args = Path(sys.argv[0]).name, sys.argv[1:]
    state.setdefault("calls", []).append([tool, *args])
    text, error, code = "", "", 0
    option = lambda key: args[args.index(key) + 1]
    inspect_image = tool == "docker" and args[:2] == ["image", "inspect"]
    inspect_container = tool == "docker" and args[:1] == ["inspect"]
    if inspect_image or inspect_container:
        ref = args[2] if inspect_image else args[1]
        item = state["images" if inspect_image else "containers"].get(ref)
        if item is None:
            error, code = "Error: No such image/object: " + ref, 1
            if inspect_container:
                error = "Error: No such object: " + ref
            else:
                error = "Error: No such image: " + ref
        elif "--format" in args:
            text = "15123"
        else:
            text = json.dumps([{"Id": item["id"], "Config": {"Labels": item["labels"]}}])
    elif tool == "docker" and args[:2] == ["image", "rm"]:
        ref = args[-1]
        if ref in state.get("fail_remove", []):
            error, code = "injected image removal failure", 1
        else:
            del state["images"][ref]
    elif tool == "docker" and args[:1] == ["build"]:
        ref = option("-t")
        owner = option("--label").split("=", 1)[1]
        state["images"][ref] = {"id": "sha256:built-" + str(len(state["images"])),
                                "labels": {"brewlet.benchmark": owner}}
    elif tool == "docker" and args[:1] == ["tag"]:
        state["images"][args[2]] = dict(state["images"][args[1]])
    elif tool == "docker" and args[:1] == ["create"]:
        name, owner = option("--name"), option("--label").split("=", 1)[1]
        text = "registry-id"
        state["containers"][name] = {"id": text, "labels": {"brewlet.benchmark": owner}}
    elif tool == "docker" and args[:1] == ["start"]:
        pass
    elif tool == "docker" and args[:1] == ["rm"]:
        for name, item in list(state["containers"].items()):
            if item["id"] == args[-1]:
                del state["containers"][name]
    elif tool == "kind" and args[:2] == ["create", "cluster"]:
        cluster = option("--name")
        state["cluster"] = cluster
        state["containers"][cluster + "-control-plane"] = {
            "id": "node-id", "labels": {"io.x-k8s.kind.cluster": cluster}}
    elif tool == "kind" and args[:2] == ["delete", "cluster"]:
        if state.get("fail_kind_delete"):
            error, code = "injected node removal failure", 1
        else:
            state["containers"].pop(option("--name") + "-control-plane")
    elif tool == "kubectl":
        if "current-context" in args:
            text = "kind-" + state["cluster"]
        elif "nodeprofile" in args and "get" in args:
            text = json.dumps({"metadata": {"uid": "profile-id", "labels": {
                "benchmark": state.get("run_id")}}})
    elif tool == "helm":
        if state.get("fail_helm"):
            error, code = "injected Helm cleanup failure", 1
    elif tool == "git":
        if "status" in args:
            paths = args[args.index("--") + 1:]
            text = "\n".join(line for line in state.get("git_status", "").splitlines()
                             if any(line[3:] == p or line[3:].startswith(p + "/") for p in paths))
        elif "rev-parse" in args:
            text = state.get("fixture_head", "fixture-actual-head") if any(
                ".checkout" in x for x in args) else state.get("git_head", "actual-source-head")
        elif "remote" in args:
            text = "https://github.com/spring-projects/spring-petclinic.git"
    else:
        error, code = "Unsupported mock CLI command: " + repr([tool, *args]), 2
    path.write_text(json.dumps(state))
    if text:
        print(text)
    if error:
        print(error, file=sys.stderr)
    raise SystemExit(code)


if __name__ == "__main__":
    main()
