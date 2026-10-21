"""Optional Maven producer artifacts through the ordinary authenticated node path."""

import json
import os
import shlex

from harness import IDS, ROOT, STORE, artifact_target, require, require_owned, runtime_version_matches


def prepare(h):
    wrapper = h.work / "maven-brewlet"
    wrapper.write_text("#!/bin/sh\nset -eu\nexec " + shlex.join(map(str, h.cli_command)) + ' "$@"\n')
    os.chmod(wrapper, 0o700)
    environment = {"MAVEN_OPTS": "-Dmaven.repo.local=" + str(h.work / "m2")}
    h.run(["mvn", "-B", "-q", "-f", ROOT / "maven-plugin/pom.xml", "install"],
          env=environment, timeout=600)
    pom = h.work / "fixtures/java/app/pom.xml"
    goal = "sh.brewlet:brewlet-maven-plugin:" + json.loads((ROOT / "release.json").read_text())["version"] + ":"
    common = [f"-Dbrewlet.executable={wrapper}", "-Dbrewlet.platforms=linux/amd64,linux/arm64"]
    layouts = {}
    for variant in ("bundled", "managed"):
        layout = h.work / ("oci-maven-" + variant)
        options = []
        if variant == "managed":
            dependencies = h.work / "oci-maven-dependencies"
            bom = "sh.brewlet.fixture:fixture-bom:2.0.0"
            h.run(["mvn", "-B", "-q", "-f", pom, goal + "layer-set", *common,
                   f"-Dbrewlet.store={dependencies}",
                   "-Dbrewlet.layerSet=fixtures/java-dependencies:v2",
                   "-Dbrewlet.sourceBom=" + bom], env=environment, timeout=300)
            options = [f"-Dbrewlet.layerSetInput={dependencies}", "-Dbrewlet.sourceBom=" + bom]
        h.run(["mvn", "-B", "-q", "-f", pom, "package", goal + "push", *common,
               f"-Dbrewlet.store={layout}", "-Dbrewlet.image=fixtures/java-maven-" + variant + ":v2",
               *options], env=environment, timeout=300)
        target, roles = artifact_target(layout)
        require(len(roles) == 2, "Maven producer did not emit both explicit platform variants")
        descriptor = json.loads((layout / "index.json").read_text())["manifests"][0]
        require(descriptor["digest"] == target, "Maven outer reference does not identify runnable index")
        reference = h.delivery.publish(layout, descriptor, "delivery/maven-" + variant)
        require(reference.endswith("@" + target), "Maven publication changed the produced OCI target")
        layouts[variant] = reference
    return layouts


def execute(h):
    require(hasattr(h, "delivery") and bool(h.initial), "Maven node case requires real base execution")
    references = prepare(h)
    names, original = [], {}
    image = h.get("runtimeimage", IDS["java"])
    require_owned(image, h.run_id, h.owned[("runtimeimage", "", IDS["java"])])
    require(image["spec"]["source"]["image"] == h.runtime_images["java"]["after"],
            "Maven node case requires the actual post-update Java runtime")
    previous = image["status"]["generation"]
    probes = h.runtime_vendor_inputs["java"]

    def start(variant, revision, minimum=None):
        name = "maven-" + variant + "-" + revision
        pod = h.pod(name, "java", barrier=minimum)
        pod["spec"]["containers"][0]["image"] = references[variant]
        h.apply(pod)
        names.append(name)
        obj = h.wait("Maven-produced real application " + name, lambda: h.ready(name))
        h.assert_report(h.http(name), "java", arguments=[], cwd="/app")
        h.assert_report(json.loads(h.exec(name, h.probe("java")).stdout), "java",
                        arguments=["--probe", "two words"], cwd="/app")
        version = h.exec(name, probes["probe"])
        require(runtime_version_matches(version.stdout + version.stderr, probes[revision]["version"]),
                "Maven application executed the wrong actual vendor runtime")
        h.exec(name, ["/bin/sh", "-ec",
                      "test ! -e /launch.json; "
                      "if touch /app/maven-write-must-fail; then exit 81; fi; "
                      "grep -q '^NoNewPrivs:[[:space:]]*1$' /proc/self/status; "
                      "grep -q '^CapEff:[[:space:]]*0000000000000000$' /proc/self/status"])
        return obj, h.lease(obj)

    for variant in references:
        original[variant] = start(variant, "after")
    image["spec"]["source"]["image"] = h.runtime_images["java"]["before"]
    h.replace_inventory_spec(image)
    updated = h.wait("Maven runtime rotation allocates new generation", lambda:
        (value if value.get("status", {}).get("generation", 0) > previous else False)
        if (value := h.get("runtimeimage", IDS["java"])) else False)
    generation = updated["status"]["generation"]
    h.wait("Maven runtime target actually validated", lambda: h.advertised(IDS["java"], "ready", generation))
    barrier = {"id": IDS["java"], "inventoryUID": updated["metadata"]["uid"], "generation": generation}
    for variant, (pod, lease) in original.items():
        current = h.get("pod", pod["metadata"]["name"], h.namespace)
        require(current["metadata"]["uid"] == pod["metadata"]["uid"] and h.lease(current) == lease,
                "runtime rotation replaced a Maven-produced task or lease")
        old_version = h.exec(pod["metadata"]["name"], probes["probe"])
        require(runtime_version_matches(old_version.stdout + old_version.stderr,
                                        probes["after"]["version"]),
                "existing Maven task/Exec changed its leased runtime")
        replacement, replacement_lease = start(variant, "before", barrier)
        require(replacement_lease["runtime"]["generation"] == generation
                and replacement_lease["runtime"]["inventoryUID"] == barrier["inventoryUID"]
                and replacement["spec"]["containers"][0]["image"] == references[variant],
                "Maven replacement changed application bytes or missed generation barrier")
        h.record("Maven " + variant + " artifact executes, probes and rotates without republishing",
                 {"applicationImage": references[variant], "oldLease": lease,
                  "newLease": replacement_lease, "explicitRuntimeID": IDS["java"]},
                 ["MAV-01", "LANG-01", "LANG-02", "PROC-03"])
    for name in names:
        pod = h.get("pod", name, h.namespace)
        task = h.lease(pod)["taskID"]
        h.delete("pod", name, h.namespace)
        h.wait("owned Maven task Pod drained " + name, lambda name=name:
               h.get("pod", name, h.namespace, absent=True) is None)
        h.wait("Maven durable task lease drained " + name, lambda task=task:
               h.node_exec("test", "!", "-e", STORE + "/.tasks/" + task + "/lease.json",
                           check=False).returncode == 0)
    h.result["mavenNode"] = {"outcome": "passed", "applicationImages": references,
                              "genericShimPath": True, "applicationRepublishedDuringRotation": False}
    h.result["gaps"] = [gap for gap in h.result["gaps"] if not gap.startswith("Maven-produced artifact on-node")]
    h.save()
