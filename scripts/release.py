"""Build and validate Brewlet distributions; this module never publishes."""

import argparse
from datetime import datetime, timezone
import gzip
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import xml.etree.ElementTree as ET
import zipfile

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from acceptance.evidence import candidate_errors, read_json, working_identity
from scripts.maven_notice import notices

GO = "go1.26.0"
SYFT = "1.55.0"
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
VERSION = re.compile(r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
                     r"(?:-(?:alpha|beta|rc)\.(?:0|[1-9][0-9]*))?")
NS = {"m": "http://maven.apache.org/POM/4.0.0"}


def require(value, message):
    if not value:
        raise ValueError(message)


def run(args, *, cwd=ROOT, env=None):
    try:
        return subprocess.check_output([str(arg) for arg in args], cwd=cwd,
                                       env={**os.environ, **(env or {})}, text=True)
    except subprocess.CalledProcessError as error:
        print(error.output, file=sys.stderr)
        raise


def sha(data):
    return hashlib.sha256(data).hexdigest()


def write_json(path, value):
    Path(path).write_text(json.dumps(value, sort_keys=True, indent=2) + "\n")


def is_prerelease(version):
    """Only alpha/beta/rc suffixed versions are GitHub prereleases; plain X.Y.Z releases are marked latest."""
    require(VERSION.fullmatch(version), "invalid release version: " + repr(version))
    return "-" in version


def is_stable(version):
    """Versions below 1.0.0 are betas; suffixed versions are prereleases; neither is stable."""
    return not is_prerelease(version) and not version.startswith("0.")


def policy(root=ROOT):
    config = read_json(root / "release.json")
    require(config["schemaVersion"] == 1 and VERSION.fullmatch(config["version"]),
            "release version must be MAJOR.MINOR.PATCH with an optional -alpha.N, -beta.N or -rc.N suffix")
    require(config["repository"] == "microsoft/brewlet"
            and config["workflow"] == ".github/workflows/release.yml", "untrusted release authority")
    require(config["cliTargets"] == ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
            and config["imagePlatforms"] == ["linux/amd64", "linux/arm64"], "unsupported target matrix")
    require(config["images"] == {
        "operator": "ghcr.io/microsoft/brewlet-operator",
        "admission": "ghcr.io/microsoft/brewlet-admission",
        "node": "ghcr.io/microsoft/brewlet-node-provisioner"},
        "image coordinate change requires a reviewed release-policy change")
    require(config["chart"] == "ghcr.io/microsoft/charts/brewlet", "unexpected chart coordinate")
    require(config["maven"] == {"groupId": "sh.brewlet", "artifactId": "brewlet-maven-plugin",
                                 "minimumJava": 17}, "unexpected Maven support coordinates")
    pom = ET.parse(root / "maven-plugin/pom.xml").getroot()
    for field, expected in {**{k: config["maven"][k] for k in ("groupId", "artifactId")},
                            "version": config["version"]}.items():
        require(pom.findtext("m:" + field, namespaces=NS) == expected, "Maven identity drift: " + field)
    chart = (root / "kubernetes/charts/brewlet/Chart.yaml").read_text()
    for field in ("version", "appVersion"):
        require(re.search(r"^" + field + ": " + re.escape(config["version"]) + "$", chart, re.M),
                "chart identity drift: " + field)
    values = (root / "kubernetes/charts/brewlet/values.yaml").read_text()
    for repository in config["images"].values():
        require(values.count(repository + ":" + config["version"]) == 1, "source-chart image identity drift")
    return config


def epoch():
    return int(run(["git", "show", "-s", "--format=%ct", "HEAD"]).strip())


