#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Prepare matched conventional images from the Maven plugin's runnable OCI image."""
import argparse
import hashlib
import io
import json
import pathlib
import tarfile


def blob(root, digest):
    return (root / "blobs" / digest.replace(":", "/")).read_bytes()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("layout", type=pathlib.Path)
    parser.add_argument("output", type=pathlib.Path)
    parser.add_argument("--jdk-image", required=True)
    args = parser.parse_args()
    root, output = args.layout, args.output
    output.mkdir(parents=True, exist_ok=True)
    descriptor = json.loads((root / "index.json").read_text())["manifests"][0]
    manifest = json.loads(blob(root, descriptor["digest"]))
    if "manifests" in manifest:
        descriptor = next(x for x in manifest["manifests"] if x["platform"]["architecture"] == "arm64")
        manifest = json.loads(blob(root, descriptor["digest"]))
    config = json.loads(manifest["annotations"]["brewlet.sh/jvm-config"])
    for layer in manifest["layers"]:
        target = output / "app"
        if layer.get("annotations", {}).get("brewlet.sh/layer") == "classpath":
            target /= "lib"
        target.mkdir(parents=True, exist_ok=True)
        with tarfile.open(fileobj=io.BytesIO(blob(root, layer["digest"])), mode="r:*") as archive:
            archive.extractall(target, filter="data")
    classpath = ":".join("/app/" + p for p in config["entry"]["classPath"])
    flags = ["-XX:+ExitOnOutOfMemoryError", "-Xms128m", "-Xmx256m",
             "-Dspring.profiles.active=h2", "-Dspring.main.banner-mode=off",
             "-Djava.io.tmpdir=/work"]
    command = ["java", *flags, "-cp", classpath, config["entry"]["mainClass"]]
    # The runtime mounts the complete source-image userland at /; /opt/jdk is its
    # stable JDK path. A symlink gives the conventional image the same argv path.
    dockerfile = (f"FROM {args.jdk_image}\n"
                  "RUN ln -s /opt/java/openjdk /opt/jdk && mkdir -p /work\n"
                  "COPY app/lib/ /app/lib/\n"
                  f"COPY app/{config['mainJar']} /app/{config['mainJar']}\n"
                  "ENV JAVA_HOME=/opt/jdk PATH=/opt/jdk/bin:/usr/bin:/bin HOME=/\n"
                  "WORKDIR /app\nUSER 65532:65532\n"
                  f"ENTRYPOINT {json.dumps(command)}\n")
    (output / "Dockerfile").write_text(dockerfile)
    inventory = {str(p.relative_to(output)): {"bytes": p.stat().st_size,
                 "sha256": hashlib.sha256(p.read_bytes()).hexdigest()}
                 for p in sorted((output / "app").rglob("*.jar"))}
    (output / "launch.json").write_text(json.dumps({
        "flags": flags, "command": command, "config": config,
        "files": inventory, "platform_manifest": descriptor,
    }, indent=2) + "\n")


if __name__ == "__main__":
    main()
