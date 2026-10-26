#!/usr/bin/env python3
"""Build real fixture applications; optionally exercise the generic and Maven producers.

Routine producer integration packages the Python fixture with the generic CLI and
the Java fixture through the Maven producer. The Java, Node.js, .NET and PHP
fixtures are selectable with --languages and are exercised daily by their
per-language workflows. This is host/producer integration, never a claim of
node/runtime acceptance.
"""

import argparse
from datetime import datetime, timedelta, timezone
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
import xml.etree.ElementTree as ET


ROOT = Path(__file__).resolve().parents[2]
FIXTURES = Path(__file__).resolve().parent
PLUGIN = "sh.brewlet:brewlet-maven-plugin:" + json.loads((ROOT / "release.json").read_text())["version"] + ":"


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
        assert manifest["annotations"]["brewlet.sh/artifact-version"] == "1"
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


FIXTURE_LANGUAGES = ("java", "python", "node", "dotnet", "php")
ROUTINE_LANGUAGES = ("python",)


def validate_languages(languages):
    languages = tuple(languages)
    if not languages or any(language not in FIXTURE_LANGUAGES for language in languages) \
            or len(set(languages)) != len(languages):
        raise ValueError(f"unsupported fixture language selection: {languages!r}")
    return languages


def prepare_java(work):
    java = work / "java"
    shutil.copytree(FIXTURES / "java", java, ignore=shutil.ignore_patterns("target"))
    run(["mvn", "-B", "-q", "-f", java / "dependency/pom.xml", "install"])
    run(["mvn", "-B", "-q", "-f", java / "bom/pom.xml", "install"])
    run(["mvn", "-B", "-q", "-f", java / "app/pom.xml", "package"])
    return java


