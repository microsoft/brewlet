# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Exercise release smoke with real Helm and mocked transport, never a cluster."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import unittest
import uuid


ROOT = Path(__file__).resolve().parents[2]
DIGEST = "0123456789abcdef" * 4
VERSION = "0.5.0"

# Transport/workload stand-ins fail on unrecognized commands. Helm still
# reads/renders the actual packaged chart, with synthetic release image pins.
MOCK = r'''
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import time

name = Path(sys.argv[0]).name
args = sys.argv[1:]
work = Path(os.environ["SMOKE_WORK"])
version = os.environ["SMOKE_VERSION"]
mutation = os.environ.get("SMOKE_MUTATION", "")
with (work / "calls.jsonl").open("a") as log:
    log.write(json.dumps([name, *args]) + "\n")

def option(flag):
    return args[args.index(flag) + 1]

def reject():
    raise SystemExit("unexpected offline command: " + repr([name, *args]))

if name in ("curl", "installer"):
    assert (Path(os.environ["CURL_HOME"]) / ".curlrc").read_text() == ""
if name in ("helm", "docker", "provenance"):
    assert json.loads((Path(os.environ["DOCKER_CONFIG"]) / "config.json").read_text()) == {}
    assert json.loads(Path(os.environ["HELM_REGISTRY_CONFIG"]).read_text()) == {}

if name == "brewlet":
    if args == ["version"]:
        print(version)
    elif args[0] == "push":
        print("fixture push")
    elif args[0] == "inspect":
        print('{"mainJar": "app.jar"}')
    elif args[0] == "run":
        (work / "app-running").touch()
        time.sleep(120)
    elif args[0] == "bundle":
        target = Path(option("--out"))
        target.mkdir()
        (target / "config.json").write_text("{}")
    else:
        reject()
elif name == "curl":
    url = args[-1]
    if "--insecure" in args or "-k" in args:
        reject()
    if url.startswith("http://127.0.0.1:") and url.endswith("/healthz"):
        raise SystemExit(0 if (work / "app-running").exists() else 22)
    elif url.startswith("http://127.0.0.1:") and url.endswith("/hello"):
        print("Hello from a JAR")
    elif url == f"https://github.com/microsoft/brewlet/archive/refs/tags/v{version}.tar.gz":
        with tarfile.open(option("-o"), "w:gz") as archive:
            payload = b"<project/>"
            entry = tarfile.TarInfo(f"brewlet-{version}/integration-tests/fixtures/demo-app/pom.xml")
            entry.size = len(payload)
            archive.addfile(entry, io.BytesIO(payload))
    elif url in (
        f"https://github.com/microsoft/brewlet/releases/download/v{version}/brewlet-maven-plugin-{version}.jar",
        f"https://github.com/microsoft/brewlet/releases/download/v{version}/brewlet-maven-plugin-{version}.pom",
    ):
        Path(option("-o")).write_text("offline fixture")
    else:
        reject()
elif name == "mvn":
    if "-f" in args:
        target = Path(option("-f")).parent / "target"
        target.mkdir(exist_ok=True)
        (target / "app.jar").write_text("fixture")
        if any(arg.endswith(":build") for arg in args):
            (target / "brewlet/oci").mkdir(parents=True)
            (target / "brewlet/jvm-config.json").write_text("{}")
            (target / "brewlet/oci/index.json").write_text("{}")
    elif not any("maven-install-plugin" in arg for arg in args):
        reject()
elif name == "helm":
    if args[0] == "pull":
        assert args[1] == "oci://ghcr.io/microsoft/charts/brewlet"
        assert option("--version") == version
        if mutation == "chart-access-denied":
            raise SystemExit("denied: requested access to the resource is denied")
        shutil.copy(work / f"brewlet-{version}.tgz", Path(option("--destination")) / f"brewlet-{version}.tgz")
    elif args[0] in ("template", "show"):
        if args[0] == "show" and args[1] != "chart":
            reject()
        if args[0] == "template" and mutation == "missing-values":
            index = args.index("--values")
            del args[index:index + 2]
        result = subprocess.run([os.environ["REAL_HELM"], *args], capture_output=True, text=True)
        output = result.stdout
        if args[0] == "show":
            if mutation == "chart-name":
                output = output.replace("name: brewlet\n", "name: unrelated\n")
            if mutation == "chart-version":
                output = output.replace("version: " + version, "version: 9.9.9")
            if mutation == "chart-app-version":
                output = output.replace("appVersion:", "wrongAppVersion:")
        else:
            replacements = {
                "pool": ('- "release-smoke"', '- "all-workers"'),
                "pool-key": ('key: "brewlet.sh/test-pool"', 'key: "other/pool"'),
                "jdk": ("distribution: temurin", "distribution: other"),
                "feature": ("feature: 21", "feature: 25"),
                "source": ("registry.example.com/brewlet-tests/jdk@", "registry.example.com/unapproved/jdk@"),
                "java-home": ("/opt/java/openjdk", "/unexpected/java"),
                "profile-name": ("  name: default\n", "  name: other\n"),
                "profile-kind": ("kind: NodeProfile\n", "kind: UnrelatedProfile\n"),
                "component-tag": ("ghcr.io/microsoft/brewlet-operator@sha256:" + "0123456789abcdef" * 4,
                                  "ghcr.io/microsoft/brewlet-operator:" + version),
            }
            if mutation in replacements:
                output = output.replace(*replacements[mutation])
            if mutation == "no-install-metadata":
                output = "\n".join(line for line in output.split("\n")
                                   if "meta.helm.sh/release-" not in line)
        sys.stdout.write(output)
        sys.stderr.write(result.stderr)
        raise SystemExit(result.returncode)
    else:
        reject()
elif name == "docker":
    if args[:2] != ["manifest", "inspect"]:
        reject()
    if mutation == "manifest-failure":
        raise SystemExit(1)
elif name == "provenance":
    assert args == [version]
    if mutation == "provenance-failure":
        raise SystemExit(1)
elif name == "installer":
    assert os.environ["BREWLET_VERSION"] == version
    target = Path(os.environ["BREWLET_INSTALL_DIR"])
    target.mkdir()
    shutil.copy(work / "bin/brewlet", target / "brewlet")
else:
    reject()
'''


class ReleaseArtifactsTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.helm = shutil.which(os.environ.get("HELM", "helm"))
        if not cls.helm:
            raise RuntimeError("Helm is required for offline release-smoke regressions")
        cls.work = ROOT / (".test-release-smoke-" + uuid.uuid4().hex)
        cls.work.mkdir()
        cls.addClassCleanup(shutil.rmtree, cls.work)
        credentials = cls.work / "caller-credentials"
        credentials.mkdir()
        cls.caller_configs = {
            ".curlrc": 'header = "Authorization: Bearer fixture"\n',
            "config.json": '{"credsStore": "must-not-be-used"}',
            "registry.json": '{"auths": {"ghcr.io": {"auth": "fixture"}}}',
        }
        for name, content in cls.caller_configs.items():
            (credentials / name).write_text(content)
        chart = cls.work / "chart"
        shutil.copytree(ROOT / "kubernetes/charts/brewlet", chart)
        values = (chart / "values.yaml").read_text()
        old = '  digests:\n    operator: ""\n    provisioner: ""\n    admission: ""'
        new = "  digests:\n" + "\n".join(
            f'    {name}: "sha256:{DIGEST}"'
            for name in ("operator", "provisioner", "admission")
        )
        if old not in values:
            raise AssertionError("Update synthetic release pins to match the actual chart")
        (chart / "values.yaml").write_text(values.replace(old, new))
        subprocess.run([cls.helm, "package", str(chart), "--version", VERSION,
                        "--app-version", VERSION, "--destination", str(cls.work)],
                       check=True, capture_output=True, text=True, timeout=60)
        subject = cls.work / "subject"
        (subject / "site/scripts").mkdir(parents=True)
        (subject / "scripts").mkdir()
        shutil.copy(ROOT / "site/scripts/verify-release-artifacts.sh",
                    subject / "site/scripts/verify-release-artifacts.sh")
        binary = cls.work / "bin"
        binary.mkdir()
        for name in ("brewlet", "curl", "mvn", "docker", "helm", "provenance", "installer"):
            path = binary / name
            path.write_text(f"#!{sys.executable}\n" + MOCK)
            path.chmod(0o755)
        (subject / "site/install.sh").write_text('#!/bin/sh\nexec installer\n')
        provenance = subject / "scripts/verify-release-provenance.sh"
        provenance.write_text('#!/bin/sh\nexec provenance "$@"\n')
        provenance.chmod(0o755)

    def smoke(self, mutation=""):
        for name in ("calls.jsonl", "app-running"):
            (self.work / name).unlink(missing_ok=True)
        env = dict(os.environ, SMOKE_WORK=str(self.work), SMOKE_VERSION=VERSION,
                   SMOKE_MUTATION=mutation, REAL_HELM=self.helm,
                   CURL_HOME=str(self.work / "caller-credentials"),
                   DOCKER_CONFIG=str(self.work / "caller-credentials"),
                   HELM_REGISTRY_CONFIG=str(self.work / "caller-credentials/registry.json"),
                   PATH=str(self.work / "bin") + os.pathsep + os.environ["PATH"])
        env.pop("BREWLET_MIN_PROVENANCE_VERSION", None)
        result = subprocess.run(
            ["bash", str(self.work / "subject/site/scripts/verify-release-artifacts.sh"), VERSION],
            cwd=self.work, env=env, capture_output=True, text=True, timeout=60,
        )
        self.assertFalse(list(self.work.glob(".brewlet-release-smoke-*")), "Smoke workspace leaked")
        for name, content in self.caller_configs.items():
            self.assertEqual((self.work / "caller-credentials" / name).read_text(), content,
                             "Smoke test changed caller credentials")
        return result

    def test_anonymous_chart_failure_explains_independent_package_visibility(self):
        result = self.smoke("chart-access-denied")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Anonymous chart pull failed", result.stderr)
        self.assertIn("making the source repository public does not make its packages public",
                      result.stderr)

    def test_real_chart_renders_with_required_inputs_and_keeps_release_verification(self):
        result = self.smoke()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        calls = [json.loads(line) for line in (self.work / "calls.jsonl").read_text().splitlines()]
        self.assertEqual(sum(call[0] == "installer" for call in calls), 1)
        self.assertIn(["provenance", VERSION], calls)
        templates = [call for call in calls if call[:2] == ["helm", "template"]]
        self.assertEqual(len(templates), 1)
        self.assertIn("--values", templates[0])
        self.assertTrue(any(call[:3] == ["helm", "show", "chart"] for call in calls))
        for component in ("operator", "admission", "node-provisioner"):
            self.assertIn(["docker", "manifest", "inspect",
                           f"ghcr.io/microsoft/brewlet-{component}@sha256:{DIGEST}"], calls)
        self.assertFalse(any(call[0] == "helm" and call[1] in ("install", "upgrade") for call in calls))

    def test_wrong_chart_identity_or_rendered_inventory_fails_closed(self):
        for mutation in (
            "chart-name", "chart-version", "chart-app-version",
            "missing-values",
            "pool", "pool-key", "jdk", "feature", "source", "java-home",
            "profile-name", "profile-kind", "component-tag",
            "manifest-failure", "provenance-failure",
        ):
            with self.subTest(mutation=mutation):
                result = self.smoke(mutation)
                self.assertNotEqual(result.returncode, 0, f"Smoke accepted {mutation}")

    def test_render_does_not_require_install_time_ownership_annotations(self):
        # The pinned released chart predates templated ownership annotations.
        # Inventory and provenance verification must not require later additions.
        result = self.smoke("no-install-metadata")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