def archive(path, entries, timestamp):
    """Stable headers, ordering and gzip metadata; entries are bytes and modes."""
    with Path(path).open("wb") as output:
        with gzip.GzipFile(filename="", fileobj=output, mode="wb", mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as tar:
                for name, (data, mode) in sorted(entries.items()):
                    require(not PurePosixPath(name).is_absolute() and ".." not in PurePosixPath(name).parts,
                            "unsafe archive member")
                    info = tarfile.TarInfo(name)
                    info.size, info.mode, info.mtime = len(data), mode, timestamp
                    tar.addfile(info, io.BytesIO(data))


def members(path):
    with tarfile.open(path) as tar:
        result = {}
        for entry in tar:
            name = str(PurePosixPath(entry.name))
            require(not PurePosixPath(name).is_absolute() and ".." not in PurePosixPath(name).parts,
                    "unsafe archive path")
            if entry.isdir():
                continue
            require(entry.isfile() and name not in result, "non-regular or duplicate archive member")
            result[name] = (tar.extractfile(entry).read(), entry.mode)
        return result


def binary_notice(package, target, destination):
    system, arch = target.split("/")
    run(["go", "run", "./scripts/notice", "--goos", system, "--goarch", arch,
         "--output", destination, *package], env={"GOTOOLCHAIN": GO})
    return Path(destination).read_bytes()


def build_cli(output, target, config, identity):
    require(target in config["cliTargets"], "unsupported CLI target")
    system, arch = target.split("/")
    with tempfile.TemporaryDirectory() as temp:
        executable = Path(temp) / "brewlet"
        run(["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false",
             "-ldflags", f"-s -w -X main.version={config['version']} -X main.commit={identity['checkout']}",
             "-o", executable, "./cmd/brewlet"],
            env={"GOOS": system, "GOARCH": arch, "CGO_ENABLED": "0", "GOTOOLCHAIN": GO})
        notice = binary_notice(["./cmd/brewlet"], target, Path(temp) / "NOTICE.txt")
        archive(output / f"brewlet_{config['version']}_{system}_{arch}.tar.gz", {
            "brewlet": (executable.read_bytes(), 0o755),
            "LICENSE.txt": ((ROOT / "LICENSE.txt").read_bytes(), 0o644),
            "NOTICE.txt": (notice, 0o644),
        }, epoch())


def build_maven(output, config):
    run(["mvn", "-B", "-q", "-f", "maven-plugin/pom.xml", "-Prelease",
         f"-Dproject.build.outputTimestamp={epoch()}", "clean", "verify"])
    base = "brewlet-maven-plugin-" + config["version"]
    for suffix in (".jar", "-sources.jar", "-javadoc.jar"):
        shutil.copyfile(ROOT / "maven-plugin/target" / (base + suffix), output / (base + suffix))
    with zipfile.ZipFile(output / (base + ".jar")) as jar:
        legal = {name: jar.read(name) for name in ("META-INF/LICENSE.txt", "META-INF/NOTICE.txt")}
    for suffix in ("-sources.jar", "-javadoc.jar"):
        path = output / (base + suffix)
        with zipfile.ZipFile(path) as jar:
            entries = {name: jar.read(name) for name in jar.namelist() if not name.endswith("/")}
        entries.update(legal)
        with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as jar:
            for name, data in sorted(entries.items()):
                info = zipfile.ZipInfo(name, datetime.fromtimestamp(epoch(), timezone.utc).timetuple()[:6])
                info.compress_type = zipfile.ZIP_DEFLATED
                info.external_attr = 0o100644 << 16
                jar.writestr(info, data)
    shutil.copyfile(ROOT / "maven-plugin/pom.xml", output / (base + ".pom"))
    with zipfile.ZipFile(output / (base + ".jar")) as jar:
        require(jar.read("META-INF/LICENSE.txt") == (ROOT / "LICENSE.txt").read_bytes(),
                "Maven license mismatch")
        require(jar.read("META-INF/NOTICE.txt").decode() ==
                notices((ROOT / "maven-plugin/target/runtime-classpath.txt").read_text()),
                "Maven runtime notices mismatch")
    smoke_maven(output, config)


def smoke_maven(output, config):
    import platform
    system = {"Linux": "linux", "Darwin": "darwin"}.get(platform.system())
    arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    require(system and arch, "unsupported Maven consumer platform")
    base = "brewlet-maven-plugin-" + config["version"]
    # Separate local repository and consumer POM: never target/classes or a source rebuild.
    with tempfile.TemporaryDirectory() as temp:
        executable = Path(temp) / "brewlet"
        data = members(output / f"brewlet_{config['version']}_{system}_{arch}.tar.gz")
        executable.write_bytes(data["brewlet"][0])
        executable.chmod(0o755)
        pom = Path(temp) / "pom.xml"
        pom.write_text('<project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion>'
                       '<groupId>release.smoke</groupId><artifactId>consumer</artifactId>'
                       '<version>1</version></project>')
        run(["mvn", "-B", "-q", f"-Dmaven.repo.local={temp}",
             "org.apache.maven.plugins:maven-install-plugin:3.1.4:install-file",
             "-Dfile=" + str(output / (base + ".jar")),
             "-DpomFile=" + str(output / (base + ".pom"))])
        run(["mvn", "-B", "-q", "-f", pom, f"-Dmaven.repo.local={temp}",
             f"sh.brewlet:brewlet-maven-plugin:{config['version']}:conformance",
             "-Dbrewlet.fixtures=" + str(ROOT / "conformance/artifact-v1.json"),
             "-Dbrewlet.conformanceResult=" + str(Path(temp) / "result.json"),
             "-Dbrewlet.executable=" + str(executable)])
        result = read_json(Path(temp) / "result.json")
        require(result and all(row["passed"] is True for row in result), "Maven consumer conformance failed")


def read_blob(layout, descriptor):
    digest = descriptor["digest"]
    require(DIGEST.fullmatch(digest), "invalid OCI digest")
    path = layout / "blobs/sha256" / digest[7:]
    require(not path.is_symlink(), "symlink OCI blob")
    data = path.read_bytes()
    require("sha256:" + sha(data) == digest and len(data) == descriptor["size"], "OCI blob mismatch")
    return data


def image_identity(layout, component, config):
    index = read_json(layout / "index.json")
    require(index["schemaVersion"] == 2 and len(index["manifests"]) == 1, "one image index required")
    top = index["manifests"][0]
    multi = json.loads(read_blob(layout, top))
    children = multi.get("manifests", [])
    require(len(children) == 2, "exactly two native image manifests required")
    platforms = {}
    for child in children:
        manifest = json.loads(read_blob(layout, child))
        image = json.loads(read_blob(layout, manifest["config"]))
        platform = image["os"] + "/" + image["architecture"]
        require(platform in config["imagePlatforms"] and platform not in platforms, "wrong image platform")
        require(image["config"]["Entrypoint"] == ["/" + {"node": "brewlet-node"}.get(component, component)],
                "wrong component entrypoint")
        labels = image["config"]["Labels"]
        require(labels["org.opencontainers.image.version"] == config["version"]
                and labels["org.opencontainers.image.source"] == "https://github.com/" + config["repository"],
                "wrong image release labels")
        require(re.fullmatch(r"[0-9a-f]{40}", labels["org.opencontainers.image.revision"]),
                "missing image source revision")
        # Read the final merged legal files, accounting for normal OCI whiteouts.
        final = {}
        binaries = {"operator": ("operator", "admission"), "admission": ("admission",),
                    "node": ("brewlet-node", "containerd-shim-brewlet-v2")}[component]
        selected = {"LICENSE.txt", "NOTICE.txt", *binaries}
        for layer in manifest["layers"]:
            with tarfile.open(fileobj=io.BytesIO(read_blob(layout, layer))) as tar:
                entries = tar.getmembers()
                if any(str(PurePosixPath(e.name.removeprefix("./"))) == ".wh..wh..opq" for e in entries):
                    final.clear()
                for entry in entries:
                    name = str(PurePosixPath(entry.name.removeprefix("./")))
                    if name.startswith(".wh.") and name.removeprefix(".wh.") in selected:
                        final.pop(name.removeprefix(".wh."), None)
                for entry in entries:
                    name = str(PurePosixPath(entry.name.removeprefix("./")))
                    if name in selected:
                        require(entry.isfile(), "image binary/legal resource is not a regular file")
                        final[name] = tar.extractfile(entry).read()
        require(final.get("LICENSE.txt") == (ROOT / "LICENSE.txt").read_bytes(), "image license mismatch")
        require(final.get("NOTICE.txt", b"").startswith(
            ("Brewlet third-party notices\nTarget: " + platform + "\n").encode()),
            "missing or wrong-platform image notices")
        with tempfile.TemporaryDirectory() as temp:
            for binary in binaries:
                require(final.get(binary), "missing image executable")
                path = Path(temp) / binary
                path.write_bytes(final[binary])
                metadata = run(["go", "version", "-m", path])
                require("GOOS=linux" in metadata and "GOARCH=" + image["architecture"] in metadata,
                        "image executable target differs from manifest")
                toolchain = metadata.splitlines()[0].rsplit(" ", 1)[-1]
                text = final["NOTICE.txt"].decode()
                require("Go runtime " + toolchain + "\n" in text, "image runtime notice mismatch")
                for line in metadata.splitlines():
                    fields = line.split()
                    if fields and fields[0] == "dep":
                        require(" ".join(fields[1:3]) + "\n" in text, "missing redistributed module notice")
        platforms[platform] = {"digest": child["digest"], "revision": labels["org.opencontainers.image.revision"]}
    return {"digest": top["digest"], "platforms": platforms}


def build_images(output, config, identity):
    for component in config["images"]:
        dockerfile = "provisioner/Dockerfile" if component == "node" else "kubernetes/Dockerfile"
        args = ["docker", "buildx", "build", "--file", dockerfile,
                "--platform", ",".join(config["imagePlatforms"]), "--provenance=false", "--sbom=false",
                "--build-arg", "SOURCE_DATE_EPOCH=" + str(epoch()),
                "--label", "org.opencontainers.image.version=" + config["version"],
                "--label", "org.opencontainers.image.source=https://github.com/" + config["repository"],
                "--label", "org.opencontainers.image.revision=" + identity["checkout"],
                "--output", f"type=oci,dest={output / ('image-' + component)},tar=false,rewrite-timestamp=true"]
        if component != "node":
            args += ["--target", component]
        # Only tracked source bytes enter Docker's context; never private work files.
        with tempfile.TemporaryDirectory() as temp:
            payload = subprocess.check_output(["git", "archive", identity["checkout"]], cwd=ROOT)
            with tarfile.open(fileobj=io.BytesIO(payload)) as tar:
                tar.extractall(temp, filter="data")
            run([*args, temp])
        image_identity(output / ("image-" + component), component, config)


def build_chart(output, config):
    with tempfile.TemporaryDirectory() as temp:
        chart = Path(temp) / "brewlet"
        shutil.copytree(ROOT / "kubernetes/charts/brewlet", chart)
        values = (chart / "values.yaml").read_text()
        for component, key in (("operator", "operator"), ("admission", "admission"), ("node", "provisioner")):
            image = image_identity(output / ("image-" + component), component, config)
            old = config["images"][component] + ":" + config["version"]
            require(values.count(old) == 1, "chart image coordinate drift: " + key)
            values = values.replace(old, config["images"][component] + "@" + image["digest"])
        (chart / "values.yaml").write_text(values)
        shutil.copyfile(ROOT / "LICENSE.txt", chart / "LICENSE.txt")
        shutil.copyfile(ROOT / "NOTICE.txt", chart / "NOTICE.txt")
        run(["helm", "lint", chart])
        run(["helm", "package", chart, "--destination", temp])
        packaged = members(Path(temp) / f"brewlet-{config['version']}.tgz")
        archive(output / f"brewlet-{config['version']}.tgz", packaged, epoch())


def expected_files(config):
    version = config["version"]
    base = "brewlet-maven-plugin-" + version
    files = {f"brewlet_{version}_{target.replace('/', '_')}.tar.gz" for target in config["cliTargets"]}
    files |= {base + suffix for suffix in (".jar", ".pom", "-sources.jar", "-javadoc.jar")}
    files.add(f"brewlet-{version}.tgz")
    return files


def validate_payloads(output, config):
    for target in config["cliTargets"]:
        data = members(output / f"brewlet_{config['version']}_{target.replace('/', '_')}.tar.gz")
        require(set(data) == {"brewlet", "LICENSE.txt", "NOTICE.txt"}, "incorrect CLI archive inventory")
        require(data["brewlet"][0] and data["brewlet"][1] == 0o755, "missing executable CLI")
        require(data["LICENSE.txt"][0] == (ROOT / "LICENSE.txt").read_bytes(), "CLI license mismatch")
        with tempfile.TemporaryDirectory() as temp:
            expected = binary_notice(["./cmd/brewlet"], target, Path(temp) / "notice")
        require(data["NOTICE.txt"][0] == expected, "CLI notice differs from actual target dependencies")
    base = "brewlet-maven-plugin-" + config["version"]
    pom = ET.fromstring((output / (base + ".pom")).read_bytes())
    for field, value in {"groupId": config["maven"]["groupId"], "artifactId": config["maven"]["artifactId"],
                         "version": config["version"]}.items():
        require(pom.findtext("m:" + field, namespaces=NS) == value, "packaged Maven identity drift")
    for suffix in (".jar", "-sources.jar", "-javadoc.jar"):
        with zipfile.ZipFile(output / (base + suffix)) as jar:
            require(len(jar.namelist()) == len(set(jar.namelist())), "duplicate JAR entry")
            require(jar.read("META-INF/LICENSE.txt") == (ROOT / "LICENSE.txt").read_bytes(), "JAR license mismatch")
            require(jar.read("META-INF/NOTICE.txt").strip(), "empty JAR notice")
            if suffix == ".jar":
                plugin = ET.fromstring(jar.read("META-INF/maven/plugin.xml"))
                require(plugin.findtext("version") == config["version"], "plugin descriptor identity drift")
    chart = output / f"brewlet-{config['version']}.tgz"
    contents = members(chart)
    require(contents["brewlet/LICENSE.txt"][0] == (ROOT / "LICENSE.txt").read_bytes(), "chart license mismatch")
    for source in (ROOT / "kubernetes/charts/brewlet/crds").glob("*.yaml"):
        require(contents["brewlet/crds/" + source.name][0] == source.read_bytes(), "packaged CRD mismatch")
    for args in ([], ["--set", "admission.enabled=false"],
                 ["--set-json", 'provisioner.profiles=[{"name":"render-check","spec":{"nodePool":{"key":"release.test/pool","names":["isolated"]},"runtimeImages":[]}}]']):
        rendered = run(["helm", "template", "brewlet", chart, *args])
        for component in config["images"]:
            if component == "admission" and args == ["--set", "admission.enabled=false"]:
                continue
            image = image_identity(output / ("image-" + component), component, config)
            require(config["images"][component] + "@" + image["digest"] in rendered,
                    "packaged chart does not pin actual component digest")


def inventory(output, config, identity):
    files = expected_files(config)
    require({p.name for p in output.iterdir() if p.is_file()} == files, "unexpected/missing release payload")
    images = {component: image_identity(output / ("image-" + component), component, config)
              for component in config["images"]}
    for image in images.values():
        require(all(p["revision"] == identity["checkout"] for p in image["platforms"].values()),
                "image built from another source")
    return {"schemaVersion": 1, "version": config["version"], "candidate": identity,
            "sourceTimestamp": epoch(), "images": images,
            "files": {name: {"sha256": sha((output / name).read_bytes()), "size": (output / name).stat().st_size}
                      for name in sorted(files)},
            "tools": {"go": GO, "maven": run(["mvn", "-v"]).splitlines()[0],
                      "helm": run(["helm", "version", "--short"]).strip(), "syft": SYFT}}


def validate_inventory(output, config, *, candidate=True):
    document = read_json(output / "candidate.json")
    require(document["schemaVersion"] == 1 and document["version"] == config["version"], "candidate identity mismatch")
    if candidate:
        require(not candidate_errors(document["candidate"]), "artifact candidate differs from immutable checkout")
    require(set(document["files"]) == expected_files(config), "incorrect release inventory")
    allowed = expected_files(config) | {"candidate.json", "checksums.txt", "sboms", "evidence",
                                        "attestations", "central-bundle.zip", "signatures",
                                        "published.json", "central-deployment.json"}
    allowed |= {"image-" + name for name in config["images"]}
    require(all(path.name in allowed for path in output.iterdir()), "unlisted release output")
    for name, expected in document["files"].items():
        path = output / name
        require(not path.is_symlink() and path.stat().st_size == expected["size"]
                and sha(path.read_bytes()) == expected["sha256"], "artifact digest mismatch: " + name)
    checksums = "".join(row["sha256"] + "  " + name + "\n" for name, row in sorted(document["files"].items()))
    require((output / "checksums.txt").read_text() == checksums, "checksum inventory mismatch")
    for component in config["images"]:
        require(image_identity(output / ("image-" + component), component, config) == document["images"][component],
                "image inventory mismatch")
    return document


def smoke_cli(output, target, config):
    import platform
    machine = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64"}.get(platform.machine())
    native = {"Linux": "linux", "Darwin": "darwin"}.get(platform.system(), "unsupported") + "/" + str(machine)
    require(target == native and target in config["cliTargets"], "native CLI target required")
    document = validate_inventory(output, config, candidate=False)
    name = f"brewlet_{config['version']}_{target.replace('/', '_')}.tar.gz"
    with tempfile.TemporaryDirectory() as temp:
        data = members(output / name)
        binary = Path(temp) / "brewlet"
        binary.write_bytes(data["brewlet"][0])
        binary.chmod(0o755)
        require(json.loads(run([binary, "version"])) ==
                {"version": config["version"], "commit": document["candidate"]["checkout"], "platform": target},
                "native CLI build identity mismatch")
    return document, name


def build(output, selection, target):
    config, identity = policy(), working_identity()
    output.mkdir(parents=True, exist_ok=True)
    if selection == "all":
        require(not list(output.iterdir()), "candidate output must be empty")
    if selection in ("all", "cli"):
        for item in ([target] if target else config["cliTargets"]):
            build_cli(output, item, config, identity)
    if selection in ("all", "maven"):
        build_maven(output, config)
    if selection in ("all", "images"):
        build_images(output, config, identity)
    if selection in ("all", "chart"):
        build_chart(output, config)
    if selection in ("all", "seal"):
        require(working_identity() == identity, "candidate source changed during packaging")
        validate_payloads(output, config)
        write_json(output / "candidate.json", inventory(output, config, identity))
        (output / "checksums.txt").write_text("".join(
            sha((output / name).read_bytes()) + "  " + name + "\n" for name in sorted(expected_files(config))))


def reproduce(output):
    config = policy()
    original = validate_inventory(output, config)
    with tempfile.TemporaryDirectory() as temp:
        second = Path(temp) / "candidate"
        build(second, "all", None)
        rebuilt = validate_inventory(second, config)
        require(original["files"] == rebuilt["files"] and original["images"] == rebuilt["images"],
                "repeated candidate builds produced different payload bytes/digests")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("identity", "channel", "build", "verify", "smoke-cli",
                                            "smoke-maven", "reproduce"))
    parser.add_argument("--output", type=Path, default=ROOT / "dist/candidate")
    parser.add_argument("--only", choices=("all", "cli", "maven", "images", "chart", "seal"), default="all")
    parser.add_argument("--target")
    args = parser.parse_args()
    output = args.output.resolve()
    if args.command == "identity":
        print(json.dumps(policy(), sort_keys=True))
    elif args.command == "channel":
        version = policy()["version"]
        print("prerelease" if is_prerelease(version) else "stable" if is_stable(version) else "beta")
    elif args.command == "build":
        build(output, args.only, args.target)
    elif args.command == "verify":
        validate_inventory(output, policy())
        validate_payloads(output, policy())
    elif args.command == "smoke-maven":
        validate_inventory(output, policy())
        smoke_maven(output, policy())
    elif args.command == "reproduce":
        reproduce(output)
    else:
        smoke_cli(output, args.target, policy())


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, KeyError, subprocess.CalledProcessError) as error:
        print("release failed:", error, file=sys.stderr)
        raise SystemExit(1)
