#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Generate isolated benchmark inputs and sanitized provenance, without modifying source."""
import argparse
import json
from pathlib import Path
import platform
import subprocess
import zipfile
from resources import Ledger, preflight

JDK = "docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b"
NODE_IMAGE = "kindest/node@sha256:85a0a530f46466f6ead1118c1ce772c682d154c3d5eb7fd91ae83f5bb6abcdd6"


def source_provenance(work, ledger):
    head = preflight()
    if head != ledger.state["source_commit"]:
        raise ValueError("Source HEAD changed since invocation began")
    checkout = str(work / "fixture/.checkout")
    return {"source_commit": head,
            "workload_commit": subprocess.check_output(
                ["git", "-C", checkout, "rev-parse", "HEAD"], text=True).strip(),
            "workload_repository": subprocess.check_output(
                ["git", "-C", checkout, "remote", "get-url", "origin"], text=True).strip()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["inputs", "revision", "metadata"])
    parser.add_argument("--work", type=Path, required=True)
    parser.add_argument("--node")
    args = parser.parse_args()
    w = args.work
    ledger = Ledger(w)
    prefix = ledger.state["names"]["prefix"]
    if preflight() != ledger.state["source_commit"]:
        raise ValueError("Source HEAD changed since invocation began")
    if args.action == "inputs":
        (w / "pom.xml").write_text(f"""<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>sh.brewlet.benchmark</groupId><artifactId>petclinic</artifactId><version>1</version>
  <properties><maven.compiler.release>21</maven.compiler.release></properties>
  <build><plugins><plugin>
    <groupId>sh.brewlet</groupId><artifactId>brewlet-maven-plugin</artifactId><version>0.1.0-SNAPSHOT</version>
    <configuration>
      <image>{prefix}/brewlet:baseline</image>
      <jarFile>${{project.basedir}}/fixture/target/spring-petclinic.jar</jarFile>
      <layered>true</layered><format>image</format><arch><param>arm64</param></arch>
      <ociOutputDirectory>${{project.basedir}}/brewlet-oci</ociOutputDirectory>
    </configuration>
  </plugin></plugins></build>
</project>
""")
        profile = {"apiVersion": "node.brewlet.sh/v1alpha1", "kind": "NodeProfile",
                   "metadata": {"name": "phase10", "labels": {"benchmark": ledger.state["run_id"]}},
                   "spec": {"nodePool": {"key": "brewlet.sh/benchmark", "names": ["phase10"],
                                        "includeControlPlane": True},
                            "jdks": [{"distribution": "temurin", "feature": 21,
                                      "source": {"image": JDK, "javaHome": "/opt/java/openjdk"}}],
                            "rollout": {"validate": True, "containerdRestart": "validated"}}}
        (w / "profile.json").write_text(json.dumps(profile, indent=2) + "\n")
        (w / "provisioner.Dockerfile").write_text(Path("provisioner/Dockerfile").read_text()
                                               .replace("/tmp/containerd", "/work/containerd")
                                               .replace("/tmp/crictl", "/work/crictl"))
    elif args.action == "revision":
        (w / "revision").mkdir(exist_ok=True)
        (w / "fat").mkdir(exist_ok=True)
        with zipfile.ZipFile(w / "fixture/target/spring-petclinic.jar") as source, \
             zipfile.ZipFile(w / "revision/spring-petclinic.jar", "w") as target:
            for entry in source.infolist():
                target.writestr(entry, source.read(entry))
            entry = zipfile.ZipInfo("BOOT-INF/classes/benchmark-revision.txt", (2020, 1, 1, 0, 0, 0))
            entry.compress_type = zipfile.ZIP_DEFLATED
            target.writestr(entry, b"Controlled benchmark resource revision 1. Dependencies unchanged.\n")
        (w / "revision-pom.xml").write_text((w / "pom.xml").read_text()
            .replace("fixture/target/spring-petclinic.jar", "revision/spring-petclinic.jar")
            .replace("brewlet-oci", "brewlet-revision-oci").replace("brewlet:baseline", "brewlet:revision"))
        (w / "fat/Dockerfile").write_text(
            f'FROM {JDK}\nCOPY spring-petclinic.jar /app/spring-petclinic.jar\nWORKDIR /app\n'
            'USER 65532:65532\nENTRYPOINT ["java","-jar","/app/spring-petclinic.jar"]\n')
    else:
        ledger.verify_node(args.node, w / "kubeconfig")
        run = lambda argv: subprocess.check_output(argv, text=True).strip()
        info = json.loads(run(["docker", "info", "--format", "{{json .}}"]))
        image = json.loads(run(["docker", "image", "inspect", prefix + "/conventional:baseline"]))[0]
        # The node's imported image list contains the actual OCI manifest digest,
        # whereas Docker .Id is the config digest.
        listing = run(["docker", "exec", args.node, "ctr", "-n", "k8s.io", "images", "ls"])
        conventional = next(line.split()[2] for line in listing.splitlines()
                            if line.startswith(prefix + "/conventional:baseline "))
        brewlet = json.loads((w / "brewlet-oci/index.json").read_text())["manifests"][0]["digest"]
        images = {"conventional": prefix + "/conventional@" + conventional,
                  "brewlet": prefix + "/brewlet@" + brewlet}
        for variant, ref in images.items():
            run(["docker", "exec", args.node, "ctr", "-n", "k8s.io", "images", "tag",
                 prefix + "/" + variant + ":baseline", ref])
        data = {**source_provenance(w, ledger),
                "run_id": ledger.state["run_id"], "resource_names": ledger.state["names"],
                "host_os": platform.system(), "host_arch": platform.machine(),
                "host_os_version": platform.mac_ver()[0] or platform.release(),
                "python_version": run(["python3", "--version"]),
                "fixture_build_jdk": run(["java", "--version"]),
                "docker": {k: info.get(k) for k in ["Architecture", "NCPU", "MemTotal",
                                                  "OperatingSystem", "ServerVersion"]},
                "jdk_source": JDK, "kind_node_image": NODE_IMAGE,
                "go_version": run(["go", "version"]),
                "node_kernel": run(["docker", "exec", args.node, "uname", "-srmo"]),
                "containerd_version": run(["docker", "exec", args.node, "ctr", "version"]),
                "runc_version": run(["docker", "exec", args.node, "runc", "--version"]),
                "jdk_userland_os_release": run(["docker", "exec", args.node, "cat",
                                               "/opt/brewlet/jdks/temurin-21/etc/os-release"]),
                "images": images, "node_memory_cap_bytes": 5368709120,
                "conventional_config_digest": image["Id"],
                "path": "real containerd CRI + production Brewlet shim; shipped operator and provisioner; no Ratify/HPA",
                "placement": "Both variants explicitly pinned with spec.nodeName to the dedicated node; scheduler queue time is excluded.",
                "runtime_scope": "Same full JDK and source-image userland on both sides; JRE-only and jlink alternatives are not measured.",
                "source_modifications": "Production/build/fixture paths verified clean before side effects and metadata capture; benchmark/docs changes allowed. Provisioner build extraction staging relocated to /work.",
                "custom_appcds": False}
        (w / "setup.json").write_text(json.dumps(data, indent=2) + "\n")


if __name__ == "__main__":
    main()