def build_and_probe(work, languages=FIXTURE_LANGUAGES):
    languages = validate_languages(languages)
    commands, layers, java = {}, {}, None
    if "java" in languages:
        java = prepare_java(work)
        java_app = work / "java-app"
        java_dependencies = work / "java-dependencies"
        java_app.mkdir()
        (java_app / "lib").mkdir()
        java_dependencies.mkdir()
        shutil.copy2(java / "app/target/fixture-java-2.0.0.jar", java_app / "app.jar")
        shutil.copy2(java / "dependency/target/greeting-2.0.0.jar",
                     java_dependencies / "greeting-2.0.0.jar")
        commands["java"] = ["java", "--class-path",
                            str(java_app / "app.jar") + os.pathsep + str(java_dependencies / "*"),
                            "sh.brewlet.fixture.Main", "--probe", "two words"]
        layers["java"] = [("dependency", java_dependencies, "/app/lib"), ("app", java_app, "/app")]
    if "python" in languages:
        commands["python"] = ["python3", FIXTURES / "python/src/main.py", "--probe", "two words"]
        layers["python"] = [("dependency", FIXTURES / "python/dependencies", "/app/dependencies"),
                            ("app", FIXTURES / "python/src", "/app/src")]
    if "node" in languages:
        commands["node"] = ["node", FIXTURES / "node/src/main.mjs", "--probe", "two words"]
        layers["node"] = [("dependency", FIXTURES / "node/dependencies", "/app/dependencies"),
                          ("app", FIXTURES / "node/src", "/app/src")]
    if "dotnet" in languages:
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
        commands["dotnet"] = ["dotnet", published / "App.dll", "--probe", "two words"]
        layers["dotnet"] = [("dependency", dotnet_dependencies, "/app"), ("app", dotnet_app, "/app")]
    if "php" in languages:
        commands["php"] = ["php", "-d", "include_path=" + str(FIXTURES / "php/dependencies"),
                           FIXTURES / "php/src/main.php", "--probe", "two words"]
        layers["php"] = [("dependency", FIXTURES / "php/dependencies", "/app/dependencies"),
                         ("app", FIXTURES / "php/src", "/app/src")]

    environment = dict(os.environ, BREWLET_TEST_VALUE="explicit-contract-value")
    reports = {}
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

    return reports, layers, java


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--brewlet", type=Path)
    parser.add_argument("--host-only", action="store_true")
    parser.add_argument("--work-parent", type=Path, default=Path(tempfile.gettempdir()))
    parser.add_argument("--languages", default=",".join(ROUTINE_LANGUAGES),
                        help="comma-separated generic CLI fixtures (default: %(default)s; "
                             "choices: " + ",".join(FIXTURE_LANGUAGES) + ")")
    args = parser.parse_args()
    if not args.host_only and not args.brewlet:
        parser.error("--brewlet is required unless --host-only is explicit")
    try:
        languages = validate_languages(args.languages.split(","))
    except ValueError as error:
        parser.error(str(error))
    work = Path(tempfile.mkdtemp(prefix="brewlet-v2-fixtures-", dir=args.work_parent)).resolve()
    print(f"Retained fixture work: {work}", flush=True)
    reports, layers, java = build_and_probe(work, languages)
    evidence = {"scope": "host-fixtures", "languages": list(languages), "reports": reports, "images": {}}
    if not args.host_only:
        brewlet = args.brewlet.resolve()
        for language, layer_inputs in layers.items():
            digests = []
            for attempt in range(2):
                layout = work / f"oci-{language}-{attempt}"
                command = [brewlet, "build", f"fixtures/{language}:v2", "--store", layout,
                           "--platform", "linux/amd64", "--platform", "linux/arm64",
                           "--launch", FIXTURES / language / "launch.json"]
                for role, source, target in layer_inputs:
                    command += ["--layer", f"{role}={source}:{target}"]
                run(command)
                digests.append(image_target(layout))
                run([brewlet, "inspect", f"fixtures/{language}:v2", "--store", layout])
            assert digests[0] == digests[1], f"{language}: nondeterministic packaging"
            evidence["images"][language] = digests[0]
        # The Maven producer is a Java product component, so it always uses the Java fixture.
        if java is None:
            java = prepare_java(work)
        run(["mvn", "-B", "-q", "-f", ROOT / "maven-plugin/pom.xml", "install"])
        run(["mvn", "-B", "-q", "-f", java / "app/pom.xml",
             "package",
             PLUGIN + "push",
             f"-Dbrewlet.executable={brewlet}",
             f"-Dbrewlet.store={work / 'oci-java-maven'}",
             "-Dbrewlet.image=fixtures/java-maven:v2",
             "-Dbrewlet.platforms=linux/amd64,linux/arm64"])
        evidence["images"]["java-maven"] = image_target(work / "oci-java-maven")
        managed_set = work / "oci-java-layer-set"
        source_bom = "sh.brewlet.fixture:fixture-bom:2.0.0"
        run(["mvn", "-B", "-q", "-f", java / "app/pom.xml",
             PLUGIN + "layer-set",
             f"-Dbrewlet.executable={brewlet}", f"-Dbrewlet.store={managed_set}",
             "-Dbrewlet.layerSet=fixtures/java-dependencies:v2",
             f"-Dbrewlet.sourceBom={source_bom}"])
        managed_output = work / "oci-java-managed"
        managed_arguments = [
            "package", PLUGIN + "push",
            f"-Dbrewlet.executable={brewlet}", f"-Dbrewlet.store={managed_output}",
            "-Dbrewlet.image=fixtures/java-managed:v2",
            "-Dbrewlet.platforms=linux/amd64,linux/arm64",
            f"-Dbrewlet.layerSetInput={managed_set}", f"-Dbrewlet.sourceBom={source_bom}",
        ]
        run(["mvn", "-B", "-q", "-f", java / "app/pom.xml", *managed_arguments])
        evidence["images"]["java-managed"] = image_target(managed_output)
        dependency_manifest = blob(managed_set, json.loads(
            (managed_set / "index.json").read_text())["manifests"][0])
        application_index = blob(managed_output, json.loads(
            (managed_output / "index.json").read_text())["manifests"][0])
        application_manifest = blob(managed_output, application_index["manifests"][0])
        expected_layers = [layer["digest"] for layer in dependency_manifest["layers"]
                           if layer.get("annotations", {}).get("brewlet.sh/layer") == "dependency"]
        actual_layers = [layer["digest"] for layer in application_manifest["layers"]
                         if layer["annotations"]["brewlet.sh/layer"] == "dependency"]
        assert actual_layers == expected_layers and len(actual_layers) == 1, (
            "Java managed dependencies must be reused byte-for-byte, without a duplicate bundled layer")
        app_only_digests = []
        for attempt in range(2):
            app_only = work / f"oci-java-application-only-{attempt}"
            run(["mvn", "-B", "-q", "-f", java / "app/pom.xml",
                 "package", PLUGIN + "push",
                 f"-Dbrewlet.executable={brewlet}", f"-Dbrewlet.store={app_only}",
                 "-Dbrewlet.image=fixtures/java-application-only:v2",
                 "-Dbrewlet.platforms=linux/amd64,linux/arm64",
                 "-Dbrewlet.applicationOnly=true", f"-Dbrewlet.sourceBom={source_bom}"])
            target = json.loads((app_only / "index.json").read_text())["manifests"][0]
            app_only_digests.append(target["digest"])
            index = blob(app_only, target)
            for descriptor in index["manifests"]:
                manifest = blob(app_only, descriptor)
                assert [layer["annotations"]["brewlet.sh/layer"] for layer in manifest["layers"]] == [
                    "app", "launch"], "app-only Maven mode bundled dependencies"
        assert app_only_digests[0] == app_only_digests[1], "app-only build graph staging is nondeterministic"
        release_policy = work / "development-release-policy.json"
        release_policy.write_text(json.dumps({"schemaVersion": 1, "mode": "development",
            "notAfter": (datetime.now(timezone.utc) + timedelta(hours=1)).isoformat(),
            "publisher": {"identity": "", "keys": []},
            "composer": {"identity": "", "keys": []}}))
        composed = work / "oci-java-release-composed"
        run([brewlet, "bundle", "fixtures/java-release:v2", "--store", composed,
             "--application", str(app_only) + "@" + target["digest"],
             "--layer-set", str(managed_set) + "@" + json.loads(
                 (managed_set / "index.json").read_text())["manifests"][0]["digest"],
             "--platform", "linux/arm64", "--policy", release_policy])
        composed_target = json.loads((composed / "index.json").read_text())["manifests"][0]
        composed_manifest = blob(composed, composed_target)
        original = blob(app_only, next(member for member in index["manifests"]
                                      if member["platform"]["architecture"] == "arm64"))
        assert composed_manifest["layers"][-2:] == original["layers"], (
            "release composition changed app-only Maven build metadata or launch")
        evidence["mavenApplicationOnly"] = {"digest": target["digest"], "deterministic": True,
            "composedDigest": composed_target["digest"], "scope": "producer-integration-development-policy"}
        scope_drift = work / "java-scope-drift"
        shutil.copytree(java / "app", scope_drift, ignore=shutil.ignore_patterns("target"))
        tree = ET.parse(scope_drift / "pom.xml")
        namespace = "{http://maven.apache.org/POM/4.0.0}"
        dependency = tree.getroot().find(f"{namespace}dependencies/{namespace}dependency")
        assert dependency.find(f"{namespace}artifactId").text == "greeting"
        ET.SubElement(dependency, namespace + "scope").text = "runtime"
        ET.register_namespace("", "http://maven.apache.org/POM/4.0.0")
        ET.register_namespace("xsi", "http://www.w3.org/2001/XMLSchema-instance")
        tree.write(scope_drift / "pom.xml", encoding="utf-8", xml_declaration=True)
        invalid_arguments = [
            argument if not argument.startswith("-Dbrewlet.store=")
            else f"-Dbrewlet.store={work / 'oci-java-scope-drift'}"
            for argument in managed_arguments]
        rejected = subprocess.run(["mvn", "-B", "-q", "-f", str(scope_drift / "pom.xml"),
                                   *map(str, invalid_arguments)],
                                  text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        assert rejected.returncode != 0 and "graph" in rejected.stdout.lower(), (
            "Maven must reject changed dependency scope even when JAR bytes match: " + rejected.stdout)
        evidence["mavenManagedScopeDrift"] = "rejected"
        spec = importlib.util.spec_from_file_location("maven_conformance", ROOT / "integration-tests/maven_conformance.py")
        conformance = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(conformance)
        evidence["mavenConformanceCases"] = conformance.run(
            brewlet, java / "app/pom.xml", work, ROOT / "conformance/artifact-v1.json")
        evidence["scope"] = "host-fixtures-and-producer-integration"
    (work / "result.json").write_text(json.dumps(evidence, indent=2) + "\n")
    print(f"Passed ({evidence['scope']}); NOT real-node acceptance")


if __name__ == "__main__":
    main()
