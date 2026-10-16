#!/usr/bin/env python3
"""Build four real applications; optionally exercise the generic OCI producer.

This is host/producer integration, never a claim of node/runtime acceptance.
"""

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import selectors
import subprocess
import tempfile
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
FIXTURES = Path(__file__).resolve().parent


def run(command, *, cwd=None, env=None, capture=False):
    print("+", " ".join(map(str, command)), flush=True)
    return subprocess.run(
        list(map(str, command)), check=True, cwd=cwd, env=env,
        text=True, stdout=subprocess.PIPE if capture else None,
    ).stdout


def blob(layout, descriptor):
    algorithm, digest = descriptor["digest"].split(":")
    if algorithm != "sha256" or len(digest) != 64:
        raise AssertionError("noncanonical descriptor digest")
    data = (layout / "blobs" / algorithm / digest).read_bytes()
    assert len(data) == descriptor["size"]
    assert hashlib.sha256(data).hexdigest() == digest
    return json.loads(data)


def image_target(layout):
    index = json.loads((layout / "index.json").read_text())
    assert len(index["manifests"]) == 1, "one producer target expected"
    descriptor = index["manifests"][0]
    target = blob(layout, descriptor)
    platforms = target.get("manifests", [descriptor])
    assert len(platforms) == 2, "explicit two-platform index required"
    payload_digests = None
    for manifest_descriptor in platforms:
        manifest = blob(layout, manifest_descriptor)
        assert manifest["annotations"]["brewlet.sh/artifact-version"] == "2"
        roles = [layer["annotations"]["brewlet.sh/layer"] for layer in manifest["layers"]]
        assert roles[-1] == "launch" and roles.count("launch") == 1
        assert "app" in roles and "dependency" in roles
        config = blob(layout, manifest["config"])
        assert config["config"].get("User", "") == ""
        assert config["config"]["WorkingDir"] == "/app"
        assert len(config["rootfs"]["diff_ids"]) == len(manifest["layers"])
        digests = [layer["digest"] for layer in manifest["layers"]]
        if payload_digests is None:
            payload_digests = digests
        else:
            assert digests == payload_digests, "neutral payload layers must deduplicate"
    return descriptor["digest"]


