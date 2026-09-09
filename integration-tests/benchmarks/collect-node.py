#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Run inside the task-owned node; read only exact CRI processes and ancestors."""
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import time


def read(path):
    try:
        return Path(path).read_text()
    except (OSError, UnicodeError):
        return None


def process(pid):
    root = Path("/proc") / str(pid)
    status = read(root / "status")
    if status is None:
        return {"pid": pid, "error": "process unavailable"}
    fields = dict(line.split(":", 1) for line in status.splitlines() if ":" in line)
    smaps = read(root / "smaps_rollup")
    mem = {}
    if smaps:
        mem = {line.split(":")[0]: int(line.split()[1]) * 1024
               for line in smaps.splitlines()[1:] if ":" in line and line.split()[1].isdigit()}
    return {"pid": pid, "ppid": int(fields["PPid"]), "uid": fields["Uid"].split(),
            "cmdline": (root / "cmdline").read_bytes().decode().strip("\0").split("\0"),
            "rss_bytes": mem.get("Rss"), "pss_bytes": mem.get("Pss"),
            "private_dirty_bytes": mem.get("Private_Dirty"),
            "shared_clean_bytes": mem.get("Shared_Clean"),
            "smaps_available": smaps is not None}


def main():
    output = {"sampled_unix_ns": time.time_ns(), "containers": [], "shims": []}
    seen = set()
    for cid in json.load(sys.stdin):
        cri = json.loads(subprocess.check_output(["crictl", "inspect", cid]))
        pid = cri["info"]["pid"]
        entry = process(pid)
        entry.update({"container_id": cid, "cri_started_at": cri["status"]["startedAt"],
                      "runtime_type": cri["info"].get("runtimeType"),
                      "oci_readonly_root": cri["info"]["runtimeSpec"]["root"].get("readonly", False)})
        cgroup = read(f"/proc/{pid}/cgroup")
        entry["cgroup"] = cgroup
        path = next((x.split("::", 1)[1] for x in (cgroup or "").splitlines()
                     if x.startswith("0::")), None)
        entry["cgroup_values"] = {}
        if path:
            for name in ["memory.current", "memory.max", "cpu.max", "memory.stat", "cpu.stat"]:
                value = read("/sys/fs/cgroup" + path + "/" + name)
                entry["cgroup_values"][name] = value.strip() if value else None
        # Read the image's declared real path: traversing an absolute symlink
        # through /proc/PID/root would otherwise resolve against the node root.
        release = read(f"/proc/{pid}/root/opt/java/openjdk/release")
        entry["jdk_release"] = release
        entry["jdk_release_sha256"] = hashlib.sha256(release.encode()).hexdigest() if release else None
        if release:
            entry["java_executable_sha256"] = hashlib.sha256(Path(f"/proc/{pid}/exe").read_bytes()).hexdigest()
            entry["cds_mappings"] = [line for line in (read(f"/proc/{pid}/maps") or "").splitlines()
                                     if ".jsa" in line]
            env = dict(pair.split("=", 1) for pair in Path(f"/proc/{pid}/environ").read_bytes()
                       .decode().strip("\0").split("\0") if "=" in pair)
            entry["runtime_environment"] = {key: env.get(key) for key in
                ["LANG", "LANGUAGE", "LC_ALL", "JAVA_HOME", "PATH", "HOME", "JAVA_VERSION",
                 "JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS"]}
        if "--descendants" in sys.argv:
            descendants = {pid}
            candidates = {}
            for status_path in Path("/proc").glob("[0-9]*/status"):
                status = read(status_path)
                if status:
                    fields = dict(line.split(":", 1) for line in status.splitlines() if ":" in line)
                    candidates[int(status_path.parent.name)] = int(fields["PPid"])
            while True:
                found = {child for child, parent in candidates.items() if parent in descendants}
                if found <= descendants:
                    break
                descendants.update(found)
            entry["descendant_processes"] = [process(child) for child in sorted(descendants - {pid})]
        entry["cri_name"] = cri["status"].get("metadata", {}).get("name")
        entry["cri_labels"] = {key: value for key, value in cri["status"].get("labels", {}).items()
                               if key in ["io.kubernetes.pod.name", "io.kubernetes.pod.namespace"]}
        parent = entry["ppid"]
        while parent > 1 and parent not in seen:
            ancestor = process(parent)
            if "error" in ancestor:
                break
            if any("containerd-shim" in item for item in ancestor["cmdline"]):
                seen.add(parent)
                output["shims"].append(ancestor)
                break
            parent = ancestor["ppid"]
        output["containers"].append(entry)
    print(json.dumps(output))


if __name__ == "__main__":
    main()