def build_and_probe(work):
    java = work / "java"
    shutil.copytree(FIXTURES / "java", java, ignore=shutil.ignore_patterns("target"))
    run(["mvn", "-B", "-q", "-f", java / "dependency/pom.xml", "install"])
    run(["mvn", "-B", "-q", "-f", java / "app/pom.xml", "package"])
    java_app = work / "java-app"
    java_dependencies = work / "java-dependencies"
    java_app.mkdir()
    (java_app / "lib").mkdir()
    java_dependencies.mkdir()
    shutil.copy2(java / "app/target/fixture-java-2.0.0.jar", java_app / "app.jar")
    shutil.copy2(java / "dependency/target/greeting-2.0.0.jar",
                 java_dependencies / "greeting-2.0.0.jar")

    dotnet = work / "dotnet"
    shutil.copytree(FIXTURES / "dotnet", dotnet,
                    ignore=shutil.ignore_patterns("bin", "obj", "target"))
    published = work / "dotnet-publish"
    run(["dotnet", "publish", dotnet / "App/App.csproj", "--configuration", "Release",
         "--output", published, "--nologo"], cwd=dotnet)
    dotnet_app = work / "dotnet-app"
    dotnet_dependencies = work / "dotnet-dependencies"
    dotnet_app.mkdir()
    dotnet_dependencies.mkdir()
    for name in ("App.dll", "App.deps.json", "App.runtimeconfig.json"):
        shutil.copy2(published / name, dotnet_app / name)
    shutil.copy2(published / "Greeting.dll", dotnet_dependencies / "Greeting.dll")

    environment = dict(os.environ, BREWLET_TEST_VALUE="explicit-contract-value")
    reports = {}
    commands = {
        "java": ["java", "--class-path",
                 str(java_app / "app.jar") + os.pathsep + str(java_dependencies / "*"),
                 "sh.brewlet.fixture.Main", "--probe", "two words"],
        "python": ["python3", FIXTURES / "python/src/main.py", "--probe", "two words"],
        "node": ["node", FIXTURES / "node/src/main.mjs", "--probe", "two words"],
        "dotnet": ["dotnet", published / "App.dll", "--probe", "two words"],
    }
    for language, command in commands.items():
        process_environment = dict(environment)
        if language == "python":
            process_environment["PYTHONPATH"] = str(FIXTURES / "python/dependencies")
            process_environment["PYTHONDONTWRITEBYTECODE"] = "1"
        output = run(command, env=process_environment, cwd=work, capture=True)
        report = json.loads(output)
        assert report["language"] == language
        assert report["dependency"] == "shared-contract"
        assert report["arguments"] == ["--probe", "two words"]
        assert report["value"] == "explicit-contract-value"
        assert report["cwd"] == str(work)
        assert report["uid"] == os.getuid() and report["gid"] == os.getgid()
        server_environment = dict(process_environment, BREWLET_TEST_PORT="0")
        server_command = [value for value in command if value != "--probe"]
        server = subprocess.Popen(list(map(str, server_command)), cwd=work, env=server_environment,
                                  text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            with selectors.DefaultSelector() as selector:
                selector.register(server.stdout, selectors.EVENT_READ)
                if not selector.select(timeout=30):
                    raise AssertionError(f"{language}: HTTP server did not become ready")
            ready = json.loads(server.stdout.readline())
            with urllib.request.urlopen(f"http://127.0.0.1:{ready['listening']}/", timeout=10) as response:
                served = json.loads(response.read())
            assert served["language"] == language and served["dependency"] == "shared-contract"
            assert served["arguments"] == ["two words"]
        finally:
            server.terminate()
            try:
                server.communicate(timeout=10)
            except subprocess.TimeoutExpired:
                server.kill()
                server.communicate(timeout=10)
        reports[language] = report

    return reports, {
        "java": [("dependency", java_dependencies, "/app/lib"), ("app", java_app, "/app")],
        "python": [("dependency", FIXTURES / "python/dependencies", "/app/dependencies"),
                   ("app", FIXTURES / "python/src", "/app/src")],
        "node": [("dependency", FIXTURES / "node/dependencies", "/app/dependencies"),
                 ("app", FIXTURES / "node/src", "/app/src")],
        "dotnet": [("dependency", dotnet_dependencies, "/app"), ("app", dotnet_app, "/app")],
    }, java


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--brewlet", type=Path)
    parser.add_argument("--host-only", action="store_true")
    parser.add_argument("--work-parent", type=Path, default=Path(tempfile.gettempdir()))
    args = parser.parse_args()
    if not args.host_only and not args.brewlet:
        parser.error("--brewlet is required unless --host-only is explicit")
    work = Path(tempfile.mkdtemp(prefix="brewlet-v2-fixtures-", dir=args.work_parent)).resolve()
    print(f"Retained fixture work: {work}", flush=True)
    reports, layers, java = build_and_probe(work)
    evidence = {"scope": "host-fixtures", "reports": reports, "images": {}}
    if not args.host_only:
        brewlet = args.brewlet.resolve()
        for language, layer_inputs in layers.items():
            digests = []
            for attempt in range(2):
                layout = work / f"oci-{language}-{attempt}"
                command = [brewlet, "push", f"fixtures/{language}:v2", "--store", layout,
                           "--platform", "linux/amd64", "--platform", "linux/arm64",
                           "--launch", FIXTURES / language / "launch.json"]
                for role, source, target in layer_inputs:
                    command += ["--layer", f"{role}={source}:{target}"]
                run(command)
                digests.append(image_target(layout))
                run([brewlet, "inspect", f"fixtures/{language}:v2", "--store", layout])
            assert digests[0] == digests[1], f"{language}: nondeterministic packaging"
            evidence["images"][language] = digests[0]
        run(["mvn", "-B", "-q", "-f", ROOT / "maven-plugin/pom.xml", "install"])
        run(["mvn", "-B", "-q", "-f", java / "app/pom.xml",
             "package",
             "sh.brewlet:brewlet-maven-plugin:2.0.0-SNAPSHOT:push",
             f"-Dbrewlet.executable={brewlet}",
             f"-Dbrewlet.store={work / 'oci-java-maven'}",
             "-Dbrewlet.image=fixtures/java-maven:v2",
             "-Dbrewlet.platforms=linux/amd64,linux/arm64"])
        evidence["images"]["java-maven"] = image_target(work / "oci-java-maven")
        spec = importlib.util.spec_from_file_location("maven_conformance", ROOT / "integration-tests/maven_conformance.py")
        conformance = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(conformance)
        evidence["mavenConformanceCases"] = conformance.run(
            brewlet, java / "app/pom.xml", work, ROOT / "conformance/artifact-v2.json")
        evidence["scope"] = "host-fixtures-and-producer-integration"
    (work / "result.json").write_text(json.dumps(evidence, indent=2) + "\n")
    print(f"Passed ({evidence['scope']}); NOT real-node acceptance")


if __name__ == "__main__":
    main()
